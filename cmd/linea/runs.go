package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// runsCmd lists the runs a team server retains, newest first, each with its
// summary once it has a report. Local commands keep no runs, so it
// needs --server. Its subcommands read or act on one run (runs_read.go).
func runsCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		if sub, ok := runsSubcommands[args[0]]; ok {
			return sub(ctx, args[1:], stdout, stderr)
		}
	}
	fs := newFlagSet("runs", "--server URL [--scenario NAME] [--runtime NAME] [--result RESULT] [--limit N] [--cursor C | --all] [--format text|json] | log|show|cancel|assert|diffs|inputs|summary|evidence|evaluations|delete|keep|unkeep --server URL --run ID [--format text|json] | prune --server URL [--before DATE] [--project P] [--scenario S] [--runtime R] [--result R] [--dry-run] [--format text|json] | storage --server URL [--format text|json] | snapshot --server URL --run ID --ref REF --out FILE", stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	var q platform.RunQuery
	f := &q.RunFilter
	fs.StringVar(&f.Scenario, "scenario", "", "only runs of this scenario name")
	fs.StringVar(&f.Runtime, "runtime", "", "only runs on this runtime name (built-in for the debugger's own)")
	fs.StringVar(&f.Result, "result", "", "only runs that ended this way: completed, partial, failed or cancelled, or for a run without a report its state queued, running or error")
	fs.IntVar(&q.Limit, "limit", 0, fmt.Sprintf("runs per page (default %d, at most %d)", platform.DefaultRunPage, platform.MaxRunPage))
	fs.StringVar(&q.Cursor, "cursor", "", "continue after the page that printed this cursor")
	all := fs.Bool("all", false, "follow every page to the oldest matching run")
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	if *all && q.Cursor != "" {
		fmt.Fprintln(stderr, "--all and --cursor cannot be combined")
		fs.Usage()
		return exitError
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
	page, err := c.ListRuns(ctx, q)
	for err == nil && *all && page.NextCursor != "" {
		q.Cursor = page.NextCursor
		var next *api.RunPage
		if next, err = c.ListRuns(ctx, q); err == nil {
			next.Runs = append(page.Runs, next.Runs...)
			page = next
		}
	}
	if err != nil {
		return fail(stderr, err)
	}
	if *format == "json" {
		err = writeIndented(stdout, page)
	} else {
		err = writeRuns(stdout, page.Runs, page.NextCursor)
	}
	if err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

// runsLogCmd prints the Collector log tail a failed or partial run kept:
// info level and above, environment values redacted.
func runsLogCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("runs log", "--server URL --run ID [--format text|json]", stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	id := fs.String("run", "", "run whose log to print")
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	if *server == "" || *id == "" {
		fmt.Fprintln(stderr, "--server and --run are required: runs are kept by linea-server")
		fs.Usage()
		return exitError
	}
	c, err := connect(*server, false)
	if err != nil {
		return fail(stderr, err)
	}
	log, err := c.RunLog(ctx, *id)
	if err != nil {
		return fail(stderr, err)
	}
	if *format == "json" {
		if err := writeIndented(stdout, log); err != nil {
			return fail(stderr, err)
		}
		return exitOK
	}
	switch {
	case !log.Kept:
		fmt.Fprintf(stderr, "run %s kept no log: only a failed or partial run keeps its Collector log\n", log.RunID)
	case log.Truncated:
		fmt.Fprintln(stderr, "the log's start was dropped to keep its tail within the size limit")
	}
	if _, err := io.WriteString(stdout, log.Log); err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

// writeRuns prints one row per run: how execution ended, how many inputs were
// injected and the assertion counts, or "-" where the run has no report. A
// next page, when there is one, is named under the table.
func writeRuns(w io.Writer, runs []api.Run, nextCursor string) error {
	t := newTextOut(w)
	if len(runs) == 0 {
		t.none("runs")
		t.nextPage(nextCursor)
		return t.err()
	}
	rows := make([][]string, 0, len(runs))
	for _, r := range runs {
		scenario, rt, result, inputs, assertions := "-", "-", strings.ToUpper(string(r.State)), "-", "-"
		if s := r.Summary; s != nil {
			result = strings.ToUpper(string(s.Status))
			if s.Truncated {
				result += " (truncated)"
			}
			if s.Scenario != "" {
				scenario = s.Scenario
			}
			if s.Runtime != nil {
				rt = s.Runtime.Name + " " + s.Runtime.CollectorVersion
			}
			inputs = fmt.Sprintf("%d of %d", s.Injected, s.Inputs)
			if a := s.Assertions; a != nil {
				assertions = fmt.Sprintf("%s (%d pass, %d fail, %d inconclusive, %d error)", strings.ToUpper(string(a.Outcome)), a.Pass, a.Fail, a.Inconclusive, a.Error)
			}
		}
		rows = append(rows, []string{r.ID, result, r.Created.Local().Format(time.DateTime), scenario, rt, inputs, assertions})
	}
	t.table(0, []string{"id", "status", "created", "scenario", "runtime", "inputs", "assertions"}, rows)
	t.nextPage(nextCursor)
	return t.err()
}
