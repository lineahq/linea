package instrument

import (
	"sync"
	"testing"

	"go.opentelemetry.io/collector/pdata/plog"

	"github.com/lineahq/linea/internal/debugger/telemetry"
)

func TestBudgetExhaustionIsMonotonicAndCounted(t *testing.T) {
	b := NewBudget(100, 3)
	if !b.Charge(60, 1) || !b.Charge(30, 1) {
		t.Fatal("charges within the budget were refused")
	}
	if b.Charge(20, 1) {
		t.Fatal("a charge past the byte budget was kept")
	}
	// A charge that would fit is refused once the budget is exhausted, so
	// the kept evidence stays a prefix.
	if b.Charge(1, 0) {
		t.Fatal("a charge after exhaustion was kept")
	}
	if !b.Exhausted() {
		t.Fatal("budget not exhausted")
	}
	if events, bytes := b.Dropped(); events != 1 || bytes != 21 {
		t.Fatalf("dropped = %d events, %d bytes; want 1, 21", events, bytes)
	}
	b.Force(1000, 5)
	if events, bytes := b.Dropped(); events != 1 || bytes != 21 {
		t.Fatalf("forced items counted as dropped: %d, %d", events, bytes)
	}

	events := NewBudget(0, 2)
	if !events.Charge(1<<40, 1) || !events.Charge(1<<40, 1) || events.Charge(0, 1) {
		t.Fatal("event budget: want two kept, the third refused, bytes unlimited")
	}

	var unlimited *Budget
	if !unlimited.Charge(1<<50, 1<<30) || unlimited.Exhausted() {
		t.Fatal("a nil budget refused a charge")
	}
	unlimited.Force(1, 1)
	if events, bytes := unlimited.Dropped(); events != 0 || bytes != 0 {
		t.Fatal("a nil budget dropped something")
	}
	if !NewBudget(0, 0).Charge(1<<50, 1<<30) {
		t.Fatal("a zero budget is unlimited")
	}
}

func TestArchivedSizeIsBase64Length(t *testing.T) {
	for n, want := range map[int]int64{0: 0, 1: 4, 2: 4, 3: 4, 4: 8, 300: 400} {
		if got := ArchivedSize(n); got != want {
			t.Errorf("ArchivedSize(%d) = %d, want %d", n, got, want)
		}
	}
}

func budgetLogs(body string) telemetry.Payload {
	ld := plog.NewLogs()
	ld.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty().Body().SetStr(body)
	return telemetry.FromLogs(ld)
}

func TestRecorderKeepsAPrefixAndAlwaysTheLifecycle(t *testing.T) {
	rec := NewRecorderWithID("run")
	rec.SetBudget(NewBudget(0, 3))
	var observed []int
	rec.SetObserver(func(e Event) { observed = append(observed, e.Seq) })

	if seq := rec.Record(Event{Kind: KindRunStarted, Source: SourceOrchestrator}); seq != 1 {
		t.Fatalf("run.started seq = %d", seq)
	}
	if seq := rec.RecordTelemetry(Event{Kind: KindInput, Component: "otlp"}, budgetLogs("a")); seq != 2 {
		t.Fatalf("input seq = %d", seq)
	}
	if seq := rec.Record(Event{Kind: KindIngress, Component: "batch"}); seq != 3 {
		t.Fatalf("ingress seq = %d", seq)
	}
	// The budget is spent: further observations are not kept.
	if seq := rec.RecordTelemetry(Event{Kind: KindEgress, Component: "batch"}, budgetLogs("b")); seq != 0 {
		t.Fatalf("event past the budget recorded at seq %d", seq)
	}
	if seq := rec.Record(Event{Kind: KindStageError, Component: "batch"}); seq != 0 {
		t.Fatalf("event past the budget recorded at seq %d", seq)
	}
	// The run's end is always kept, right after the kept prefix.
	if seq := rec.Record(Event{Kind: KindRunCompleted, Source: SourceOrchestrator}); seq != 4 {
		t.Fatalf("run.completed seq = %d, want 4", seq)
	}

	events := rec.Events()
	for i, e := range events {
		if e.Seq != i+1 {
			t.Fatalf("event %d has seq %d; sequences must stay contiguous", i, e.Seq)
		}
	}
	if len(events) != 4 || events[3].Kind != KindRunCompleted {
		t.Fatalf("events = %+v", events)
	}
	if len(observed) != 4 || observed[3] != 4 {
		t.Fatalf("observer saw %v; it must not see refused events", observed)
	}
	if snaps := rec.Snapshots(); len(snaps) != 1 {
		t.Fatalf("stored %d snapshots; a refused event must not store its snapshot", len(snaps))
	}
}

func TestRecorderChargesAStoredSnapshotOnce(t *testing.T) {
	p := budgetLogs("same")
	data, err := p.JSON()
	if err != nil {
		t.Fatal(err)
	}
	probe := NewRecorderWithID("run")
	b := NewBudget(1<<30, 0)
	probe.SetBudget(b)
	probe.RecordTelemetry(Event{Kind: KindInput}, p)
	first := b.usedBytes
	probe.RecordTelemetry(Event{Kind: KindInput}, p)
	second := b.usedBytes - first
	if first-second != ArchivedSize(len(data)) {
		t.Fatalf("first charge %d, second %d: the snapshot (%d archived bytes) must be charged once", first, second, ArchivedSize(len(data)))
	}

	// A budget that fits the second event but not a new snapshot still keeps
	// a reference to the stored one.
	rec := NewRecorderWithID("run")
	rec.SetBudget(NewBudget(first+second, 0))
	if rec.RecordTelemetry(Event{Kind: KindInput}, p) == 0 || rec.RecordTelemetry(Event{Kind: KindInput}, p) == 0 {
		t.Fatal("a repeated snapshot was charged again")
	}
	if rec.RecordTelemetry(Event{Kind: KindInput}, budgetLogs("other")) != 0 {
		t.Fatal("a new snapshot past the budget was kept")
	}
}

func TestRecorderBudgetIsSafeConcurrently(t *testing.T) {
	rec := NewRecorderWithID("run")
	b := NewBudget(0, 50)
	rec.SetBudget(b)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				rec.RecordTelemetry(Event{Kind: KindIngress, Component: "c"}, budgetLogs(string(rune('a'+i))))
			}
		}()
	}
	wg.Wait()
	events := rec.Events()
	if len(events) != 50 {
		t.Fatalf("kept %d events, want 50", len(events))
	}
	for i, e := range events {
		if e.Seq != i+1 {
			t.Fatalf("event %d has seq %d", i, e.Seq)
		}
	}
	if dropped, _ := b.Dropped(); dropped != 8*20-50 {
		t.Fatalf("dropped %d events, want %d", dropped, 8*20-50)
	}
}
