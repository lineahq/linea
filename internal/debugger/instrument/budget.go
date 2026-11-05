package instrument

import "sync"

// Budget bounds the evidence one run keeps: the bytes its evidence archive
// stores and the number of events it records. One budget is shared by
// everything that lands in the run's archive (the recorder, captured exporter
// output and the reference run). Once a charge is refused the budget is
// exhausted for good, so the kept evidence is a prefix of what happened; it
// keeps counting what was not kept. Components never see a budget: only
// observation stops. A nil Budget keeps everything.
type Budget struct {
	bytes  int64
	events int

	mu            sync.Mutex
	usedBytes     int64
	usedEvents    int
	exhausted     bool
	droppedBytes  int64
	droppedEvents int
}

// NewBudget returns a budget of bytes and events; zero or negative leaves that
// dimension unlimited.
func NewBudget(bytes int64, events int) *Budget {
	return &Budget{bytes: max(bytes, 0), events: max(events, 0)}
}

// Charge reports whether an item costing bytes and events may be kept, and
// accounts for it either way.
func (b *Budget) Charge(bytes int64, events int) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.exhausted && (b.bytes > 0 && b.usedBytes+bytes > b.bytes || b.events > 0 && b.usedEvents+events > b.events) {
		b.exhausted = true
	}
	if b.exhausted {
		b.droppedBytes += bytes
		b.droppedEvents += events
		return false
	}
	b.usedBytes += bytes
	b.usedEvents += events
	return true
}

// Force accounts for an item that is always kept (a run's lifecycle events).
func (b *Budget) Force(bytes int64, events int) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.usedBytes += bytes
	b.usedEvents += events
}

// Dropped returns what was not kept so far.
func (b *Budget) Dropped() (events int, bytes int64) {
	if b == nil {
		return 0, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.droppedEvents, b.droppedBytes
}

// Exhausted reports whether a charge has been refused.
func (b *Budget) Exhausted() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.exhausted
}

// ArchivedSize is the size of n bytes stored as a JSON byte string (base64),
// as evidence archives store snapshots.
func ArchivedSize(n int) int64 { return 4 * ((int64(n) + 2) / 3) }
