package runtime

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configgrpc"
	"go.opentelemetry.io/collector/config/confighttp"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/exporter"
	"go.opentelemetry.io/collector/exporter/otlpexporter"
	"go.opentelemetry.io/collector/extension"
	"go.opentelemetry.io/collector/otelcol"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/processor/processorhelper"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/mock"
	"github.com/lineahq/linea/internal/debugger/topology"
)

var (
	forwarderType = component.MustNewType("forwarder")
	tapType       = component.MustNewType("tap")
)

// forwarderConfig declares its clients as the owner's live capture
// extension does: an OTLP exporter configuration to deliver through, a
// gRPC control client and an HTTP client.
type forwarderConfig struct {
	Delivery otlpexporter.Config     `mapstructure:"delivery"`
	Control  configgrpc.ClientConfig `mapstructure:"control"`
	Upload   confighttp.ClientConfig `mapstructure:"upload"`
}

// forwarder is a custom extension that delivers what the tap processor
// hands it through its own OTLP exporter and uploads it over HTTP.
type forwarder struct {
	set      extension.Settings
	cfg      *forwarderConfig
	delivery exporter.Logs
	control  *grpc.ClientConn
	upload   *http.Client
}

func (f *forwarder) Start(ctx context.Context, host component.Host) error {
	var err error
	set := exporter.Settings{ID: component.MustNewIDWithName("otlp", "delivery"), TelemetrySettings: f.set.TelemetrySettings, BuildInfo: f.set.BuildInfo}
	if f.delivery, err = otlpexporter.NewFactory().CreateLogs(ctx, set, &f.cfg.Delivery); err != nil {
		return err
	}
	if err := f.delivery.Start(ctx, host); err != nil {
		return err
	}
	if f.control, err = f.cfg.Control.ToClientConn(ctx, host.GetExtensions(), f.set.TelemetrySettings); err != nil {
		return err
	}
	// The control service is the extension's own protocol.
	if err := f.control.Invoke(ctx, "/forwarder.v1.Control/Register", &emptypb.Empty{}, &emptypb.Empty{}); err != nil {
		f.set.Logger.Warn("control registration failed: " + err.Error())
	}
	f.upload, err = f.cfg.Upload.ToClient(ctx, host.GetExtensions(), f.set.TelemetrySettings)
	return err
}

func (f *forwarder) Shutdown(ctx context.Context) error {
	var err error
	if f.delivery != nil {
		err = f.delivery.Shutdown(ctx)
	}
	if f.control != nil {
		err = errors.Join(err, f.control.Close())
	}
	return err
}

func (f *forwarder) forward(ctx context.Context, ld plog.Logs) error {
	if err := f.delivery.ConsumeLogs(ctx, ld); err != nil {
		return err
	}
	body, err := plogotlp.NewExportRequestFromLogs(ld).MarshalProto()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.cfg.Upload.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	resp, err := f.upload.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// tap is a custom processor that hands every batch to the forwarder.
func tap() processor.Factory {
	return processor.NewFactory(tapType, func() component.Config { return &struct{}{} },
		processor.WithLogs(func(ctx context.Context, set processor.Settings, cfg component.Config, next consumer.Logs) (processor.Logs, error) {
			var fwd *forwarder
			return processorhelper.NewLogs(ctx, set, cfg, next,
				func(ctx context.Context, ld plog.Logs) (plog.Logs, error) {
					return ld, fwd.forward(ctx, ld)
				},
				processorhelper.WithStart(func(_ context.Context, host component.Host) error {
					var ok bool
					if fwd, ok = host.GetExtensions()[component.NewID(forwarderType)].(*forwarder); !ok {
						return errors.New("no forwarder extension")
					}
					return nil
				}))
		}, component.StabilityLevelDevelopment))
}

// withForwarder registers the forwarder and the tap as components of a
// custom module.
func withForwarder(t *testing.T) {
	t.Helper()
	previous := factoriesOf
	t.Cleanup(func() { factoriesOf = previous })
	factoriesOf = func() (otelcol.Factories, error) {
		f, err := previous()
		if err != nil {
			return f, err
		}
		f.Extensions[forwarderType] = extension.NewFactory(forwarderType,
			func() component.Config {
				return &forwarderConfig{Delivery: *otlpexporter.NewFactory().CreateDefaultConfig().(*otlpexporter.Config)}
			},
			func(_ context.Context, set extension.Settings, cfg component.Config) (extension.Extension, error) {
				return &forwarder{set: set, cfg: cfg.(*forwarderConfig)}, nil
			}, component.StabilityLevelDevelopment)
		f.ExtensionModules[forwarderType] = "example.com/forwarder v1.0.0"
		f.Processors[tapType] = tap()
		f.ProcessorModules[tapType] = "example.com/forwarder v1.0.0"
		return f, nil
	}
}

const forwarderYAML = `extensions:
  forwarder:
    delivery:
      endpoint: collector.example:4317
      tls: {ca_file: /etc/forwarder/ca.pem}
      sending_queue: {enabled: false}
      retry_on_failure: {enabled: false}
    control: {endpoint: control.example:443}
    upload: {endpoint: https://upload.example/ingest/v1/logs}
receivers: {otlp: {protocols: {grpc: {}}}}
processors: {tap: {}}
exporters: {debug: {}}
service:
  extensions: [forwarder]
  pipelines: {logs: {receivers: [otlp], processors: [tap], exporters: [debug]}}
`

// TestSimulatedEndpointsReceiveWhatCustomComponentsSend: a custom
// extension's client endpoints are loopback sinks; what it delivers
// through its own OTLP exporter and uploads over HTTP is an endpoint
// output with an event, its control call is refused and reported, and the
// topology says the endpoints are simulated.
func TestSimulatedEndpointsReceiveWhatCustomComponentsSend(t *testing.T) {
	withForwarder(t)
	r := runCompleted(t, Options{ConfigYAML: forwarderYAML, Inputs: []Input{logInput("captured")}})

	outs := map[string]runresult.EndpointOutput{}
	for _, o := range r.EndpointOutputs {
		outs[o.Component+":"+o.Endpoint+"/"+o.Signal] = o
	}
	for _, k := range []string{"extension/forwarder:delivery/logs", "extension/forwarder:upload/logs"} {
		o, ok := outs[k]
		if !ok || o.Batches != 1 || o.Counts.Items != 1 || !strings.Contains(string(o.Data), "captured") {
			t.Errorf("endpoint output %s = %+v (all: %v)", k, o, r.EndpointOutputs)
		}
	}
	var received []string
	for _, e := range r.Events {
		if e.Kind == instrument.KindEndpointReceived {
			if e.Source != instrument.SourceEndpointSink || e.ComponentKind != "extension" || e.Snapshot == "" {
				t.Errorf("endpoint event %+v", e)
			}
			received = append(received, e.Component+":"+e.Endpoint)
		}
	}
	if len(received) != 2 {
		t.Errorf("endpoint.received events %v, want delivery and upload", received)
	}

	sim := map[string]mock.SimulatedEndpoint{}
	for _, s := range r.SimulatedEndpoints {
		sim[s.Path] = s
	}
	if s := sim["delivery"]; s.Component != "extension/forwarder" || s.Protocol != mock.ProtocolGRPC || s.Endpoint != "collector.example:4317" || s.Refused != nil {
		t.Errorf("delivery %+v", s)
	}
	if s := sim["control"]; len(s.Refused) != 1 || s.Refused[0].Call != "/forwarder.v1.Control/Register" {
		t.Errorf("control %+v", s)
	}
	if s := sim["upload"]; s.Protocol != mock.ProtocolHTTP || s.Endpoint != "https://upload.example/ingest/v1/logs" {
		t.Errorf("upload %+v", s)
	}
	var declared bool
	for _, d := range r.Mocks {
		if d.Provider == mock.EndpointsProvider && d.Ref == "extension/forwarder" && len(d.Supplied) == 3 && len(d.Warnings) == 1 {
			declared = true
		}
	}
	if !declared {
		t.Errorf("mocks %+v lack the forwarder's endpoints", r.Mocks)
	}

	factories, err := factoriesOf()
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := config.ResolveSource(context.Background(), config.Source{YAML: forwarderYAML}, factories, nil)
	if err != nil {
		t.Fatal(err)
	}
	g := topology.Build(resolved.Config, ExecutionModes(resolved.Config, factories))
	for _, c := range g.Components {
		if c.Ref == "extension/forwarder" && (c.Mode != topology.ModeDependencyMocked ||
			!strings.Contains(c.Note, endpointNote+"delivery, control, upload")) {
			t.Errorf("%s: mode %s, note %q", c.Ref, c.Mode, c.Note)
		}
	}

	// The reference run simulates the same endpoints and completes.
	ref, err := RunReference(context.Background(), Options{ConfigYAML: forwarderYAML, Inputs: []Input{logInput("captured")}})
	if err != nil {
		t.Fatal(err)
	}
	if ref.Status != runresult.StatusCompleted || ref.EndpointOutputs != nil {
		t.Fatalf("reference run %s: %s, endpoint outputs %v", ref.Status, ref.StatusReason, ref.EndpointOutputs)
	}
}

// TestComponentsWithoutClientsAreNotRedirected: an upstream processor
// keeps its mode, and a custom component without endpoints too.
func TestComponentsWithoutClientsAreNotRedirected(t *testing.T) {
	cfg := &forwarderConfig{}
	if got := mock.Clients(cfg); got != nil {
		t.Fatalf("clients %v of a configuration without endpoints", got)
	}
	if mode, note := withEndpointNote(topology.ModeReal, "n", nil); mode != topology.ModeReal || note != "n" {
		t.Fatalf("withEndpointNote without clients = %s, %q", mode, note)
	}
}
