// Capture smoke exercises the actual distribution. These OTLP servers are test
// fixtures only; no control service, debug backend, or viewer is shipped.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	_ "google.golang.org/grpc/encoding/gzip"
)

type record struct {
	signal   string
	encoded  []byte
	resource pcommon.Map
	count    int
}
type traces struct {
	ptraceotlp.UnimplementedGRPCServer
	records chan record
}

func (s *traces) Export(_ context.Context, r ptraceotlp.ExportRequest) (ptraceotlp.ExportResponse, error) {
	for _, resource := range r.Traces().ResourceSpans().All() {
		data := ptrace.NewTraces()
		resource.CopyTo(data.ResourceSpans().AppendEmpty())
		encoded, _ := (&ptrace.ProtoMarshaler{}).MarshalTraces(data)
		s.records <- record{"traces", encoded, resource.Resource().Attributes(), data.SpanCount()}
	}
	return ptraceotlp.NewExportResponse(), nil
}

type logs struct {
	plogotlp.UnimplementedGRPCServer
	records chan record
}

func (s *logs) Export(_ context.Context, r plogotlp.ExportRequest) (plogotlp.ExportResponse, error) {
	for _, resource := range r.Logs().ResourceLogs().All() {
		data := plog.NewLogs()
		resource.CopyTo(data.ResourceLogs().AppendEmpty())
		encoded, _ := (&plog.ProtoMarshaler{}).MarshalLogs(data)
		s.records <- record{"logs", encoded, resource.Resource().Attributes(), data.LogRecordCount()}
	}
	return plogotlp.NewExportResponse(), nil
}

type metrics struct {
	pmetricotlp.UnimplementedGRPCServer
	records chan record
}

func (s *metrics) Export(_ context.Context, r pmetricotlp.ExportRequest) (pmetricotlp.ExportResponse, error) {
	for _, resource := range r.Metrics().ResourceMetrics().All() {
		data := pmetric.NewMetrics()
		resource.CopyTo(data.ResourceMetrics().AppendEmpty())
		encoded, _ := (&pmetric.ProtoMarshaler{}).MarshalMetrics(data)
		s.records <- record{"metrics", encoded, resource.Resource().Attributes(), data.DataPointCount()}
	}
	return pmetricotlp.NewExportResponse(), nil
}
func fixture(port string) (*grpc.Server, chan record, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		return nil, nil, err
	}
	server := grpc.NewServer()
	records := make(chan record, 64)
	ptraceotlp.RegisterGRPCServer(server, &traces{records: records})
	plogotlp.RegisterGRPCServer(server, &logs{records: records})
	pmetricotlp.RegisterGRPCServer(server, &metrics{records: records})
	go server.Serve(listener)
	return server, records, nil
}

type output struct {
	sync.Mutex
	bytes.Buffer
}

func (o *output) Write(p []byte) (int, error) { o.Lock(); defer o.Unlock(); return o.Buffer.Write(p) }
func (o *output) text() string                { o.Lock(); defer o.Unlock(); return o.Buffer.String() }
func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: capture-smoke COLLECTOR CONFIG")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(binary, config string) (result error) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	backend, originals, err := fixture("14318")
	if err != nil {
		return err
	}
	defer backend.Stop()
	ingress, captured, err := fixture("14317")
	if err != nil {
		return err
	}
	defer ingress.Stop()
	var out output
	cmd := exec.CommandContext(ctx, binary, "--config", config)
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case err := <-done:
			result = errors.Join(result, err)
		case <-time.After(4 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			result = errors.Join(result, errors.New("the Collector's shutdown exceeded 4s"))
		}
		if result != nil {
			fmt.Fprint(os.Stderr, out.text())
		}
	}()
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	for {
		response, err := client.Get("http://127.0.0.1:17777/v1/status")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == 200 {
				break
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("local control startup: %w", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	conn, err := grpc.NewClient("127.0.0.1:4317", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	// Send the exact same input with capture disabled, active, and stopped.
	activate := func(id string) error {
		points := []map[string]string{}
		for _, signal := range []string{"traces", "logs", "metrics"} {
			for _, point := range []string{"ingress", "egress"} {
				points = append(points, map[string]string{"pipeline": signal, "point": point})
			}
		}
		request := map[string]any{"session_id": id, "principal": "synthetic-smoke", "target": map[string]string{"collector_group": "test", "pod": "smoke-pod"}, "capture_points": points, "signals": []string{"traces", "logs", "metrics"}, "expires_at": time.Now().Add(30 * time.Second).UTC()}
		encoded, _ := json.Marshal(request)
		response, err := client.Post("http://127.0.0.1:17777/v1/sessions", "application/json", bytes.NewReader(encoded))
		if err != nil {
			return err
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != 200 {
			return fmt.Errorf("start capture: %s", body)
		}
		return nil
	}
	var baseline map[string][]byte
	for _, phase := range []string{"inactive", "active", "stopped"} {
		if phase == "active" {
			if err := activate("docker-capture"); err != nil {
				return err
			}
		}
		if phase == "stopped" {
			req, _ := http.NewRequest(http.MethodDelete, "http://127.0.0.1:17777/v1/sessions/docker-capture", nil)
			response, err := client.Do(req)
			if err != nil {
				return err
			}
			response.Body.Close()
			if response.StatusCode != 204 {
				return fmt.Errorf("stop: status %d", response.StatusCode)
			}
		}
		if err := send(ctx, conn); err != nil {
			return fmt.Errorf("%s export: %w", phase, err)
		}
		seen := map[string][]byte{}
		for range 3 {
			select {
			case record := <-originals:
				if record.count != 1 {
					return fmt.Errorf("original record count %d", record.count)
				}
				seen[record.signal] = record.encoded
			case <-ctx.Done():
				return errors.New("original delivery timed out")
			}
		}
		if phase == "inactive" {
			baseline = seen
		} else {
			for signal, original := range baseline {
				if !bytes.Equal(original, seen[signal]) {
					return fmt.Errorf("%s original %s changed", phase, signal)
				}
			}
		}
		if phase == "active" {
			seenPoints := map[string]bool{}
			for range 6 {
				select {
				case record := <-captured:
					point, ok := record.resource.Get("capture.point")
					if !ok {
						return errors.New("capture point metadata missing")
					}
					id, ok := record.resource.Get("capture.session.id")
					if !ok || id.Str() != "docker-capture" {
						return errors.New("session metadata missing")
					}
					if _, ok := record.resource.Get("http.request.header.authorization"); ok {
						return errors.New("capture redaction failed")
					}
					if record.count != 1 {
						return errors.New("unexpected captured record count")
					}
					seenPoints[record.signal+"/"+point.Str()] = true
				case <-ctx.Done():
					return errors.New("active capture delivery timed out")
				}
			}
			if len(seenPoints) != 6 {
				return fmt.Errorf("only %d signal/point combinations captured", len(seenPoints))
			}
		} else {
			select {
			case <-captured:
				return fmt.Errorf("capture continued while %s", phase)
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	// A deliberately tiny byte queue overflows promptly with the ingress down.
	ingress.Stop()
	if err := activate("docker-outage"); err != nil {
		return err
	}
	outageCtx, outageCancel := context.WithTimeout(ctx, 5*time.Second)
	defer outageCancel()
	for range 20 {
		if err := send(outageCtx, conn); err != nil {
			return fmt.Errorf("ingress outage affected original export: %w", err)
		}
		for range 3 {
			select {
			case record := <-originals:
				if !bytes.Equal(record.encoded, baseline[record.signal]) {
					return errors.New("ingress outage changed original telemetry")
				}
			case <-outageCtx.Done():
				return errors.New("ingress outage blocked original delivery")
			}
		}
	}
	response, err := client.Get("http://127.0.0.1:17777/v1/status")
	if err != nil {
		return err
	}
	var status struct {
		Sessions []struct {
			SessionID string `json:"session_id"`
			Stats     struct {
				Dropped map[string]int64 `json:"dropped"`
			} `json:"stats"`
		} `json:"sessions"`
	}
	err = json.NewDecoder(response.Body).Decode(&status)
	response.Body.Close()
	if err != nil {
		return err
	}
	var dropped int64
	for _, session := range status.Sessions {
		if session.SessionID == "docker-outage" {
			dropped = session.Stats.Dropped["queue_full"]
		}
	}
	if dropped == 0 {
		return errors.New("outage did not exercise queue overflow")
	}
	fmt.Println("Capture smoke passed: all signals at ingress/egress, redacted tagged copies, stop, and byte-identical originals through capture ingress outage/queue overflow.")
	return nil
}
func attrs(resource pcommon.Resource) {
	resource.Attributes().PutStr("service.name", "capture-smoke")
	resource.Attributes().PutStr("http.request.header.authorization", "Bearer synthetic-secret")
}
func send(ctx context.Context, conn *grpc.ClientConn) error {
	t := ptrace.NewTraces()
	tr := t.ResourceSpans().AppendEmpty()
	attrs(tr.Resource())
	ts := tr.ScopeSpans().AppendEmpty()
	ts.Scope().SetName("smoke")
	span := ts.Spans().AppendEmpty()
	span.SetName("synthetic-span")
	span.SetTraceID(pcommon.TraceID{1})
	span.SetSpanID(pcommon.SpanID{2})
	if _, err := ptraceotlp.NewGRPCClient(conn).Export(ctx, ptraceotlp.NewExportRequestFromTraces(t)); err != nil {
		return err
	}
	l := plog.NewLogs()
	lr := l.ResourceLogs().AppendEmpty()
	attrs(lr.Resource())
	ls := lr.ScopeLogs().AppendEmpty()
	ls.Scope().SetName("smoke")
	ls.LogRecords().AppendEmpty().Body().SetStr("synthetic-log")
	if _, err := plogotlp.NewGRPCClient(conn).Export(ctx, plogotlp.NewExportRequestFromLogs(l)); err != nil {
		return err
	}
	m := pmetric.NewMetrics()
	mr := m.ResourceMetrics().AppendEmpty()
	attrs(mr.Resource())
	ms := mr.ScopeMetrics().AppendEmpty()
	ms.Scope().SetName("smoke")
	metric := ms.Metrics().AppendEmpty()
	metric.SetName("synthetic-metric")
	metric.SetEmptyGauge().DataPoints().AppendEmpty().SetIntValue(42)
	_, err := pmetricotlp.NewGRPCClient(conn).Export(ctx, pmetricotlp.NewExportRequestFromMetrics(m))
	return err
}
