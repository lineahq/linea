package translog

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLineFieldsAndLevels(t *testing.T) {
	log, rec := NewRecorder()
	log.Log(EventOperationState, Operation("abc123"), From(StateNone), To(StateStarting), Why(ReasonNone))
	log.Log(EventPluginFatal, Cause(ReasonStorageLost))
	log.Log(EventPluginStarted, Version("0.0.0-dev"))
	entries := rec.Entries()
	if len(entries) != 3 {
		t.Fatalf("got %d lines: %s", len(entries), rec.Raw())
	}
	first := entries[0]
	if first.Name() != "operation_state" || first.Text("operation") != "abc123" || first.Text("from") != "none" || first.Text("to") != "starting" || first.Text("level") != "INFO" || first.Text("component") != "livecapture" {
		t.Fatalf("operation line %v", first)
	}
	if _, ok := first["reason"]; ok {
		t.Fatalf("ReasonNone was logged: %v", first)
	}
	if entries[1].Text("cause") != "storage_lost" || entries[1].Text("level") != "ERROR" {
		t.Fatalf("fatal line %v", entries[1])
	}
	if entries[2].Text("version") != "0.0.0-dev" {
		t.Fatalf("version line %v", entries[2])
	}
}

// IDs and versions outside the narrow character set never reach a line, so a
// sentence, a token with separators or an error string cannot be smuggled in
// through an ID constructor.
func TestIDsAreChecked(t *testing.T) {
	log, rec := NewRecorder()
	for _, bad := range []string{"", "has space", "Bearer abc", `quote"`, "line\nbreak", strings.Repeat("a", maxIDLength+1), "a=b", "café"} {
		log.Log(EventPeerConnected, Peer(bad), Credential(bad), Operation(bad), Agent(bad), Authority(bad), Instance(bad), Epoch(bad))
	}
	entries := rec.Entries()
	if len(entries) != 8 {
		t.Fatalf("got %d lines", len(entries))
	}
	for _, e := range entries {
		for _, key := range []string{"peer", "credential", "operation", "agent", "authority", "instance", "epoch"} {
			if e.Text(key) != "invalid" {
				t.Fatalf("field %s=%q was not replaced: %v", key, e.Text(key), e)
			}
		}
	}
	log, rec = NewRecorder()
	log.Log(EventPluginStarted, Version("1.0 beta"))
	log.Log(EventPluginStarted, Version(strings.Repeat("1", 65)))
	log.Log(EventPluginStarted, Version("1.2.3-rc.1+build"))
	versions := rec.Entries()
	if versions[0].Text("version") != "invalid" || versions[1].Text("version") != "invalid" || versions[2].Text("version") != "1.2.3-rc.1+build" {
		t.Fatalf("versions %s", rec.Raw())
	}
	log, rec = NewRecorder()
	log.Log(EventPeerConnected, Peer(strings.Repeat("0123456789abcdef", 4)), Credential("cred-1_a.b"))
	if got := rec.Entries()[0]; got.Text("peer") != strings.Repeat("0123456789abcdef", 4) || got.Text("credential") != "cred-1_a.b" {
		t.Fatalf("valid IDs were changed: %v", got)
	}
}

func TestEnumsAreClosed(t *testing.T) {
	if ParseReason("") != ReasonNone || ParseReason("requested_stop") != ReasonRequestedStop || ParseReason("a token") != ReasonUnknown {
		t.Fatal("ParseReason")
	}
	if ParseState("active") != StateActive || ParseState("none") != StateUnknown || ParseState("x") != StateUnknown {
		t.Fatal("ParseState")
	}
	if ParseLink("no_match") != LinkNoMatch || ParseLink("x") != LinkUnknown {
		t.Fatal("ParseLink")
	}
	// A value outside the table prints as unknown, never as its number.
	if Reason(250).String() != "unknown" || State(250).String() != "unknown" || Link(250).String() != "unknown" || Event(250).String() != "unknown" || Listener(250).String() != "unknown" {
		t.Fatal("out-of-range enum was not unknown")
	}
	// Every constant has a name, and names are unique and in the line format.
	seen := map[string]bool{}
	for r := ReasonUnknown; r < reasonCount; r++ {
		name := r.String()
		if name == "" || seen[name] || strings.ToLower(name) != name || strings.ContainsAny(name, " -") {
			t.Fatalf("reason %d name %q", r, name)
		}
		seen[name] = true
	}
	seen = map[string]bool{}
	for e := EventPluginStarted; e < eventCount; e++ {
		name := e.String()
		if name == "unknown" || seen[name] {
			t.Fatalf("event %d name %q", e, name)
		}
		seen[name] = true
	}
}

func TestDiscardAndNilWriteNothing(t *testing.T) {
	var nilLogger *Logger
	nilLogger.Log(EventPluginStarted, Version("1.0.0"))
	nilLogger.Close(time.Second)
	Discard().Log(EventPluginStarted, Version("1.0.0"))
	var summary *FailureSummary
	summary.Record(ListenerHTTP)
	summary.Tick(time.Now())
	summary.Flush(time.Now())
}

func TestFailureSummaryOneLinePerListenerPerWindow(t *testing.T) {
	log, rec := NewRecorder()
	start := time.Unix(1_700_000_000, 0)
	summary := NewFailureSummary(log, 10*time.Second, start)
	for range 3 {
		summary.Record(ListenerHTTP)
	}
	summary.Record(ListenerOTLP)
	summary.Tick(start.Add(9 * time.Second))
	if len(rec.Entries()) != 0 {
		t.Fatalf("summary before the window ended: %s", rec.Raw())
	}
	summary.Tick(start.Add(10 * time.Second))
	entries := rec.Entries()
	if len(entries) != 2 {
		t.Fatalf("got %d lines: %s", len(entries), rec.Raw())
	}
	counts := map[string]float64{}
	for _, e := range entries {
		if e.Name() != "auth_failures" || e["duration_ms"] != float64(10_000) {
			t.Fatalf("line %v", e)
		}
		counts[e.Text("listener")] = e["count"].(float64)
	}
	if counts["http_api"] != 3 || counts["otlp_grpc"] != 1 || len(counts) != 2 {
		t.Fatalf("counts %v", counts)
	}
	// A quiet window writes nothing, and the window restarts at the tick.
	summary.Tick(start.Add(20 * time.Second))
	summary.Record(ListenerControl)
	summary.Tick(start.Add(29 * time.Second))
	summary.Tick(start.Add(30 * time.Second))
	entries = rec.Entries()
	if len(entries) != 3 || entries[2].Text("listener") != "control_grpc" || entries[2]["count"] != float64(1) {
		t.Fatalf("after the quiet window: %s", rec.Raw())
	}
	// Flush writes the open window early, once.
	summary.Record(ListenerControl)
	summary.Flush(start.Add(31 * time.Second))
	summary.Flush(start.Add(32 * time.Second))
	if len(rec.Entries()) != 4 {
		t.Fatalf("flush: %s", rec.Raw())
	}
}

// slowWriter blocks until released, like a stderr pipe nobody reads.
type slowWriter struct {
	release chan struct{}
	mu      sync.Mutex
	buf     bytes.Buffer
}

func (w *slowWriter) Write(p []byte) (int, error) {
	<-w.release
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// Logging never waits for the writer: a blocked stderr drops lines and counts
// them instead of stalling the caller.
func TestLogNeverWaitsForTheWriter(t *testing.T) {
	w := &slowWriter{release: make(chan struct{})}
	log := New(w)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range queueLines * 3 {
			log.Log(EventPeerConnected, Peer("p"))
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Log blocked on a stuck writer")
	}
	if log.Dropped() == 0 {
		t.Fatal("nothing was dropped")
	}
	// A stuck writer also cannot hold Close beyond its wait.
	start := time.Now()
	log.Close(50 * time.Millisecond)
	if time.Since(start) > 5*time.Second {
		t.Fatal("Close waited for the stuck writer")
	}
	close(w.release)
}

func TestCloseFlushesQueuedLines(t *testing.T) {
	var buf syncBuffer
	log := New(&buf)
	log.Log(EventPluginStopped, Version("1.0.0"), Why(ReasonShutdown))
	log.Close(5 * time.Second)
	if !strings.Contains(buf.String(), `"msg":"plugin_stopped"`) || !strings.Contains(buf.String(), `"reason":"shutdown"`) {
		t.Fatalf("line not flushed: %q", buf.String())
	}
	log.Log(EventPluginStarted, Version("1.0.0"))
	if log.Dropped() != 1 {
		t.Fatalf("a line after Close was not dropped: %d", log.Dropped())
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
