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
	var errs []error
	printf := func(format string, a ...any) {
		_, err := fmt.Fprintf(w, format, a...)
		errs = append(errs, err)
	}
	printf("upgrade check: %s\n", c.Verdict)
	for _, side := range []struct {
		name  string
		rt    platform.UpgradeRuntime
		valid bool
	}{{"from", c.From, c.Configuration.FromValid}, {"to", c.To, c.Configuration.ToValid}} {
		printf("%s %s: Collector %s", side.name, side.rt.Name, side.rt.CollectorVersion)
		if !side.rt.Verified {
			printf(" (unverified)")
		}
		if side.valid {
			printf(", configuration valid\n")
		} else {
			printf(", configuration invalid\n")
		}
	}
	for _, r := range c.Reasons {
		printf("inconclusive: %s: %s\n", r.Code, r.Message)
	}
	for _, d := range c.Configuration.Added {
		printf("new diagnostic: %s %s %s: %s\n", d.Severity, d.Code, d.Component, d.Message)
	}
	for _, d := range c.Configuration.Removed {
		printf("gone diagnostic: %s %s %s: %s\n", d.Severity, d.Code, d.Component, d.Message)
	}
	for _, g := range c.FeatureGates.Changes {
		printf("feature gate %s", g.ID)
		if g.Component != "" {
			printf(" (%s)", g.Component)
		}
		printf(": %s -> %s\n", gateText(g.From), gateText(g.To))
	}
	for _, side := range c.FeatureGates.Unknown {
		printf("feature gates: the %s runtime does not list them\n", side)
	}
	for _, s := range c.Scenarios {
		if s.Error != "" {
			printf("scenario %s: not compared: %s\n", s.Name, s.Error)
			continue
		}
		printf("scenario %s: %s, assertions %s -> %s, runs %s %s\n", s.Name, s.Outcome,
			outcomeText(string(s.BeforeAssertions)), outcomeText(string(s.AfterAssertions)), s.Before, s.After)
	}
	for _, n := range c.Notes {
		printf("note: %s\n", n)
	}
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
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
