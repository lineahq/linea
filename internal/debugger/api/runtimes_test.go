package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/lineahq/linea/internal/debugger/platform"
	"github.com/lineahq/linea/internal/debugger/store/memory"
	"github.com/lineahq/linea/internal/debugger/worker"
)

// memoryRuntimes is a RuntimeStore without Docker for the adapter's
// mapping: builds succeed at once, and no runtime starts a worker.
type memoryRuntimes struct {
	mu     sync.Mutex
	images map[string]platform.RuntimeInfo
	pushed []string
	// start, when set, starts a runtime's worker.
	start func(*platform.RuntimeInfo) (platform.RuntimeWorker, error)
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

func (m *memoryRuntimes) Start(_ context.Context, info *platform.RuntimeInfo) (platform.RuntimeWorker, error) {
	if m.start != nil {
		return m.start(info)
	}
	return nil, errors.New("no workers here")
}

// TestRuntimeRoutesMapPlatformOperations lists, gets, pushes and
// deletes a runtime over HTTP, and maps refusals to their codes.
func TestRuntimeRoutesMapPlatformOperations(t *testing.T) {
	store := &memoryRuntimes{images: map[string]platform.RuntimeInfo{}}
	service := platform.New(platform.Options{Paths: platform.PathPolicy{Unrestricted: true}, Runtimes: store})
	t.Cleanup(service.Close)
	_, srv := newServerOptions(t, Options{Platform: service})
	api := srv.URL + "/api/v1"

	store.images["v0162"] = platform.RuntimeInfo{Name: "v0162", ImageID: "sha256:v0162", Tags: []string{"linea-runtime:v0162"}, CollectorVersion: "0.162.0", Verified: true, Manifest: "dist:\n  otelcol_version: 0.162.0\n"}

	resp, err := http.Get(api + "/runtimes")
	if err != nil {
		t.Fatal(err)
	}
	if list := decodeBody[[]Runtime](t, resp, http.StatusOK); len(list) != 1 || list[0].Name != "v0162" || list[0].Tags == nil {
		t.Fatalf("runtimes = %+v", list)
	}
	resp, err = http.Get(api + "/runtimes/v0162")
	if err != nil {
		t.Fatal(err)
	}
	if rt := decodeBody[Runtime](t, resp, http.StatusOK); rt.Manifest != "dist:\n  otelcol_version: 0.162.0\n" {
		t.Fatalf("runtime = %+v", rt)
	}
	resp = post(t, api+"/runtimes/v0162/push", RuntimePushRequest{Target: "registry.example/rt:1"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || len(store.pushed) != 1 {
		t.Fatalf("push: %d, %v", resp.StatusCode, store.pushed)
	}
	old := decodeBody[OperationError](t, post(t, api+"/runtimes/import", RuntimeImportRequest{Ref: "old:1"}), http.StatusConflict)
	if old.Code != string(platform.CodeUnsupportedBuild) {
		t.Fatalf("import of another protocol = %+v", old)
	}
	decodeBody[OperationError](t, post(t, api+"/runtimes/import", RuntimeImportRequest{Ref: "missing:1"}), http.StatusNotFound)

	// A run naming a runtime the store does not have is unavailable.
	missing := decodeBody[Error](t, post(t, api+"/runs", Request{ConfigYAML: readText(t, multisignalConfig), ScenarioYAML: scenarioText(t, multisignalScenario), Runtime: "gone"}), http.StatusServiceUnavailable)
	if !strings.Contains(missing.Error, "gone") {
		t.Fatalf("missing runtime = %+v", missing)
	}

	wantNoContent(t, del(t, api+"/runtimes/v0162"))
	decodeBody[OperationError](t, del(t, api+"/runtimes/v0162"), http.StatusNotFound)
}

// TestRuntimeRoutesWithoutDockerAreUnavailable answers every runtime route
// of a server without a runtime store with 503.
func TestRuntimeRoutesWithoutDockerAreUnavailable(t *testing.T) {
	_, srv := newServer(t)
	resp, err := http.Get(srv.URL + "/api/v1/runtimes")
	if err != nil {
		t.Fatal(err)
	}
	if e := decodeBody[OperationError](t, resp, http.StatusServiceUnavailable); e.Code != string(platform.CodeUnavailable) {
		t.Fatalf("list = %+v", e)
	}
}

// TestCatalogRoutesMapPlatformOperations reads the support table and a
// catalog and writes a manifest from a configuration over HTTP.
func TestCatalogRoutesMapPlatformOperations(t *testing.T) {
	_, srv := newServerOptions(t, Options{})
	api := srv.URL + "/api/v1"
	resp, err := http.Get(api + "/collector-versions")
	if err != nil {
		t.Fatal(err)
	}
	support := decodeBody[CollectorSupport](t, resp, http.StatusOK)
	if support.Oldest != "0.160.0" || len(support.Verified) == 0 || support.BuiltIn == "" {
		t.Fatalf("support = %+v", support)
	}
	resp, err = http.Get(api + "/collector-versions/" + support.Verified[0] + "/catalog")
	if err != nil {
		t.Fatal(err)
	}
	if cat := decodeBody[platform.CollectorCatalog](t, resp, http.StatusOK); len(cat.Components) == 0 {
		t.Fatal("empty catalog")
	}
	resp, err = http.Get(api + "/collector-versions/0.150.0/catalog")
	if err != nil {
		t.Fatal(err)
	}
	decodeBody[OperationError](t, resp, http.StatusNotFound)
	m := decodeBody[ComposedManifest](t, post(t, api+"/manifests", ManifestRequest{
		ConfigYAML: "processors:\n  transform:\n", Components: []string{"exporters/debug", "receivers/mystery"},
	}), http.StatusOK)
	if !strings.Contains(m.Manifest, "transformprocessor") || !strings.Contains(m.Manifest, "debugexporter") || len(m.Unknown) != 1 {
		t.Fatalf("manifest = %+v", m)
	}
	decodeBody[OperationError](t, post(t, api+"/manifests", ManifestRequest{Preset: "nope"}), http.StatusBadRequest)

	// A custom source's components are added to the manifest.
	stamp, err := filepath.Abs(filepath.Join("..", "..", "..", "testdata", "runtimes", "stampprocessor"))
	if err != nil {
		t.Fatal(err)
	}
	const stampModule = "example.com/linea/stampprocessor"
	m = decodeBody[ComposedManifest](t, post(t, api+"/manifests", ManifestRequest{
		Components: []string{"receivers/otlp"}, Sources: []RuntimeSource{{Module: stampModule, Dir: stamp}},
	}), http.StatusOK)
	if !strings.Contains(m.Manifest, "gomod: "+stampModule+" v0.0.0") || len(m.Custom) != 1 || m.Custom[0] != "processors/"+stampModule {
		t.Fatalf("manifest with a source = %+v", m)
	}
}

// TestRuntimeStorageRoutesMapPlatformOperations reports and prunes runtime
// storage over HTTP: the store's runtimes speak no worker protocol,
// so they are stale and only a stale prune removes them.
func TestRuntimeStorageRoutesMapPlatformOperations(t *testing.T) {
	store := &memoryRuntimes{images: map[string]platform.RuntimeInfo{
		"old": {Name: "old", ImageID: "sha256:old", Tags: []string{"linea-runtime:old"}, Size: 1000},
	}}
	service := platform.New(platform.Options{Runtimes: store})
	t.Cleanup(service.Close)
	_, srv := newServerOptions(t, Options{Platform: service})
	api := srv.URL + "/api/v1"

	resp, err := http.Get(api + "/runtime-storage")
	if err != nil {
		t.Fatal(err)
	}
	st := decodeBody[RuntimeStorage](t, resp, http.StatusOK)
	if len(st.Runtimes) != 1 || st.Runtimes[0].State != "stale" || st.Runtimes[0].Size != 1000 || st.Reclaimable != 1000 ||
		st.Caches == nil || st.Leftovers == nil {
		t.Fatalf("storage = %+v", st)
	}
	if res := decodeBody[RuntimePruneResult](t, post(t, api+"/runtime-storage/prune", RuntimePruneRequest{Replaced: true}), http.StatusOK); len(res.Removed) != 0 || res.Kept == nil {
		t.Fatalf("replaced prune = %+v", res)
	}
	res := decodeBody[RuntimePruneResult](t, post(t, api+"/runtime-storage/prune", RuntimePruneRequest{Stale: true}), http.StatusOK)
	if len(res.Removed) != 1 || res.Removed[0].ImageID != "sha256:old" || res.Reclaimed != 1000 {
		t.Fatalf("stale prune = %+v", res)
	}

	none := platform.New(platform.Options{})
	t.Cleanup(none.Close)
	_, bare := newServerOptions(t, Options{Platform: none})
	resp, err = http.Get(bare.URL + "/api/v1/runtime-storage")
	if err != nil {
		t.Fatal(err)
	}
	decodeBody[OperationError](t, resp, http.StatusServiceUnavailable)
}

// TestDefaultRuntimeRoutesMapPlatformOperations reads the default runtime
// before a choice, chooses a runtime of the catalog over HTTP, marks it in
// the runtime list, and keeps it from being removed.
func TestDefaultRuntimeRoutesMapPlatformOperations(t *testing.T) {
	_, _, srv := workspaceServer(t, memory.NewData())
	api := srv.URL + "/api/v1"

	resp, err := http.Get(api + "/runtime-default")
	if err != nil {
		t.Fatal(err)
	}
	if d := decodeBody[RuntimeDefault](t, resp, http.StatusOK); d.Name != platform.BuiltInRuntime || d.SetBy != "" || d.Set != nil {
		t.Fatalf("default before a choice = %+v", d)
	}
	decodeBody[OperationError](t, put(t, api+"/runtime-default", RuntimeDefaultRequest{Name: "edge"}), http.StatusNotFound)
	decodeBody[OperationError](t, put(t, api+"/runtime-default", RuntimeDefaultRequest{Name: "Edge!"}), http.StatusBadRequest)
	if d := decodeBody[RuntimeDefault](t, put(t, api+"/runtime-default", RuntimeDefaultRequest{Name: platform.BuiltInRuntime}), http.StatusOK); d.Name != platform.BuiltInRuntime || d.SetBy == "" || d.Set == nil {
		t.Fatalf("built-in chosen = %+v", d)
	}

	hello, err := json.Marshal(worker.Hello{Protocol: worker.ProtocolVersion, Runtime: "sha256:" + strings.Repeat("e", 64), CollectorVersion: "0.162.0"})
	if err != nil {
		t.Fatal(err)
	}
	decodeBody[Runtime](t, post(t, api+"/runtimes/register", RuntimeRegisterRequest{Name: "edge", Image: "registry.example/linea-runtime:edge", Hello: hello}), http.StatusOK)
	if d := decodeBody[RuntimeDefault](t, put(t, api+"/runtime-default", RuntimeDefaultRequest{Name: "edge"}), http.StatusOK); d.Name != "edge" || d.SetBy == "" || d.Set == nil {
		t.Fatalf("edge chosen = %+v", d)
	}
	resp, err = http.Get(api + "/runtime-default")
	if err != nil {
		t.Fatal(err)
	}
	if d := decodeBody[RuntimeDefault](t, resp, http.StatusOK); d.Name != "edge" {
		t.Fatalf("default after a choice = %+v", d)
	}
	resp, err = http.Get(api + "/runtimes/edge")
	if err != nil {
		t.Fatal(err)
	}
	if rt := decodeBody[Runtime](t, resp, http.StatusOK); !rt.Default {
		t.Fatalf("edge = %+v", rt)
	}
	if e := decodeBody[OperationError](t, del(t, api+"/runtimes/edge"), http.StatusConflict); e.Code != string(platform.CodeConflict) {
		t.Fatalf("removing the default runtime = %+v", e)
	}
}

func put(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
