package engine

import (
	"fmt"
	"slices"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/runresult"
	"github.com/lineahq/linea/internal/debugger/topology"
)

// Run warning codes.
const (
	// WarningSelfCheckFailed: a self-check of the run's runtime failed; the
	// detail says which evidence is affected.
	WarningSelfCheckFailed = "self_check_failed"
	// WarningUnattributedStage: the Collector gave a processor no pipeline
	// ID, so its instances in several pipelines are reported as one stage.
	WarningUnattributedStage = "unattributed_stage"
	// WarningUnattributedRoute: a connector delivered to a consumer the
	// debugger could not name a pipeline for.
	WarningUnattributedRoute = "unattributed_route"
)

// RunWarning says where a run's evidence may be wrong. Component is the
// component's ID when one is concerned.
type RunWarning struct {
	Code      string `json:"code"`
	Component string `json:"component,omitempty"`
	Detail    string `json:"detail"`
}

// runWarnings derives the run's warnings from its runtime's self-check and
// its recorded evidence. They never change the run's outcome.
func runWarnings(rt *ReportRuntime, g *topology.Graph, stages []analysis.Stage, res *runresult.Result) []RunWarning {
	var out []RunWarning
	if rt != nil {
		for _, c := range runresult.FailedSelfChecks(rt.SelfCheck) {
			out = append(out, RunWarning{Code: WarningSelfCheckFailed, Detail: c.Code + ": " + c.Detail})
		}
	}
	var unattributed []string
	for _, s := range stages {
		if s.Kind == string(topology.KindProcessor) && s.Pipeline == "" && !slices.Contains(unattributed, s.Component) {
			unattributed = append(unattributed, s.Component)
			out = append(out, RunWarning{Code: WarningUnattributedStage, Component: s.Component,
				Detail: "the Collector gave this processor no pipeline ID, so its instances in every pipeline it is in are reported as one stage"})
		}
	}
	connectors := map[string]bool{}
	if g != nil {
		for _, c := range g.Components {
			if c.Kind == topology.KindConnector {
				connectors[c.ID] = true
			}
		}
	}
	routes := map[string]int{}
	var order []string
	if res != nil {
		for _, e := range res.Events {
			if e.Kind == instrument.KindEgress && e.Pipeline == "" && connectors[e.Component] {
				if routes[e.Component] == 0 {
					order = append(order, e.Component)
				}
				routes[e.Component]++
			}
		}
	}
	for _, c := range order {
		out = append(out, RunWarning{Code: WarningUnattributedRoute, Component: c,
			Detail: fmt.Sprintf("%d deliveries of this connector name no pipeline, so its routes are not shown", routes[c])})
	}
	return out
}
