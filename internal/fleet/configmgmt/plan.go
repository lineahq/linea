package configmgmt

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// UnmanagedEntry is the single entry sent to an agent that was managed and
// no longer matches any assignment. Its empty mapping leaves the supervisor's
// local config files in charge. An empty config map is never sent: the
// supervisor stops the Collector on one and still reports success.
const UnmanagedEntry = "00-unmanaged"

var unmanagedBody = []byte("{}")

var assignmentName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidAssignmentName reports whether name can form a config map entry.
func ValidAssignmentName(name string) bool { return assignmentName.MatchString(name) }

// AssignmentNameRule is the reason for a name ValidAssignmentName refuses.
const AssignmentNameRule = "name must be 1 to 63 lowercase letters, digits or hyphens, starting with a letter or digit"

// ErrTooManyLayers means more than MaxLayers assignments match one agent.
var ErrTooManyLayers = errors.New("too many matching layers")

// Layer is one matched assignment's version body.
type Layer struct {
	Layer int
	Name  string
	Body  []byte
}

// Entry is one AgentConfigMap entry.
type Entry struct {
	Name string
	Body []byte
}

// Plan is the config map an agent should run and its hash.
type Plan struct {
	Entries []Entry
	Hash    []byte
}

// Unmanaged reports whether the plan only releases a formerly managed agent.
func (p Plan) Unmanaged() bool {
	return len(p.Entries) == 1 && p.Entries[0].Name == UnmanagedEntry
}

// Desired orders matched layers by (layer, name) into named entries. It
// returns ok=false when nothing matches; the caller then decides whether the
// agent must be released with Unmanage.
func Desired(layers []Layer) (Plan, bool, error) {
	if len(layers) == 0 {
		return Plan{}, false, nil
	}
	if len(layers) > MaxLayers {
		return Plan{}, false, ErrTooManyLayers
	}
	sorted := slices.Clone(layers)
	slices.SortFunc(sorted, func(a, b Layer) int {
		if a.Layer != b.Layer {
			return a.Layer - b.Layer
		}
		return strings.Compare(a.Name, b.Name)
	})
	entries := make([]Entry, len(sorted))
	for i, l := range sorted {
		if l.Layer < 0 || l.Layer > MaxLayer || !ValidAssignmentName(l.Name) {
			return Plan{}, false, fmt.Errorf("invalid layer %d %q", l.Layer, l.Name)
		}
		if i > 0 && sorted[i-1].Layer == l.Layer && sorted[i-1].Name == l.Name {
			return Plan{}, false, fmt.Errorf("duplicate layer %q", l.Name)
		}
		entries[i] = Entry{Name: fmt.Sprintf("%02d-%s", l.Layer, l.Name), Body: l.Body}
	}
	return Plan{Entries: entries, Hash: Hash(entries)}, true, nil
}

// Unmanage is the plan that releases a formerly managed agent.
func Unmanage() Plan {
	entries := []Entry{{Name: UnmanagedEntry, Body: unmanagedBody}}
	return Plan{Entries: entries, Hash: Hash(entries)}
}

// Hash is SHA-256 over the name-sorted "name\0sha256(body)\n" lines. Entry
// names cannot contain NUL or newline, so the encoding is unambiguous.
func Hash(entries []Entry) []byte {
	names, digests := make([]string, len(entries)), make([][]byte, len(entries))
	for i, e := range entries {
		names[i], digests[i] = e.Name, digestOf(e.Body)
	}
	return hashDigests(names, digests)
}

func digestOf(body []byte) []byte {
	d := sha256.Sum256(body)
	return d[:]
}

// hashDigests hashes name-sorted (name, body digest) pairs.
func hashDigests(names []string, digests [][]byte) []byte {
	order := make([]int, len(names))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(a, b int) int { return strings.Compare(names[a], names[b]) })
	h := sha256.New()
	for _, i := range order {
		h.Write([]byte(names[i]))
		h.Write([]byte{0})
		h.Write(digests[i])
		h.Write([]byte{'\n'})
	}
	return h.Sum(nil)
}

// State is a remote config status reported by an agent.
type State string

const (
	Unset    State = ""
	Applying State = "applying"
	Applied  State = "applied"
	Failed   State = "failed"
)

// Status is an agent's last reported RemoteConfigStatus.
type Status struct {
	Hash  []byte
	State State
}

// Agent is everything the send rule needs about one session. Ownership of
// the session at its current generation is checked by the caller.
type Agent struct {
	Enabled             bool
	CredentialAllowed   bool
	AcceptsRemoteConfig bool
	Status              Status
	Held                []byte
}

// Decision is the outcome of the send rule.
type Decision struct {
	Send      bool
	Plan      Plan
	ClearHold bool
	Held      bool
	// Missing lists component types the agent's build lacks; the plan is
	// not sent.
	Missing []string
}

// Decide applies the send rule. desired is the plan from Desired with its ok
// result; when nothing matches, an agent that has reported a remote config
// hash other than the unmanaged one is released, and one that never had a
// remote config is left alone.
func Decide(a Agent, desired Plan, matched bool) Decision {
	if !a.Enabled || !a.CredentialAllowed || !a.AcceptsRemoteConfig {
		return Decision{}
	}
	plan := desired
	if !matched {
		if len(a.Status.Hash) == 0 {
			return Decision{ClearHold: len(a.Held) > 0}
		}
		plan = Unmanage()
	}
	var d Decision
	if len(a.Held) > 0 {
		if bytes.Equal(a.Held, plan.Hash) {
			return Decision{Held: true}
		}
		d.ClearHold = true
	}
	if !bytes.Equal(a.Status.Hash, plan.Hash) {
		d.Send, d.Plan = true, plan
	}
	return d
}

// NextHold returns the hash to hold after an agent reports next, given its
// previous status: a failed hash, or one that was applying when the agent
// reported another hash as applied (a local rollback). Otherwise the hold is
// unchanged.
func NextHold(prev, next Status, held []byte) []byte {
	if next.State == Failed && len(next.Hash) > 0 {
		return next.Hash
	}
	if prev.State == Applying && next.State == Applied && len(prev.Hash) > 0 && !bytes.Equal(prev.Hash, next.Hash) {
		return prev.Hash
	}
	return held
}
