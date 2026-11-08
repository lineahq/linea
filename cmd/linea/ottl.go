package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// ottlCmd previews one transform processor's statements on an input with
// the upstream OTTL APIs. A preview is never verified
// against a Collector run; run or statement test does that.
func ottlCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	const synopsis = "preview --config <collector.yaml> --processor ID --input <otlp.json> [--scenario <scenario.yaml>] [--runtime NAME] [--format text|json] [--server URL]"
	if len(args) == 0 || args[0] != "preview" {
		if len(args) > 0 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
			fmt.Fprintf(stdout, "usage: linea ottl %s\n", synopsis)
			return exitOK
		}
		fmt.Fprintf(stderr, "usage: linea ottl %s\n", synopsis)
		return exitError
	}
	fs := newFlagSet("ottl preview", synopsis[len("preview "):], stderr)
	var config, processor, input, scenario, runtimeName string
	fs.StringVar(&config, "config", "", "Collector configuration YAML (read-only)")
	fs.StringVar(&processor, "processor", "", "transform processor ID, for example transform/redact")
	fs.StringVar(&input, "input", "", "OTLP JSON input to transform")
	fs.StringVar(&scenario, "scenario", "", "scenario whose environment resolves ${env:...} references")
	fs.StringVar(&runtimeName, "runtime", "", "built runtime to preview on, or a runtime image reference; empty is the built-in registry")
	runtimeCacheFlag(fs)
	format := formatFlag(fs, "text")
	imports := importEnvFlag(fs)
	server := serverFlag(fs)
	if code, ok := parse(fs, args[1:], format); !ok {
		return code
	}
	if config == "" || processor == "" || input == "" {
		fmt.Fprintln(stderr, "--config, --processor and --input are required")
		fs.Usage()
		return exitError
	}
	inputJSON, err := readArg("input", input)
	if err != nil {
		return fail(stderr, err)
	}
	var raw []byte
	if *server != "" {
		c, err := connect(*server, false)
		if err != nil {
			return fail(stderr, err)
		}
		req := api.OTTLRequest{Processor: processor, Input: json.RawMessage(inputJSON), Runtime: runtimeName}
		if req.ConfigYAML, err = readArg("config", config); err != nil {
			return fail(stderr, err)
		}
		if scenario != "" {
			if req.ScenarioYAML, err = scenarioSource(scenario); err != nil {
				return fail(stderr, err)
			}
		}
		if raw, err = c.PreviewOTTL(ctx, req); err != nil {
			return fail(stderr, err)
		}
	} else {
		req := platform.PreviewRequest{Config: platform.Source{Path: config}, Processor: processor, Input: []byte(inputJSON), Runtime: runtimeName}
		if scenario != "" {
			req.Scenario = &platform.ScenarioInput{Scenario: platform.Source{Path: scenario}}
		}
		env, err := localEnvironment(*imports)
		if err != nil {
			return fail(stderr, err)
		}
		service := platform.New(platform.Options{Environment: env, Paths: platform.PathPolicy{Unrestricted: true}, Runtimes: dockerRuntimes()})
		defer service.Close()
		ev, err := service.PreviewTransformation(ctx, cliCaller, req)
		if err != nil {
			return fail(stderr, err)
		}
		if raw, err = encoded(ev); err != nil {
			return fail(stderr, err)
		}
	}
	if *format == "json" {
		_, err = stdout.Write(raw)
	} else {
		err = writePreview(stdout, raw)
	}
	if err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

func writePreview(w io.Writer, raw []byte) error {
	var ev struct {
		Valid       bool                  `json:"valid"`
		Diagnostics []platform.Diagnostic `json:"diagnostics"`
		Output      json.RawMessage       `json:"output"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		return err
	}
	t := newTextOut(w)
	status := "valid"
	if !ev.Valid {
		status = "invalid"
	}
	t.line(0, "%s  OTTL preview: %s (never verified against a Collector run)", t.status(status, 0), plural(len(ev.Diagnostics), "diagnostic"))
	if len(ev.Output) > 0 {
		t.field(1, "output", "--format json prints the output and its diff")
	}
	writeDiagnostics(t, ev.Diagnostics)
	return t.err()
}
