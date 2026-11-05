package platform

import (
	"archive/tar"
	"bytes"
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

// serverBuildManifest builds example.com/proc from a local directory.
const serverBuildManifest = "dist:\n  otelcol_version: 0.162.0\nprocessors:\n  - gomod: example.com/proc v0.0.0\n    path: ./proc\n"

var (
	buildAlice = Caller{Principal: Principal{ID: "user:alice", Kind: PrincipalUser, Groups: []string{"engineers"}}, Client: "http"}
	buildBob   = Caller{Principal: Principal{ID: "user:bob", Kind: PrincipalUser, Groups: []string{"engineers"}}, Client: "http"}
	buildVera  = Caller{Principal: Principal{ID: "user:vera", Kind: PrincipalUser, Groups: []string{"readers"}}, Client: "http"}
)

// serverBuildPlatform is a team server without a runtime store: builds go
// to the repository's build queue. testCaller is its admin.
func serverBuildPlatform(t *testing.T) *Service {
	t.Helper()
	access, err := NewPolicy([]Binding{
		{Subject: Subject{Principal: testCaller.Principal.ID}, Role: "admin", Scope: ScopeAll},
		{Subject: Subject{Group: "engineers"}, Role: "engineer", Scope: ScopeAll},
		{Subject: Subject{Group: "readers"}, Role: "viewer", Scope: ScopeAll},
	})
	if err != nil {
		t.Fatal(err)
	}
	return openPlatform(t, memory.NewData(), Options{Access: access})
}

// moduleArchive is a tar of the files in name order, so equal files make
// equal archives; a name ending in "@" is a symbolic link.
func moduleArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		content := files[name]
		h := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if link, ok := strings.CutSuffix(name, "@"); ok {
			h = &tar.Header{Name: link, Linkname: content, Typeflag: tar.TypeSymlink}
		}
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := w.Write([]byte(content)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func procArchive(t *testing.T, extra string) []byte {
	return moduleArchive(t, map[string]string{"go.mod": "module example.com/proc\n", "proc.go": "package proc\n" + extra})
}

func serverBuildRequest(name string, archive []byte) BuildRuntimeRequest {
	return BuildRuntimeRequest{Name: name, Manifest: Source{Text: serverBuildManifest},
		Sources: []SourceOverride{{Module: "example.com/proc", Archive: archive}}}
}

// TestServerBuildsTakeUploadedSourcesOnly queues a build from uploaded
// module archives and refuses every other source, an unsafe archive and a
// manifest directory the upload lacks, before admitting anything.
func TestServerBuildsTakeUploadedSourcesOnly(t *testing.T) {
	ctx := context.Background()
	s := serverBuildPlatform(t)
	build, err := s.BuildRuntime(ctx, buildAlice, serverBuildRequest("edge", procArchive(t, "")))
	if err != nil {
		t.Fatal(err)
	}
	if build.State != BuildQueued || build.Reused || build.Caller.Principal != "user:alice" || build.CollectorVersion != "0.162.0" || !build.Verified {
		t.Fatalf("build = %+v", build)
	}
	_, err = s.BuildRuntime(ctx, buildVera, serverBuildRequest("edge", procArchive(t, "")))
	wantCode(t, err, CodeForbidden)
	for name, req := range map[string]BuildRuntimeRequest{
		"no source":       {Name: "edge", Manifest: Source{Text: serverBuildManifest}},
		"a directory":     {Name: "edge", Manifest: Source{Text: serverBuildManifest}, Sources: []SourceOverride{{Module: "example.com/proc", Dir: "proc"}}},
		"a git source":    {Name: "edge", Manifest: Source{Text: serverBuildManifest}, Sources: []SourceOverride{{Module: "example.com/proc", Git: "https://example.com/proc.git"}}},
		"a symbolic link": serverBuildRequest("edge", moduleArchive(t, map[string]string{"go.mod": "module example.com/proc\n", "link@": "/etc/passwd"})),
		"an escape":       serverBuildRequest("edge", moduleArchive(t, map[string]string{"go.mod": "module example.com/proc\n", "../x.go": "package x\n"})),
		"another module":  serverBuildRequest("edge", moduleArchive(t, map[string]string{"go.mod": "module example.com/other\n"})),
		"no go.mod":       serverBuildRequest("edge", moduleArchive(t, map[string]string{"proc.go": "package proc\n"})),
		"not a tar":       serverBuildRequest("edge", []byte("not a tar archive, just some bytes that are long enough")),
	} {
		_, err := s.BuildRuntime(ctx, buildAlice, req)
		if codeOf(err) != CodeValidationFailed {
			t.Errorf("%s: %v, want validation_failed", name, err)
		}
	}
	for name, req := range map[string]BuildRuntimeRequest{
		"a Go image":      {Name: "edge", Manifest: Source{Text: serverBuildManifest}, GoImage: "golang:latest"},
		"offline":         {Name: "edge", Manifest: Source{Text: serverBuildManifest}, Offline: true},
		"a manifest file": {Name: "edge", Manifest: Source{Path: "manifest.yaml"}},
		"a bad name":      serverBuildRequest("Edge", procArchive(t, "")),
	} {
		if _, err := s.BuildRuntime(ctx, buildAlice, req); codeOf(err) != CodeInvalidRequest {
			t.Errorf("%s: %v, want invalid_request", name, err)
		}
	}
	page, err := s.ListRuntimeBuilds(ctx, buildAlice, BuildListRequest{})
	if err != nil || len(page.Builds) != 1 || page.Builds[0].ID != build.ID {
		t.Fatalf("list = %+v, %v", page, err)
	}
}

// TestServerBuildsAreVisibleToOwnersAndManagers: a build is its owner's;
// another engineer neither reads, lists nor cancels it, an admin does all
// three. Unfinished builds are bounded per principal.
func TestServerBuildsAreVisibleToOwnersAndManagers(t *testing.T) {
	ctx := context.Background()
	s := serverBuildPlatform(t)
	first, err := s.BuildRuntime(ctx, buildAlice, serverBuildRequest("edge", procArchive(t, "")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BuildRuntime(ctx, buildAlice, serverBuildRequest("edge", procArchive(t, "// 2"))); err != nil {
		t.Fatal(err)
	}
	_, err = s.BuildRuntime(ctx, buildAlice, serverBuildRequest("edge", procArchive(t, "// 3")))
	wantCode(t, err, CodeResourceExhausted)

	_, err = s.GetRuntimeBuild(ctx, buildBob, first.ID, 0)
	wantCode(t, err, CodeNotFound)
	_, err = s.CancelRuntimeBuild(ctx, buildBob, first.ID)
	wantCode(t, err, CodeNotFound)
	for _, want := range []struct {
		caller Caller
		builds int
	}{{buildAlice, 2}, {buildBob, 0}, {testCaller, 2}} {
		page, err := s.ListRuntimeBuilds(ctx, want.caller, BuildListRequest{})
		if err != nil || len(page.Builds) != want.builds {
			t.Errorf("%s lists %+v, %v", want.caller.Principal.ID, page, err)
		}
	}
	page, err := s.ListRuntimeBuilds(ctx, testCaller, BuildListRequest{Limit: 1})
	if err != nil || len(page.Builds) != 1 || page.NextCursor == "" {
		t.Fatalf("first page = %+v, %v", page, err)
	}
	if next, err := s.ListRuntimeBuilds(ctx, testCaller, BuildListRequest{Limit: 1, Cursor: page.NextCursor}); err != nil || len(next.Builds) != 1 || next.Builds[0].ID != first.ID {
		t.Fatalf("second page = %+v, %v", next, err)
	}

	// Cancelling wakes a watcher and frees a slot.
	_, changed, err := s.WatchRuntimeBuild(ctx, buildAlice, first.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := s.CancelRuntimeBuild(ctx, testCaller, first.ID)
	if err != nil || cancelled.State != BuildCancelled {
		t.Fatalf("cancel = %+v, %v", cancelled, err)
	}
	select {
	case <-changed:
	default:
		t.Fatal("the watcher was not woken")
	}
	if again, err := s.CancelRuntimeBuild(ctx, buildAlice, first.ID); err != nil || again.State != BuildCancelled {
		t.Fatalf("cancel again = %+v, %v", again, err)
	}
	if _, err := s.BuildRuntime(ctx, buildAlice, serverBuildRequest("edge", procArchive(t, "// 3"))); err != nil {
		t.Fatal(err)
	}
}

// TestServerBuildsReuseIdenticalInputs: a build of the inputs of a done
// build is done at once with its binary, without saying whose it was; a
// build whose builder is lost twice fails in upkeep with a reason.
func TestServerBuildsReuseIdenticalInputs(t *testing.T) {
	ctx := context.Background()
	s := serverBuildPlatform(t)
	builds, _ := s.serverBuilds()
	first, err := s.BuildRuntime(ctx, buildAlice, serverBuildRequest("edge", procArchive(t, "")))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	claimed, err := builds.ClaimBuilds(ctx, "builder-1", 1, now, now.Add(time.Minute))
	if err != nil || len(claimed) != 1 || claimed[0].ID != first.ID {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	if err := builds.AppendBuildLog(ctx, first.ID, "builder-1", claimed[0].Attempt, []string{"compiling"}); err != nil {
		t.Fatal(err)
	}
	if _, err := builds.FinishBuild(ctx, first.ID, "builder-1", claimed[0].Attempt, artifact.BuildOutcome{State: artifact.BuildDone,
		Result:       []byte(`{"runtime":"sha256:` + strings.Repeat("c", 64) + `","hello":{"protocol":2,"runtime":"sha256:` + strings.Repeat("c", 64) + `"},"os":"linux","arch":"amd64"}`),
		BinaryDigest: "sha256:" + strings.Repeat("b", 64), BinaryBytes: 1 << 20}, now); err != nil {
		t.Fatal(err)
	}
	done, err := s.GetRuntimeBuild(ctx, buildAlice, first.ID, 0)
	if err != nil || done.State != BuildDone || len(done.Log) != 1 || done.Log[0] != "compiling" {
		t.Fatalf("done = %+v, %v", done, err)
	}

	reused, err := s.BuildRuntime(ctx, buildBob, serverBuildRequest("other-name", procArchive(t, "")))
	if err != nil {
		t.Fatal(err)
	}
	if reused.State != BuildDone || !reused.Reused || reused.Caller.Principal != "user:bob" || reused.ID == first.ID {
		t.Fatalf("reused = %+v", reused)
	}
	if e, err := s.catalog.Runtime(ctx, "other-name"); err != nil || e.Build != reused.ID || e.RegisteredBy != "user:bob" || e.Binary == nil {
		t.Fatalf("the reused build registered %+v, %v", e, err)
	}

	lost, err := s.BuildRuntime(ctx, buildBob, serverBuildRequest("edge", procArchive(t, "// lost")))
	if err != nil {
		t.Fatal(err)
	}
	for attempt := range serverBuildAttempts {
		if claimed, err := builds.ClaimBuilds(ctx, "builder-2", 1, now, now.Add(time.Second)); err != nil || len(claimed) != 1 {
			t.Fatalf("claim %d = %+v, %v", attempt, claimed, err)
		}
		if err := s.buildUpkeep(ctx, builds, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	failed, err := s.GetRuntimeBuild(ctx, buildBob, lost.ID, 0)
	if err != nil || failed.State != BuildFailed || !strings.Contains(failed.Error, "stopped responding") {
		t.Fatalf("failed = %+v, %v", failed, err)
	}
	// A failed build returns its manifest as sent, so any browser can
	// rebuild it; the list does not carry it.
	if failed.Manifest != serverBuildManifest {
		t.Fatalf("failed build's manifest = %q", failed.Manifest)
	}
	page, err := s.ListRuntimeBuilds(ctx, buildBob, BuildListRequest{})
	if err != nil || len(page.Builds) == 0 || page.Builds[0].Manifest != "" {
		t.Fatalf("list = %+v, %v", page, err)
	}
}
