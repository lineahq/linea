package platform

import (
	"context"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

// TestProjectScopesFilterAndRefuse binds one engineer to the project every
// resource joins and another to a different project: the first sees and acts
// on everything, the second sees empty lists and is refused by ID.
func TestProjectScopesFilterAndRefuse(t *testing.T) {
	data := memory.NewData()
	project := func(id string) Scope { return Scope{Kind: authz.ScopeProject, ID: id} }
	access, err := NewPolicy([]Binding{
		{Subject: Subject{Principal: testCaller.Principal.ID}, Role: "admin", Scope: ScopeAll},
		{Subject: Subject{Principal: "user:alice"}, Role: "engineer", Scope: project(artifact.DefaultProject)},
		{Subject: Subject{Principal: "user:bob"}, Role: "engineer", Scope: project("other")},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := openPlatform(t, data, Options{Access: access, Prepare: unprepared, Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
		return report(req, runresult.StatusCompleted), nil
	}})
	ctx := context.Background()
	run := waitRun(t, submit(t, s, request("quick")))
	saved, err := s.SaveArtifact(ctx, testCaller, SaveArtifactRequest{WorkspaceID: workspaceID(t, s), Kind: artifact.Scenario, Label: "a", Content: []byte("x")})
	if err != nil {
		t.Fatal(err)
	}
	if run.Project != artifact.DefaultProject || saved.Artifact.Project != artifact.DefaultProject {
		t.Fatalf("projects: run %q, artifact %q", run.Project, saved.Artifact.Project)
	}
	user := func(id string) Caller {
		return Caller{Principal: Principal{ID: id, Kind: PrincipalUser}, Client: "test"}
	}
	alice, bob := user("user:alice"), user("user:bob")

	if runs, err := allRuns(ctx, s, alice, RunFilter{}); err != nil || len(runs) != 1 {
		t.Errorf("alice runs = %d, %v", len(runs), err)
	}
	if _, err := s.GetRun(ctx, alice, run.ID); err != nil {
		t.Errorf("alice get run: %v", err)
	}
	if page, err := s.ListArtifacts(ctx, alice, ListArtifactsRequest{}); err != nil || len(page.Items) != 1 {
		t.Errorf("alice artifacts = %+v, %v", page, err)
	}

	if runs, err := allRuns(ctx, s, bob, RunFilter{}); err != nil || len(runs) != 0 {
		t.Errorf("bob runs = %d, %v", len(runs), err)
	}
	if page, err := s.ListArtifacts(ctx, bob, ListArtifactsRequest{}); err != nil || len(page.Items) != 0 {
		t.Errorf("bob artifacts = %+v, %v", page, err)
	}
	if suites, err := s.ListSuites(ctx, bob); err != nil || len(suites) != 0 {
		t.Errorf("bob suites = %d, %v", len(suites), err)
	}
	refused := map[string]error{}
	_, refused["get run"] = s.GetRun(ctx, bob, run.ID)
	_, refused["run bundle"] = s.GetRunBundle(ctx, bob, run.ID)
	_, refused["run inputs"] = s.GetRunInputs(ctx, bob, run.ID)
	_, refused["evaluations"] = s.ListEvaluations(ctx, bob, run.ID)
	refused["delete run"] = s.DeleteRun(ctx, bob, run.ID)
	_, refused["get artifact"] = s.GetArtifact(ctx, bob, saved.Artifact.ID)
	_, refused["artifact revision"] = s.GetArtifactRevision(ctx, bob, saved.Artifact.ID, saved.Revision.Ref.RevisionID)
	_, refused["update artifact"] = s.SaveArtifact(ctx, bob, SaveArtifactRequest{WorkspaceID: workspaceID(t, s), ArtifactID: saved.Artifact.ID,
		ExpectedGeneration: saved.Artifact.Generation, ExpectedHead: saved.Artifact.Head, Kind: artifact.Scenario, Content: []byte("y")})
	_, refused["create artifact"] = s.SaveArtifact(ctx, bob, SaveArtifactRequest{WorkspaceID: workspaceID(t, s), Kind: artifact.Scenario, Content: []byte("y")})
	_, refused["submit run"] = s.submit(ctx, bob, ModeQuick, request("quick"), time.Time{})
	for name, err := range refused {
		if codeOf(err) != CodeForbidden {
			t.Errorf("bob %s: %v, want forbidden", name, err)
		}
	}
	if _, err := s.GetRun(ctx, alice, run.ID); err != nil {
		t.Errorf("refusals changed the run: %v", err)
	}

	// A caller who may read every project sees all of them without a
	// project named, and only the named project's work with one.
	for _, c := range []struct {
		project string
		want    int
	}{{"", 1}, {artifact.DefaultProject, 1}, {"other", 0}} {
		admin := testCaller
		admin.Project = c.project
		if runs, err := allRuns(ctx, s, admin, RunFilter{}); err != nil || len(runs) != c.want {
			t.Errorf("admin in %q: runs = %d, %v; want %d", c.project, len(runs), err, c.want)
		}
		if page, err := s.ListArtifacts(ctx, admin, ListArtifactsRequest{}); err != nil || len(page.Items) != c.want {
			t.Errorf("admin in %q: artifacts = %+v, %v; want %d", c.project, page, err, c.want)
		}
		if suites, err := s.ListSuites(ctx, admin); err != nil || len(suites) != 0 {
			t.Errorf("admin in %q: suites = %d, %v", c.project, len(suites), err)
		}
		if page, err := s.ListCaptureSessions(ctx, admin, CollectorPageRequest{}); err != nil || len(page.Items) != 0 {
			t.Errorf("admin in %q: captures = %+v, %v", c.project, page, err)
		}
	}
	// Naming a project the caller holds nothing in is refused, not empty.
	bob.Project = artifact.DefaultProject
	if _, err := allRuns(ctx, s, bob, RunFilter{}); codeOf(err) != CodeForbidden {
		t.Errorf("bob runs in default: %v, want forbidden", err)
	}
}

func workspaceID(t *testing.T, s *Service) string {
	t.Helper()
	info, err := s.GetWorkspaceInfo(context.Background(), testCaller)
	if err != nil {
		t.Fatal(err)
	}
	return info.ID
}
