package livecapture

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/lineahq/linea/internal/fleetplugins/livecapture/translog"
	"github.com/lineahq/linea/pluginsdk"
)

// heldRepo lets a test hold the writer inside a write: hold.Lock() blocks the
// next write, and entered receives once per write that started.
type heldRepo struct {
	*memoryRepo
	hold           sync.RWMutex
	entered        chan struct{}
	saves, inserts atomic.Int32
}

func newHeldRepo() *heldRepo {
	return &heldRepo{memoryRepo: newMemoryRepo(), entered: make(chan struct{}, 64)}
}
func (h *heldRepo) wait() {
	h.entered <- struct{}{}
	h.hold.RLock()
	h.hold.RUnlock()
}
func (h *heldRepo) Save(ctx context.Context, op Operation, previous int64) error {
	h.wait()
	h.saves.Add(1)
	return h.memoryRepo.Save(ctx, op, previous)
}
func (h *heldRepo) Insert(ctx context.Context, op Operation) error {
	h.wait()
	h.inserts.Add(1)
	return h.memoryRepo.Insert(ctx, op)
}

// heldFixture is fixture with a heldRepo; it must be installed before the
// writer's first write.
func heldFixture(t *testing.T) (*Server, CreateRequest, *peer, *heldRepo) {
	t.Helper()
	s, request, p := fixture(t)
	repo := newHeldRepo()
	repo.memoryRepo = s.repo.(*memoryRepo)
	s.repo = repo
	return s, request, p, repo
}

func drain(c chan struct{}) {
	for {
		select {
		case <-c:
		default:
			return
		}
	}
}

// Saves are decided under the lock and written later, in order. A save that
// waits absorbs the next one; reads see the decided state at once; the log
// shows only what was written.
func TestSavesAreWrittenBehindTheLock(t *testing.T) {
	s, request, p, repo := heldFixture(t)
	log, rec := translog.NewRecorder()
	s.log = log
	op, err := s.Create(context.Background(), "owner", request)
	if err != nil {
		t.Fatal(err)
	}
	drain(repo.entered)
	wire := op.Targets[0].WireID
	repo.hold.Lock()
	s.mu.Lock()
	s.applyReportLocked(p, report{Statuses: []collectorStatus{{SessionID: wire, State: "active"}}})
	s.mu.Unlock()
	<-repo.entered // the writer holds the "active" save
	s.mu.Lock()
	revision := s.operations[op.ID].op.Revision
	s.mu.Unlock()
	stopped, err := s.Stop(context.Background(), "owner", op.ID, revision)
	if err != nil || stopped.State != "stopping" {
		t.Fatalf("stop under a held writer: %v, %v", stopped.State, err)
	}
	read, err := s.Get(context.Background(), "owner", op.ID)
	if err != nil || read.State != "stopping" || read.Revision != stopped.Revision {
		t.Fatalf("read %v rev %d, want the decided stopping rev %d: %v", read.State, read.Revision, stopped.Revision, err)
	}
	s.mu.Lock()
	s.applyReportLocked(p, report{Statuses: []collectorStatus{{SessionID: wire, State: "completed"}}})
	s.mu.Unlock()
	read, err = s.Get(context.Background(), "owner", op.ID)
	if err != nil || read.State != "stopped" {
		t.Fatalf("ended operation read as %v before its write: %v", read.State, err)
	}
	repo.hold.Unlock()
	s.saves.waitSettled()
	if n := repo.saves.Load(); n != 2 {
		t.Fatalf("%d writes, want 2: the stop and the end share one", n)
	}
	stored, err := repo.memoryRepo.Get(context.Background(), op.ID)
	if err != nil || stored.State != "stopped" || stored.Revision != read.Revision {
		t.Fatalf("stored %v rev %d, want stopped rev %d", stored.State, stored.Revision, read.Revision)
	}
	var steps []string
	for _, e := range logged(rec, "operation_state") {
		steps = append(steps, e.Text("from")+">"+e.Text("to"))
	}
	if want := "none>starting starting>active active>stopped"; joinSteps(steps) != want {
		t.Fatalf("logged %q, want %q", joinSteps(steps), want)
	}
}

func joinSteps(steps []string) string {
	out := ""
	for i, s := range steps {
		if i > 0 {
			out += " "
		}
		out += s
	}
	return out
}

// A released lease is written before the insert that takes the peer next, so
// the peer is free again as soon as the plugin decided so.
func TestInsertWaitsForTheReleaseBeforeIt(t *testing.T) {
	s, request, p, repo := heldFixture(t)
	op, err := s.Create(context.Background(), "owner", request)
	if err != nil {
		t.Fatal(err)
	}
	drain(repo.entered)
	repo.hold.Lock()
	wire := op.Targets[0].WireID
	s.mu.Lock()
	s.applyReportLocked(p, report{Statuses: []collectorStatus{{SessionID: wire, State: "completed"}}})
	s.mu.Unlock()
	<-repo.entered
	next := request
	next.ID = pluginsdk.RandomID()
	result := make(chan error, 1)
	go func() {
		_, err := s.Create(context.Background(), "owner", next)
		result <- err
	}()
	repo.hold.Unlock()
	if err := <-result; err != nil {
		t.Fatal("insert overtook the lease release before it:", err)
	}
}

// Two identical creates at once answer like a replay: one insert, one
// operation, no conflict.
func TestConcurrentIdenticalCreatesAgree(t *testing.T) {
	s, request, _, repo := heldFixture(t)
	repo.hold.Lock()
	type answer struct {
		op  Operation
		err error
	}
	answers := make(chan answer, 2)
	create := func() {
		op, err := s.Create(context.Background(), "owner", request)
		answers <- answer{op, err}
	}
	go create()
	<-repo.entered // the first insert is being written
	go create()
	repo.hold.Unlock()
	first, second := <-answers, <-answers
	if first.err != nil || second.err != nil || first.op.ID != second.op.ID || first.op.Revision != second.op.Revision {
		t.Fatalf("identical creates disagree: %v / %v", first.err, second.err)
	}
	if n := repo.inserts.Load(); n != 1 {
		t.Fatalf("%d inserts, want 1", n)
	}
}
