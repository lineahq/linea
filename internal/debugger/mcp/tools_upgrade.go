package mcp

import (
	"context"
	"fmt"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/platform"
)

type compareVersionsInput struct {
	From      string   `json:"from,omitempty" jsonschema:"Runtime the configuration runs on today, from linea_list_runtimes; empty or built-in is the debugger's own Collector."`
	To        string   `json:"to" jsonschema:"Runtime of the release to move to, from linea_list_runtimes; built-in is the debugger's own Collector."`
	Config    source   `json:"config" jsonschema:"Collector configuration to check."`
	Scenarios []source `json:"scenarios,omitempty" jsonschema:"Scenario sources that exercise the configuration; each runs on both runtimes on one captured input. Without scenarios only the configuration and feature gates are compared."`
	Ignore    []string `json:"ignore,omitempty" jsonschema:"OTTL paths whose differences are counted but do not change behavior, such as log.attributes[\"request.id\"]."`
}

// upgradeResult is an upgrade check with its Markdown rendering.
type upgradeResult struct {
	Check    *platform.UpgradeCheck `json:"check"`
	Markdown string                 `json:"markdown"`
}

// summary states server-computed facts only: the verdict and counts, never
// runtime names, versions, diagnostics or gate IDs.
func (r upgradeResult) summary() string {
	c := r.Check
	text := fmt.Sprintf("Upgrade check: verdict %s; configuration valid from %t, to %t; %s; %s",
		c.Verdict, c.Configuration.FromValid, c.Configuration.ToValid,
		plural(len(c.Configuration.Added), "new diagnostic"), plural(len(c.FeatureGates.Changes), "feature gate change"))
	if len(c.Reasons) > 0 {
		text += "; " + plural(len(c.Reasons), "inconclusive reason")
	}
	counts := map[string]int{}
	for _, s := range c.Scenarios {
		if s.Error != "" {
			counts["not compared"]++
		} else {
			counts[string(s.Outcome)]++
		}
	}
	text += "; " + plural(len(c.Scenarios), "scenario")
	for _, k := range []string{string(platform.ComparisonSame), string(platform.ComparisonDifferent), string(platform.ComparisonInconclusive), "not compared"} {
		if counts[k] > 0 {
			text += fmt.Sprintf(", %d %s", counts[k], k)
		}
	}
	return text + "."
}

func addUpgradeTools(server *sdk.Server, service *platform.Service) {
	addTool(server, "linea_compare_versions", "Check a Collector upgrade: validate the configuration on the current runtime (from) and the target (to), compare their feature gates for its components, and, when both accept it, run each scenario on both runtimes on one captured input and compare the runs. Verdict compatible, configuration_breaks, behavior_changes or inconclusive; inconclusive is never a pass. The target runtime must already be registered. The call waits for every run; ending it abandons the check and cancels the runs it was waiting for.",
		func(ctx context.Context, in compareVersionsInput) (summarized, error) {
			if in.To == "" {
				return nil, invalidRequest("to must name the target runtime")
			}
			cfg, err := in.Config.platform()
			if err != nil {
				return nil, err
			}
			req := platform.UpgradeCheckRequest{From: in.From, To: in.To, Config: cfg, Ignore: in.Ignore}
			for _, s := range in.Scenarios {
				src, err := s.platform()
				if err != nil {
					return nil, err
				}
				req.Scenarios = append(req.Scenarios, platform.ScenarioInput{Scenario: src})
			}
			c, err := service.CheckUpgrade(ctx, callerOf(ctx), req)
			if err != nil {
				return nil, err
			}
			return upgradeResult{Check: c, Markdown: c.Markdown()}, nil
		})
}
