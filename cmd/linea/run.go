package main

import (
	"context"
	"fmt"
	"io"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// runCmd replays a fixture or scenario and prints the engine report.
func runCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("run", "--config <collector.yaml> (--fixture <otlp.json> | --scenario <scenario.yaml>) [flags]", stderr)
	var config, fixture, scenario, receiver string
	var req platform.RunRequest
	fs.StringVar(&config, "config", "", "Collector configuration YAML (read-only)")
	fs.StringVar(&fixture, "fixture", "", "OTLP JSON fixture (traces, metrics or logs)")
	fs.StringVar(&scenario, "scenario", "", "scenario YAML declaring inputs and environment (instead of --fixture)")
	fs.StringVar(&receiver, "receiver", "", "receiver to inject --fixture into when several feed its signal")
	fs.BoolVar(&req.Compare, "compare", false, "also run an uninstrumented reference Collector over loopback OTLP and compare final output")
	fs.BoolVar(&req.Normalize, "normalize", false, "canonical order and fixed run IDs and times, for golden files")
	fs.StringVar(&req.Runtime, "runtime", "", "built runtime to run on (see 'runtime list'), or a runtime image reference; empty is the built-in registry")
	runtimeCacheFlag(fs)
	verify := fs.Bool("verify-recommendations", false, "run each suggested change on the run's inputs and compare (one more run per change)")
	format := fs.String("format", "", "output format: json or text (default: text on a terminal, json otherwise)")
	verbose := fs.Bool("verbose", false, "write Collector logs to stderr")
	imports := importEnvFlag(fs)
	server := serverFlag(fs)
	if code, ok := parse(fs, args, nil); !ok {
		return code
	}
	if *format == "" {
		// A person reads the report; a pipe or a file gets the full JSON.
		*format = "json"
		if terminal(stdout) {
			*format = "text"
		}
	}
	if code, ok := checkFormat(fs, *format); !ok {
		return code
	}
	if config == "" || (fixture == "") == (scenario == "") {
		fmt.Fprintln(stderr, "--config and exactly one of --fixture or --scenario are required")
		fs.Usage()
		return exitError
	}
	if *server != "" {
		c, err := connect(*server, *verbose)
		if err != nil {
			return fail(stderr, err)
		}
		return runConnected(ctx, c, api.Request{ConfigLabel: config, FixtureLabel: fixture, ScenarioLabel: scenario, Receiver: receiver,
			Compare: req.Compare, Normalize: req.Normalize, Runtime: req.Runtime}, *format, *verify, stdout, stderr)
	}
	req.Config = platform.Source{Path: config}
	if fixture != "" {
		req.Mode, req.Quick = platform.ModeQuick, &platform.QuickInput{Fixture: platform.Source{Path: fixture}, Receiver: receiver}
	} else {
		req.Mode, req.Scenario = platform.ModeScenario, &platform.ScenarioInput{Scenario: platform.Source{Path: scenario}}
		if receiver != "" {
			// --receiver belongs to a quick run; the platform rejects the mix.
			req.Quick = &platform.QuickInput{Receiver: receiver}
		}
	}
	logger, err := newLogger(*verbose)
	if err != nil {
		return fail(stderr, err)
	}

	env, err := localEnvironment(*imports)
	if err != nil {
		return fail(stderr, err)
	}
	service := platform.New(platform.Options{Logger: logger, Environment: env, Paths: platform.PathPolicy{Unrestricted: true}, Runtimes: dockerRuntimes()})
	defer service.Close()
	var rep *platform.Report
	var verified []verification
	if !*verify {
		rep, err = service.ExecuteRun(ctx, cliCaller, req)
	} else {
		// A verification names a stored run; submit to keep it.
		var run platform.Run
		if run, err = service.SubmitRun(ctx, cliCaller, req); err == nil {
			if rep, err = service.GetRunReport(ctx, cliCaller, run.ID, true); err == nil {
				verified, err = verifyAll(rep, verifyLocal(ctx, service, run.ID))
			}
		}
	}
	if err != nil {
		return fail(stderr, err)
	}
	if err := writeRun(stdout, stderr, *format, rep, nil, verified); err != nil {
		return fail(stderr, err)
	}
	if rep.Mismatch() {
		return exitMismatch
	}
	return exitOK
}

// writeRun prints a run's report: raw as the server sent it when given and
// no verification changed it, and the verifications in text after the
// report, or on stderr beside JSON.
func writeRun(stdout, stderr io.Writer, format string, rep *platform.Report, raw []byte, verified []verification) error {
	var err error
	switch {
	case format != "json":
		err = writeText(stdout, rep)
		if err == nil {
			err = writeVerifications(stdout, verified)
		}
		return err
	case raw != nil && verified == nil:
		_, err = stdout.Write(raw)
	default:
		err = platform.Encode(stdout, rep)
	}
	if err == nil {
		err = writeVerifications(stderr, verified)
	}
	return err
}
