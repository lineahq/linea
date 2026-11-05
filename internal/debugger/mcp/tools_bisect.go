package mcp

import (
	"context"
	"fmt"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/platform"
)

type bisectConfigInput struct {
	Agent     string   `json:"agent" jsonschema:"Fleet agent ID whose configuration is rebuilt."`
	Config    string   `json:"config,omitempty" jsonschema:"Fleet config ID to search; empty when the agent receives exactly one config layer."`
	Good      int      `json:"good,omitempty" jsonschema:"Version known to pass; 0 is the oldest version fleet keeps."`
	Bad       int      `json:"bad,omitempty" jsonschema:"Version known to fail; 0 is the version fleet sends the agent now."`
	Scenarios []source `json:"scenarios" jsonschema:"Scenario sources with assertions that reproduce the problem; every probed version runs each one, and a failed assertion makes the version bad."`
	Local     []source `json:"local,omitempty" jsonschema:"The supervisor's local config files, merged under fleet's layers in the supervisor's order; fleet never sees them."`
	Runtime   string   `json:"runtime,omitempty" jsonschema:"Runtime to run the probes on, from linea_list_runtimes; empty is the debugger's own Collector."`
	Ignore    []string `json:"ignore,omitempty" jsonschema:"OTTL paths whose differences do not count when the culprit's runs are compared."`
}

// bisectResult is a bisection with its Markdown rendering.
type bisectResult struct {
	Bisection *platform.Bisection `json:"bisection"`
	Markdown  string              `json:"markdown"`
}

// summary states server-computed facts only: the verdict, versions and
// counts, never config names, messages or bodies.
func (r bisectResult) summary() string {
	b := r.Bisection
	text := fmt.Sprintf("Config bisect: verdict %s; versions %d to %d; %s", b.Verdict, b.Good, b.Bad, plural(b.Probes, "probe"))
	switch {
	case b.Verdict == platform.BisectFound:
		text += fmt.Sprintf("; first bad version %d, last good %d", b.FirstBad, b.LastGood)
	case b.FirstBad > 0 && b.LastGood > 0:
		text += fmt.Sprintf("; first bad version between %d and %d", b.LastGood+1, b.FirstBad)
	}
	if len(b.Reasons) > 0 {
		text += "; " + plural(len(b.Reasons), "inconclusive reason")
	}
	return text + "."
}

func addBisectTools(server *sdk.Server, service *platform.Service) {
	addTool(server, "linea_bisect_config", "Find the fleet config version that made scenarios fail on an agent. Rebuilds the agent's configuration from the config layers fleet sends it, as the OpAMP supervisor merges them, for each probed version of one config (the other layers stay at the versions fleet sends now), runs every scenario on it and halves the range between a good and a bad version. Versions whose configuration is invalid or whose runs are inconclusive are skipped. Verdict found, no_regression, already_bad or inconclusive; the culprit diffs the last good and first bad bodies and compares the changed scenarios' runs. Needs the server's fleet connection. The call waits for every run; ending it cancels the run it was waiting for.",
		func(ctx context.Context, in bisectConfigInput) (summarized, error) {
			req := platform.BisectRequest{AgentID: in.Agent, ConfigID: in.Config, Good: in.Good, Bad: in.Bad, Runtime: in.Runtime, Ignore: in.Ignore}
			for _, s := range in.Scenarios {
				src, err := s.platform()
				if err != nil {
					return nil, err
				}
				req.Scenarios = append(req.Scenarios, platform.ScenarioInput{Scenario: src})
			}
			for _, l := range in.Local {
				src, err := l.platform()
				if err != nil {
					return nil, err
				}
				req.Local = append(req.Local, src)
			}
			b, err := service.BisectFleetConfig(ctx, callerOf(ctx), req)
			if err != nil {
				return nil, err
			}
			return bisectResult{Bisection: b, Markdown: b.Markdown()}, nil
		})
}
