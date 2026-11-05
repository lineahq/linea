package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/lineahq/linea/internal/debugger/engine/local"
)

func init() { Use(local.Collector) }

// TestWithoutACollectorOnlyRuntimeWorkPrepares is the team server's case: a
// process without an in-process Collector prepares and encodes a request
// for a runtime and refuses the built-in runtime's work.
func TestWithoutACollectorOnlyRuntimeWorkPrepares(t *testing.T) {
	prev := inProcess.Load()
	Use(nil)
	t.Cleanup(func() { inProcess.Store(prev) })
	ctx := context.Background()
	fixture := `{"resourceSpans":[]}`

	if _, err := Prepare(ctx, Request{ConfigYAML: captureConfig, FixtureJSON: fixture}); !errors.Is(err, ErrNoCollector) {
		t.Fatalf("built-in preparation = %v", err)
	}
	if _, err := Execute(ctx, Request{ConfigYAML: captureConfig, FixtureJSON: fixture}); !errors.Is(err, ErrNoCollector) {
		t.Fatalf("execution = %v", err)
	}
	req, err := Prepare(ctx, Request{ConfigYAML: captureConfig, FixtureJSON: fixture, Compare: true,
		Runtime: &RuntimeTarget{ID: "sha256:aaaa", Name: "custom"}})
	if err != nil {
		t.Fatalf("runtime preparation: %v", err)
	}
	if _, _, err := EncodeBundle(req); err != nil {
		t.Fatalf("runtime bundle: %v", err)
	}
	if rt := BuiltInRuntime(); rt.CollectorVersion == "" || len(rt.FeatureGates) != 0 || len(rt.Providers) != 0 {
		t.Fatalf("built-in runtime without a Collector = %+v", rt)
	}
}
