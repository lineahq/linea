package translog

import (
	"sync"
	"sync/atomic"
	"time"
)

// SummaryEvery is how often a listener's rejected credentials are summarized.
const SummaryEvery = 10 * time.Second

// FailureSummary turns the rejected credentials on each listener into one
// EventAuthFailures line per window, only for a listener with a count above
// zero. Record is a lock-free increment, so a flood of bad credentials costs
// no more than a counter. The caller supplies the clock: Tick and Flush take
// the current time, so tests need no sleeps. A nil FailureSummary ignores
// everything.
type FailureSummary struct {
	log     *Logger
	every   time.Duration
	counts  [listenerCount]atomic.Uint64
	mu      sync.Mutex
	started time.Time
}

// NewFailureSummary starts a window at now that ends every every (SummaryEvery
// when every is not positive).
func NewFailureSummary(log *Logger, every time.Duration, now time.Time) *FailureSummary {
	if every <= 0 {
		every = SummaryEvery
	}
	return &FailureSummary{log: log, every: every, started: now}
}

// Record counts one rejected credential on listener.
func (f *FailureSummary) Record(listener Listener) {
	if f == nil || listener >= listenerCount {
		return
	}
	f.counts[listener].Add(1)
}

// Tick closes the window when it is due at now and logs a line per listener
// with failures.
func (f *FailureSummary) Tick(now time.Time) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if now.Sub(f.started) < f.every {
		return
	}
	f.emitLocked(now)
}

// Flush logs what the open window holds, whatever its age.
func (f *FailureSummary) Flush(now time.Time) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.emitLocked(now)
}

func (f *FailureSummary) emitLocked(now time.Time) {
	window := max(now.Sub(f.started), 0)
	f.started = now
	for listener := range f.counts {
		if n := f.counts[listener].Swap(0); n > 0 {
			f.log.Log(EventAuthFailures, OnListener(Listener(listener)), Count(int(n)), Elapsed(window))
		}
	}
}
