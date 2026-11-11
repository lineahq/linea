package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.uber.org/zap"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// linea-server exports its own metrics (the run queue, leases, workers,
// builds, storage and audit log; platform/server_metrics.go) over OTLP, set
// up by the OpenTelemetry SDK's standard environment variables. They
// describe the server, not any Collector it debugs. Export is opt-in: it
// starts when OTEL_METRICS_EXPORTER is otlp, or, with that variable unset,
// when an OTLP endpoint is set. The exporter itself reads the rest of the
// OTEL_EXPORTER_OTLP_* variables (headers, TLS, timeout, compression), and
// the SDK reads OTEL_METRIC_EXPORT_INTERVAL and OTEL_RESOURCE_ATTRIBUTES.

// otlpProtocol is how the metrics are sent: "grpc" or "http/protobuf".
type otlpProtocol string

// metricsExport decides from the environment whether the server exports its
// metrics and over which OTLP protocol. An empty protocol exports nothing.
func metricsExport(lookup envLookup) (otlpProtocol, error) {
	get := func(name string) string {
		v, _ := lookup(name)
		return strings.TrimSpace(v)
	}
	if strings.EqualFold(get("OTEL_SDK_DISABLED"), "true") {
		return "", nil
	}
	switch exporter := strings.ToLower(get("OTEL_METRICS_EXPORTER")); exporter {
	case "none":
		return "", nil
	case "otlp":
	case "":
		if get("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") == "" && get("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
			return "", nil
		}
	default:
		return "", fmt.Errorf("OTEL_METRICS_EXPORTER=%s: linea-server exports its metrics with otlp, or none", exporter)
	}
	protocol := get("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL")
	if protocol == "" {
		protocol = get("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	switch protocol {
	case "", "http/protobuf":
		return "http/protobuf", nil
	case "grpc":
		return "grpc", nil
	}
	return "", fmt.Errorf("OTLP protocol %q: linea-server sends its metrics with grpc or http/protobuf", protocol)
}

// serverMeterProvider is the SDK meter provider that exports the server's
// metrics, or nil when the environment asks for no export. The caller shuts
// it down, which sends the last collection.
func serverMeterProvider(ctx context.Context, lookup envLookup, logger *zap.Logger) (*sdkmetric.MeterProvider, otlpProtocol, error) {
	protocol, err := metricsExport(lookup)
	if err != nil || protocol == "" {
		return nil, "", err
	}
	var exporter sdkmetric.Exporter
	if protocol == "grpc" {
		exporter, err = otlpmetricgrpc.New(ctx)
	} else {
		exporter, err = otlpmetrichttp.New(ctx)
	}
	if err != nil {
		return nil, "", fmt.Errorf("OTLP metrics exporter: %w", err)
	}
	// OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES override the defaults.
	// The instance ID tells this process's series from other instances'.
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
		_ = exporter.Shutdown(ctx)
		return nil, "", fmt.Errorf("OTEL_RESOURCE_ATTRIBUTES: %w", err)
	}
	// An export that fails is logged and retried at the next interval; the
	// message names no header or credential.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		if logger != nil {
			logger.Warn("server metrics not exported", zap.Error(err))
		}
	}))
	return sdkmetric.NewMeterProvider(sdkmetric.WithResource(res), sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter))), protocol, nil
}

// meterProvider is p as the metric API's provider: a nil *MeterProvider is
// a nil interface, so the platform records nothing.
func meterProvider(p *sdkmetric.MeterProvider) metric.MeterProvider {
	if p == nil {
		return nil
	}
	return p
}
