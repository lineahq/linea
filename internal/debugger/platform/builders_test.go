package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/release"
	"github.com/lineahq/linea/internal/debugger/store/memory"
	"github.com/lineahq/linea/internal/debugger/worker"
)

const testBuildToken = "a-build-token-of-at-least-32-characters"

// objectRepository is the memory repository with object storage the way
// the pg store has it: presigned URLs served by a test server.
type objectRepository struct {
	artifact.Repository
	builds artifact.Builds
	srv    *httptest.Server

	mu      sync.Mutex
	objects map[string][]byte // by URL path
	blobs   map[string][]byte // by digest
	files   map[string]string // by URL path: files on disk
}

func newObjectRepository(t *testing.T, data *memory.Data) *objectRepository {
	repo := memory.Open(data, artifact.Limits{})
	o := &objectRepository{Repository: repo, builds: repo, objects: map[string][]byte{}, blobs: map[string][]byte{}, files: map[string]string{}}
	o.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		defer o.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			if f, ok := o.files[r.URL.Path]; ok {
				http.ServeFile(w, r, f)
				return
			}
			b, ok := o.objects[r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(b)
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			o.objects[r.URL.Path] = b
		}
	}))
	t.Cleanup(o.srv.Close)
	return o
}

func (o *objectRepository) RunBundleURL(context.Context, string, time.Duration) (artifact.ObjectURL, error) {
	return artifact.ObjectURL{}, artifact.ErrNotFound
}

func (o *objectRepository) StageURL(_ context.Context, name string, _ time.Duration) (string, error) {
	return o.srv.URL + "/staging/" + name, nil
}

func (o *objectRepository) TakeStaged(context.Context, string, string, int64, int64) ([]byte, error) {
	return nil, artifact.ErrNotFound
}

// inputs maps the digests of the build's inputs to their bytes.
func (o *objectRepository) BuildInputURL(ctx context.Context, id, digest string, _ time.Duration) (artifact.ObjectURL, error) {
	r, err := o.builds.Build(ctx, id)
	if err != nil {
		return artifact.ObjectURL{}, err
	}
	for _, in := range r.Inputs {
		if in.Digest == digest {
			o.mu.Lock()
			defer o.mu.Unlock()
			b, ok := o.blobs[digest]
			if !ok {
				return artifact.ObjectURL{}, artifact.ErrNotFound
			}
			o.objects["/blobs/"+digest] = b
			return artifact.ObjectURL{URL: o.srv.URL + "/blobs/" + digest, Digest: digest, Size: int64(len(b))}, nil
		}
	}
	return artifact.ObjectURL{}, artifact.ErrNotFound
}

func (o *objectRepository) StoreBuildBinary(ctx context.Context, id, builder string, attempt int, name, digest string, size, limit int64) error {
	r, err := o.builds.Build(ctx, id)
	if err != nil {
		return err
	}
	if r.State != artifact.BuildRunning || r.Builder != builder || r.Attempt != attempt {
		return artifact.ErrConflict
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	b, ok := o.objects["/staging/"+name]
	delete(o.objects, "/staging/"+name)
	switch {
	case !ok:
		return artifact.ErrNotFound
	case size > limit:
		return artifact.ErrLimit
	case int64(len(b)) != size || blobDigest(b) != digest:
		return artifact.ErrCorrupt
	}
	o.blobs[digest] = b
	return nil
}

// AdmitBuild keeps the inputs' bytes, as the pg store's blobs do.
func (o *objectRepository) AdmitBuild(ctx context.Context, a artifact.BuildAdmission, limits artifact.BuildLimits) (artifact.BuildRecord, error) {
	o.mu.Lock()
	o.blobs[blobDigest(a.Manifest)] = a.Manifest
	for _, s := range a.Sources {
		o.blobs[blobDigest(s.Archive)] = s.Archive
	}
	o.mu.Unlock()
	return o.builds.AdmitBuild(ctx, a, limits)
}

// builderPlatform is a team server whose repository has object storage and
// that takes builders with testBuildToken, and the URL builders dial.
func builderPlatform(t *testing.T) (*Service, *objectRepository, string) {
	t.Helper()
	return builderPlatformWith(t, Options{})
}

// builderPlatformWith is builderPlatform with opts, whose access and build
// token it sets.
func builderPlatformWith(t *testing.T, opts Options) (*Service, *objectRepository, string) {
	t.Helper()
	renew, poll, flush := buildRenew, buildPoll, buildFlush
	t.Cleanup(func() { buildRenew, buildPoll, buildFlush = renew, poll, flush })
	buildRenew, buildPoll, buildFlush = 20*time.Millisecond, 20*time.Millisecond, 10*time.Millisecond
	access, err := NewPolicy([]Binding{
		{Subject: Subject{Principal: testCaller.Principal.ID}, Role: "admin", Scope: ScopeAll},
		{Subject: Subject{Group: "engineers"}, Role: "engineer", Scope: ScopeAll},
	})
	if err != nil {
		t.Fatal(err)
	}
	repo := newObjectRepository(t, memory.NewData())
	opts.Access, opts.BuilderToken = access, testBuildToken
	s, err := Open(repo, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	srv := httptest.NewServer(http.HandlerFunc(s.ConnectBuilder))
	t.Cleanup(srv.Close)
	return s, repo, srv.URL
}

// serveTestBuilds runs a builder in this process with build until the test
// ends.
func serveTestBuilds(t *testing.T, url string, build worker.BuildFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- worker.ServeBuilds(ctx, worker.BuilderOptions{URL: url, Token: testBuildToken,
			Hello: worker.BuilderHello{Name: "builder-a", Capacity: 1, OS: "linux", Arch: "amd64"}}, build)
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("ServeBuilds = %v", err)
		}
	})
}

// fakeBuild reads every input of the job and uploads binary as the built
// worker of identity.
func fakeBuild(binary []byte, identity string) worker.BuildFunc {
	return func(ctx context.Context, job worker.BuildJob, log func(string)) worker.BuildResult {
		for _, in := range append([]worker.BuildInput{job.Manifest}, job.Sources...) {
			resp, err := http.Get(in.URL)
			if err != nil {
				return worker.BuildResult{Error: err.Error()}
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if blobDigest(b) != in.Digest || int64(len(b)) != in.Size {
				return worker.BuildResult{Error: "input " + in.Module + " differs"}
			}
		}
		log(fmt.Sprintf("building %s for Collector %s", job.Name, job.CollectorVersion))
		req, _ := http.NewRequest(http.MethodPut, job.BinaryURL, bytes.NewReader(binary))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return worker.BuildResult{Error: err.Error()}
		}
		resp.Body.Close()
		return worker.BuildResult{BinaryDigest: blobDigest(binary), BinarySize: int64(len(binary)), OS: "linux", Arch: "amd64",
			Hello: &worker.Hello{Protocol: worker.ProtocolVersion, Runtime: identity, ProductVersion: release.ProductVersion,
				CollectorVersion: job.CollectorVersion, Verified: job.Verified, Name: "ignored", Capacity: 3}}
	}
}

// waitServerBuild waits until the server build ends.
func waitServerBuild(t *testing.T, s *Service, caller Caller, id string) *RuntimeBuild {
	t.Helper()
	deadline := time.After(time.Minute)
	for {
		b, changed, err := s.WatchRuntimeBuild(context.Background(), caller, id, 0)
		if err != nil {
			t.Fatal(err)
		}
		if BuildState(b.State) == BuildDone || b.State == BuildFailed || b.State == BuildCancelled {
			return b
		}
		select {
		case <-changed:
		case <-time.After(100 * time.Millisecond):
		case <-deadline:
			t.Fatalf("build %s did not end: %+v", id, b)
		}
	}
}

// TestABuilderBuildsAndRegistersARuntime: a connected builder claims a
// queued build, reads its inputs and uploads the binary; the server ends
// the build done and registers the runtime under the build's name, by the
// build's principal. An identical build later reuses it and registers its
// own name.
func TestABuilderBuildsAndRegistersARuntime(t *testing.T) {
	ctx := context.Background()
	s, repo, url := builderPlatform(t)
	identity := "sha256:" + strings.Repeat("c", 64)
	binary := []byte("a static worker binary")
	serveTestBuilds(t, url, fakeBuild(binary, identity))

	build, err := s.BuildRuntime(ctx, buildAlice, serverBuildRequest("edge", procArchive(t, "")))
	if err != nil {
		t.Fatal(err)
	}
	done := waitServerBuild(t, s, buildAlice, build.ID)
	if done.State != BuildDone || done.Error != "" || len(done.Log) != 1 || !strings.HasPrefix(done.Log[0], "building edge") {
		t.Fatalf("build = %+v", done)
	}
	if !bytes.Equal(repo.blobs[blobDigest(binary)], binary) {
		t.Fatal("the binary was not stored")
	}
	e, err := s.catalog.Runtime(ctx, "edge")
	if err != nil {
		t.Fatal(err)
	}
	if e.Identity != identity || e.Build != build.ID || e.RegisteredBy != "user:alice" || e.Image != "" ||
		e.Binary == nil || e.Binary.Digest != blobDigest(binary) || e.Binary.Bytes != int64(len(binary)) || e.Binary.OS != "linux" || e.Binary.Arch != "amd64" ||
		string(e.Manifest) != serverBuildManifest {
		t.Fatalf("catalog entry = %+v", e)
	}
	var hello worker.Hello
	if err := json.Unmarshal(e.Hello, &hello); err != nil || hello.Name != "" || hello.Capacity != 0 || hello.Runtime != identity {
		t.Fatalf("catalog hello = %+v, %v", hello, err)
	}

	reused, err := s.BuildRuntime(ctx, buildBob, serverBuildRequest("edge-copy", procArchive(t, "")))
	if err != nil || reused.State != BuildDone || !reused.Reused {
		t.Fatalf("reused = %+v, %v", reused, err)
	}
	if e, err := s.catalog.Runtime(ctx, "edge-copy"); err != nil || e.Identity != identity || e.Build != reused.ID || e.RegisteredBy != "user:bob" ||
		string(e.Manifest) != serverBuildManifest {
		t.Fatalf("reused catalog entry = %+v, %v", e, err)
	}
}

// TestABuiltRuntimeListsItsManifest: a runtime built on the server keeps its
// build's manifest in its catalog entry, so the listing and a read name it
// without the build. An entry saved before entries kept a manifest is read
// through from its build while the build is kept, and has none without it.
func TestABuiltRuntimeListsItsManifest(t *testing.T) {
	ctx := context.Background()
	s, _, url := builderPlatform(t)
	identity := "sha256:" + strings.Repeat("c", 64)
	serveTestBuilds(t, url, fakeBuild([]byte("a static worker binary"), identity))
	build, err := s.BuildRuntime(ctx, buildAlice, serverBuildRequest("edge", procArchive(t, "")))
	if err != nil {
		t.Fatal(err)
	}
	waitServerBuild(t, s, buildAlice, build.ID)
	e, err := s.catalog.Runtime(ctx, "edge")
	if err != nil || string(e.Manifest) != serverBuildManifest {
		t.Fatalf("catalog entry manifest = %q, %v", e.Manifest, err)
	}

	// The entry as an older server saved it: the same build, no manifest.
	older := e
	older.Name, older.Manifest = "older", nil
	if err := s.catalog.SaveRuntime(ctx, older); err != nil {
		t.Fatal(err)
	}
	gone := older
	gone.Name, gone.Build = "gone", "run-trimmed"
	if err := s.catalog.SaveRuntime(ctx, gone); err != nil {
		t.Fatal(err)
	}

	// The catalog's half of ListRuntimes: this platform has no Docker.
	listed, err := s.catalogRuntimes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	manifests := map[string]string{}
	for _, r := range listed {
		manifests[r.Name] = r.Manifest
	}
	for name, want := range map[string]string{"edge": serverBuildManifest, "older": serverBuildManifest, "gone": ""} {
		if got, ok := manifests[name]; !ok || got != want {
			t.Errorf("listed %s manifest = %q (listed %v); want %q", name, got, ok, want)
		}
		if r, err := s.GetRuntime(ctx, buildAlice, name); err != nil || r.Manifest != want {
			t.Errorf("read %s = %+v, %v; want manifest %q", name, r, err, want)
		}
	}
}

// TestABuilderResultIsChecked: a build whose builder fails, whose binary
// differs from the uploaded one or whose worker is another version fails
// with a reason, and registers nothing.
func TestABuilderResultIsChecked(t *testing.T) {
	identity := "sha256:" + strings.Repeat("d", 64)
	for _, tc := range []struct {
		name  string
		build worker.BuildFunc
		want  string
	}{
		{"a failed build", func(context.Context, worker.BuildJob, func(string)) worker.BuildResult {
			return worker.BuildResult{Error: "go: module example.com/proc: not found"}
		}, "not found"},
		{"another binary", func(ctx context.Context, job worker.BuildJob, log func(string)) worker.BuildResult {
			r := fakeBuild([]byte("uploaded"), identity)(ctx, job, log)
			r.BinaryDigest = blobDigest([]byte("reported"))
			return r
		}, "differs"},
		{"another version", func(ctx context.Context, job worker.BuildJob, log func(string)) worker.BuildResult {
			r := fakeBuild([]byte("binary"), identity)(ctx, job, log)
			r.Hello.ProductVersion = "0.0.0-other"
			return r
		}, "0.0.0-other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, url := builderPlatform(t)
			serveTestBuilds(t, url, tc.build)
			build, err := s.BuildRuntime(context.Background(), buildAlice, serverBuildRequest("edge", procArchive(t, "")))
			if err != nil {
				t.Fatal(err)
			}
			done := waitServerBuild(t, s, buildAlice, build.ID)
			if done.State != BuildFailed || !strings.Contains(done.Error, tc.want) {
				t.Fatalf("build = %+v, want failed naming %q", done, tc.want)
			}
			if _, err := s.catalog.Runtime(context.Background(), "edge"); err == nil {
				t.Fatal("a failed build registered a runtime")
			}
		})
	}
}

// TestCancellingABuildStopsItsBuilder: a cancel request reaches the
// builder through the lease renewal, and the build ends cancelled.
func TestCancellingABuildStopsItsBuilder(t *testing.T) {
	s, _, url := builderPlatform(t)
	started := make(chan struct{})
	serveTestBuilds(t, url, func(ctx context.Context, job worker.BuildJob, log func(string)) worker.BuildResult {
		close(started)
		<-ctx.Done()
		return worker.BuildResult{Error: "stopped"}
	})
	build, err := s.BuildRuntime(context.Background(), buildAlice, serverBuildRequest("edge", procArchive(t, "")))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Minute):
		t.Fatal("the builder did not start the build")
	}
	if _, err := s.CancelRuntimeBuild(context.Background(), buildAlice, build.ID); err != nil {
		t.Fatal(err)
	}
	if done := waitServerBuild(t, s, buildAlice, build.ID); done.State != BuildCancelled {
		t.Fatalf("build = %+v", done)
	}
}

// TestBuildersNeedATokenAndObjectStorage: a service without a build token
// or without object storage refuses builders, and a wrong token is
// refused.
func TestBuildersNeedATokenAndObjectStorage(t *testing.T) {
	for name, s := range map[string]*Service{
		"no token":          openPlatform(t, memory.NewData(), Options{}),
		"no object storage": openPlatform(t, memory.NewData(), Options{BuilderToken: testBuildToken}),
	} {
		rec := httptest.NewRecorder()
		s.ConnectBuilder(rec, httptest.NewRequest(http.MethodGet, worker.BuilderPath, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", name, rec.Code)
		}
	}
	_, _, url := builderPlatform(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	err := worker.ServeBuilds(ctx, worker.BuilderOptions{URL: url, Token: "wrong", Hello: worker.BuilderHello{Name: "b", Capacity: 1}}, nil)
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("ServeBuilds with a wrong token = %v", err)
	}
}

// TestABuildStoresOnlyTheImageItAskedFor: a build keeps the builder's
// image only by digest in the configured repository, keeps an export
// failure as its reason, and keeps nothing when no export was configured.
func TestABuildStoresOnlyTheImageItAskedFor(t *testing.T) {
	const repo = "registry.example.com/team/runtimes"
	digest := "sha256:" + strings.Repeat("e", 64)
	for _, tc := range []struct {
		name, repository  string
		result            worker.BuildResult
		image, exportFail string
	}{
		{"exported", repo, worker.BuildResult{Image: repo + "@" + digest}, repo + "@" + digest, ""},
		{"failed", repo, worker.BuildResult{ExportError: "push refused"}, "", "push refused"},
		{"by tag", repo, worker.BuildResult{Image: repo + ":edge"}, "", "the builder reported no image in " + repo},
		{"elsewhere", repo, worker.BuildResult{Image: "registry.example.com/other@" + digest}, "", "the builder reported no image in " + repo},
		{"missing", repo, worker.BuildResult{}, "", "the builder reported no image in " + repo},
		{"not asked", "", worker.BuildResult{Image: repo + "@" + digest, ExportError: "x"}, "", ""},
	} {
		image, fail := checkExport(tc.result, tc.repository)
		if image != tc.image || fail != tc.exportFail {
			t.Errorf("%s: checkExport = %q, %q", tc.name, image, fail)
		}
	}
}

// TestAnExportRepositoryIsChecked: Open refuses a build export that names
// no image repository and normalizes one that does.
func TestAnExportRepositoryIsChecked(t *testing.T) {
	if _, err := openMemory(memory.NewData(), Options{BuildExport: "registry.example.com/team/runtimes:edge"}); err == nil {
		t.Fatal("Open took a tagged export repository")
	}
	s, err := openMemory(memory.NewData(), Options{BuildExport: "runtimes"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.buildExport != "docker.io/library/runtimes" {
		t.Fatalf("buildExport = %q", s.buildExport)
	}
}

// TestABuildExportsItsImageWhenConfigured: with a build export, the job
// names the repository and the build's name as the tag, and the build
// shows the image the builder pushed; a failed push leaves it done with
// the reason.
func TestABuildExportsItsImageWhenConfigured(t *testing.T) {
	const repo = "registry.example.com/team/runtimes"
	identity := "sha256:" + strings.Repeat("f", 64)
	image := repo + "@sha256:" + strings.Repeat("1", 64)
	for _, tc := range []struct {
		name               string
		image, exportError string
	}{
		{"pushed", image, ""},
		{"refused", "", "push refused: denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, url := builderPlatformWith(t, Options{BuildExport: repo})
			serveTestBuilds(t, url, func(ctx context.Context, job worker.BuildJob, log func(string)) worker.BuildResult {
				r := fakeBuild([]byte("binary"), identity)(ctx, job, log)
				if job.Export == nil || job.Export.Repository != repo || job.Export.Tag != "edge" {
					return worker.BuildResult{Error: fmt.Sprintf("export = %+v", job.Export)}
				}
				r.Image, r.ExportError = tc.image, tc.exportError
				return r
			})
			build, err := s.BuildRuntime(context.Background(), buildAlice, serverBuildRequest("edge", procArchive(t, "")))
			if err != nil {
				t.Fatal(err)
			}
			done := waitServerBuild(t, s, buildAlice, build.ID)
			if done.State != BuildDone || done.Image != tc.image || done.ExportError != tc.exportError {
				t.Fatalf("build = %+v", done)
			}
		})
	}
}
