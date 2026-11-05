package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"gopkg.in/yaml.v3"

	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/ottldebug"
	"github.com/lineahq/linea/internal/debugger/platform"
	"github.com/lineahq/linea/internal/debugger/scenario"
	"github.com/lineahq/linea/samples"
)

var update = flag.Bool("update", false, "rewrite openapi.json")

// TestDecodeReadsOneDocument: a request body is one JSON document, which
// whitespace may follow and nothing else.
func TestDecodeReadsOneDocument(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		ok         bool
	}{
		{"one document", `{"name":"a"}`, true},
		{"a trailing newline", `{"name":"a"}` + "\n", true},
		{"trailing whitespace", `{"name":"a"}` + " \t\r\n ", true},
		{"two documents", `{"name":"a"}{"name":"b"}`, false},
		{"two documents on two lines", `{"name":"a"}` + "\n" + `{"name":"b"}` + "\n", false},
		{"trailing garbage", `{"name":"a"}garbage`, false},
		{"a trailing value", `{"name":"a"} 1`, false},
		{"a trailing bracket", `{"name":"a"}}`, false},
		{"empty", ``, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var v struct{ Name string }
			w := httptest.NewRecorder()
			ok := decode(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body)), &v)
			if ok != tc.ok {
				t.Fatalf("decode = %v, want %v (%d %s)", ok, tc.ok, w.Code, w.Body)
			}
			if !ok && w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", w.Code)
			}
			if ok && v.Name != "a" {
				t.Errorf("decoded %+v", v)
			}
		})
	}
}

var (
	multisignalConfig   = filepath.Join("..", "..", "..", "testdata", "multisignal", "config.yaml")
	multisignalScenario = filepath.Join("..", "..", "..", "testdata", "multisignal", "scenario.yaml")
)

// The server reads no host file, so tests send their files as the connected
// CLI does: as text, the scenario's input files inlined, the path as label.

// readText reads a test file as request text.
func readText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// scenarioText reads a test scenario with the files it names inlined.
func scenarioText(t *testing.T, path string) string {
	t.Helper()
	raw, err := scenario.Inline([]byte(readText(t, path)), filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// inlined is a scenario run of a test configuration and scenario.
func inlined(t *testing.T, config, scenario string) Request {
	t.Helper()
	return Request{ConfigYAML: readText(t, config), ConfigLabel: config, ScenarioYAML: scenarioText(t, scenario), ScenarioLabel: scenario}
}

// inlinedFixture is a quick run of a test configuration and fixture.
func inlinedFixture(t *testing.T, config, fixture string) Request {
	t.Helper()
	return Request{ConfigYAML: readText(t, config), ConfigLabel: config, FixtureJSON: readText(t, fixture), FixtureLabel: fixture}
}

func newServer(t *testing.T) (*Server, *httptest.Server) {
	return newServerOptions(t, Options{})
}

func newServerOptions(t *testing.T, opts Options) (*Server, *httptest.Server) {
	t.Helper()
	s := New(opts)
	srv := httptest.NewServer(s)
	t.Cleanup(func() {
		srv.Close()
		s.Close()
	})
	return s, srv
}

func post(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func get(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeBody[T any](t *testing.T, resp *http.Response, status int) T {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != status {
		t.Fatalf("%s %s: status %d, want %d: %s", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, status, raw)
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	return v
}

// sseMessage is one server-sent event.
type sseMessage struct{ id, event, data string }

func readSSE(t *testing.T, r io.Reader) []sseMessage {
	t.Helper()
	var out []sseMessage
	var cur sseMessage
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			out = append(out, cur)
			cur = sseMessage{}
			continue
		}
		k, v, _ := strings.Cut(line, ": ")
		switch k {
		case "id":
			cur.id = v
		case "event":
			cur.event = v
		case "data":
			cur.data = v
		}
	}
	return out
}

// TestRunLifecycle follows the web client's path: queue a run, follow its
// event stream to the end, then fetch the run, its report, a snapshot and
// its diffs, each independently of the stream.
func TestRunLifecycle(t *testing.T) {
	_, srv := newServer(t)
	created := decodeBody[Run](t, post(t, srv.URL+"/api/v1/runs", inlined(t, multisignalConfig, multisignalScenario)), http.StatusAccepted)
	if created.ID == "" || (created.State != StateQueued && created.State != StateRunning) {
		t.Fatalf("created = %+v", created)
	}
	base := srv.URL + "/api/v1/runs/" + created.ID

	resp := get(t, base+"/events")
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	msgs := readSSE(t, resp.Body)
	resp.Body.Close()
	last := msgs[len(msgs)-1]
	var end End
	if err := json.Unmarshal([]byte(last.data), &end); err != nil || last.event != "end" ||
		end.State != StateDone || end.Status != runresult.StatusCompleted {
		t.Fatalf("last message %+v (%v)", last, err)
	}
	events := msgs[:len(msgs)-1]
	var snapshot string
	for i, m := range events {
		var e instrument.Event
		if err := json.Unmarshal([]byte(m.data), &e); err != nil {
			t.Fatal(err)
		}
		if m.event != "event" || e.Seq != i+1 || m.id != "1:"+e.ID[strings.LastIndex(e.ID, ":")+1:] || e.RunID != created.ID {
			t.Fatalf("message %d = %+v, event %+v", i, m, e)
		}
		if snapshot == "" {
			snapshot = e.Snapshot
		}
	}

	got := decodeBody[Run](t, get(t, base), http.StatusOK)
	if got.State != StateDone || got.Report == nil || got.Report.Result.Snapshots != nil ||
		len(got.Report.Result.Events) != len(events) || got.Finished == nil {
		t.Fatalf("run = %+v", got)
	}
	rep := decodeBody[engine.Report](t, get(t, base+"/report"), http.StatusOK)
	if len(rep.Result.Snapshots) == 0 || rep.Result.RunID != created.ID {
		t.Fatalf("report snapshots %d, run %s", len(rep.Result.Snapshots), rep.Result.RunID)
	}
	otlp := decodeBody[map[string]any](t, get(t, base+"/snapshots/"+snapshot), http.StatusOK)
	if len(otlp) == 0 {
		t.Fatal("empty snapshot")
	}
	diffs := decodeBody[Diffs](t, get(t, base+"/diffs"), http.StatusOK)
	if len(diffs.Stages) == 0 || len(diffs.OTTL) != 1 {
		t.Fatalf("diffs = %+v", diffs)
	}
	list := decodeBody[RunPage](t, get(t, srv.URL+"/api/v1/runs"), http.StatusOK).Runs
	if len(list) != 1 || list[0].ID != created.ID || list[0].Report != nil {
		t.Fatalf("list = %+v", list)
	}

	// A reconnecting client resumes after the last event it saw.
	req, _ := http.NewRequest(http.MethodGet, base+"/events", nil)
	req.Header.Set("Last-Event-ID", events[len(events)-3].id)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resumed := readSSE(t, resp.Body)
	resp.Body.Close()
	if len(resumed) != 3 || resumed[0].id != events[len(events)-2].id || resumed[2].event != "end" {
		t.Fatalf("resumed = %+v", resumed)
	}

	// A client whose cursor is in another attempt is told to restart and
	// receives the run's attempt from its first event.
	req, _ = http.NewRequest(http.MethodGet, base+"/events", nil)
	req.Header.Set("Last-Event-ID", "2:5")
	if resp, err = http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	}
	restarted := readSSE(t, resp.Body)
	resp.Body.Close()
	var restart Restart
	if len(restarted) != len(msgs)+1 || restarted[0].event != "restart" || restarted[0].id != "1:0" ||
		json.Unmarshal([]byte(restarted[0].data), &restart) != nil || restart.Attempt != 1 || restarted[1].id != "1:1" {
		t.Fatalf("restarted = %.3v", restarted)
	}
}

func TestEventCursorsNameAnAttemptAndASeq(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want platform.EventCursor
		ok   bool
	}{
		{"2:17", platform.EventCursor{Attempt: 2, Seq: 17}, true},
		{"1:0", platform.EventCursor{Attempt: 1}, true},
		{"17", platform.EventCursor{Attempt: 1, Seq: 17}, true}, // an id from before attempts
		{"0:3", platform.EventCursor{}, false},
		{"1:-1", platform.EventCursor{}, false},
		{"a:1", platform.EventCursor{}, false},
		{"1:", platform.EventCursor{}, false},
		{"", platform.EventCursor{}, false},
	} {
		got, err := parseEventCursor(tc.id)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("parseEventCursor(%q) = %+v, %v", tc.id, got, err)
		}
	}
}

// TestRunAcceptsInlineInputs: the editor's text is run without files.
func TestRunAcceptsInlineInputs(t *testing.T) {
	_, srv := newServer(t)
	cfg, err := os.ReadFile(multisignalConfig)
	if err != nil {
		t.Fatal(err)
	}
	scenario := "version: 1\nenvironment: {DEPLOY_ENV: test}\ninputs:\n  - signal: logs\n    data: {resourceLogs: [{scopeLogs: [{logRecords: [{body: {stringValue: hello}}]}]}]}\n"
	created := decodeBody[Run](t, post(t, srv.URL+"/api/v1/runs", engine.Request{ConfigYAML: string(cfg), ScenarioYAML: scenario}), http.StatusAccepted)
	rep := decodeBody[engine.Report](t, get(t, srv.URL+"/api/v1/runs/"+created.ID+"/report?wait=true"), http.StatusOK)
	// otlp/backend and the local debug exporter.
	if rep.Result.Status != runresult.StatusCompleted || len(rep.Result.Outputs) != 2 || rep.Provenance.Config.Path != "" {
		t.Fatalf("result = %+v, provenance %+v", rep.Result, rep.Provenance)
	}
}

func TestRunRejectsPreparationErrorsBeforeAdmission(t *testing.T) {
	s, srv := newServer(t)
	if e := decodeBody[Error](t, post(t, srv.URL+"/api/v1/runs", engine.Request{ConfigYAML: "receivers: {nope: {}}\n", FixtureJSON: `{"resourceSpans":[]}`}), http.StatusUnprocessableEntity); e.Error == "" {
		t.Fatal("no error")
	}
	if len(listRuns(t, s.runs)) != 0 {
		t.Fatal("preparation failure admitted a run")
	}
}

func TestQuickRunRetainsOptionalMocksWithoutRequiringScenario(t *testing.T) {
	s, srv := newServer(t)
	dir := filepath.Join("..", "..", "..", "samples", "pod-enrichment")
	fixture, err := os.ReadFile(filepath.Join(dir, "input.json"))
	if err != nil {
		t.Fatal(err)
	}
	scn, err := scenario.Load(filepath.Join(dir, "scenario.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	setup, err := yaml.Marshal(map[string]any{"mocks": scn.Mocks})
	if err != nil {
		t.Fatal(err)
	}
	created := decodeBody[Run](t, post(t, srv.URL+"/api/v1/runs", engine.Request{
		ConfigYAML: readText(t, filepath.Join(dir, "config.yaml")), FixtureJSON: string(fixture),
		ClientAddress: "10.42.0.17", SetupYAML: string(setup),
	}), http.StatusAccepted)
	rep := decodeBody[engine.Report](t, get(t, srv.URL+"/api/v1/runs/"+created.ID+"/report?wait=true"), http.StatusOK)
	if rep.Assertions != nil || rep.Scenario != "" || rep.Provenance.Setup == nil || rep.Result.Status != runresult.StatusCompleted {
		t.Fatalf("quick mode lost its contract: %+v", rep)
	}
	checks, err := assertion.Evaluate(scn.Assertions, rep.Topology, rep.Result)
	if err != nil || checks.Outcome != assertion.Pass {
		t.Fatalf("quick mocks were not applied: %+v, %v", checks, err)
	}
	r, err := s.runs.GetRun(context.Background(), caller, created.ID)
	if err != nil || r.Mode != platform.ModeQuick || r.Caller != platform.Local("http").Actor() {
		t.Fatalf("quick mode was not retained: %v", err)
	}
}

// TestCancelQueuedRun blocks an injected executor so the next run stays queued.
func TestCancelQueuedRun(t *testing.T) {
	started := make(chan struct{}, 1)
	service := platform.New(platform.Options{Paths: platform.PathPolicy{Unrestricted: true}, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		started <- struct{}{}
		<-ctx.Done()
		return &engine.Report{Result: &runresult.Result{RunID: req.RunID, Status: runresult.StatusCancelled}}, nil
	}})
	t.Cleanup(service.Close)
	if _, err := service.SubmitRun(context.Background(), platform.Local("test"), platform.RunRequest{Mode: platform.ModeScenario, Config: platform.Source{Path: multisignalConfig}, Scenario: &platform.ScenarioInput{Scenario: platform.Source{Path: multisignalScenario}}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start")
	}
	s, srv := newServerOptions(t, Options{Platform: service})
	created := decodeBody[Run](t, post(t, srv.URL+"/api/v1/runs", inlined(t, multisignalConfig, multisignalScenario)), http.StatusAccepted)
	if created.State != StateQueued {
		t.Fatalf("state = %s", created.State)
	}
	base := srv.URL + "/api/v1/runs/" + created.ID
	decodeBody[Error](t, get(t, base+"/report"), http.StatusConflict)
	decodeBody[Run](t, post(t, base+"/cancel", struct{}{}), http.StatusAccepted)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if v := waitRun(ctx, t, s.runs, created.ID); v.State != platform.StateCancelled || v.Started != nil {
		t.Fatalf("run = %+v", v)
	}
}

func TestSharedPlatformOwnershipAndCrossAdapterCancellation(t *testing.T) {
	started := make(chan struct{}, 1)
	service := platform.New(platform.Options{Paths: platform.PathPolicy{Unrestricted: true}, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		started <- struct{}{}
		<-ctx.Done()
		return &engine.Report{Result: &runresult.Result{RunID: req.RunID, Status: runresult.StatusCancelled}}, nil
	}})
	t.Cleanup(service.Close)
	first, a := newServerOptions(t, Options{Platform: service})
	_, b := newServerOptions(t, Options{Platform: service})
	created := decodeBody[Run](t, post(t, a.URL+"/api/v1/runs", inlined(t, multisignalConfig, multisignalScenario)), http.StatusAccepted)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start")
	}
	first.Close()
	base := b.URL + "/api/v1/runs/" + created.ID
	if v := decodeBody[Run](t, get(t, base), http.StatusOK); v.State != StateRunning {
		t.Fatalf("closing adapter changed shared run: %+v", v)
	}
	decodeBody[Run](t, post(t, base+"/cancel", struct{}{}), http.StatusAccepted)
	rep := decodeBody[engine.Report](t, get(t, base+"/report?wait=true"), http.StatusOK)
	if rep.Result.RunID != created.ID || rep.Result.Status != runresult.StatusCancelled {
		t.Fatalf("cross-adapter cancellation lost evidence: %+v", rep.Result)
	}
}

func TestPlatformAdmissionErrors(t *testing.T) {
	started := make(chan struct{}, 1)
	service := platform.New(platform.Options{QueueCapacity: 1, Paths: platform.PathPolicy{Unrestricted: true}, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		started <- struct{}{}
		<-ctx.Done()
		return &engine.Report{Result: &runresult.Result{RunID: req.RunID, Status: runresult.StatusCancelled}}, nil
	}})
	t.Cleanup(service.Close)
	_, srv := newServerOptions(t, Options{Platform: service})
	url := srv.URL + "/api/v1/runs"
	req := inlined(t, multisignalConfig, multisignalScenario)
	decodeBody[Run](t, post(t, url, req), http.StatusAccepted)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start")
	}
	decodeBody[Run](t, post(t, url, req), http.StatusAccepted)
	if e := decodeBody[Error](t, post(t, url, req), http.StatusServiceUnavailable); e.Error != platform.ErrQueueFull.Error() {
		t.Fatalf("full queue = %+v", e)
	}
	if got := decodeBody[RunPage](t, get(t, url), http.StatusOK).Runs; len(got) != 2 {
		t.Fatalf("rejected submission created a run: %+v", got)
	}
	service.Close()
	if e := decodeBody[Error](t, post(t, url, req), http.StatusServiceUnavailable); e.Error != platform.ErrClosed.Error() {
		t.Fatalf("closed platform = %+v", e)
	}
}

func TestValidateAndGraph(t *testing.T) {
	_, srv := newServer(t)
	cfg, err := os.ReadFile(multisignalConfig)
	if err != nil {
		t.Fatal(err)
	}
	ins := decodeBody[engine.Inspection](t, post(t, srv.URL+"/api/v1/config/validate", InspectRequest{ConfigYAML: readText(t, multisignalConfig), ConfigLabel: multisignalConfig}), http.StatusOK)
	if !ins.Valid || ins.Topology == nil || ins.Effective == nil || ins.Config.Path != multisignalConfig {
		t.Fatalf("inspection = %+v", ins)
	}
	g := decodeBody[ConfigGraph](t, post(t, srv.URL+"/api/v1/config/graph", engine.InspectRequest{ConfigYAML: string(cfg)}), http.StatusOK)
	if !g.Valid || len(g.Topology.Pipelines) == 0 {
		t.Fatalf("graph = %+v", g)
	}
	bad := decodeBody[engine.Inspection](t, post(t, srv.URL+"/api/v1/config/validate", engine.InspectRequest{ConfigYAML: "receivers: {nope: {}}\n"}), http.StatusOK)
	if bad.Valid || len(bad.Diagnostics) == 0 || bad.Topology != nil {
		t.Fatalf("inspection = %+v", bad)
	}
}

func TestRejectsBadRequests(t *testing.T) {
	_, srv := newServer(t)
	tests := []struct {
		name, method, path, contentType, host, body string
		want                                        int
	}{
		{"malformed", "POST", "/api/v1/runs", "application/json", "", "{", http.StatusBadRequest},
		{"unknown field", "POST", "/api/v1/runs", "application/json", "", `{"config_yaml":"x","fixture_json":"y","bogus":1}`, http.StatusBadRequest},
		{"missing input", "POST", "/api/v1/runs", "application/json", "", `{"config_yaml":"x"}`, http.StatusBadRequest},
		{"a host path", "POST", "/api/v1/config/validate", "application/json", "", `{"config":"config.yaml"}`, http.StatusBadRequest},
		{"a scenario directory", "POST", "/api/v1/runs", "application/json", "", `{"config_yaml":"x","scenario_yaml":"y","scenario_dir":"."}`, http.StatusBadRequest},
		{"form post", "POST", "/api/v1/runs", "text/plain", "", `{}`, http.StatusUnsupportedMediaType},
		{"foreign host", "GET", "/api/v1/runs", "", "attacker.example:7070", "", http.StatusForbidden},
		{"unknown run", "GET", "/api/v1/runs/nope", "", "", "", http.StatusNotFound},
		{"unknown endpoint", "GET", "/api/v1/nope", "", "", "", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(tt.method, srv.URL+tt.path, strings.NewReader(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			if tt.host != "" {
				req.Host = tt.host
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			if e := decodeBody[Error](t, resp, tt.want); e.Error == "" {
				t.Fatal("no error message")
			}
		})
	}
}

func TestLoopbackHost(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1:7070": true, "localhost:7070": true, "LOCALHOST": true, "[::1]:7070": true, "127.5.0.1": true,
		"example.com:7070": false, "192.168.1.2:7070": false, "localhost.example.com": false, "": false,
	} {
		if got := loopbackHost(host); got != want {
			t.Errorf("loopbackHost(%q) = %v", host, got)
		}
	}
}

// TestOpenAPIIsCurrent keeps the committed contract equal to the generated
// one. Regenerate with: go test ./internal/debugger/api -run TestOpenAPIIsCurrent -update
func TestOpenAPIIsCurrent(t *testing.T) {
	doc, err := OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	if *update {
		if err := os.WriteFile("openapi.json", doc, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	committed, err := os.ReadFile("openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.ReplaceAll(committed, []byte("\r\n"), []byte("\n")), doc) {
		t.Fatal("openapi.json is stale; run go test ./internal/debugger/api -run TestOpenAPIIsCurrent -update")
	}
	var parsed struct {
		Components struct {
			Schemas map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(doc, &parsed); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Request", "Report", "Run", "Event", "Inspection", "Stage", "Diff"} {
		if parsed.Components.Schemas[name] == nil {
			t.Errorf("schema %s missing", name)
		}
	}
	_, srv := newServer(t)
	served := get(t, srv.URL+"/api/v1/openapi.json")
	defer served.Body.Close()
	body, _ := io.ReadAll(served.Body)
	if !bytes.Equal(body, doc) {
		t.Fatal("served document differs")
	}
}

func TestFallbackServesNonAPIPaths(t *testing.T) {
	s := New(Options{Fallback: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "app")
	})})
	srv := httptest.NewServer(s)
	defer srv.Close()
	defer s.Close()
	resp := get(t, srv.URL+"/runs/abc")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "app" {
		t.Fatalf("body = %q", body)
	}
	if resp := get(t, srv.URL+"/api/v1/nope"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("api status %d", resp.StatusCode)
	}
}

func TestEvaluateOTTL(t *testing.T) {
	_, srv := newServer(t)
	cfg, err := os.ReadFile(multisignalConfig)
	if err != nil {
		t.Fatal(err)
	}
	logs, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "multisignal", "logs.json"))
	if err != nil {
		t.Fatal(err)
	}
	url := srv.URL + "/api/v1/ottl/evaluate"
	ev := decodeBody[engine.OTTLEvaluation](t, post(t, url, engine.OTTLRequest{
		ConfigYAML: string(cfg), ScenarioYAML: scenarioText(t, multisignalScenario), Processor: "transform/logs", Input: logs,
	}), http.StatusOK)
	if !ev.Valid || ev.Replay == nil || ev.Replay.Verification != ottldebug.VerificationNotVerifiable ||
		len(ev.Output) == 0 || ev.Diff == nil || len(ev.Diff.Changes) == 0 {
		t.Fatalf("evaluation = %+v", ev)
	}

	// An edited statement that does not parse is a diagnostic, not an error.
	edited := strings.Replace(string(cfg), `set(log.severity_text, "WARN")`, `set(log.severity_text, "WARN"`, 1)
	bad := decodeBody[engine.OTTLEvaluation](t, post(t, url, engine.OTTLRequest{
		ConfigYAML: edited, ScenarioYAML: scenarioText(t, multisignalScenario), Processor: "transform/logs", Input: logs,
	}), http.StatusOK)
	if bad.Valid || len(bad.Diagnostics) == 0 || bad.Replay != nil {
		t.Fatalf("evaluation of an unparseable statement = %+v", bad)
	}

	for name, req := range map[string]engine.OTTLRequest{
		"not a transform": {ConfigYAML: string(cfg), Processor: "batch", Input: logs},
		"unknown":         {ConfigYAML: string(cfg), Processor: "transform/nope", ScenarioYAML: scenarioText(t, multisignalScenario), Input: logs},
		"no input":        {ConfigYAML: string(cfg), Processor: "transform/logs"},
	} {
		if resp := post(t, url, req); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, resp.StatusCode)
		}
	}
}

// TestEvaluateAssertionsAgainstARun re-evaluates edited assertions against a
// done run's recorded observations, including attribute assertions that read
// its snapshots.
func TestEvaluateAssertionsAgainstARun(t *testing.T) {
	_, srv := newServer(t)
	dir := filepath.Join("..", "..", "..", "testdata", "paging")
	created := decodeBody[Run](t, post(t, srv.URL+"/api/v1/runs", engine.Request{
		ConfigYAML: readText(t, filepath.Join(dir, "config.yaml")), ScenarioYAML: scenarioText(t, filepath.Join(dir, "scenario.yaml")),
	}), http.StatusAccepted)
	rep := decodeBody[engine.Report](t, get(t, srv.URL+"/api/v1/runs/"+created.ID+"/report?wait=true"), http.StatusOK)
	if rep.Assertions == nil || rep.Assertions.Outcome != assertion.Fail {
		t.Fatalf("run assertions = %+v", rep.Assertions)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "scenario.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	original := string(raw)
	url := srv.URL + "/api/v1/runs/" + created.ID + "/assertions"
	for _, tc := range []struct {
		name     string
		scenario string
		want     assertion.Outcome
	}{
		{"unchanged", original, assertion.Fail},
		{"expect no page", strings.Replace(strings.Replace(original, "equals: 1}", "equals: 0}", 1), "    attributes:\n      equals: {route: pager}\n", "", 1), assertion.Pass},
		{"archived attribute", original + "    attributes:\n      equals: {route: archive}\n", assertion.Fail},
	} {
		got := decodeBody[assertion.Report](t, post(t, url, engine.AssertionsRequest{ScenarioYAML: tc.scenario}), http.StatusOK)
		if got.Outcome != tc.want {
			t.Errorf("%s: outcome %s, want %s: %+v", tc.name, got.Outcome, tc.want, got.Results)
		}
	}

	for _, tc := range []struct {
		name   string
		url    string
		body   engine.AssertionsRequest
		status int
	}{
		{"no scenario", url, engine.AssertionsRequest{}, http.StatusBadRequest},
		{"invalid scenario", url, engine.AssertionsRequest{ScenarioYAML: "version: 9\n"}, http.StatusUnprocessableEntity},
		{"unknown run", srv.URL + "/api/v1/runs/nope/assertions", engine.AssertionsRequest{ScenarioYAML: original}, http.StatusNotFound},
	} {
		if resp := post(t, tc.url, tc.body); resp.StatusCode != tc.status {
			t.Errorf("%s: status %d, want %d", tc.name, resp.StatusCode, tc.status)
		}
	}
}

// TestSamplesRunFromTheirServedText runs a served sample both ways the web
// client offers: its scenario text, and its input alone (fixture_json).
func TestSamplesRunFromTheirServedText(t *testing.T) {
	_, srv := newServer(t)
	list := decodeBody[[]samples.Sample](t, get(t, srv.URL+"/api/v1/samples"), http.StatusOK)
	if len(list) == 0 {
		t.Fatal("no samples")
	}
	s := list[0]
	for name, req := range map[string]engine.Request{
		"scenario": {ConfigYAML: s.ConfigYAML, ScenarioYAML: s.ScenarioYAML},
		"quick":    {ConfigYAML: s.ConfigYAML, FixtureJSON: s.InputJSON, Receiver: s.Receiver},
	} {
		created := decodeBody[Run](t, post(t, srv.URL+"/api/v1/runs", req), http.StatusAccepted)
		rep := decodeBody[engine.Report](t, get(t, srv.URL+"/api/v1/runs/"+created.ID+"/report?wait=true"), http.StatusOK)
		if len(rep.Result.Outputs) == 0 || rep.Provenance.Fixture == nil && name == "quick" {
			t.Errorf("%s run: outputs %+v, provenance %+v", name, rep.Result.Outputs, rep.Provenance)
		}
		if name == "scenario" && (rep.Assertions == nil || rep.Assertions.Outcome != assertion.Outcome(s.Expect)) {
			t.Errorf("scenario run assertions = %+v, want %s", rep.Assertions, s.Expect)
		}
	}
}
