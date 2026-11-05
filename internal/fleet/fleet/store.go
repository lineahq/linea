// Package fleet owns reporting state without depending on transports or clients.
package fleet

import (
	"encoding/hex"
	"errors"
	"maps"
	"slices"
	"sort"
	"sync"
	"time"
)

var (
	ErrCapacity = errors.New("inventory capacity reached")
	ErrStale    = errors.New("stale connection generation")
	// ErrBusy is writer backpressure: retry later instead of blocking reports.
	ErrBusy = errors.New("fleet writer busy")
)

// Takeover policy: the newest connection wins, and this many takeovers within
// the window mark the UID as shared by more than one process.
const (
	ConflictTakeovers = 3
	ConflictWindow    = time.Minute
)

type Health struct {
	Healthy bool `json:"healthy"`
	// ObservedAt is when the reported health last changed.
	ObservedAt time.Time `json:"observed_at"`
	// Status is the agent's top-level status string, such as StatusOK.
	Status string `json:"status,omitempty"`
	// Error is stored only when the operator opts in, redacted and truncated.
	Error               string `json:"error,omitempty"`
	ComponentsUnhealthy int    `json:"components_unhealthy"`
}

// Status is the health detail beyond the healthy flag.
type Status struct {
	Status string
	Error  string
}

// Component merges one Collector component's place in the effective
// configuration with its reported health. ID is kind:name, e.g. receiver:otlp.
type Component struct {
	ID        string   `json:"id"`
	Pipelines []string `json:"pipelines,omitempty"`
	Healthy   *bool    `json:"healthy,omitempty"`
	Status    string   `json:"status,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// RemoteConfig is an agent's last RemoteConfigStatus. Hashes are hex. Held
// is set only by config management: the hash withheld after a failed apply.
type RemoteConfig struct {
	Hash   string `json:"hash,omitempty"`
	Status string `json:"status,omitempty"`
	// Error is stored only when the operator opts into health error text.
	Error string `json:"error,omitempty"`
	Held  string `json:"held_hash,omitempty"`
}

// RemoteConfigState is the stored remote config status of an agent.
type RemoteConfigState struct {
	RemoteConfig
	// ObservedAt is when the status last changed.
	ObservedAt time.Time `json:"observed_at"`
}

// Report holds the approved fields of one agent's current state. Inventory
// sections stay nil until the agent first reports them; the gateway keeps the
// last reported value, so a section never reverts to nil.
type Report struct {
	Capabilities uint64
	Healthy      *bool
	Status       *Status
	Attributes   map[string]string
	Components   []Component
	ConfigHash   string
	// AvailableComponents is the hex hash of the agent's component build;
	// empty keeps the stored one.
	AvailableComponents string
	RemoteConfig        *RemoteConfig
}

// Changes names the field groups that differ, in a stable order.
func Changes(previous, next Report) []string {
	changed := []string{}
	if previous.Capabilities != next.Capabilities {
		changed = append(changed, "capabilities")
	}
	if next.Healthy != nil && (previous.Healthy == nil || *previous.Healthy != *next.Healthy) ||
		next.Status != nil && (previous.Status == nil || *previous.Status != *next.Status) {
		changed = append(changed, "health")
	}
	if next.Attributes != nil && (previous.Attributes == nil || !maps.Equal(previous.Attributes, next.Attributes)) {
		changed = append(changed, "attributes")
	}
	if next.Components != nil && (previous.Components == nil || !EqualComponents(previous.Components, next.Components)) ||
		next.ConfigHash != previous.ConfigHash || next.AvailableComponents != "" && next.AvailableComponents != previous.AvailableComponents {
		changed = append(changed, "components")
	}
	if next.RemoteConfig != nil && (previous.RemoteConfig == nil || *previous.RemoteConfig != *next.RemoteConfig) {
		changed = append(changed, "config")
	}
	return changed
}

func EqualComponents(a, b []Component) bool {
	return slices.EqualFunc(a, b, func(x, y Component) bool {
		return x.ID == y.ID && x.Status == y.Status && x.Error == y.Error && slices.Equal(x.Pipelines, y.Pipelines) &&
			(x.Healthy == nil) == (y.Healthy == nil) && (x.Healthy == nil || *x.Healthy == *y.Healthy)
	})
}

// Unhealthy counts components that report unhealthy.
func Unhealthy(components []Component) int {
	n := 0
	for _, c := range components {
		if c.Healthy != nil && !*c.Healthy {
			n++
		}
	}
	return n
}

type Agent struct {
	ID             string            `json:"id"`
	Generation     uint64            `json:"generation"`
	Connected      bool              `json:"connected"`
	Presence       string            `json:"presence"`
	FirstSeenAt    time.Time         `json:"first_seen_at"`
	ConnectedAt    time.Time         `json:"connected_at"`
	DisconnectedAt *time.Time        `json:"disconnected_at,omitempty"`
	LastSeenAt     *time.Time        `json:"last_seen_at,omitempty"`
	LastReportAt   *time.Time        `json:"last_report_at,omitempty"`
	CredentialID   string            `json:"credential_id,omitempty"`
	Capabilities   uint64            `json:"capabilities"`
	Health         *Health           `json:"health,omitempty"`
	Attributes     map[string]string `json:"attributes"`
	Components     []Component       `json:"components,omitempty"`
	ConfigHash     string            `json:"config_hash,omitempty"`
	// AvailableComponentsHash identifies the agent's component build.
	AvailableComponentsHash string             `json:"available_components_hash,omitempty"`
	RemoteConfig            *RemoteConfigState `json:"remote_config,omitempty"`
	IdentityConflict        bool               `json:"identity_conflict"`
}

type record struct {
	Agent
	takeovers  int
	takeoverAt time.Time
}

// Store is the ephemeral development repository. It follows the durable
// semantics: newest connection wins and heartbeats only touch memory.
type Store struct {
	mu     sync.RWMutex
	agents map[string]*record
	max    int
}

func NewStore(maxAgents int) *Store {
	if maxAgents < 1 {
		panic("maxAgents must be positive")
	}
	return &Store{agents: make(map[string]*record), max: maxAgents}
}

func ValidID(id string) bool {
	if len(id) != 32 {
		return false
	}
	b, err := hex.DecodeString(id)
	return err == nil && hex.EncodeToString(b) == id
}

// Connect takes ownership for a new connection and fences any older one.
func (s *Store) Connect(id string, report Report, now time.Time) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now = now.UTC()
	r, exists := s.agents[id]
	if !exists {
		if len(s.agents) >= s.max && !s.evict() {
			return 0, ErrCapacity
		}
		r = &record{Agent: Agent{ID: id, FirstSeenAt: now, Attributes: map[string]string{}}}
		s.agents[id] = r
	}
	if r.Connected {
		if now.Sub(r.takeoverAt) < ConflictWindow {
			r.takeovers++
		} else {
			r.takeovers, r.takeoverAt = 1, now
		}
		if r.takeovers >= ConflictTakeovers {
			r.IdentityConflict = true
		}
	}
	r.Generation++
	r.Connected, r.Presence = true, "connected"
	r.ConnectedAt, r.LastSeenAt, r.DisconnectedAt = now, &now, nil
	r.apply(report, now)
	return r.Generation, nil
}

// evict removes the agent disconnected longest, as the durable store does.
func (s *Store) evict() bool {
	var oldest *record
	for _, r := range s.agents {
		if !r.Connected && (oldest == nil || r.DisconnectedAt.Before(*oldest.DisconnectedAt)) {
			oldest = r
		}
	}
	if oldest != nil {
		delete(s.agents, oldest.ID)
	}
	return oldest != nil
}

func (r *record) apply(report Report, now time.Time) {
	r.Capabilities = report.Capabilities
	if report.Healthy != nil {
		next := Health{Healthy: *report.Healthy, ObservedAt: now}
		if report.Status != nil {
			next.Status, next.Error = report.Status.Status, report.Status.Error
		}
		// ObservedAt is the time health last changed, as in the durable store.
		if h := r.Health; h != nil && h.Healthy == next.Healthy && h.Status == next.Status && h.Error == next.Error {
			next.ObservedAt = h.ObservedAt
		}
		r.Health = &next
	}
	if report.Attributes != nil {
		r.Attributes = maps.Clone(report.Attributes)
	}
	if report.Components != nil {
		r.Components = slices.Clone(report.Components)
	}
	if r.Health != nil {
		r.Health.ComponentsUnhealthy = Unhealthy(r.Components)
	}
	// An empty hash keeps the stored one, as in the durable store.
	if report.ConfigHash != "" {
		r.ConfigHash = report.ConfigHash
	}
	if report.AvailableComponents != "" {
		r.AvailableComponentsHash = report.AvailableComponents
	}
	if rc := report.RemoteConfig; rc != nil {
		next := RemoteConfigState{RemoteConfig: *rc, ObservedAt: now}
		if r.RemoteConfig != nil && r.RemoteConfig.RemoteConfig == *rc {
			next.ObservedAt = r.RemoteConfig.ObservedAt
		}
		r.RemoteConfig = &next
	}
}

// report is the stored state in the shape agents report it.
func (r *record) report() Report {
	out := Report{Capabilities: r.Capabilities, Attributes: maps.Clone(r.Attributes), Components: slices.Clone(r.Components), ConfigHash: r.ConfigHash,
		AvailableComponents: r.AvailableComponentsHash}
	if r.Health != nil {
		healthy := r.Health.Healthy
		out.Healthy, out.Status = &healthy, &Status{Status: r.Health.Status, Error: r.Health.Error}
	}
	if r.RemoteConfig != nil {
		rc := r.RemoteConfig.RemoteConfig
		out.RemoteConfig = &rc
	}
	return out
}

// Observe copies only approved fields; no protocol payload is retained.
func (s *Store) Observe(id string, generation uint64, report Report, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.agents[id]
	if !ok || !r.Connected || r.Generation != generation {
		return ErrStale
	}
	now = now.UTC()
	r.LastSeenAt = &now
	r.apply(report, now)
	return nil
}

func (s *Store) Seen(id string, generation uint64, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.agents[id]; ok && r.Connected && r.Generation == generation {
		now = now.UTC()
		r.LastSeenAt = &now
	}
}

func (s *Store) Disconnect(id string, generation uint64, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.agents[id]
	if !ok || r.Generation != generation || !r.Connected {
		return
	}
	now = now.UTC()
	r.Connected, r.Presence = false, "disconnected"
	r.DisconnectedAt = &now
}

func clone(a Agent, detail bool) Agent {
	if a.Health != nil {
		h := *a.Health
		a.Health = &h
	}
	if a.RemoteConfig != nil {
		rc := *a.RemoteConfig
		a.RemoteConfig = &rc
	}
	if a.LastSeenAt != nil {
		t := *a.LastSeenAt
		a.LastSeenAt = &t
		a.LastReportAt = &t
	}
	if a.DisconnectedAt != nil {
		t := *a.DisconnectedAt
		a.DisconnectedAt = &t
	}
	a.Attributes = maps.Clone(a.Attributes)
	if detail {
		a.Components = slices.Clone(a.Components)
	} else {
		a.Components = nil
	}
	return a
}

func (s *Store) Get(id string) (Agent, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.agents[id]
	if !ok {
		return Agent{}, false
	}
	return clone(r.Agent, true), true
}

type Page struct {
	Items     []Agent `json:"items"`
	NextAfter string  `json:"next_after,omitempty"`
}

// List is a current-state keyset page, not a historical snapshot.
func (s *Store) List(after string, limit int, connected *bool) Page {
	p, _ := s.Query(AgentQuery{After: after, Limit: limit, Connected: connected})
	return p
}

// Query applies the same filters, order and positions as the durable store.
func (s *Store) Query(q AgentQuery) (Page, error) {
	if q.Group != "" {
		return Page{}, ErrNotFound // the memory store has no groups
	}
	sortValue := func(a *record) string { return "" }
	afterValue, afterID := "", q.After
	if q.Sort != "" {
		if _, ok := Promoted[q.Sort]; !ok {
			return Page{}, Invalid("sort " + PromotedRule)
		}
		sortValue = func(a *record) string { return a.Attributes[q.Sort] }
		if q.After != "" {
			var err error
			if afterValue, afterID, err = DecodeAfter(q.After); err != nil {
				return Page{}, err
			}
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	matches := make([]*record, 0, len(s.agents))
	for _, r := range s.agents {
		v := sortValue(r)
		if (q.After == "" || v > afterValue || v == afterValue && r.ID > afterID) &&
			(q.Connected == nil || r.Connected == *q.Connected) &&
			(q.Credential == "" || r.CredentialID == q.Credential) &&
			(q.Healthy == nil || r.Health != nil && r.Health.Healthy == *q.Healthy) &&
			q.Selector.Matches(r.Attributes) {
			matches = append(matches, r)
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		a, b := sortValue(matches[i]), sortValue(matches[j])
		return a < b || a == b && matches[i].ID < matches[j].ID
	})
	p := Page{Items: make([]Agent, 0, min(q.Limit, len(matches)))}
	for _, r := range matches[:min(q.Limit, len(matches))] {
		p.Items = append(p.Items, clone(r.Agent, false))
	}
	if len(matches) > q.Limit {
		last := matches[q.Limit-1]
		p.NextAfter = last.ID
		if q.Sort != "" {
			p.NextAfter = EncodeAfter(sortValue(last), last.ID)
		}
	}
	return p, nil
}
