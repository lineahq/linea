package client

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lineahq/linea/build"
	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/platform"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

// memoryRuntimes is a RuntimeStore without Docker: builds succeed at once
// and no runtime starts a worker.
type memoryRuntimes struct {
	mu     sync.Mutex
	images map[string]platform.RuntimeInfo
	pushed []string
}

func (m *memoryRuntimes) Check(spec platform.RuntimeBuildSpec) error {
	if strings.Contains(string(spec.Manifest), "unknown") {
		return &platform.Error{Code: platform.CodeValidationFailed, Message: "manifest refused",
			Diagnostics: []platform.Diagnostic{{Severity: "error", Code: "manifest_modules", Message: "module example.com/unknown is not in the dependency set"}}}
	}
	return nil
}

func (m *memoryRuntimes) Build(_ context.Context, spec platform.RuntimeBuildSpec, log func(string)) (*platform.RuntimeInfo, error) {
	log("built " + spec.Name)
	info := platform.RuntimeInfo{Name: spec.Name, ImageID: "sha256:" + spec.Name, Tags: []string{"linea-runtime:" + spec.Name},
		CollectorVersion: "0.162.0", Verified: true, Manifest: string(spec.Manifest)}
	m.mu.Lock()
	m.images[spec.Name] = info
	m.mu.Unlock()
	return &info, nil
}

func (m *memoryRuntimes) List(context.Context) ([]platform.RuntimeInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []platform.RuntimeInfo
	for _, info := range m.images {
		out = append(out, info)
	}
	return out, nil
}

func (m *memoryRuntimes) Get(_ context.Context, ref string) (*platform.RuntimeInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if info, ok := m.images[ref]; ok {
		return &info, nil
	}
	return nil, platform.ErrRuntimeNotFound
}

func (m *memoryRuntimes) Delete(_ context.Context, info *platform.RuntimeInfo) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.images, info.Name)
	return nil
}

func (m *memoryRuntimes) Push(_ context.Context, info *platform.RuntimeInfo, target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pushed = append(m.pushed, info.Name+"->"+target)
	return nil
}

func (m *memoryRuntimes) Import(_ context.Context, ref string, _ bool, accept func(*platform.RuntimeInfo) error) (*platform.RuntimeInfo, error) {
	if ref != "old:1" {
		return nil, platform.ErrRuntimeNotFound
	}
	info := platform.RuntimeInfo{Name: "old", ImageID: "sha256:old", Protocol: -1}
	return nil, accept(&info)
}

func (m *memoryRuntimes) Start(context.Context, *platform.RuntimeInfo) (platform.RuntimeWorker, error) {
	return nil, errors.New("no workers here")
}

// TestRuntimeMethodsSpeakTheRuntimeRoutes builds, follows, lists, pushes and
// removes a runtime through a real API server, and keeps its refusals'
// codes and diagnostics.
func TestRuntimeMethodsSpeakTheRuntimeRoutes(t *testing.T) {
	store := &memoryRuntimes{images: map[string]platform.RuntimeInfo{}}
	service := platform.New(platform.Options{Paths: platform.PathPolicy{Unrestricted: true}, Runtimes: store})
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

	store.images["v0162"] = platform.RuntimeInfo{Name: "v0162", ImageID: "sha256:v0162", Tags: []string{"linea-runtime:v0162"}, CollectorVersion: "0.162.0", Verified: true}

	list, err := c.ListRuntimes(ctx)
	if err != nil || len(list) != 1 || list[0].Name != "v0162" || list[0].CollectorVersion != "0.162.0" {
		t.Fatalf("ListRuntimes = %+v, %v", list, err)
	}
	if err := c.PushRuntime(ctx, "v0162", "registry.example/rt:1"); err != nil || len(store.pushed) != 1 {
		t.Fatalf("PushRuntime = %v, %v", err, store.pushed)
	}

	var refused *Error
	if _, err := c.ImportRuntime(ctx, "missing:1", false); !errors.As(err, &refused) || refused.Status != 404 {
		t.Fatalf("import of a missing image = %v", err)
	}

	if support, err := c.CollectorSupport(ctx); err != nil || support.Oldest != "0.160.0" || len(support.Verified) == 0 {
		t.Fatalf("CollectorSupport = %+v, %v", support, err)
	}
	if m, err := c.ComposeManifest(ctx, api.ManifestRequest{Components: []string{"processors/transform"}}); err != nil || !strings.Contains(m.Manifest, "transformprocessor") {
		t.Fatalf("ComposeManifest = %+v, %v", m, err)
	}
	if _, err := c.ComposeManifest(ctx, api.ManifestRequest{Preset: "nope"}); !errors.As(err, &refused) || refused.Status != 400 {
		t.Fatalf("unknown preset = %v", err)
	}

	if err := c.DeleteRuntime(ctx, "v0162"); err != nil {
		t.Fatal(err)
	}
	if list, err := c.ListRuntimes(ctx); err != nil || len(list) != 0 {
		t.Fatalf("after delete = %+v, %v", list, err)
	}
	if err := c.DeleteRuntime(ctx, "v0162"); !errors.As(err, &refused) || refused.Status != 404 {
		t.Fatalf("second delete = %v", err)
	}
}

// TestUpgradeMethodsSpeakTheUpgradeRoutes moves a manifest to another
// release and keeps an upgrade check's refusals; the API tests run checks.
func TestUpgradeMethodsSpeakTheUpgradeRoutes(t *testing.T) {
	store := &memoryRuntimes{images: map[string]platform.RuntimeInfo{}}
	service := platform.New(platform.Options{Paths: platform.PathPolicy{Unrestricted: true}, Runtimes: store})
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

	m, err := c.RetargetManifest(ctx, api.ManifestRetargetRequest{Version: "0.160.0", ManifestYAML: string(build.Manifest)})
	if err != nil || m.To != "0.160.0" || !strings.Contains(m.Manifest, "otelcol_version: 0.160.0") || len(m.Moved) == 0 {
		t.Fatalf("RetargetManifest = %+v, %v", m, err)
	}
	var refused *Error
	if _, err := c.RetargetManifest(ctx, api.ManifestRetargetRequest{Version: "0.150.0", ManifestYAML: string(build.Manifest)}); !errors.As(err, &refused) || refused.Status != 404 {
		t.Fatalf("an unverified release = %v", err)
	}
	cfg := "receivers:\n  otlp:\n    protocols:\n      grpc:\nexporters:\n  debug:\nservice:\n  pipelines:\n    logs:\n      receivers: [otlp]\n      exporters: [debug]\n"
	for _, tc := range []struct {
		from, to string
		status   int
	}{{"", "built-in", 400}, {"", "missing", 503}} {
		_, _, err := c.CheckUpgrade(ctx, api.UpgradeCheckRequest{From: tc.from, To: tc.to, ConfigYAML: cfg})
		if !errors.As(err, &refused) || refused.Status != tc.status {
			t.Errorf("%q to %q = %v, want %d", tc.from, tc.to, err, tc.status)
		}
	}
}

// TestDefaultRuntimeMethodsSpeakTheDefaultRoute reads the default runtime
// before a choice, chooses the built-in runtime, and keeps the refusal of
// a runtime the catalog does not hold.
func TestDefaultRuntimeMethodsSpeakTheDefaultRoute(t *testing.T) {
	service, err := platform.Open(memory.Open(memory.NewData(), artifact.Limits{}), platform.Options{Paths: platform.PathPolicy{Unrestricted: true}})
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

	if d, err := c.GetDefaultRuntime(ctx); err != nil || d.Name != platform.BuiltInRuntime || d.SetBy != "" || d.Set != nil {
		t.Fatalf("GetDefaultRuntime = %+v, %v", d, err)
	}
	var refused *Error
	if _, err := c.SetDefaultRuntime(ctx, "gone"); !errors.As(err, &refused) || refused.Status != 404 {
		t.Fatalf("a runtime the catalog does not hold = %v", err)
	}
	if d, err := c.SetDefaultRuntime(ctx, platform.BuiltInRuntime); err != nil || d.Name != platform.BuiltInRuntime || d.SetBy == "" || d.Set == nil {
		t.Fatalf("SetDefaultRuntime = %+v, %v", d, err)
	}
	if d, err := c.GetDefaultRuntime(ctx); err != nil || d.SetBy == "" || d.Set == nil {
		t.Fatalf("GetDefaultRuntime after a choice = %+v, %v", d, err)
	}
}
