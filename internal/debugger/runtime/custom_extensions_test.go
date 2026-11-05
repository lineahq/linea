package runtime

import (
	"context"
	"errors"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/extension"
	"go.opentelemetry.io/collector/otelcol"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/processor/processorhelper"
	"go.uber.org/zap"

	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/telemetry"
	"github.com/lineahq/linea/internal/debugger/topology"
)

var (
	managerType = component.MustNewType("manager")
	userType    = component.MustNewType("usemanager")
)

// manager is a custom extension that components find by its concrete type,
// as the owner's capture processor finds *livecaptureextension.Manager.
type manager struct {
	started bool
	logger  *zap.Logger
}

func (m *manager) Start(context.Context, component.Host) error {
	m.started = true
	m.logger.Info("manager started", zap.String("mode", "test"))
	return nil
}

func (m *manager) Shutdown(context.Context) error { return nil }

// usesManager is a custom processor that refuses to start without a
// started manager extension.
func usesManager() processor.Factory {
	return processor.NewFactory(userType, func() component.Config { return &struct{}{} },
		processor.WithLogs(func(ctx context.Context, set processor.Settings, cfg component.Config, next consumer.Logs) (processor.Logs, error) {
			return processorhelper.NewLogs(ctx, set, cfg, next,
				func(_ context.Context, ld plog.Logs) (plog.Logs, error) {
					set.Logger.Info("consumed", zap.Int("records", ld.LogRecordCount()))
					return ld, nil
				},
				processorhelper.WithStart(func(_ context.Context, host component.Host) error {
					m, ok := host.GetExtensions()[component.NewID(managerType)].(*manager)
					if !ok {
						return errors.New("extension manager does not manage")
					}
					if !m.started {
						return errors.New("extension manager has not started")
					}
					return nil
				}))
		}, component.StabilityLevelDevelopment))
}

// withCustomComponents adds the manager extension and the processor that
// uses it to the registry, recorded with a custom module as a runtime
// built with custom modules records them.
func withCustomComponents(t *testing.T) {
	t.Helper()
	previous := factoriesOf
	t.Cleanup(func() { factoriesOf = previous })
	factoriesOf = func() (otelcol.Factories, error) {
		f, err := previous()
		if err != nil {
			return f, err
		}
		f.Extensions[managerType] = extension.NewFactory(managerType, func() component.Config { return &struct{}{} },
			func(_ context.Context, set extension.Settings, _ component.Config) (extension.Extension, error) {
				return &manager{logger: set.Logger}, nil
			},
			component.StabilityLevelDevelopment)
		f.ExtensionModules[managerType] = "example.com/custom v1.0.0"
		f.Processors[userType] = usesManager()
		f.ProcessorModules[userType] = "example.com/custom v1.0.0"
		return f, nil
	}
}

// customExtensionConfig starts the custom manager and an upstream
// health_check on a port the test holds, which only an inert health_check
// survives.
func customExtensionConfig(port string) string {
	return "extensions: {manager: {}, health_check: {endpoint: 127.0.0.1:" + port + "}}\n" +
		"receivers: {otlp: {protocols: {grpc: {}}}}\n" +
		"processors: {usemanager: {}}\n" +
		"exporters: {debug: {}}\n" +
		"service:\n  extensions: [health_check, manager]\n" +
		"  pipelines: {logs: {receivers: [otlp], processors: [usemanager], exporters: [debug]}}\n"
}

func logInput(body string) Input {
	ld := plog.NewLogs()
	ld.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty().Body().SetStr(body)
	return Input{Payload: telemetry.FromLogs(ld)}
}

// TestCustomExtensionsRunForReal: a custom extension is the real one, so a
// processor that looks it up by its type starts; its start and stop are
// recorded from the Collector's status reports; an upstream extension
// stays inert, and the topology says which runs.
func TestCustomExtensionsRunForReal(t *testing.T) {
	withCustomComponents(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	yaml := customExtensionConfig(port)

	r := runCompleted(t, Options{ConfigYAML: yaml, Inputs: []Input{logInput("hello")}})
	var lifecycle []string
	for _, e := range r.Events {
		if e.Source != instrument.SourceStatusWatcher {
			continue
		}
		if e.Component != "manager" || e.Mode != instrument.ModeReal || e.Error != "" {
			t.Errorf("status event %+v", e)
		}
		lifecycle = append(lifecycle, string(e.Kind))
	}
	if len(lifecycle) != 2 || lifecycle[0] != string(instrument.KindComponentStarted) || lifecycle[1] != string(instrument.KindComponentStopped) {
		t.Fatalf("custom extension lifecycle %v", lifecycle)
	}
	// The processor's start is recorded too, after the extension's.
	var processorStarted bool
	for _, e := range r.Events {
		if e.Kind == instrument.KindComponentStarted && e.Component == "usemanager" {
			processorStarted = e.Error == ""
		}
	}
	if !processorStarted {
		t.Fatal("usemanager did not start")
	}
	// Both custom components' own log lines are events, in order.
	var logs []string
	for _, e := range r.Events {
		if e.Kind == instrument.KindComponentLog {
			logs = append(logs, e.ComponentKind+"/"+e.Component+": "+e.Log.Message+" "+e.Log.Fields["mode"]+e.Log.Fields["records"])
		}
	}
	if want := []string{"extension/manager: manager started test", "processor/usemanager: consumed 1"}; !slices.Equal(logs, want) {
		t.Errorf("component logs %q, want %q", logs, want)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ref, err := RunReference(ctx, Options{ConfigYAML: yaml, Inputs: []Input{logInput("hello")}})
	if err != nil {
		t.Fatal(err)
	}
	if ref.Status != runresult.StatusCompleted {
		t.Fatalf("reference run %s: %s", ref.Status, ref.StatusReason)
	}

	factories, err := factoriesOf()
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := config.ResolveSource(context.Background(), config.Source{YAML: yaml}, factories, nil)
	if err != nil {
		t.Fatal(err)
	}
	g := topology.Build(resolved.Config, ExecutionModes(resolved.Config, factories))
	for _, c := range g.Components {
		switch c.Ref {
		case "extension/manager":
			if c.Mode != topology.ModeReal || c.Note != customExtensionNote {
				t.Errorf("%s: mode %s, note %q", c.Ref, c.Mode, c.Note)
			}
		case "extension/health_check":
			if c.Mode != topology.ModeSimulated || c.Note != extensionNote {
				t.Errorf("%s: mode %s, note %q", c.Ref, c.Mode, c.Note)
			}
		}
	}
}

// TestUpstreamOnlyRunsAddNoWatcher: without a custom extension the
// configuration runs as written.
func TestUpstreamOnlyRunsAddNoWatcher(t *testing.T) {
	withCustomComponents(t)
	factories, err := factoriesOf()
	if err != nil {
		t.Fatal(err)
	}
	yaml := "extensions: {health_check: {}, manager: {}}\nreceivers: {otlp: {protocols: {grpc: {}}}}\nexporters: {debug: {}}\n" +
		"service: {extensions: [health_check], pipelines: {logs: {receivers: [otlp], exporters: [debug]}}}\n"
	resolved, err := config.ResolveSource(context.Background(), config.Source{YAML: yaml}, factories, nil)
	if err != nil {
		t.Fatal(err)
	}
	uris, err := statusWatcherURIs(resolved.Config, factories.ExtensionModules)
	if err != nil || uris != nil {
		t.Fatalf("watcher overrides %v, %v", uris, err)
	}
}

// TestProcessorMetricsAreInTheResult: a filter processor's own metric,
// and the processor helper's item counts, are the run's component metrics.
func TestProcessorMetricsAreInTheResult(t *testing.T) {
	yaml := "receivers: {otlp: {protocols: {grpc: {}}}}\n" +
		"processors: {filter/drop: {logs: {log_record: ['body == \"drop\"']}}}\n" +
		"exporters: {debug: {}}\n" +
		"service: {pipelines: {logs: {receivers: [otlp], processors: [filter/drop], exporters: [debug]}}}\n"
	r := runCompleted(t, Options{ConfigYAML: yaml, Inputs: []Input{logInput("drop"), logInput("keep")}})
	values := map[string]int64{}
	for _, m := range r.ComponentMetrics {
		if m.Component != "filter/drop" || m.ComponentKind != instrument.ComponentProcessor || m.Pipeline != "logs" || m.IntValue == nil {
			continue
		}
		values[m.Name] += *m.IntValue
	}
	if values["otelcol_processor_filter_logs.filtered"] != 1 {
		t.Errorf("filtered = %d, want 1 (metrics %v)", values["otelcol_processor_filter_logs.filtered"], values)
	}
	if values["otelcol_processor_incoming_items"] != 2 || values["otelcol_processor_outgoing_items"] != 1 {
		t.Errorf("item counts %v", values)
	}
}
