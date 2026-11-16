package livecapture

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lineahq/linea/pluginsdk"
)

// linkFixture is a server whose peers start unlinked and whose host answers
// resolve requests through answer. The clock is injected.
type linkFixture struct {
	s      *Server
	clock  time.Time
	mu     sync.Mutex
	pods   []string
	answer func(pod string) pluginsdk.ResolveResponse
}

func newLinkFixture(t *testing.T) *linkFixture {
	t.Helper()
	s, _, _ := fixture(t)
	s.peers = map[string]*peer{}
	f := &linkFixture{s: s, clock: time.Unix(1_700_000_000, 0)}
	s.now = func() time.Time { return f.clock }
	h := &fakeHost{resolve: func(in pluginsdk.ResolveRequest) pluginsdk.ResolveResponse {
		pod := in.Attributes["k8s.pod.name"]
		if pod == "" {
			return pluginsdk.ResolveResponse{} // the host.name fallback finds nothing
		}
		f.mu.Lock()
		f.pods = append(f.pods, pod)
		f.mu.Unlock()
		return f.answer(pod)
	}}
	withHost(t, s, h)
	return f
}

func (f *linkFixture) add(id, pod string) *peer {
	p := &peer{info: PeerInfo{ID: id, Generation: 1, CredentialID: "credential", Link: PeerLink{State: linkPending}, Identity: Identity{Pod: pod, Group: "test"}}, lastSeen: f.clock, wake: make(chan struct{}, 1), done: make(chan struct{})}
	f.s.peers[id] = p
	return p
}

// attempted returns the pods resolved since the last call.
func (f *linkFixture) attempted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.pods
	f.pods = nil
	return out
}

func (f *linkFixture) pass() time.Duration { return f.s.linkPeers(f.s.session) }

func nothing(string) pluginsdk.ResolveResponse { return pluginsdk.ResolveResponse{} }

func one(agent string) func(string) pluginsdk.ResolveResponse {
	return func(string) pluginsdk.ResolveResponse { return pluginsdk.ResolveResponse{AgentIDs: []string{agent}} }
}

// More unlinkable peers than one pass can take no longer starve a linkable
// one: the peers with the oldest attempt go first, so the cut rotates.
func TestLinkerRotatesThroughUnlinkablePeers(t *testing.T) {
	f := newLinkFixture(t)
	agent := pluginsdk.RandomID()
	f.answer = func(pod string) pluginsdk.ResolveResponse {
		if pod == "linkable" {
			return pluginsdk.ResolveResponse{AgentIDs: []string{agent}}
		}
		return pluginsdk.ResolveResponse{}
	}
	for i := range maxLinksPerPass + 6 {
		f.add(fmt.Sprintf("%064d", i), fmt.Sprintf("pod-%03d", i))
	}
	// Its ID sorts after every other peer, which is where the old linker never
	// looked.
	last := f.add(strings.Repeat("f", 64), "linkable")

	f.pass()
	if first := f.attempted(); len(first) != maxLinksPerPass || slices.Contains(first, "linkable") {
		t.Fatalf("first pass tried %d peers, linkable among them: %v", len(first), slices.Contains(first, "linkable"))
	}
	f.clock = f.clock.Add(linkPassGap)
	f.pass()
	second := f.attempted()
	if len(second) != 7 || !slices.Contains(second, "linkable") {
		t.Fatalf("second pass tried %v", second)
	}
	if last.info.AgentID != agent || last.info.Link.State != linkLinked {
		t.Fatalf("linkable peer: agent %q, state %q", last.info.AgentID, last.info.Link.State)
	}

	// The 64 tried first are due again once their backoff ends; the seven tried
	// second only after theirs, which started later.
	f.clock = f.clock.Add(linkBackoffMin)
	f.pass()
	third := f.attempted()
	if len(third) != maxLinksPerPass {
		t.Fatalf("third pass tried %d peers", len(third))
	}
}

// A peer is tried oldest attempt first, whatever its ID.
func TestLinkerTakesOldestAttemptFirst(t *testing.T) {
	f := newLinkFixture(t)
	f.answer = nothing
	old, recent := f.add(strings.Repeat("9", 64), "old"), f.add(strings.Repeat("1", 64), "recent")
	f.add(strings.Repeat("5", 64), "never")
	old.linkTried, recent.linkTried = f.clock.Add(-time.Hour), f.clock.Add(-time.Minute)
	f.pass()
	if got := f.attempted(); !slices.Equal(got, []string{"never", "old", "recent"}) {
		t.Fatalf("order %v", got)
	}
}

// An unlinked peer backs off 2 s, 4 s, ... to 60 s; a linked peer refreshes
// every minute; a host error counts as an attempt and keeps the old state.
func TestLinkerBackoffAndStates(t *testing.T) {
	f := newLinkFixture(t)
	f.answer = nothing
	p := f.add(strings.Repeat("a", 64), "pod")

	delays := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, time.Minute, time.Minute}
	for i, delay := range delays {
		f.pass()
		if got := f.attempted(); len(got) != 1 {
			t.Fatalf("attempt %d: tried %v", i+1, got)
		}
		if p.info.Link.State != linkNoMatch {
			t.Fatalf("attempt %d: state %q", i+1, p.info.Link.State)
		}
		// One moment before the backoff ends nothing is tried; at its end it is.
		f.clock = f.clock.Add(delay - time.Nanosecond)
		f.pass()
		if got := f.attempted(); len(got) != 0 {
			t.Fatalf("attempt %d: peer retried %v early", i+1, got)
		}
		f.clock = f.clock.Add(time.Nanosecond)
	}

	// The wait the loop is given is the time until the next attempt.
	f.pass()
	f.attempted()
	if wait := f.s.nextLinkWait(); wait != time.Minute {
		t.Fatalf("next wait %v", wait)
	}

	// Ambiguity shows as its own state and backs off the same way.
	f.answer = func(string) pluginsdk.ResolveResponse {
		return pluginsdk.ResolveResponse{AgentIDs: []string{pluginsdk.RandomID(), pluginsdk.RandomID()}}
	}
	f.clock = f.clock.Add(time.Minute)
	f.pass()
	if p.info.Link.State != linkAmbiguous || p.info.AgentID != "" {
		t.Fatalf("ambiguous: state %q agent %q", p.info.Link.State, p.info.AgentID)
	}

	// Linking resets the backoff and schedules the refresh.
	agent := pluginsdk.RandomID()
	f.answer = one(agent)
	f.clock = f.clock.Add(time.Minute)
	f.pass()
	if p.info.Link.State != linkLinked || p.linkFailures != 0 || !p.linkNext.Equal(f.clock.Add(linkedRefresh)) {
		t.Fatalf("linked: state %q failures %d next %v", p.info.Link.State, p.linkFailures, p.linkNext.Sub(f.clock))
	}

	// A host error keeps the link, counts as an attempt and backs off.
	f.s.host.Endpoint = "http://127.0.0.1:1"
	f.clock = f.clock.Add(linkedRefresh)
	f.pass()
	if p.info.Link.State != linkLinked || p.info.AgentID != agent || p.linkFailures != 1 || !p.linkNext.Equal(f.clock.Add(linkBackoffMin)) || !p.linkTried.Equal(f.clock) {
		t.Fatalf("after a host error: state %q failures %d next %v", p.info.Link.State, p.linkFailures, p.linkNext.Sub(f.clock))
	}
}

// A peer that never got an answer shows pending, and a host error before the
// first answer leaves it so.
func TestLinkerPendingUntilAnAttemptFinishes(t *testing.T) {
	f := newLinkFixture(t)
	f.s.host.Endpoint = "http://127.0.0.1:1"
	p := f.add(strings.Repeat("a", 64), "pod")
	f.pass()
	if p.info.Link.State != linkPending || p.linkFailures != 1 {
		t.Fatalf("state %q failures %d", p.info.Link.State, p.linkFailures)
	}
}

// One counter per outcome, named in the metrics.
func TestLinkerCountsEachOutcome(t *testing.T) {
	f := newLinkFixture(t)
	agent := pluginsdk.RandomID()
	f.answer = func(pod string) pluginsdk.ResolveResponse {
		switch pod {
		case "linked":
			return pluginsdk.ResolveResponse{AgentIDs: []string{agent}}
		case "ambiguous":
			return pluginsdk.ResolveResponse{AgentIDs: []string{pluginsdk.RandomID(), pluginsdk.RandomID()}}
		}
		return pluginsdk.ResolveResponse{}
	}
	for i, pod := range []string{"linked", "ambiguous", "no match", "no match"} {
		f.add(strings.Repeat(fmt.Sprint(i+1), 64), pod)
	}
	f.pass()
	f.s.host.Endpoint = "http://127.0.0.1:1"
	f.clock = f.clock.Add(time.Minute)
	f.pass() // all four peers fail on the host
	counters := f.s.Metrics().Counters
	want := map[string]uint64{"peer_link_linked_total": 1, "peer_link_ambiguous_total": 1, "peer_link_no_match_total": 2, "peer_link_error_total": 4}
	for name, n := range want {
		if counters[name] != n {
			t.Errorf("%s = %d, want %d", name, counters[name], n)
		}
	}
}

// The schema lists exactly the states the plugin can show.
func TestOpenAPIPeerLinkStatesMatchPlugin(t *testing.T) {
	var document struct {
		Components struct {
			Schemas struct {
				PeerLink struct {
					Properties struct {
						State struct {
							Enum []string `json:"enum"`
						} `json:"state"`
					} `json:"properties"`
				} `json:"PeerLink"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(openAPI, &document); err != nil {
		t.Fatal(err)
	}
	got := document.Components.Schemas.PeerLink.Properties.State.Enum
	want := []string{linkPending, linkLinked, linkNoMatch, linkAmbiguous}
	if !slices.Equal(got, want) {
		t.Fatalf("schema states %v, plugin states %v", got, want)
	}
}
