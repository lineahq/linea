package livecapture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lineahq/linea/pluginsdk"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// fakeHost serves the fleet host services the plugin uses.
type fakeHost struct {
	mu          sync.Mutex
	introspect  func(pluginsdk.IntrospectRequest) pluginsdk.Introspection
	resolve     func(pluginsdk.ResolveRequest) pluginsdk.ResolveResponse
	group       func(pluginsdk.GroupResolveRequest) (pluginsdk.GroupResolveResponse, int)
	introspects int
}

func (h *fakeHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out any
	code := http.StatusOK
	switch r.URL.Path {
	case pluginsdk.IntrospectPath:
		var in pluginsdk.IntrospectRequest
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			code = http.StatusBadRequest
			break
		}
		h.introspects++
		out = h.introspect(in)
	case pluginsdk.AgentsResolvePath:
		var in pluginsdk.ResolveRequest
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			code = http.StatusBadRequest
			break
		}
		out = h.resolve(in)
	case pluginsdk.GroupsResolvePath:
		var in pluginsdk.GroupResolveRequest
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			code = http.StatusBadRequest
			break
		}
		out, code = h.group(in)
	default:
		code = http.StatusNotFound
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if code == http.StatusOK {
		_ = json.NewEncoder(w).Encode(out)
	} else {
		_, _ = w.Write([]byte(`{"error":"rejected"}`))
	}
}

func withHost(t *testing.T, s *Server, h *fakeHost) {
	t.Helper()
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	s.host.Endpoint = server.URL
	s.session = pluginsdk.Session{Epoch: s.epoch, InstanceID: s.instance, Permissions: []string{pluginsdk.AgentsRead, pluginsdk.CredentialsIntrospect}}
}

func bearer(token string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+token))
}

func TestCredentialAdmission(t *testing.T) {
	s, _, _ := fixture(t)
	now := time.Now()
	soon := now.Add(time.Second)
	active := func(labels map[string]string) pluginsdk.Introspection {
		return pluginsdk.Introspection{Active: true, CredentialID: "credential", Role: "agent", Labels: labels}
	}
	cases := []struct {
		name   string
		in     pluginsdk.Introspection
		admit  bool
		allows string
		denies string
	}{
		{"groups", active(map[string]string{"capture": "edge, gateway"}), true, "gateway", "other"},
		{"wildcard", active(map[string]string{"capture": "*"}), true, "anything", ""},
		{"missing label", active(map[string]string{"team": "edge"}), false, "", ""},
		{"empty label", active(map[string]string{"capture": " "}), false, "", ""},
		{"empty group", active(map[string]string{"capture": "edge,,gateway"}), false, "", ""},
		{"inactive", pluginsdk.Introspection{CredentialID: "credential", Role: "agent", Labels: map[string]string{"capture": "*"}}, false, "", ""},
		{"management role", pluginsdk.Introspection{Active: true, CredentialID: "credential", Role: "management", Labels: map[string]string{"capture": "*"}}, false, "", ""},
	}
	for _, c := range cases {
		entry := s.admit([32]byte{}, c.in, now)
		if entry.admitted != c.admit {
			t.Fatalf("%s: admitted=%v", c.name, entry.admitted)
		}
		if c.allows != "" && !entry.credential.allows(c.allows) {
			t.Fatalf("%s: group %q denied", c.name, c.allows)
		}
		if c.denies != "" && entry.credential.allows(c.denies) {
			t.Fatalf("%s: group %q allowed", c.name, c.denies)
		}
		if !c.admit && entry.until != now.Add(negativeTTL) {
			t.Fatalf("%s: rejection cached for the wrong time", c.name)
		}
	}
	expiring := active(map[string]string{"capture": "*"})
	expiring.ExpiresAt = &soon
	if entry := s.admit([32]byte{}, expiring, now); !entry.until.Equal(soon) {
		t.Fatal("cache outlived the credential")
	}
	s.config.CaptureLabel = "otel.capture"
	if !s.admit([32]byte{}, active(map[string]string{"otel.capture": "edge"}), now).admitted {
		t.Fatal("configured capture label ignored")
	}
}

func TestAuthenticateIntrospectsAndCaches(t *testing.T) {
	s, _, _ := fixture(t)
	allowed, unlabeled := strings.Repeat("1", 64), strings.Repeat("2", 64)
	h := &fakeHost{introspect: func(in pluginsdk.IntrospectRequest) pluginsdk.Introspection {
		sum := sha256.Sum256([]byte(allowed))
		if in.TokenSHA256 == hex.EncodeToString(sum[:]) {
			return pluginsdk.Introspection{Active: true, CredentialID: "allowed", Role: "agent", Labels: map[string]string{"capture": "test"}}
		}
		return pluginsdk.Introspection{Active: true, CredentialID: "unlabeled", Role: "agent"}
	}}
	withHost(t, s, h)
	for range 2 {
		c, err := s.authenticate(bearer(allowed))
		if err != nil || c.id != "allowed" || !c.allows("test") {
			t.Fatal("labeled agent credential rejected", err)
		}
	}
	if h.introspects != 1 {
		t.Fatalf("introspected %d times, want cached", h.introspects)
	}
	if _, err := s.authenticate(bearer(unlabeled)); status.Code(err) != codes.Unauthenticated {
		t.Fatal("credential without capture label admitted", err)
	}
	if _, err := s.authenticate(context.Background()); status.Code(err) != codes.Unauthenticated {
		t.Fatal("missing credential admitted", err)
	}
	s.host.Endpoint = "http://127.0.0.1:1"
	if _, err := s.authenticate(bearer(strings.Repeat("3", 64))); status.Code(err) != codes.Unavailable {
		t.Fatal("unreachable host did not fail closed", err)
	}
	s.mu.Lock()
	s.hostOK = false
	s.mu.Unlock()
	if _, err := s.authenticate(bearer(allowed)); status.Code(err) != codes.Unavailable {
		t.Fatal("cached credential admitted without host authority", err)
	}
}

func TestRecheckClosesRevokedAndRegroupedPeers(t *testing.T) {
	s, _, p := fixture(t)
	label := "test"
	active := true
	h := &fakeHost{introspect: func(pluginsdk.IntrospectRequest) pluginsdk.Introspection {
		return pluginsdk.Introspection{Active: active, CredentialID: "credential", Role: "agent", Labels: map[string]string{"capture": label}}
	}}
	withHost(t, s, h)
	s.recheckCredentials(s.session)
	if !p.alive() {
		t.Fatal("peer with an allowed credential was closed")
	}
	label = "other"
	s.recheckCredentials(s.session)
	if p.alive() || s.observed.credentialClosed.Load() != 1 {
		t.Fatal("peer outside its credential's groups stayed connected")
	}
	second := &peer{credential: p.credential, info: PeerInfo{ID: strings.Repeat("b", 64), Generation: 1, CredentialID: "credential", Identity: p.info.Identity}, wake: make(chan struct{}, 1), done: make(chan struct{})}
	s.peers[second.info.ID] = second
	label, active = "test", false
	s.recheckCredentials(s.session)
	if second.alive() {
		t.Fatal("peer with a revoked credential stayed connected")
	}
	s.host.Endpoint = "http://127.0.0.1:1"
	third := &peer{credential: p.credential, info: PeerInfo{ID: strings.Repeat("c", 64), Generation: 1, CredentialID: "credential", Identity: p.info.Identity}, wake: make(chan struct{}, 1), done: make(chan struct{})}
	s.peers[third.info.ID] = third
	s.recheckCredentials(s.session)
	if !third.alive() {
		t.Fatal("host outage closed peers")
	}
}

func TestLinkPeersRequiresUniqueMatch(t *testing.T) {
	s, _, p := fixture(t)
	agent := pluginsdk.RandomID()
	var requests []pluginsdk.ResolveRequest
	matches := map[string][]string{}
	h := &fakeHost{resolve: func(in pluginsdk.ResolveRequest) pluginsdk.ResolveResponse {
		requests = append(requests, in)
		key := "k8s"
		if in.Attributes["host.name"] != "" {
			key = "host"
		}
		return pluginsdk.ResolveResponse{AgentIDs: append([]string{}, matches[key]...)}
	}}
	withHost(t, s, h)

	matches["k8s"] = []string{agent}
	s.linkPeers(s.session)
	if p.info.AgentID != agent {
		t.Fatal("unique Kubernetes match was not linked")
	}
	first := requests[0]
	if first.CredentialID != "credential" || first.Attributes["k8s.pod.name"] != "pod" || first.Attributes["k8s.cluster.name"] != "cluster" || first.Attributes["k8s.namespace.name"] != "" {
		t.Fatalf("resolve request %+v", first)
	}

	// A linked peer is not re-resolved until its refresh is due.
	requests = nil
	s.linkPeers(s.session)
	if len(requests) != 0 {
		t.Fatal("linked peer re-resolved before refresh")
	}

	matches["k8s"] = nil
	matches["host"] = []string{agent}
	p.linkedAt, p.info.AgentID = time.Time{}, ""
	s.linkPeers(s.session)
	if p.info.AgentID != agent || len(requests) != 2 {
		t.Fatal("host.name fallback was not linked")
	}

	matches["k8s"] = []string{agent, pluginsdk.RandomID()}
	p.linkedAt = time.Time{}
	s.linkPeers(s.session)
	if p.info.AgentID != "" {
		t.Fatal("ambiguous match was linked")
	}
}

func TestFleetTargeting(t *testing.T) {
	s, r, p := fixture(t)
	agent := pluginsdk.RandomID()
	group := pluginsdk.RandomID()
	groupCode := http.StatusOK
	var seen pluginsdk.GroupResolveRequest
	h := &fakeHost{group: func(in pluginsdk.GroupResolveRequest) (pluginsdk.GroupResolveResponse, int) {
		seen = in
		return pluginsdk.GroupResolveResponse{AgentIDs: []string{agent, pluginsdk.RandomID()}, Revision: 3}, groupCode
	}}
	withHost(t, s, h)
	request := func(mutate func(*CreateRequest)) CreateRequest {
		out := r
		out.ID, out.Targets = pluginsdk.RandomID(), nil
		mutate(&out)
		return out
	}

	// Unlinked peers are not addressable by fleet agent or group.
	if _, err := s.Create(context.Background(), "owner", request(func(c *CreateRequest) { c.Agents = []string{agent} })); !errors.Is(err, ErrConflict) {
		t.Fatal("unlinked agent targeted", err)
	}
	if _, err := s.Create(context.Background(), "owner", request(func(c *CreateRequest) { c.GroupID = group })); !errors.Is(err, ErrConflict) {
		t.Fatal("group without linked peers admitted", err)
	}
	p.info.AgentID = agent

	for code, want := range map[int]error{http.StatusNotFound: ErrNotFound, http.StatusUnprocessableEntity: ErrCapacity, http.StatusBadRequest: ErrInvalid, http.StatusServiceUnavailable: ErrUnavailable} {
		groupCode = code
		if _, err := s.Create(context.Background(), "owner", request(func(c *CreateRequest) { c.Selector = "env=prod" })); !errors.Is(err, want) {
			t.Fatalf("group resolve status %d gave %v", code, err)
		}
	}
	groupCode = http.StatusOK

	op, err := s.Create(context.Background(), "owner", request(func(c *CreateRequest) { c.GroupID = group }))
	if err != nil || len(op.Targets) != 1 || op.Targets[0].PeerID != p.info.ID || op.Targets[0].AgentID != agent {
		t.Fatal("group target did not select the linked peer", err, op.Targets)
	}
	if seen.GroupID != group || seen.Connected == nil || !*seen.Connected {
		t.Fatalf("group resolve request %+v", seen)
	}
	if _, err := s.Stop(context.Background(), "owner", op.ID, op.Revision); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.applyReportLocked(p, report{Statuses: []collectorStatus{{SessionID: s.operations[op.ID].op.Targets[0].WireID, State: "completed"}}})
	s.mu.Unlock()

	op, err = s.Create(context.Background(), "owner", request(func(c *CreateRequest) { c.Agents = []string{agent} }))
	if err != nil || len(op.Targets) != 1 || op.Targets[0].AgentID != agent {
		t.Fatal("agent target was not admitted", err)
	}
	// A busy peer leaves the group with nothing to capture.
	if _, err := s.Create(context.Background(), "owner", request(func(c *CreateRequest) { c.GroupID = group })); !errors.Is(err, ErrConflict) {
		t.Fatal("busy peer selected again", err)
	}
	// Two peers claiming one agent are ambiguous.
	twin := &peer{info: PeerInfo{ID: strings.Repeat("b", 64), Generation: 1, AgentID: agent, Identity: p.info.Identity}, lastSeen: time.Now(), wake: make(chan struct{}, 1), done: make(chan struct{})}
	s.peers[twin.info.ID] = twin
	if _, err := s.Create(context.Background(), "owner", request(func(c *CreateRequest) { c.Agents = []string{agent} })); !errors.Is(err, ErrConflict) {
		t.Fatal("ambiguous agent admitted", err)
	}
}

func TestValidateCreateModes(t *testing.T) {
	_, r, _ := fixture(t)
	agent := pluginsdk.RandomID()
	cases := []struct {
		name   string
		mutate func(*CreateRequest)
		ok     bool
	}{
		{"targets", func(*CreateRequest) {}, true},
		{"agents", func(c *CreateRequest) { c.Targets, c.Agents = nil, []string{agent} }, true},
		{"selector", func(c *CreateRequest) { c.Targets, c.Selector = nil, "env=prod" }, true},
		{"group", func(c *CreateRequest) { c.Targets, c.GroupID = nil, pluginsdk.RandomID() }, true},
		{"no mode", func(c *CreateRequest) { c.Targets = nil }, false},
		{"two modes", func(c *CreateRequest) { c.Agents = []string{agent} }, false},
		{"duplicate agents", func(c *CreateRequest) { c.Targets, c.Agents = nil, []string{agent, agent} }, false},
		{"invalid agent", func(c *CreateRequest) { c.Targets, c.Agents = nil, []string{"agent"} }, false},
		{"too many agents", func(c *CreateRequest) {
			c.Targets = nil
			for range 17 {
				c.Agents = append(c.Agents, pluginsdk.RandomID())
			}
		}, false},
		{"control in selector", func(c *CreateRequest) { c.Targets, c.Selector = nil, "env=prod\n" }, false},
		{"long selector", func(c *CreateRequest) { c.Targets, c.Selector = nil, "env="+strings.Repeat("a", 4096) }, false},
		{"invalid group", func(c *CreateRequest) { c.Targets, c.GroupID = nil, "group" }, false},
	}
	for _, c := range cases {
		request := r
		c.mutate(&request)
		if err := validateCreate(request); (err == nil) != c.ok {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
}

// Admission refuses a filter larger than the target Collector reports it
// compiles, and says why, for named targets and for groups alike.
func TestFilterMustFitCollectorLimits(t *testing.T) {
	s, r, p := fixture(t)
	p.info.Identity.FilterLimits = FilterLimits{MaxConditions: 2, MaxConditionChars: 8, MaxRegexChars: 8}
	agent, group := pluginsdk.RandomID(), pluginsdk.RandomID()
	p.info.AgentID = agent
	withHost(t, s, &fakeHost{group: func(pluginsdk.GroupResolveRequest) (pluginsdk.GroupResolveResponse, int) {
		return pluginsdk.GroupResolveResponse{AgentIDs: []string{agent}, Revision: 1}, http.StatusOK
	}})
	for name, c := range map[string]struct {
		filter func(*Filter)
		want   string
	}{
		"count":  {func(f *Filter) { f.Resource, f.Traces.Span = []string{"true", "true"}, []string{"true"} }, "3 conditions; the Collector compiles at most 2"},
		"length": {func(f *Filter) { f.Traces.Span = []string{`name == "é12345"`} }, "a condition has 16 characters; the Collector compiles at most 8"},
	} {
		for mode, target := range map[string]func(*CreateRequest){"targets": func(*CreateRequest) {}, "group": func(c *CreateRequest) { c.Targets, c.GroupID = nil, group }} {
			request := r
			request.ID = pluginsdk.RandomID()
			target(&request)
			c.filter(&request.Filter)
			_, err := s.Create(context.Background(), "owner", request)
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%s %s: %v", name, mode, err)
			}
		}
	}
	r.Filter.Resource = []string{"true", "true"}
	if _, err := s.Create(context.Background(), "owner", r); err != nil {
		t.Fatal("filter within the limits refused", err)
	}
}

// A refused capture request names the rule it broke.
func TestValidateCreateNamesTheRule(t *testing.T) {
	_, r, _ := fixture(t)
	for want, mutate := range map[string]func(*CreateRequest){
		"set exactly one of targets":         func(c *CreateRequest) { c.Targets = nil },
		"signals must be unique traces":      func(c *CreateRequest) { c.Signals = []string{"profiles"} },
		"expires_at must be in the future":   func(c *CreateRequest) { c.ExpiresAt = time.Now().Add(time.Hour) },
		"each filter list allows at most 32": func(c *CreateRequest) { c.Filter.Resource = make([]string, 33) },
	} {
		c := r
		mutate(&c)
		if err := validateCreate(c); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), want) {
			t.Fatalf("want %q, got %v", want, err)
		}
	}
}
