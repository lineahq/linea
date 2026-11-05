package mcp

import (
	"context"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/platform"
)

type inspectInput struct {
	Config    source  `json:"config" jsonschema:"Collector configuration source."`
	Scenario  *source `json:"scenario,omitempty" jsonschema:"Optional scenario source supplying environment values and mocks; mutually exclusive with setup_yaml."`
	SetupYAML string  `json:"setup_yaml,omitempty" jsonschema:"Optional quick-run setup YAML; mutually exclusive with scenario."`
	Runtime   string  `json:"runtime,omitempty" jsonschema:"Optional runtime name, image reference or ID from linea_list_runtimes; empty or built-in is the debugger's own Collector."`
}

func (in inspectInput) platform() (platform.InspectRequest, error) {
	config, err := in.Config.platform()
	if err != nil {
		return platform.InspectRequest{}, err
	}
	scenario, err := scenarioSource(in.Scenario)
	if err != nil {
		return platform.InspectRequest{}, err
	}
	return platform.InspectRequest{Config: config, Scenario: scenario, Setup: in.SetupYAML, Runtime: in.Runtime}, nil
}

type getPipelineInput struct {
	inspectInput
	Pipeline string `json:"pipeline" jsonschema:"Pipeline ID from linea_list_pipelines."`
}

type previewInput struct {
	Config    source  `json:"config" jsonschema:"Collector configuration source."`
	Processor string  `json:"processor" jsonschema:"Configured transform processor ID."`
	InputJSON string  `json:"input_json" jsonschema:"OTLP JSON payload as text."`
	Scenario  *source `json:"scenario,omitempty" jsonschema:"Optional scenario source supplying environment values."`
	Runtime   string  `json:"runtime,omitempty" jsonschema:"Optional runtime name, image reference or ID from linea_list_runtimes; empty or built-in is the debugger's own Collector."`
}

type validationResult struct {
	Valid       bool                  `json:"valid"`
	Diagnostics []platform.Diagnostic `json:"diagnostics"`
	standIns
}

func validation(ins *platform.Inspection) validationResult {
	return validationResult{Valid: ins.Valid, Diagnostics: ins.Diagnostics, standIns: standInsOf(ins.Topology, ins.References)}
}

type componentView struct {
	ID   string `json:"id"`
	Mode string `json:"mode"`
}

type pipelineView struct {
	ID         string          `json:"id"`
	Signal     string          `json:"signal"`
	Receivers  []componentView `json:"receivers"`
	Processors []componentView `json:"processors"`
	Exporters  []componentView `json:"exporters"`
}

type pipelinesResult struct {
	validationResult
	Pipelines []pipelineView `json:"pipelines"`
}

type pipelineResult struct {
	validationResult
	Graph     *platform.Graph `json:"graph,omitempty"`
	Effective map[string]any  `json:"effective,omitempty"`
}

type previewResult struct {
	validationResult
	Verified bool                 `json:"verified"`
	Replay   *platform.OTTLReport `json:"replay,omitempty"`
	Diff     *analysis.Diff       `json:"diff,omitempty"`
}

// A summarized result states its outcome in one line of text built only from
// server-computed facts (validity, counts). Untrusted content (IDs, config,
// diagnostics, telemetry) stays in the structured result.
type summarized interface {
	summary() string
}

func (r validationResult) verdict() string {
	if r.Valid {
		return "Configuration is valid"
	}
	return "Configuration is invalid"
}

func (r validationResult) summary() string {
	return r.verdict() + ": " + diagnosticCounts(r.Diagnostics) + r.facts() + "."
}

func (r pipelinesResult) summary() string {
	return fmt.Sprintf("%s: %s; %s%s.", r.verdict(), plural(len(r.Pipelines), "pipeline"), diagnosticCounts(r.Diagnostics), r.facts())
}

func (r pipelineResult) summary() string {
	if r.Graph == nil {
		return r.validationResult.summary()
	}
	return fmt.Sprintf("%s: pipeline found with %s and %s; %s%s.", r.verdict(),
		plural(len(r.Graph.Components), "component"), plural(len(r.Graph.Edges), "edge"), diagnosticCounts(r.Diagnostics), r.facts())
}

func (r previewResult) summary() string {
	text := "Unverified OTTL preview, not a test. " + r.verdict()
	if r.Replay != nil {
		statements := 0
		for _, g := range r.Replay.Groups {
			statements += len(g.Statements)
		}
		text += "; " + plural(statements, "statement") + " replayed"
	}
	if r.Diff != nil {
		text += fmt.Sprintf("; %s in, %s out", plural(r.Diff.Before.Items, "record"), plural(r.Diff.After.Items, "record"))
	}
	return text + "; " + diagnosticCounts(r.Diagnostics) + "."
}

// The SDK validates arguments against each input schema, inferred from the
// typed handler (see inputSchema). Results are explicit objects so the SDK
// never falls back to telemetry text.
func addTool[In any](server *sdk.Server, name, description string, handle func(context.Context, In) (summarized, error)) {
	addToolWith(server, name, description, nil, handle)
}

// addToolWith adjusts the inferred input schema before registering the tool.
func addToolWith[In any](server *sdk.Server, name, description string, adjust func(*jsonschema.Schema), handle func(context.Context, In) (summarized, error)) {
	schema, err := inputSchema[In]()
	if err != nil {
		panic(fmt.Sprintf("mcp: input schema for %s: %v", name, err))
	}
	if adjust != nil {
		adjust(schema)
	}
	sdk.AddTool(server, &sdk.Tool{Name: name, Description: description, InputSchema: schema},
		func(ctx context.Context, req *sdk.CallToolRequest, in In) (*sdk.CallToolResult, any, error) {
			out, err := handle(ctx, in)
			if err != nil {
				return toolError(err), nil, nil
			}
			return toolResult(out.summary(), out), nil, nil
		})
}

func addInspectionTools(server *sdk.Server, service *platform.Service) {
	inspect := func(ctx context.Context, in inspectInput) (*platform.Inspection, error) {
		req, err := in.platform()
		if err != nil {
			return nil, err
		}
		return service.InspectConfiguration(ctx, callerOf(ctx), req)
	}
	addTool(server, "linea_validate_config", "Validate a Collector configuration against the supported distribution.",
		func(ctx context.Context, in inspectInput) (summarized, error) {
			ins, err := inspect(ctx, in)
			if err != nil {
				return nil, err
			}
			return validation(ins), nil
		})
	addTool(server, "linea_list_pipelines", "List pipelines and ordered component IDs with their execution modes.",
		func(ctx context.Context, in inspectInput) (summarized, error) {
			ins, err := inspect(ctx, in)
			if err != nil {
				return nil, err
			}
			out := pipelinesResult{validationResult: validation(ins), Pipelines: []pipelineView{}}
			if ins.Topology == nil {
				return out, nil
			}
			components := func(refs []string) []componentView {
				out := []componentView{}
				for _, ref := range refs {
					for _, c := range ins.Topology.Components {
						if c.Ref == ref {
							out = append(out, componentView{ID: c.ID, Mode: string(c.Mode)})
							break
						}
					}
				}
				return out
			}
			for _, p := range ins.Topology.Pipelines {
				out.Pipelines = append(out.Pipelines, pipelineView{
					ID: p.ID, Signal: p.Signal, Receivers: components(p.Receivers),
					Processors: components(p.Processors), Exporters: components(p.Exporters),
				})
			}
			return out, nil
		})
	addTool(server, "linea_get_pipeline", "Inspect one pipeline's graph, component modes and redacted effective component configuration.",
		func(ctx context.Context, in getPipelineInput) (summarized, error) {
			if in.Pipeline == "" {
				return nil, invalidRequest("pipeline is required")
			}
			ins, err := inspect(ctx, in.inspectInput)
			if err != nil {
				return nil, err
			}
			out := pipelineResult{validationResult: validation(ins)}
			if !ins.Valid || ins.Topology == nil {
				return out, nil
			}
			graph := platform.Graph{}
			refs := map[string]bool{}
			for _, p := range ins.Topology.Pipelines {
				if p.ID == in.Pipeline {
					graph.Pipelines = append(graph.Pipelines, p)
					for _, group := range [][]string{p.Receivers, p.Processors, p.Exporters} {
						for _, ref := range group {
							refs[ref] = true
						}
					}
					break
				}
			}
			if len(graph.Pipelines) == 0 {
				return nil, &platform.Error{Code: platform.CodeNotFound, Message: "pipeline not found"}
			}
			out.Effective = map[string]any{}
			for _, c := range ins.Topology.Components {
				if !refs[c.Ref] {
					continue
				}
				graph.Components = append(graph.Components, c)
				section := string(c.Kind) + "s"
				if configs, ok := ins.Effective[section].(map[string]any); ok {
					if value, ok := configs[c.ID]; ok {
						selected, ok := out.Effective[section].(map[string]any)
						if !ok {
							selected = map[string]any{}
							out.Effective[section] = selected
						}
						selected[c.ID] = value
					}
				}
			}
			for _, e := range ins.Topology.Edges {
				if e.Pipeline == in.Pipeline {
					graph.Edges = append(graph.Edges, e)
				}
			}
			out.Graph = &graph
			return out, nil
		})
	addTool(server, "linea_preview_ottl", "Preview only. Not a test and never verified against a Collector run.",
		func(ctx context.Context, in previewInput) (summarized, error) {
			config, err := in.Config.platform()
			if err != nil {
				return nil, err
			}
			scenario, err := scenarioSource(in.Scenario)
			if err != nil {
				return nil, err
			}
			preview, err := service.PreviewTransformation(ctx, callerOf(ctx), platform.PreviewRequest{
				Config: config, Scenario: scenario, Processor: in.Processor, Input: []byte(in.InputJSON), Runtime: in.Runtime,
			})
			if err != nil {
				return nil, err
			}
			return previewResult{validationResult: validationResult{Valid: preview.Valid, Diagnostics: preview.Diagnostics},
				Verified: false, Replay: preview.Replay, Diff: preview.Diff}, nil
		})
}
