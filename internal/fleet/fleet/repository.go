package fleet

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/lineahq/linea/pluginsdk"
)

var (
	ErrUnavailable   = errors.New("fleet storage unavailable")
	ErrNotFound      = errors.New("resource not found")
	ErrConflict      = errors.New("resource conflict")
	ErrInvalid       = errors.New("invalid argument")
	ErrPrecondition  = errors.New("revision precondition failed")
	ErrCursorExpired = errors.New("event cursor expired; refresh inventory and groups")
)

// InvalidError is ErrInvalid with a reason the API returns to the caller.
// The reason names the field and the rule it broke, never the value.
type InvalidError struct{ Reason string }

func (e *InvalidError) Error() string        { return "invalid argument: " + e.Reason }
func (e *InvalidError) Is(target error) bool { return target == ErrInvalid }

// Invalid returns ErrInvalid with a reason; see InvalidError.
func Invalid(reason string) error { return &InvalidError{Reason: reason} }

// IDRule is the reason for a malformed resource ID.
const IDRule = "must be 32 lowercase hexadecimal characters"

// Repository isolates publication and reads from transport and database details.
// ConnectAgent and ObserveAgent commit before they return; SeenAgent only
// records liveness in memory and is persisted periodically. ConnectAgent
// returns the stored report: sections the agent did not send keep their
// stored values, so a reconnect's empty first message loses nothing.
type Repository interface {
	ConnectAgent(ctx context.Context, id, credential string, report Report, now time.Time) (uint64, Report, error)
	ObserveAgent(ctx context.Context, id, credential string, generation uint64, report Report, now time.Time) error
	SeenAgent(id string, generation uint64, now time.Time)
	DisconnectAgent(ctx context.Context, id string, generation uint64, now time.Time) error
	StopAgents(context.Context) error
	GetAgent(context.Context, string) (Agent, error)
	ListAgents(context.Context, AgentQuery) (Page, error)
	// Summary counts the inventory, optionally grouped by a promoted attribute.
	Summary(ctx context.Context, groupBy string) (Summary, error)
	Ready(context.Context) error
}

type Summary struct {
	Total        int64          `json:"total"`
	Connected    int64          `json:"connected"`
	Disconnected int64          `json:"disconnected"`
	Unknown      int64          `json:"unknown"`
	Groups       []SummaryGroup `json:"groups,omitempty"`
}

// SummaryGroup counts agents sharing one attribute value; "" means unset.
type SummaryGroup struct {
	Value     string `json:"value"`
	Total     int64  `json:"total"`
	Connected int64  `json:"connected"`
}

// MaxSummaryGroups bounds grouped summaries to the largest groups.
const MaxSummaryGroups = 100

// Group kinds: static groups list agent IDs, selector groups match attributes
// when read.
const (
	GroupStatic   = "static"
	GroupSelector = "selector"
	// MaxGroupMembers bounds static groups.
	MaxGroupMembers = 1000
)

type Group struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Revision int64  `json:"revision"`
	// MemberCount is omitted for selector groups in list pages, where counting
	// every selector would be unbounded work.
	MemberCount *int     `json:"member_count,omitempty"`
	AgentIDs    []string `json:"agent_ids,omitempty"`
	Selector    string   `json:"selector,omitempty"`
}

type GroupPage struct {
	Items     []Group `json:"items"`
	NextAfter string  `json:"next_after,omitempty"`
}

type Event struct {
	ID         int64           `json:"id,string"`
	Kind       string          `json:"kind"`
	SubjectID  string          `json:"subject_id"`
	ActorID    string          `json:"actor_id"`
	OccurredAt time.Time       `json:"occurred_at"`
	Detail     json.RawMessage `json:"detail,omitempty"`
	// ActingFor names the user a delegating credential acted for.
	ActingFor *pluginsdk.Acting `json:"acting_for,omitempty"`
}

type EventPage struct {
	Items      []Event `json:"items"`
	NextCursor string  `json:"next_cursor"`
}

type ControlRepository interface {
	GetGroup(context.Context, string) (Group, error)
	ListGroups(context.Context, string, int) (GroupPage, error)
	PutGroup(context.Context, string, Group, int64) (Group, error)
	DeleteGroup(context.Context, string, string, int64) error
	Events(context.Context, string, int) (EventPage, error)
}

// EventNotifier wakes long-polling readers. The channel closes on the next
// committed event hint; readers must still page, since hints can be lost.
type EventNotifier interface {
	EventSignal() <-chan struct{}
}

type PluginLifecycle struct {
	ID       string `json:"id"`
	Desired  string `json:"desired"`
	Revision int64  `json:"revision"`
}

type PluginLifecycleRepository interface {
	PluginLifecycles(context.Context) ([]PluginLifecycle, error)
	SetPluginLifecycle(context.Context, string, PluginLifecycle, int64) (PluginLifecycle, error)
}

// The memory store remains a deliberately ephemeral development adapter.
func (s *Store) ConnectAgent(_ context.Context, id, credential string, report Report, now time.Time) (uint64, Report, error) {
	generation, err := s.Connect(id, report, now)
	if err != nil {
		return 0, Report{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.agents[id]
	if r == nil || r.Generation != generation {
		return generation, report, nil
	}
	r.CredentialID = credential
	return generation, r.report(), nil
}
func (s *Store) ObserveAgent(_ context.Context, id, _ string, gen uint64, report Report, now time.Time) error {
	return s.Observe(id, gen, report, now)
}
func (s *Store) SeenAgent(id string, gen uint64, now time.Time) { s.Seen(id, gen, now) }
func (s *Store) DisconnectAgent(_ context.Context, id string, gen uint64, now time.Time) error {
	s.Disconnect(id, gen, now)
	return nil
}
func (s *Store) GetAgent(_ context.Context, id string) (Agent, error) {
	a, ok := s.Get(id)
	if !ok {
		return Agent{}, ErrNotFound
	}
	return a, nil
}
func (s *Store) ListAgents(_ context.Context, q AgentQuery) (Page, error) {
	return s.Query(q)
}
func (s *Store) Ready(context.Context) error { return nil }
func (s *Store) StopAgents(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	for _, r := range s.agents {
		if r.Connected {
			r.Connected, r.Presence = false, "disconnected"
			r.DisconnectedAt = &now
		}
	}
	return nil
}
func (s *Store) Summary(_ context.Context, groupBy string) (Summary, error) {
	if _, ok := Promoted[groupBy]; groupBy != "" && !ok {
		return Summary{}, Invalid("group_by " + PromotedRule)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	r := Summary{Total: int64(len(s.agents))}
	groups := map[string]*SummaryGroup{}
	for _, a := range s.agents {
		if a.Connected {
			r.Connected++
		} else {
			r.Disconnected++
		}
		if groupBy == "" {
			continue
		}
		v := a.Attributes[groupBy]
		g := groups[v]
		if g == nil {
			g = &SummaryGroup{Value: v}
			groups[v] = g
		}
		g.Total++
		if a.Connected {
			g.Connected++
		}
	}
	if groupBy != "" {
		r.Groups = []SummaryGroup{}
		for _, g := range groups {
			r.Groups = append(r.Groups, *g)
		}
		r.Groups = SortSummaryGroups(r.Groups)
	}
	return r, nil
}

// SortSummaryGroups orders the largest groups first and keeps the bound.
func SortSummaryGroups(groups []SummaryGroup) []SummaryGroup {
	slices.SortFunc(groups, func(a, b SummaryGroup) int {
		if a.Total != b.Total {
			return cmp.Compare(b.Total, a.Total)
		}
		return strings.Compare(a.Value, b.Value)
	})
	return groups[:min(len(groups), MaxSummaryGroups)]
}

// NormalizeGroup validates a group's members or selector and returns it in
// stored form: sorted unique agent IDs, or a canonical selector. Names are
// validated by the caller.
func NormalizeGroup(g Group) (Group, error) {
	if !ValidID(g.ID) {
		return g, Invalid("id " + IDRule)
	}
	if g.Selector != "" {
		if len(g.AgentIDs) > 0 {
			return g, Invalid("a group has either agent_ids or a selector, not both")
		}
		parsed, err := ParseSelector(g.Selector)
		if err != nil {
			return g, err
		}
		g.Kind, g.Selector, g.AgentIDs = GroupSelector, parsed.String(), nil
		return g, nil
	}
	if len(g.AgentIDs) > MaxGroupMembers {
		return g, Invalid(fmt.Sprintf("a static group has at most %d agent_ids", MaxGroupMembers))
	}
	for _, id := range g.AgentIDs {
		if !ValidID(id) {
			return g, Invalid("agent_ids " + IDRule)
		}
	}
	g.AgentIDs = slices.Clone(g.AgentIDs)
	slices.Sort(g.AgentIDs)
	g.Kind, g.AgentIDs = GroupStatic, slices.Compact(g.AgentIDs)
	return g, nil
}
