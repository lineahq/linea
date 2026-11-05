package testbed

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/open-telemetry/opentelemetry-collector-contrib/testbed/testbed"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// selectedAttribute marks every selectEvery-th generated record. Every
// scenario, including the baseline, carries it, so payloads are identical.
const (
	selectedAttribute = "linea.selected"
	selectEvery       = 10
)

// mode is one pipeline variant: the capture processor in one of its states,
// or a reference that measures stock components on the same load.
type mode struct {
	name string
	// processor adds the livecapture extension and capture/ingress processor.
	processor bool
	// session starts a capture session before the load; filter is its record
	// condition ("" captures every record).
	session bool
	filter  string
	// captured is the expected number of captured copies per sent record.
	captured func(sent uint64) uint64
	// reference replaces the capture processor with a contrib processor and
	// returns its name and configuration.
	reference func(s signal) (name, config string)
	// secondExporter adds a plain OTLP exporter to the capture backend with
	// this compression: what sending every record twice costs without capture.
	secondExporter string
}

// neverMatches selects no record; filters with it evaluate every record and
// keep or capture none.
const neverMatches = `attributes["` + selectedAttribute + `"] == "never"`

var modes = []mode{
	{name: "Baseline", captured: none},
	{name: "Passthrough", processor: true, captured: none},
	{name: "CaptureAll", processor: true, session: true, captured: all},
	{name: "CaptureFilter10pct", processor: true, session: true, filter: `attributes["` + selectedAttribute + `"] == true`, captured: func(sent uint64) uint64 { return sent / selectEvery }},
	{name: "CaptureFilterNone", processor: true, session: true, filter: neverMatches, captured: none},
	// Reference modes measure stock components on the same load.
	{name: "RefSecondExporterGzip", secondExporter: "gzip", captured: all},
	{name: "RefSecondExporterNone", secondExporter: "none", captured: all},
	{name: "RefFilterNone", captured: none, reference: func(s signal) (string, string) {
		// error_mode ignore matches the capture filters, where an
		// evaluation error counts as no match.
		return "filter", fmt.Sprintf(`
  filter:
    error_mode: ignore
    %s:
      %s:
        - '%s.%s'`, s.pipeline, s.filterProcessorContext, s.context, neverMatches)
	}},
	{name: "RefTransformSet", captured: none, reference: func(s signal) (string, string) {
		return "transform", fmt.Sprintf(`
  transform:
    error_mode: ignore
    %s:
      - context: %s
        statements:
          - 'set(%s.attributes["linea.reference"], "set")'`, s.transformStatements, s.context, s.context)
	}},
	{name: "RefAttributesUpsert", captured: none, reference: func(signal) (string, string) {
		return "attributes", `
  attributes:
    actions:
      - key: linea.reference
        value: set
        action: upsert`
	}},
}

func none(uint64) uint64     { return 0 }
func all(sent uint64) uint64 { return sent }

// signal is one telemetry type with the testbed sender and the session filter
// key and record context the capture extension expects.
type signal struct {
	pipeline  string
	sender    func(port int) testbed.DataSender
	filterKey string
	context   string
	// filterProcessorContext and transformStatements configure the record
	// context of the reference processors.
	filterProcessorContext string
	transformStatements    string
	// limits enable the testbed's CPU and RSS sampling (it samples only
	// when a limit is set) and guard against regressions: about twice the
	// highest peak any mode reached on Linux. See
	// docs/collector-components/benchmarks.md.
	limits testbed.ResourceSpec
}

var signals = []signal{
	{
		pipeline: "traces", filterKey: "traces", context: "span",
		filterProcessorContext: "span", transformStatements: "trace_statements",
		sender: func(port int) testbed.DataSender { return testbed.NewOTLPTraceDataSender(testbed.DefaultHost, port) },
		limits: testbed.ResourceSpec{ExpectedMaxCPU: 30, ExpectedMaxRAM: 120},
	},
	{
		pipeline: "logs", filterKey: "logs", context: "log",
		filterProcessorContext: "log_record", transformStatements: "log_statements",
		sender: func(port int) testbed.DataSender { return testbed.NewOTLPLogsDataSender(testbed.DefaultHost, port) },
		limits: testbed.ResourceSpec{ExpectedMaxCPU: 30, ExpectedMaxRAM: 120},
	},
	{
		pipeline: "metrics", filterKey: "metrics", context: "datapoint",
		filterProcessorContext: "datapoint", transformStatements: "metric_statements",
		sender: func(port int) testbed.DataSender { return testbed.NewOTLPMetricDataSender(testbed.DefaultHost, port) },
		limits: testbed.ResourceSpec{ExpectedMaxCPU: 60, ExpectedMaxRAM: 120},
	},
}

func TestCaptureTraces10kSPS(t *testing.T)  { runSignal(t, signals[0]) }
func TestCaptureLogs10kDPS(t *testing.T)    { runSignal(t, signals[1]) }
func TestCaptureMetrics10kDPS(t *testing.T) { runSignal(t, signals[2]) }

func runSignal(t *testing.T, s signal) {
	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) { scenario(t, s, m) })
	}
}

// scenario follows contrib's Scenario10kItemsPerSecond: 10k items per second
// in batches of 100 from one sender, a Collector child process limited to
// GOMAXPROCS=2 with the batch processor, and an OTLP/gRPC mock backend.
func scenario(t *testing.T, s signal, m mode) {
	sender := s.sender(freePort(t))
	receiver := testbed.NewOTLPDataReceiver(freePort(t))
	capturePort, controlPort := freePort(t), freePort(t)

	agent := testbed.NewChildProcessCollector(testbed.WithEnvVar("GOMAXPROCS", "2"))
	cleanup, err := agent.PrepareConfig(t, collectorConfig(s, m, sender, receiver, capturePort, controlPort))
	require.NoError(t, err)
	t.Cleanup(cleanup)

	options := testbed.LoadOptions{DataItemsPerSecond: 10_000, ItemsPerBatch: 100, Parallel: 1}
	tc := testbed.NewTestCase(t, &markingProvider{DataProvider: testbed.NewPerfTestDataProvider(options)}, sender, receiver, agent, &testbed.PerfTestValidator{}, performanceResultsSummary, testbed.WithResourceLimits(s.limits))
	t.Cleanup(tc.Stop)

	var captureBackend *testbed.MockBackend
	if m.processor || m.secondExporter != "" {
		// The capture extension's delivery exporter, or the second exporter,
		// sends copies here.
		captureBackend = testbed.NewMockBackend(tc.ComposeTestResultFileName("capture-backend.log"), testbed.NewOTLPDataReceiver(capturePort))
		require.NoError(t, captureBackend.Start())
		t.Cleanup(captureBackend.Stop)
	}

	tc.StartBackend()
	tc.StartAgent()
	control := fmt.Sprintf("http://127.0.0.1:%d", controlPort)
	if m.session {
		startSession(t, tc, control, s, m)
	}

	tc.StartLoad(options)
	tc.WaitFor(func() bool { return tc.LoadGenerator.DataItemsSent() > 0 }, "load generator started")
	tc.Sleep(tc.Duration)
	tc.StopLoad()
	tc.WaitFor(func() bool { return tc.LoadGenerator.DataItemsSent() == tc.MockBackend.DataItemsReceived() }, "all data items received")

	if captureBackend != nil {
		want := m.captured(tc.LoadGenerator.DataItemsSent())
		ok := tc.WaitForN(func() bool { return captureBackend.DataItemsReceived() >= want }, 10*time.Second, "all captured copies received")
		got := captureBackend.DataItemsReceived()
		if m.session {
			status := sessionStatus(t, control)
			require.NoError(t, os.WriteFile(tc.ComposeTestResultFileName("capture-status.json"), status, 0o600))
			t.Logf("captured %d of %d sent (want %d); session %s", got, tc.LoadGenerator.DataItemsSent(), want, status)
		}
		if ok && got != want {
			t.Errorf("captured %d copies, want %d", got, want)
		}
	}
	tc.ValidateData()
}

func collectorConfig(s signal, m mode, sender testbed.DataSender, receiver testbed.DataReceiver, capturePort, controlPort int) string {
	extensions, serviceExtensions, processors := "", "[]", "[batch]"
	if m.processor {
		// Limits are raised so a 10k/s session measures processor cost, not
		// the stock rate and session budgets (which would drop copies).
		extensions = fmt.Sprintf(`
extensions:
  livecapture:
    enabled: true
    environment: test
    collector_group: testbed
    pod: testbed
    local_control:
      enabled: true
      endpoint: 127.0.0.1:%d
      allow_raw: true
    delivery:
      endpoint: 127.0.0.1:%d
      tls:
        insecure: true
    limits:
      max_bytes_per_second: 1GiB
      max_records_per_second: 10000000
      max_total_bytes: 8GiB
      max_total_records: 100000000
`, controlPort, capturePort)
		serviceExtensions = "[livecapture]"
		processors = "[capture/ingress, batch]"
	}
	reference := ""
	if m.reference != nil {
		var name string
		name, reference = m.reference(s)
		processors = "[" + name + ", batch]"
	}
	exporters, exporterNames := receiver.GenConfigYAMLStr(), receiver.ProtocolName()
	if m.secondExporter != "" {
		exporters += fmt.Sprintf(`
  otlp_grpc/second:
    endpoint: 127.0.0.1:%d
    compression: %s
    tls:
      insecure: true`, capturePort, m.secondExporter)
		exporterNames += ", otlp_grpc/second"
	}
	return fmt.Sprintf(`
receivers:%s
exporters:%s
processors:
  batch:
  capture/ingress:
    sensitivity: redacted%s
%s
service:
  extensions: %s
  telemetry:
    metrics:
      level: none
  pipelines:
    %s:
      receivers: [%s]
      processors: %s
      exporters: [%s]
`, sender.GenConfigYAMLStr(), exporters, reference, extensions, serviceExtensions, s.pipeline, sender.ProtocolName(), processors, exporterNames)
}

// startSession opens a session through the extension's local control
// endpoint, retrying until the Collector child process is listening.
func startSession(t *testing.T, tc *testbed.TestCase, control string, s signal, m mode) {
	filter := map[string]any{}
	if m.filter != "" {
		filter[s.filterKey] = map[string]any{s.context: []string{m.filter}}
	}
	request, err := json.Marshal(map[string]any{
		"session_id":     "testbed",
		"principal":      "testbed",
		"signals":        []string{s.pipeline},
		"target":         map[string]string{"collector_group": "testbed"},
		"capture_points": []map[string]string{{"pipeline": s.pipeline, "point": "ingress"}},
		"filter":         filter,
		// The extension caps sessions at five minutes.
		"expires_at": time.Now().Add(min(tc.Duration+time.Minute, 5*time.Minute)).UTC(),
	})
	require.NoError(t, err)
	var last error
	started := tc.WaitForN(func() bool {
		response, err := http.Post(control+"/v1/sessions", "application/json", bytes.NewReader(request))
		if err != nil {
			last = err
			return false
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		if response.StatusCode >= 300 {
			last = fmt.Errorf("control %d: %s", response.StatusCode, body)
			return false
		}
		return true
	}, 20*time.Second, "capture session started")
	if !started {
		t.Fatalf("start capture session: %v", last)
	}
}

func sessionStatus(t *testing.T, control string) []byte {
	response, err := http.Get(control + "/v1/status")
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	var compact bytes.Buffer
	if json.Compact(&compact, body) == nil {
		return compact.Bytes()
	}
	return body
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// markingProvider wraps contrib's perf data provider and marks every
// selectEvery-th record of each batch with selectedAttribute, so a filter can
// select an exact share of the load.
type markingProvider struct {
	testbed.DataProvider
}

func (p *markingProvider) GenerateTraces() (ptrace.Traces, bool) {
	data, done := p.DataProvider.GenerateTraces()
	var i int
	for _, rs := range data.ResourceSpans().All() {
		for _, ss := range rs.ScopeSpans().All() {
			for _, span := range ss.Spans().All() {
				mark(span.Attributes(), &i)
			}
		}
	}
	return data, done
}

func (p *markingProvider) GenerateLogs() (plog.Logs, bool) {
	data, done := p.DataProvider.GenerateLogs()
	var i int
	for _, rl := range data.ResourceLogs().All() {
		for _, sl := range rl.ScopeLogs().All() {
			for _, record := range sl.LogRecords().All() {
				mark(record.Attributes(), &i)
			}
		}
	}
	return data, done
}

func (p *markingProvider) GenerateMetrics() (pmetric.Metrics, bool) {
	data, done := p.DataProvider.GenerateMetrics()
	var i int
	for _, rm := range data.ResourceMetrics().All() {
		for _, sm := range rm.ScopeMetrics().All() {
			for _, metric := range sm.Metrics().All() {
				// The perf provider generates gauges only.
				for _, point := range metric.Gauge().DataPoints().All() {
					mark(point.Attributes(), &i)
				}
			}
		}
	}
	return data, done
}

func mark(attributes pcommon.Map, i *int) {
	attributes.PutBool(selectedAttribute, *i%selectEvery == 0)
	*i++
}
