package runtime

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/otelcol"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"go.opentelemetry.io/collector/pipeline"
	"go.opentelemetry.io/collector/receiver/otlpreceiver"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	_ "google.golang.org/grpc/encoding/gzip" // the OTLP exporter compresses with gzip by default

	"github.com/lineahq/linea/internal/debugger/mock"
	"github.com/lineahq/linea/internal/debugger/mock/builtin"
	"github.com/lineahq/linea/internal/debugger/telemetry"
	"github.com/lineahq/linea/internal/debugger/topology"
)

var (
	otlpReceiverType     = component.MustNewType("otlp")
	otlpExporterType     = component.MustNewType("otlp_grpc")
	otlpHTTPExporterType = component.MustNewType("otlp_http")
)

// RunReference executes the same configuration with no instrumentation and
// no replay substitution: the real OTLP receiver receives the fixture over
// loopback gRPC and the real OTLP exporters deliver to local OTLP sinks.
// Components that depend on the environment run against the same mocks as
// in the instrumented run (the scenario's files, Kubernetes objects and
// simulated clock), and an input's declared client address is reported to
// the pipeline as its connection's address.
//
// The user's file is still unchanged; network endpoints are redirected by
// layering yaml: provider overrides after it. Only the internal telemetry is
// isolated (no Prometheus listener). Supported only for configurations whose
// receivers are OTLP or mocked and whose non-local exporters are OTLP (gRPC
// or HTTP).
func RunReference(ctx context.Context, opts Options) (*runresult.Result, error) {
	logger := opts.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	base, err := factoriesOf()
	if err != nil {
		return nil, err
	}
	resolved, err := resolveOptions(ctx, opts, base)
	if err != nil {
		return nil, err
	}
	base = withStandIns(base, resolved)
	if err := CheckReferencePrerequisites(resolved.Config); err != nil {
		return nil, err
	}
	targets, err := resolveTargets(resolved.Config, opts.Inputs, len(opts.Mocks.Files) > 0)
	if err != nil {
		return nil, err
	}
	settle, settleReason := settleFor(resolved.Config, opts.Settle)
	if err := checkWaits(ctx, targets, settle, settleReason); err != nil {
		return nil, err
	}

	// The same mocks as the instrumented run: the clock starts at the same
	// time, and receivers reading a mock deliver where inputs are injected.
	rep := newReplay()
	host := newMockHost(nil, rep)
	spec := opts.Mocks
	if spec.Clock.IsZero() {
		spec.Clock = latestRecordTime(targets)
	}
	plugins, err := Plugins()
	if err != nil {
		return nil, err
	}
	if err := mock.CheckSettings(spec, plugins); err != nil {
		return nil, err
	}
	providers := mock.Build(resolved.Config, spec, host, builtin.Providers, plugins)
	mocked := mock.Modes(providers)
	var peers *peerAddrs
	for _, t := range targets {
		if t.client != nil {
			peers = newPeerAddrs()
		}
	}

	used := usedComponents(resolved.Config)
	uris := slices.Clone(resolved.URIs)
	grpcAddrs := map[component.ID]string{}

	for _, id := range used.receivers {
		if _, ok := mocked[topology.Ref(topology.KindReceiver, id)]; ok {
			continue
		}
		if id.Type() != otlpReceiverType {
			return nil, fmt.Errorf("reference run cannot drive receiver %s", id)
		}
		cfg := resolved.Config.Receivers[id].(*otlpreceiver.Config)
		grpcAddr, err := freeAddr()
		if err != nil {
			return nil, err
		}
		uris = append(uris, override("receivers", id, "protocols::grpc::endpoint", grpcAddr))
		if cfg.Protocols.HTTP.HasValue() {
			httpAddr, err := freeAddr()
			if err != nil {
				return nil, err
			}
			uris = append(uris, override("receivers", id, "protocols::http::endpoint", httpAddr))
		}
		grpcAddrs[id] = grpcAddr
		// The reference run sends over gRPC, which it adds when only HTTP
		// is configured; gRPC then keeps headers as the HTTP protocol
		// would, as the instrumented run decides (includesMetadata).
		if !cfg.Protocols.GRPC.HasValue() && cfg.Protocols.HTTP.HasValue() && cfg.Protocols.HTTP.Get().ServerConfig.IncludeMetadata {
			uris = append(uris, override("receivers", id, "protocols::grpc::include_metadata", "true"))
		}
	}

	sinks := map[string]*sink{}
	defer func() {
		for _, s := range sinks {
			s.stop()
		}
	}()
	for _, id := range used.exporters {
		if slices.Contains(localExporters, id.Type()) {
			continue
		}
		switch base.Exporters[id.Type()].Type() {
		case otlpExporterType:
			s, err := startSink()
			if err != nil {
				return nil, err
			}
			sinks[id.String()] = s
			uris = append(uris,
				override("exporters", id, "endpoint", s.addr),
				override("exporters", id, "tls::insecure", "true"),
			)
		case otlpHTTPExporterType:
			s, err := startHTTPSink()
			if err != nil {
				return nil, err
			}
			sinks[id.String()] = s
			// Per-signal endpoints take precedence over endpoint, so all
			// are redirected; compression only changes the transport.
			base := "http://" + s.addr
			uris = append(uris,
				override("exporters", id, "endpoint", base),
				override("exporters", id, "traces_endpoint", base+"/v1/traces"),
				override("exporters", id, "metrics_endpoint", base+"/v1/metrics"),
				override("exporters", id, "logs_endpoint", base+"/v1/logs"),
				override("exporters", id, "compression", "none"),
			)
		default:
			return nil, fmt.Errorf("reference run cannot redirect exporter %s", id)
		}
	}

	// The reference run simulates the same endpoints and keeps nothing
	// they receive: only exporter outputs are compared.
	endpoints := mock.NewEndpoints(nil)
	defer endpoints.Close()
	local := newLocalSink()
	factories, err := referenceFactories(base, providers, endpoints, peers, local)
	if err != nil {
		return nil, err
	}
	factories.Telemetry = isolatedTelemetry(logger)
	session := mock.NewSession(resolved.Config, spec, providers, endpoints, logger)
	defer session.Close()
	if err := session.Prepare(ctx); err != nil {
		return nil, err
	}
	inputs := newInputResults(targets)
	started, runErr := runCollector(ctx, factories, resolved, uris, func(ctx context.Context) error {
		if err := session.Ready(ctx); err != nil {
			return err
		}
		for i, t := range targets {
			if err := sleep(ctx, t.after); err != nil {
				return err
			}
			if err := session.Input(ctx, t.providerInput(i)); err != nil {
				return err
			}
			inputs[i].Injected = true
			addr, ok := grpcAddrs[t.receiver]
			if !ok {
				// A mocked receiver: inject where it delivers, as the
				// instrumented run does.
				if err := rep.inject(ctx, t.receiver, t.payload.Clone()); err != nil {
					inputs[i].Error = err.Error()
				}
				continue
			}
			var dialed func(net.Addr)
			if t.client != nil {
				client := t.client
				dialed = func(local net.Addr) { peers.set(local, client) }
			}
			if err := sendOTLP(t.outgoing(ctx), addr, t.payload.Clone(), dialed); err != nil {
				inputs[i].Error = err.Error()
			}
		}
		if err := session.InputsDone(ctx); err != nil {
			return err
		}
		return sleep(ctx, settle)
	})
	session.Close()
	status, reason := classify(ctx, runErr)
	if !started && status == runresult.StatusFailed {
		return nil, runErr
	}

	// The reference run keeps no events or snapshots; its outputs share the
	// request's budget, charged in exporter order once the run has ended.
	droppedEvents, droppedBytes := opts.Budget.Dropped()
	exports := map[string][]captured{}
	for _, name := range slices.Sorted(maps.Keys(sinks)) {
		for _, p := range sinks[name].payloads() {
			exports[name] = append(exports[name], keep(opts.Budget, p))
		}
	}
	for _, name := range local.names() {
		for _, p := range local.payloads(name) {
			exports[name] = append(exports[name], keep(opts.Budget, p))
		}
	}
	outputs, incomplete, err := buildOutputs(exports)
	if err != nil {
		return nil, err
	}
	events, bytes := opts.Budget.Dropped()
	cut := truncation(events-droppedEvents, bytes-droppedBytes, incomplete)
	if cut != nil && status == runresult.StatusCompleted {
		status, reason = runresult.StatusPartial, budgetReason
	}
	inputs = append(inputs, host.inputs()...)
	declared := session.Declarations()
	return &runresult.Result{
		Mode:         runresult.ModeReference,
		Status:       status,
		StatusReason: reason,
		Inputs:       inputs,
		Outputs:      outputs,
		Mocks:        declared,
		Settle:       settleResult(settle, settleReason),
		Truncation:   cut,
	}, nil
}

type pipelineComponents struct {
	receivers []component.ID
	exporters []component.ID
}

// usedComponents lists the receivers and exporters the pipelines use.
// Connectors appear in both lists but run in process, so they are left out.
func usedComponents(cfg *otelcol.Config) pipelineComponents {
	var used pipelineComponents
	for _, p := range cfg.Service.Pipelines {
		for _, id := range p.Receivers {
			if _, ok := cfg.Connectors[id]; !ok && !slices.Contains(used.receivers, id) {
				used.receivers = append(used.receivers, id)
			}
		}
		for _, id := range p.Exporters {
			if _, ok := cfg.Connectors[id]; !ok && !slices.Contains(used.exporters, id) {
				used.exporters = append(used.exporters, id)
			}
		}
	}
	return used
}

func override(section string, id component.ID, key, value string) string {
	return fmt.Sprintf("yaml:%s::%s::%s: %s", section, id, key, value)
}

func freeAddr() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer l.Close()
	return l.Addr().String(), nil
}

// sendOTLP exports p to the OTLP gRPC receiver at addr over a connection of
// its own. dialed, when set, receives the connection's local address before
// any data is sent.
func sendOTLP(ctx context.Context, addr string, p telemetry.Payload, dialed func(net.Addr)) error {
	dial := func(ctx context.Context, target string) (net.Conn, error) {
		c, err := new(net.Dialer).DialContext(ctx, "tcp", target)
		if err == nil && dialed != nil {
			dialed(c.LocalAddr())
		}
		return c, err
	}
	conn, err := grpc.NewClient("passthrough:///"+addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(dial))
	if err != nil {
		return err
	}
	defer conn.Close()
	switch p.Signal {
	case pipeline.SignalTraces:
		_, err = ptraceotlp.NewGRPCClient(conn).Export(ctx, ptraceotlp.NewExportRequestFromTraces(p.Traces))
	case pipeline.SignalMetrics:
		_, err = pmetricotlp.NewGRPCClient(conn).Export(ctx, pmetricotlp.NewExportRequestFromMetrics(p.Metrics))
	case pipeline.SignalLogs:
		_, err = plogotlp.NewGRPCClient(conn).Export(ctx, plogotlp.NewExportRequestFromLogs(p.Logs))
	default:
		err = fmt.Errorf("unsupported signal %s", p.Signal)
	}
	if err != nil {
		return fmt.Errorf("send fixture to %s: %w", addr, err)
	}
	return nil
}

// sink is a loopback OTLP server (gRPC or HTTP) standing in for an
// exporter's downstream endpoint.
type sink struct {
	addr string
	stop func()
	mu   sync.Mutex
	got  []telemetry.Payload
}

func startSink() (*sink, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	server := grpc.NewServer()
	s := &sink{addr: l.Addr().String(), stop: server.Stop}
	ptraceotlp.RegisterGRPCServer(server, &traceSink{s: s})
	pmetricotlp.RegisterGRPCServer(server, &metricSink{s: s})
	plogotlp.RegisterGRPCServer(server, &logSink{s: s})
	go server.Serve(l) //nolint:errcheck // returns on stop
	return s, nil
}

// startHTTPSink serves the OTLP/HTTP paths with protobuf or JSON bodies.
func startHTTPSink() (*sink, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &sink{addr: l.Addr().String()}
	mux := http.NewServeMux()
	handle := func(path string, decode func(body []byte, json bool) (telemetry.Payload, []byte, error)) {
		mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
			var body io.Reader = r.Body
			if r.Header.Get("Content-Encoding") == "gzip" {
				zr, err := gzip.NewReader(r.Body)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				defer zr.Close()
				body = zr
			}
			raw, err := io.ReadAll(body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			isJSON := strings.HasPrefix(r.Header.Get("Content-Type"), "application/json")
			p, resp, err := decode(raw, isJSON)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			s.add(p)
			if isJSON {
				w.Header().Set("Content-Type", "application/json")
			} else {
				w.Header().Set("Content-Type", "application/x-protobuf")
			}
			w.Write(resp) //nolint:errcheck // the exporter retries on failure
		})
	}
	handle("/v1/traces", func(b []byte, isJSON bool) (telemetry.Payload, []byte, error) {
		req, resp := ptraceotlp.NewExportRequest(), ptraceotlp.NewExportResponse()
		if isJSON {
			out, _ := resp.MarshalJSON()
			return telemetry.FromTraces(req.Traces()), out, req.UnmarshalJSON(b)
		}
		out, _ := resp.MarshalProto()
		return telemetry.FromTraces(req.Traces()), out, req.UnmarshalProto(b)
	})
	handle("/v1/metrics", func(b []byte, isJSON bool) (telemetry.Payload, []byte, error) {
		req, resp := pmetricotlp.NewExportRequest(), pmetricotlp.NewExportResponse()
		if isJSON {
			out, _ := resp.MarshalJSON()
			return telemetry.FromMetrics(req.Metrics()), out, req.UnmarshalJSON(b)
		}
		out, _ := resp.MarshalProto()
		return telemetry.FromMetrics(req.Metrics()), out, req.UnmarshalProto(b)
	})
	handle("/v1/logs", func(b []byte, isJSON bool) (telemetry.Payload, []byte, error) {
		req, resp := plogotlp.NewExportRequest(), plogotlp.NewExportResponse()
		if isJSON {
			out, _ := resp.MarshalJSON()
			return telemetry.FromLogs(req.Logs()), out, req.UnmarshalJSON(b)
		}
		out, _ := resp.MarshalProto()
		return telemetry.FromLogs(req.Logs()), out, req.UnmarshalProto(b)
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	s.stop = func() { server.Close() } //nolint:errcheck // nothing to do on close errors
	go server.Serve(l)                 //nolint:errcheck // returns on close
	return s, nil
}

func (s *sink) add(p telemetry.Payload) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Requests may be pooled by the gRPC layer; keep an owned copy.
	s.got = append(s.got, p.Clone())
}

func (s *sink) payloads() []telemetry.Payload {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.got)
}

type traceSink struct {
	ptraceotlp.UnimplementedGRPCServer
	s *sink
}

func (t *traceSink) Export(_ context.Context, req ptraceotlp.ExportRequest) (ptraceotlp.ExportResponse, error) {
	t.s.add(telemetry.FromTraces(req.Traces()))
	return ptraceotlp.NewExportResponse(), nil
}

type metricSink struct {
	pmetricotlp.UnimplementedGRPCServer
	s *sink
}

func (m *metricSink) Export(_ context.Context, req pmetricotlp.ExportRequest) (pmetricotlp.ExportResponse, error) {
	m.s.add(telemetry.FromMetrics(req.Metrics()))
	return pmetricotlp.NewExportResponse(), nil
}

type logSink struct {
	plogotlp.UnimplementedGRPCServer
	s *sink
}

func (l *logSink) Export(_ context.Context, req plogotlp.ExportRequest) (plogotlp.ExportResponse, error) {
	l.s.add(telemetry.FromLogs(req.Logs()))
	return plogotlp.NewExportResponse(), nil
}
