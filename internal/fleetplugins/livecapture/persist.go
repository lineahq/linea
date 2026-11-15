package livecapture

import (
	"context"
	"sync"
	"time"

	"github.com/lineahq/linea/internal/fleetplugins/livecapture/translog"
)

// Operation state is decided under s.mu and written outside it, by one writer
// goroutine, in decision order. One order for inserts and saves keeps a
// released lease ahead of the insert that takes the peer next. A save still
// waiting for the writer absorbs the next save of the same operation: the row
// then moves from the first save's revision to the latest snapshot in one
// write. Revisions only grow and released targets stay released, so writing a
// later snapshot at the earlier position is safe.
//
// A failed save ends the plugin, as a failed write under the lock did. A
// failed insert is the creator's error; the writer drops the saves that
// followed it.

// shutdownFlush bounds how long the writer keeps writing after the server's
// context ended.
const shutdownFlush = 2 * time.Second

type persistItem struct {
	op       Operation
	previous int64
	// done receives an insert's result; it is nil for a save.
	done chan error
	// from is the state logged before this write; the writer logs the
	// transition to op.State once the write succeeded.
	from translog.State
}

type saveQueue struct {
	mu    sync.Mutex
	items []*persistItem
	// saves holds the queued saves not yet taken, by operation ID.
	saves map[string]*persistItem
	// inflight is the item being written, still visible to lookups.
	inflight *persistItem
	wake     chan struct{}
	settled  *sync.Cond
}

func newSaveQueue() *saveQueue {
	q := &saveQueue{saves: map[string]*persistItem{}, wake: make(chan struct{}, 1)}
	q.settled = sync.NewCond(&q.mu)
	return q
}

func (q *saveQueue) push(item *persistItem) {
	q.mu.Lock()
	if item.done == nil {
		if queued := q.saves[item.op.ID]; queued != nil {
			// The queued save keeps its revision and its logged state.
			queued.op = item.op
			q.mu.Unlock()
			return
		}
		q.saves[item.op.ID] = item
	}
	q.items = append(q.items, item)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// take removes the next item and marks it in flight.
func (q *saveQueue) take() *persistItem {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return nil
	}
	item := q.items[0]
	q.items[0] = nil
	q.items = q.items[1:]
	if q.saves[item.op.ID] == item {
		delete(q.saves, item.op.ID)
	}
	q.inflight = item
	return item
}

func (q *saveQueue) finish() {
	q.mu.Lock()
	q.inflight = nil
	q.settled.Broadcast()
	q.mu.Unlock()
}

// latest is the newest decided snapshot of id that the writer has not
// finished writing.
func (q *saveQueue) latest(id string) (Operation, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if item := q.saves[id]; item != nil {
		return cloneOperation(item.op), true
	}
	for i := len(q.items) - 1; i >= 0; i-- {
		if q.items[i].op.ID == id {
			return cloneOperation(q.items[i].op), true
		}
	}
	if q.inflight != nil && q.inflight.op.ID == id {
		return cloneOperation(q.inflight.op), true
	}
	return Operation{}, false
}

// waitSettled blocks until the writer has written or dropped every item.
func (q *saveQueue) waitSettled() {
	q.mu.Lock()
	for len(q.items) > 0 || q.inflight != nil {
		q.settled.Wait()
	}
	q.mu.Unlock()
}

// saveLocked queues the operation's current state. The writer logs the
// transition once it is written, so the log shows only saved states.
func (s *Server) saveLocked(a *activeOperation) {
	previous := a.op.Revision
	a.op.Revision++
	s.saves.push(&persistItem{op: cloneOperation(a.op), previous: previous, from: a.logged})
	a.logged = translog.ParseState(a.op.State)
}

// insertLocked queues a new operation; its result arrives on the returned
// channel once the saves decided before it are written.
func (s *Server) insertLocked(op Operation) <-chan error {
	done := make(chan error, 1)
	s.saves.push(&persistItem{op: cloneOperation(op), done: done, from: translog.StateNone})
	return done
}

// persist is the writer. It stops once the server's context ended and the
// queue is empty, or shutdownFlush after that context ended.
func (s *Server) persist() {
	defer s.wg.Done()
	base := context.WithoutCancel(s.ctx)
	var flushUntil time.Time
	failed := false
	refused := map[string]bool{}
	for {
		item := s.saves.take()
		if item == nil {
			select {
			case <-s.saves.wake:
				continue
			case <-s.ctx.Done():
			}
			if item = s.saves.take(); item == nil {
				return
			}
		}
		ctx, cancel := base, context.CancelFunc(func() {})
		if s.ctx.Err() != nil {
			if flushUntil.IsZero() {
				flushUntil = time.Now().Add(shutdownFlush)
			}
			ctx, cancel = context.WithDeadline(base, flushUntil)
		}
		if item.done != nil {
			// A new insert of an ID starts over.
			delete(refused, item.op.ID)
		}
		var err error
		switch {
		case failed || refused[item.op.ID] || ctx.Err() != nil:
			err = ErrUnavailable
		case item.done != nil:
			err = s.repo.Insert(ctx, item.op)
		default:
			err = s.repo.Save(ctx, item.op, item.previous)
		}
		cancel()
		if to := translog.ParseState(item.op.State); err == nil && to != item.from {
			s.logOperation(item.op.ID, item.from, to, item.op.Reason)
		}
		if item.done != nil {
			if err != nil {
				refused[item.op.ID] = true
			}
			item.done <- err
		} else if err != nil && !failed && !refused[item.op.ID] && s.ctx.Err() == nil {
			failed = true
			s.mu.Lock()
			s.fatalLocked(translog.ReasonSaveFailed)
			s.mu.Unlock()
		}
		s.saves.finish()
	}
}
