package main

// Linux-only experiments run a real Collector subprocess and independent OTLP
// sinks. They never use the interactive generator or serialize viewer JSON.
import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type perfSink struct {
	counts   [3]atomic.Int64
	delay    time.Duration
	outage   bool
	captured bool
	invalid  atomic.Int64
}

func (s *perfSink) receive(ctx context.Context, signal int, records int, resources []pcommon.Resource) error {
	if s.outage {
		return status.Error(codes.Unavailable, "injected capture sink outage")
	}
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for _, resource := range resources {
		_, tagged := resource.Attributes().Get("capture.session.id")
		if tagged != s.captured {
			s.invalid.Add(1)
		}
	}
	s.counts[signal].Add(int64(records))
	return nil
}
func (s *perfSink) total() int64 { return s.counts[0].Load() + s.counts[1].Load() + s.counts[2].Load() }

type perfTraceSink struct {
	ptraceotlp.UnimplementedGRPCServer
	sink *perfSink
}

func (s *perfTraceSink) Export(ctx context.Context, r ptraceotlp.ExportRequest) (ptraceotlp.ExportResponse, error) {
	resources := make([]pcommon.Resource, 0, r.Traces().ResourceSpans().Len())
	for _, item := range r.Traces().ResourceSpans().All() {
		resources = append(resources, item.Resource())
	}
	return ptraceotlp.NewExportResponse(), s.sink.receive(ctx, 0, r.Traces().SpanCount(), resources)
}

type perfLogSink struct {
	plogotlp.UnimplementedGRPCServer
	sink *perfSink
}

func (s *perfLogSink) Export(ctx context.Context, r plogotlp.ExportRequest) (plogotlp.ExportResponse, error) {
	resources := make([]pcommon.Resource, 0, r.Logs().ResourceLogs().Len())
	for _, item := range r.Logs().ResourceLogs().All() {
		resources = append(resources, item.Resource())
	}
	return plogotlp.NewExportResponse(), s.sink.receive(ctx, 1, r.Logs().LogRecordCount(), resources)
}

type perfMetricSink struct {
	pmetricotlp.UnimplementedGRPCServer
	sink *perfSink
}

func (s *perfMetricSink) Export(ctx context.Context, r pmetricotlp.ExportRequest) (pmetricotlp.ExportResponse, error) {
	resources := make([]pcommon.Resource, 0, r.Metrics().ResourceMetrics().Len())
	for _, item := range r.Metrics().ResourceMetrics().All() {
		resources = append(resources, item.Resource())
	}
	return pmetricotlp.NewExportResponse(), s.sink.receive(ctx, 2, r.Metrics().DataPointCount(), resources)
}
func servePerfSink(endpoint string, sink *perfSink) (*grpc.Server, error) {
	l, err := net.Listen("tcp", endpoint)
	if err != nil {
		return nil, err
	}
	s := grpc.NewServer(grpc.MaxRecvMsgSize(16 << 20))
	ptraceotlp.RegisterGRPCServer(s, &perfTraceSink{sink: sink})
	plogotlp.RegisterGRPCServer(s, &perfLogSink{sink: sink})
	pmetricotlp.RegisterGRPCServer(s, &perfMetricSink{sink: sink})
	go func() { _ = s.Serve(l) }()
	return s, nil
}

type perfCase struct {
	Name        string
	Rate        int    // total records/sec, evenly divided over the three signals
	Mode        string // absent, inactive, resource-none, span-none, selective, all
	Percent     int
	Raised      bool
	Payload     int
	Delay       time.Duration
	Outage      bool
	Budget      int
	BytesRaised bool
	RecordLimit int
	QueueMiB    int
}
type perfResult struct {
	Case               perfCase        `json:"case"`
	Seconds            float64         `json:"seconds"`
	InputRecords       int64           `json:"input_records"`
	InputErrors        int64           `json:"input_errors"`
	MainRecords        int64           `json:"main_records"`
	CapturedRecords    int64           `json:"captured_records"`
	InvalidTags        int64           `json:"invalid_tags"`
	InputRPS           float64         `json:"input_records_per_second"`
	RequestP50MS       float64         `json:"request_p50_ms"`
	RequestP95MS       float64         `json:"request_p95_ms"`
	CollectorCPUCores  float64         `json:"collector_cpu_cores"`
	CollectorPeakRSS   int64           `json:"collector_peak_rss_bytes"`
	CollectorPeakHeap  int64           `json:"collector_peak_heap_bytes"`
	QueuePeakBytes     int64           `json:"queue_peak_bytes"`
	CaptureBatchMeanUS float64         `json:"capture_batch_mean_us"`
	CaptureStats       json.RawMessage `json:"capture_session"`
	ExporterFailed     float64         `json:"exporter_failed_records"`
	StopMS             float64         `json:"stop_ms"`
	FirstCaptureDropMS float64         `json:"first_capture_drop_ms"`
	FirstInputErrorMS  float64         `json:"first_input_error_ms"`
	Queue95PercentMS   float64         `json:"queue_95_percent_ms"`
}

func perfConfiguration(c perfCase) string {
	limits := ""
	if c.Raised {
		limits = "    limits:\n      max_bytes_per_second: 1GiB\n      max_records_per_second: 10000000\n      max_total_bytes: 8GiB\n      max_total_records: 100000000\n"
	}
	if c.BytesRaised {
		limits = "    limits:\n      max_bytes_per_second: 1GiB\n"
	}
	if c.QueueMiB > 0 {
		limits += fmt.Sprintf("      max_queue_bytes: %dMiB\n", c.QueueMiB)
	}
	extensions := "extensions: {}\n"
	serviceExtensions := "[]"
	processors := "[memory_limiter, batch]"
	if c.Mode != "absent" {
		extensions = "extensions:\n  livecapture:\n    enabled: true\n    environment: test\n    collector_group: perf\n    pod: perf\n    local_control:\n      enabled: true\n      endpoint: 127.0.0.1:17777\n      allow_raw: true\n    delivery:\n      endpoint: 127.0.0.1:14317\n      tls:\n        insecure: true\n" + limits
		serviceExtensions = "[livecapture]"
		processors = "[memory_limiter, capture/ingress, batch]"
	}
	return extensions + `receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 127.0.0.1:4317
processors:
  memory_limiter:
    check_interval: 100ms
    limit_mib: 128
    spike_limit_mib: 32
  capture/ingress:
    sensitivity: redacted
  batch:
    timeout: 100ms
    send_batch_size: 1000
    send_batch_max_size: 1000
exporters:
  otlp/main:
    endpoint: 127.0.0.1:14318
    tls:
      insecure: true
    sending_queue:
      enabled: false
    retry_on_failure:
      enabled: false
service:
  extensions: ` + serviceExtensions + `
  telemetry:
    logs:
      level: error
    metrics:
      level: detailed
      readers:
        - pull:
            exporter:
              prometheus:
                host: 127.0.0.1
                port: 8888
  pipelines:
    traces:
      receivers: [otlp]
      processors: ` + processors + `
      exporters: [otlp/main]
    logs:
      receivers: [otlp]
      processors: ` + processors + `
      exporters: [otlp/main]
    metrics:
      receivers: [otlp]
      processors: ` + processors + `
      exporters: [otlp/main]
`
}

// One shared immutable request per signal. Unique IDs within each 100-record
// batch make selection percentages exact. This excludes generation cost from
// the measured pipeline; payload repetition and default gzip are disclosed.
func perfRequests(payload int) (ptraceotlp.ExportRequest, plogotlp.ExportRequest, pmetricotlp.ExportRequest) {
	t, l, m := synthetic(1)
	ts := t.ResourceSpans().At(0).ScopeSpans().At(0).Spans()
	firstSpan := ts.At(0)
	ls := l.ResourceLogs().At(0).ScopeLogs().At(0).LogRecords()
	firstLog := ls.At(0)
	ms := m.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(0).Gauge().DataPoints()
	firstPoint := ms.At(0)
	for i := range 100 {
		span, log, point := firstSpan, firstLog, firstPoint
		if i > 0 {
			span = ts.AppendEmpty()
			firstSpan.CopyTo(span)
			log = ls.AppendEmpty()
			firstLog.CopyTo(log)
			point = ms.AppendEmpty()
			firstPoint.CopyTo(point)
		}
		span.SetTraceID(pcommon.TraceID{byte(i + 1)})
		span.SetSpanID(pcommon.SpanID{byte(i + 1)})
		for _, attrs := range []pcommon.Map{span.Attributes(), log.Attributes(), point.Attributes()} {
			attrs.PutInt("perf.index", int64(i))
			if payload > 0 {
				attrs.PutStr("perf.payload", strings.Repeat("x", payload))
			}
		}
	}
	return ptraceotlp.NewExportRequestFromTraces(t), plogotlp.NewExportRequestFromLogs(l), pmetricotlp.NewExportRequestFromMetrics(m)
}
func perfHTTP(method, path string, value any, target any) error {
	var body []byte
	var err error
	if value != nil {
		body, err = json.Marshal(value)
		if err != nil {
			return err
		}
	}
	r, err := http.NewRequest(method, "http://127.0.0.1:17777"+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if value != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	client := http.Client{Timeout: 4 * time.Second}
	response, err := client.Do(r)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		b, _ := io.ReadAll(response.Body)
		return fmt.Errorf("control %d: %s", response.StatusCode, b)
	}
	if target != nil {
		return json.NewDecoder(response.Body).Decode(target)
	}
	return nil
}
func perfMetrics() map[string]float64 {
	values := map[string]float64{}
	client := http.Client{Timeout: time.Second}
	r, err := client.Get("http://127.0.0.1:8888/metrics")
	if err != nil {
		return values
	}
	defer r.Body.Close()
	scanner := bufio.NewScanner(r.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		key := strings.Split(fields[0], "{")[0]
		v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err == nil {
			values[key] += v
		}
	}
	return values
}

// Linux /proc uses USER_HZ=100 for exported process CPU ticks. Read both user
// and system time; child CPU is excluded. RSS comes from VmRSS, not Docker totals.
func perfProcess(pid int) (float64, int64) {
	b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	text := string(b)
	at := strings.LastIndex(text, ")")
	if at < 0 {
		return 0, 0
	}
	f := strings.Fields(text[at+1:])
	if len(f) < 13 {
		return 0, 0
	}
	u, _ := strconv.ParseFloat(f[11], 64)
	s, _ := strconv.ParseFloat(f[12], 64)
	b, _ = os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	var rss int64
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			rss, _ = strconv.ParseInt(fields[1], 10, 64)
			rss *= 1024
		}
	}
	return (u + s) / 100, rss
}
func performanceCase(binary string, duration time.Duration, c perfCase) (result perfResult, err error) {
	result.Case = c
	mainSink := &perfSink{}
	captureSink := &perfSink{captured: true, delay: c.Delay, outage: c.Outage}
	mainServer, err := servePerfSink("127.0.0.1:14318", mainSink)
	if err != nil {
		return result, err
	}
	defer mainServer.Stop()
	captureServer, err := servePerfSink("127.0.0.1:14317", captureSink)
	if err != nil {
		return result, err
	}
	defer captureServer.Stop()
	dir, err := os.MkdirTemp("", "capture-perf-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(dir)
	config := dir + "/collector.yaml"
	if err = os.WriteFile(config, []byte(perfConfiguration(c)), 0600); err != nil {
		return result, err
	}
	logs, err := os.Create(dir + "/collector.log")
	if err != nil {
		return result, err
	}
	defer logs.Close()
	cmd := exec.Command(binary, "--config", config)
	cmd.Stdout = logs
	cmd.Stderr = logs
	cmd.Env = append(os.Environ(), "GOMAXPROCS=2")
	if err = cmd.Start(); err != nil {
		return result, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); close(done) }()
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(4 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}()
	ready := false
	for range 100 {
		if len(perfMetrics()) > 0 {
			ready = true
			break
		}
		select {
		case e := <-done:
			b, _ := os.ReadFile(logs.Name())
			return result, fmt.Errorf("collector exited %v: %s", e, b)
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		return result, fmt.Errorf("collector metrics did not become ready")
	}
	conn, err := grpc.NewClient("127.0.0.1:4317", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return result, err
	}
	defer conn.Close()
	tr, lr, mr := perfRequests(c.Payload)
	traceClient := ptraceotlp.NewGRPCClient(conn)
	logClient := plogotlp.NewGRPCClient(conn)
	metricClient := pmetricotlp.NewGRPCClient(conn)
	export := func(ctx context.Context, signal int) error {
		switch signal {
		case 0:
			r, e := traceClient.Export(ctx, tr)
			if e == nil && r.PartialSuccess().RejectedSpans() > 0 {
				return fmt.Errorf("partial rejection")
			}
			return e
		case 1:
			r, e := logClient.Export(ctx, lr)
			if e == nil && r.PartialSuccess().RejectedLogRecords() > 0 {
				return fmt.Errorf("partial rejection")
			}
			return e
		default:
			r, e := metricClient.Export(ctx, mr)
			if e == nil && r.PartialSuccess().RejectedDataPoints() > 0 {
				return fmt.Errorf("partial rejection")
			}
			return e
		}
	}
	// Warm transports and the main batch exporter before enabling capture.
	for i := range 3 {
		for attempt := range 100 {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err = export(ctx, i)
			cancel()
			if err == nil {
				break
			}
			if attempt < 99 {
				time.Sleep(50 * time.Millisecond)
			}
		}
		if err != nil {
			return result, err
		}
	}
	time.Sleep(250 * time.Millisecond)
	if c.Mode != "absent" && c.Mode != "inactive" {
		filter := map[string]any{}
		switch c.Mode {
		case "resource-none":
			filter["resource"] = []string{`attributes["service.name"] == "never"`}
		case "span-none":
			filter["traces"] = map[string]any{"span": []string{`name == "never"`}}
			filter["logs"] = map[string]any{"log": []string{`body == "never"`}}
			filter["metrics"] = map[string]any{"datapoint": []string{`attributes["perf.index"] < 0`}}
		case "selective":
			condition := fmt.Sprintf(`attributes["perf.index"] < %d`, c.Percent)
			filter["traces"] = map[string]any{"span": []string{condition}}
			filter["logs"] = map[string]any{"log": []string{condition}}
			filter["metrics"] = map[string]any{"datapoint": []string{condition}}
		case "regex":
			filter["traces"] = map[string]any{"span": []string{`IsMatch(name, "^POST /api/(checkout|inventory|billing)$")`}}
			filter["logs"] = map[string]any{"log": []string{`IsMatch(body, "^Synthetic .* request #[0-9]+ returned [0-9]+$")`}}
			filter["metrics"] = map[string]any{"datapoint": []string{`IsMatch(attributes["url.path"], "^/api/(checkout|inventory|billing)$")`}}
		}
		request := map[string]any{"session_id": "perf", "principal": "local-perf", "signals": []string{"traces", "logs", "metrics"}, "target": map[string]string{"collector_group": "perf"}, "capture_points": []map[string]string{{"pipeline": "traces", "point": "ingress"}, {"pipeline": "logs", "point": "ingress"}, {"pipeline": "metrics", "point": "ingress"}}, "filter": filter, "expires_at": time.Now().Add(duration + 15*time.Second).UTC()}
		if c.Budget > 0 {
			request["limits"] = map[string]int{"total_records": c.Budget}
		}
		if c.RecordLimit > 0 {
			request["limits"] = map[string]int{"record_bytes": c.RecordLimit}
		}
		if err = perfHTTP("POST", "/v1/sessions", request, nil); err != nil {
			return result, err
		}
	}
	mainBefore := mainSink.total()
	captureBefore := captureSink.total()
	cpuBefore, _ := perfProcess(cmd.Process.Pid)
	metricsBefore := perfMetrics()
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	var samples sync.WaitGroup
	samples.Add(1)
	go func() {
		defer samples.Done()
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, rss := perfProcess(cmd.Process.Pid)
				result.CollectorPeakRSS = max(result.CollectorPeakRSS, rss)
				metrics := perfMetrics()
				result.QueuePeakBytes = max(result.QueuePeakBytes, int64(metrics["otelcol_capture_queue_bytes"]))
				result.CollectorPeakHeap = max(result.CollectorPeakHeap, int64(metrics["otelcol_process_runtime_heap_alloc_bytes"]))
				if result.FirstCaptureDropMS == 0 && metrics["otelcol_capture_records_dropped_total"] > 0 {
					result.FirstCaptureDropMS = float64(time.Since(started)) / float64(time.Millisecond)
				}
				queueLimit := int64(8 << 20)
				if c.QueueMiB > 0 {
					queueLimit = int64(c.QueueMiB) << 20
				}
				if result.Queue95PercentMS == 0 && int64(metrics["otelcol_capture_queue_bytes"]) >= queueLimit*95/100 {
					result.Queue95PercentMS = float64(time.Since(started)) / float64(time.Millisecond)
				}
			}
		}
	}()
	var accepted, errors atomic.Int64
	var wg sync.WaitGroup
	var latencyMu sync.Mutex
	var latencies []float64
	jobs := make(chan int)
	var firstInputError atomic.Int64
	for range 12 {
		wg.Go(func() {
			var local []float64
			defer func() { latencyMu.Lock(); latencies = append(latencies, local...); latencyMu.Unlock() }()
			for signal := range jobs {
				callCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
				before := time.Now()
				e := export(callCtx, signal)
				stop()
				local = append(local, float64(time.Since(before))/float64(time.Millisecond))
				if e != nil {
					errors.Add(1)
					firstInputError.CompareAndSwap(0, time.Since(started).Nanoseconds())
				} else {
					accepted.Add(100)
				}
			}
		})
	}
	// Absolute deadlines prevent the interactive generator's ticker quantization.
	interval := time.Second * 100 / time.Duration(c.Rate)
	for i := 0; ctx.Err() == nil; i++ {
		deadline := started.Add(time.Duration(i) * interval)
		if delay := time.Until(deadline); delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
			}
		}
		if ctx.Err() != nil {
			break
		}
		select {
		case jobs <- i % 3:
		case <-ctx.Done():
		}
	}
	close(jobs)
	wg.Wait()
	samples.Wait()
	result.Seconds = time.Since(started).Seconds()
	result.InputRecords = accepted.Load()
	result.InputErrors = errors.Load()
	result.FirstInputErrorMS = float64(firstInputError.Load()) / float64(time.Millisecond)
	result.InputRPS = float64(result.InputRecords) / result.Seconds
	cpuAfter, rss := perfProcess(cmd.Process.Pid)
	result.CollectorPeakRSS = max(result.CollectorPeakRSS, rss)
	result.CollectorCPUCores = (cpuAfter - cpuBefore) / result.Seconds
	slices.Sort(latencies)
	if len(latencies) > 0 {
		result.RequestP50MS = latencies[len(latencies)/2]
		result.RequestP95MS = latencies[min(len(latencies)-1, int(float64(len(latencies))*.95))]
	}
	// Freeze input, allow normal batches and healthy capture to finish, then drain.
	time.Sleep(350 * time.Millisecond)
	metricsAfter := perfMetrics()
	count := metricsAfter["otelcol_capture_filter_duration_seconds_count"] - metricsBefore["otelcol_capture_filter_duration_seconds_count"]
	if count > 0 {
		result.CaptureBatchMeanUS = (metricsAfter["otelcol_capture_filter_duration_seconds_sum"] - metricsBefore["otelcol_capture_filter_duration_seconds_sum"]) * 1e6 / count
	}
	for key, value := range metricsAfter {
		if strings.HasPrefix(key, "otelcol_exporter_send_failed_") {
			result.ExporterFailed += value - metricsBefore[key]
		}
	}
	if c.Mode != "absent" {
		before := time.Now()
		if err = perfHTTP("DELETE", "/v1/sessions/perf", nil, nil); err != nil {
			return result, err
		}
		result.StopMS = float64(time.Since(before)) / float64(time.Millisecond)
		var status struct {
			Sessions []json.RawMessage `json:"sessions"`
		}
		if err = perfHTTP("GET", "/v1/status", nil, &status); err != nil {
			return result, err
		}
		if len(status.Sessions) > 0 {
			result.CaptureStats = status.Sessions[len(status.Sessions)-1]
		}
	}
	result.MainRecords = mainSink.total() - mainBefore
	result.CapturedRecords = captureSink.total() - captureBefore
	result.InvalidTags = mainSink.invalid.Load() + captureSink.invalid.Load()
	if result.MainRecords != result.InputRecords {
		return result, fmt.Errorf("main pipeline mismatch: accepted=%d delivered=%d", result.InputRecords, result.MainRecords)
	}
	if result.InvalidTags != 0 {
		return result, fmt.Errorf("capture/original routing crossed")
	}
	if slices.Contains([]string{"absent", "inactive", "resource-none", "span-none"}, c.Mode) && result.CapturedRecords != 0 {
		return result, fmt.Errorf("unexpected capture with mode %s", c.Mode)
	}
	if c.Mode == "selective" && c.Raised && c.Rate <= 15000 && result.CapturedRecords != result.InputRecords*int64(c.Percent)/100 {
		return result, fmt.Errorf("selection percentage did not match delivered capture")
	}
	if c.Outage && result.CapturedRecords != 0 {
		return result, fmt.Errorf("injected outage unexpectedly delivered capture")
	}
	if c.Budget > 0 && result.CapturedRecords != int64(c.Budget) {
		return result, fmt.Errorf("capture total budget was not respected")
	}
	return result, nil
}
func runPerformance(binary string, duration time.Duration, selected string) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("run performance experiments in the Docker performance target")
	}
	if duration < time.Second || duration > 60*time.Second {
		return fmt.Errorf("perf-seconds must be 1..60")
	}
	var cases []perfCase
	for _, rate := range []int{3000, 15000, 60000, 180000} {
		for _, mode := range []string{"absent", "inactive", "all"} {
			cases = append(cases, perfCase{Name: fmt.Sprintf("%s-%d", mode, rate), Rate: rate, Mode: mode})
		}
	}
	for _, mode := range []string{"resource-none", "span-none"} {
		cases = append(cases, perfCase{Name: mode, Rate: 60000, Mode: mode})
	}
	for _, percent := range []int{1, 10, 100} {
		cases = append(cases, perfCase{Name: fmt.Sprintf("select-%d", percent), Rate: 15000, Mode: "selective", Percent: percent, Raised: true})
	}
	cases = append(cases,
		perfCase{Name: "raised-60000", Rate: 60000, Mode: "all", Raised: true},
		perfCase{Name: "raised-180000", Rate: 180000, Mode: "all", Raised: true},
		perfCase{Name: "slow-4k", Rate: 15000, Mode: "all", Raised: true, Payload: 4000, Delay: 500 * time.Millisecond},
		perfCase{Name: "outage-small", Rate: 15000, Mode: "all", Raised: true, Outage: true},
		perfCase{Name: "outage-4k", Rate: 15000, Mode: "all", Raised: true, Payload: 4000, Outage: true},
		perfCase{Name: "budget-1000", Rate: 3000, Mode: "all", Budget: 1000},
		perfCase{Name: "records-limit", Rate: 15000, Mode: "all", BytesRaised: true},
		perfCase{Name: "record-size", Rate: 3000, Mode: "all", Payload: 4000, RecordLimit: 1024},
		perfCase{Name: "regex", Rate: 15000, Mode: "regex", Raised: true},
		perfCase{Name: "absent-360000", Rate: 360000, Mode: "absent"},
		perfCase{Name: "all-360000", Rate: 360000, Mode: "all"},
		perfCase{Name: "raised-360000", Rate: 360000, Mode: "all", Raised: true},
		perfCase{Name: "outage-64m", Rate: 15000, Mode: "all", Raised: true, Payload: 4000, Outage: true, QueueMiB: 64},
	)
	matched := 0
	for _, c := range cases {
		if selected != "" && !slices.Contains(strings.Split(selected, ","), c.Name) {
			continue
		}
		matched++
		fmt.Fprintf(os.Stderr, "Performance experiment: %s\n", c.Name)
		r, err := performanceCase(binary, duration, c)
		if e := json.NewEncoder(os.Stdout).Encode(r); e != nil {
			return e
		}
		if err != nil {
			return fmt.Errorf("%s: %w", c.Name, err)
		}
	}
	if matched == 0 {
		return fmt.Errorf("no experiments matched %q", selected)
	}
	return nil
}
