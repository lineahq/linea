package api

import (
	"bytes"
	"context"
	"github.com/lineahq/linea/internal/debugger/store/memory"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/platform"
)

func workspaceServer(t *testing.T, data *memory.Data) (*platform.Service, *Server, *httptest.Server) {
	t.Helper()
	service, err := platform.Open(memory.Open(data, artifact.Limits{}), platform.Options{Paths: platform.PathPolicy{Unrestricted: true}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	s, srv := newServerOptions(t, Options{Platform: service})
	return service, s, srv
}

func TestArtifactSaveRejectsClientOriginWithoutChangingArtifacts(t *testing.T) {
	_, _, srv := workspaceServer(t, memory.NewData())
	url := srv.URL + "/api/v1/artifacts"
	page := decodeBody[artifact.Page](t, get(t, url), http.StatusOK)
	first := decodeBody[artifact.Saved](t, post(t, url, SaveRequest{WorkspaceID: page.Workspace.ID, Kind: platform.ArtifactKind("configuration"), Content: []byte("original")}), http.StatusCreated)
	before := decodeBody[artifact.Page](t, get(t, url), http.StatusOK)
	beforeRevision := decodeBody[artifact.RevisionData](t, get(t, url+"/"+first.Artifact.ID+"/revisions/"+first.Artifact.Head), http.StatusOK)
	// Both creation and an otherwise valid head update must be rejected.
	for _, req := range []map[string]any{
		{"workspace_id": page.Workspace.ID, "kind": "configuration", "content_base64": "bmV3"},
		{"workspace_id": page.Workspace.ID, "kind": "configuration", "content_base64": "bmV3", "artifact_id": first.Artifact.ID, "expected_generation": first.Artifact.Generation, "expected_head_revision_id": first.Artifact.Head},
	} {
		req["origin"] = map[string]any{"kind": "candidate", "base_digest": "forged"}
		e := decodeBody[ArtifactError](t, post(t, url, req), http.StatusBadRequest)
		if e.Code != "invalid_request" {
			t.Fatalf("origin rejection = %+v", e)
		}
		after := decodeBody[artifact.Page](t, get(t, url), http.StatusOK)
		if !reflect.DeepEqual(before, after) {
			t.Fatal("origin rejection changed artifact list or heads")
		}
		afterRevision := decodeBody[artifact.RevisionData](t, get(t, url+"/"+first.Artifact.ID+"/revisions/"+first.Artifact.Head), http.StatusOK)
		if !reflect.DeepEqual(beforeRevision, afterRevision) {
			t.Fatal("origin rejection changed revision")
		}
	}
}

func TestArtifactsPersistOriginalBytesAndRejectStaleHeadUpdates(t *testing.T) {
	data := memory.NewData()
	service, _, srv := workspaceServer(t, data)
	url := srv.URL + "/api/v1/artifacts"
	page := decodeBody[artifact.Page](t, get(t, url), http.StatusOK)
	original := []byte("# verbatim\r\ninvalid draft: [\r\n\xff")
	req := artifact.SaveRequest{WorkspaceID: page.Workspace.ID, Kind: artifact.Configuration, Label: "config", Content: original}
	first := decodeBody[artifact.Saved](t, post(t, url, req), http.StatusCreated)
	if first.Revision.Actor != "local-owner" {
		t.Fatalf("actor = %s", first.Revision.Actor)
	}
	req.ArtifactID, req.ExpectedGeneration, req.ExpectedHead = first.Artifact.ID, first.Artifact.Generation, first.Artifact.Head
	req.Content = []byte("next draft")
	second := decodeBody[artifact.Saved](t, post(t, url, req), http.StatusCreated)
	conflict := decodeBody[ArtifactError](t, post(t, url, req), http.StatusConflict)
	if conflict.Code != "conflict" || conflict.Current == nil || conflict.Current.Head != second.Artifact.Head {
		t.Fatalf("conflict = %+v", conflict)
	}
	oldURL := url + "/" + first.Artifact.ID + "/revisions/" + first.Revision.Ref.RevisionID
	old := decodeBody[artifact.RevisionData](t, get(t, oldURL), http.StatusOK)
	if !bytes.Equal(old.Content, original) {
		t.Fatal("old revision changed")
	}
	srv.Close()
	service.Close()
	_, _, srv = workspaceServer(t, data)
	url = srv.URL + "/api/v1/artifacts"
	metadata := decodeBody[artifact.Metadata](t, get(t, url+"/"+first.Artifact.ID), http.StatusOK)
	if metadata.Head != second.Artifact.Head {
		t.Fatalf("recovered metadata = %+v", metadata)
	}
	old = decodeBody[artifact.RevisionData](t, get(t, url+"/"+first.Artifact.ID+"/revisions/"+first.Revision.Ref.RevisionID), http.StatusOK)
	if !bytes.Equal(old.Content, original) {
		t.Fatal("restart changed original bytes")
	}
}

func TestArtifactsAreSharedAndAdapterCloseKeepsServiceOpen(t *testing.T) {
	data := memory.NewData()
	service, a, first := workspaceServer(t, data)
	_, second := newServerOptions(t, Options{Platform: service})
	info, err := service.GetWorkspaceInfo(context.Background(), caller)
	if err != nil {
		t.Fatal(err)
	}
	saved := decodeBody[artifact.Saved](t, post(t, first.URL+"/api/v1/artifacts", artifact.SaveRequest{WorkspaceID: info.ID, Kind: artifact.Fixture, Content: []byte("{}")}), http.StatusCreated)
	a.Close()
	metadata := decodeBody[artifact.Metadata](t, get(t, second.URL+"/api/v1/artifacts/"+saved.Artifact.ID), http.StatusOK)
	if metadata.Head != saved.Artifact.Head {
		t.Fatal("adapters disagree about saved head")
	}
	if _, err := service.GetArtifact(context.Background(), caller, saved.Artifact.ID); err != nil {
		t.Fatalf("adapter close closed the shared service: %v", err)
	}
}

func TestArtifactErrorsAndClientActorRefusal(t *testing.T) {
	_, ephemeral := newServer(t)
	if e := decodeBody[ArtifactError](t, get(t, ephemeral.URL+"/api/v1/artifacts"), http.StatusServiceUnavailable); e.Code != "workspace_required" {
		t.Fatalf("error = %+v", e)
	}
	service, _, srv := workspaceServer(t, memory.NewData())
	url := srv.URL + "/api/v1/artifacts"
	for _, body := range []string{`{"actor":"forged"}`, `{}`, `{"content_base64":"not base64"}`, `{} {}`} {
		resp, err := http.Post(url, "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		e := decodeBody[ArtifactError](t, resp, http.StatusBadRequest)
		if e.Code == "" || e.Error == "" {
			t.Fatalf("error = %+v", e)
		}
	}
	for _, path := range []string{"?limit=nope", "?limit=-1", "?kind=unknown", "?cursor=../catalog"} {
		decodeBody[ArtifactError](t, get(t, url+path), http.StatusBadRequest)
	}
	decodeBody[ArtifactError](t, get(t, url+"/unknown"), http.StatusNotFound)
	service.Close()
	decodeBody[ArtifactError](t, get(t, url), http.StatusServiceUnavailable)
}

func TestPersistentSourcesDoNotRequireNamedArtifactsForQuickRuns(t *testing.T) {
	data := memory.NewData()
	service, _, srv := workspaceServer(t, data)
	tracesDir := filepath.Join("..", "..", "..", "testdata", "traces")
	created := decodeBody[Run](t, post(t, srv.URL+"/api/v1/runs", inlinedFixture(t, filepath.Join(tracesDir, "config.yaml"), filepath.Join(tracesDir, "traces.json"))), http.StatusAccepted)
	rep := decodeBody[engine.Report](t, get(t, srv.URL+"/api/v1/runs/"+created.ID+"/report?wait=true"), http.StatusOK)
	if rep.Assertions != nil || rep.Scenario != "" || len(rep.Result.Outputs) == 0 {
		t.Fatal("quick mode unexpectedly requires a saved scenario")
	}
	page := decodeBody[artifact.Page](t, get(t, srv.URL+"/api/v1/artifacts"), http.StatusOK)
	if len(page.Items) != 0 {
		t.Fatal("quick execution cluttered the named-artifact catalog")
	}
	srv.Close()
	service.Close()
	_, _, srv = workspaceServer(t, data)
	if runs := decodeBody[RunPage](t, get(t, srv.URL+"/api/v1/runs"), http.StatusOK).Runs; len(runs) != 1 || runs[0].ID != created.ID || runs[0].State != StateDone {
		t.Fatal("workspace lost quick-run history")
	}
}

func TestArtifactQuotaMappingAndPaging(t *testing.T) {
	service, err := platform.Open(memory.New(artifact.Limits{BlobBytes: 3}), platform.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	_, srv := newServerOptions(t, Options{Platform: service})
	url := srv.URL + "/api/v1/artifacts"
	page := decodeBody[artifact.Page](t, get(t, url), http.StatusOK)
	req := artifact.SaveRequest{WorkspaceID: page.Workspace.ID, Kind: artifact.Scenario, Content: []byte("long")}
	if e := decodeBody[ArtifactError](t, post(t, url, req), http.StatusRequestEntityTooLarge); e.Code != "resource_exhausted" {
		t.Fatalf("quota = %+v", e)
	}
	var saved []artifact.Saved
	for _, kind := range []artifact.Kind{artifact.Scenario, artifact.Setup} {
		req.Kind, req.Content = kind, []byte("{}")
		saved = append(saved, decodeBody[artifact.Saved](t, post(t, url, req), http.StatusCreated))
	}
	page = decodeBody[artifact.Page](t, get(t, url+"?limit=1"), http.StatusOK)
	next := decodeBody[artifact.Page](t, get(t, url+"?limit=1&cursor="+page.NextCursor), http.StatusOK)
	if len(page.Items) != 1 || len(next.Items) != 1 || page.Items[0].ID == next.Items[0].ID {
		t.Fatal("pagination repeated or skipped an artifact")
	}
	response := get(t, url+"/"+saved[0].Artifact.ID+"/revisions/"+saved[1].Revision.Ref.RevisionID)
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("artifact source can be cached")
	}
	decodeBody[ArtifactError](t, response, http.StatusNotFound)
}

// Runs and inspections take saved revisions by reference: the HTTP
// shape maps onto the platform's resolution and its error codes.
func TestRequestsBySavedRevisionReference(t *testing.T) {
	_, _, srv := workspaceServer(t, memory.NewData())
	tracesDir := filepath.Join("..", "..", "..", "testdata", "traces")
	config, err := os.ReadFile(filepath.Join(tracesDir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	traces, err := os.ReadFile(filepath.Join(tracesDir, "traces.json"))
	if err != nil {
		t.Fatal(err)
	}
	url := srv.URL + "/api/v1/artifacts"
	page := decodeBody[artifact.Page](t, get(t, url), http.StatusOK)
	save := func(kind string, content []byte) *platform.ArtifactRef {
		saved := decodeBody[artifact.Saved](t, post(t, url, SaveRequest{WorkspaceID: page.Workspace.ID, Kind: platform.ArtifactKind(kind), Content: content}), http.StatusCreated)
		return &platform.ArtifactRef{ArtifactID: saved.Artifact.ID, RevisionID: saved.Revision.Ref.RevisionID}
	}
	configRef, fixtureRef := save("configuration", config), save("fixture", traces)

	created := decodeBody[Run](t, post(t, srv.URL+"/api/v1/runs", Request{ConfigRef: configRef, FixtureRef: fixtureRef, FixtureLabel: "traces.json"}), http.StatusAccepted)
	rep := decodeBody[engine.Report](t, get(t, srv.URL+"/api/v1/runs/"+created.ID+"/report?wait=true"), http.StatusOK)
	if rep.Provenance.Fixture == nil || rep.Provenance.Fixture.Path != "traces.json" || len(rep.Result.Outputs) == 0 {
		t.Fatalf("report by reference = %+v", rep.Provenance)
	}
	run := decodeBody[Run](t, get(t, srv.URL+"/api/v1/runs/"+created.ID), http.StatusOK)
	if run.Bundle == nil || run.Bundle.Sources == nil || *run.Bundle.Sources.Config != *configRef || *run.Bundle.Sources.Fixture != *fixtureRef {
		t.Fatalf("bundle sources = %+v", run.Bundle)
	}
	ins := decodeBody[engine.Inspection](t, post(t, srv.URL+"/api/v1/config/validate", InspectRequest{ConfigRef: configRef, ConfigLabel: "saved"}), http.StatusOK)
	if !ins.Valid || ins.Config.Path != "saved" {
		t.Fatalf("inspection by reference = %+v", ins)
	}

	missing := &platform.ArtifactRef{ArtifactID: configRef.ArtifactID, RevisionID: "missing"}
	decodeBody[Error](t, post(t, srv.URL+"/api/v1/runs", Request{ConfigRef: missing, FixtureRef: fixtureRef}), http.StatusNotFound)
	decodeBody[Error](t, post(t, srv.URL+"/api/v1/config/validate", InspectRequest{ConfigRef: missing}), http.StatusNotFound)
	decodeBody[Error](t, post(t, srv.URL+"/api/v1/runs", Request{ConfigRef: fixtureRef, FixtureRef: fixtureRef}), http.StatusBadRequest)
	decodeBody[Error](t, post(t, srv.URL+"/api/v1/runs", Request{ConfigYAML: string(config), ConfigRef: configRef, FixtureRef: fixtureRef}), http.StatusBadRequest)

	_, ephemeral := newServer(t)
	decodeBody[Error](t, post(t, ephemeral.URL+"/api/v1/runs", Request{ConfigRef: configRef, FixtureJSON: string(traces)}), http.StatusServiceUnavailable)
	decodeBody[Error](t, post(t, ephemeral.URL+"/api/v1/config/validate", InspectRequest{ConfigRef: configRef}), http.StatusServiceUnavailable)
}
