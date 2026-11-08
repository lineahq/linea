package main

import (
	"context"
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

// writeInspection prints the verdict, the diagnostics, the pipelines and
// how each component runs.
func writeInspection(w io.Writer, ins *platform.Inspection) error {
	t := newTextOut(w)
	status := "valid"
	if !ins.Valid {
		status = "invalid"
	}
	t.line(0, "%s  config %s", t.status(status, 0), ins.Config.Path)
	t.field(1, "sha256", fmt.Sprintf("%.12s", ins.Config.SHA256))
	if len(ins.Diagnostics) > 0 {
		t.heading("diagnostics")
		for _, d := range ins.Diagnostics {
			where := ""
			if d.Component != "" {
				where = " [" + d.Component + "]"
			}
			t.line(1, "%s %s%s: %s", t.status(string(d.Severity), 0), d.Code, where, d.Message)
			t.field(2, "hint", d.Hint)
		}
	}
	if g := ins.Topology; g != nil {
		t.heading("pipelines")
		var rows [][]string
		for _, p := range g.Pipelines {
			rows = append(rows, []string{p.ID, list(p.Receivers) + " -> " + list(p.Processors) + " -> " + list(p.Exporters)})
		}
		t.table(1, nil, rows)
		t.heading("components")
		rows = nil
		for _, c := range g.Components {
			var notes []string
			if len(c.Pipelines) > 1 {
				notes = append(notes, "shared by "+strings.Join(c.Pipelines, ", "))
			}
			if c.Note != "" {
				notes = append(notes, c.Note)
			}
			rows = append(rows, []string{c.Ref, string(c.Mode), strings.Join(notes, "; ")})
		}
		t.table(1, []string{"component", "mode", "note"}, rows)
		if len(g.Unused) > 0 {
			t.heading("unused (not built by the Collector)")
			for _, u := range g.Unused {
				t.line(1, "%s", u)
			}
		}
	}
	return t.err()
}

func list(ids []string) string {
	if len(ids) == 0 {
		return "(none)"
	}
	return strings.Join(ids, ", ")
}
