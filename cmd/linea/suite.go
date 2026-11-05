package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/lineahq/linea/internal/debugger/platform"
)

const suiteSynopsis = "list --server URL [--format text|json] | show|report --server URL --suite ID [--wait] [--format text|json] | rerun --server URL --suite ID [--report <file.xml|file.json>] [--format text|json] | junit --server URL --suite ID --out <file.xml> | delete --server URL --suite ID [--format text|json]"

func suiteUsage(w io.Writer) {
	fmt.Fprintf(w, "usage: linea suite %s\n", suiteSynopsis)
}

// suiteCmd dispatches the subcommands for the suites a team server
// retains: list them, show one or its report, rerun one from its
// recorded inputs, write its JUnit report, or delete one with its
// member runs.
func suiteCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		suiteUsage(stderr)
		return exitError
	}
	rest := args[1:]
	switch args[0] {
	case "help", "-h", "-help", "--help":
		suiteUsage(stdout)
		return exitOK
	case "list":
		return suiteListCmd(ctx, rest, stdout, stderr)
	case "show":
		return suiteShowCmd(ctx, rest, stdout, stderr)
	case "report":
		return suiteReportCmd(ctx, rest, stdout, stderr)
	case "rerun":
		return suiteRerunCmd(ctx, rest, stdout, stderr)
	case "junit":
		return suiteJUnitCmd(ctx, rest, stdout, stderr)
	case "delete":
		return suiteDeleteCmd(ctx, rest, stdout, stderr)
	}
	fmt.Fprintf(stderr, "unknown suite command %q\n", args[0])
	suiteUsage(stderr)
	return exitError
}

// suiteRerunCmd reruns a suite from its members' recorded inputs, waits for
// the new suite and prints its report as test does, exiting with its outcome.
func suiteRerunCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("suite rerun", "--server URL --suite ID [--report <file.xml|file.json>] [--format text|json]", stderr)
	format := formatFlag(fs, "text")
	server := serverFlag(fs)
	id := fs.String("suite", "", "suite to rerun")
	report := fs.String("report", "", "also write the report to this file: .xml (JUnit) or .json")
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	if *server == "" || *id == "" {
		fmt.Fprintln(stderr, "--server and --suite are required: suites are kept by linea-server")
		fs.Usage()
		return exitError
	}
	ext := strings.ToLower(filepath.Ext(*report))
	if *report != "" && ext != ".xml" && ext != ".json" {
		fmt.Fprintf(stderr, "--report %s: use a .xml (JUnit) or .json file\n", *report)
		fs.Usage()
		return exitError
	}
	c, err := connect(*server, false)
	if err != nil {
		return fail(stderr, err)
	}
	rep, err := c.RerunSuite(ctx, *id)
	if err != nil {
		return fail(stderr, err)
	}
	if *format == "json" {
		err = platform.Encode(stdout, rep)
	} else {
		err = writeTestText(stdout, rep)
	}
	if err != nil {
		return fail(stderr, err)
	}
	if *report != "" {
		if err := writeReportFile(*report, ext, rep); err != nil {
			return fail(stderr, err)
		}
	}
	return outcomeCode(rep.Outcome)
}

// suiteJUnitCmd writes a finished suite's JUnit XML to a new file.
func suiteJUnitCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("suite junit", "--server URL --suite ID --out <file.xml>", stderr)
	server := serverFlag(fs)
	id := fs.String("suite", "", "finished suite to export")
	out := fs.String("out", "", "JUnit file to create; an existing file is never overwritten")
	if code, ok := parse(fs, args, nil); !ok {
		return code
	}
	if *server == "" || *id == "" || *out == "" {
		fmt.Fprintln(stderr, "--server, --suite and --out are required")
		fs.Usage()
		return exitError
	}
	c, err := connect(*server, false)
	if err != nil {
		return fail(stderr, err)
	}
	raw, err := c.SuiteJUnit(ctx, *id)
	if err != nil {
		return fail(stderr, err)
	}
	if err := writeNew(*out, string(raw)); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "wrote %s from suite %s\n", *out, *id)
	return exitOK
}
