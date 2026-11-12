package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// linea-server exports its own metrics (the run queue, leases, workers,
// builds, storage and audit log; platform/server_metrics.go) and traces
// (request and run spans; tracing.go, platform/server_traces.go) over OTLP,
// set up by the OpenTelemetry SDK's standard environment variables. They
// describe the server, not any Collector it debugs. Export is opt-in per
// signal: it starts when OTEL_METRICS_EXPORTER or OTEL_TRACES_EXPORTER is
// otlp, or, with that variable unset, when an OTLP endpoint for the signal
// or for all signals is set. The exporters read the rest of the
// OTEL_EXPORTER_OTLP_* variables (headers, TLS, timeout, compression), and
// the SDK reads OTEL_METRIC_EXPORT_INTERVAL, OTEL_TRACES_SAMPLER, the
// OTEL_BSP_* batch settings and OTEL_RESOURCE_ATTRIBUTES.

// otlpProtocol is how a signal is sent: "grpc" or "http/protobuf".
type otlpProtocol string

// metricsExport and tracesExport decide from the environment whether the
// server exports the signal and over which OTLP protocol. An empty protocol
// exports nothing.
func metricsExport(lookup envLookup) (otlpProtocol, error) { return otlpExport(lookup, "metrics") }
func tracesExport(lookup envLookup) (otlpProtocol, error)  { return otlpExport(lookup, "traces") }

func otlpExport(lookup envLookup, signal string) (otlpProtocol, error) {
	get := func(name string) string {
		v, _ := lookup(name)
		return strings.TrimSpace(v)
	}
	upper := strings.ToUpper(signal)
	if strings.EqualFold(get("OTEL_SDK_DISABLED"), "true") {
		return "", nil
	}
	switch exporter := strings.ToLower(get("OTEL_" + upper + "_EXPORTER")); exporter {
	case "none":
		return "", nil
	case "otlp":
	case "":
		if get("OTEL_EXPORTER_OTLP_"+upper+"_ENDPOINT") == "" && get("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
			return "", nil
		}
	default:
		return "", fmt.Errorf("OTEL_%s_EXPORTER=%s: linea-server exports its %s with otlp, or none", upper, exporter, signal)
	}
	protocol := get("OTEL_EXPORTER_OTLP_" + upper + "_PROTOCOL")
	if protocol == "" {
		protocol = get("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	switch protocol {
	case "", "http/protobuf":
		return "http/protobuf", nil
	case "grpc":
		return "grpc", nil
	}
	return "", fmt.Errorf("OTLP protocol %q: linea-server sends its %s with grpc or http/protobuf", protocol, signal)
}

// serverTelemetry holds the SDK providers that export the server's metrics
// and traces; either is nil when the environment asks for no export of it.
type serverTelemetry struct {
	meters          *sdkmetric.MeterProvider
	tracers         *sdktrace.TracerProvider
	metricsProtocol otlpProtocol
	tracesProtocol  otlpProtocol
}

// newServerTelemetry sets up the exports the environment asks for. The
// caller shuts it down, which sends what is left.
func newServerTelemetry(ctx context.Context, lookup envLookup, logger *zap.Logger) (*serverTelemetry, error) {
	t := &serverTelemetry{}
	var err error
	if t.metricsProtocol, err = metricsExport(lookup); err != nil {
		return nil, err
	}
	if t.tracesProtocol, err = tracesExport(lookup); err != nil {
		return nil, err
	}
	if t.metricsProtocol == "" && t.tracesProtocol == "" {
		return t, nil
	}
	// OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES override the defaults.
	// The instance ID tells this process's series and spans from other
	// instances.
	instance := make([]byte, 16)
	_, _ = rand.Read(instance)
	res, err := resource.New(ctx,
		resource.WithAttributes(
			attribute.String("service.name", "linea-server"),
			attribute.String("service.version", platform.ProductVersion()),
			attribute.String("service.instance.id", hex.EncodeToString(instance)),
		),
		resource.WithFromEnv(),
	)
	if err != nil {
		return nil, fmt.Errorf("OTEL_RESOURCE_ATTRIBUTES: %w", err)
	}
	if t.metricsProtocol != "" {
		var exporter sdkmetric.Exporter
		if t.metricsProtocol == "grpc" {
			exporter, err = otlpmetricgrpc.New(ctx)
		} else {
			exporter, err = otlpmetrichttp.New(ctx)
		}
		if err != nil {
			return nil, fmt.Errorf("OTLP metrics exporter: %w", err)
		}
		t.meters = sdkmetric.NewMeterProvider(sdkmetric.WithResource(res), sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)))
	}
	if t.tracesProtocol != "" {
		var exporter *otlptrace.Exporter
		if t.tracesProtocol == "grpc" {
			exporter, err = otlptracegrpc.New(ctx)
		} else {
			exporter, err = otlptracehttp.New(ctx)
		}
		if err != nil {
			t.shutdown()
			return nil, fmt.Errorf("OTLP traces exporter: %w", err)
		}
		// The sampler comes from OTEL_TRACES_SAMPLER (default
		// parentbased_always_on).
		t.tracers = sdktrace.NewTracerProvider(sdktrace.WithResource(res), sdktrace.WithBatcher(exporter))
	}
	// An export that fails is logged and retried with the next collection or
	// batch; the message names no header or credential.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		if logger != nil {
			logger.Warn("server telemetry not exported", zap.Error(err))
		}
	}))
	return t, nil
}

// shutdown sends what is left and stops the exports, bounded.
func (t *serverTelemetry) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if t.meters != nil {
		_ = t.meters.Shutdown(ctx)
	}
	if t.tracers != nil {
		_ = t.tracers.Shutdown(ctx)
	}
}

// meterProvider and tracerProvider are the API providers to record with: a
// nil SDK provider is a nil interface, so nothing is recorded.
func (t *serverTelemetry) meterProvider() metric.MeterProvider {
	if t.meters == nil {
		return nil
	}
	return t.meters
}

func (t *serverTelemetry) tracerProvider() trace.TracerProvider {
	if t.tracers == nil {
		return nil
	}
	return t.tracers
}
