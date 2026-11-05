package memory

import (
	"context"
	"slices"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

// presence is one instance's record of its connected workers.
type presence struct {
	workers []artifact.PresentWorker
	expires time.Time
}

// SetWorkers replaces instance's record with workers, valid until expires.
func (s *Store) SetWorkers(ctx context.Context, instance string, workers []artifact.PresentWorker, expires time.Time) error {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return err
	}
	if err := artifact.ValidatePresence(instance, workers); err != nil {
		return err
	}
	if len(workers) == 0 {
		delete(d.presence, instance)
		return nil
	}
	if d.presence == nil {
		d.presence = map[string]presence{}
	}
	kept := slices.Clone(workers)
	for i := range kept {
		kept[i].Warm = slices.Clone(kept[i].Warm)
	}
	d.presence[instance] = presence{workers: kept, expires: expires}
	return nil
}

// WorkerCounts counts the workers of unexpired records by runtime identity.
func (s *Store) WorkerCounts(ctx context.Context, now time.Time) (map[string]artifact.WorkerCount, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return nil, err
	}
	out := map[string]artifact.WorkerCount{}
	for _, p := range d.presence {
		if !p.expires.After(now) {
			continue
		}
		for _, w := range p.workers {
			out[w.Identity] = out[w.Identity].Add(w.Capacity, false)
			if w.Own != "" {
				out[w.Own] = out[w.Own].Add(w.Capacity, true)
			}
		}
	}
	return out, nil
}

// WarmRuntimes returns the runtimes pool workers hold running in the
// unexpired records of instances other than except.
func (s *Store) WarmRuntimes(ctx context.Context, now time.Time, except string) (map[string]bool, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for instance, p := range d.presence {
		if instance == except || !p.expires.After(now) {
			continue
		}
		for _, w := range p.workers {
			for _, id := range w.Warm {
				out[id] = true
			}
		}
	}
	return out, nil
}
