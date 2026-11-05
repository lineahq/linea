// Package upgrade decides a Collector upgrade check from evidence the
// platform gathered: the configuration inspected on both runtimes, both
// runtimes' feature gates and one run comparison per scenario. Like
// internal/debugger/comparison it is a pure projection: it never executes, and the
// runtime difference alone never makes a scenario different.
package upgrade

import (
	"fmt"
	"slices"
	"strings"

	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/comparison"
	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/release"
)

// Schema versions the upgrade check document.
const Schema = "0.1"

// Verdict is the upgrade check's answer.
type Verdict string

const (
	// Compatible: the target loads the configuration and every scenario
	// behaves the same with the same assertion outcomes.
	Compatible Verdict = "compatible"
	// ConfigurationBreaks: the target refuses a configuration the current
	// runtime accepts.
	ConfigurationBreaks Verdict = "configuration_breaks"
	// BehaviorChanges: a scenario's comparison is different, or an
	// assertion outcome changed.
	BehaviorChanges Verdict = "behavior_changes"
	// Inconclusive: the evidence cannot answer; the reasons say why.
	Inconclusive Verdict = "inconclusive"
)

// Reason codes of an inconclusive check.
const (
	// ReasonFromInvalid: the current runtime refuses the configuration, so
	// the target cannot be judged against it.
	ReasonFromInvalid = "from_configuration_invalid"
	// ReasonScenarioInconclusive: a scenario's comparison is inconclusive.
	ReasonScenarioInconclusive = "scenario_inconclusive"
	// ReasonScenarioFailed: a scenario's runs could not be compared.
	ReasonScenarioFailed = "scenario_failed"
)

// Runtime is one side of the check.
type Runtime struct {
	// Name is the runtime's name, "built-in" for the debugger's own.
	Name             string `json:"name"`
	CollectorVersion string `json:"collector_version"`
	// Verified is false for a release outside the support table.
	Verified bool `json:"verified"`
}

// Inspection is the configuration's inspection on one runtime.
type Inspection struct {
	Valid       bool
	Diagnostics []config.Diagnostic
}

// Input is the evidence of one check.
type Input struct {
	From, To Runtime
	// FromConfig and ToConfig are the configuration's inspections.
	FromConfig, ToConfig Inspection
	// FromGates and ToGates are each runtime's feature gates; nil when the
	// runtime does not list them.
	FromGates, ToGates []release.Gate
	// Components are the configuration's "<kind>/<type>" components, which
	// select the component gates that matter.
	Components []string
	// Scenarios are the scenarios' results, in request order; empty when
	// the target refused the configuration or none were given.
	Scenarios []Scenario
}

// Check is the upgrade check.
type Check struct {
	Schema  string   `json:"upgrade_check_schema"`
	Verdict Verdict  `json:"verdict"`
	Reasons []Reason `json:"reasons,omitempty"`
	From    Runtime  `json:"from"`
	To      Runtime  `json:"to"`
	// Configuration compares the configuration's diagnostics.
	Configuration Configuration `json:"configuration"`
	FeatureGates  FeatureGates  `json:"feature_gates"`
	Scenarios     []Scenario    `json:"scenarios"`
	Notes         []string      `json:"notes,omitempty"`
}

type Reason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Configuration compares the configuration on both runtimes.
type Configuration struct {
	FromValid bool `json:"from_valid"`
	ToValid   bool `json:"to_valid"`
	// Added are the target's diagnostics the current runtime does not
	// report, Removed the current runtime's the target no longer reports.
	Added   []config.Diagnostic `json:"added"`
	Removed []config.Diagnostic `json:"removed"`
}

// FeatureGates lists the gates whose stage or default differs, or that
// exist on one side only: every gate a component of the configuration
// links and every gate the Collector itself links.
type FeatureGates struct {
	Changes []GateChange `json:"changes"`
	// Unknown names the sides ("from", "to") whose runtime does not list
	// its gates; no change is claimed for them.
	Unknown []string `json:"unknown,omitempty"`
}

// GateChange is one gate that differs.
type GateChange struct {
	ID string `json:"id"`
	// Component is the configuration's "<kind>/<type>" the gate belongs
	// to (the first, when several link it); empty for a gate of the
	// Collector itself.
	Component string `json:"component,omitempty"`
	// From and To are the gate on each runtime; nil where it does not
	// exist.
	From        *GateState `json:"from,omitempty"`
	To          *GateState `json:"to,omitempty"`
	Description string     `json:"description,omitempty"`
}

// GateState is a gate's stage and default on one runtime.
type GateState struct {
	Stage   string `json:"stage"`
	Enabled bool   `json:"enabled"`
}

// Scenario is one scenario run on both runtimes on one captured input.
type Scenario struct {
	// Name is the scenario's label or path.
	Name string `json:"name"`
	// Before and After are the runs on the current and target runtimes.
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
	// Outcome is the comparison's; empty when the runs were not compared.
	Outcome          comparison.Outcome `json:"outcome,omitempty"`
	BeforeAssertions assertion.Outcome  `json:"before_assertions,omitempty"`
	AfterAssertions  assertion.Outcome  `json:"after_assertions,omitempty"`
	// InputsIdentical is the comparison's proof, by digest, that both runs
	// had the same captured inputs.
	InputsIdentical bool   `json:"inputs_identical"`
	InputsDigest    string `json:"inputs_digest,omitempty"`
	// Error says why the scenario was not compared.
	Error      string                 `json:"error,omitempty"`
	Comparison *comparison.Comparison `json:"comparison,omitempty"`
}

// ScenarioOf summarizes a comparison of a scenario's two runs.
func ScenarioOf(name string, c *comparison.Comparison) Scenario {
	s := Scenario{Name: name, Before: c.Before.ID, After: c.After.ID, Outcome: c.Outcome,
		BeforeAssertions: c.Before.Assertions, AfterAssertions: c.After.Assertions,
		InputsIdentical: c.InputsIdentical, Comparison: c}
	if c.InputsIdentical {
		s.InputsDigest = c.Before.InputsDigest
	}
	return s
}

// AssertionsChanged reports whether the scenario's assertions ended
// differently on the two runtimes.
func (s Scenario) AssertionsChanged() bool {
	return s.BeforeAssertions != s.AfterAssertions
}

// Assess decides the check.
func Assess(in Input) *Check {
	c := &Check{Schema: Schema, From: in.From, To: in.To,
		Configuration: diffDiagnostics(in.FromConfig, in.ToConfig),
		FeatureGates:  diffGates(in.FromGates, in.ToGates, in.Components),
		Scenarios:     slices.Clone(in.Scenarios)}
	if c.Scenarios == nil {
		c.Scenarios = []Scenario{}
	}
	if !in.From.Verified || !in.To.Verified {
		c.Notes = append(c.Notes, "an unverified Collector release is part of this check")
	}
	switch {
	case !in.FromConfig.Valid:
		c.Verdict = Inconclusive
		c.reason(ReasonFromInvalid, fmt.Sprintf("Collector %s refuses the configuration, so the upgrade cannot be judged against it", in.From.CollectorVersion))
		return c
	case !in.ToConfig.Valid:
		c.Verdict = ConfigurationBreaks
		return c
	}
	if len(in.Scenarios) == 0 {
		c.Notes = append(c.Notes, "no scenario was given, so behavior was not compared")
	}
	changed := false
	for _, s := range in.Scenarios {
		switch {
		case s.Error != "":
			c.reason(ReasonScenarioFailed, fmt.Sprintf("scenario %s was not compared: %s", s.Name, s.Error))
		case s.Outcome == comparison.Different || s.AssertionsChanged():
			changed = true
		case s.Outcome == comparison.Inconclusive:
			c.reason(ReasonScenarioInconclusive, fmt.Sprintf("the comparison of scenario %s is inconclusive", s.Name))
		}
	}
	switch {
	case changed:
		c.Verdict = BehaviorChanges
	case len(c.Reasons) > 0:
		c.Verdict = Inconclusive
	default:
		c.Verdict = Compatible
	}
	return c
}

func (c *Check) reason(code, message string) {
	c.Reasons = append(c.Reasons, Reason{Code: code, Message: message})
}

func diffDiagnostics(from, to Inspection) Configuration {
	out := Configuration{FromValid: from.Valid, ToValid: to.Valid, Added: []config.Diagnostic{}, Removed: []config.Diagnostic{}}
	for _, d := range to.Diagnostics {
		if !slices.Contains(from.Diagnostics, d) {
			out.Added = append(out.Added, d)
		}
	}
	for _, d := range from.Diagnostics {
		if !slices.Contains(to.Diagnostics, d) {
			out.Removed = append(out.Removed, d)
		}
	}
	return out
}

// componentKinds are the gate ID prefixes of component gates
// ("processor.transform.…") and their manifest kinds.
var componentKinds = map[string]string{
	"receiver": "receivers", "processor": "processors", "exporter": "exporters",
	"connector": "connectors", "extension": "extensions",
}

// gateComponent returns the configuration component a gate belongs to, or
// "" for a gate that concerns every configuration; ok is false for a gate
// no component of the configuration links. A side that knows the gate's
// scope decides: a Collector gate concerns every configuration, a
// component gate the configuration's components linking it, and a gate
// only the debugger links none. Without a scope on either side (workers
// from before scopes, or a gate the build did not find), the gate ID
// decides: "<kind>.<type>.…" is that component's, anything else every
// configuration's.
func gateComponent(id string, sides []release.Gate, components []string) (string, bool) {
	scoped := false
	for _, g := range sides {
		switch g.Scope {
		case release.GateScopeCollector:
			return "", true
		case release.GateScopeComponent:
			scoped = true
			for _, c := range components {
				if slices.Contains(g.Components, c) {
					return c, true
				}
			}
		case release.GateScopeDebugger:
			scoped = true
		}
	}
	if scoped {
		return "", false
	}
	prefix, rest, _ := strings.Cut(id, ".")
	kind, isComponent := componentKinds[prefix]
	if !isComponent {
		return "", true
	}
	segment, _, _ := strings.Cut(rest, ".")
	segment = strings.ReplaceAll(strings.ToLower(segment), "_", "")
	for _, c := range components {
		k, typ, _ := strings.Cut(c, "/")
		if k == kind && strings.HasPrefix(segment, strings.ReplaceAll(typ, "_", "")) {
			return c, true
		}
	}
	return "", false
}

func diffGates(from, to []release.Gate, components []string) FeatureGates {
	out := FeatureGates{Changes: []GateChange{}}
	if from == nil {
		out.Unknown = append(out.Unknown, "from")
	}
	if to == nil {
		out.Unknown = append(out.Unknown, "to")
	}
	if len(out.Unknown) > 0 {
		return out
	}
	byID := map[string]*GateChange{}
	sides := map[string][]release.Gate{}
	var ids []string
	add := func(g release.Gate, side func(*GateChange) **GateState) {
		sides[g.ID] = append(sides[g.ID], g)
		ch := byID[g.ID]
		if ch == nil {
			ch = &GateChange{ID: g.ID, Description: g.Description}
			byID[g.ID] = ch
			ids = append(ids, g.ID)
		}
		*side(ch) = &GateState{Stage: g.Stage, Enabled: g.Enabled}
	}
	for _, g := range from {
		add(g, func(c *GateChange) **GateState { return &c.From })
	}
	for _, g := range to {
		add(g, func(c *GateChange) **GateState { return &c.To })
	}
	slices.Sort(ids)
	for _, id := range ids {
		ch := byID[id]
		if ch.From != nil && ch.To != nil && *ch.From == *ch.To {
			continue
		}
		component, ok := gateComponent(id, sides[id], components)
		if !ok {
			continue
		}
		ch.Component = component
		out.Changes = append(out.Changes, *ch)
	}
	return out
}
