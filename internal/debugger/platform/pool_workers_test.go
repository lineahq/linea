package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/store/memory"
	"github.com/lineahq/linea/internal/debugger/worker"
)

// poolChild makes the test binary a worker: a pool worker starts it as the
// stored binary of a runtime built on the server, and TestMain serves the
// protocol instead of running tests. A pool strips LINEA_* variables from
// its children's environment, so the switch is named otherwise.
const poolChild = "PLATFORM_TEST_POOL_CHILD"

func TestMain(m *testing.M) {
	if os.Getenv(poolChild) == "serve" {
		if err := worker.Serve(context.Background(), os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// RuntimeBinaryURL serves a stored binary, from memory or from disk.
func (o *objectRepository) RuntimeBinaryURL(_ context.Context, digest string, _ time.Duration) (artifact.ObjectURL, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	path := "/binaries/" + digest
	if b, ok := o.blobs[digest]; ok {
		o.objects[path] = b
		return artifact.ObjectURL{URL: o.srv.URL + path, Digest: digest, Size: int64(len(b))}, nil
	}
	if f, ok := o.files[path]; ok {
		st, err := os.Stat(f)
		if err != nil {
			return artifact.ObjectURL{}, err
		}
		return artifact.ObjectURL{URL: o.srv.URL + path, Digest: digest, Size: st.Size()}, nil
	}
	return artifact.ObjectURL{}, artifact.ErrNotFound
}

// poolPlatform is a team server with object storage that takes workers
// with poolToken, a runtime "edge" built on the server whose stored binary
// is this test binary, and a pool worker of this platform connected to it.
func poolPlatform(t *testing.T) (*Service, artifact.RuntimeBinary) {
	t.Helper()
	return poolPlatformOn(t, memory.NewData(), false)
}

// poolPlatformOn is poolPlatform on data, shared with other instances when
// shared is set.
func poolPlatformOn(t *testing.T, data *memory.Data, shared bool) (*Service, artifact.RuntimeBinary) {
	t.Helper()
	ctx := context.Background()
	access, err := NewPolicy([]Binding{{Subject: Subject{Principal: testCaller.Principal.ID}, Role: "admin", Scope: ScopeAll}})
	if err != nil {
		t.Fatal(err)
	}
	repo := newObjectRepository(t, data)
	s, err := Open(repo, Options{Access: access, Paths: PathPolicy{Unrestricted: true}, WorkerToken: poolToken, Shared: shared})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.New()
	size, err := io.Copy(sum, f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	binary := artifact.RuntimeBinary{Digest: "sha256:" + hex.EncodeToString(sum.Sum(nil)), Bytes: size, OS: runtime.GOOS, Arch: runtime.GOARCH}
	repo.mu.Lock()
	repo.files["/binaries/"+binary.Digest] = exe
	repo.mu.Unlock()
	hello, err := worker.Identity()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(hello)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.catalog.SaveRuntime(ctx, artifact.RuntimeEntry{Name: "edge", Identity: hello.Runtime, Build: "build-edge", Hello: raw,
		Binary: &binary, RegisteredBy: "user:alice", Registered: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	servePool(t, s)
	return s, binary
}

// servePool connects a pool worker of this platform with two slots to s.
// Its own build is this test binary, which serves as a worker.
func servePool(t *testing.T, s *Service) {
	t.Helper()
	ctx := context.Background()
	t.Setenv(poolChild, "serve")
	cache := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(s.ConnectWorker))
	t.Cleanup(srv.Close)
	poolCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- worker.ServePool(poolCtx, worker.PoolOptions{URL: srv.URL, Token: poolToken, Name: "pool", Capacity: 2, Warm: 1, CacheDir: cache})
	}()
	// Registered after the cache, so the pool stops its children before
	// the cache is removed.
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("ServePool = %v", err)
		}
	})
	until(t, func() bool { return s.connectedSlots("pool") == 2 })
}

// TestAPoolWorkerRunsARuntimeBuiltOnTheServer covers the
// platform: a run on a runtime built on the server is pinned to its binary
// at admission, and a pool worker of the binary's platform, with nothing
// registered for it, fetches the binary, starts it and runs the run, with
// the report a run on the built-in runtime gives. Inspections run there
// too, and the catalog counts the pool among the runtime's workers.
func TestAPoolWorkerRunsARuntimeBuiltOnTheServer(t *testing.T) {
	s, binary := poolPlatform(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	infos, err := s.catalogRuntimes(ctx)
	if err != nil || len(infos) != 1 || infos[0].Catalog.Workers != 1 || infos[0].Catalog.Slots != 2 {
		t.Fatalf("catalog = %+v, %v", infos, err)
	}

	req := RunRequest{Mode: ModeQuick, Config: Source{Path: logsCleanup("config.yaml")}, Quick: &QuickInput{Fixture: Source{Path: logsCleanup("input.json")}}, Normalize: true}
	builtIn, err := s.ExecuteRun(ctx, testCaller, req)
	if err != nil {
		t.Fatal(err)
	}
	req.Runtime = "edge"
	h := submitPool(t, s, req)
	if b := h.binary; b == nil || b.Digest != binary.Digest || b.Bytes != binary.Bytes || b.Platform != binary.OS+"/"+binary.Arch {
		t.Fatalf("pinned binary = %+v", b)
	}
	v := waitPoolRun(t, h)
	if v.State != StateDone {
		t.Fatalf("run = %s, %v", v.State, h.Err())
	}
	rep := h.View(true).Report
	if rep.Runtime == nil || rep.Runtime.Name != "edge" {
		t.Fatalf("runtime = %+v", rep.Runtime)
	}
	a, b := *rep, *builtIn
	a.Runtime, b.Runtime = nil, nil
	if ja, jb := mustJSON(t, a), mustJSON(t, b); ja != jb {
		t.Fatal("the pool's report differs from the built-in run's beyond its runtime")
	}

	ins, err := s.InspectConfiguration(ctx, testCaller, InspectRequest{Config: Source{Path: logsCleanup("config.yaml")}, Runtime: "edge"})
	if err != nil || !ins.Valid {
		t.Fatalf("inspection = %+v, %v", ins, err)
	}
}

// TestARunPinnedToABinaryWaitsForAPoolOfItsPlatform: a pool worker of
// another platform claims none of the runs pinned to a binary.
func TestARunPinnedToABinaryWaitsForAPoolOfItsPlatform(t *testing.T) {
	e := &executor{conn: &connectedWorker{hello: worker.Hello{Runtime: "sha256:" + hex.EncodeToString(make([]byte, 32)),
		Pool: &worker.PoolHello{OS: "linux", Arch: "arm64", Warm: 1}}}}
	target := engine.RuntimeTarget{ID: "sha256:" + hex.EncodeToString(make([]byte, 32))}
	for _, tc := range []struct {
		binary *artifact.RunBinary
		want   bool
	}{
		{nil, false},
		{&artifact.RunBinary{Platform: "linux/amd64"}, false},
		{&artifact.RunBinary{Platform: "linux/arm64"}, true},
	} {
		if got := e.runs(target, tc.binary); got != tc.want {
			t.Errorf("runs(%+v) = %v, want %v", tc.binary, got, tc.want)
		}
	}
	if !e.serves(true, "sha256:other") || e.serves(false, "builtin") {
		t.Error("a pool serves every pooled runtime and no built-in one")
	}
	if e.presence() != "pool:linux/arm64" {
		t.Errorf("presence = %q", e.presence())
	}
}

// TestInstancesSeeEachOthersWarmPools: a pool's presence lists the runtimes
// it holds running once a run started one, and another instance reads them,
// while the pool's own instance does not count them as elsewhere.
func TestInstancesSeeEachOthersWarmPools(t *testing.T) {
	data := memory.NewData()
	a, _ := poolPlatformOn(t, data, true)
	b := openPlatform(t, data, Options{Shared: true})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	entry, err := a.catalog.Runtime(ctx, "edge")
	if err != nil {
		t.Fatal(err)
	}
	if warm := b.warmElsewhere(time.Now()); warm[entry.Identity] {
		t.Fatal("the pool holds the runtime warm before any run")
	}
	h := submitPool(t, a, RunRequest{Mode: ModeQuick, Config: Source{Path: logsCleanup("config.yaml")},
		Quick: &QuickInput{Fixture: Source{Path: logsCleanup("input.json")}}, Runtime: "edge"})
	if v := waitPoolRun(t, h); v.State != StateDone {
		t.Fatalf("run = %s, %v", v.State, h.Err())
	}
	at := time.Now()
	until(t, func() bool {
		// Each read is a second later, past the cached one.
		at = at.Add(warmReadEvery)
		return b.warmElsewhere(at)[entry.Identity]
	})
	if a.warmElsewhere(at.Add(warmReadEvery))[entry.Identity] {
		t.Error("an instance counts its own pool as another instance's")
	}
}

// TestAColdPoolLeavesARuntimeToAWarmPoolForTheGrace: only a pool that would
// have to start the runtime defers, and only until poolGrace after the
// oldest run queued.
func TestAColdPoolLeavesARuntimeToAWarmPoolForTheGrace(t *testing.T) {
	queued := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		rank  int
		after time.Duration
		want  bool
	}{
		{"worker of the runtime", 0, 0, false},
		{"pool holding it", 1, 0, false},
		{"pool with room", 2, 0, true},
		{"full pool", 3, time.Second, true},
		{"grace over", 2, poolGrace, false},
		{"long after", 3, time.Minute, false},
	} {
		until, ok := graceUntil(tc.rank, queued, queued.Add(tc.after))
		if ok != tc.want || !until.Equal(queued.Add(poolGrace)) {
			t.Errorf("%s: graceUntil = %v, %v", tc.name, until, ok)
		}
	}
}

// TestAPoolsSuspectBinaryFailsItsRunsUntilItIsRegisteredAgain: a pool that
// downloads a binary other than the one assigned fails the run, the server
// records the binary as suspect on its catalog entry (pool and cause, no
// URL), refuses further runs on it at once, and registering the runtime
// again clears the mark.
func TestAPoolsSuspectBinaryFailsItsRunsUntilItIsRegisteredAgain(t *testing.T) {
	s, binary := poolPlatform(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	repo := s.repository.(*objectRepository)
	wrong := filepath.Join(t.TempDir(), "wrong")
	if err := os.WriteFile(wrong, []byte("not the runtime binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo.mu.Lock()
	repo.files["/binaries/"+binary.Digest] = wrong
	repo.mu.Unlock()

	req := RunRequest{Mode: ModeQuick, Config: Source{Path: logsCleanup("config.yaml")},
		Quick: &QuickInput{Fixture: Source{Path: logsCleanup("input.json")}}, Runtime: "edge"}
	h := submitPool(t, s, req)
	if v := waitPoolRun(t, h); v.State != StateError || !errors.Is(h.Err(), engine.ErrRuntimeUnavailable) || !errors.Is(h.Err(), worker.ErrSuspectBinary) {
		t.Fatalf("run = %s, %v; want runtime unavailable", v.State, h.Err())
	}
	entry, err := s.catalog.Runtime(ctx, "edge")
	if err != nil {
		t.Fatal(err)
	}
	m := entry.Suspect
	if m == nil || m.Pool != artifact.PoolIdentity(runtime.GOOS+"/"+runtime.GOARCH) || !strings.Contains(m.Cause, "not the one assigned") ||
		strings.Contains(m.Cause, "http") || m.At.IsZero() {
		t.Fatalf("suspicion = %+v", m)
	}
	infos, err := s.catalogRuntimes(ctx)
	if err != nil || len(infos) != 1 || infos[0].Catalog.Suspect == nil || infos[0].Catalog.Suspect.Cause != m.Cause {
		t.Fatalf("catalog = %+v, %v", infos, err)
	}

	_, err = s.SubmitRun(ctx, testCaller, req)
	if ErrorCode(err) != CodeUnavailable || !strings.Contains(err.Error(), "found its binary wrong") {
		t.Fatalf("a run on the suspect runtime = %v; want it refused at admission", err)
	}
	// A run pinned before the mark fails before it reaches a pool.
	if _, err := s.poolBinary(ctx, engine.RuntimeTarget{ID: entry.Identity, Name: "edge"}, &artifact.RunBinary{Digest: binary.Digest, Bytes: binary.Bytes,
		Platform: binary.OS + "/" + binary.Arch}); ErrorCode(err) != CodeUnavailable {
		t.Fatalf("poolBinary = %v", err)
	}

	entry.Suspect = nil
	if err := s.catalog.SaveRuntime(ctx, entry); err != nil {
		t.Fatal(err)
	}
	if got, err := s.catalog.Runtime(ctx, "edge"); err != nil || got.Suspect != nil {
		t.Fatalf("registered again: %+v, %v", got.Suspect, err)
	}
}

func TestASuspicionRecordsNoURL(t *testing.T) {
	err := fmt.Errorf("fetch https://store.example/binaries/x?X-Amz-Signature=secret failed\nagain")
	got := suspicionCause(err)
	if strings.Contains(got, "secret") || strings.Contains(got, "\n") || !strings.Contains(got, "<url>") {
		t.Fatalf("cause = %q", got)
	}
	long := suspicionCause(errors.New(strings.Repeat("é", artifact.MaxSuspicionCause)))
	if len(long) > artifact.MaxSuspicionCause || !utf8.ValidString(long) {
		t.Fatalf("long cause: %d bytes, valid %v", len(long), utf8.ValidString(long))
	}
}
