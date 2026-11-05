// Package instrument decorates Collector component factories so that real
// components execute unchanged while the debugger records the telemetry that
// crosses their boundaries.
package instrument

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// EventSchemaVersion versions the Event JSON shape independently of the
// scenario schema.
const EventSchemaVersion = "0.6"

// Kind identifies what an event observed. Names follow the event
// model.
type Kind string

const (
	KindRunStarted       Kind = "run.started"
	KindRunCompleted     Kind = "run.completed"
	KindRunFailed        Kind = "run.failed"
	KindRunCancelled     Kind = "run.cancelled"
	KindComponentStarted Kind = "component.started"
	KindComponentStopped Kind = "component.stopped"
	KindInput            Kind = "input.injected"
	KindIngress          Kind = "stage.input"
	KindEgress           Kind = "stage.output"
	KindStageError       Kind = "stage.error"
	KindExport           Kind = "exporter.captured"
	// KindDecision: a processor decided whether to keep a trace
	// (tail_sampling); Decision says how, and the snapshot holds the spans
	// it evaluated.
	KindDecision Kind = "stage.decision"
	// KindComponentLog: a component that runs for real logged an entry
	// (Log); an observation, so the evidence budget bounds it.
	KindComponentLog Kind = "component.log"
	// KindEndpointReceived: a simulated client endpoint of a component
	// that runs for real received an OTLP export; Endpoint names
	// the endpoint, and the snapshot holds what arrived.
	KindEndpointReceived Kind = "endpoint.received"
)

// Source names the mechanism that made an observation.
type Source string

const (
	SourceOrchestrator     Source = "orchestrator"
	SourceProcessorWrapper Source = "processor_wrapper"
	SourceReplayReceiver   Source = "replay_receiver"
	SourceCaptureExporter  Source = "capture_exporter"
	// SourceConnectorWrapper: a decorated connector; its stage.input is what
	// it consumed and each stage.output what it delivered into one pipeline.
	SourceConnectorWrapper Source = "connector_wrapper"
	// SourceMockedReceiver: a real receiver read a mocked dependency (for
	// example filelog reading the scenario's files); its output is recorded
	// as the receiver's input to the pipeline.
	SourceMockedReceiver Source = "mocked_receiver"
	// SourceStatusWatcher: the Collector's status reports for an extension
	// that runs for real (a custom one); it records the extension's
	// component.started and component.stopped.
	SourceStatusWatcher Source = "status_watcher"
	// SourceComponentTelemetry: a component's own telemetry, its log
	// entries recorded through the logger the run hands it.
	SourceComponentTelemetry Source = "component_telemetry"
	// SourceEndpointSink: a loopback OTLP sink standing for a client
	// endpoint a component's configuration declares.
	SourceEndpointSink Source = "endpoint_sink"
)

// Execution modes as recorded on events; they match topology.Mode values.
const (
	ModeReal             = "real"
	ModeSimulated        = "simulated"
	ModeCaptured         = "captured"
	ModeDependencyMocked = "dependency_mocked"
)

// Event is one observation of a run.
type Event struct {
	// ID is unique across runs: "<run_id>:<seq>".
	ID    string    `json:"id"`
	RunID string    `json:"run_id"`
	Seq   int       `json:"seq"`
	Time  time.Time `json:"time"`
	Kind  Kind      `json:"kind"`
	// Component is the component ID (for example "transform/logs").
	Component string `json:"component,omitempty"`
	// ComponentKind is the component's kind ("receiver", "processor",
	// "connector" or "extension") on events whose source does not imply
	// it: component.log and an extension's lifecycle (schema 0.6).
	ComponentKind string `json:"component_kind,omitempty"`
	// Pipeline is set for processor events: the Collector builds one
	// processor instance per pipeline. On a connector's stage.output it is
	// the pipeline the output was delivered into.
	Pipeline string `json:"pipeline,omitempty"`
	Signal   string `json:"signal,omitempty"`
	Mode     string `json:"mode,omitempty"`
	Source   Source `json:"source"`
	// InputSeq links a stage.output or stage.error to the stage.input that
	// was being consumed when it occurred (synchronous emission). It is zero
	// for emissions that cannot be attributed, such as a batch flush.
	InputSeq int `json:"input_seq,omitempty"`
	// FromSeq links an event that received telemetry (stage.input,
	// exporter.captured) to the event that delivered exactly that call:
	// an input.injected or a stage.output. It is read from the context
	// the delivering call passed down (see WithEmission), so it is an
	// invocation-level link, not a content match. Zero when unknown.
	FromSeq int               `json:"from_seq,omitempty"`
	Counts  *telemetry.Counts `json:"counts,omitempty"`
	// Snapshot references the telemetry in Snapshots, taken at the moment of
	// capture, so later in-place mutation does not affect it.
	Snapshot string `json:"snapshot,omitempty"`
	Error    string `json:"error,omitempty"`
	// Decision is set on stage.decision events.
	Decision *Decision `json:"decision,omitempty"`
	// Log is set on component.log events.
	Log *LogEntry `json:"log,omitempty"`
	// Endpoint is where the client configuration of an endpoint.received
	// event sits in the component's configuration ("delivery").
	Endpoint string `json:"endpoint,omitempty"`
	// Objects numbers the pdata object of each record of the snapshot, in
	// telemetry.Records order, per run (from 1). A stage that passes a record
	// on in place passes on its object, so the same number before and after
	// a stage is the same record, whatever the stage changed. Omitted when
	// objects cannot identify records (see telemetry.Objects), after the run
	// ended and on events recorded before schema 0.5.
	Objects []int `json:"objects,omitempty"`
}

// Snapshots is a content-addressed store of OTLP JSON snapshots, keyed by
// "sha256:<hex>". Identical telemetry observed at several boundaries (for
// example a stage.output and the next stage.input) is stored once.
type Snapshots map[string]json.RawMessage

// Payload decodes the snapshot referenced by ref.
func (s Snapshots) Payload(ref string) (telemetry.Payload, error) {
	data, ok := s[ref]
	if !ok {
		return telemetry.Payload{}, fmt.Errorf("snapshot %q not found", ref)
	}
	return telemetry.Decode(data)
}

// Recorder collects events from concurrently running components in arrival
// order.
type Recorder struct {
	runID string
	now   func() time.Time

	mu        sync.Mutex
	events    []Event
	snapshots Snapshots
	observer  func(Event)
	err       error
	budget    *Budget
	// objects numbers every record object observed during the run and
	// keeps it reachable, so its memory never holds another record while
	// the run lasts; ReleaseObjects drops them when the run ends.
	objects  map[unsafe.Pointer]int
	released bool
	// telemetry holds components' own meter providers.
	telemetry componentTelemetry
	// decisions hooks sampling processors (see SetDecisionHook).
	decisions DecisionHook
}

// SetBudget bounds what the recorder keeps (see Budget). Past the budget,
// Record and RecordTelemetry keep nothing and return 0, except for the run's
// lifecycle events, which are always kept.
func (r *Recorder) SetBudget(b *Budget) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.budget = b
}

// SetObserver registers fn to receive every event as it is recorded, in
// order. fn is called with the recorder locked, so it must return quickly
// and must not record events itself.
func (r *Recorder) SetObserver(fn func(Event)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observer = fn
}

// NewRecorder returns an empty recorder with a new random run ID.
func NewRecorder() *Recorder { return NewRecorderWithID(NewRunID()) }

// NewRecorderWithID returns an empty recorder for the given run ID, for
// callers that must know the ID before the run starts.
func NewRecorderWithID(runID string) *Recorder {
	return &Recorder{runID: runID, now: time.Now, snapshots: Snapshots{}}
}

// NewRunID returns a new random run ID ("run-" and 16 hex digits).
func NewRunID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "run-" + hex.EncodeToString(b[:])
}

// RunID identifies the run the recorder belongs to.
func (r *Recorder) RunID() string { return r.runID }

// Record appends e, filling in run ID, sequence number and time, and returns
// the sequence number, or 0 when the budget does not keep it.
func (r *Recorder) Record(e Event) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.append(e, 0)
}

// runLifecycle reports whether e is a run lifecycle event, which is always kept
// so that the timeline stays well-formed.
func runLifecycle(e Event) bool {
	if e.Source != SourceOrchestrator {
		return false
	}
	switch e.Kind {
	case KindRunStarted, KindRunCompleted, KindRunFailed, KindRunCancelled:
		return true
	}
	return false
}

// append records e unless the budget refuses it together with extra bytes
// (a new snapshot). An event costs twice its JSON size: evidence archives
// store the live event list and the report's events.
func (r *Recorder) append(e Event, extra int64) int {
	e.Seq = len(r.events) + 1
	e.RunID = r.runID
	e.ID = fmt.Sprintf("%s:%d", r.runID, e.Seq)
	e.Time = r.now().UTC()
	if r.budget != nil {
		size := extra
		if data, err := json.Marshal(e); err == nil {
			size += 2 * int64(len(data))
		}
		if runLifecycle(e) {
			r.budget.Force(size, 1)
		} else if !r.budget.Charge(size, 1) {
			return 0
		}
	}
	r.events = append(r.events, e)
	if r.observer != nil {
		r.observer(e)
	}
	return e.Seq
}

// RecordTelemetry snapshots p into e and records it. It returns 0 if the
// snapshot failed, or if the budget does not keep it; a failure is retained
// and reported by Err rather than interfering with the pipeline. A snapshot
// already stored costs nothing more.
func (r *Recorder) RecordTelemetry(e Event, p telemetry.Payload) int {
	data, err := p.JSON()
	counts := p.Counts()
	sum := sha256.Sum256(data)
	ref := "sha256:" + hex.EncodeToString(sum[:])
	objects, identified := telemetry.Objects(p)

	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		if r.err == nil {
			r.err = fmt.Errorf("snapshot %s %s: %w", e.Kind, e.Component, err)
		}
		return 0
	}
	_, stored := r.snapshots[ref]
	var extra int64
	if !stored {
		extra = ArchivedSize(len(data))
	}
	e.Signal = p.Signal.String()
	e.Counts = &counts
	e.Snapshot = ref
	var fresh map[unsafe.Pointer]int
	if identified && !r.released {
		e.Objects, fresh = r.number(objects)
	}
	seq := r.append(e, extra)
	if seq == 0 {
		return 0
	}
	if !stored {
		r.snapshots[ref] = data
	}
	if len(fresh) > 0 && r.objects == nil {
		r.objects = make(map[unsafe.Pointer]int, len(fresh))
	}
	for o, n := range fresh {
		r.objects[o] = n
	}
	return seq
}

// number returns the number of each object, giving objects not seen before
// the next numbers. Those are returned separately, to be kept only with
// their event.
func (r *Recorder) number(objects []unsafe.Pointer) ([]int, map[unsafe.Pointer]int) {
	out := make([]int, len(objects))
	var fresh map[unsafe.Pointer]int
	next := len(r.objects) + 1
	for i, o := range objects {
		if n, ok := r.objects[o]; ok {
			out[i] = n
			continue
		}
		if n, ok := fresh[o]; ok {
			out[i] = n
			continue
		}
		if fresh == nil {
			fresh = map[unsafe.Pointer]int{}
		}
		fresh[o] = next
		out[i] = next
		next++
	}
	return out, fresh
}

// ReleaseObjects drops the record objects once the run has ended. Later
// events carry no object numbers, since released memory may then hold other
// records.
func (r *Recorder) ReleaseObjects() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.objects = nil
	r.released = true
}

// Events returns a copy of the recorded events.
func (r *Recorder) Events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

// Snapshots returns a copy of the snapshot store.
func (r *Recorder) Snapshots() Snapshots {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(Snapshots, len(r.snapshots))
	for k, v := range r.snapshots {
		out[k] = v
	}
	return out
}

// Err reports the first snapshot failure, if any.
func (r *Recorder) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// emissionKey marks a context with the event that recorded the telemetry
// being delivered in that call. Components never read it.
type emissionKey struct{}

// WithEmission marks ctx as delivering the telemetry recorded by event seq.
// A seq of 0 (a failed snapshot) leaves ctx unchanged.
func WithEmission(ctx context.Context, seq int) context.Context {
	if seq == 0 {
		return ctx
	}
	return context.WithValue(ctx, emissionKey{}, seq)
}

// EmissionOf returns the seq set by the nearest WithEmission, or 0.
func EmissionOf(ctx context.Context) int {
	seq, _ := ctx.Value(emissionKey{}).(int)
	return seq
}
