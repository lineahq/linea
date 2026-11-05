package mock

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/confighttp"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"go.opentelemetry.io/collector/pipeline"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// maxSinkBody bounds an HTTP request body a sink reads.
const maxSinkBody = 64 << 20

// startGRPC serves the OTLP trace, metric and log services; any other
// method is answered Unimplemented and recorded.
func (s *sink) startGRPC() error {
	ln, err := net.Listen("tcp", loopback)
	if err != nil {
		return err
	}
	srv := grpc.NewServer(grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		method, _ := grpc.MethodFromServerStream(stream)
		s.refuse(method)
		return status.Errorf(codes.Unimplemented, "linea simulates only OTLP export at this endpoint, not %s", method)
	}))
	ptraceotlp.RegisterGRPCServer(srv, &grpcTraces{s: s})
	pmetricotlp.RegisterGRPCServer(srv, &grpcMetrics{s: s})
	plogotlp.RegisterGRPCServer(srv, &grpcLogs{s: s})
	go func() { _ = srv.Serve(ln) }()
	s.addr = ln.Addr().String()
	s.stop = func(context.Context) error {
		srv.Stop()
		return nil
	}
	return nil
}

type grpcTraces struct {
	ptraceotlp.UnimplementedGRPCServer
	s *sink
}

func (g grpcTraces) Export(_ context.Context, req ptraceotlp.ExportRequest) (ptraceotlp.ExportResponse, error) {
	g.s.received(telemetry.FromTraces(req.Traces()))
	return ptraceotlp.NewExportResponse(), nil
}

type grpcMetrics struct {
	pmetricotlp.UnimplementedGRPCServer
	s *sink
}

func (g grpcMetrics) Export(_ context.Context, req pmetricotlp.ExportRequest) (pmetricotlp.ExportResponse, error) {
	g.s.received(telemetry.FromMetrics(req.Metrics()))
	return pmetricotlp.NewExportResponse(), nil
}

type grpcLogs struct {
	plogotlp.UnimplementedGRPCServer
	s *sink
}

func (g grpcLogs) Export(_ context.Context, req plogotlp.ExportRequest) (plogotlp.ExportResponse, error) {
	g.s.received(telemetry.FromLogs(req.Logs()))
	return plogotlp.NewExportResponse(), nil
}

// startHTTP serves OTLP/HTTP export (protobuf or JSON, any compression
// confighttp accepts) at every path ending in /v1/traces, /v1/metrics or
// /v1/logs; anything else is answered 404 and recorded.
func (s *sink) startHTTP() error {
	sc := confighttp.NewDefaultServerConfig()
	sc.NetAddr.Endpoint = loopback
	ctx := context.Background()
	ln, err := sc.ToListener(ctx)
	if err != nil {
		return err
	}
	set := component.TelemetrySettings{Logger: zap.NewNop(), MeterProvider: metricnoop.NewMeterProvider(), TracerProvider: tracenoop.NewTracerProvider()}
	srv, err := sc.ToServer(ctx, nil, set, http.HandlerFunc(s.serveHTTP))
	if err != nil {
		_ = ln.Close()
		return err
	}
	go func() { _ = srv.Serve(ln) }()
	s.addr = ln.Addr().String()
	s.stop = func(ctx context.Context) error {
		if err := srv.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
	return nil
}

func (s *sink) serveHTTP(w http.ResponseWriter, r *http.Request) {
	signal, ok := otlpSignal(r.URL.Path)
	if !ok || r.Method != http.MethodPost {
		s.refuse(r.Method + " " + r.URL.Path)
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxSinkBody))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	asJSON := strings.HasPrefix(r.Header.Get("Content-Type"), "application/json")
	p, resp, err := decodeExport(signal, body, asJSON)
	if err != nil {
		s.refuse(r.Method + " " + r.URL.Path + ": " + err.Error())
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.received(p)
	if asJSON {
		w.Header().Set("Content-Type", "application/json")
	} else {
		w.Header().Set("Content-Type", "application/x-protobuf")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp)
}

func otlpSignal(path string) (pipeline.Signal, bool) {
	for _, sig := range signals {
		if strings.HasSuffix(path, "/v1/"+sig.String()) {
			return sig, true
		}
	}
	return pipeline.Signal{}, false
}

// decodeExport decodes an OTLP/HTTP export request and encodes the empty
// response in the same encoding.
func decodeExport(signal pipeline.Signal, body []byte, asJSON bool) (telemetry.Payload, []byte, error) {
	type codec interface {
		UnmarshalProto([]byte) error
		UnmarshalJSON([]byte) error
	}
	decode := func(req codec) error {
		if asJSON {
			return req.UnmarshalJSON(body)
		}
		return req.UnmarshalProto(body)
	}
	encode := func(proto func() ([]byte, error), json func() ([]byte, error)) ([]byte, error) {
		if asJSON {
			return json()
		}
		return proto()
	}
	switch signal {
	case pipeline.SignalTraces:
		req := ptraceotlp.NewExportRequest()
		if err := decode(&req); err != nil {
			return telemetry.Payload{}, nil, err
		}
		resp := ptraceotlp.NewExportResponse()
		b, err := encode(resp.MarshalProto, resp.MarshalJSON)
		return telemetry.FromTraces(req.Traces()), b, err
	case pipeline.SignalMetrics:
		req := pmetricotlp.NewExportRequest()
		if err := decode(&req); err != nil {
			return telemetry.Payload{}, nil, err
		}
		resp := pmetricotlp.NewExportResponse()
		b, err := encode(resp.MarshalProto, resp.MarshalJSON)
		return telemetry.FromMetrics(req.Metrics()), b, err
	default:
		req := plogotlp.NewExportRequest()
		if err := decode(&req); err != nil {
			return telemetry.Payload{}, nil, err
		}
		resp := plogotlp.NewExportResponse()
		b, err := encode(resp.MarshalProto, resp.MarshalJSON)
		return telemetry.FromLogs(req.Logs()), b, err
	}
}
