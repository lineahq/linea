package platform

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/store/memory"
	"github.com/lineahq/linea/internal/debugger/worker"
)

// TestTheTeamChoosesTheDefaultRuntime: a default chosen on one instance is
// every instance's default, survives a restart, is audited with the
// previous and new default, and keeps its runtime from being removed.
func TestTheTeamChoosesTheDefaultRuntime(t *testing.T) {
	data := memory.NewData()
	opts := Options{Shared: true, Paths: PathPolicy{Unrestricted: true}, WorkerToken: poolToken, BuiltInOnWorkers: true}
	a := openPlatform(t, data, opts)
	b := openPlatform(t, data, opts)
	ctx := context.Background()

	if d, err := b.GetDefaultRuntime(ctx, testCaller); err != nil || d.Name != BuiltInRuntime || d.Set != nil || d.SetBy != "" {
		t.Fatalf("default before one was chosen = %+v, %v", d, err)
	}
	hello := rawHello(t, worker.Hello{Protocol: worker.ProtocolVersion, Runtime: "sha256:" + strings.Repeat("e", 64), CollectorVersion: "0.160.0"})
	if _, err := a.RegisterRuntime(ctx, testCaller, RegisterRuntimeRequest{Name: "edge", Image: "registry.example/linea-runtime:edge", Hello: hello}); err != nil {
		t.Fatal(err)
	}
	for name, code := range map[string]Code{"gateway": CodeNotFound, "Edge!": CodeInvalidRequest, "": CodeInvalidRequest} {
		if _, err := a.SetDefaultRuntime(ctx, testCaller, name); ErrorCode(err) != code {
			t.Errorf("default %q = %v; want %s", name, err, code)
		}
	}
	d, err := a.SetDefaultRuntime(ctx, testCaller, "edge")
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "edge" || d.Set == nil || d.SetBy != testCaller.Actor().Principal {
		t.Fatalf("chosen default = %+v", d)
	}

	// The other instance follows the change feed.
	if d, err := b.GetDefaultRuntime(ctx, testCaller); err != nil || d.Name != "edge" {
		t.Fatalf("default on the other instance = %+v, %v", d, err)
	}
	if got := b.runtimeName(""); got != "edge" {
		t.Errorf("runtime of a request that names none = %q", got)
	}
	if err := b.DeleteRuntime(ctx, testCaller, "edge"); ErrorCode(err) != CodeConflict || !strings.Contains(err.Error(), "linea runtime default") {
		t.Errorf("removing the default runtime = %v", err)
	}
	list, err := b.ListRuntimes(ctx, testCaller)
	if err != nil {
		t.Fatal(err)
	}
	var defaults []string
	for _, rt := range list {
		if rt.Default {
			defaults = append(defaults, rt.Name)
		}
	}
	if !slices.Equal(defaults, []string{"edge"}) {
		t.Errorf("listed defaults = %v", defaults)
	}
	if info, err := b.GetRuntime(ctx, testCaller, "edge"); err != nil || !info.Default {
		t.Errorf("edge = %+v, %v", info, err)
	}

	page, err := a.ListAuditEvents(ctx, testCaller, AuditFilter{Operation: "SetDefaultRuntime"})
	if err != nil {
		t.Fatal(err)
	}
	var resources [][]string
	for _, e := range page.Events {
		if e.Code == "" {
			resources = append(resources, e.Resources)
		}
	}
	if len(resources) != 1 || !slices.Equal(resources[0], []string{BuiltInRuntime, "edge"}) {
		t.Errorf("audited changes = %v", resources)
	}

	// A restarted instance reads the chosen default.
	c := openPlatform(t, data, opts)
	if d, err := c.GetDefaultRuntime(ctx, testCaller); err != nil || d.Name != "edge" {
		t.Fatalf("default after a restart = %+v, %v", d, err)
	}
	if _, err := c.SetDefaultRuntime(ctx, testCaller, BuiltInRuntime); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteRuntime(ctx, testCaller, "edge"); err != nil {
		t.Errorf("removing a runtime that is no longer the default = %v", err)
	}
	until(t, func() bool { return a.runtimeName("") == BuiltInRuntime })
}
