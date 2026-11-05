package platform

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

// sharingPlatform opens a platform with projects payments and checkout,
// alice an engineer in payments and bob one in checkout.
func sharingPlatform(t *testing.T) (s *Service, alice, bob Caller) {
	t.Helper()
	project := func(id string) Scope { return Scope{Kind: authz.ScopeProject, ID: id} }
	access, err := NewPolicy([]Binding{
		{Subject: Subject{Principal: testCaller.Principal.ID}, Role: "admin", Scope: ScopeAll},
		{Subject: Subject{Principal: "user:alice"}, Role: "engineer", Scope: project("payments")},
		{Subject: Subject{Principal: "user:bob"}, Role: "engineer", Scope: project("checkout")},
	})
	if err != nil {
		t.Fatal(err)
	}
	s = openPlatform(t, memory.NewData(), Options{Access: access, Prepare: unprepared, Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
		return report(req, runresult.StatusCompleted), nil
	}})
	for _, id := range []string{"payments", "checkout"} {
		if _, err := s.CreateProject(context.Background(), testCaller, CreateProjectRequest{Revision: projectRevision(t, s), ID: id, Name: id}); err != nil {
			t.Fatal(err)
		}
	}
	user := func(id, project string) Caller {
		return Caller{Principal: Principal{ID: id, Kind: PrincipalUser}, Client: "test", Project: project}
	}
	return s, user("user:alice", "payments"), user("user:bob", "checkout")
}

func projectRevision(t *testing.T, s *Service) uint64 {
	t.Helper()
	l, err := s.ListProjects(context.Background(), testCaller)
	if err != nil {
		t.Fatal(err)
	}
	return l.Revision
}

// TestLibraryItemsReachTheirAudienceOnly publishes a configuration to the
// library and shares it with payments: alice lists, reads, runs and copies
// it there, bob in checkout sees nothing and is refused.
func TestLibraryItemsReachTheirAudienceOnly(t *testing.T) {
	ctx := context.Background()
	s, alice, bob := sharingPlatform(t)
	config, input, _ := sampleSources(t)
	admin := testCaller
	admin.Project = artifact.LibraryProject
	saved, err := s.SaveArtifact(ctx, admin, SaveArtifactRequest{WorkspaceID: workspaceID(t, s), Kind: artifact.Configuration, Label: "shared", Content: []byte(config)})
	if err != nil {
		t.Fatal(err)
	}
	if saved.Artifact.Project != artifact.LibraryProject {
		t.Fatalf("published into %q", saved.Artifact.Project)
	}
	if _, err := s.SetLibraryAudience(ctx, testCaller, SetLibraryAudienceRequest{Revision: projectRevision(t, s), ArtifactID: saved.Artifact.ID, Projects: []string{"payments"}}); err != nil {
		t.Fatal(err)
	}

	page, err := s.ListLibrary(ctx, alice, ListLibraryRequest{})
	if err != nil || len(page.Items) != 1 || page.Items[0].Artifact.ID != saved.Artifact.ID {
		t.Fatalf("alice library = %+v, %v", page, err)
	}
	if item := page.Items[0]; item.SharedBy != "" || !slices.Equal(item.Audience, []string{"payments"}) {
		t.Errorf("alice sees item %+v; want her project and no sharer", item)
	}
	if _, err := s.GetArtifact(ctx, alice, saved.Artifact.ID); err != nil {
		t.Errorf("alice get: %v", err)
	}
	if page, err := s.ListLibrary(ctx, bob, ListLibraryRequest{}); err != nil || len(page.Items) != 0 {
		t.Errorf("bob library = %+v, %v", page, err)
	}
	if _, err := s.GetArtifact(ctx, bob, saved.Artifact.ID); codeOf(err) != CodeForbidden {
		t.Errorf("bob get: %v, want forbidden", err)
	}

	ref := &ArtifactRef{ArtifactID: saved.Artifact.ID, RevisionID: saved.Revision.Ref.RevisionID}
	quick := RunRequest{Mode: ModeQuick, Config: Source{Artifact: ref}, Quick: &QuickInput{Fixture: Source{Text: input}}}
	if _, err := s.SubmitRun(ctx, alice, quick); err != nil {
		t.Errorf("alice runs the shared configuration: %v", err)
	}
	other := testCaller
	other.Project = artifact.DefaultProject
	if _, err := s.SubmitRun(ctx, other, quick); codeOf(err) != CodeInvalidRequest {
		t.Errorf("a run in the default project: %v, want invalid request", err)
	}

	copied, err := s.CopyArtifact(ctx, alice, CopyArtifactRequest{ArtifactID: saved.Artifact.ID, Label: "mine"})
	if err != nil {
		t.Fatal(err)
	}
	if copied.Artifact.Project != "payments" || copied.Artifact.Label != "mine.yaml" || !reflect.DeepEqual(copied.Revision.Origin, saved.Revision.Origin) {
		t.Errorf("copy = %+v %+v", copied.Artifact, copied.Revision.Origin)
	}
	if _, err := s.CopyArtifact(ctx, bob, CopyArtifactRequest{ArtifactID: saved.Artifact.ID}); codeOf(err) != CodeForbidden {
		t.Errorf("bob copy: %v, want forbidden", err)
	}
	if _, err := s.SetLibraryAudience(ctx, alice, SetLibraryAudienceRequest{Revision: projectRevision(t, s), ArtifactID: saved.Artifact.ID, Projects: []string{"*"}}); codeOf(err) != CodeForbidden {
		t.Errorf("alice shares: %v, want forbidden", err)
	}
}

// TestSharingListsAreValidated refuses lists that mix "*" with projects,
// name the library or an unknown project, or repeat one.
func TestSharingListsAreValidated(t *testing.T) {
	ctx := context.Background()
	s, _, _ := sharingPlatform(t)
	for name, list := range map[string][]string{
		"star and project": {"*", "payments"},
		"library":          {artifact.LibraryProject},
		"unknown":          {"nowhere"},
		"twice":            {"payments", "payments"},
	} {
		_, err := s.SetRuntimeAvailability(ctx, testCaller, SetRuntimeAvailabilityRequest{Revision: projectRevision(t, s), Runtime: BuiltInRuntime, Projects: list})
		if codeOf(err) != CodeInvalidRequest {
			t.Errorf("%s: %v, want invalid request", name, err)
		}
	}
}

// TestRuntimeAvailabilityLimitsProjects makes the built-in runtime
// available to payments only: checkout cannot run on it until an empty
// list restores it to every project.
func TestRuntimeAvailabilityLimitsProjects(t *testing.T) {
	ctx := context.Background()
	s, alice, bob := sharingPlatform(t)
	set := func(projects ...string) {
		t.Helper()
		if _, err := s.SetRuntimeAvailability(ctx, testCaller, SetRuntimeAvailabilityRequest{Revision: projectRevision(t, s), Runtime: BuiltInRuntime, Projects: projects}); err != nil {
			t.Fatal(err)
		}
	}
	set("payments")
	if _, err := s.submit(ctx, alice, ModeQuick, request("quick"), time.Time{}); err != nil {
		t.Errorf("alice run: %v", err)
	}
	if _, err := s.submit(ctx, bob, ModeQuick, request("quick"), time.Time{}); codeOf(err) != CodeForbidden {
		t.Errorf("bob run: %v, want forbidden", err)
	}
	set()
	if _, err := s.submit(ctx, bob, ModeQuick, request("quick"), time.Time{}); err != nil {
		t.Errorf("bob run after an empty list: %v", err)
	}
}

// TestRuntimeAvailabilityCoversEveryOperation refuses inspection, OTTL
// previews, upgrade checks and suites on a runtime the caller's project
// cannot use, as it refuses runs, and lets the project it is available to
// through.
func TestRuntimeAvailabilityCoversEveryOperation(t *testing.T) {
	ctx := context.Background()
	s, alice, bob := sharingPlatform(t)
	if _, err := s.SetRuntimeAvailability(ctx, testCaller, SetRuntimeAvailabilityRequest{Revision: projectRevision(t, s), Runtime: BuiltInRuntime, Projects: []string{"payments"}}); err != nil {
		t.Fatal(err)
	}
	config := Source{Text: "config"}
	for name, op := range map[string]func(Caller) error{
		"inspect": func(c Caller) error {
			_, err := s.InspectConfiguration(ctx, c, InspectRequest{Config: config})
			return err
		},
		"preview": func(c Caller) error {
			_, err := s.PreviewTransformation(ctx, c, PreviewRequest{Config: config})
			return err
		},
		"upgrade check": func(c Caller) error {
			_, err := s.CheckUpgrade(ctx, c, UpgradeCheckRequest{To: "next", Config: config})
			return err
		},
		"suite": func(c Caller) error {
			_, err := s.SubmitSuite(ctx, c, SubmitSuiteRequest{Config: config, Members: []ScenarioInput{{Scenario: Source{Text: "scenario"}}}})
			return err
		},
	} {
		if err := op(bob); ErrorCode(err) != CodeForbidden || !strings.Contains(err.Error(), "not available to project checkout") {
			t.Errorf("%s by bob: %v, want forbidden", name, err)
		}
		if err := op(alice); ErrorCode(err) == CodeForbidden {
			t.Errorf("%s by alice: %v, want it past the availability check", name, err)
		}
	}
}
