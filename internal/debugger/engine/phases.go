package engine

import (
	"slices"
	"sync"
	"time"
)

// Phases a run records about its own execution: where its time went, never
// what it ran. A worker sends them with its result and the team server
// turns them into spans (platform/server_traces.go). The names are a
// contract once published.
const (
	PhaseRuntimeAcquire     = "runtime.acquire"
	PhaseBundleFetch        = "bundle.fetch"
	PhaseBundleDecode       = "bundle.decode"
	PhaseConfigResolve      = "config.resolve"
	PhaseCollectorRun       = "collector.run"
	PhaseAnalysis           = "analysis"
	PhaseCollectorReference = "collector.reference"
	PhaseResultEncode       = "result.encode"
	PhaseResultUpload       = "result.upload"
)

var knownPhases = []string{PhaseRuntimeAcquire, PhaseBundleFetch, PhaseBundleDecode, PhaseConfigResolve,
	PhaseCollectorRun, PhaseAnalysis, PhaseCollectorReference, PhaseResultEncode, PhaseResultUpload}

// KnownPhase reports whether name is one of the phases above.
func KnownPhase(name string) bool { return slices.Contains(knownPhases, name) }

// Phase is one timed part of an execution: its offset from the moment the
// recorder started (a worker: when it received the request), its duration,
// and whether it failed. It carries no message.
type Phase struct {
	Name     string        `json:"name"`
	Offset   time.Duration `json:"offset_ns"`
	Duration time.Duration `json:"duration_ns"`
	Failed   bool          `json:"failed,omitempty"`
}

// Phases records an execution's phases. A nil *Phases records nothing; it is
// safe for concurrent use.
type Phases struct {
	origin time.Time
	mu     sync.Mutex
	list   []Phase
}

// NewPhases starts a recorder whose offsets count from origin.
func NewPhases(origin time.Time) *Phases { return &Phases{origin: origin} }

// Add appends phases recorded elsewhere, such as a worker's.
func (p *Phases) Add(phases ...Phase) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.list = append(p.list, phases...)
	p.mu.Unlock()
}

// List returns the phases recorded so far.
func (p *Phases) List() []Phase {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.list)
}

// Since is the offset of t from the recorder's origin.
func (p *Phases) Since(t time.Time) time.Duration {
	if p == nil {
		return 0
	}
	return t.Sub(p.origin)
}

// PhaseTracker times consecutive phases: Next ends the current phase and
// starts another, Done ends the last. A nil tracker records nothing.
type PhaseTracker struct {
	p     *Phases
	name  string
	start time.Time
}

// Track starts a tracker over p; with p nil it returns nil.
func (p *Phases) Track() *PhaseTracker {
	if p == nil {
		return nil
	}
	return &PhaseTracker{p: p}
}

// Next ends the current phase as succeeded and starts name.
func (t *PhaseTracker) Next(name string) {
	if t == nil {
		return
	}
	now := time.Now()
	t.end(now, false)
	t.name, t.start = name, now
}

// Done ends the current phase, failed when err is set.
func (t *PhaseTracker) Done(err error) {
	if t == nil {
		return
	}
	t.end(time.Now(), err != nil)
	t.name = ""
}

func (t *PhaseTracker) end(now time.Time, failed bool) {
	if t.name == "" {
		return
	}
	t.p.Add(Phase{Name: t.name, Offset: t.p.Since(t.start), Duration: now.Sub(t.start), Failed: failed})
}
