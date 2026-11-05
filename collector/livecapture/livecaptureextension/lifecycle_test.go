package livecaptureextension

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"google.golang.org/grpc"
)

// hangingIngress accepts exports and never answers them.
type hangingIngress struct {
	ptraceotlp.UnimplementedGRPCServer
	release chan struct{}
}

func (s *hangingIngress) Export(ctx context.Context, _ ptraceotlp.ExportRequest) (ptraceotlp.ExportResponse, error) {
	select {
	case <-s.release:
	case <-ctx.Done():
	}
	return ptraceotlp.NewExportResponse(), nil
}

// A stopped session's queued copies must not stay alive next to a new
// session's queue: across stop/start cycles against a hanging ingress the
// pod-wide queue stays within max_queue_bytes.
func TestStopStartCyclesKeepQueueWithinCap(t *testing.T) {
	// Created first so its cleanup shuts down after the ingress is released.
	m, _ := testManager(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ingress := &hangingIngress{release: make(chan struct{})}
	server := grpc.NewServer()
	ptraceotlp.RegisterGRPCServer(server, ingress)
	go server.Serve(listener)
	t.Cleanup(func() { close(ingress.release); server.Stop() })
	m.host = testHost{}
	m.config.Delivery.ClientConfig.Endpoint = listener.Addr().String()
	m.config.Limits.MaxBytesPerSecond = 1 << 30
	m.config.Limits.MaxRecordsPerSecond = 1 << 30
	m.config.Limits.MaxTotalBytes = 1 << 40
	m.config.Limits.MaxTotalRecords = 1 << 40
	m.delivery = newOTLPDelivery(m)
	queues := m.settings.MeterProvider.(queueMeterProvider).queues
	limit := int64(m.config.Limits.MaxQueueBytes)
	data := ptrace.NewTraces()
	span := data.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	payload := strings.Repeat("x", 3000)
	for cycle := range 3 {
		r := traceRequest(fmt.Sprintf("cycle-%d", cycle))
		s := start(t, m, r)
		for i := 0; s.stats().Dropped["queue_full"] == 0 && i < 20000; i++ {
			span.Attributes().PutStr("payload", fmt.Sprintf("%d-%s", i, payload))
			m.CaptureTraces(context.Background(), r.CapturePoints[0], data)
		}
		if s.stats().Dropped["queue_full"] == 0 {
			t.Fatal("queue never filled against the hanging ingress")
		}
		if occupied := queues.sum(context.Background()); occupied > limit {
			t.Fatalf("cycle %d: %d queued bytes exceed the %d byte cap", cycle, occupied, limit)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if err := m.StopSession(ctx, r.SessionID); err != nil {
			t.Fatal(err)
		}
		cancel()
	}
}
