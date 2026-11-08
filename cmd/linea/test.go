package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/client"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// stringsFlag collects a repeatable flag.
type stringsFlag []string

func (s *stringsFlag) String() string     { return strings.Join(*s, ",") }
func (s *stringsFlag) Set(v string) error { *s = append(*s, v); return nil }

// testCmd runs scenario assertions and exits with the outcome's code.
func testCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("test", "--config <collector.yaml> (--scenario <scenario.yaml>... | --tests <dir>) [--report <file.xml|file.json>] [flags]", stderr)
	var req platform.SuiteRequest
	var scenarios stringsFlag
	fs.StringVar(&req.Config, "config", "", "Collector configuration YAML (read-only)")
	fs.Var(&scenarios, "scenario", "scenario YAML with assertions (repeatable)")
	fs.StringVar(&req.Runtime, "runtime", "", "built runtime to run on (see 'runtime list'), or a runtime image reference; empty is the built-in registry")
	runtimeCacheFlag(fs)
	dir := fs.String("tests", "", "directory whose *.yaml and *.yml scenarios are run, in name order")
	report := fs.String("report", "", "also write the report to a file: JUnit XML for .xml, JSON for .json")
	format := formatFlag(fs, "text")
	verbose := fs.Bool("verbose", false, "write Collector logs to stderr")
	imports := importEnvFlag(fs)
	server := serverFlag(fs)
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	req.Scenarios = scenarios
	if *dir != "" {
		found, err := scenarioFiles(*dir)
		if err != nil {
			return fail(stderr, err)
		}
		req.Scenarios = append(req.Scenarios, found...)
	}
	if req.Config == "" || len(req.Scenarios) == 0 {
		fmt.Fprintln(stderr, "--config and at least one --scenario or a --tests directory with scenarios are required")
		fs.Usage()
		return exitError
	}
	ext := strings.ToLower(filepath.Ext(*report))
	if *report != "" && ext != ".xml" && ext != ".json" {
		fmt.Fprintf(stderr, "--report %s: use a .xml (JUnit) or .json file\n", *report)
		fs.Usage()
		return exitError
	}
	var rep *platform.SuiteReport
	var err error
	if *server != "" {
		var c *client.Client
		if c, err = connect(*server, *verbose); err == nil {
			rep, err = testConnected(ctx, c, req)
		}
	} else {
		rep, err = testLocal(ctx, req, *verbose, *imports)
	}
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

func testLocal(ctx context.Context, req platform.SuiteRequest, verbose bool, imports []string) (*platform.SuiteReport, error) {
	logger, err := newLogger(verbose)
	if err != nil {
		return nil, err
	}
	env, err := localEnvironment(imports)
	if err != nil {
		return nil, err
	}
	service := platform.New(platform.Options{Logger: logger, Environment: env, Paths: platform.PathPolicy{Unrestricted: true}, Runtimes: dockerRuntimes()})
	defer service.Close()
	return service.RunSuite(ctx, cliCaller, req)
}

func outcomeCode(o assertion.Outcome) int {
	switch o {
	case assertion.Pass:
		return exitOK
	case assertion.Fail:
		return exitFail
	case assertion.Inconclusive:
		return exitInconclusive
	}
	return exitError
}

func scenarioFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if !e.IsDir() && (ext == ".yaml" || ext == ".yml") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--tests %s: no .yaml or .yml scenarios", dir)
	}
	slices.Sort(out)
	return out, nil
}

func writeReportFile(path, ext string, rep *platform.SuiteReport) (err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	if ext == ".json" {
		return platform.Encode(f, rep)
	}
	return rep.WriteJUnit(f)
}

// writeTestText prints the suite's verdict and counts first, then each
// scenario with its assertions and the checks that did not pass.
func writeTestText(w io.Writer, rep *platform.SuiteReport) error {
	t := newTextOut(w)
	counts := map[assertion.Outcome]int{}
	for _, s := range rep.Scenarios {
		counts[s.Outcome]++
	}
	t.line(0, "%s  %s: %d passed, %d failed, %d inconclusive, %d errors", t.status(string(rep.Outcome), 0),
		plural(len(rep.Scenarios), "scenario"), counts[assertion.Pass], counts[assertion.Fail], counts[assertion.Inconclusive], counts[assertion.Error])
	if len(rep.Scenarios) == 0 {
		return t.err()
	}
	t.heading("scenarios")
	width := 0
	for _, s := range rep.Scenarios {
		width = max(width, len(s.Outcome))
	}
	for _, s := range rep.Scenarios {
		name := s.Name
		if name == "" {
			name = s.Path
		}
		t.line(1, "%s  %s  (%s)", t.status(string(s.Outcome), width), name, s.Path)
		if s.Error != "" {
			t.field(2, "error", s.Error)
			continue
		}
		if s.Assertions.RunStatus != "completed" {
			t.field(2, "run status", string(s.Assertions.RunStatus))
		}
		writeAssertionResults(t, 2, s.Assertions.Results, true)
	}
	return t.err()
}

// writeAssertionResults prints each assertion's outcome and target, and
// under it the checks that did not pass; detail adds their messages and
// violations.
func writeAssertionResults(t *textOut, depth int, results []assertion.Result, detail bool) {
	width := statusWidth(results, func(r assertion.Result) string { return string(r.Outcome) })
	for _, r := range results {
		t.line(depth, "%s  %s: %s", t.status(string(r.Outcome), width), r.Name, r.Target)
		for _, c := range r.Checks {
			if c.Outcome == assertion.Pass {
				continue
			}
			t.line(depth+2, "%s", c.Line())
			if !detail {
				continue
			}
			if c.Message != "" && c.Expected != "" {
				t.line(depth+3, "%s", c.Message)
			}
			for _, v := range c.Violations {
				t.line(depth+3, "at %s: %s", v.Location, v.Observed)
			}
		}
	}
}

func label(o assertion.Outcome) string { return strings.ToUpper(string(o)) }
