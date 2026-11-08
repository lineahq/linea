package main

import (
	"context"
	"fmt"
	"io"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// upgradeCheckCmd checks a configuration and its scenarios on the current
// runtime and a target one and exits with the verdict's code: 0
// compatible, 2 configuration breaks or behavior changes, 4 inconclusive.
func upgradeCheckCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("upgrade-check", "--config <collector.yaml> --to RUNTIME [--from RUNTIME] [--scenario <scenario.yaml>... | --tests <dir>] [flags]", stderr)
	var config, from, to string
	var scenarios, ignore stringsFlag
	fs.StringVar(&config, "config", "", "Collector configuration YAML to check (read-only)")
	fs.StringVar(&from, "from", "", "runtime the configuration runs on today (see 'runtime list'), or a runtime image reference; empty is the built-in registry")
	fs.StringVar(&to, "to", "", "runtime of the release to move to; built-in is the built-in registry")
	runtimeCacheFlag(fs)
	fs.Var(&scenarios, "scenario", "scenario YAML to run on both runtimes (repeatable)")
	dir := fs.String("tests", "", "directory whose *.yaml and *.yml scenarios are run, in name order")
	fs.Var(&ignore, "ignore", `OTTL path whose differences are counted but do not change behavior, such as log.attributes["request.id"] (repeatable)`)
	format := fs.String("format", "text", "output format: text, json or markdown")
	verbose := fs.Bool("verbose", false, "write Collector logs to stderr")
	imports := importEnvFlag(fs)
	server := serverFlag(fs)
	if code, ok := parse(fs, args, nil); !ok {
		return code
	}
	if config == "" || to == "" || (len(scenarios) > 0 && *dir != "") {
		fmt.Fprintln(stderr, "--config and --to are required, with --scenario files or a --tests directory, not both")
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
	var c *platform.UpgradeCheck
	var raw []byte
	var err error
	if *server != "" {
		cl, err := connect(*server, *verbose)
		if err != nil {
			return fail(stderr, err)
		}
		req, err := connectedUpgradeCheck(config, from, to, files, ignore)
		if err != nil {
			return fail(stderr, err)
		}
		if raw, c, err = cl.CheckUpgrade(ctx, req); err != nil {
			return fail(stderr, err)
		}
	} else if c, err = checkUpgradeLocally(ctx, config, from, to, files, ignore, *verbose, *imports); err != nil {
		return fail(stderr, err)
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
		err = writeUpgradeCheck(stdout, c)
	}
	if err != nil {
		return fail(stderr, err)
	}
	return upgradeExit(c.Verdict)
}

// upgradeExit maps a verdict to upgrade-check's exit code.
func upgradeExit(v platform.UpgradeVerdict) int {
	switch v {
	case platform.UpgradeCompatible:
		return exitOK
	case platform.UpgradeConfigurationBreaks, platform.UpgradeBehaviorChanges:
		return exitMismatch
	case platform.UpgradeInconclusive:
		return exitInconclusive
	}
	return exitError
}

func checkUpgradeLocally(ctx context.Context, config, from, to string, scenarios, ignore []string, verbose bool, imports []string) (*platform.UpgradeCheck, error) {
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
	req := platform.UpgradeCheckRequest{From: from, To: to, Config: platform.Source{Path: config}, Ignore: ignore}
	for _, s := range scenarios {
		req.Scenarios = append(req.Scenarios, platform.ScenarioInput{Scenario: platform.Source{Path: s}})
	}
	return service.CheckUpgrade(ctx, cliCaller, req)
}

// connectedUpgradeCheck reads the file arguments for a server check.
func connectedUpgradeCheck(config, from, to string, scenarios, ignore []string) (api.UpgradeCheckRequest, error) {
	req := api.UpgradeCheckRequest{From: from, To: to, ConfigLabel: config, Ignore: ignore}
	var err error
	if req.ConfigYAML, err = readArg("config", config); err != nil {
		return req, err
	}
	for _, s := range scenarios {
		sc := api.SuiteScenario{ScenarioLabel: s}
		if sc.ScenarioYAML, err = scenarioSource(s); err != nil {
			return req, err
		}
		req.Scenarios = append(req.Scenarios, sc)
	}
	return req, nil
}

// writeUpgradeCheck prints the check as plain text; --format markdown
// renders the same facts for a pull request.
func writeUpgradeCheck(w io.Writer, c *platform.UpgradeCheck) error {
	t := newTextOut(w)
	t.line(0, "%s  Collector %s -> %s", t.status(string(c.Verdict), 0), c.From.CollectorVersion, c.To.CollectorVersion)
	for _, side := range []struct {
		name  string
		rt    platform.UpgradeRuntime
		valid bool
	}{{"from", c.From, c.Configuration.FromValid}, {"to", c.To, c.Configuration.ToValid}} {
		desc := side.rt.Name + ", Collector " + side.rt.CollectorVersion
		if !side.rt.Verified {
			desc += " (unverified)"
		}
		if side.valid {
			desc += ", configuration valid"
		} else {
			desc += ", configuration invalid"
		}
		t.field(1, side.name, desc)
	}
	if len(c.Reasons) > 0 {
		t.heading("inconclusive")
		for _, r := range c.Reasons {
			t.line(1, "%s: %s", r.Code, r.Message)
		}
	}
	if len(c.Configuration.Added) > 0 || len(c.Configuration.Removed) > 0 {
		t.heading("diagnostics")
		for _, d := range c.Configuration.Added {
			t.line(1, "new: %s %s %s: %s", d.Severity, d.Code, d.Component, d.Message)
		}
		for _, d := range c.Configuration.Removed {
			t.line(1, "gone: %s %s %s: %s", d.Severity, d.Code, d.Component, d.Message)
		}
	}
	if len(c.FeatureGates.Unknown) > 0 || len(c.FeatureGates.Changes) > 0 {
		t.heading("feature gates")
		for _, side := range c.FeatureGates.Unknown {
			t.line(1, "the %s runtime does not list them", side)
		}
		for _, g := range c.FeatureGates.Changes {
			id := g.ID
			if g.Component != "" {
				id += " (" + g.Component + ")"
			}
			t.line(1, "%s: %s -> %s", id, gateText(g.From), gateText(g.To))
		}
	}
	if len(c.Scenarios) > 0 {
		t.heading("scenarios")
		word := func(s platform.UpgradeScenario) string {
			if s.Error != "" {
				return "error"
			}
			return string(s.Outcome)
		}
		width := statusWidth(c.Scenarios, word)
		for _, s := range c.Scenarios {
			t.line(1, "%s  %s", t.status(word(s), width), s.Name)
			if s.Error != "" {
				t.field(2, "not compared", s.Error)
				continue
			}
			t.line(2, "assertions: %s -> %s", outcomeText(string(s.BeforeAssertions)), outcomeText(string(s.AfterAssertions)))
			t.line(2, "runs: before %s, after %s", s.Before, s.After)
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

func gateText(g *platform.UpgradeGateState) string {
	if g == nil {
		return "absent"
	}
	if g.Enabled {
		return g.Stage + " on"
	}
	return g.Stage + " off"
}
