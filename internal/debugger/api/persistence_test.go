package api

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/lineahq/linea/internal/debugger/store/memory"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/platform"
)

func responseBytes(t *testing.T, resp *http.Response, status int) []byte {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != status {
		t.Fatalf("response %d: %s, %v", resp.StatusCode, raw, err)
	}
	return raw
}

func TestWorkspaceAPIReopensIdenticalEvidenceInBothModes(t *testing.T) {
	for _, mode := range []string{"quick", "scenario"} {
		t.Run(mode, func(t *testing.T) {
			data := memory.NewData()
			service, _, srv := workspaceServer(t, data)
			req := engine.Request{ConfigYAML: readText(t, multisignalConfig), Compare: true, Normalize: mode == "quick"}
			if mode == "quick" {
				req.FixtureJSON = readText(t, filepath.Join("..", "..", "..", "testdata", "traces", "traces.json"))
			} else {
				req.ScenarioYAML = scenarioText(t, multisignalScenario)
			}
			created := decodeBody[Run](t, post(t, srv.URL+"/api/v1/runs", req), http.StatusAccepted)
			path := "/api/v1/runs/" + created.ID
			original := responseBytes(t, get(t, srv.URL+path+"/report?wait=true"), http.StatusOK)
			var rep engine.Report
			if err := json.Unmarshal(original, &rep); err != nil {
				t.Fatal(err)
			}
			if len(rep.Result.Snapshots) == 0 || rep.Reference == nil {
				t.Fatal("acceptance run lacks snapshots/reference evidence")
			}
			diffs := responseBytes(t, get(t, srv.URL+path+"/diffs"), http.StatusOK)
			events := responseBytes(t, get(t, srv.URL+path+"/events"), http.StatusOK)
			var snapshotRef string
			for ref := range rep.Result.Snapshots {
				snapshotRef = ref
				break
			}
			snapshot := responseBytes(t, get(t, srv.URL+path+"/snapshots/"+snapshotRef), http.StatusOK)
			srv.Close()
			service.Close()
			_, _, srv = workspaceServer(t, data)
			for endpoint, want := range map[string][]byte{"/report": original, "/diffs": diffs, "/events": events, "/snapshots/" + snapshotRef: snapshot} {
				got := responseBytes(t, get(t, srv.URL+path+endpoint), http.StatusOK)
				if !bytes.Equal(got, want) {
					t.Fatalf("%s changed on restart: before %.200s, after %.200s", endpoint, want, got)
				}
			}
			request, _ := http.NewRequest(http.MethodGet, srv.URL+path+"/events", nil)
			request.Header.Set("Last-Event-ID", "1:1")
			resp, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			messages := readSSE(t, resp.Body)
			resp.Body.Close()
			if len(messages) < 2 || messages[0].id != "1:2" || messages[len(messages)-1].event != "end" {
				t.Fatal("recovered SSE resume lost cursor or terminal end")
			}
		})
	}
}

func TestWorkspaceAdmissionAndEvidenceErrorsHaveExplicitHTTPStatus(t *testing.T) {
	service, err := platform.Open(memory.New(artifact.Limits{Runs: 1, RunBytes: 1}), platform.Options{Prepare: func(_ context.Context, req engine.Request) (engine.Request, error) { return req, nil }, Execute: func(context.Context, engine.Request) (*engine.Report, error) {
		return &engine.Report{Schema: engine.ReportSchemaVersion}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	_, srv := newServerOptions(t, Options{Platform: service})
	req := engine.Request{ConfigYAML: "config", FixtureJSON: "{}"}
	created := decodeBody[Run](t, post(t, srv.URL+"/api/v1/runs", req), http.StatusAccepted)
	decodeBody[Error](t, get(t, srv.URL+"/api/v1/runs/"+created.ID+"/report?wait=true"), http.StatusInternalServerError)
	decodeBody[Error](t, post(t, srv.URL+"/api/v1/runs", req), http.StatusRequestEntityTooLarge)
	if len(listRuns(t, service)) != 1 {
		t.Fatal("failed admission added history")
	}
}

func TestRecoveredInterruptedRunUsesCompatibleHTTPState(t *testing.T) {
	data := memory.NewData()
	store := memory.Open(data, artifact.Limits{})
	if err := store.EnableRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	id := "run-0123456789abcdef"
	metadata, _ := json.Marshal(map[string]any{"schema": 1, "id": id, "mode": "quick", "state": "queued", "created": time.Now().UTC(), "started": time.Time{}, "finished": time.Time{}})
	if _, err := store.SaveRun(context.Background(), id, artifact.DefaultProject, 0, false, artifact.RunIndex{}, metadata, nil); err != nil {
		t.Fatal(err)
	}
	store.Close()
	_, _, srv := workspaceServer(t, data)
	path := srv.URL + "/api/v1/runs/" + id
	r := decodeBody[Run](t, get(t, path), http.StatusOK)
	if r.State != StateError || !r.Interrupted || r.Error == "" || r.Report != nil {
		t.Fatalf("interrupted = %+v", r)
	}
	decodeBody[Error](t, get(t, path+"/report?wait=true"), http.StatusConflict)
	resp := get(t, path+"/events")
	messages := readSSE(t, resp.Body)
	resp.Body.Close()
	if len(messages) != 1 || messages[0].event != "end" {
		t.Fatal("interrupted stream did not terminate without invented events")
	}
	var end End
	if err := json.Unmarshal([]byte(messages[0].data), &end); err != nil || end.State != StateError || end.Error == "" {
		t.Fatalf("end = %+v, %v", end, err)
	}
	cancelled := decodeBody[Run](t, post(t, path+"/cancel", nil), http.StatusAccepted)
	if !cancelled.Interrupted || cancelled.State != StateError {
		t.Fatal("cancellation changed recovered state")
	}
}

// runJournal encodes every stored run record with its evaluations.
func runJournal(t *testing.T, data *memory.Data) []byte {
	t.Helper()
	ctx := context.Background()
	store := memory.Open(data, artifact.Limits{})
	defer store.Close()
	if err := store.EnableRuns(ctx); err != nil {
		t.Fatal(err)
	}
	records, err := store.Runs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	journal := map[string]any{"runs": records}
	for _, r := range records {
		evaluations, err := store.Evaluations(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		journal["evaluations/"+r.ID] = evaluations
	}
	raw, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// HTTP v1 evaluates assertions without recording them, even on a workspace.
func TestWorkspaceAssertionEvaluationRecordsNothing(t *testing.T) {
	data := memory.NewData()
	service, _, srv := workspaceServer(t, data)
	fixtures := filepath.Join("..", "..", "..", "testdata", "paging")
	created := decodeBody[Run](t, post(t, srv.URL+"/api/v1/runs", inlined(t, filepath.Join(fixtures, "config.yaml"), filepath.Join(fixtures, "scenario.yaml"))), http.StatusAccepted)
	before := decodeBody[engine.Report](t, get(t, srv.URL+"/api/v1/runs/"+created.ID+"/report?wait=true"), http.StatusOK)
	var expected bytes.Buffer
	if before.Assertions == nil {
		t.Fatal("run has no recorded assertions")
	}
	if err := platform.Encode(&expected, before.Assertions); err != nil {
		t.Fatal(err)
	}
	scenario, err := os.ReadFile(filepath.Join(fixtures, "scenario.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	journal := runJournal(t, data)
	response := responseBytes(t, post(t, srv.URL+"/api/v1/runs/"+created.ID+"/assertions", engine.AssertionsRequest{ScenarioYAML: string(scenario)}), http.StatusOK)
	if !bytes.Equal(expected.Bytes(), response) {
		t.Fatal("HTTP evaluation changed the assertion response")
	}
	var got assertion.Report
	if err := json.Unmarshal(response, &got); err != nil {
		t.Fatal(err)
	}
	if got.Outcome == "" {
		t.Fatalf("evaluation = %+v", got)
	}
	if infos, err := service.ListEvaluations(context.Background(), platform.Local("test"), created.ID); err != nil || len(infos) != 0 {
		t.Fatalf("HTTP evaluation was recorded: %+v, %v", infos, err)
	}
	if now := runJournal(t, data); !bytes.Equal(journal, now) {
		t.Fatal("HTTP evaluation changed the run journal")
	}
}
