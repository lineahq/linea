package runtime

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/extension"
	"go.opentelemetry.io/collector/otelcol"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/processor"

	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/runresult"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// The self-check probes the Collector internals the instrumentation relies
// on (docs/internals/factory-decoration.md, stage-analysis.md,
// record-objects.md, components.md, collector-embedding.md; verified against
// v0.162.0) by running one record through a small configuration on the run
// path itself. Its receiver, processor, connector and exporter types are in
// no distribution, so stand-ins run in their place in every runtime; the
// mutating processor and the watched extension are the probe's own.
const selfCheckConfig = `
receivers:
  linea_probe: {}
processors:
  linea_probe: {}
  linea_probe_mutating: {}
connectors:
  linea_probe_connector: {}
exporters:
  linea_probe: {}
extensions:
  linea_probe_watched: {}
service:
  extensions: [linea_probe_watched]
  pipelines:
    logs/a:
      receivers: [linea_probe]
      processors: [linea_probe]
      exporters: [linea_probe_connector]
    logs/b:
      receivers: [linea_probe_connector]
      processors: [linea_probe]
      exporters: [linea_probe]
    logs/c:
      receivers: [linea_probe]
      processors: [linea_probe_mutating]
      exporters: [linea_probe]
`

var (
	probeMutatingType = component.MustNewType("linea_probe_mutating")
	probeWatchedType  = component.MustNewType("linea_probe_watched")
)

// probeModule marks the probe's own components as custom, so the watched
// extension runs for real with the status watcher, as a custom extension
// does in a run.
const probeModule = "linea.invalid/selfcheck v0.0.0"

// selfCheckTimeout bounds the probe; it takes milliseconds.
const selfCheckTimeout = 30 * time.Second

var selfCheck = sync.OnceValue(func() []runresult.SelfCheck {
	ctx, cancel := context.WithTimeout(context.Background(), selfCheckTimeout)
	defer cancel()
	return runSelfCheck(ctx, factoriesOf)
})

// SelfCheck probes, once per process, the Collector internals the
// instrumentation relies on and returns one check per assumption. It never
// fails: a probe that cannot run is a failed check.
func SelfCheck() []runresult.SelfCheck { return slices.Clone(selfCheck()) }

func runSelfCheck(ctx context.Context, load func() (otelcol.Factories, error)) []runresult.SelfCheck {
	ld := plog.NewLogs()
	ld.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty().Body().SetStr("linea self-check")
	res, err := run(ctx, Options{ConfigYAML: selfCheckConfig, Inputs: []Input{{Payload: telemetry.FromLogs(ld)}}}, func() (otelcol.Factories, error) {
		return withProbeFactories(load())
	})
	if err != nil {
		return []runresult.SelfCheck{{Code: runresult.SelfCheckProbe, Detail: "the self-check pipeline did not run: " + err.Error()}}
	}
	supported, why := telemetry.ObjectsSupported()
	return judgeSelfCheck(res, supported, why)
}

func withProbeFactories(f otelcol.Factories, err error) (otelcol.Factories, error) {
	if err != nil {
		return f, err
	}
	f.Processors = maps.Clone(f.Processors)
	f.Processors[probeMutatingType] = mutatingProcessor()
	f.ProcessorModules = withModule(f.ProcessorModules, probeMutatingType)
	f.Extensions = maps.Clone(f.Extensions)
	f.Extensions[probeWatchedType] = extension.NewFactory(probeWatchedType, func() component.Config { return &struct{}{} },
		func(context.Context, extension.Settings, component.Config) (extension.Extension, error) {
			return struct {
				component.StartFunc
				component.ShutdownFunc
			}{}, nil
		}, component.StabilityLevelDevelopment)
	f.ExtensionModules = withModule(f.ExtensionModules, probeWatchedType)
	return f, nil
}

func withModule(m map[component.Type]string, t component.Type) map[component.Type]string {
	out := maps.Clone(m)
	if out == nil {
		out = map[component.Type]string{}
	}
	out[t] = probeModule
	return out
}

// mutatingProcessor passes logs on unchanged but declares that it mutates
// them, so the fan-out in front of it must hand it a copy.
func mutatingProcessor() processor.Factory {
	return processor.NewFactory(probeMutatingType, func() component.Config { return &struct{}{} },
		processor.WithLogs(func(_ context.Context, _ processor.Settings, _ component.Config, next consumer.Logs) (processor.Logs, error) {
			return mutating{next: next}, nil
		}, component.StabilityLevelDevelopment))
}

type mutating struct {
	component.StartFunc
	component.ShutdownFunc
	next consumer.Logs
}

func (mutating) Capabilities() consumer.Capabilities { return consumer.Capabilities{MutatesData: true} }

func (m mutating) ConsumeLogs(ctx context.Context, ld plog.Logs) error {
	return m.next.ConsumeLogs(ctx, ld)
}

// judgeSelfCheck turns the probe run's events into one check per
// assumption. objects says whether record objects can identify records.
func judgeSelfCheck(res *runresult.Result, objects bool, objectsWhy string) []runresult.SelfCheck {
	var injected, captured int
	var probeInputs, mutatingInputs, connectorOutputs []instrument.Event
	unlinked := 0
	extension := map[instrument.Kind]bool{}
	for _, e := range res.Events {
		switch e.Kind {
		case instrument.KindInput:
			injected++
		case instrument.KindIngress:
			switch e.Component {
			case "linea_probe":
				probeInputs = append(probeInputs, e)
			case probeMutatingType.String():
				mutatingInputs = append(mutatingInputs, e)
			}
			if e.FromSeq == 0 {
				unlinked++
			}
		case instrument.KindEgress:
			if e.Component == "linea_probe_connector" {
				connectorOutputs = append(connectorOutputs, e)
			}
			if e.InputSeq == 0 {
				unlinked++
			}
		case instrument.KindExport:
			captured++
			if e.FromSeq == 0 {
				unlinked++
			}
		case instrument.KindComponentStarted, instrument.KindComponentStopped:
			if e.Component == probeWatchedType.String() {
				extension[e.Kind] = true
			}
		}
	}
	check := func(code string, ok bool, detail string, a ...any) runresult.SelfCheck {
		if ok {
			return runresult.SelfCheck{Code: code, OK: true}
		}
		return runresult.SelfCheck{Code: code, Detail: fmt.Sprintf(detail, a...)}
	}
	pipelines := func(events []instrument.Event) []string {
		var out []string
		for _, e := range events {
			out = append(out, e.Pipeline)
		}
		slices.Sort(out)
		return out
	}

	var out []runresult.SelfCheck
	got := pipelines(probeInputs)
	out = append(out, check(runresult.SelfCheckPipelineAttribution, slices.Equal(got, []string{"logs/a", "logs/b"}),
		"a processor in pipelines logs/a and logs/b recorded its inputs as pipelines %q; the Collector did not give it the pipeline ID on its logger, so a processor shared by pipelines is reported as one stage", got))
	got = pipelines(connectorOutputs)
	out = append(out, check(runresult.SelfCheckConnectorRoutes, slices.Equal(got, []string{"logs/b"}),
		"a connector feeding pipeline logs/b recorded its deliveries as pipelines %q, so connector routes do not name their pipelines", got))
	out = append(out, check(runresult.SelfCheckEmissionContext, unlinked == 0 && len(probeInputs) > 0,
		"%d recorded inputs and outputs could not be linked to the call that caused them, so stages are reported as deferred and lineage falls back to matching content", unlinked))

	var probeA, probeB []int
	for _, e := range probeInputs {
		switch e.Pipeline {
		case "logs/a":
			probeA = e.Objects
		case "logs/b":
			probeB = e.Objects
		}
	}
	attributed := out[0].OK
	switch {
	case !attributed:
		out = append(out, check(runresult.SelfCheckObjectIdentity, false, "not checked: it needs pipeline attribution"))
	case !objects:
		out = append(out, check(runresult.SelfCheckObjectIdentity, false, "%s; records are matched by content, and claims that two stages saw the same record are not made", objectsWhy))
	default:
		out = append(out, check(runresult.SelfCheckObjectIdentity, len(probeA) == 1 && slices.Equal(probeA, probeB),
			"a record passed on unchanged had objects %v before a connector and %v after it, so record objects do not identify records", probeA, probeB))
	}
	var mutated []int
	if len(mutatingInputs) == 1 {
		mutated = mutatingInputs[0].Objects
	}
	switch {
	case !attributed:
		out = append(out, check(runresult.SelfCheckFanoutClone, false, "not checked: it needs pipeline attribution"))
	case !objects:
		out = append(out, check(runresult.SelfCheckFanoutClone, false, "not checked: it needs record objects (%s)", objectsWhy))
	default:
		out = append(out, check(runresult.SelfCheckFanoutClone, len(probeA) == 1 && len(mutated) == 1 && probeA[0] != mutated[0],
			"a fan-out to a consumer that mutates data and one that does not handed them objects %v and %v; a copy was expected for the mutating one", mutated, probeA))
	}
	out = append(out, check(runresult.SelfCheckExtensionStatus, extension[instrument.KindComponentStarted] && extension[instrument.KindComponentStopped],
		"a watched extension's start and stop were not recorded (started %v, stopped %v), so custom extensions' lifecycles are missing from runs",
		extension[instrument.KindComponentStarted], extension[instrument.KindComponentStopped]))
	// One record reaches the exporter through logs/b and one through logs/c.
	out = append(out, check(runresult.SelfCheckLifecycle, res.Completed() && injected == 1 && captured == 2,
		"the probe run ended %s with %d of 1 inputs injected and %d of 2 exports captured", res.Status, injected, captured))
	return out
}
