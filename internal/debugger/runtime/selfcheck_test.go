package runtime

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/collector/otelcol"
	"go.opentelemetry.io/collector/pdata/plog"

	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/runresult"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// TestSelfCheckPassesOnTheBuiltInRuntime: on the release the
// instrumentation was verified against, every assumption holds.
func TestSelfCheckPassesOnTheBuiltInRuntime(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	checks := runSelfCheck(ctx, factoriesOf)
	if len(checks) != 7 {
		t.Fatalf("checks = %+v", checks)
	}
	for _, c := range checks {
		if !c.OK {
			t.Errorf("%s failed: %s", c.Code, c.Detail)
		}
	}
	if failed := runresult.FailedSelfChecks(SelfCheck()); len(failed) != 0 {
		t.Errorf("SelfCheck() failed %+v", failed)
	}
}

// probeRun runs the self-check configuration and returns its result.
func probeRun(t *testing.T) *runresult.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ld := plog.NewLogs()
	ld.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty().Body().SetStr("probe")
	res, err := run(ctx, Options{ConfigYAML: selfCheckConfig, Inputs: []Input{{Payload: telemetry.FromLogs(ld)}}}, func() (otelcol.Factories, error) {
		return withProbeFactories(factoriesOf())
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// TestSelfCheckFailsEachBrokenAssumption breaks one assumption at a time in
// the probe's evidence, as a Collector release that changed it would, and
// expects exactly that check to fail.
func TestSelfCheckFailsEachBrokenAssumption(t *testing.T) {
	base := probeRun(t)
	edit := func(match func(instrument.Event) bool, change func(*instrument.Event)) func(*runresult.Result) {
		return func(r *runresult.Result) {
			for i := range r.Events {
				if match(r.Events[i]) {
					change(&r.Events[i])
				}
			}
		}
	}
	is := func(kind instrument.Kind, component string) func(instrument.Event) bool {
		return func(e instrument.Event) bool { return e.Kind == kind && e.Component == component }
	}
	cases := []struct {
		name    string
		fault   func(*runresult.Result)
		objects bool
		fails   []string
	}{
		{"logger without the pipeline ID", edit(is(instrument.KindIngress, "linea_probe"), func(e *instrument.Event) { e.Pipeline = "" }), true,
			[]string{runresult.SelfCheckPipelineAttribution, runresult.SelfCheckObjectIdentity, runresult.SelfCheckFanoutClone}},
		{"router without pipeline IDs", edit(is(instrument.KindEgress, "linea_probe_connector"), func(e *instrument.Event) { e.Pipeline = "" }), true,
			[]string{runresult.SelfCheckConnectorRoutes}},
		{"wrapper dropping the context", edit(func(e instrument.Event) bool { return e.Kind == instrument.KindIngress }, func(e *instrument.Event) { e.FromSeq = 0 }), true,
			[]string{runresult.SelfCheckEmissionContext}},
		{"renamed pooling gate", func(*runresult.Result) {}, false,
			[]string{runresult.SelfCheckObjectIdentity, runresult.SelfCheckFanoutClone}},
		{"copy after a connector", edit(func(e instrument.Event) bool {
			return e.Kind == instrument.KindIngress && e.Component == "linea_probe" && e.Pipeline == "logs/b"
		}, func(e *instrument.Event) { e.Objects = []int{999} }), true,
			[]string{runresult.SelfCheckObjectIdentity}},
		{"no copy for a mutating consumer", func(r *runresult.Result) {
			var a []int
			for _, e := range r.Events {
				if e.Kind == instrument.KindIngress && e.Component == "linea_probe" && e.Pipeline == "logs/a" {
					a = e.Objects
				}
			}
			edit(is(instrument.KindIngress, "linea_probe_mutating"), func(e *instrument.Event) { e.Objects = a })(r)
		}, true, []string{runresult.SelfCheckFanoutClone}},
		{"extension status not reported", func(r *runresult.Result) {
			r.Events = slices.DeleteFunc(r.Events, is(instrument.KindComponentStarted, "linea_probe_watched"))
		}, true, []string{runresult.SelfCheckExtensionStatus}},
		{"lost flush", func(r *runresult.Result) {
			r.Events = slices.DeleteFunc(r.Events, func(e instrument.Event) bool { return e.Kind == instrument.KindExport })
		}, true, []string{runresult.SelfCheckLifecycle}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := *base
			r.Events = slices.Clone(base.Events)
			for i := range r.Events {
				r.Events[i].Objects = slices.Clone(r.Events[i].Objects)
			}
			tc.fault(&r)
			var failed []string
			for _, c := range judgeSelfCheck(&r, tc.objects, "the gate was not found") {
				if !c.OK {
					failed = append(failed, c.Code)
					if c.Detail == "" {
						t.Errorf("%s failed without a detail", c.Code)
					}
				}
			}
			if !slices.Equal(failed, tc.fails) {
				t.Errorf("failed = %v, want %v", failed, tc.fails)
			}
		})
	}
}

// TestSelfCheckReportsAProbeThatCannotRun: a probe that cannot build its
// Collector is one failed check, never a panic.
func TestSelfCheckReportsAProbeThatCannotRun(t *testing.T) {
	checks := runSelfCheck(context.Background(), func() (otelcol.Factories, error) { return otelcol.Factories{}, errors.New("no factories") })
	if len(checks) != 1 || checks[0].Code != runresult.SelfCheckProbe || checks[0].OK || !strings.Contains(checks[0].Detail, "no factories") {
		t.Fatalf("checks = %+v", checks)
	}
}
