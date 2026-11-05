package platform

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/store/memory"
	"github.com/lineahq/linea/internal/debugger/worker"
)

// TestAWorkerOfThisReleaseRegistersTheBuiltInRuntime runs requests that
// name no runtime, or the built-in one, on a team server's connected
// worker of its own release, which nobody registered, and
// removes the entry of another release no worker serves.
func TestAWorkerOfThisReleaseRegistersTheBuiltInRuntime(t *testing.T) {
	s := New(Options{Paths: PathPolicy{Unrestricted: true}, WorkerToken: poolToken, DefaultRuntime: BuiltInRuntime, BuiltInOnWorkers: true})
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	inspect := InspectRequest{Config: Source{Path: logsCleanup("config.yaml")}}
	if _, err := s.InspectConfiguration(ctx, testCaller, inspect); ErrorCode(err) != CodeUnavailable || !strings.Contains(err.Error(), "no worker of this release") {
		t.Fatalf("inspection before a worker connected = %v", err)
	}
	stale := artifact.RuntimeEntry{Name: builtInEntryName("0.0.1"), Identity: "sha256:" + strings.Repeat("b", 64),
		Hello: []byte(`{}`), RegisteredBy: "linea", Registered: time.Now()}
	if err := s.catalog.SaveRuntime(ctx, stale); err != nil {
		t.Fatal(err)
	}

	connectPoolUnregistered(t, s, "default", 2)
	var info *RuntimeInfo
	until(t, func() bool {
		var err error
		info, err = s.GetRuntime(ctx, testCaller, BuiltInRuntime)
		return err == nil
	})
	own, err := worker.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if c := info.Catalog; info.Name != BuiltInRuntime || c == nil || c.Identity != own.Runtime || c.Workers != 1 || c.Confirmed.IsZero() {
		t.Fatalf("built-in runtime = %+v, %+v", info, info.Catalog)
	}
	if _, err := s.catalog.Runtime(ctx, stale.Name); !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("another release's entry = %v", err)
	}
	list, err := s.ListRuntimes(ctx, testCaller)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, rt := range list {
		names = append(names, rt.Name)
	}
	if strings.Join(names, ",") != BuiltInRuntime {
		t.Fatalf("listed runtimes = %v", names)
	}

	quick := RunRequest{Mode: ModeQuick, Config: Source{Path: logsCleanup("config.yaml")}, Quick: &QuickInput{Fixture: Source{Path: logsCleanup("input.json")}}}
	for _, name := range []string{"", BuiltInRuntime} {
		quick.Runtime = name
		rep, err := s.ExecuteRun(ctx, testCaller, quick)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Runtime == nil || rep.Runtime.Name != BuiltInRuntime || rep.Runtime.Kind != engine.RuntimeBuiltIn {
			t.Fatalf("runtime %q: report runtime = %+v", name, rep.Runtime)
		}
		ins, err := s.InspectConfiguration(ctx, testCaller, InspectRequest{Config: inspect.Config, Runtime: name})
		if err != nil {
			t.Fatal(err)
		}
		if ins.Runtime == nil || ins.Runtime.Kind != engine.RuntimeBuiltIn || ins.Runtime.CollectorVersion != rep.Runtime.CollectorVersion {
			t.Fatalf("runtime %q: inspection runtime = %+v", name, ins.Runtime)
		}
	}
	// The entry's catalog name is no runtime a request selects.
	if _, err := s.InspectConfiguration(ctx, testCaller, InspectRequest{Config: inspect.Config, Runtime: ownBuiltInEntry()}); err == nil {
		t.Fatal("a request selected the built-in entry's catalog name")
	}
}

// TestOnlyAWorkerOfThisReleaseRegistersTheBuiltInRuntime leaves the
// catalog alone for a worker of another release, a runtime build's worker,
// and on a server that runs the built-in runtime itself.
func TestOnlyAWorkerOfThisReleaseRegistersTheBuiltInRuntime(t *testing.T) {
	own, err := worker.Identity()
	if err != nil {
		t.Fatal(err)
	}
	other := own
	other.ProductVersion = "other"
	custom := own
	custom.BuiltIn = false
	for name, tc := range map[string]struct {
		onWorkers bool
		hello     worker.Hello
	}{
		"other release":                      {true, other},
		"runtime build":                      {true, custom},
		"pool worker that cannot run itself": {true, func() worker.Hello { h := own; h.Pool = &worker.PoolHello{OS: "linux", Arch: "amd64"}; return h }()},
		"in this process":                    {false, own},
	} {
		s := New(Options{Paths: PathPolicy{Unrestricted: true}, WorkerToken: poolToken, BuiltInOnWorkers: tc.onWorkers})
		s.registerBuiltIn(tc.hello)
		entries, err := s.catalog.RuntimeEntries(context.Background())
		s.Close()
		if err != nil || len(entries) != 0 {
			t.Errorf("%s: catalog = %+v, %v", name, entries, err)
		}
	}
}

// TestAPoolRunsTheBuiltInRuntimeAsItsOwnBuild shows that a team
// server whose only worker is a pool registers the built-in runtime from
// the pool's own build, runs requests that name no runtime on it without
// fetching a binary, and counts the pool among its workers.
func TestAPoolRunsTheBuiltInRuntimeAsItsOwnBuild(t *testing.T) {
	// A team server stores runtime binaries; it takes no pool otherwise.
	s, err := Open(newObjectRepository(t, memory.NewData()), Options{Paths: PathPolicy{Unrestricted: true}, WorkerToken: poolToken,
		DefaultRuntime: BuiltInRuntime, BuiltInOnWorkers: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	servePool(t, s)

	var info *RuntimeInfo
	until(t, func() bool {
		var err error
		info, err = s.GetRuntime(ctx, testCaller, BuiltInRuntime)
		return err == nil
	})
	own, err := worker.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if c := info.Catalog; c == nil || c.Identity != own.Runtime || c.Workers != 1 || c.Slots != 2 {
		t.Fatalf("built-in runtime = %+v, %+v", info, info.Catalog)
	}

	quick := RunRequest{Mode: ModeQuick, Config: Source{Path: logsCleanup("config.yaml")}, Quick: &QuickInput{Fixture: Source{Path: logsCleanup("input.json")}}}
	rep, err := s.ExecuteRun(ctx, testCaller, quick)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Runtime == nil || rep.Runtime.Name != BuiltInRuntime || rep.Runtime.Kind != engine.RuntimeBuiltIn {
		t.Fatalf("report runtime = %+v", rep.Runtime)
	}
	h := submitPool(t, s, quick)
	if h.binary != nil {
		t.Fatalf("a run on the built-in runtime was pinned to %+v", h.binary)
	}
	if v := waitPoolRun(t, h); v.State != StateDone {
		t.Fatalf("queued run = %s, %v", v.State, h.Err())
	}
	ins, err := s.InspectConfiguration(ctx, testCaller, InspectRequest{Config: quick.Config})
	if err != nil || !ins.Valid || ins.Runtime == nil || ins.Runtime.Kind != engine.RuntimeBuiltIn {
		t.Fatalf("inspection = %+v, %v", ins, err)
	}
}
