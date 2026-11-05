package api

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// TestAPIAnswersWhileARunExecutes checks the requirement that the local API stays responsive
// while runs are executing: with the only worker held by a run, inspection
// of the 100-component workload, the run list, the run itself and the
// samples are answered. The run is released only after every answer, so a
// handler that waited on the worker could not answer at all.
func TestAPIAnswersWhileARunExecutes(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	service := platform.New(platform.Options{Paths: platform.PathPolicy{Unrestricted: true}, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		started <- struct{}{}
		<-release
		return &engine.Report{Result: &runresult.Result{RunID: req.RunID, Status: runresult.StatusCompleted}}, nil
	}})
	t.Cleanup(service.Close)
	_, srv := newServerOptions(t, Options{Platform: service})

	created := decodeBody[Run](t, post(t, srv.URL+"/api/v1/runs", inlined(t, multisignalConfig, multisignalScenario)), http.StatusAccepted)
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("worker did not start")
	}

	components := filepath.Join("..", "..", "..", "testdata", "perf", "components-100.yaml")
	if ins := decodeBody[engine.Inspection](t, post(t, srv.URL+"/api/v1/config/validate", InspectRequest{ConfigYAML: readText(t, components)}), http.StatusOK); !ins.Valid || len(ins.Topology.Components) != 100 {
		t.Fatalf("inspection while running = valid %v, diagnostics %v", ins.Valid, ins.Diagnostics)
	}
	if g := decodeBody[ConfigGraph](t, post(t, srv.URL+"/api/v1/config/graph", InspectRequest{ConfigYAML: readText(t, components)}), http.StatusOK); !g.Valid {
		t.Fatalf("graph while running = %+v", g.Diagnostics)
	}
	if runs := decodeBody[RunPage](t, get(t, srv.URL+"/api/v1/runs"), http.StatusOK).Runs; len(runs) != 1 || runs[0].State != StateRunning {
		t.Fatalf("run list while running = %+v", runs)
	}
	if v := decodeBody[Run](t, get(t, srv.URL+"/api/v1/runs/"+created.ID), http.StatusOK); v.State != StateRunning {
		t.Fatalf("run while running = %+v", v)
	}
	if resp := get(t, srv.URL+"/api/v1/samples"); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("samples while running: status %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	close(release)
	if rep := decodeBody[engine.Report](t, get(t, srv.URL+"/api/v1/runs/"+created.ID+"/report?wait=true"), http.StatusOK); rep.Result.Status != runresult.StatusCompleted {
		t.Fatalf("released run = %+v", rep.Result)
	}
}
