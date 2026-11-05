package platform

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/release"
	"github.com/lineahq/linea/internal/debugger/worker"
)

// fakeRuntimes is a RuntimeStore whose runtimes are the in-process engine
// behind a bundle boundary, as a worker is: it decodes each run for its
// own runtime ID, so a bundle naming another runtime is refused.
type fakeRuntimes struct {
	mu       sync.Mutex
	images   map[string]RuntimeInfo // by name
	starts   atomic.Int32
	inspects atomic.Int32
	// build, when set, is what Build waits for before succeeding.
	build chan struct{}
	// execute, when set, is what every Execute waits for.
	execute chan struct{}
	// importable is what Import finds.
	importable *RuntimeInfo
	// specs are the builds Build was given.
	specs []RuntimeBuildSpec
	// By runtime name: what a runtime changes in its reports and
	// inspections, as another Collector release would, and the feature
	// gates its worker lists.
	mutate  map[string]func(*engine.Report)
	inspect map[string]func(*engine.Inspection)
	gates   map[string][]release.Gate
}

func newFakeRuntimes(names ...string) *fakeRuntimes {
	f := &fakeRuntimes{images: map[string]RuntimeInfo{}}
	for _, n := range names {
		f.images[n] = fakeInfo(n)
	}
	return f
}

func fakeInfo(name string) RuntimeInfo {
	return RuntimeInfo{Name: name, ImageID: "sha256:image-" + name, CollectorVersion: "0.162.0",
		Protocol: worker.ProtocolVersion, Verified: true, Manifest: "dist:\n  name: " + name + "\n", ModuleSet: "sha256:modules"}
}

func (f *fakeRuntimes) Check(spec RuntimeBuildSpec) error {
	if strings.Contains(string(spec.Manifest), "unknown") {
		return manifestDiagnostics("manifest_modules", "module example.com/unknown is not in the dependency set")
	}
	return nil
}

func (f *fakeRuntimes) Build(ctx context.Context, spec RuntimeBuildSpec, log func(string)) (*RuntimeInfo, error) {
	log("compiling " + spec.Name)
	if f.build != nil {
		select {
		case <-f.build:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	info := fakeInfo(spec.Name)
	info.Manifest = string(spec.Manifest)
	f.mu.Lock()
	f.specs = append(f.specs, spec)
	f.images[spec.Name] = info
	f.mu.Unlock()
	log("built " + spec.Name)
	return &info, nil
}

func (f *fakeRuntimes) List(context.Context) ([]RuntimeInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []RuntimeInfo
	for _, info := range f.images {
		out = append(out, info)
	}
	return out, nil
}

func (f *fakeRuntimes) Get(_ context.Context, ref string) (*RuntimeInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, info := range f.images {
		if info.Name == ref || info.ImageID == ref {
			return &info, nil
		}
	}
	return nil, ErrRuntimeNotFound
}

func (f *fakeRuntimes) Delete(_ context.Context, info *RuntimeInfo) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.images, info.Name)
	return nil
}

func (f *fakeRuntimes) Push(context.Context, *RuntimeInfo, string) error { return nil }

func (f *fakeRuntimes) Import(_ context.Context, ref string, _ bool, accept func(*RuntimeInfo) error) (*RuntimeInfo, error) {
	if f.importable == nil || f.importable.ImageID != ref {
		return nil, ErrRuntimeNotFound
	}
	info := *f.importable
	if err := accept(&info); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.images[info.Name] = info
	f.mu.Unlock()
	return &info, nil
}

func (f *fakeRuntimes) Start(_ context.Context, info *RuntimeInfo) (RuntimeWorker, error) {
	f.starts.Add(1)
	return &fakeWorker{f: f, id: "sha256:runtime-" + info.Name, name: info.Name}, nil
}

type fakeWorker struct {
	f        *fakeRuntimes
	id, name string
}

func (w *fakeWorker) FeatureGates() []release.Gate { return w.f.gates[w.name] }

func (w *fakeWorker) Target() engine.RuntimeTarget { return engine.RuntimeTarget{ID: w.id} }

func (w *fakeWorker) Execute(ctx context.Context, req engine.Request) (*engine.Report, error) {
	if w.f.execute != nil {
		select {
		case <-w.f.execute:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	raw, _, err := engine.EncodeBundle(req)
	if err != nil {
		return nil, err
	}
	decoded, err := engine.DecodeBundleForRuntime(ctx, raw, w.id)
	if err != nil {
		return nil, err
	}
	decoded.RunID, decoded.Observer = req.RunID, req.Observer
	rep, err := engine.Execute(ctx, decoded)
	if err == nil && w.f.mutate[w.name] != nil {
		w.f.mutate[w.name](rep)
	}
	return rep, err
}

func (w *fakeWorker) Inspect(ctx context.Context, in *engine.InspectionInput) (*engine.Inspection, error) {
	w.f.inspects.Add(1)
	ins, err := engine.CompleteInspection(ctx, in)
	if err == nil && w.f.inspect[w.name] != nil {
		w.f.inspect[w.name](ins)
	}
	return ins, err
}

func (w *fakeWorker) EvaluateOTTL(ctx context.Context, in *engine.OTTLInput) (*engine.OTTLEvaluation, error) {
	return engine.CompleteOTTL(ctx, in)
}

func (w *fakeWorker) Close() error { return nil }

func logsCleanup(parts ...string) string {
	return filepath.Join(append([]string{"..", "..", "..", "samples", "logs-cleanup"}, parts...)...)
}

// TestARunOnABuiltRuntimeRecordsIt runs a quick and a scenario run on a
// selected runtime: the bundle names the runtime's identity, the report
// records the runtime and is otherwise the built-in run's report,
// and one worker serves every run and inspection.
func TestARunOnABuiltRuntimeRecordsIt(t *testing.T) {
	store := newFakeRuntimes("v0162")
	s := New(Options{Paths: PathPolicy{Unrestricted: true}, Runtimes: store})
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, req := range []RunRequest{
		{Mode: ModeQuick, Config: Source{Path: logsCleanup("config.yaml")}, Quick: &QuickInput{Fixture: Source{Path: logsCleanup("input.json")}}, Normalize: true},
		{Mode: ModeScenario, Config: Source{Path: logsCleanup("config.yaml")}, Scenario: &ScenarioInput{Scenario: Source{Path: logsCleanup("scenario.yaml")}}, Normalize: true},
	} {
		builtIn, err := s.ExecuteRun(ctx, testCaller, req)
		if err != nil {
			t.Fatal(err)
		}
		req.Runtime = "v0162"
		run, err := s.SubmitRun(ctx, testCaller, req)
		if err != nil {
			t.Fatal(err)
		}
		rep, err := s.GetRunReport(ctx, testCaller, run.ID, true)
		if err != nil {
			t.Fatal(err)
		}
		rt := rep.Runtime
		if rt == nil || rt.Kind != engine.RuntimeContainer || rt.Name != "v0162" || rt.ImageID != "sha256:image-v0162" ||
			rt.CollectorVersion != "0.162.0" || !strings.HasPrefix(rt.ManifestDigest, "sha256:") || !rt.Verified || rt.FeatureGates == nil {
			t.Fatalf("runtime = %+v", rt)
		}
		if builtIn.Runtime == nil || builtIn.Runtime.Kind != engine.RuntimeBuiltIn {
			t.Fatalf("built-in runtime = %+v", builtIn.Runtime)
		}
		a, b := *rep, *builtIn
		a.Runtime, b.Runtime = nil, nil
		if ja, jb := mustJSON(t, a), mustJSON(t, b); ja != jb {
			t.Fatalf("%s: the report differs from the built-in run's beyond its runtime:\n%s\n%s", req.Mode, ja, jb)
		}
		raw, err := s.GetRunBundle(ctx, testCaller, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.DecodeBundleForRuntime(ctx, raw, "sha256:runtime-v0162"); err != nil {
			t.Fatalf("bundle is not for the runtime: %v", err)
		}
	}
	ins, err := s.InspectConfiguration(ctx, testCaller, InspectRequest{Config: Source{Path: logsCleanup("config.yaml")}, Runtime: "v0162"})
	if err != nil || !ins.Valid || store.inspects.Load() != 1 {
		t.Fatalf("inspection = %+v, %v, %d worker inspections", ins, err, store.inspects.Load())
	}
	if n := store.starts.Load(); n != 1 {
		t.Fatalf("%d workers started, want one for every use", n)
	}
}

// TestADefaultRuntimeKeepsWorkOutOfProcess sends runs, suites, inspections
// and previews that name no runtime, or the built-in one, to the default
// runtime (team mode); a missing default is unavailable.
func TestADefaultRuntimeKeepsWorkOutOfProcess(t *testing.T) {
	store := newFakeRuntimes("team")
	s := New(Options{Paths: PathPolicy{Unrestricted: true}, Runtimes: store, DefaultRuntime: "team"})
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	quick := RunRequest{Mode: ModeQuick, Config: Source{Path: logsCleanup("config.yaml")}, Quick: &QuickInput{Fixture: Source{Path: logsCleanup("input.json")}}}
	for _, name := range []string{"", BuiltInRuntime} {
		quick.Runtime = name
		rep, err := s.ExecuteRun(ctx, testCaller, quick)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Runtime == nil || rep.Runtime.Kind != engine.RuntimeContainer || rep.Runtime.Name != "team" {
			t.Fatalf("runtime %q: report runtime = %+v", name, rep.Runtime)
		}
		if _, err := s.InspectConfiguration(ctx, testCaller, InspectRequest{Config: Source{Path: logsCleanup("config.yaml")}, Runtime: name}); err != nil {
			t.Fatal(err)
		}
	}
	if n := store.inspects.Load(); n != 2 {
		t.Fatalf("%d worker inspections, want 2", n)
	}
	suite, err := s.SubmitSuite(ctx, testCaller, SubmitSuiteRequest{Config: Source{Path: logsCleanup("config.yaml")},
		Members: []ScenarioInput{{Scenario: Source{Path: logsCleanup("scenario.yaml")}}}})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := s.GetRunReport(ctx, testCaller, suite.Members[0].RunID, true)
	if err != nil || rep.Runtime == nil || rep.Runtime.Name != "team" {
		t.Fatalf("suite member runtime = %+v, %v", rep, err)
	}

	missing := New(Options{Paths: PathPolicy{Unrestricted: true}, Runtimes: newFakeRuntimes(), DefaultRuntime: "team"})
	defer missing.Close()
	quick.Runtime = ""
	_, err = missing.ExecuteRun(ctx, testCaller, quick)
	wantCode(t, err, CodeUnavailable)
	_, err = missing.PreviewTransformation(ctx, testCaller, PreviewRequest{Config: Source{Path: logsCleanup("config.yaml")}, Processor: "transform"})
	wantCode(t, err, CodeUnavailable)
}

// TestAMissingRuntimeIsUnavailable names the runtime in the error, for runs,
// inspections, suites and services without Docker.
func TestAMissingRuntimeIsUnavailable(t *testing.T) {
	ctx := context.Background()
	s := New(Options{Paths: PathPolicy{Unrestricted: true}, Runtimes: newFakeRuntimes()})
	defer s.Close()
	quick := RunRequest{Mode: ModeQuick, Config: Source{Path: logsCleanup("config.yaml")}, Quick: &QuickInput{Fixture: Source{Path: logsCleanup("input.json")}}, Runtime: "gone"}
	_, err := s.SubmitRun(ctx, testCaller, quick)
	wantCode(t, err, CodeUnavailable)
	if !strings.Contains(err.Error(), "gone") || !strings.Contains(err.Error(), "build or import") {
		t.Fatalf("error = %v", err)
	}
	_, err = s.InspectConfiguration(ctx, testCaller, InspectRequest{Config: Source{Path: logsCleanup("config.yaml")}, Runtime: "gone"})
	wantCode(t, err, CodeUnavailable)
	_, err = s.SubmitSuite(ctx, testCaller, SubmitSuiteRequest{Config: Source{Path: logsCleanup("config.yaml")},
		Members: []ScenarioInput{{Scenario: Source{Path: logsCleanup("scenario.yaml")}}}, Runtime: "gone"})
	wantCode(t, err, CodeUnavailable)
	_, err = s.GetRuntime(ctx, testCaller, "gone")
	wantCode(t, err, CodeNotFound)

	plain := New(Options{Paths: PathPolicy{Unrestricted: true}})
	defer plain.Close()
	_, err = plain.SubmitRun(ctx, testCaller, quick)
	wantCode(t, err, CodeUnavailable)
	_, err = plain.ListRuntimes(ctx, testCaller)
	wantCode(t, err, CodeUnavailable)
	// Naming the built-in runtime is the same as naming none.
	quick.Runtime = BuiltInRuntime
	if _, err := plain.ExecuteRun(ctx, testCaller, quick); err != nil {
		t.Fatal(err)
	}
}

// TestAnImportWithAnotherProtocolIsRefused accepts only images whose worker
// speaks this debugger's protocol, and never makes the others
// nameable.
func TestAnImportWithAnotherProtocolIsRefused(t *testing.T) {
	ctx := context.Background()
	store := newFakeRuntimes()
	s := New(Options{Runtimes: store})
	defer s.Close()
	old := fakeInfo("old")
	old.Protocol = worker.ProtocolVersion + 1
	store.importable = &old
	_, err := s.ImportRuntime(ctx, testCaller, ImportRuntimeRequest{Ref: old.ImageID})
	wantCode(t, err, CodeUnsupportedBuild)
	if _, err := s.GetRuntime(ctx, testCaller, "old"); ErrorCode(err) != CodeNotFound {
		t.Fatalf("a refused import is nameable: %v", err)
	}
	current := fakeInfo("ci")
	store.importable = &current
	info, err := s.ImportRuntime(ctx, testCaller, ImportRuntimeRequest{Ref: current.ImageID, Pull: true})
	if err != nil || info.Name != "ci" {
		t.Fatalf("import = %+v, %v", info, err)
	}
	_, err = s.ImportRuntime(ctx, testCaller, ImportRuntimeRequest{Ref: "sha256:missing"})
	wantCode(t, err, CodeNotFound)
}

// TestABuildNeverDelaysARun holds a build on the build slot while a run
// executes and finishes, then lets the build finish.
func TestABuildNeverDelaysARun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store := newFakeRuntimes()
	store.build = make(chan struct{})
	s := New(Options{Paths: PathPolicy{Unrestricted: true}, Runtimes: store})
	defer s.Close()
	build, err := s.BuildRuntime(ctx, testCaller, BuildRuntimeRequest{Name: "v0162", Manifest: Source{Text: "dist:\n  otelcol_version: 0.162.0\n"}})
	if err != nil || build.State != BuildQueued {
		t.Fatalf("build = %+v, %v", build, err)
	}
	// The log starts with the manifest's warnings (no optional features).
	var lines int
	for {
		v, changed, err := s.WatchRuntimeBuild(ctx, testCaller, build.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if v.State == BuildRunning && v.LogLines > len(v.Warnings) {
			if v.Log[len(v.Log)-1] != "compiling v0162" {
				t.Fatalf("log = %q", v.Log)
			}
			lines = v.LogLines
			break
		}
		<-changed
	}
	rep, err := s.ExecuteRun(ctx, testCaller, RunRequest{Mode: ModeQuick, Config: Source{Path: logsCleanup("config.yaml")}, Quick: &QuickInput{Fixture: Source{Path: logsCleanup("input.json")}}})
	if err != nil || rep == nil {
		t.Fatalf("run during a build = %v", err)
	}
	if v, _ := s.GetRuntimeBuild(ctx, testCaller, build.ID, 0); v.State != BuildRunning || v.Manifest != "dist:\n  otelcol_version: 0.162.0\n" {
		t.Fatalf("build = %s with manifest %q while the run finished", v.State, v.Manifest)
	}
	close(store.build)
	done, err := s.WaitRuntimeBuild(ctx, testCaller, build.ID, lines)
	if err != nil || done.State != BuildDone || done.Runtime == nil || done.Runtime.Name != "v0162" ||
		done.LogFrom != lines || len(done.Log) != 1 || done.LogLines != lines+1 {
		t.Fatalf("finished build = %+v, %v", done, err)
	}
	list, err := s.ListRuntimes(ctx, testCaller)
	if err != nil || len(list) != 1 || list[0].Name != "v0162" {
		t.Fatalf("runtimes = %+v, %v", list, err)
	}
}

// TestBuildsAreCheckedBoundedAndCancellable refuses a manifest before
// compiling, bounds the build queue and cancels queued and running builds.
func TestBuildsAreCheckedBoundedAndCancellable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	store := newFakeRuntimes()
	store.build = make(chan struct{})
	s := New(Options{Runtimes: store})
	defer s.Close()
	_, err := s.BuildRuntime(ctx, testCaller, BuildRuntimeRequest{Name: "bad", Manifest: Source{Text: "dist:\n  otelcol_version: 0.162.0\nprocessors:\n  - gomod: example.com/unknown v1.0.0\n"}})
	wantCode(t, err, CodeValidationFailed)
	var e *Error
	if !errors.As(err, &e) || len(e.Diagnostics) != 1 || e.Diagnostics[0].Code != "manifest_modules" {
		t.Fatalf("diagnostics = %+v", err)
	}
	for _, name := range []string{"", "Upper", "built-in", "-dash", strings.Repeat("a", 64)} {
		_, err := s.BuildRuntime(ctx, testCaller, BuildRuntimeRequest{Name: name, Manifest: Source{Text: "dist:\n  otelcol_version: 0.162.0\n"}})
		wantCode(t, err, CodeInvalidRequest)
	}
	var ids []string
	add := func(name string) {
		b, err := s.BuildRuntime(ctx, testCaller, BuildRuntimeRequest{Name: name, Manifest: Source{Text: "dist:\n  otelcol_version: 0.162.0\n"}})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, b.ID)
	}
	// One build runs; the queue is full once it holds the capacity beside it.
	add("running")
	for {
		v, changed, _ := s.WatchRuntimeBuild(ctx, testCaller, ids[0], 0)
		if v.State == BuildRunning {
			break
		}
		<-changed
	}
	for i := range buildQueueCapacity {
		add("queued" + string(rune('a'+i)))
	}
	_, err = s.BuildRuntime(ctx, testCaller, BuildRuntimeRequest{Name: "full", Manifest: Source{Text: "dist:\n  otelcol_version: 0.162.0\n"}})
	wantCode(t, err, CodeResourceExhausted)
	if errors.As(err, &e); !e.Retryable {
		t.Fatal("a full build queue is not retryable")
	}
	if v, err := s.CancelRuntimeBuild(ctx, testCaller, ids[2]); err != nil || v.State != BuildCancelled {
		t.Fatalf("cancel queued = %+v, %v", v, err)
	}
	if _, err := s.CancelRuntimeBuild(ctx, testCaller, ids[0]); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.WaitRuntimeBuild(ctx, testCaller, ids[0], 0); v.State != BuildCancelled {
		t.Fatalf("cancel running = %+v", v)
	}
	page, _ := s.ListRuntimeBuilds(ctx, testCaller, BuildListRequest{})
	if list := page.Builds; len(list) != len(ids) || list[0].ID != ids[len(ids)-1] {
		t.Fatalf("builds = %+v", list)
	}
	_, err = s.GetRuntimeBuild(ctx, testCaller, "missing", 0)
	wantCode(t, err, CodeNotFound)
}

// TestARuntimeInUseIsNotDeleted refuses deleting a runtime with a queued or
// running run, and deletes it once idle.
func TestARuntimeInUseIsNotDeleted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store := newFakeRuntimes("busy")
	store.execute = make(chan struct{})
	s := New(Options{Paths: PathPolicy{Unrestricted: true}, Runtimes: store})
	defer s.Close()
	run, err := s.SubmitRun(ctx, testCaller, RunRequest{Mode: ModeQuick, Config: Source{Path: logsCleanup("config.yaml")},
		Quick: &QuickInput{Fixture: Source{Path: logsCleanup("input.json")}}, Runtime: "busy"})
	if err != nil {
		t.Fatal(err)
	}
	wantCode(t, s.DeleteRuntime(ctx, testCaller, "busy"), CodeConflict)
	close(store.execute)
	if _, err := s.GetRunReport(ctx, testCaller, run.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRuntime(ctx, testCaller, "busy"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetRuntime(ctx, testCaller, "busy"); ErrorCode(err) != CodeNotFound {
		t.Fatalf("deleted runtime = %v", err)
	}
	wantCode(t, s.DeleteRuntime(ctx, testCaller, BuiltInRuntime), CodeInvalidRequest)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
