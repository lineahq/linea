package configmgmt

import (
	"cmp"
	"context"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/lineahq/linea/internal/fleet/fleet"
)

// MaxConfigs bounds config documents; each keeps at most KeepVersions
// versions plus those an assignment references.
const (
	MaxConfigs   = 1000
	KeepVersions = 100
)

// ValidText bounds descriptions and change messages: UTF-8, at most 1 KiB,
// no control characters other than newlines.
// TextRule is the reason for text ValidText refuses.
const TextRule = "must be at most 1,024 bytes of UTF-8 without control characters other than newlines"

func ValidText(s string) bool {
	return len(s) <= 1024 && utf8.ValidString(s) && strings.IndexFunc(s, func(r rune) bool { return r != '\n' && unicode.IsControl(r) }) < 0
}

// Config is a named container of immutable versions.
type Config struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Description   string    `json:"description"`
	Revision      int64     `json:"revision"`
	LatestVersion int       `json:"latest_version"`
	CreatedAt     time.Time `json:"created_at"`
	CreatedBy     string    `json:"created_by"`
}

type ConfigPage struct {
	Items     []Config `json:"items"`
	NextAfter string   `json:"next_after,omitempty"`
}

// Version is one immutable config body. Body is set only for admin reads.
type Version struct {
	ConfigID  string    `json:"config_id"`
	Version   int       `json:"version"`
	SHA256    string    `json:"sha256"`
	Size      int       `json:"size"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
	Body      *string   `json:"body,omitempty"`
}

type VersionPage struct {
	Items     []Version `json:"items"`
	NextAfter string    `json:"next_after,omitempty"`
}

// Assignment binds a config version, as a layer, to a group or an inline
// selector, optionally narrowed to credentials carrying labels. Version is 0
// while the rollout creating the assignment runs: until it completes, only
// its released targets receive the layer. RolloutID names the active
// rollout, if any.
type Assignment struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	ConfigID         string            `json:"config_id"`
	Version          int               `json:"version,omitempty"`
	RolloutID        string            `json:"rollout_id,omitempty"`
	Layer            int               `json:"layer"`
	GroupID          string            `json:"group_id,omitempty"`
	Selector         string            `json:"selector,omitempty"`
	CredentialLabels map[string]string `json:"credential_labels,omitempty"`
	Revision         int64             `json:"revision"`
	CreatedAt        time.Time         `json:"created_at"`
	CreatedBy        string            `json:"created_by"`
}

type AssignmentPage struct {
	Items     []Assignment `json:"items"`
	NextAfter string       `json:"next_after,omitempty"`
}

// Repository stores desired state. Writes require an admin actor, record an
// event without bodies and notify replicas when desired state changes.
type Repository interface {
	CreateConfig(ctx context.Context, actor string, c Config) (Config, error)
	GetConfig(ctx context.Context, id string) (Config, error)
	ListConfigs(ctx context.Context, after string, limit int) (ConfigPage, error)
	UpdateConfig(ctx context.Context, actor string, c Config, expected int64) (Config, error)
	DeleteConfig(ctx context.Context, actor, id string, expected int64) error
	CreateVersion(ctx context.Context, actor, configID string, body []byte, message string) (Version, error)
	GetVersion(ctx context.Context, configID string, version int, body bool) (Version, error)
	ListVersions(ctx context.Context, configID string, after, limit int) (VersionPage, error)
	// CreateAssignment stores an assignment without an active version and
	// starts the rollout r to a.Version.
	CreateAssignment(ctx context.Context, actor string, a Assignment, r RolloutRequest) (Assignment, RolloutView, error)
	GetAssignment(ctx context.Context, id string) (Assignment, error)
	ListAssignments(ctx context.Context, after string, limit int) (AssignmentPage, error)
	// CreateRollout changes or deletes an assignment through a rollout.
	CreateRollout(ctx context.Context, actor string, r RolloutRequest) (RolloutView, error)
	GetRollout(ctx context.Context, id string) (RolloutView, error)
	ListRollouts(ctx context.Context, after string, limit int) (RolloutPage, error)
	ListRolloutTargets(ctx context.Context, id string, outcome Outcome, after string, limit int) (RolloutTargetPage, error)
	// ControlRollout pauses, resumes, rolls back or cancels a rollout.
	ControlRollout(ctx context.Context, actor, id string, action Control) (RolloutView, error)
	// Targets returns every assignment with its resolved target.
	Targets(ctx context.Context) ([]Target, error)
	// ClearHold releases an agent's failure hold so its desired config is
	// sent again.
	ClearHold(ctx context.Context, actor, agentID string) error
	// ComponentSet returns the component types of an agent build, or
	// fleet.ErrNotFound when the build has not reported them.
	ComponentSet(ctx context.Context, hash string) (Components, error)
}

// NormalizeAssignment checks an assignment's fields and returns its stored
// form with a canonical selector.
func NormalizeAssignment(a Assignment, validLabels func(map[string]string) bool) (Assignment, error) {
	switch {
	case !fleet.ValidID(a.ID):
		return a, fleet.Invalid("id " + fleet.IDRule)
	case !fleet.ValidID(a.ConfigID):
		return a, fleet.Invalid("config_id " + fleet.IDRule)
	case !ValidAssignmentName(a.Name):
		return a, fleet.Invalid(AssignmentNameRule)
	case a.Version < 1:
		return a, fleet.Invalid("version must be at least 1")
	case a.Layer < 0 || a.Layer > MaxLayer:
		return a, fleet.Invalid(fmt.Sprintf("layer must be 0 to %d", MaxLayer))
	case (a.GroupID == "") == (a.Selector == ""):
		return a, fleet.Invalid("an assignment has either a group_id or a selector")
	case a.GroupID != "" && !fleet.ValidID(a.GroupID):
		return a, fleet.Invalid("group_id " + fleet.IDRule)
	case !validLabels(a.CredentialLabels):
		return a, fleet.Invalid("credential_labels are invalid")
	}
	if a.Selector != "" {
		s, err := fleet.ParseSelector(a.Selector)
		if err != nil {
			return a, err
		}
		if s.Empty() {
			return a, fleet.Invalid("selector must have a term")
		}
		a.Selector = s.String()
	}
	if len(a.CredentialLabels) == 0 {
		a.CredentialLabels = nil
	}
	return a, nil
}

// Target is an assignment with its target resolved for in-memory matching:
// an inline or group selector, or a static group's members.
type Target struct {
	Assignment
	// Digest is the SHA-256 of the assigned version body; nil while the
	// assignment has no active version.
	Digest []byte
	// Override is the running or paused rollout of this assignment.
	Override *Override
	selector fleet.Selector
	members  map[string]struct{}
}

// Override is what an overriding rollout gives its released targets: another
// version, or no layer at all when Version is 0 (a deletion).
type Override struct {
	RolloutID string
	Version   int
	Digest    []byte
	Released  map[string]struct{}
}

// NewTarget resolves an assignment. For a group target, groupSelector is the
// group's selector, or empty with members for a static group.
func NewTarget(a Assignment, digest []byte, groupSelector string, members []string) (Target, error) {
	t := Target{Assignment: a, Digest: digest}
	text := a.Selector
	if a.GroupID != "" {
		text = groupSelector
	}
	if text == "" {
		t.members = make(map[string]struct{}, len(members))
		for _, id := range members {
			t.members[id] = struct{}{}
		}
		return t, nil
	}
	s, err := fleet.ParseSelector(text)
	if err != nil {
		return t, err
	}
	t.selector = s
	return t, nil
}

// Matches reports whether an agent with these attributes and credential
// labels is targeted. Required labels must all be present with equal values.
func (t Target) Matches(agentID string, attributes, labels map[string]string) bool {
	for k, v := range t.CredentialLabels {
		if got, ok := labels[k]; !ok || got != v {
			return false
		}
	}
	if t.members != nil {
		_, ok := t.members[agentID]
		return ok
	}
	return t.selector.Matches(attributes)
}

// LayerRef describes one entry of an agent's desired config without its body.
type LayerRef struct {
	Name         string `json:"name"`
	AssignmentID string `json:"assignment_id,omitempty"`
	ConfigID     string `json:"config_id,omitempty"`
	Version      int    `json:"version,omitempty"`
	SHA256       string `json:"sha256"`
}

// Desired config of one agent: the layers to send and their hash. Unmanaged
// is set for the single entry releasing a formerly managed agent.
type DesiredConfig struct {
	Layers    []LayerRef `json:"layers"`
	Hash      string     `json:"hash"`
	Unmanaged bool       `json:"unmanaged,omitempty"`
}

// Match returns the targets an agent matches in entry order, each with the
// version the agent receives: a rollout's version for its released targets,
// otherwise the assignment's. A target without a version for this agent is
// skipped.
func Match(targets []Target, agentID string, attributes, labels map[string]string) []Target {
	var out []Target
	for _, t := range targets {
		if !t.Matches(agentID, attributes, labels) {
			continue
		}
		if o := t.Override; o != nil {
			if _, released := o.Released[agentID]; released {
				t.Version, t.Digest = o.Version, o.Digest
			}
		}
		if t.Version > 0 {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b Target) int {
		return cmp.Or(cmp.Compare(a.Layer, b.Layer), strings.Compare(a.Name, b.Name))
	})
	return out
}

// Describe returns the desired config for matched targets, using the same
// names and hash as Desired. reported is the agent's last remote config
// hash; with no match, a reported hash means the agent must be released.
// It returns nil when the agent was never managed and matches nothing.
func Describe(matched []Target, reported string) (*DesiredConfig, error) {
	if len(matched) == 0 {
		if reported == "" {
			return nil, nil
		}
		p := Unmanage()
		digest := digestOf(unmanagedBody)
		return &DesiredConfig{Layers: []LayerRef{{Name: UnmanagedEntry, SHA256: hex.EncodeToString(digest)}}, Hash: hex.EncodeToString(p.Hash), Unmanaged: true}, nil
	}
	if len(matched) > MaxLayers {
		return nil, ErrTooManyLayers
	}
	d := &DesiredConfig{Layers: make([]LayerRef, 0, len(matched))}
	names, digests := make([]string, 0, len(matched)), make([][]byte, 0, len(matched))
	for _, t := range matched {
		name := fmt.Sprintf("%02d-%s", t.Layer, t.Name)
		names, digests = append(names, name), append(digests, t.Digest)
		d.Layers = append(d.Layers, LayerRef{Name: name, AssignmentID: t.ID, ConfigID: t.ConfigID, Version: t.Version, SHA256: hex.EncodeToString(t.Digest)})
	}
	d.Hash = hex.EncodeToString(hashDigests(names, digests))
	return d, nil
}
