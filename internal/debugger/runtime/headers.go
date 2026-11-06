package runtime

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"go.opentelemetry.io/collector/client"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/otelcol"
	"go.opentelemetry.io/collector/receiver/otlpreceiver"
	"google.golang.org/grpc/metadata"
)

// includesMetadata reports whether an OTLP receiver hands request headers
// to the pipeline (include_metadata, confighttp v0.162.0 and configgrpc
// v1.68.0: every header is copied into client.Info.Metadata). An input
// carries no protocol, so the gRPC protocol decides when it is configured,
// as the reference run sends over gRPC; otherwise the HTTP protocol does.
func includesMetadata(cfg *otelcol.Config, id component.ID) (bool, error) {
	c, ok := cfg.Receivers[id].(*otlpreceiver.Config)
	if !ok {
		return false, fmt.Errorf("headers apply to OTLP receivers only, and %s is not one", id)
	}
	if c.Protocols.GRPC.HasValue() {
		return c.Protocols.GRPC.Get().IncludeMetadata, nil
	}
	if c.Protocols.HTTP.HasValue() {
		return c.Protocols.HTTP.Get().ServerConfig.IncludeMetadata, nil
	}
	return false, nil
}

// withClient returns ctx carrying what the receiver would report about the
// request: the declared client address, and the headers when the receiver
// includes metadata.
func (t target) withClient(ctx context.Context) context.Context {
	if t.client == nil && !t.keepHeaders {
		return ctx
	}
	var info client.Info
	if t.client != nil {
		info.Addr = t.client
	}
	if t.keepHeaders {
		info.Metadata = client.NewMetadata(t.headers)
	}
	return client.NewContext(ctx, info)
}

// outgoing returns ctx carrying the input's headers as gRPC metadata, for
// the reference run's request to the real receiver.
func (t target) outgoing(ctx context.Context) context.Context {
	if len(t.headers) == 0 {
		return ctx
	}
	md := metadata.MD{}
	for name, v := range t.headers {
		md.Append(name, v...)
	}
	return metadata.NewOutgoingContext(ctx, md)
}

// headerNames lists the input's header names, never their values.
func (t target) headerNames() []string {
	return slices.Sorted(maps.Keys(t.headers))
}
