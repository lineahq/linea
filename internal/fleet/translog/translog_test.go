package translog

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

type buffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

// lines decodes every line written so far.
func (b *buffer) lines(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.b.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line is not JSON: %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestLineCarriesEventAndTypedFields(t *testing.T) {
	var out buffer
	log := New(&out)
	log.Warn(ReplicaFenced, ID(KeyReplica, "r-1"), Why(ReasonHeartbeatLapsed), Version("1.2.3-rc.1+build.5"),
		Count(CountAgents, 7), Duration(DurationOutage, 1500*time.Millisecond), Now(StatePaused), On(ListenerAgent))
	lines := out.lines(t)
	if len(lines) != 1 {
		t.Fatalf("lines = %d", len(lines))
	}
	got := lines[0]
	want := map[string]any{"msg": "replica_fenced", "level": "WARN", "replica": "r-1", "reason": "heartbeat_lapsed", "version": "1.2.3-rc.1+build.5",
		"agents": float64(7), "outage_ms": float64(1500), "component": "fleet", "state": "paused", "listener": "agent"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if _, ok := got["event"]; ok {
		t.Error("the event name is the msg field")
	}
	if _, ok := got["time"]; !ok {
		t.Error("line has no time")
	}
}

func TestIDsAndVersionsAreCheckedNotTrusted(t *testing.T) {
	var out buffer
	log := New(&out)
	for _, hostile := range []string{
		"", "has space", "line\nbreak", "tab\there", `quote"d`, "bearer=secret/token", "héllo", strings.Repeat("a", maxIDLength+1), "{\"k\":1}",
	} {
		log.Info(CredentialRevoked, ID(KeyCredential, hostile), Version(hostile))
	}
	for i, line := range out.lines(t) {
		if line["credential"] != invalid || line["version"] != invalid {
			t.Errorf("case %d: id=%v version=%v, want %q", i, line["credential"], line["version"], invalid)
		}
	}
	out2 := buffer{}
	New(&out2).Info(CredentialRevoked, ID(KeyCredential, strings.Repeat("a", maxIDLength)), ID(KeyActor, "memory-admin"), Version("0.0.0-dev"))
	if l := out2.lines(t)[0]; l["credential"] == invalid || l["actor"] != "memory-admin" || l["version"] != "0.0.0-dev" {
		t.Errorf("valid values were rejected: %v", l)
	}
}

func TestZeroValuesNeverWriteFreeText(t *testing.T) {
	var out buffer
	log := New(&out)
	// Out-of-range enumerations print as "unknown", never as a number or text.
	log.Info(Event(200), Why(Reason(200)), Now(State(200)), On(Listener(200)))
	log.Info(ProcessStarted, ID(IDKey(200), "x"))
	log.Info(ProcessStarted, Count(CountKey(200), -5))
	log.Info(ProcessStarted, Duration(DurationKey(200), -time.Second))
	lines := out.lines(t)
	if l := lines[0]; l["msg"] != "unknown" || l["reason"] != "unknown" || l["state"] != "unknown" || l["listener"] != "unknown" {
		t.Errorf("line = %v", l)
	}
	if lines[1]["unknown"] != "x" || lines[2]["unknown"] != float64(0) || lines[3]["unknown_ms"] != float64(0) {
		t.Errorf("lines = %v", lines)
	}
}

func TestEveryEnumerationIsNamed(t *testing.T) {
	check := func(kind string, names []string, first int) {
		seen := map[string]bool{}
		for i := first; i < len(names); i++ {
			if names[i] == "" || names[i] == "unknown" {
				t.Errorf("%s %d has no name", kind, i)
			}
			if seen[names[i]] {
				t.Errorf("%s name %q is used twice", kind, names[i])
			}
			seen[names[i]] = true
		}
	}
	check("event", eventNames[:], 1)
	check("reason", reasonNames[:], 1)
	check("state", stateNames[:], 1)
	check("listener", listenerNames[:], 1)
	check("id key", idKeyNames[:], 1)
	check("count key", countKeyNames[:], 1)
	check("duration key", durationKeyNames[:], 1)
}

func TestNilAndDiscardLoggersAreSafe(t *testing.T) {
	var nilLog *Logger
	nilLog.Info(ProcessStarted, Version("1"))
	nilLog.Warn(LeaderLost)
	nilLog.Error(ProcessStopped)
	nilLog.Limited(ConfigWithheld)
	nilLog.AuthFailure(ListenerAgent)
	nilLog.Flush()
	nilLog.Summarize(canceled(), nil)
	(&Logger{}).Info(ProcessStarted)
	Discard().Info(ProcessStarted)
}

func canceled() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestAuthFailuresAreOneSummaryLinePerListenerAndOnlyWhenCounted(t *testing.T) {
	var out buffer
	log := New(&out)
	log.Flush()
	if n := len(out.lines(t)); n != 0 {
		t.Fatalf("an empty window wrote %d lines", n)
	}
	for range 3 {
		log.AuthFailure(ListenerAgent)
	}
	log.AuthFailure(ListenerManagement)
	log.Flush()
	lines := out.lines(t)
	if len(lines) != 2 {
		t.Fatalf("lines = %v", lines)
	}
	counts := map[any]any{}
	for _, l := range lines {
		if l["msg"] != "auth_failures" {
			t.Errorf("msg = %v", l["msg"])
		}
		counts[l["listener"]] = l["count"]
	}
	if counts["agent"] != float64(3) || counts["management"] != float64(1) || len(counts) != 2 {
		t.Errorf("counts = %v", counts)
	}
	// The window restarted.
	log.Flush()
	if n := len(out.lines(t)); n != 2 {
		t.Errorf("a counted window repeated: %d lines", n)
	}
}

func TestSummarizeFlushesOnTicksAndOnceMoreAtTheEnd(t *testing.T) {
	var out buffer
	log := New(&out)
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); log.Summarize(ctx, ticks) }()

	log.AuthFailure(ListenerPluginHost)
	ticks <- time.Time{}
	// The unbuffered send returns once Summarize took the tick; a second
	// send returns only after the first flush finished.
	ticks <- time.Time{}
	lines := out.lines(t)
	if len(lines) != 1 || lines[0]["listener"] != "plugin_host" || lines[0]["count"] != float64(1) {
		t.Fatalf("after tick: %v", lines)
	}
	log.AuthFailure(ListenerPluginHost)
	log.AuthFailure(ListenerPluginHost)
	cancel()
	<-done
	lines = out.lines(t)
	if len(lines) != 2 || lines[1]["count"] != float64(2) {
		t.Fatalf("after stop: %v", lines)
	}
}

func TestLimitedWritesABoundedNumberAndReportsTheRest(t *testing.T) {
	var out buffer
	log := New(&out)
	for range limitPerWindow + 5 {
		log.Limited(ConfigWithheld, ID(KeyAgent, "a"), Why(ReasonIncompatibleComponents))
	}
	if n := len(out.lines(t)); n != limitPerWindow {
		t.Fatalf("written = %d, want %d", n, limitPerWindow)
	}
	log.Flush()
	lines := out.lines(t)
	last := lines[len(lines)-1]
	if len(lines) != limitPerWindow+1 || last["msg"] != "lines_suppressed" || last["of"] != "config_withheld" || last["suppressed"] != float64(5) {
		t.Fatalf("summary = %v", last)
	}
	// A new window writes again.
	log.Limited(ConfigWithheld)
	if n := len(out.lines(t)); n != limitPerWindow+2 {
		t.Errorf("lines = %d", n)
	}
}
