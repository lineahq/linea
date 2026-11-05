package mcp

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/platform"
)

var runtimeToolText = regexp.MustCompile(`^(Runtimes: [0-9]+ built runtimes?( and [0-9]+ remote runtimes?)?, [0-9]+ unverified; the built-in runtime is always available|Runtime build: state (queued|running|done|failed|cancelled)(, not finished within the wait)?; verified (true|false)(; reused an earlier build)?(; [0-9]+ manifest warnings?)?; [0-9]+ log lines? returned of [0-9]+|Manifest written: [0-9]+ custom components? from sources, [0-9]+ requested components? not in the catalog, [0-9]+ warnings?)\.$`)

// memoryRuntimes is a RuntimeStore without Docker: builds succeed at once
// and no runtime starts a worker.
type memoryRuntimes struct {
	mu     sync.Mutex
	images map[string]platform.RuntimeInfo
	// start, when set, starts a worker for a runtime.
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
	log("compiling " + spec.Name)
	log("built " + spec.Name)
	info := platform.RuntimeInfo{Name: spec.Name, ImageID: "sha256:" + spec.Name, CollectorVersion: "0.162.0", Verified: true, Manifest: string(spec.Manifest)}
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

func (m *memoryRuntimes) Delete(context.Context, *platform.RuntimeInfo) error { return nil }

func (m *memoryRuntimes) Push(context.Context, *platform.RuntimeInfo, string) error { return nil }

func (m *memoryRuntimes) Import(context.Context, string, bool, func(*platform.RuntimeInfo) error) (*platform.RuntimeInfo, error) {
	return nil, platform.ErrRuntimeNotFound
}

func (m *memoryRuntimes) Start(_ context.Context, info *platform.RuntimeInfo) (platform.RuntimeWorker, error) {
	if m.start != nil {
		return m.start(info)
	}
	return nil, errors.New("no workers here")
}

func textOf(t *testing.T, r *sdk.CallToolResult) string {
	t.Helper()
	text, ok := r.Content[0].(*sdk.TextContent)
	if !ok {
		t.Fatalf("content = %+v", r.Content)
	}
	return text.Text
}

// TestRuntimeToolsListRuntimes lists a built runtime; the text states counts
// only, never the name.
func TestRuntimeToolsListRuntimes(t *testing.T) {
	ctx, session, _ := newTestClient(t, platform.Options{Runtimes: &memoryRuntimes{images: map[string]platform.RuntimeInfo{"v0162": v0162}}})

	r, out := callTool(t, ctx, session, "linea_list_runtimes", map[string]any{})
	if list, _ := out["runtimes"].([]any); r.IsError || len(list) != 1 || textOf(t, r) != "Runtimes: 1 built runtime, 0 unverified; the built-in runtime is always available." {
		t.Fatalf("list = %s, %+v", textOf(t, r), out)
	}
}

// TestRuntimeToolsMapRefusals makes a run on a runtime that was never built
// unavailable.
func TestRuntimeToolsMapRefusals(t *testing.T) {
	ctx, session, _ := newTestClient(t, platform.Options{Runtimes: &memoryRuntimes{images: map[string]platform.RuntimeInfo{}}})

	r, out := callTool(t, ctx, session, "linea_validate_config", map[string]any{"config": map[string]any{"text": inspectionConfig}, "runtime": "gone"})
	if !r.IsError || out["code"] != string(platform.CodeUnavailable) {
		t.Fatalf("validation on a missing runtime = %+v", out)
	}
}

// TestRuntimeToolsWithoutAStoreAreUnavailable answers on a platform that
// cannot build runtimes.
func TestRuntimeToolsWithoutAStoreAreUnavailable(t *testing.T) {
	ctx, session, _ := newTestClient(t, platform.Options{})
	r, out := callTool(t, ctx, session, "linea_list_runtimes", map[string]any{})
	if !r.IsError || out["code"] != string(platform.CodeUnavailable) {
		t.Fatalf("list = %+v", out)
	}
}

// TestComposeManifestToolWritesFromTheCatalog writes a manifest from a
// configuration and a picked component, naming unknown ones only by count
// in the text, and adds a custom source's components.
func TestComposeManifestToolWritesFromTheCatalog(t *testing.T) {
	ctx, session, _ := newTestClient(t, platform.Options{Paths: platform.PathPolicy{Unrestricted: true}})
	r, out := callTool(t, ctx, session, "linea_compose_manifest", map[string]any{
		"configuration": map[string]any{"text": "processors:\n  transform:\n"}, "components": []string{"exporters/debug", "receivers/mystery"},
	})
	manifest, _ := out["manifest"].(string)
	if r.IsError || !strings.Contains(manifest, "transformprocessor") || !strings.Contains(manifest, "debugexporter") {
		t.Fatalf("manifest = %+v", out)
	}
	if text := textOf(t, r); text != "Manifest written: 0 custom components from sources, 1 requested component not in the catalog, 0 warnings." {
		t.Fatalf("text = %s", text)
	}
	// A source is a remote repository: the server reads no host directory.
	r, out = callTool(t, ctx, session, "linea_compose_manifest", map[string]any{
		"components": []string{"receivers/otlp"}, "sources": []map[string]any{{"module": "example.com/linea/stampprocessor", "dir": "stampprocessor"}},
	})
	if !r.IsError || out["code"] != string(platform.CodeInvalidRequest) {
		t.Fatalf("a source directory = %+v", out)
	}
	r, out = callTool(t, ctx, session, "linea_compose_manifest", map[string]any{"preset": "nope"})
	if !r.IsError || out["code"] != string(platform.CodeInvalidRequest) {
		t.Fatalf("unknown preset = %+v", out)
	}
}
