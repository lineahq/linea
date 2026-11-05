package platform

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/pmezard/go-difflib/difflib"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/bisect"
	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/fleetcapture"
)

// Fleet config bisect under platform names; the definitions live in
// internal/debugger/bisect.
type (
	Bisection             = bisect.Bisection
	BisectVerdict         = bisect.Verdict
	BisectStatus          = bisect.Status
	BisectReason          = bisect.Reason
	BisectTarget          = bisect.Target
	BisectLayer           = bisect.Layer
	BisectVersion         = bisect.Version
	BisectScenarioResult  = bisect.ScenarioResult
	BisectRollout         = bisect.Rollout
	BisectCulprit         = bisect.Culprit
	BisectCulpritScenario = bisect.CulpritScenario
)

// Bisection verdicts and version statuses.
const (
	BisectFound        = bisect.Found
	BisectNoRegression = bisect.NoRegression
	BisectAlreadyBad   = bisect.AlreadyBad
	BisectInconclusive = bisect.Inconclusive

	BisectGood     = bisect.Good
	BisectBad      = bisect.Bad
	BisectSkipped  = bisect.Skipped
	BisectUntested = bisect.Untested
)

// Bisect bounds: fleet keeps a config's newest 100 versions, and each probe
// runs every scenario.
const (
	maxBisectVersions  = 100
	maxBisectScenarios = 16
	maxBisectLocal     = 8
	maxRolloutPages    = 10
)

// BisectRequest searches one fleet config's versions for the first that
// makes the scenarios fail on an agent.
type BisectRequest struct {
	// AgentID is the fleet agent whose configuration is rebuilt.
	AgentID string
	// ConfigID is the searched config; empty when the agent receives
	// exactly one layer.
	ConfigID string
	// Good and Bad bound the range. Zero Good is the oldest version fleet
	// still keeps; zero Bad the version fleet sends the agent now.
	Good, Bad int
	// Scenarios are the test: each probe runs every one, and a version is
	// bad when an assertion fails. Each must declare assertions.
	Scenarios []ScenarioInput
	// Local are the supervisor's local config files, merged under fleet's
	// layers in order; fleet never sees them.
	Local []Source
	// Runtime runs the probes; empty is the built-in one.
	Runtime string
	// Ignore lists OTTL paths whose differences do not count when the
	// culprit's runs are compared, as in CompareRequest.
	Ignore []string
}

// BisectFleetConfig answers which version of a fleet config made the
// scenarios fail on an agent. It reads the layers fleet sends the
// agent, rebuilds the agent's configuration for each probed version of the
// searched config as the OpAMP supervisor merges it (bisect.Compose), with
// the other layers at the versions fleet sends now, and runs the scenarios on
// it. It probes the bad bound, the good bound and then halves the range; a
// version whose configuration is invalid or whose runs are inconclusive is
// skipped. Once it knows a last good and a first bad version, adjacent or
// separated only by skipped ones, it diffs their bodies and compares the
// runs of each scenario that changed.
// It waits for every run; when ctx ends first, it cancels the run it was
// waiting for and returns the context's error.
func (s *Service) BisectFleetConfig(ctx context.Context, caller Caller, req BisectRequest) (_ *Bisection, err error) {
	defer s.audit(&caller, "BisectFleetConfig", &err)()
	if err := s.begin(ctx, caller, authz.RunSubmit); err != nil {
		return nil, err
	}
	ctx = actingFor(ctx, caller)
	if err := req.validate(); err != nil {
		return nil, err
	}
	if err := s.collectors.checkFleet(); err != nil {
		var e *Error
		if errors.As(err, &e) && e.Code == CodeUnavailable {
			return nil, &Error{Code: CodeUnavailable, Message: "config bisect needs a fleet connection; start linea-server with --fleet-url"}
		}
		return nil, err
	}
	fleet, ok := s.collectors.fleet.(fleetConfigs)
	if !ok {
		return nil, &Error{Code: CodeUnavailable, Message: "the fleet connection cannot read config versions"}
	}
	local, err := s.bisectLocal(ctx, caller, req.Local)
	if err != nil {
		return nil, err
	}
	b := &bisection{s: s, caller: caller, req: req, fleet: fleet, local: local, bodies: map[string][]byte{}}
	if err := b.target(ctx); err != nil {
		return nil, err
	}
	if err := b.candidates(ctx); err != nil {
		return nil, err
	}
	out, err := bisect.Search(ctx, len(b.versions), b.probe)
	if err != nil {
		if ctx.Err() != nil {
			return nil, classify(ctx.Err(), CodeCancelled)
		}
		return nil, err
	}
	result := bisect.Assemble(b.t, b.versions, out)
	result.Notes = append(b.notes, result.Notes...)
	if out.LastGood >= 0 && out.FirstBad >= 0 {
		if result.Culprit, err = b.culprit(ctx, out.LastGood, out.FirstBad); err != nil {
			return nil, err
		}
	}
	b.rollouts(ctx, result)
	return result, nil
}

func (r BisectRequest) validate() error {
	invalid := func(message string) error { return &Error{Code: CodeInvalidRequest, Message: message} }
	switch {
	case !collectorText(r.AgentID, 128, true):
		return invalid("agent must be a fleet agent ID")
	case r.ConfigID != "" && !collectorText(r.ConfigID, 128, true):
		return invalid("config must be a fleet config ID")
	case r.Good < 0 || r.Bad < 0:
		return invalid("good and bad must be versions")
	case r.Good > 0 && r.Bad > 0 && r.Good >= r.Bad:
		return invalid("the good version must be older than the bad one")
	case len(r.Scenarios) == 0:
		return invalid("a bisect needs at least one scenario with assertions")
	case len(r.Scenarios) > maxBisectScenarios:
		return invalid(fmt.Sprintf("a bisect takes at most %d scenarios", maxBisectScenarios))
	case len(r.Local) > maxBisectLocal:
		return invalid(fmt.Sprintf("a bisect takes at most %d local files", maxBisectLocal))
	}
	return nil
}

// fleetConfigs is the part of fleetcapture.Client a bisect reads.
type fleetConfigs interface {
	AgentConfig(ctx context.Context, agentID string) (*fleetcapture.AgentConfig, error)
	StoredConfig(ctx context.Context, id string) (*fleetcapture.StoredConfig, error)
	ConfigVersions(ctx context.Context, id string, after, limit int) (*fleetcapture.ConfigVersionPage, error)
	ConfigVersion(ctx context.Context, id string, version int) (*fleetcapture.ConfigVersion, error)
	Rollouts(ctx context.Context, after string, limit int) (*fleetcapture.RolloutPage, error)
}

func (s *Service) bisectLocal(ctx context.Context, caller Caller, sources []Source) ([]bisect.Part, error) {
	var parts []bisect.Part
	for i, src := range sources {
		role := "local file " + strconv.Itoa(i+1)
		if _, err := s.resolveSource(ctx, caller, &src, artifact.Configuration, role); err != nil {
			return nil, err
		}
		raw, err := (config.Source{Path: src.Path, YAML: src.Text}).BytesWith(s.policy.Paths)
		if err != nil {
			return nil, classify(fmt.Errorf("%s: %w", role, err), CodeValidationFailed)
		}
		parts = append(parts, bisect.Part{Name: role, Body: raw})
	}
	return parts, nil
}

// bisection is one bisect's state while it runs.
type bisection struct {
	s      *Service
	caller Caller
	req    BisectRequest
	fleet  fleetConfigs
	local  []bisect.Part
	t      bisect.Target
	// layers are the agent's entries; searched indexes the varied one.
	layers   []fleetcapture.DesiredLayer
	searched int
	versions []bisect.Version
	bodies   map[string][]byte // by "<config>/<version>"
	notes    []string
}

// target reads the layers fleet sends the agent and picks the searched one.
func (b *bisection) target(ctx context.Context) error {
	view, err := b.fleet.AgentConfig(ctx, b.req.AgentID)
	if err != nil {
		return fleetError(ctx, err)
	}
	switch {
	case view.Error == "too_many_layers":
		return &Error{Code: CodeConflict, Message: "more assignments match the agent than fleet sends, so fleet sends it nothing new"}
	case view.Desired == nil || len(view.Desired.Layers) == 0:
		return &Error{Code: CodeInvalidRequest, Message: "fleet sends this agent no config layers"}
	case view.Desired.Unmanaged:
		return &Error{Code: CodeInvalidRequest, Message: "fleet no longer manages this agent; it runs its local configuration"}
	}
	b.layers = slices.Clone(view.Desired.Layers)
	slices.SortStableFunc(b.layers, func(x, y fleetcapture.DesiredLayer) int { return compareStrings(x.Name, y.Name) })
	b.searched = -1
	var entries []string
	for i, l := range b.layers {
		entries = append(entries, l.Name)
		if l.ConfigID == "" || l.Version < 1 {
			return &Error{Code: CodeConflict, Message: fmt.Sprintf("fleet's layer %s names no config version", l.Name)}
		}
		if b.req.ConfigID == "" && len(b.layers) == 1 || l.ConfigID == b.req.ConfigID {
			if b.searched >= 0 {
				return &Error{Code: CodeInvalidRequest, Message: "the config is in several of the agent's layers"}
			}
			b.searched = i
		}
	}
	if b.searched < 0 {
		message := "the agent receives several layers; name the config to search"
		if b.req.ConfigID != "" {
			message = "the agent receives no layer of that config"
		}
		return &Error{Code: CodeInvalidRequest, Message: fmt.Sprintf("%s (layers %v)", message, entries)}
	}
	l := b.layers[b.searched]
	b.t = bisect.Target{AgentID: b.req.AgentID, ConfigID: l.ConfigID, AssignmentID: l.AssignmentID, Entry: l.Name, LocalFiles: len(b.local), Layers: []bisect.Layer{}}
	if c, err := b.fleet.StoredConfig(ctx, l.ConfigID); err == nil {
		b.t.ConfigName = c.Name
	} else if ctx.Err() != nil {
		return fleetError(ctx, err)
	}
	for i, layer := range b.layers {
		b.t.Layers = append(b.t.Layers, bisect.Layer{Entry: layer.Name, ConfigID: layer.ConfigID, Version: layer.Version, Searched: i == b.searched})
	}
	if len(b.local) == 0 {
		b.notes = append(b.notes, "no local supervisor files were given; if the agent's supervisor merges local config files under fleet's layers, pass them too")
	}
	b.notes = append(b.notes, "the supervisor's own OpAMP extension and telemetry settings are not merged; they do not touch the data path",
		"the search assumes that once a version is bad, every later version is bad too")
	return nil
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// candidates lists the searched config's versions in range, oldest first.
func (b *bisection) candidates(ctx context.Context) error {
	id := b.t.ConfigID
	good, bad := b.req.Good, b.req.Bad
	if bad == 0 {
		bad = b.layers[b.searched].Version
	}
	if good >= bad {
		return &Error{Code: CodeInvalidRequest, Message: fmt.Sprintf("the good version must be older than the bad version %d", bad)}
	}
	after := bad + 1
	for {
		page, err := b.fleet.ConfigVersions(ctx, id, after, maxBisectVersions)
		if err != nil {
			return fleetError(ctx, err)
		}
		for _, v := range page.Items {
			if v.Version < good || v.Version > bad {
				continue
			}
			b.versions = append(b.versions, bisect.Version{Version: v.Version, SHA256: v.SHA256, Message: v.Message,
				CreatedAt: v.CreatedAt, CreatedBy: v.CreatedBy, Status: bisect.Untested})
		}
		last := 0
		if n := len(page.Items); n > 0 {
			last = page.Items[n-1].Version
		}
		if page.NextAfter == "" || last <= good || last == 0 || len(b.versions) > maxBisectVersions {
			break
		}
		after = last
	}
	slices.SortFunc(b.versions, func(x, y bisect.Version) int { return x.Version - y.Version })
	switch {
	case len(b.versions) > maxBisectVersions:
		return &Error{Code: CodeInvalidRequest, Message: fmt.Sprintf("the range holds more than %d versions; narrow it with good", maxBisectVersions)}
	case len(b.versions) == 0 || b.versions[len(b.versions)-1].Version != bad:
		return &Error{Code: CodeNotFound, Message: fmt.Sprintf("fleet no longer keeps version %d of the config", bad)}
	case good > 0 && b.versions[0].Version != good:
		return &Error{Code: CodeNotFound, Message: fmt.Sprintf("fleet no longer keeps version %d of the config", good)}
	case len(b.versions) < 2:
		return &Error{Code: CodeInvalidRequest, Message: "the range holds a single version; there is nothing to search"}
	}
	if missing := b.versions[len(b.versions)-1].Version - b.versions[0].Version + 1 - len(b.versions); missing > 0 {
		b.notes = append(b.notes, fmt.Sprintf("fleet no longer keeps %d versions of the range, so they were not searched", missing))
	}
	return nil
}

// body reads a version's body once.
func (b *bisection) body(ctx context.Context, configID string, version int) ([]byte, error) {
	key := configID + "/" + strconv.Itoa(version)
	if raw, ok := b.bodies[key]; ok {
		return raw, nil
	}
	v, err := b.fleet.ConfigVersion(ctx, configID, version)
	if err != nil {
		return nil, fleetError(ctx, err)
	}
	if v.Body == "" {
		return nil, &Error{Code: CodeForbidden, Message: "fleet returned no config body; reading bodies needs a fleet admin credential"}
	}
	b.bodies[key] = []byte(v.Body)
	return b.bodies[key], nil
}

// compose rebuilds the agent's configuration with the searched layer at a
// version.
func (b *bisection) compose(ctx context.Context, version int) ([]byte, error) {
	var remote []bisect.Part
	for i, l := range b.layers {
		v := l.Version
		if i == b.searched {
			v = version
		}
		raw, err := b.body(ctx, l.ConfigID, v)
		if err != nil {
			return nil, err
		}
		remote = append(remote, bisect.Part{Name: l.Name, Body: raw})
	}
	return bisect.Compose(b.local, remote)
}

// probe judges one candidate. Fleet and caller failures end the search; a
// configuration that cannot be merged or loaded skips the version.
func (b *bisection) probe(ctx context.Context, i int) (bisect.Status, error) {
	v := &b.versions[i]
	raw, err := b.compose(ctx, v.Version)
	var merge *Error
	switch {
	case errors.As(err, &merge):
		return "", err
	case err != nil:
		v.Reason = "the layers do not merge: " + err.Error()
		return bisect.Skipped, nil
	}
	cfg := Source{Text: string(raw), Label: b.label(v.Version)}
	sc := b.req.Scenarios[0]
	ins, err := b.s.InspectConfiguration(ctx, b.caller, InspectRequest{Config: cfg, Scenario: &sc, Runtime: b.req.Runtime})
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", err
	}
	if !ins.Valid {
		v.Reason = "the merged configuration is invalid"
		for _, d := range ins.Diagnostics {
			if d.Severity == config.SeverityError {
				v.Reason += ": " + d.Message
				break
			}
		}
		return bisect.Skipped, nil
	}
	for j, sc := range b.req.Scenarios {
		res, err := b.scenario(ctx, cfg, sc)
		if err != nil {
			return "", err
		}
		res.Name = scenarioName(sc.Scenario, j)
		v.Scenarios = append(v.Scenarios, res)
	}
	status := bisect.Judge(v.Scenarios)
	if status == bisect.Skipped {
		v.Reason = "a scenario did not establish an outcome"
	}
	return status, nil
}

func (b *bisection) label(version int) string {
	name := b.t.ConfigName
	if name == "" {
		name = b.t.ConfigID
	}
	return fmt.Sprintf("fleet %s v%d", name, version)
}

// scenario runs one scenario and waits for its assertions. A run that
// cannot be admitted for its input or configuration is a result; a full
// queue, a missing runtime or the caller leaving ends the search.
func (b *bisection) scenario(ctx context.Context, cfg Source, sc ScenarioInput) (bisect.ScenarioResult, error) {
	run, err := b.s.SubmitRun(ctx, b.caller, RunRequest{Mode: ModeScenario, Config: cfg, Scenario: &sc, Runtime: b.req.Runtime})
	if err != nil {
		if ctx.Err() != nil {
			return bisect.ScenarioResult{}, ctx.Err()
		}
		switch ErrorCode(err) {
		case CodeValidationFailed, CodeInvalidRequest:
			return bisect.ScenarioResult{Outcome: assertion.Error, Error: err.Error()}, nil
		}
		return bisect.ScenarioResult{}, err
	}
	res := bisect.ScenarioResult{Run: run.ID}
	rep, err := b.s.GetRunReport(ctx, b.caller, run.ID, true)
	switch {
	case err != nil && ctx.Err() != nil:
		_, _ = b.s.CancelRun(context.WithoutCancel(ctx), b.caller, run.ID)
		return res, ctx.Err()
	case err != nil:
		res.Outcome, res.Error = assertion.Error, err.Error()
	case rep.Assertions == nil:
		res.Outcome, res.Error = assertion.Error, "scenario declares no assertions"
	default:
		res.Outcome = rep.Assertions.Outcome
	}
	return res, nil
}

// culprit diffs the searched config between the last good and the first
// bad version and compares the runs of each scenario whose outcome changed.
func (b *bisection) culprit(ctx context.Context, good, bad int) (*bisect.Culprit, error) {
	g, w := b.versions[good], b.versions[bad]
	c := &bisect.Culprit{Version: w.Version, LastGood: g.Version, Scenarios: []bisect.CulpritScenario{}}
	before, err := b.body(ctx, b.t.ConfigID, g.Version)
	if err != nil {
		return nil, err
	}
	after, err := b.body(ctx, b.t.ConfigID, w.Version)
	if err != nil {
		return nil, err
	}
	c.Diff, err = difflib.GetUnifiedDiffString(difflib.UnifiedDiff{A: candidateDiffLines(string(before)), B: candidateDiffLines(string(after)),
		FromFile: "v" + strconv.Itoa(g.Version), ToFile: "v" + strconv.Itoa(w.Version), Context: 3})
	if err != nil {
		return nil, classify(err, CodeInternal)
	}
	for i := range min(len(g.Scenarios), len(w.Scenarios)) {
		gs, ws := g.Scenarios[i], w.Scenarios[i]
		if gs.Outcome == ws.Outcome || gs.Run == "" || ws.Run == "" {
			continue
		}
		cs := bisect.CulpritScenario{Name: ws.Name, Good: gs.Run, Bad: ws.Run, GoodAssertions: gs.Outcome, BadAssertions: ws.Outcome}
		cmp, err := b.s.CompareRuns(ctx, b.caller, CompareRequest{Before: gs.Run, After: ws.Run, Ignore: b.req.Ignore})
		switch {
		case err != nil && ctx.Err() != nil:
			return nil, ctx.Err()
		case err != nil:
			cs.Error = err.Error()
		default:
			cs.Comparison, cs.InputsIdentical = cmp, cmp.InputsIdentical
		}
		c.Scenarios = append(c.Scenarios, cs)
	}
	return c, nil
}

// rollouts adds fleet's retained rollouts of the assignment to each
// version. They only annotate the answer: a failed read becomes a note.
func (b *bisection) rollouts(ctx context.Context, result *Bisection) {
	if b.t.AssignmentID == "" {
		return
	}
	at := map[int]int{}
	for i, v := range result.Versions {
		at[v.Version] = i
	}
	after := ""
	for range maxRolloutPages {
		page, err := b.fleet.Rollouts(ctx, after, 100)
		if err != nil {
			result.Notes = append(result.Notes, "fleet's rollouts could not be read, so versions are not matched to rollouts")
			return
		}
		for _, r := range page.Items {
			i, ok := at[r.ToVersion]
			if r.AssignmentID != b.t.AssignmentID || !ok {
				continue
			}
			result.Versions[i].Rollouts = append(result.Versions[i].Rollouts, bisect.Rollout{ID: r.ID, State: r.State,
				CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, CreatedBy: r.CreatedBy})
		}
		if page.NextAfter == "" {
			break
		}
		after = page.NextAfter
	}
	for i := range result.Versions {
		slices.SortFunc(result.Versions[i].Rollouts, func(x, y bisect.Rollout) int { return x.CreatedAt.Compare(y.CreatedAt) })
	}
}
