package main

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/api"
)

func TestHumanBytesUsesBinaryUnits(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 6144: "6.0 KiB", 1536: "1.5 KiB", 5 << 20: "5.0 MiB", 4 << 30: "4.0 GiB"} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestParseBeforeTakesADateOrATime(t *testing.T) {
	for in, want := range map[string]time.Time{
		"2026-10-01":           time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		"2026-10-01T12:30:00Z": time.Date(2026, 10, 1, 12, 30, 0, 0, time.UTC),
	} {
		if got, err := parseBefore(in); err != nil || !got.Equal(want) {
			t.Errorf("parseBefore(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	offset, err := parseBefore("2026-10-01T12:00:00+02:00")
	if err != nil || !offset.Equal(time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("an offset time = %v, %v", offset, err)
	}
	for _, in := range []string{"yesterday", "2026-13-01", "2026-10-01 12:00", ""} {
		if _, err := parseBefore(in); err == nil {
			t.Errorf("parseBefore(%q) succeeded", in)
		}
	}
}

func TestPolicyTextNamesWhatIsBounded(t *testing.T) {
	for want, p := range map[string]api.RetentionPolicy{
		"none, runs are kept until deleted":          {},
		"at most 1 run":                              {Runs: 1},
		"older than 36h0m0s removed":                 {AgeSeconds: 36 * 3600},
		"at most 500 runs, older than 7d removed":    {Runs: 500, AgeSeconds: 7 * 24 * 3600},
		"at most 10000 runs, older than 30d removed": {Runs: 10000, AgeSeconds: 30 * 24 * 3600},
	} {
		if got := policyText(p); got != want {
			t.Errorf("policyText(%+v) = %q, want %q", p, got, want)
		}
	}
}

// runs keep, unkeep, prune and storage map the retention operations: keep
// shows in the run list, a dry run counts what the prune then deletes, and
// a kept run stays.
func TestRunsRetentionCommands(t *testing.T) {
	ctx := context.Background()
	server := startServe(t)
	dir := filepath.Join("..", "..", "samples", "logs-cleanup")
	for range 3 {
		if code, _, stderr := runCLI(t, "run", "--config", filepath.Join(dir, "config.yaml"), "--fixture", filepath.Join(dir, "input.json"), "--normalize", "--server", server); code != exitOK {
			t.Fatalf("run: exit %d, %s", code, stderr)
		}
	}
	listed := func() []api.Run {
		var page api.RunPage
		decodeParityJSON(t, parityHTTP(t, ctx, http.MethodGet, server+"/api/v1/runs", nil, http.StatusOK), &page)
		return page.Runs
	}
	runs := listed()
	if len(runs) != 3 {
		t.Fatalf("runs = %d, want 3", len(runs))
	}
	id := runs[0].ID

	if code, out, stderr := runCLI(t, "runs", "keep", "--server", server, "--run", id); code != exitOK || out != "run "+id+" kept: retention and prune skip it\n" {
		t.Fatalf("keep: exit %d, %q, %s", code, out, stderr)
	}
	if got := listed(); !got[0].Kept || got[1].Kept || got[2].Kept {
		t.Fatalf("the run list after keeping = %+v", got)
	}
	var kept api.Run
	if decodeParityJSON(t, parityCLI(t, ctx, []string{"runs", "keep", "--server", server, "--run", id, "--format", "json"}), &kept); !kept.Kept || kept.ID != id {
		t.Fatalf("keep json = %+v", kept)
	}

	code, out, stderr := runCLI(t, "runs", "storage", "--server", server)
	if code != exitOK || !strings.Contains(out, "runs: 3, 1 kept\n") || !strings.Contains(out, "oldest run: ") || !strings.Contains(out, "retention: at most 10000 runs, older than 30d removed\n") || !strings.HasPrefix(out, "storage: ") {
		t.Fatalf("storage: exit %d, %q, %s", code, out, stderr)
	}
	var usage api.StorageUsage
	if decodeParityJSON(t, parityCLI(t, ctx, []string{"runs", "storage", "--server", server, "--format", "json"}), &usage); usage.Runs != 3 || usage.KeptRuns != 1 || usage.Used <= 0 || usage.Retention.Runs != 10000 {
		t.Fatalf("storage json = %+v", usage)
	}

	code, out, stderr = runCLI(t, "runs", "prune", "--server", server, "--before", "2000-01-01", "--dry-run")
	if code != exitOK || out != "would prune 0 runs, freeing 0 B\n" {
		t.Fatalf("prune old: exit %d, %q, %s", code, out, stderr)
	}
	code, dry, stderr := runCLI(t, "runs", "prune", "--server", server, "--dry-run")
	if code != exitOK || !strings.HasPrefix(dry, "would prune 2 runs, freeing ") || len(listed()) != 3 {
		t.Fatalf("dry run: exit %d, %q, %s", code, dry, stderr)
	}
	code, real, stderr := runCLI(t, "runs", "prune", "--server", server, "--before", time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	if want := strings.Replace(strings.Replace(dry, "would prune", "pruned", 1), "freeing", "freed", 1); code != exitOK || real != want {
		t.Fatalf("prune: exit %d, %q, want %q, %s", code, real, want, stderr)
	}
	if got := listed(); len(got) != 1 || got[0].ID != id {
		t.Fatalf("runs after the prune = %+v, want only the kept run", got)
	}
	var none api.RunPruneResult
	if decodeParityJSON(t, parityCLI(t, ctx, []string{"runs", "prune", "--server", server, "--dry-run", "--format", "json"}), &none); none.Runs != 0 || !none.DryRun {
		t.Fatalf("prune json = %+v", none)
	}

	if code, out, _ := runCLI(t, "runs", "unkeep", "--server", server, "--run", id); code != exitOK || out != "run "+id+" no longer kept: retention and prune apply to it\n" {
		t.Fatalf("unkeep: exit %d, %q", code, out)
	}
	if got := listed(); got[0].Kept {
		t.Fatalf("the run list after unkeeping = %+v", got)
	}
	if code, out, _ := runCLI(t, "runs", "prune", "--server", server); code != exitOK || !strings.HasPrefix(out, "pruned 1 run, freed ") {
		t.Fatalf("prune the last run: exit %d, %q", code, out)
	}
	if got := listed(); len(got) != 0 {
		t.Fatalf("runs = %+v, want none", got)
	}

	for _, args := range [][]string{
		{"runs", "keep", "--server", server},
		{"runs", "unkeep", "--server", server, "--run", "run-unknown"},
		{"runs", "keep", "--run", id},
		{"runs", "prune"},
		{"runs", "prune", "--server", server, "--before", "yesterday"},
		{"runs", "prune", "--server", server, "--result", "unknown"},
		{"runs", "storage"},
	} {
		if code, _, _ := runCLI(t, args...); code != exitError {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}
