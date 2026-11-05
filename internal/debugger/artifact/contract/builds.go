package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

func buildID(n int) string { return fmt.Sprintf("build-%016x", n) }

func digestOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

var buildLimits = artifact.BuildLimits{PerPrincipal: 10, Total: 100}

// buildAt is a build admission of key, by principal, at t.
func buildAt(n int, principal, key string, t time.Time) artifact.BuildAdmission {
	return artifact.BuildAdmission{ID: buildID(n), Principal: principal, Name: "edge", Key: digestOf(key), Spec: []byte(`{"name":"edge"}`),
		Manifest: []byte("dist:\n  name: edge\n"), Sources: []artifact.BuildSource{{Module: "example.com/proc", Archive: []byte("tar " + key)}}, Created: t}
}

func admitBuild(t *testing.T, r artifact.Repository, a artifact.BuildAdmission) artifact.BuildRecord {
	t.Helper()
	got, err := r.AdmitBuild(ctx, a, buildLimits)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func buildIDs(records []artifact.BuildRecord) []string {
	var ids []string
	for _, b := range records {
		ids = append(ids, b.ID)
	}
	return ids
}

// buildAdmission: an admitted build is queued with its inputs; an existing
// ID conflicts, invalid admissions are refused, and the unfinished builds of
// a principal and in total are bounded.
func buildAdmission(t *testing.T, b Backend) {
	r := openRuns(t, b)
	t0 := time.Now().UTC().Truncate(time.Millisecond)
	got := admitBuild(t, r, buildAt(1, "user:a", "one", t0))
	if got.State != artifact.BuildQueued || got.Principal != "user:a" || got.Name != "edge" || got.Key != digestOf("one") || got.ReusedFrom != "" ||
		!got.Created.Equal(t0) || string(got.Spec) != `{"name":"edge"}` {
		t.Errorf("admitted = %+v", got)
	}
	wantInputs := []artifact.BuildInput{{Digest: digestOf("dist:\n  name: edge\n"), Bytes: 19}, {Module: "example.com/proc", Digest: digestOf("tar one"), Bytes: 7}}
	if !slices.Equal(got.Inputs, wantInputs) {
		t.Errorf("inputs = %+v, want %+v", got.Inputs, wantInputs)
	}
	if read, err := r.Build(ctx, buildID(1)); err != nil || read.State != artifact.BuildQueued || !slices.Equal(read.Inputs, wantInputs) {
		t.Errorf("build = %+v, %v", read, err)
	}
	if _, err := r.Build(ctx, buildID(99)); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("unknown build: %v", err)
	}
	if m, err := r.BuildManifest(ctx, buildID(1)); err != nil || string(m) != "dist:\n  name: edge\n" {
		t.Errorf("manifest = %q, %v", m, err)
	}
	if _, err := r.BuildManifest(ctx, buildID(99)); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("unknown build's manifest: %v", err)
	}
	if _, err := r.AdmitBuild(ctx, buildAt(1, "user:a", "two", t0), buildLimits); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("existing ID: %v", err)
	}
	bad := buildAt(2, "user:a", "two", t0)
	bad.Key = "not-a-digest"
	if _, err := r.AdmitBuild(ctx, bad, buildLimits); !errors.Is(err, artifact.ErrInvalidRequest) {
		t.Errorf("bad key: %v", err)
	}
	huge := buildAt(2, "user:a", "two", t0)
	huge.Sources[0].Archive = make([]byte, artifact.MaxBuildInputBytes)
	if _, err := r.AdmitBuild(ctx, huge, buildLimits); !errors.Is(err, artifact.ErrLimit) {
		t.Errorf("oversized inputs: %v", err)
	}
	limits := artifact.BuildLimits{PerPrincipal: 2, Total: 3}
	if _, err := r.AdmitBuild(ctx, buildAt(2, "user:a", "two", t0), limits); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AdmitBuild(ctx, buildAt(3, "user:a", "three", t0), limits); !errors.Is(err, artifact.ErrLimit) {
		t.Errorf("a third build of one principal: %v", err)
	}
	if _, err := r.AdmitBuild(ctx, buildAt(4, "user:b", "four", t0), limits); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AdmitBuild(ctx, buildAt(5, "user:c", "five", t0), limits); !errors.Is(err, artifact.ErrLimit) {
		t.Errorf("a fourth build in total: %v", err)
	}
	// A finished build no longer counts.
	if _, err := r.CancelBuild(ctx, buildID(1), t0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AdmitBuild(ctx, buildAt(5, "user:c", "five", t0), limits); err != nil {
		t.Errorf("after a cancellation: %v", err)
	}
	if _, err := r.Build(ctx, buildID(3)); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("a refused build was stored: %v", err)
	}
}

// buildLeases: builders claim queued builds oldest first and never share
// one; only the holder of a lease logs and finishes; a done build is reused
// by a later build of the same key, a failed one is not; and every write is
// on the change feed.
func buildLeases(t *testing.T, b Backend) {
	r, reader := openRuns(t, b), openRuns(t, b)
	head, err := reader.ChangeHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().UTC().Truncate(time.Millisecond)
	for n, key := range []string{"one", "two", "three"} {
		admitBuild(t, r, buildAt(n+1, "user:a", key, t0.Add(time.Duration(n)*time.Second)))
	}
	expires := t0.Add(time.Minute)
	first, err := r.ClaimBuilds(ctx, "builder-1", 2, t0, expires)
	if err != nil || !slices.Equal(buildIDs(first), []string{buildID(1), buildID(2)}) {
		t.Fatalf("first claim = %v, %v", buildIDs(first), err)
	}
	for _, c := range first {
		if c.State != artifact.BuildRunning || c.Builder != "builder-1" || c.Attempt != 1 || !c.Expires.Equal(expires) || !c.Started.Equal(t0) || len(c.Inputs) != 2 {
			t.Errorf("claimed = %+v", c)
		}
	}
	if c, err := r.CountBuilds(ctx); err != nil || c != (artifact.BuildCounts{Queued: 1, Running: 2}) {
		t.Errorf("counts after the first claim = %+v, %v", c, err)
	}
	second, err := r.ClaimBuilds(ctx, "builder-2", 5, t0, expires)
	if err != nil || !slices.Equal(buildIDs(second), []string{buildID(3)}) {
		t.Fatalf("second claim = %v, %v", buildIDs(second), err)
	}
	if none, err := r.ClaimBuilds(ctx, "builder-3", 5, t0, expires); err != nil || len(none) != 0 {
		t.Errorf("empty queue claim = %v, %v", buildIDs(none), err)
	}
	if err := r.AppendBuildLog(ctx, buildID(1), "builder-2", 1, []string{"stolen"}); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("another builder logs: %v", err)
	}
	if err := r.AppendBuildLog(ctx, buildID(1), "builder-1", 1, []string{"go build", "ok"}); err != nil {
		t.Fatal(err)
	}
	later := t0.Add(5 * time.Minute)
	renewed, err := r.RenewBuilds(ctx, "builder-1", later)
	if err != nil || !slices.Equal(buildIDs(renewed), []string{buildID(1), buildID(2)}) || !renewed[0].Expires.Equal(later) {
		t.Fatalf("renewed = %+v, %v", renewed, err)
	}
	binary := digestOf("binary one")
	done := artifact.BuildOutcome{State: artifact.BuildDone, Result: []byte(`{"ok":true}`), BinaryDigest: binary, BinaryBytes: 4096}
	if _, err := r.FinishBuild(ctx, buildID(1), "builder-2", 1, done, t0); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("another builder finishes: %v", err)
	}
	if _, err := r.FinishBuild(ctx, buildID(1), "builder-1", 1, artifact.BuildOutcome{State: artifact.BuildDone}, t0); !errors.Is(err, artifact.ErrInvalidRequest) {
		t.Errorf("done without a binary: %v", err)
	}
	finished := t0.Add(2 * time.Minute)
	got, err := r.FinishBuild(ctx, buildID(1), "builder-1", 1, done, finished)
	if err != nil || got.State != artifact.BuildDone || got.BinaryDigest != binary || got.BinaryBytes != 4096 || string(got.Result) != `{"ok":true}` ||
		!got.Finished.Equal(finished) || got.Builder != "" || got.LogLines != 2 {
		t.Fatalf("finished = %+v, %v", got, err)
	}
	if _, err := r.FinishBuild(ctx, buildID(1), "builder-1", 1, done, finished); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("finishing twice: %v", err)
	}
	if err := r.AppendBuildLog(ctx, buildID(1), "builder-1", 1, []string{"late"}); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("logging a finished build: %v", err)
	}
	if _, err := r.FinishBuild(ctx, buildID(2), "builder-1", 1, artifact.BuildOutcome{State: artifact.BuildFailed, Result: []byte(`{"error":"x"}`)}, finished); err != nil {
		t.Fatal(err)
	}
	if c, err := r.CountBuilds(ctx); err != nil || c != (artifact.BuildCounts{Running: 1}) {
		t.Errorf("counts after two finished = %+v, %v", c, err)
	}
	// The same inputs again: build 1's binary is reused at once.
	reused := admitBuild(t, r, buildAt(4, "user:b", "one", finished))
	if reused.State != artifact.BuildDone || reused.ReusedFrom != buildID(1) || reused.BinaryDigest != binary || reused.BinaryBytes != 4096 ||
		string(reused.Result) != `{"ok":true}` || !reused.Finished.Equal(finished) || reused.Principal != "user:b" {
		t.Errorf("reused = %+v", reused)
	}
	// A failed build's inputs build again.
	if again := admitBuild(t, r, buildAt(5, "user:a", "two", finished)); again.State != artifact.BuildQueued || again.ReusedFrom != "" {
		t.Errorf("after a failure = %+v", again)
	}
	got2, _ := readChanges(t, reader, head, artifact.MaxChanges)
	for _, want := range []string{"build " + buildID(1), "build " + buildID(4), "build " + buildID(5)} {
		if !slices.Contains(got2, want) {
			t.Errorf("changes %q lack %q", got2, want)
		}
	}
}

// buildExpiry: a lease that ended returns its build to the queue, where the
// next claim is its next attempt, until the attempts are used; then it fails.
func buildExpiry(t *testing.T, b Backend) {
	r := openRuns(t, b)
	t0 := time.Now().UTC().Truncate(time.Millisecond)
	admitBuild(t, r, buildAt(1, "user:a", "one", t0))
	if _, err := r.ClaimBuilds(ctx, "builder-1", 1, t0, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if none, err := r.ExpireBuilds(ctx, t0.Add(30*time.Second), 2, []byte(`{"error":"lost"}`)); err != nil || len(none) != 0 {
		t.Errorf("expired before the lease ended = %v, %v", buildIDs(none), err)
	}
	expired, err := r.ExpireBuilds(ctx, t0.Add(2*time.Minute), 2, []byte(`{"error":"lost"}`))
	if err != nil || len(expired) != 1 || expired[0].State != artifact.BuildQueued || expired[0].Builder != "" {
		t.Fatalf("expired = %+v, %v", expired, err)
	}
	if err := r.AppendBuildLog(ctx, buildID(1), "builder-1", 1, []string{"late"}); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("the lost builder logs: %v", err)
	}
	again, err := r.ClaimBuilds(ctx, "builder-2", 1, t0.Add(2*time.Minute), t0.Add(3*time.Minute))
	if err != nil || len(again) != 1 || again[0].Attempt != 2 {
		t.Fatalf("second claim = %+v, %v", again, err)
	}
	failed, err := r.ExpireBuilds(ctx, t0.Add(4*time.Minute), 2, []byte(`{"error":"lost"}`))
	if err != nil || len(failed) != 1 || failed[0].State != artifact.BuildFailed || string(failed[0].Result) != `{"error":"lost"}` ||
		!failed[0].Finished.Equal(t0.Add(4*time.Minute)) {
		t.Fatalf("after the last attempt = %+v, %v", failed, err)
	}
	if none, _ := r.ClaimBuilds(ctx, "builder-3", 1, t0.Add(5*time.Minute), t0.Add(6*time.Minute)); len(none) != 0 {
		t.Errorf("a failed build was claimed: %v", buildIDs(none))
	}
}

// buildCancel: a queued build is cancelled at once; a running one is flagged
// for its builder, which then ends it; a terminal one conflicts.
func buildCancel(t *testing.T, b Backend) {
	r := openRuns(t, b)
	t0 := time.Now().UTC().Truncate(time.Millisecond)
	admitBuild(t, r, buildAt(1, "user:a", "one", t0))
	admitBuild(t, r, buildAt(2, "user:a", "two", t0.Add(time.Second)))
	if _, err := r.ClaimBuilds(ctx, "builder-1", 1, t0, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	queued, err := r.CancelBuild(ctx, buildID(2), t0.Add(time.Second))
	if err != nil || queued.State != artifact.BuildCancelled || !queued.Finished.Equal(t0.Add(time.Second)) {
		t.Errorf("cancelled queued = %+v, %v", queued, err)
	}
	running, err := r.CancelBuild(ctx, buildID(1), t0.Add(time.Second))
	if err != nil || running.State != artifact.BuildRunning || !running.CancelRequested {
		t.Errorf("cancelled running = %+v, %v", running, err)
	}
	renewed, err := r.RenewBuilds(ctx, "builder-1", t0.Add(2*time.Minute))
	if err != nil || len(renewed) != 1 || !renewed[0].CancelRequested {
		t.Errorf("renewed = %+v, %v", renewed, err)
	}
	if _, err := r.FinishBuild(ctx, buildID(1), "builder-1", 1, artifact.BuildOutcome{State: artifact.BuildCancelled}, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{buildID(1), buildID(2)} {
		if _, err := r.CancelBuild(ctx, id, t0); !errors.Is(err, artifact.ErrConflict) {
			t.Errorf("cancelling terminal %s: %v", id, err)
		}
	}
	if _, err := r.CancelBuild(ctx, buildID(9), t0); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("cancelling an unknown build: %v", err)
	}
	if none, _ := r.ClaimBuilds(ctx, "builder-2", 5, t0, t0.Add(time.Minute)); len(none) != 0 {
		t.Errorf("a cancelled build was claimed: %v", buildIDs(none))
	}
}

// buildLogs: a build keeps the last MaxBuildLogLines lines, numbered from
// its first line, clipped to storable text.
func buildLogs(t *testing.T, b Backend) {
	r := openRuns(t, b)
	t0 := time.Now().UTC().Truncate(time.Millisecond)
	admitBuild(t, r, buildAt(1, "user:a", "one", t0))
	if _, err := r.ClaimBuilds(ctx, "builder-1", 1, t0, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	total := artifact.MaxBuildLogLines + 500
	var batch []string
	for n := 0; n < total; n++ {
		batch = append(batch, fmt.Sprintf("line %d", n))
		if len(batch) == 700 || n == total-1 {
			if err := r.AppendBuildLog(ctx, buildID(1), "builder-1", 1, batch); err != nil {
				t.Fatal(err)
			}
			batch = nil
		}
	}
	lines, first, err := r.BuildLog(ctx, buildID(1), 0)
	if err != nil || first != 500 || len(lines) != artifact.MaxBuildLogLines || lines[0] != "line 500" || lines[len(lines)-1] != fmt.Sprintf("line %d", total-1) {
		t.Fatalf("log from 0: first %d, %d lines, %v", first, len(lines), err)
	}
	tail, first, err := r.BuildLog(ctx, buildID(1), total-2)
	if err != nil || first != total-2 || !slices.Equal(tail, []string{fmt.Sprintf("line %d", total-2), fmt.Sprintf("line %d", total-1)}) {
		t.Errorf("tail = %q from %d, %v", tail, first, err)
	}
	if none, first, err := r.BuildLog(ctx, buildID(1), total); err != nil || len(none) != 0 || first != total {
		t.Errorf("after the end = %q from %d, %v", none, first, err)
	}
	if got, _ := r.Build(ctx, buildID(1)); got.LogLines != total {
		t.Errorf("log lines = %d, want %d", got.LogLines, total)
	}
	odd := artifact.ClipBuildLine(strings.Repeat("x", artifact.MaxBuildLogLine+10) + "\x00")
	if len(odd) != artifact.MaxBuildLogLine {
		t.Errorf("clipped line has %d bytes", len(odd))
	}
	if err := r.AppendBuildLog(ctx, buildID(1), "builder-1", 1, []string{artifact.ClipBuildLine("bad \xff byte\x00")}); err != nil {
		t.Errorf("clipped line refused: %v", err)
	}
	if _, _, err := r.BuildLog(ctx, buildID(9), 0); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("unknown build's log: %v", err)
	}
}

// buildPages: builds list newest first, by principal, in pages; trimming
// keeps the newest terminal builds and every unfinished one.
func buildPages(t *testing.T, b Backend) {
	r := openRuns(t, b)
	t0 := time.Now().UTC().Truncate(time.Millisecond)
	for n := 1; n <= 7; n++ {
		admitBuild(t, r, buildAt(n, []string{"user:a", "user:b"}[n%2], fmt.Sprint(n), t0.Add(time.Duration(n)*time.Second)))
	}
	all := func(q artifact.BuildQuery) []string {
		var ids []string
		for {
			page, err := r.ListBuilds(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, buildIDs(page.Builds)...)
			if page.NextCursor == "" {
				return ids
			}
			q.Cursor = page.NextCursor
		}
	}
	if got := all(artifact.BuildQuery{Limit: 3}); !slices.Equal(got, []string{buildID(7), buildID(6), buildID(5), buildID(4), buildID(3), buildID(2), buildID(1)}) {
		t.Errorf("all = %v", got)
	}
	if got := all(artifact.BuildQuery{Principal: "user:a", Limit: 2}); !slices.Equal(got, []string{buildID(6), buildID(4), buildID(2)}) {
		t.Errorf("user:a = %v", got)
	}
	page, err := r.ListBuilds(ctx, artifact.BuildQuery{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ListBuilds(ctx, artifact.BuildQuery{Principal: "user:a", Cursor: page.NextCursor}); !errors.Is(err, artifact.ErrInvalidRequest) {
		t.Errorf("another query's cursor: %v", err)
	}
	if _, err := r.ListBuilds(ctx, artifact.BuildQuery{Cursor: "garbage"}); !errors.Is(err, artifact.ErrInvalidRequest) {
		t.Errorf("malformed cursor: %v", err)
	}
	// Builds 1 to 5 end; 6 and 7 stay queued.
	for n := 1; n <= 5; n++ {
		if _, err := r.CancelBuild(ctx, buildID(n), t0); err != nil {
			t.Fatal(err)
		}
	}
	trimmed, err := r.TrimBuilds(ctx, 2)
	if err != nil || trimmed != 3 {
		t.Errorf("trimmed %d, %v", trimmed, err)
	}
	if got := all(artifact.BuildQuery{}); !slices.Equal(got, []string{buildID(7), buildID(6), buildID(5), buildID(4)}) {
		t.Errorf("after trimming = %v", got)
	}
	if again, err := r.TrimBuilds(ctx, 2); err != nil || again != 0 {
		t.Errorf("trimming again = %d, %v", again, err)
	}
}
