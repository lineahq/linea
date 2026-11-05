package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/platform"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

var retentionToolText = regexp.MustCompile(`^(Run kept|Run no longer kept|Run prune: [0-9]+ runs (would be removed|removed), [0-9]+ bytes (would be freed|freed)|Run storage: [0-9]+ runs, [0-9]+ kept; [0-9]+ bytes used of [0-9]+)\.$`)

// The retention tools apply the HTTP rules: an owner keeps and unkeeps a
// run, another engineer is forbidden, only an admin prunes, a dry run counts
// what the prune then removes, and kept runs stay and show in the run list.
func TestRetentionToolsApplyTheHTTPRules(t *testing.T) {
	access, err := platform.NewPolicy([]platform.Binding{
		{Subject: platform.Subject{Principal: "user:root"}, Role: "admin", Scope: platform.ScopeAll},
		{Subject: platform.Subject{Principal: "user:alice"}, Role: "engineer", Scope: platform.ScopeAll},
		{Subject: platform.Subject{Principal: "user:bob"}, Role: "engineer", Scope: platform.ScopeAll},
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := platform.Open(memory.Open(memory.NewData(), artifact.Limits{}), platform.Options{Access: access, Retention: platform.RetentionPolicy{Runs: 200, Age: 24 * time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	server := httptest.NewServer(New(Options{Platform: service, Authenticator: tokenAuthenticator{"t-alice": "alice", "t-bob": "bob", "t-root": "root"}}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	connect := func(token string) *sdk.ClientSession {
		t.Helper()
		client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
		session, err := client.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: server.URL, HTTPClient: &http.Client{Transport: bearerTransport(token)}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = session.Close() })
		return session
	}
	alice, bob, root := connect("t-alice"), connect("t-bob"), connect("t-root")

	var ids []string
	for range 3 {
		r, out := callTool(t, ctx, alice, "linea_run", runArgs(platform.ModeQuick))
		ids = append(ids, runID(t, r, out))
	}
	listed := func() []any {
		t.Helper()
		_, out := callTool(t, ctx, alice, "linea_list_runs", map[string]any{})
		return out["runs"].([]any)
	}

	for _, name := range []string{"linea_keep_run", "linea_unkeep_run"} {
		r, out := callTool(t, ctx, bob, name, map[string]any{"run_id": ids[0]})
		assertToolError(t, r, out, platform.CodeForbidden, false)
	}
	r, out := callTool(t, ctx, alice, "linea_keep_run", map[string]any{"run_id": "run-unknown"})
	assertToolError(t, r, out, platform.CodeNotFound, false)
	if r, out = callTool(t, ctx, alice, "linea_keep_run", map[string]any{"run_id": ids[0]}); r.IsError || out["kept"] != true || out["run_id"] != ids[0] || textOf(t, r) != "Run kept." {
		t.Fatalf("keep = %s, %+v", textOf(t, r), out)
	}
	if _, got := callTool(t, ctx, alice, "linea_get_run", map[string]any{"run_id": ids[0]}); got["kept"] != true {
		t.Fatalf("get run = %+v", got)
	}
	kept := 0
	for _, run := range listed() {
		if run.(map[string]any)["kept"] == true {
			kept++
		}
	}
	if kept != 1 {
		t.Fatalf("the run list shows %d kept runs, want 1", kept)
	}

	_, usage := callTool(t, ctx, alice, "linea_get_storage_usage", map[string]any{})
	policy, _ := usage["retention"].(map[string]any)
	if usage["runs"] != float64(3) || usage["kept_runs"] != float64(1) || usage["oldest_run"] == nil || policy["runs"] != float64(200) || policy["age_seconds"] != float64(24*3600) {
		t.Fatalf("storage usage = %+v", usage)
	}

	r, out = callTool(t, ctx, alice, "linea_prune_runs", map[string]any{"dry_run": true})
	assertToolError(t, r, out, platform.CodeForbidden, false)
	r, out = callTool(t, ctx, root, "linea_prune_runs", map[string]any{"before": "yesterday"})
	assertToolError(t, r, out, platform.CodeInvalidRequest, false)
	r, out = callTool(t, ctx, root, "linea_prune_runs", map[string]any{"result": "unknown"})
	assertToolError(t, r, out, platform.CodeInvalidRequest, false)

	r, dry := callTool(t, ctx, root, "linea_prune_runs", map[string]any{"dry_run": true})
	if r.IsError || dry["runs"] != float64(2) || dry["dry_run"] != true || dry["bytes"].(float64) <= 0 || len(listed()) != 3 {
		t.Fatalf("dry run = %+v", dry)
	}
	r, done := callTool(t, ctx, root, "linea_prune_runs", map[string]any{"before": time.Now().Add(time.Hour).Format(time.RFC3339)})
	if r.IsError || done["runs"] != dry["runs"] || done["bytes"] != dry["bytes"] || done["dry_run"] != false {
		t.Fatalf("prune = %+v, dry run said %+v", done, dry)
	}
	if left := listed(); len(left) != 1 || left[0].(map[string]any)["run_id"] != ids[0] {
		t.Fatalf("runs after the prune = %+v", left)
	}

	if r, out = callTool(t, ctx, root, "linea_unkeep_run", map[string]any{"run_id": ids[0]}); r.IsError || out["kept"] != false || textOf(t, r) != "Run no longer kept." {
		t.Fatalf("an admin unkeeps another's run = %s, %+v", textOf(t, r), out)
	}
}
