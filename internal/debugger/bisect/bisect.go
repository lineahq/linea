// Package bisect finds the fleet config version that broke a scenario: it
// rebuilds the configuration an agent receives from fleet's config layers
// (Compose), searches one config's versions with the scenarios' assertions as
// the test (Search), and describes the answer (Bisection). Like
// internal/debugger/upgrade it never executes; the platform runs the probes.
package bisect

import (
	"fmt"
	"time"

	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/comparison"
)

// Schema versions the bisection document.
const Schema = "0.1"

// Verdict is the bisection's answer.
type Verdict string

const (
	// Found: a good version is directly followed by a bad one.
	Found Verdict = "found"
	// NoRegression: the scenarios pass on the bad bound, so the range holds
	// nothing to find.
	NoRegression Verdict = "no_regression"
	// AlreadyBad: the scenarios already fail on the good bound.
	AlreadyBad Verdict = "already_bad"
	// Inconclusive: a bound could not be judged, or only versions that
	// could not be judged separate the last good and the first bad one.
	Inconclusive Verdict = "inconclusive"
)

// Reason codes of an inconclusive bisection.
const (
	// ReasonBoundSkipped: the good or bad bound could not be judged.
	ReasonBoundSkipped = "bound_not_judged"
	// ReasonSkippedBetween: versions that could not be judged lie between
	// the last good and the first bad version.
	ReasonSkippedBetween = "skipped_versions_between"
)

type Reason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Target is the agent and the config whose versions were searched. Layers
// are every config entry the agent receives, in name order; the searched
// one varies and the others stay at the versions fleet sends today.
type Target struct {
	AgentID      string  `json:"agent_id"`
	ConfigID     string  `json:"config_id"`
	ConfigName   string  `json:"config_name,omitempty"`
	AssignmentID string  `json:"assignment_id,omitempty"`
	Entry        string  `json:"entry"`
	Layers       []Layer `json:"layers"`
	// LocalFiles is the number of local supervisor files merged under the
	// layers.
	LocalFiles int `json:"local_files"`
}

// Layer is one config entry the agent receives.
type Layer struct {
	Entry    string `json:"entry"`
	ConfigID string `json:"config_id"`
	Version  int    `json:"version"`
	Searched bool   `json:"searched,omitempty"`
}

// Version is one candidate version, oldest first in a Bisection.
type Version struct {
	Version   int       `json:"version"`
	SHA256    string    `json:"sha256,omitempty"`
	Message   string    `json:"message,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by,omitempty"`
	Status    Status    `json:"status"`
	// Reason says why a skipped version could not be judged.
	Reason string `json:"reason,omitempty"`
	// Scenarios are the probe's results, in request order; empty when the
	// version was not probed or its configuration was refused first.
	Scenarios []ScenarioResult `json:"scenarios,omitempty"`
	// Rollouts are fleet's retained rollouts of the assignment to this
	// version.
	Rollouts []Rollout `json:"rollouts,omitempty"`
}

// ScenarioResult is one scenario on one version.
type ScenarioResult struct {
	Name    string            `json:"name"`
	Run     string            `json:"run,omitempty"`
	Outcome assertion.Outcome `json:"outcome"`
	Error   string            `json:"error,omitempty"`
}

// Rollout is a fleet rollout that moved the assignment to a version.
type Rollout struct {
	ID        string    `json:"id"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	CreatedBy string    `json:"created_by,omitempty"`
}

// Culprit explains the first bad version against the last good one.
type Culprit struct {
	Version  int `json:"version"`
	LastGood int `json:"last_good"`
	// Diff is the unified diff of the searched config's bodies.
	Diff string `json:"diff"`
	// Scenarios compare the last good and the first bad version's run of
	// each scenario whose assertions ended differently.
	Scenarios []CulpritScenario `json:"scenarios"`
}

// CulpritScenario is one scenario's run on the last good and the first bad
// version, compared on identical inputs when InputsIdentical is set.
type CulpritScenario struct {
	Name            string                 `json:"name"`
	Good            string                 `json:"good"`
	Bad             string                 `json:"bad"`
	GoodAssertions  assertion.Outcome      `json:"good_assertions"`
	BadAssertions   assertion.Outcome      `json:"bad_assertions"`
	InputsIdentical bool                   `json:"inputs_identical"`
	Error           string                 `json:"error,omitempty"`
	Comparison      *comparison.Comparison `json:"comparison,omitempty"`
}

// Bisection is the answer to "which version of this config broke the
// scenarios?".
type Bisection struct {
	Schema  string   `json:"bisect_schema"`
	Verdict Verdict  `json:"verdict"`
	Reasons []Reason `json:"reasons,omitempty"`
	Target  Target   `json:"target"`
	// Good and Bad are the searched range's bounds.
	Good int `json:"good"`
	Bad  int `json:"bad"`
	// FirstBad and LastGood are versions; zero when unknown.
	FirstBad int       `json:"first_bad,omitempty"`
	LastGood int       `json:"last_good,omitempty"`
	Probes   int       `json:"probes"`
	Versions []Version `json:"versions"`
	Culprit  *Culprit  `json:"culprit,omitempty"`
	Notes    []string  `json:"notes,omitempty"`
}

// Judge returns a probe's status from its scenarios: bad when any failed,
// even if another could not run, skipped when any could not establish an
// outcome, else good. A probe without scenarios is skipped.
func Judge(results []ScenarioResult) Status {
	judged := len(results) > 0
	for _, r := range results {
		switch r.Outcome {
		case assertion.Fail:
			return Bad
		case assertion.Pass:
		default:
			judged = false
		}
	}
	if !judged {
		return Skipped
	}
	return Good
}

// Assemble describes a finished search over versions, oldest first. The
// culprit is added by the caller once it has compared the runs.
func Assemble(target Target, versions []Version, out Outcome) *Bisection {
	b := &Bisection{Schema: Schema, Verdict: out.Verdict, Target: target, Probes: out.Probes, Versions: versions}
	if len(versions) > 0 {
		b.Good, b.Bad = versions[0].Version, versions[len(versions)-1].Version
	}
	for i := range b.Versions {
		if i < len(out.Statuses) {
			b.Versions[i].Status = out.Statuses[i]
		}
	}
	if out.FirstBad >= 0 {
		b.FirstBad = versions[out.FirstBad].Version
	}
	if out.LastGood >= 0 {
		b.LastGood = versions[out.LastGood].Version
	}
	if b.Verdict != Inconclusive {
		return b
	}
	switch {
	case out.LastGood < 0 && out.FirstBad < 0:
		b.reason(ReasonBoundSkipped, fmt.Sprintf("version %d, the bad bound, could not be judged", b.Bad))
	case out.LastGood < 0:
		b.reason(ReasonBoundSkipped, fmt.Sprintf("version %d, the good bound, could not be judged", b.Good))
	default:
		b.reason(ReasonSkippedBetween, fmt.Sprintf("the first bad version is between %d and %d, but the versions between could not be judged", b.LastGood+1, b.FirstBad))
	}
	return b
}

func (b *Bisection) reason(code, message string) {
	b.Reasons = append(b.Reasons, Reason{Code: code, Message: message})
}
