package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
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
			tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "SUITE\tCREATED\tCONFIG\tMEMBERS")
			for _, s := range suites {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\n", s.ID, s.Created.Local().Format(time.DateTime), s.Config, len(s.Members))
			}
			err = tw.Flush()
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
			fmt.Fprintf(stdout, "suite %s on %s: done %t, outcome %s\n", s.ID, s.Config, s.Done, outcome)
			tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "MEMBER\tRUN\tSTATE\tOUTCOME")
			for _, m := range s.Members {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", m.Label, dash(m.RunID), dash(string(m.State)), dash(string(m.Outcome)))
			}
			err = tw.Flush()
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
