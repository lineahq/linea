package runtime

import (
	"context"
	"sync"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/pipeline"

	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// mockHost is the runtime side of mock.Host: telemetry a receiver produced
// from a mocked dependency is recorded as that receiver's input, and its
// next consumer stays available for scenario inputs.
type mockHost struct {
	rec *instrument.Recorder
	rep *replay

	mu       sync.Mutex
	order    []component.ID
	received map[component.ID]telemetry.Counts
}

func newMockHost(rec *instrument.Recorder, rep *replay) *mockHost {
	return &mockHost{rec: rec, rep: rep, received: map[component.ID]telemetry.Counts{}}
}

func (h *mockHost) Received(ctx context.Context, id component.ID, p telemetry.Payload) context.Context {
	// The reference run has no recorder; it only counts.
	var seq int
	if h.rec != nil {
		seq = h.rec.RecordTelemetry(instrument.Event{
			Kind:      instrument.KindInput,
			Component: id.String(),
			Mode:      instrument.ModeDependencyMocked,
			Source:    instrument.SourceMockedReceiver,
		}, p)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	c, seen := h.received[id]
	if !seen {
		h.order = append(h.order, id)
	}
	n := p.Counts()
	c.Resources += n.Resources
	c.Scopes += n.Scopes
	c.Items += n.Items
	h.received[id] = c
	if h.rec == nil {
		return ctx
	}
	return instrument.WithEmission(ctx, seq)
}

func (h *mockHost) Register(id component.ID, signal pipeline.Signal, next any) {
	h.rep.register(id, signal, next)
}

// inputs summarizes what each mocked receiver emitted, in first-emission
// order.
func (h *mockHost) inputs() []runresult.InputResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]runresult.InputResult, 0, len(h.order))
	for _, id := range h.order {
		out = append(out, runresult.InputResult{
			Receiver: id.String(), Signal: pipeline.SignalLogs.String(),
			Counts: h.received[id], Injected: true, Mock: id.Type().String(),
		})
	}
	return out
}
