package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// Suite reads on a team server: the retained suites, one
// suite's members and states, and its report. --format json prints the
// server's JSON as sent.

func suiteListCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("suite list", "--server URL [--format text|json]", stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	if *server == "" {
		fmt.Fprintln(stderr, "--server is required: suites are kept by linea-server")
		fs.Usage()
		return exitError
	}
	c, err := connect(*server, false)
	if err != nil {
		return fail(stderr, err)
	}
	raw, err := c.ListSuites(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	if *format == "json" {
		_, err = stdout.Write(raw)
	} else {
		var suites []api.Suite
		if err = json.Unmarshal(raw, &suites); err == nil {
			t := newTextOut(stdout)
			if len(suites) == 0 {
				t.none("suites")
			}
			rows := make([][]string, 0, len(suites))
			for _, s := range suites {
				rows = append(rows, []string{s.ID, s.Created.Local().Format(time.DateTime), s.Config, fmt.Sprint(len(s.Members))})
			}
			t.table(0, []string{"id", "created", "config", "members"}, rows)
			err = t.err()
		}
	}
	if err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

func suiteShowCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("suite show", "--server URL --suite ID [--format text|json]", stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	id := fs.String("suite", "", "the suite's ID")
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	if *server == "" || *id == "" {
		fmt.Fprintln(stderr, "--server and --suite are required: suites are kept by linea-server")
		fs.Usage()
		return exitError
	}
	c, err := connect(*server, false)
	if err != nil {
		return fail(stderr, err)
	}
	raw, err := c.GetSuite(ctx, *id)
	if err != nil {
		return fail(stderr, err)
	}
	if *format == "json" {
		_, err = stdout.Write(raw)
	} else {
		var s api.SuiteStatus
		if err = json.Unmarshal(raw, &s); err == nil {
			outcome := "pending"
			if s.Outcome != "" {
				outcome = string(s.Outcome)
			}
			t := newTextOut(stdout)
			t.line(0, "%s  suite %s", t.status(outcome, 0), s.ID)
			t.field(1, "config", s.Config)
			t.field(1, "done", fmt.Sprint(s.Done))
			if len(s.Members) > 0 {
				t.heading("members")
				rows := make([][]string, 0, len(s.Members))
				for _, m := range s.Members {
					rows = append(rows, []string{m.Label, dash(m.RunID), dash(string(m.State)), strings.ToUpper(dash(string(m.Outcome)))})
				}
				t.table(1, []string{"member", "run", "state", "outcome"}, rows)
			}
			err = t.err()
		}
	}
	if err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

// suiteReportCmd prints a suite's report as test does and exits with its
// outcome.
func suiteReportCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("suite report", "--server URL --suite ID [--wait] [--format text|json]", stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	id := fs.String("suite", "", "the suite's ID")
	wait := fs.Bool("wait", false, "wait for every member to finish; otherwise an unfinished suite is refused")
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	if *server == "" || *id == "" {
		fmt.Fprintln(stderr, "--server and --suite are required: suites are kept by linea-server")
		fs.Usage()
		return exitError
	}
	c, err := connect(*server, false)
	if err != nil {
		return fail(stderr, err)
	}
	raw, err := c.SuiteReport(ctx, *id, *wait)
	if err != nil {
		return fail(stderr, err)
	}
	var rep platform.SuiteReport
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&rep); err != nil {
		return fail(stderr, err)
	}
	if *format == "json" {
		_, err = stdout.Write(raw)
	} else {
		err = writeTestText(stdout, &rep)
	}
	if err != nil {
		return fail(stderr, err)
	}
	return outcomeCode(rep.Outcome)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
