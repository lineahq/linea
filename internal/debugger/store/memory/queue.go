package memory

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

// queueEntry is an unfinished admitted run's scheduling state; the entry
// leaves the queue when the run turns terminal. generation mirrors the run
// record's.
type queueEntry struct {
	runtime    string
	priority   int
	ordinal    uint64
	generation uint64
	state      artifact.QueueState
	worker     string
	attempt    int
	released   int
	expires    time.Time
	cancel     bool
	binary     *artifact.RunBinary
}

// entries is the queue logic both Store and Queue share; callers hold the
// lock that guards it.
type entries map[string]*queueEntry

func (q entries) assignment(id string) artifact.Assignment {
	e := q[id]
	a := artifact.Assignment{RunID: id, Runtime: e.runtime, Worker: e.worker, State: e.state, Attempt: e.attempt,
		Released: e.released, Expires: e.expires, CancelRequested: e.cancel, Generation: e.generation}
	if e.binary != nil {
		b := *e.binary
		a.Binary = &b
	}
	return a
}

func validWorker(worker string) bool { return worker != "" && len(worker) <= 256 }

// validPlatform reports whether p is an "os/arch" a run's binary can name.
func validPlatform(p string) bool {
	return artifact.RunBinary{Digest: "sha256:" + strings.Repeat("0", 64), Bytes: 1, Platform: p}.Valid()
}

func (q entries) claim(worker, runtime, platform string, n int, expires time.Time) ([]artifact.Assignment, error) {
	if !validWorker(worker) || n < 0 || expires.IsZero() {
		return nil, invalid("invalid claim")
	}
	var ids []string
	for id, e := range q {
		if e.state == artifact.QueueQueued && e.runtime == runtime && (platform == "" || e.binary != nil && e.binary.Platform == platform) {
			ids = append(ids, id)
		}
	}
	slices.SortFunc(ids, func(x, y string) int {
		a, b := q[x], q[y]
		return cmp.Or(cmp.Compare(a.priority, b.priority), cmp.Compare(a.ordinal, b.ordinal))
	})
	out := []artifact.Assignment{}
	for _, id := range ids[:min(n, len(ids))] {
		e := q[id]
		e.state, e.worker, e.attempt, e.expires = artifact.QueueAssigned, worker, e.attempt+1, expires.UTC()
		out = append(out, q.assignment(id))
	}
	return out, nil
}

// held returns a's entry when the same worker and attempt still lease it.
func (q entries) held(a artifact.Assignment) *queueEntry {
	e := q[a.RunID]
	if e == nil || e.worker == "" || e.worker != a.Worker || e.attempt != a.Attempt {
		return nil
	}
	return e
}

func (q entries) start(a artifact.Assignment) error {
	e := q.held(a)
	if e == nil || e.state != artifact.QueueAssigned {
		return artifact.ErrConflict
	}
	e.state = artifact.QueueRunning
	return nil
}

func (q entries) renew(worker string, expires time.Time) ([]artifact.Assignment, error) {
	if !validWorker(worker) || expires.IsZero() {
		return nil, invalid("invalid renewal")
	}
	out := []artifact.Assignment{}
	for id, e := range q {
		if e.worker == worker {
			e.expires = expires.UTC()
			out = append(out, q.assignment(id))
		}
	}
	slices.SortFunc(out, func(x, y artifact.Assignment) int { return cmp.Compare(x.RunID, y.RunID) })
	return out, nil
}

// cancel marks an entry; one no worker holds leaves the queue.
func (q entries) cancel(id string) artifact.Assignment {
	e := q[id]
	e.cancel = true
	a := q.assignment(id)
	if e.worker == "" {
		a.State = ""
		delete(q, id)
	}
	return a
}

func (q entries) expired(now time.Time) []artifact.Assignment {
	out := []artifact.Assignment{}
	for id, e := range q {
		if e.worker != "" && e.expires.Before(now) {
			out = append(out, q.assignment(id))
		}
	}
	slices.SortFunc(out, func(x, y artifact.Assignment) int { return cmp.Compare(q[x.RunID].ordinal, q[y.RunID].ordinal) })
	return out
}

// requeue returns an expired assignment's entry to the queue and advances its
// generation.
func (q entries) requeue(a artifact.Assignment, now time.Time) (uint64, error) {
	e := q.held(a)
	if e == nil || !e.expires.Before(now) {
		return 0, artifact.ErrConflict
	}
	e.state, e.worker, e.expires = artifact.QueueQueued, "", time.Time{}
	e.generation++
	return e.generation, nil
}

func (q entries) release(a artifact.Assignment) (uint64, error) {
	e := q.held(a)
	if e == nil {
		return 0, artifact.ErrConflict
	}
	e.state, e.worker, e.expires = artifact.QueueQueued, "", time.Time{}
	e.released++
	e.generation++
	return e.generation, nil
}

func (q entries) get(id string) (artifact.Assignment, error) {
	if q[id] == nil {
		return artifact.Assignment{}, artifact.ErrNotFound
	}
	return q.assignment(id), nil
}

func (s *Store) queueReady(ctx context.Context) error {
	if err := s.ready(ctx.Err()); err != nil {
		return err
	}
	if s.d.runs == nil {
		return invalid("run journal is not enabled")
	}
	return nil
}

// Claim assigns up to n queued runs of runtime, lowest priority then
// admission order first.
func (s *Store) Claim(ctx context.Context, worker, runtime string, n int, expires time.Time) ([]artifact.Assignment, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.queueReady(ctx); err != nil {
		return nil, err
	}
	return d.queue.claim(worker, runtime, "", n, expires)
}

// ClaimBinary is Claim for a pool worker: it assigns only runs pinned to a
// binary of platform.
func (s *Store) ClaimBinary(ctx context.Context, worker, runtime, platform string, n int, expires time.Time) ([]artifact.Assignment, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.queueReady(ctx); err != nil {
		return nil, err
	}
	if !validPlatform(platform) {
		return nil, invalid("invalid claim")
	}
	return d.queue.claim(worker, runtime, platform, n, expires)
}

// Start moves an assignment the worker still holds to running.
func (s *Store) Start(ctx context.Context, a artifact.Assignment) error {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.queueReady(ctx); err != nil {
		return err
	}
	return d.queue.start(a)
}

// Renew extends the worker's leases.
func (s *Store) Renew(ctx context.Context, worker string, expires time.Time) ([]artifact.Assignment, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.queueReady(ctx); err != nil {
		return nil, err
	}
	return d.queue.renew(worker, expires)
}

// RequestCancel marks a run; one no worker holds leaves the queue.
func (s *Store) RequestCancel(ctx context.Context, id string) (artifact.Assignment, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.queueReady(ctx); err != nil {
		return artifact.Assignment{}, err
	}
	r, ok := d.runs.Records[id]
	if !ok {
		return artifact.Assignment{}, artifact.ErrNotFound
	}
	if r.Terminal {
		return artifact.Assignment{}, artifact.ErrConflict
	}
	if d.queue[id] == nil {
		return artifact.Assignment{RunID: id, CancelRequested: true, Generation: r.Generation}, nil
	}
	a := d.queue.cancel(id)
	d.change(artifact.ChangeRun, id)
	return a, nil
}

// Expired lists leases that ended before now, in admission order.
func (s *Store) Expired(ctx context.Context, now time.Time) ([]artifact.Assignment, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.queueReady(ctx); err != nil {
		return nil, err
	}
	return d.queue.expired(now), nil
}

// Requeue returns an expired assignment's run to the queue and advances its
// generation, so its former holder can no longer publish it.
func (s *Store) Requeue(ctx context.Context, a artifact.Assignment, now time.Time) (uint64, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.queueReady(ctx); err != nil {
		return 0, err
	}
	generation, err := d.queue.requeue(a, now)
	if err != nil {
		return 0, err
	}
	d.advance(a.RunID, generation)
	return generation, nil
}

// Release returns a held run to the queue and advances its generation.
func (s *Store) Release(ctx context.Context, a artifact.Assignment) (uint64, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.queueReady(ctx); err != nil {
		return 0, err
	}
	generation, err := d.queue.release(a)
	if err != nil {
		return 0, err
	}
	d.advance(a.RunID, generation)
	return generation, nil
}

// advance records a requeued or released run's new generation; the caller
// holds d.mu.
func (d *Data) advance(id string, generation uint64) {
	next := *d.runs
	next.Records = make(map[string]artifact.RunRecord, len(d.runs.Records))
	for key, value := range d.runs.Records {
		next.Records[key] = value
	}
	r := next.Records[id]
	r.Generation = generation
	next.Records[id] = r
	d.runs, d.runsSize = &next, encodedSize(next)
	d.change(artifact.ChangeRun, id)
}

// Assignment returns an unfinished run's queue entry.
func (s *Store) Assignment(ctx context.Context, id string) (artifact.Assignment, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.queueReady(ctx); err != nil {
		return artifact.Assignment{}, err
	}
	return d.queue.get(id)
}

// Queue is a run queue without a run journal, for a service that keeps no
// durable history. Admit and Remove stand in for a store's admission and
// terminal publication; an unknown or removed run is ErrNotFound.
type Queue struct {
	mu      sync.Mutex
	q       entries
	ordinal uint64
}

func NewQueue() *Queue { return &Queue{q: entries{}} }

// Admit queues a run at generation 1.
func (q *Queue) Admit(id, runtime string, priority int) error {
	return q.admit(id, runtime, priority, nil)
}

func (q *Queue) admit(id, runtime string, priority int, b *artifact.RunBinary) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.q[id] != nil {
		return artifact.ErrConflict
	}
	q.ordinal++
	q.q[id] = &queueEntry{runtime: runtime, priority: priority, ordinal: q.ordinal, generation: 1, state: artifact.QueueQueued, binary: b}
	return nil
}

// AdmitBinary is Admit for a run pinned to binary b.
func (q *Queue) AdmitBinary(id, runtime string, priority int, b artifact.RunBinary) error {
	if !b.Valid() {
		return invalid("invalid run binary")
	}
	return q.admit(id, runtime, priority, &b)
}

// Remove drops a run that ended.
func (q *Queue) Remove(id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.q, id)
}

func (q *Queue) Claim(ctx context.Context, worker, runtime string, n int, expires time.Time) ([]artifact.Assignment, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.q.claim(worker, runtime, "", n, expires)
}

func (q *Queue) ClaimBinary(ctx context.Context, worker, runtime, platform string, n int, expires time.Time) ([]artifact.Assignment, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !validPlatform(platform) {
		return nil, invalid("invalid claim")
	}
	return q.q.claim(worker, runtime, platform, n, expires)
}

func (q *Queue) Start(ctx context.Context, a artifact.Assignment) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.q.start(a)
}

func (q *Queue) Renew(ctx context.Context, worker string, expires time.Time) ([]artifact.Assignment, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.q.renew(worker, expires)
}

func (q *Queue) RequestCancel(ctx context.Context, id string) (artifact.Assignment, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.q[id] == nil {
		return artifact.Assignment{}, artifact.ErrNotFound
	}
	return q.q.cancel(id), nil
}

func (q *Queue) Expired(ctx context.Context, now time.Time) ([]artifact.Assignment, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.q.expired(now), nil
}

func (q *Queue) Requeue(ctx context.Context, a artifact.Assignment, now time.Time) (uint64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.q.requeue(a, now)
}

func (q *Queue) Release(ctx context.Context, a artifact.Assignment) (uint64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.q.release(a)
}

func (q *Queue) Assignment(ctx context.Context, id string) (artifact.Assignment, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.q.get(id)
}

var _ artifact.Queue = (*Queue)(nil)
