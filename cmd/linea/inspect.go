package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// inspectCmd validates a configuration and prints its topology. An invalid
// configuration is reported with its diagnostics and exits with exitError.
func inspectCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("inspect", "--config <collector.yaml> [--scenario <scenario.yaml>] [--runtime NAME] [--format text|json] [--server URL]", stderr)
	var config, scenario string
	fs.StringVar(&config, "config", "", "Collector configuration YAML (read-only)")
	fs.StringVar(&scenario, "scenario", "", "scenario whose environment is used to resolve ${env:...} references")
	var runtimeName string
	fs.StringVar(&runtimeName, "runtime", "", "built runtime to run on (see 'runtime list'), or a runtime image reference; empty is the built-in registry")
	runtimeCacheFlag(fs)
	format := formatFlag(fs, "text")
	imports := importEnvFlag(fs)
	server := serverFlag(fs)
	if code, ok := parse(fs, args, format); !ok {
		return code
	}
	if config == "" {
		fmt.Fprintln(stderr, "--config is required")
		fs.Usage()
		return exitError
	}

	if *server != "" {
		c, err := connect(*server, false)
		if err != nil {
			return fail(stderr, err)
		}
		return inspectConnected(ctx, c, config, scenario, runtimeName, *format, stdout, stderr)
	}

	req := platform.InspectRequest{Config: platform.Source{Path: config}, Runtime: runtimeName}
	if scenario != "" {
		req.Scenario = &platform.ScenarioInput{Scenario: platform.Source{Path: scenario}}
	}
	// Inspection starts no Collector and does not use the run queue.
	env, err := localEnvironment(*imports)
	if err != nil {
		return fail(stderr, err)
	}
	service := platform.New(platform.Options{Environment: env, Paths: platform.PathPolicy{Unrestricted: true}, Runtimes: dockerRuntimes()})
	defer service.Close()
	ins, err := service.InspectConfiguration(ctx, cliCaller, req)
	if err != nil {
		return fail(stderr, err)
	}
	if *format == "json" {
		err = platform.Encode(stdout, ins)
	} else {
		err = writeInspection(stdout, ins)
	}
	if err != nil {
		return fail(stderr, err)
	}
	if !ins.Valid {
		return exitError
	}
	return exitOK
}

func writeInspection(w io.Writer, ins *platform.Inspection) error {
	var errs []error
	printf := func(format string, a ...any) {
		_, err := fmt.Fprintf(w, format, a...)
		errs = append(errs, err)
	}
	status := "valid"
	if !ins.Valid {
		status = "INVALID"
	}
	printf("config %s (sha256 %.12s): %s\n", ins.Config.Path, ins.Config.SHA256, status)
	for _, d := range ins.Diagnostics {
		printf("  %s %s", d.Severity, d.Code)
		if d.Component != "" {
			printf(" [%s]", d.Component)
		}
		printf(": %s\n", d.Message)
		if d.Hint != "" {
			printf("    hint: %s\n", d.Hint)
		}
	}
	if g := ins.Topology; g != nil {
		printf("\npipelines:\n")
		for _, p := range g.Pipelines {
			printf("  %s: %s -> %s -> %s\n", p.ID, list(p.Receivers), list(p.Processors), list(p.Exporters))
		}
		printf("\ncomponents:\n")
		for _, c := range g.Components {
			printf("  %-28s %-17s", c.Ref, c.Mode)
			if len(c.Pipelines) > 1 {
				printf(" shared by %s", strings.Join(c.Pipelines, ", "))
			}
			if c.Note != "" {
				printf(" (%s)", c.Note)
			}
			printf("\n")
		}
		if len(g.Unused) > 0 {
			printf("\nunused (not built by the Collector): %s\n", strings.Join(g.Unused, ", "))
		}
	}
	return errors.Join(errs...)
}

func list(ids []string) string {
	if len(ids) == 0 {
		return "(none)"
	}
	return strings.Join(ids, ", ")
}
