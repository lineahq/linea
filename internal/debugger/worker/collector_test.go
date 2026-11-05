package worker

import (
	"errors"
	"testing"

	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/engine/local"
)

// Tests execute on the built-in runtime in process.
func init() { engine.Use(local.Collector) }

// TestIdentityNeedsAnInProcessCollector: a process that links no Collector,
// such as the team server, cannot describe itself as a worker.
func TestIdentityNeedsAnInProcessCollector(t *testing.T) {
	engine.Use(nil)
	t.Cleanup(func() { engine.Use(local.Collector) })
	if _, err := Identity(); !errors.Is(err, engine.ErrNoCollector) {
		t.Fatalf("Identity() without a Collector = %v", err)
	}
}

// TestOnlyAReleasesOwnWorkerIsTheBuiltInRuntime: a runtime build's worker,
// stamped custom at link time, never says it is the built-in runtime, and
// the claim is no part of the runtime identity.
func TestOnlyAReleasesOwnWorkerIsTheBuiltInRuntime(t *testing.T) {
	own, err := Identity()
	if err != nil {
		t.Fatal(err)
	}
	custom = "true"
	t.Cleanup(func() { custom = "" })
	built, err := Identity()
	if err != nil {
		t.Fatal(err)
	}
	if !own.BuiltIn || built.BuiltIn {
		t.Fatalf("built-in: own %v, runtime build %v", own.BuiltIn, built.BuiltIn)
	}
	if own.Runtime != built.Runtime {
		t.Fatalf("the claim changed the runtime identity: %s, %s", own.Runtime, built.Runtime)
	}
}
