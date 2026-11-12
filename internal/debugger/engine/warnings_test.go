package engine

import (
	"slices"
	"testing"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/runresult"
	"github.com/lineahq/linea/internal/debugger/topology"
)

// TestRunWarningsSayWhereEvidenceMayBeWrong: a failed self-check of the
// runtime, a processor stage without a pipeline and connector deliveries
// without one each become a warning; attributed evidence does not.
func TestRunWarningsSayWhereEvidenceMayBeWrong(t *testing.T) {
	rt := &ReportRuntime{SelfCheck: []runresult.SelfCheck{
		{Code: runresult.SelfCheckLifecycle, OK: true},
		{Code: runresult.SelfCheckConnectorRoutes, Detail: "routes do not name their pipelines"},
	}}
	g := &topology.Graph{Components: []topology.Component{
		{Kind: topology.KindProcessor, ID: "batch"},
		{Kind: topology.KindConnector, ID: "routing"},
		{Kind: topology.KindConnector, ID: "forward"},
	}}
	stages := []analysis.Stage{
		{Component: "batch", Kind: "processor", Pipeline: ""},
		{Component: "batch", Kind: "processor", Pipeline: ""},
		{Component: "transform", Kind: "processor", Pipeline: "logs"},
		{Component: "routing", Kind: "connector", Pipeline: ""},
	}
	res := &runresult.Result{Events: []instrument.Event{
		{Kind: instrument.KindEgress, Component: "routing", Pipeline: ""},
		{Kind: instrument.KindEgress, Component: "routing", Pipeline: ""},
		{Kind: instrument.KindEgress, Component: "forward", Pipeline: "logs/b"},
		{Kind: instrument.KindEgress, Component: "transform", Pipeline: ""},
	}}
	got := runWarnings(rt, g, stages, res)
	var codes []string
	for _, w := range got {
		codes = append(codes, w.Code+":"+w.Component)
		if w.Detail == "" {
			t.Errorf("%s without a detail", w.Code)
		}
	}
	want := []string{WarningSelfCheckFailed + ":", WarningUnattributedStage + ":batch", WarningUnattributedRoute + ":routing"}
	if !slices.Equal(codes, want) {
		t.Fatalf("warnings = %v, want %v", codes, want)
	}
	if got[0].Detail != "connector_routes: routes do not name their pipelines" || got[2].Detail != "2 deliveries of this connector name no pipeline, so its routes are not shown" {
		t.Errorf("details = %q, %q", got[0].Detail, got[2].Detail)
	}
	if w := runWarnings(&ReportRuntime{}, g, stages[2:3], &runresult.Result{}); w != nil {
		t.Errorf("attributed evidence warned %+v", w)
	}
}
