package platform

import (
	"context"
	"testing"

	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/engine/local"
)

// Tests execute on the built-in runtime in process.
func init() { engine.Use(local.Collector) }

// TestWithoutACollectorInProcessWorkIsUnavailable: a service in a process
// that links no Collector, as the team server, answers work on the
// built-in runtime in process as unavailable rather than running it.
func TestWithoutACollectorInProcessWorkIsUnavailable(t *testing.T) {
	engine.Use(nil)
	t.Cleanup(func() { engine.Use(local.Collector) })
	s := New(Options{Paths: PathPolicy{Unrestricted: true}})
	defer s.Close()
	ctx := context.Background()
	config := Source{Path: logsCleanup("config.yaml")}
	if _, err := s.InspectConfiguration(ctx, testCaller, InspectRequest{Config: config}); ErrorCode(err) != CodeUnavailable {
		t.Fatalf("inspection = %v (%s)", err, ErrorCode(err))
	}
	run := RunRequest{Mode: ModeQuick, Config: config, Quick: &QuickInput{Fixture: Source{Path: logsCleanup("input.json")}}}
	if _, err := s.ExecuteRun(ctx, testCaller, run); ErrorCode(err) != CodeUnavailable {
		t.Fatalf("run = %v (%s)", err, ErrorCode(err))
	}
}
