package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/platform"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

// retentionServer is a workspace server with owners, another engineer, a
// viewer and an admin, which headerAuthenticator signs in by X-Test-Group.
func retentionServer(t *testing.T, policy platform.RetentionPolicy) (*httptest.Server, func(user, group string) map[string]string) {
	t.Helper()
	access, err := platform.NewPolicy([]platform.Binding{
		{Subject: platform.Subject{Group: "admins"}, Role: "admin", Scope: platform.ScopeAll},
		{Subject: platform.Subject{Group: "engineers"}, Role: "engineer", Scope: platform.ScopeAll},
		{Subject: platform.Subject{Group: "readers"}, Role: "viewer", Scope: platform.ScopeAll},
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := platform.Open(memory.Open(memory.NewData(), artifact.Limits{}), platform.Options{Access: access, Retention: policy, Paths: platform.PathPolicy{Unrestricted: true},
		Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
			return &engine.Report{Schema: engine.ReportSchemaVersion, Result: &runresult.Result{RunID: req.RunID, Status: runresult.StatusCompleted}}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	_, srv := newServerOptions(t, Options{Platform: service, Authenticator: headerAuthenticator{}})
	return srv, func(user, group string) map[string]string {
		return map[string]string{"X-Test-User": user, "X-Test-Group": group, "Content-Type": "application/json"}
	}
}

// submitFinished admits a run as header's user and waits for its report.
func submitFinished(t *testing.T, srv *httptest.Server, header map[string]string) Run {
	t.Helper()
	run := decodeBody[Run](t, identityRequest(t, http.MethodPost, srv.URL+"/api/v1/runs", inlined(t, multisignalConfig, multisignalScenario), header), http.StatusAccepted)
	decodeBody[map[string]any](t, identityRequest(t, http.MethodGet, srv.URL+"/api/v1/runs/"+run.ID+"/report?wait=true", nil, header), http.StatusOK)
	return run
}

// TestKeepRoutesShowTheMarkInEveryRunView: the owner keeps and unkeeps a
// run and both answers, the run and the run list show kept; another
// engineer is forbidden and an unknown run is 404.
func TestKeepRoutesShowTheMarkInEveryRunView(t *testing.T) {
	srv, as := retentionServer(t, platform.RetentionPolicy{})
	alice, bob := as("alice", "engineers"), as("bob", "engineers")
	run := submitFinished(t, srv, alice)
	keep := srv.URL + "/api/v1/runs/" + run.ID + "/keep"
	listed := func() []Run {
		return decodeBody[RunPage](t, identityRequest(t, http.MethodGet, srv.URL+"/api/v1/runs", nil, alice), http.StatusOK).Runs
	}

	for name, tc := range map[string]struct {
		method string
		header map[string]string
		url    string
		status int
		code   platform.Code
	}{
		"another engineer keeps":   {http.MethodPut, bob, keep, http.StatusForbidden, platform.CodeForbidden},
		"another engineer unkeeps": {http.MethodDelete, bob, keep, http.StatusForbidden, platform.CodeForbidden},
		"unknown run":              {http.MethodPut, alice, srv.URL + "/api/v1/runs/run-unknown/keep", http.StatusNotFound, platform.CodeNotFound},
	} {
		e := decodeBody[OperationError](t, identityRequest(t, tc.method, tc.url, nil, tc.header), tc.status)
		if e.Code != string(tc.code) {
			t.Errorf("%s: %+v", name, e)
		}
	}
	if got := listed(); len(got) != 1 || got[0].Kept {
		t.Fatalf("the run list before keeping = %+v", got)
	}

	if kept := decodeBody[Run](t, identityRequest(t, http.MethodPut, keep, nil, alice), http.StatusOK); kept.ID != run.ID || !kept.Kept {
		t.Fatalf("keep = %+v", kept)
	}
	if got := decodeBody[Run](t, identityRequest(t, http.MethodGet, srv.URL+"/api/v1/runs/"+run.ID, nil, alice), http.StatusOK); !got.Kept {
		t.Fatalf("a kept run reads %+v", got)
	}
	if got := listed(); len(got) != 1 || !got[0].Kept {
		t.Fatalf("the run list after keeping = %+v", got)
	}
	if got := decodeBody[Run](t, identityRequest(t, http.MethodDelete, keep, nil, alice), http.StatusOK); got.Kept {
		t.Fatalf("unkeep = %+v", got)
	}
	if got := listed(); len(got) != 1 || got[0].Kept {
		t.Fatalf("the run list after unkeeping = %+v", got)
	}
	if kept := decodeBody[Run](t, identityRequest(t, http.MethodPut, keep, nil, as("root", "admins")), http.StatusOK); !kept.Kept {
		t.Fatalf("an admin keeps another's run: %+v", kept)
	}
}

// TestPruneRunsDryRunCountsWhatThePruneRemoves: a dry run removes nothing
// and counts what the same request then removes, kept runs stay, and a
// non-admin, a bad filter and an unknown field are refused.
func TestPruneRunsDryRunCountsWhatThePruneRemoves(t *testing.T) {
	srv, as := retentionServer(t, platform.RetentionPolicy{})
	alice, root := as("alice", "engineers"), as("root", "admins")
	var runs []Run
	for range 3 {
		runs = append(runs, submitFinished(t, srv, alice))
	}
	decodeBody[Run](t, identityRequest(t, http.MethodPut, srv.URL+"/api/v1/runs/"+runs[0].ID+"/keep", nil, alice), http.StatusOK)
	prune := srv.URL + "/api/v1/runs/prune"

	for name, tc := range map[string]struct {
		header map[string]string
		body   any
		status int
		code   platform.Code
	}{
		"an engineer prunes": {alice, map[string]any{"dry_run": true}, http.StatusForbidden, platform.CodeForbidden},
		"a viewer prunes":    {as("vera", "readers"), map[string]any{"dry_run": true}, http.StatusForbidden, platform.CodeForbidden},
		"an unknown result":  {root, map[string]any{"result": "unknown"}, http.StatusBadRequest, platform.CodeInvalidRequest},
		"a malformed before": {root, map[string]any{"before": "yesterday"}, http.StatusBadRequest, ""},
		"an unknown field":   {root, map[string]any{"older": "1d"}, http.StatusBadRequest, ""},
	} {
		e := decodeBody[OperationError](t, identityRequest(t, http.MethodPost, prune, tc.body, tc.header), tc.status)
		if e.Error == "" || e.Code != string(tc.code) {
			t.Errorf("%s: %+v", name, e)
		}
	}
	if none := decodeBody[RunPruneResult](t, identityRequest(t, http.MethodPost, prune, map[string]any{"before": time.Now().Add(-time.Hour).Format(time.RFC3339)}, root), http.StatusOK); none.Runs != 0 {
		t.Fatalf("pruning older runs = %+v", none)
	}

	dry := decodeBody[RunPruneResult](t, identityRequest(t, http.MethodPost, prune, map[string]any{"dry_run": true}, root), http.StatusOK)
	if dry.Runs != 2 || dry.Bytes <= 0 || !dry.DryRun {
		t.Fatalf("dry run = %+v", dry)
	}
	if got := decodeBody[RunPage](t, identityRequest(t, http.MethodGet, srv.URL+"/api/v1/runs", nil, root), http.StatusOK); len(got.Runs) != 3 {
		t.Fatalf("a dry run left %d runs, want 3", len(got.Runs))
	}
	pruned := decodeBody[RunPruneResult](t, identityRequest(t, http.MethodPost, prune, map[string]any{"before": time.Now().Add(time.Hour).Format(time.RFC3339)}, root), http.StatusOK)
	if pruned.Runs != dry.Runs || pruned.Bytes != dry.Bytes || pruned.DryRun {
		t.Fatalf("prune = %+v, dry run said %+v", pruned, dry)
	}
	got := decodeBody[RunPage](t, identityRequest(t, http.MethodGet, srv.URL+"/api/v1/runs", nil, root), http.StatusOK)
	if len(got.Runs) != 1 || got.Runs[0].ID != runs[0].ID || !got.Runs[0].Kept {
		t.Fatalf("runs after the prune = %+v, want only the kept run", got.Runs)
	}
}

// TestStorageReportsRunCountsAndThePolicy: GET /storage counts runs and
// kept runs, names the oldest and states the retention policy; a server
// without a workspace is 503.
func TestStorageReportsRunCountsAndThePolicy(t *testing.T) {
	srv, as := retentionServer(t, platform.RetentionPolicy{Runs: 500, Age: 36 * time.Hour})
	alice := as("alice", "engineers")
	storage := srv.URL + "/api/v1/storage"
	empty := decodeBody[StorageUsage](t, identityRequest(t, http.MethodGet, storage, nil, alice), http.StatusOK)
	if empty.Runs != 0 || empty.OldestRun != nil || empty.Retention != (RetentionPolicy{Runs: 500, AgeSeconds: 36 * 3600}) {
		t.Fatalf("empty storage = %+v", empty)
	}
	first, second := submitFinished(t, srv, alice), submitFinished(t, srv, alice)
	decodeBody[Run](t, identityRequest(t, http.MethodPut, srv.URL+"/api/v1/runs/"+second.ID+"/keep", nil, alice), http.StatusOK)
	usage := decodeBody[StorageUsage](t, identityRequest(t, http.MethodGet, storage, nil, alice), http.StatusOK)
	if usage.Runs != 2 || usage.KeptRuns != 1 || usage.OldestRun == nil || usage.Used <= 0 || usage.Limit <= 0 || usage.OldestRun.After(first.Created.Add(time.Minute)) {
		t.Fatalf("storage = %+v, first run created %s", usage, first.Created)
	}
	raw := decodeBody[map[string]any](t, identityRequest(t, http.MethodGet, storage, nil, alice), http.StatusOK)
	for _, key := range []string{"limit", "run_limit", "used", "runs", "kept_runs", "oldest_run", "retention"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("storage lacks %q: %+v", key, raw)
		}
	}

	_, ephemeral := newServer(t)
	if e := decodeBody[OperationError](t, get(t, ephemeral.URL+"/api/v1/storage"), http.StatusServiceUnavailable); e.Code != string(platform.CodeUnavailable) {
		t.Fatalf("storage without a workspace = %+v", e)
	}
}
