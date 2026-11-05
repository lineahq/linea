package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/client"
)

// Run retention: runs keep and unkeep protect one run from
// retention, runs prune deletes the finished runs a filter selects, and
// runs storage shows the workspace's use against its limits and policy. Like
// the other runs commands they need --server. --format json prints the
// server's JSON as sent.

func runsKeepCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runRead(ctx, "keep", "--server URL --run ID [--format text|json]", args, stdout, stderr, nil,
		func(ctx context.Context, c *client.Client, id string) ([]byte, error) { return c.KeepRun(ctx, id) },
		func(w io.Writer, raw []byte) error { return writeKept(w, raw, "kept: retention and prune skip it") })
}

func runsUnkeepCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runRead(ctx, "unkeep", "--server URL --run ID [--format text|json]", args, stdout, stderr, nil,
		func(ctx context.Context, c *client.Client, id string) ([]byte, error) { return c.UnkeepRun(ctx, id) },
		func(w io.Writer, raw []byte) error {
			return writeKept(w, raw, "no longer kept: retention and prune apply to it")
		})
}

func writeKept(w io.Writer, raw []byte, what string) error {
	var r runState
	if err := json.Unmarshal(raw, &r); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "run %s %s\n", r.ID, what)
	return err
}

// runsPruneCmd deletes, or with --dry-run only counts, the finished runs
// that are not kept, in no suite and the origin of no saved source.
func runsPruneCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("runs prune", "--server URL [--before DATE] [--project P] [--scenario S] [--runtime R] [--result R] [--dry-run] [--format text|json]", stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	var req api.RunPruneRequest
	before := fs.String("before", "", "only runs admitted before this date: 2006-01-02 (midnight UTC) or an RFC 3339 time")
	fs.StringVar(&req.Scenario, "scenario", "", "only runs of this scenario name")
	fs.StringVar(&req.Runtime, "runtime", "", "only runs on this runtime name (built-in for the debugger's own)")
	fs.StringVar(&req.Result, "result", "", "only runs that ended this way: completed, partial, failed or cancelled, or for a run without a report its state error")
	fs.BoolVar(&req.DryRun, "dry-run", false, "delete nothing and print what the same prune would delete")
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	// Without --project the prune covers every project the caller
	// administers.
	req.Project = workProject
	if *server == "" {
		fmt.Fprintln(stderr, "--server is required: runs are kept by linea-server")
		fs.Usage()
		return exitError
	}
	if *before != "" {
		t, err := parseBefore(*before)
		if err != nil {
			fmt.Fprintln(stderr, err)
			fs.Usage()
			return exitError
		}
		req.Before = &t
	}
	c, err := connect(*server, false)
	if err != nil {
		return fail(stderr, err)
	}
	raw, err := c.PruneRuns(ctx, req)
	if err != nil {
		return fail(stderr, err)
	}
	if *format == "json" {
		_, err = stdout.Write(raw)
	} else {
		err = writePruned(stdout, raw)
	}
	if err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

// parseBefore reads --before: a date is midnight UTC.
func parseBefore(v string) (time.Time, error) {
	if t, err := time.Parse(time.DateOnly, v); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("--before %q: want a date such as 2026-10-01 (midnight UTC) or an RFC 3339 time such as 2026-10-01T12:00:00Z", v)
}

func writePruned(w io.Writer, raw []byte) error {
	var res api.RunPruneResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	verb := "pruned %s, freed %s\n"
	if res.DryRun {
		verb = "would prune %s, freeing %s\n"
	}
	_, err := fmt.Fprintf(w, verb, plural(res.Runs, "run"), humanBytes(res.Bytes))
	return err
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// humanBytes prints bytes in binary units, as the server config's sizes are
// written.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// runsStorageCmd prints the workspace's storage use, its runs and the
// retention policy.
func runsStorageCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("runs storage", "--server URL [--format text|json]", stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	if *server == "" {
		fmt.Fprintln(stderr, "--server is required: runs are kept by linea-server")
		fs.Usage()
		return exitError
	}
	c, err := connect(*server, false)
	if err != nil {
		return fail(stderr, err)
	}
	raw, err := c.StorageUsage(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	if *format == "json" {
		_, err = stdout.Write(raw)
	} else {
		err = writeStorageUsage(stdout, raw)
	}
	if err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

func writeStorageUsage(w io.Writer, raw []byte) error {
	var u api.StorageUsage
	if err := json.Unmarshal(raw, &u); err != nil {
		return err
	}
	used := humanBytes(u.Used) + " used"
	if u.Limit > 0 {
		used += " of " + humanBytes(u.Limit)
	}
	fmt.Fprintf(w, "storage: %s\n", used)
	fmt.Fprintf(w, "runs: %d, %d kept\n", u.Runs, u.KeptRuns)
	if u.OldestRun != nil {
		fmt.Fprintf(w, "oldest run: %s\n", u.OldestRun.Local().Format(time.DateTime))
	}
	_, err := fmt.Fprintf(w, "retention: %s\n", policyText(u.Retention))
	return err
}

// policyText says what a retention policy bounds; whole days read as days.
func policyText(p api.RetentionPolicy) string {
	var parts []string
	if p.Runs > 0 {
		parts = append(parts, "at most "+plural(p.Runs, "run"))
	}
	if p.AgeSeconds > 0 {
		age := time.Duration(p.AgeSeconds) * time.Second
		text := age.String()
		if age%(24*time.Hour) == 0 {
			text = fmt.Sprintf("%dd", age/(24*time.Hour))
		}
		parts = append(parts, "older than "+text+" removed")
	}
	switch len(parts) {
	case 0:
		return "none, runs are kept until deleted"
	case 1:
		return parts[0]
	}
	return parts[0] + ", " + parts[1]
}
