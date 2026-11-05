package api

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// The log route returns a failed run's Collector log tail, uncached.
func TestRunLogRouteReturnsAFailedRunsLog(t *testing.T) {
	service := platform.New(platform.Options{Paths: platform.PathPolicy{Unrestricted: true}, Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
		_, _ = req.Log.Write([]byte("error\tcollector failed\n"))
		return nil, errors.New("collector failed to start")
	}})
	t.Cleanup(service.Close)
	s, srv := newServerOptions(t, Options{Platform: service})
	created := decodeBody[Run](t, post(t, srv.URL+"/api/v1/runs", inlined(t, multisignalConfig, multisignalScenario)), http.StatusAccepted)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	waitRun(ctx, t, s.runs, created.ID)

	resp := get(t, srv.URL+"/api/v1/runs/"+created.ID+"/log")
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	log := decodeBody[platform.RunLog](t, resp, http.StatusOK)
	if log != (platform.RunLog{RunID: created.ID, Kept: true, Log: "error\tcollector failed\n"}) {
		t.Fatalf("log = %+v", log)
	}
	decodeBody[Error](t, get(t, srv.URL+"/api/v1/runs/missing/log"), http.StatusNotFound)
}
