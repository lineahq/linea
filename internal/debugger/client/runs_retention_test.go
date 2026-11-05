package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/platform"
	"github.com/lineahq/linea/internal/debugger/scenario"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

// TestRetentionMethodsSpeakTheRetentionRoutes keeps, unkeeps, prunes and
// reads the storage of a workspace through a real API server, and keeps its
// refusals' status and code. A bodyless PUT must carry the JSON content
// type the server demands.
func TestRetentionMethodsSpeakTheRetentionRoutes(t *testing.T) {
	service, err := platform.Open(memory.Open(memory.NewData(), artifact.Limits{}), platform.Options{Paths: platform.PathPolicy{Unrestricted: true},
		Retention: platform.RetentionPolicy{Runs: 100, Age: 48 * time.Hour},
		Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
			return &engine.Report{Schema: engine.ReportSchemaVersion, Result: &runresult.Result{RunID: req.RunID, Status: runresult.StatusCompleted}}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	s := api.New(api.Options{Platform: service})
	srv := httptest.NewServer(s)
	t.Cleanup(func() {
		srv.Close()
		s.Close()
		service.Close()
	})
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop()

	dir := filepath.Join("..", "..", "..", "testdata", "multisignal")
	config, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	scenarioFile, err := os.ReadFile(filepath.Join(dir, "scenario.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	scenarioYAML, err := scenario.Inline(scenarioFile, dir)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for range 2 {
		if _, _, err := c.Run(ctx, api.Request{ConfigYAML: string(config), ScenarioYAML: string(scenarioYAML)}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := c.ListRuns(ctx, platform.RunQuery{})
	if err != nil || len(page.Runs) != 2 {
		t.Fatalf("ListRuns = %+v, %v", page, err)
	}
	for _, r := range page.Runs {
		ids = append(ids, r.ID)
	}

	var run api.Run
	raw, err := c.KeepRun(ctx, ids[0])
	if err != nil || json.Unmarshal(raw, &run) != nil || !run.Kept || run.ID != ids[0] {
		t.Fatalf("KeepRun = %s, %v", raw, err)
	}
	usage := storageUsage(t, c, ctx)
	if usage.Runs != 2 || usage.KeptRuns != 1 || usage.OldestRun == nil || usage.Retention != (api.RetentionPolicy{Runs: 100, AgeSeconds: 48 * 3600}) {
		t.Fatalf("StorageUsage = %+v", usage)
	}

	pruneResult := func(req api.RunPruneRequest) api.RunPruneResult {
		t.Helper()
		raw, err := c.PruneRuns(ctx, req)
		var res api.RunPruneResult
		if err != nil || json.Unmarshal(raw, &res) != nil {
			t.Fatalf("PruneRuns(%+v) = %s, %v", req, raw, err)
		}
		return res
	}
	dry := pruneResult(api.RunPruneRequest{DryRun: true})
	if dry.Runs != 1 || !dry.DryRun || dry.Bytes <= 0 {
		t.Fatalf("dry run = %+v", dry)
	}
	if done := pruneResult(api.RunPruneRequest{}); done.Runs != dry.Runs || done.Bytes != dry.Bytes || done.DryRun {
		t.Fatalf("prune = %+v, dry run said %+v", done, dry)
	}
	if usage := storageUsage(t, c, ctx); usage.Runs != 1 {
		t.Fatalf("StorageUsage after the prune = %+v", usage)
	}

	run = api.Run{}
	raw, err = c.UnkeepRun(ctx, ids[0])
	if err != nil || json.Unmarshal(raw, &run) != nil || run.Kept {
		t.Fatalf("UnkeepRun = %s, %v", raw, err)
	}
	var refused *Error
	if _, err := c.KeepRun(ctx, "run-unknown"); !errors.As(err, &refused) || refused.Status != 404 || refused.Code != string(platform.CodeNotFound) {
		t.Fatalf("keeping an unknown run = %v", err)
	}
	if _, err := c.PruneRuns(ctx, api.RunPruneRequest{Result: "unknown"}); !errors.As(err, &refused) || refused.Status != 400 || refused.Code != string(platform.CodeInvalidRequest) {
		t.Fatalf("an unknown result = %v", err)
	}
}

func storageUsage(t *testing.T, c *Client, ctx context.Context) api.StorageUsage {
	t.Helper()
	raw, err := c.StorageUsage(ctx)
	var usage api.StorageUsage
	if err != nil || json.Unmarshal(raw, &usage) != nil {
		t.Fatalf("StorageUsage = %s, %v", raw, err)
	}
	return usage
}
