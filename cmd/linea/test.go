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

func writeTestText(w io.Writer, rep *platform.SuiteReport) error {
	var errs []error
	printf := func(format string, a ...any) {
		_, err := fmt.Fprintf(w, format, a...)
		errs = append(errs, err)
	}
	counts := map[assertion.Outcome]int{}
	for _, s := range rep.Scenarios {
		counts[s.Outcome]++
		name := s.Name
		if name == "" {
			name = s.Path
		}
		printf("%-12s %s (%s)\n", label(s.Outcome), name, s.Path)
		if s.Error != "" {
			printf("    %s\n", s.Error)
			continue
		}
		if s.Assertions.RunStatus != "completed" {
			printf("    run status: %s\n", s.Assertions.RunStatus)
		}
		for _, r := range s.Assertions.Results {
			printf("  %-12s %s: %s\n", label(r.Outcome), r.Name, r.Target)
			for _, c := range r.Checks {
				if c.Outcome == assertion.Pass {
					continue
				}
				printf("      %s\n", c.Line())
				if c.Message != "" && c.Expected != "" {
					printf("        %s\n", c.Message)
				}
				for _, v := range c.Violations {
					printf("        at %s: %s\n", v.Location, v.Observed)
				}
			}
		}
	}
	printf("\n%s: %d passed, %d failed, %d inconclusive, %d errors\n", label(rep.Outcome),
		counts[assertion.Pass], counts[assertion.Fail], counts[assertion.Inconclusive], counts[assertion.Error])
	return errors.Join(errs...)
}

func label(o assertion.Outcome) string { return strings.ToUpper(string(o)) }
