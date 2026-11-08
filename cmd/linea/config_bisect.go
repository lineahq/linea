package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// configBisectCmd finds the fleet config version that made the scenarios
// fail on an agent and exits with the verdict's code: 0 found, 2 no
// regression or already bad, 4 inconclusive.
func configBisectCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("config-bisect", "--agent ID [--config ID] [--good N] [--bad N] (--scenario <scenario.yaml>... | --tests <dir>) --server URL [flags]", stderr)
	var agent, configID, runtime string
	var scenarios, local, ignore stringsFlag
	fs.StringVar(&agent, "agent", "", "fleet agent whose configuration is rebuilt")
	fs.StringVar(&configID, "config", "", "fleet config to search; empty when the agent receives exactly one layer")
	good := fs.Int("good", 0, "version known to pass; 0 is the oldest version fleet keeps")
	bad := fs.Int("bad", 0, "version known to fail; 0 is the version fleet sends the agent now")
	fs.Var(&scenarios, "scenario", "scenario YAML with assertions to run on each probed version (repeatable)")
	dir := fs.String("tests", "", "directory whose *.yaml and *.yml scenarios are run, in name order")
	fs.Var(&local, "local", "the supervisor's local config file, merged under fleet's layers (repeatable, in the supervisor's order)")
	fs.StringVar(&runtime, "runtime", "", "runtime to run the probes on (see 'runtime list'); empty is the server's default")
	fs.Var(&ignore, "ignore", "OTTL path whose differences do not count when the culprit's runs are compared (repeatable)")
	format := fs.String("format", "text", "output format: text, json or markdown")
	verbose := fs.Bool("verbose", false, "write Collector logs to stderr")
	server := serverFlag(fs)
	if code, ok := parse(fs, args, nil); !ok {
		return code
	}
	if agent == "" || (len(scenarios) > 0) == (*dir != "") || *server == "" {
		fmt.Fprintln(stderr, "--agent and --server are required, with --scenario files or a --tests directory, not both")
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
	files := []string(scenarios)
	if *dir != "" {
		var err error
		if files, err = scenarioFiles(*dir); err != nil {
			return fail(stderr, err)
		}
	}
	req := platform.BisectRequest{AgentID: agent, ConfigID: configID, Good: *good, Bad: *bad, Runtime: runtime, Ignore: ignore}
	cl, err := connect(*server, *verbose)
	if err != nil {
		return fail(stderr, err)
	}
	creq, err := connectedBisect(req, files, local)
	if err != nil {
		return fail(stderr, err)
	}
	raw, b, err := cl.BisectConfig(ctx, creq)
	if err != nil {
		return fail(stderr, err)
	}
	switch *format {
	case "json":
		_, err = stdout.Write(raw)
	case "markdown":
		_, err = io.WriteString(stdout, b.Markdown())
	default:
		err = writeBisection(stdout, b)
	}
	if err != nil {
		return fail(stderr, err)
	}
	return bisectExit(b.Verdict)
}

// bisectExit maps a verdict to config-bisect's exit code.
func bisectExit(v platform.BisectVerdict) int {
	switch v {
	case platform.BisectFound:
		return exitOK
	case platform.BisectNoRegression, platform.BisectAlreadyBad:
		return exitMismatch
	case platform.BisectInconclusive:
		return exitInconclusive
	}
	return exitError
}

// connectedBisect reads the file arguments for a server bisect.
func connectedBisect(req platform.BisectRequest, scenarios, local []string) (api.ConfigBisectRequest, error) {
	out := api.ConfigBisectRequest{Agent: req.AgentID, Config: req.ConfigID, Good: req.Good, Bad: req.Bad, Runtime: req.Runtime, Ignore: req.Ignore}
	var err error
	for _, s := range scenarios {
		sc := api.SuiteScenario{ScenarioLabel: s}
		if sc.ScenarioYAML, err = scenarioSource(s); err != nil {
			return out, err
		}
		out.Scenarios = append(out.Scenarios, sc)
	}
	for _, l := range local {
		f := api.BisectLocalFile{Label: l}
		if f.YAML, err = readArg("local", l); err != nil {
			return out, err
		}
		out.Local = append(out.Local, f)
	}
	return out, nil
}

// writeBisection prints the bisection as plain text; --format markdown
// renders the same facts for an incident note.
func writeBisection(w io.Writer, b *platform.Bisection) error {
	t := newTextOut(w)
	name := b.Target.ConfigID
	if b.Target.ConfigName != "" {
		name = b.Target.ConfigName + " (" + b.Target.ConfigID + ")"
	}
	detail := ""
	switch b.Verdict {
	case platform.BisectFound:
		detail = fmt.Sprintf(": first bad version %d, last good %d", b.FirstBad, b.LastGood)
	case platform.BisectInconclusive:
		if b.FirstBad > 0 && b.LastGood > 0 {
			detail = fmt.Sprintf(": first bad version between %d and %d", b.LastGood+1, b.FirstBad)
		}
	}
	t.line(0, "%s  agent %s%s", t.status(string(b.Verdict), 0), b.Target.AgentID, detail)
	t.field(1, "config", name)
	t.field(1, "layer", b.Target.Entry)
	t.field(1, "versions", fmt.Sprintf("%d to %d", b.Good, b.Bad))
	t.field(1, "probes", fmt.Sprint(b.Probes))
	if len(b.Reasons) > 0 {
		t.heading("inconclusive")
		for _, r := range b.Reasons {
			t.line(1, "%s: %s", r.Code, r.Message)
		}
	}
	var tested []platform.BisectVersion
	for _, v := range b.Versions {
		if v.Status != platform.BisectUntested {
			tested = append(tested, v)
		}
	}
	if len(tested) > 0 {
		t.heading("versions")
		width := statusWidth(tested, func(v platform.BisectVersion) string { return string(v.Status) })
		for _, v := range tested {
			reason := ""
			if v.Reason != "" {
				reason = " (" + v.Reason + ")"
			}
			t.line(1, "%s  version %d%s", t.status(string(v.Status), width), v.Version, reason)
			for _, s := range v.Scenarios {
				t.line(2, "%s %s %s", s.Name, outcomeText(string(s.Outcome)), s.Run)
			}
		}
	}
	if c := b.Culprit; c != nil {
		t.heading("culprit")
		for _, s := range c.Scenarios {
			var extra string
			switch {
			case s.Error != "":
				extra = ", not compared: " + s.Error
			case s.Comparison != nil:
				extra = ", comparison " + string(s.Comparison.Outcome)
			}
			t.line(1, "%s: assertions %s -> %s, runs %s %s%s", s.Name, outcomeText(string(s.GoodAssertions)), outcomeText(string(s.BadAssertions)), s.Good, s.Bad, extra)
		}
		t.heading("culprit diff")
		if c.Diff == "" {
			t.none("differences")
		} else {
			t.printf("%s", strings.TrimSuffix(c.Diff, "\n")+"\n")
		}
	}
	if len(b.Notes) > 0 {
		t.heading("notes")
		for _, n := range b.Notes {
			t.line(1, "%s", n)
		}
	}
	return t.err()
}
