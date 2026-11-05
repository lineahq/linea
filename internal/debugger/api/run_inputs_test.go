package api

import (
	"github.com/lineahq/linea/internal/debugger/store/memory"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// The inputs route returns a run's admitted sources, uncached, and never the
// environment values the configuration read.
func TestRunInputsRouteReturnsSourcesWithoutEnvironmentValues(t *testing.T) {
	const name, secret = "OTEL_API_RUN_INPUTS", "private-host-value"
	t.Setenv(name, secret)
	traces, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "traces", "traces.json"))
	if err != nil {
		t.Fatal(err)
	}
	collectorYAML := "receivers: {otlp: {protocols: {grpc: {}}}}\nprocessors: {resource: {attributes: [{key: captured, value: '${env:" + name + "}', action: upsert}]}}\nexporters: {otlp: {endpoint: example.invalid:4317}}\nservice: {pipelines: {traces: {receivers: [otlp], processors: [resource], exporters: [otlp]}}}\n"
	service, err := platform.Open(memory.New(artifact.Limits{}), platform.Options{Environment: config.EnvironmentPolicy{Host: []string{name}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	_, srv := newServerOptions(t, Options{Platform: service})
	page := decodeBody[artifact.Page](t, get(t, srv.URL+"/api/v1/artifacts"), http.StatusOK)
	saved := decodeBody[artifact.Saved](t, post(t, srv.URL+"/api/v1/artifacts", SaveRequest{WorkspaceID: page.Workspace.ID, Kind: artifact.Configuration, Content: []byte(collectorYAML)}), http.StatusCreated)
	ref := &platform.ArtifactRef{ArtifactID: saved.Artifact.ID, RevisionID: saved.Revision.Ref.RevisionID}

	for _, req := range []Request{
		{ConfigRef: ref, FixtureJSON: string(traces), FixtureLabel: "traces.json", Receiver: "otlp"},
		{ConfigYAML: collectorYAML, ConfigLabel: "edited.yaml", ScenarioYAML: "version: 1\ninputs:\n  - data: '" + strings.ReplaceAll(strings.TrimSpace(string(traces)), "\n", " ") + "'\n"},
	} {
		created := decodeBody[Run](t, post(t, srv.URL+"/api/v1/runs", req), http.StatusAccepted)
		resp := get(t, srv.URL+"/api/v1/runs/"+created.ID+"/inputs")
		if got := resp.Header.Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q", got)
		}
		raw := string(responseBytes(t, resp, http.StatusOK))
		if strings.Contains(raw, secret) {
			t.Fatalf("environment value returned: %s", raw)
		}
		in := decodeBody[RunInputs](t, get(t, srv.URL+"/api/v1/runs/"+created.ID+"/inputs"), http.StatusOK)
		if in.RunID != created.ID || in.Config.Text != collectorYAML || !slices.Equal(in.RequiredEnvironment, []string{name}) || in.Files == nil {
			t.Fatalf("inputs = %+v", in)
		}
		if req.ConfigRef != nil {
			if in.Mode != "quick" || in.Config.Artifact == nil || *in.Config.Artifact != *ref || in.Fixture == nil || in.Fixture.Label != "traces.json" ||
				in.Fixture.Text != string(traces) || in.Receiver != "otlp" || in.Scenario != nil {
				t.Fatalf("quick inputs = %+v", in)
			}
		} else if in.Mode != "scenario" || in.Config.Label != "edited.yaml" || in.Config.Artifact != nil || in.Scenario == nil || in.Scenario.Text != req.ScenarioYAML || in.Fixture != nil {
			t.Fatalf("scenario inputs = %+v", in)
		}
	}
	decodeBody[Error](t, get(t, srv.URL+"/api/v1/runs/missing/inputs"), http.StatusNotFound)
}
