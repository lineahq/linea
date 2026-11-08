package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// diffCmd compares two configurations on one input, or two server runs, and
// exits with the outcome's code: 0 same, 2 different, 4 inconclusive.
func diffCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("diff", "--before <collector.yaml> --after <collector.yaml> (--fixture <otlp.json> | --scenario <scenario.yaml>) [flags]\n       linea diff --server URL --runs <before-run-id> <after-run-id> [flags]", stderr)
	var before, after, fixture, scenario, receiver, runs string
	var ignore stringsFlag
	fs.StringVar(&before, "before", "", "Collector configuration YAML before the change (read-only)")
	fs.StringVar(&after, "after", "", "Collector configuration YAML after the change (read-only)")
	fs.StringVar(&fixture, "fixture", "", "OTLP JSON fixture both configurations receive")
	fs.StringVar(&scenario, "scenario", "", "scenario YAML both configurations run (instead of --fixture)")
	fs.StringVar(&receiver, "receiver", "", "receiver to inject --fixture into when several feed its signal")
	fs.StringVar(&runs, "runs", "", "compare two finished runs of --server instead: --runs <before> <after>")
	fs.Var(&ignore, "ignore", `OTTL path whose differences are counted but do not make the runs different, such as log.attributes["request.id"] (repeatable)`)
	format := fs.String("format", "text", "output format: text, json or markdown")
	verbose := fs.Bool("verbose", false, "write Collector logs to stderr")
	imports := importEnvFlag(fs)
	server := serverFlag(fs)
	var runtimeName, afterRuntime string
	fs.StringVar(&runtimeName, "runtime", "", "built runtime both configurations run on (see 'runtime list'), or a runtime image reference; empty is the built-in registry")
	fs.StringVar(&afterRuntime, "after-runtime", "", "built runtime of the --after run; defaults to --runtime")
	runtimeCacheFlag(fs)
	var afterRun string
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if runs != "" && fs.NArg() > 0 {
		// --runs takes two run IDs; flags may follow the second.
		afterRun = fs.Arg(0)
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return parseExit(err)
		}
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return exitError
	}
	switch *format {
	case "text", "json", "markdown":
	default:
		fmt.Fprintf(stderr, "unknown format %q\n", *format)
		fs.Usage()
		return exitError
	}
	var c *platform.Comparison
	var raw []byte
	var err error
	switch {
	case runs != "":
		if afterRun == "" || before != "" || after != "" || fixture != "" || scenario != "" || receiver != "" || runtimeName != "" || afterRuntime != "" {
			fmt.Fprintln(stderr, "--runs takes two run IDs and no configurations or inputs")
			fs.Usage()
			return exitError
		}
		if *server == "" {
			fmt.Fprintln(stderr, "--runs compares runs of a server: --server is required")
			return exitError
		}
		cl, err := connect(*server, *verbose)
		if err != nil {
			return fail(stderr, err)
		}
		raw, c, err = cl.CompareRuns(ctx, runs, afterRun, ignore)
		if err != nil {
			return fail(stderr, err)
		}
	case before == "" || after == "" || (fixture == "") == (scenario == ""):
		fmt.Fprintln(stderr, "--before, --after and exactly one of --fixture or --scenario are required, or --server with --runs")
		fs.Usage()
		return exitError
	case *server != "":
		cl, err := connect(*server, *verbose)
		if err != nil {
			return fail(stderr, err)
		}
		req, err := connectedComparison(before, after, fixture, scenario, receiver, runtimeName, afterRuntime)
		if err != nil {
			return fail(stderr, err)
		}
		raw, c, err = cl.CompareConfigs(ctx, req, ignore)
		if err != nil {
			return fail(stderr, err)
		}
	default:
		if c, err = compareLocally(ctx, before, after, fixture, scenario, receiver, runtimeName, afterRuntime, ignore, *verbose, *imports); err != nil {
			return fail(stderr, err)
		}
	}
	switch *format {
	case "json":
		if raw != nil {
			_, err = stdout.Write(raw)
		} else {
			err = platform.Encode(stdout, c)
		}
	case "markdown":
		_, err = io.WriteString(stdout, c.Markdown())
	default:
		err = writeComparison(stdout, c)
	}
	if err != nil {
		return fail(stderr, err)
	}
	return comparisonExit(c.Outcome)
}

// parseExit is the exit code of a failed parse: -h is not an error.
func parseExit(err error) int {
	if err == flag.ErrHelp {
		return exitOK
	}
	return exitError
}

// comparisonExit maps an outcome to diff's exit code.
func comparisonExit(o platform.ComparisonOutcome) int {
	switch o {
	case platform.ComparisonSame:
		return exitOK
	case platform.ComparisonDifferent:
		return exitMismatch
	case platform.ComparisonInconclusive:
		return exitInconclusive
	}
	return exitError
}

// compareLocally runs both configurations in-process, like run, and
// compares them once both are finished.
func compareLocally(ctx context.Context, before, after, fixture, scenario, receiver, runtimeName, afterRuntime string, ignore []string, verbose bool, imports []string) (*platform.Comparison, error) {
	req := platform.ComparisonRequest{Before: platform.Source{Path: before}, After: platform.Source{Path: after}, Runtime: runtimeName, AfterRuntime: afterRuntime}
	if fixture != "" {
		req.Mode, req.Quick = platform.ModeQuick, &platform.QuickInput{Fixture: platform.Source{Path: fixture}, Receiver: receiver}
	} else {
		req.Mode, req.Scenario = platform.ModeScenario, &platform.ScenarioInput{Scenario: platform.Source{Path: scenario}}
		if receiver != "" {
			// --receiver belongs to a quick run; the platform rejects the mix.
			req.Quick = &platform.QuickInput{Receiver: receiver}
		}
	}
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
	runs, err := service.SubmitComparison(ctx, cliCaller, req)
	if err != nil {
		return nil, err
	}
	for _, id := range []string{runs.Before.ID, runs.After.ID} {
		// A failed run is reported by the comparison; only an interrupt ends
		// the wait.
		if _, err := service.GetRunReport(ctx, cliCaller, id, true); err != nil && ctx.Err() != nil {
			return nil, err
		}
	}
	return service.CompareRuns(ctx, cliCaller, platform.CompareRequest{Before: runs.Before.ID, After: runs.After.ID, Ignore: ignore})
}

// connectedComparison reads the file arguments for a server comparison.
func connectedComparison(before, after, fixture, scenario, receiver, runtimeName, afterRuntime string) (api.ComparisonRequest, error) {
	req := api.ComparisonRequest{Before: api.ComparisonConfig{ConfigLabel: before}, After: api.ComparisonConfig{ConfigLabel: after}, Receiver: receiver, Runtime: runtimeName, AfterRuntime: afterRuntime}
	var err error
	if req.Before.ConfigYAML, err = readArg("before", before); err != nil {
		return req, err
	}
	if req.After.ConfigYAML, err = readArg("after", after); err != nil {
		return req, err
	}
	if fixture != "" {
		req.FixtureLabel = fixture
		req.FixtureJSON, err = readArg("fixture", fixture)
		return req, err
	}
	req.ScenarioLabel = scenario
	req.ScenarioYAML, err = scenarioSource(scenario)
	return req, err
}

// writeComparison prints the comparison as plain text; --format markdown
// renders the same facts for a review.
func writeComparison(w io.Writer, c *platform.Comparison) error {
	t := newTextOut(w)
	t.line(0, "%s  before %s -> after %s", t.status(string(c.Outcome), 0), c.Before.ID, c.After.ID)
	for _, side := range []struct {
		name string
		run  platform.ComparisonRun
	}{{"before", c.Before}, {"after", c.After}} {
		r := side.run
		var parts []string
		if r.Mode != "" {
			parts = append(parts, r.Mode)
		}
		parts = append(parts, string(r.End))
		if r.ExecutionStatus != "" {
			parts = append(parts, "execution "+string(r.ExecutionStatus))
		}
		if !r.Complete {
			parts = append(parts, "incomplete: "+r.Incomplete)
		}
		if r.Assertions != "" {
			parts = append(parts, "assertions "+string(r.Assertions))
		}
		t.field(1, side.name, strings.Join(parts, ", "))
	}
	if c.InputsIdentical {
		t.field(1, "inputs", "identical")
	} else {
		t.field(1, "inputs", "not proven identical")
	}
	if len(c.Reasons) > 0 {
		t.heading("inconclusive")
		for _, r := range c.Reasons {
			t.line(1, "%s: %s", r.Code, r.Message)
		}
	}
	if len(c.Exporters) > 0 {
		t.heading("exporters")
		for _, e := range c.Exporters {
			t.line(1, "%s (%s): %d -> %d record(s), %s", e.Exporter, e.Signal, e.Before, e.After, exporterText(e))
			for _, g := range e.Changes {
				t.line(2, "%s %s in %d record(s): %s -> %s", g.Path, g.Op, g.Count, valueText(g.Before), valueText(g.After))
			}
			if e.ChangesTruncated {
				t.line(2, "more changes are not listed")
			}
		}
	}
	if r := c.Records; r != nil {
		t.heading("injected records")
		t.line(1, "%d aligned, %d changed, %d unaligned, %d uncertain", r.Aligned, r.Changed, r.Unaligned, r.Uncertain)
		// Each changed record followed in both runs.
		for _, it := range r.Items {
			detail := ""
			if it.DivergesAt != "" {
				detail = ": diverges at " + it.DivergesAt
			}
			t.line(1, "record %d %s (%s %q)%s", it.Input, it.Ref, it.Signal, it.Name, detail)
			t.line(2, "before: %s", pathText(it.Before))
			t.line(2, "after:  %s", pathText(it.After))
		}
		if r.Truncated {
			t.line(1, "more changed records are not listed")
		}
	}
	var routes []platform.ComparisonRoute
	for _, r := range c.Routes {
		if r.Changed {
			routes = append(routes, r)
		}
	}
	if len(routes) > 0 {
		t.heading("routes")
		for _, r := range routes {
			t.line(1, "%s -> %s: %d -> %d record(s)", r.Connector, r.Pipeline, r.Before, r.After)
		}
	}
	if len(c.Errors) > 0 {
		t.heading("errors")
		for _, e := range c.Errors {
			where := e.Component
			if e.Pipeline != "" {
				where += " (" + e.Pipeline + ")"
			}
			t.line(1, "at %s %s: %q %d -> %d time(s)", e.Source, where, e.Message, e.Before, e.After)
		}
	}
	var assertions []platform.ComparisonAssertion
	for _, a := range c.Assertions {
		if a.Changed || a.Before == "" || a.After == "" {
			assertions = append(assertions, a)
		}
	}
	if len(assertions) > 0 {
		t.heading("assertions")
		for _, a := range assertions {
			t.line(1, "%q: %s -> %s", a.Name, outcomeText(string(a.Before)), outcomeText(string(a.After)))
		}
	}
	if r := c.Recommendations; r != nil {
		t.heading("recommendations")
		t.line(1, "%s", r.Counts())
		var items []platform.ComparisonRecommendation
		for _, d := range r.Items {
			if d.Status != platform.ComparisonRecommendationPersists {
				items = append(items, d)
			}
		}
		width := statusWidth(items, func(d platform.ComparisonRecommendation) string { return d.Status })
		for _, d := range items {
			where := strings.Join(d.Components, ", ")
			if d.Pipeline != "" {
				where += " (" + d.Pipeline + ")"
			}
			if d.Subject != "" {
				where += ": " + d.Subject
			}
			t.line(1, "%s  %s at %s", t.status(d.Status, width), d.Rule, where)
		}
	}
	if len(c.Ignored) > 0 {
		t.heading("ignored")
		for _, i := range c.Ignored {
			t.line(1, "%s: %d difference(s)", i.Rule, i.Count)
		}
	}
	if len(c.Notes) > 0 {
		t.heading("notes")
		for _, n := range c.Notes {
			t.line(1, "%s", n)
		}
	}
	return t.err()
}

// pathText lists the components a record passed through in one run, with the
// number of fields a stage changed (~N) and where a path ended.
func pathText(f platform.ComparisonFate) string {
	steps := make([]string, 0, len(f.Path))
	for _, s := range f.Path {
		step := s.Component
		if s.Pipeline != "" {
			step += " (" + s.Pipeline + ")"
		}
		if s.Changes > 0 {
			step += fmt.Sprintf(" ~%d", s.Changes)
		}
		if s.End != "" {
			step += " " + string(s.End)
		}
		steps = append(steps, step)
	}
	if len(steps) == 0 {
		return "(not followed)"
	}
	return strings.Join(steps, " -> ")
}

func exporterText(e platform.ComparisonExporter) string {
	switch {
	case e.OnlyIn != "":
		return "output only in the " + e.OnlyIn + " run"
	case e.Equal && e.Note != "":
		return "same records, different order"
	case e.Equal:
		return "same records"
	case e.EqualAfterIgnore:
		return "same apart from ignored paths"
	case e.Before == e.After:
		return "different records, same count"
	}
	return "different records"
}

func valueText(v *platform.SummaryValue) string {
	if v == nil {
		return "none"
	}
	var s string
	if str, ok := v.Value.(string); ok {
		s = strconv.Quote(str)
	} else {
		raw, _ := json.Marshal(v.Value)
		s = string(raw)
	}
	if v.Truncated {
		s += "..."
	}
	return s
}

func outcomeText(o string) string {
	if o == "" {
		return "absent"
	}
	return o
}
