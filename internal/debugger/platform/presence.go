package platform

import (
	"context"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

// Worker presence. With shared instances, each instance records
// its connected workers in the repository under its own ID, valid for one
// lease: on every connection and disconnection, and on every heartbeat, which
// renews the record three times a lease. The runtime catalog counts the
// workers of every instance's unexpired record, so an instance that was killed
// drops out of the counts within a lease. Close removes the record. A pool's
// record lists the runtimes it holds running and is written again whenever
// that list changes, so the scheduler of every instance can prefer a warm
// pool of another one (scheduler.go, poolGrace).

const (
	// presenceFinalTimeout bounds the write that removes a closing
	// instance's record.
	presenceFinalTimeout = 5 * time.Second
	// warmReadEvery is how long dispatch reuses its read of other
	// instances' warm pools.
	warmReadEvery = time.Second
)

// presentWorkers lists this service's connected workers.
func (s *Service) presentWorkers() []artifact.PresentWorker {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []artifact.PresentWorker
	for _, e := range s.executors {
		if e.conn != nil && !e.gone {
			w := artifact.PresentWorker{ID: e.id, Identity: e.presence(), Capacity: e.capacity}
			if e.pool() != "" {
				w.Warm, _ = e.conn.client.Warm()
				w.Warm = w.Warm[:min(len(w.Warm), artifact.MaxPresentWarm)]
				w.Own = e.own()
			}
			out = append(out, w)
		}
	}
	return out
}

// publishWorkers records this instance's connected workers until one lease
// after now. Writes are serialized, and each takes its list once it holds
// the turn, so a later write never records an older list. Once the service
// closes, only withdrawWorkers writes.
func (s *Service) publishWorkers(ctx context.Context, now time.Time) {
	if !s.shared {
		return
	}
	s.presenceMu.Lock()
	defer s.presenceMu.Unlock()
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return
	}
	if err := s.repository.SetWorkers(ctx, s.instanceID, s.presentWorkers(), now.Add(s.lease)); err != nil && ctx.Err() == nil {
		s.logError("recording connected workers", err)
	}
}

// withdrawWorkers removes this instance's record as it closes.
func (s *Service) withdrawWorkers() {
	if !s.shared {
		return
	}
	s.presenceMu.Lock()
	defer s.presenceMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), presenceFinalTimeout)
	defer cancel()
	if err := s.repository.SetWorkers(ctx, s.instanceID, nil, time.Time{}); err != nil {
		s.logError("removing the record of connected workers", err)
	}
}

// warmElsewhere returns the runtimes the pools of other instances hold
// running, read at most once per warmReadEvery. A failed read counts as none,
// so pools here never wait on a store they cannot read.
func (s *Service) warmElsewhere(now time.Time) map[string]bool {
	if !s.shared {
		return nil
	}
	s.warmMu.Lock()
	defer s.warmMu.Unlock()
	if s.warm != nil && !now.Before(s.warmRead) && now.Sub(s.warmRead) < warmReadEvery {
		return s.warm
	}
	warm, err := s.repository.WarmRuntimes(s.ctx, now, s.instanceID)
	if err != nil {
		if s.ctx.Err() == nil {
			s.logError("reading other instances' warm pools", err)
		}
		warm = map[string]bool{}
	}
	s.warm, s.warmRead = warm, now
	return warm
}
