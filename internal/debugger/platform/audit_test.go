package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/types"
	"slices"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

// TestEveryOperationRecordsAudit is the audit coverage gate:
// each operation's first statement defers its audit record under its own
// name, so no operation returns unrecorded.
func TestEveryOperationRecordsAudit(t *testing.T) {
	count := 0
	for _, f := range parseDir(t, ".") {
		for _, d := range f.file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || !isOperation(fn) {
				continue
			}
			count++
			want := fmt.Sprintf("defer %s.audit(&caller, %q, &err)()", fn.Recv.List[0].Names[0].Name, fn.Name.Name)
			if len(fn.Body.List) == 0 || statementText(fn.Body.List[0]) != want {
				t.Errorf("%s: the first statement must be %s", fn.Name.Name, want)
			}
		}
	}
	if count < 100 {
		t.Fatalf("found only %d operations", count)
	}
}

func statementText(n ast.Stmt) string {
	if d, ok := n.(*ast.DeferStmt); ok {
		return "defer " + types.ExprString(d.Call)
	}
	return ""
}

// TestAuditRecordsDecisionsWithoutContent is the audit gate: allowed and
// denied operations are recorded with principal, client, request, operation,
// permission, scope, resources and outcome, survive a restart, and no event
// holds configuration content, environment values or saved content.
func TestAuditRecordsDecisionsWithoutContent(t *testing.T) {
	const secret = "SECRET-7f3a"
	data := memory.NewData()
	access, err := NewPolicy([]Binding{
		{Subject: Subject{Principal: testCaller.Principal.ID}, Role: "admin", Scope: ScopeAll},
		{Subject: Subject{Group: "readers"}, Role: "viewer", Scope: ScopeAll},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUDIT_TOKEN", secret+"-env")
	opts := Options{Access: access, Prepare: unprepared, Environment: config.EnvironmentPolicy{Host: []string{"AUDIT_TOKEN"}},
		Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
			return report(req, runresult.StatusCompleted), nil
		}}
	s := openPlatform(t, data, opts)
	ctx := context.Background()
	admin := testCaller
	admin.RequestID = "req-1"
	vera := Caller{Principal: Principal{ID: "user:vera", Kind: PrincipalUser, Name: "Vera", Groups: []string{"readers"}}, Client: "http", RequestID: "req-2"}
	mallory := Caller{Principal: Principal{ID: "user:mallory", Kind: PrincipalUser}, Client: "mcp"}

	run, err := s.SubmitRun(ctx, admin, RunRequest{Mode: ModeQuick, Config: Source{Text: "# " + secret + "-config"},
		Quick: &QuickInput{Fixture: Source{Text: `{"` + secret + `-fixture": 1}`}, Setup: "env: {TOKEN: " + secret + "-setup}"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetRun(ctx, vera, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRun(ctx, vera, run.ID); codeOf(err) != CodeForbidden {
		t.Fatalf("viewer delete = %v", err)
	}
	if _, err := s.GetRun(ctx, admin, "run-"+secret); codeOf(err) != CodeNotFound {
		t.Fatalf("unknown run = %v", err)
	}
	if _, err := allRuns(ctx, s, mallory, RunFilter{}); codeOf(err) != CodeForbidden {
		t.Fatalf("unbound list = %v", err)
	}
	saved, err := s.SaveArtifact(ctx, admin, SaveArtifactRequest{WorkspaceID: workspaceID(t, s), Kind: artifact.Scenario, Label: secret + "-label", Content: []byte(secret + "-content")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListAuditEvents(ctx, vera, AuditFilter{}); codeOf(err) != CodeForbidden {
		t.Fatalf("viewer audit = %v", err)
	}
	s.Close()
	s = openPlatform(t, data, opts)

	page, err := s.ListAuditEvents(ctx, admin, AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	type summary struct {
		principal, operation, permission, scope string
		outcome                                 AuditOutcome
		code                                    Code
		resources                               []string
	}
	var got []summary
	for _, e := range page.Events {
		got = append(got, summary{e.Principal, e.Operation, string(e.Permission), e.Scope, e.Outcome, e.Code, e.Resources})
	}
	want := []summary{
		{"user:vera", "ListAuditEvents", "audit.read", "*", AuditDenied, "", nil},
		{LocalOwner, "SaveArtifact", "artifact.write", "project:default", AuditAllowed, "", []string{saved.Artifact.ID}},
		{LocalOwner, "GetWorkspaceInfo", "artifact.read", "", AuditAllowed, "", nil},
		{"user:mallory", "ListRuns", "run.read", "*", AuditDenied, "", nil},
		{LocalOwner, "GetRun", "run.read", "", AuditAllowed, CodeNotFound, nil},
		// Refused at entry: the viewer holds run.delete in no project.
		{"user:vera", "DeleteRun", "run.delete", "*", AuditDenied, "", nil},
		{"user:vera", "GetRun", "run.read", "project:default", AuditAllowed, "", []string{run.ID}},
		{LocalOwner, "SubmitRun", "run.submit", "project:default", AuditAllowed, "", []string{run.ID}},
	}
	// Opening the workspace records nothing; the events are the ones above.
	if len(got) != len(want) {
		t.Fatalf("events = %+v", got)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.principal != w.principal || g.operation != w.operation || g.permission != w.permission || g.scope != w.scope ||
			g.outcome != w.outcome || g.code != w.code || !slices.Equal(g.resources, w.resources) {
			t.Errorf("event %d = %+v, want %+v", i, g, w)
		}
	}
	first := page.Events[len(page.Events)-1]
	if first.RequestID != "req-1" || first.Client != "test" || first.PrincipalKind != PrincipalLocal || first.Time.IsZero() {
		t.Errorf("caller fields = %+v", first)
	}
	raw, _ := json.Marshal(page)
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "Vera") {
		t.Errorf("audit events hold request content: %s", raw)
	}

	next, err := s.ListAuditEvents(ctx, admin, AuditFilter{Limit: 3})
	if err != nil || len(next.Events) != 3 || next.NextCursor == "" || next.Events[0].Operation != "ListAuditEvents" {
		t.Fatalf("first page = %+v, %v", next, err)
	}
	rest, err := s.ListAuditEvents(ctx, admin, AuditFilter{Limit: 100, Cursor: next.NextCursor})
	if err != nil || len(rest.Events) != len(want)-2 || rest.NextCursor != "" || rest.Events[0].Seq != next.Events[2].Seq-1 {
		t.Fatalf("second page = %+v, %v", rest, err)
	}
	denied, err := s.ListAuditEvents(ctx, admin, AuditFilter{Outcome: AuditDenied, Principal: "user:vera"})
	if err != nil || len(denied.Events) != 2 {
		t.Fatalf("denied for vera = %+v, %v", denied, err)
	}
	for _, f := range []AuditFilter{{Outcome: "maybe"}, {Limit: 501}, {Cursor: "x"}} {
		if _, err := s.ListAuditEvents(ctx, admin, f); codeOf(err) != CodeInvalidRequest {
			t.Errorf("%+v = %v", f, err)
		}
	}
}

// TestNestedOperationsShareOneRecord keeps one event per adapter call when an
// operation calls another with its caller, and a fresh one once it returned.
func TestNestedOperationsShareOneRecord(t *testing.T) {
	s := New(Options{})
	t.Cleanup(s.Close)
	outer := func(ctx context.Context, caller Caller) (err error) {
		defer s.audit(&caller, "Outer", &err)()
		_, err = allRuns(ctx, s, caller, RunFilter{})
		return err
	}
	if err := outer(context.Background(), testCaller); err != nil {
		t.Fatal(err)
	}
	caller := testCaller
	done := s.audit(&caller, "Finished", new(error))
	done()
	if _, err := allRuns(context.Background(), s, caller, RunFilter{}); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListAuditEvents(context.Background(), testCaller, AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var ops []string
	for _, e := range page.Events {
		ops = append(ops, e.Operation)
	}
	if !slices.Equal(ops, []string{"ListRuns", "Finished", "Outer"}) {
		t.Fatalf("operations = %v", ops)
	}
	if page.Events[2].Permission != "run.read" {
		t.Errorf("outer record = %+v", page.Events[2])
	}
}
