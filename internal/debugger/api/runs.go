package api

import (
	"encoding/json"
	"time"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// HTTP v1 wire shapes are adapter DTOs mapped to and from platform types, so
// the platform's vocabulary can change without changing this contract.

// State is the HTTP v1 run lifecycle. Assigned means a worker
// claimed the run and has not started it; lost means its last lease ended
// before it finished. Done means a report exists, not that execution or
// assertions passed.
type State string

const (
	StateQueued    State = "queued"
	StateAssigned  State = "assigned"
	StateRunning   State = "running"
	StateDone      State = "done"
	StateError     State = "error"
	StateCancelled State = "cancelled"
	StateLost      State = "lost"
)

func stateOf(s platform.State) State {
	switch s {
	case platform.StateQueued:
		return StateQueued
	case platform.StateAssigned:
		return StateAssigned
	case platform.StateRunning:
		return StateRunning
	case platform.StateDone:
		return StateDone
	case platform.StateCancelled:
		return StateCancelled
	case platform.StateLost:
		return StateLost
	}
	return StateError
}

// Request selects a configuration and its inputs. The configuration is text
// (config_yaml) or a saved revision; the inputs are exactly one of fixture
// text, scenario text or a saved revision of either. The server reads no host
// file: every file comes with the request or from its storage.
type Request struct {
	ConfigYAML string `json:"config_yaml,omitempty"`
	// FixtureJSON is an OTLP JSON fixture as text: a quick run of sample
	// input without a scenario.
	FixtureJSON string `json:"fixture_json,omitempty"`
	// ScenarioYAML is scenario text; its inputs must be inline (data).
	ScenarioYAML string `json:"scenario_yaml,omitempty"`
	// Receiver selects the injection receiver for a fixture.
	Receiver string `json:"receiver,omitempty"`
	// ClientAddress is the sender IP address of a fixture.
	ClientAddress string `json:"client_address,omitempty"`
	// SetupYAML gives a fixture run mocks, environment values and execution
	// limits: scenario YAML without inputs or assertions.
	SetupYAML string `json:"setup_yaml,omitempty"`
	// Compare also runs an uninstrumented reference Collector.
	Compare bool `json:"compare,omitempty"`
	// Normalize removes run-specific IDs and times from the report.
	Normalize bool `json:"normalize,omitempty"`
	// ConfigRef, FixtureRef and ScenarioRef submit a saved revision instead of
	// text; its bytes are copied into the run. They need a workspace.
	ConfigRef   *platform.ArtifactRef `json:"config_ref,omitempty"`
	FixtureRef  *platform.ArtifactRef `json:"fixture_ref,omitempty"`
	ScenarioRef *platform.ArtifactRef `json:"scenario_ref,omitempty"`
	// ConfigLabel, FixtureLabel and ScenarioLabel name inline text or a
	// revision in the report's provenance, such as the file the text came from.
	ConfigLabel   string `json:"config_label,omitempty"`
	FixtureLabel  string `json:"fixture_label,omitempty"`
	ScenarioLabel string `json:"scenario_label,omitempty"`
	// Runtime names a built runtime (name, image reference or image ID);
	// empty or "built-in" is the debugger's own registry.
	Runtime string `json:"runtime,omitempty"`
}

// run maps the v1 fields onto the platform's explicit modes. Fields of both
// modes are passed on, so the platform rejects the combination.
func (r Request) run() platform.RunRequest {
	req := platform.RunRequest{Mode: platform.ModeQuick, Config: platform.Source{Text: r.ConfigYAML, Artifact: r.ConfigRef, Label: r.ConfigLabel},
		Compare: r.Compare, Normalize: r.Normalize, Runtime: r.Runtime}
	req.Scenario = scenarioInput(platform.Source{Text: r.ScenarioYAML, Artifact: r.ScenarioRef, Label: r.ScenarioLabel})
	if req.Scenario != nil {
		req.Mode = platform.ModeScenario
	}
	fixture := platform.Source{Text: r.FixtureJSON, Artifact: r.FixtureRef, Label: r.FixtureLabel}
	if fixture != (platform.Source{}) || r.Receiver != "" || r.ClientAddress != "" || r.SetupYAML != "" {
		req.Quick = &platform.QuickInput{Fixture: fixture, Receiver: r.Receiver, ClientAddress: r.ClientAddress, Setup: r.SetupYAML}
	}
	return req
}

func scenarioInput(src platform.Source) *platform.ScenarioInput {
	if src == (platform.Source{}) {
		return nil
	}
	return &platform.ScenarioInput{Scenario: src}
}

// InspectRequest selects a configuration to validate, as text (config_yaml)
// or a saved revision. A scenario, as text or a saved revision, or the setup
// of a quick run optionally supplies ${env:NAME} values and mocks.
type InspectRequest struct {
	ConfigYAML   string `json:"config_yaml,omitempty"`
	ScenarioYAML string `json:"scenario_yaml,omitempty"`
	// SetupYAML is a quick run's setup (see Request.SetupYAML).
	SetupYAML string `json:"setup_yaml,omitempty"`
	// ConfigRef and ScenarioRef name saved revisions (see Request.ConfigRef).
	ConfigRef   *platform.ArtifactRef `json:"config_ref,omitempty"`
	ScenarioRef *platform.ArtifactRef `json:"scenario_ref,omitempty"`
	// ConfigLabel names the configuration in the inspection's config.path.
	ConfigLabel string `json:"config_label,omitempty"`
	// Runtime names a built runtime (name, image reference or image ID);
	// empty or "built-in" is the debugger's own registry.
	Runtime string `json:"runtime,omitempty"`
}

func (r InspectRequest) inspect() platform.InspectRequest {
	return platform.InspectRequest{Config: platform.Source{Text: r.ConfigYAML, Artifact: r.ConfigRef, Label: r.ConfigLabel},
		Scenario: scenarioInput(platform.Source{Text: r.ScenarioYAML, Artifact: r.ScenarioRef}), Setup: r.SetupYAML, Runtime: r.Runtime}
}

// OTTLRequest evaluates one transform processor of a configuration on an
// OTLP JSON payload without running the Collector: the OTTL playground. A
// scenario supplies ${env:NAME} values.
type OTTLRequest struct {
	ConfigYAML   string          `json:"config_yaml,omitempty"`
	ScenarioYAML string          `json:"scenario_yaml,omitempty"`
	Processor    string          `json:"processor"`
	Input        json.RawMessage `json:"input"`
	// ConfigRef and ScenarioRef name saved revisions (see Request.ConfigRef).
	ConfigRef   *platform.ArtifactRef `json:"config_ref,omitempty"`
	ScenarioRef *platform.ArtifactRef `json:"scenario_ref,omitempty"`
	// Runtime names a built runtime (name, image reference or image ID);
	// empty or "built-in" is the debugger's own registry.
	Runtime string `json:"runtime,omitempty"`
}

func (r OTTLRequest) preview() platform.PreviewRequest {
	return platform.PreviewRequest{Config: platform.Source{Text: r.ConfigYAML, Artifact: r.ConfigRef},
		Scenario: scenarioInput(platform.Source{Text: r.ScenarioYAML, Artifact: r.ScenarioRef}), Processor: r.Processor, Input: r.Input, Runtime: r.Runtime}
}

// AssertionsRequest re-evaluates the assertions of an edited scenario against
// a finished run, without executing it again.
type AssertionsRequest struct {
	// ScenarioYAML is the scenario text; only its assertions are used, but
	// the whole scenario must be valid.
	ScenarioYAML string `json:"scenario_yaml"`
}

// Run is a metadata/report view; HTTP omits snapshot stores here.
type Run struct {
	ID          string           `json:"id"`
	Project     string           `json:"project_id"`
	State       State            `json:"state"`
	Error       string           `json:"error,omitempty"`
	Created     time.Time        `json:"created"`
	Started     *time.Time       `json:"started,omitempty"`
	Finished    *time.Time       `json:"finished,omitempty"`
	Report      *platform.Report `json:"report,omitempty"`
	Interrupted bool             `json:"interrupted,omitempty"`
	// Kept protects the run from retention and prune (PUT /runs/{id}/keep).
	Kept bool `json:"kept,omitempty"`
	// Attempt counts the run's assignments to a worker; a run requeued after
	// its lease ended has attempt 2.
	Attempt int                  `json:"attempt,omitempty"`
	Bundle  *platform.BundleInfo `json:"bundle,omitempty"`
	// Summary is what the run list says about a run with a report;
	// only GET /runs sets it.
	Summary *platform.RunListing `json:"summary,omitempty"`
}

// RunPage is a page of runs, newest first; next_cursor continues it and is
// absent on the last page.
type RunPage struct {
	Runs       []Run  `json:"runs"`
	NextCursor string `json:"next_cursor,omitempty"`
}

func runOf(v platform.Run) Run {
	return Run{ID: v.ID, Project: v.Project, State: stateOf(v.State), Error: v.Error, Created: v.Created, Started: v.Started,
		Finished: v.Finished, Report: v.Report, Interrupted: v.Interrupted, Kept: v.Kept, Attempt: v.Attempt, Bundle: v.Bundle, Summary: v.Listing}
}

// End is the data of the final SSE "end" event of a run.
type End struct {
	State  State              `json:"state"`
	Status platform.RunStatus `json:"status,omitempty"`
	Error  string             `json:"error,omitempty"`
}

// Restart is the data of a run event stream's "restart" event: the run was
// requeued and executes again as Attempt, whose events follow and replace
// those the client holds.
type Restart struct {
	Attempt int `json:"attempt"`
}

// RunInputs are the sources a run was admitted with (GET /runs/{id}/inputs),
// to open as drafts. They never carry environment values or the contents of
// captured files: the configuration is its root text, and files and required
// environment values are listed by name.
type RunInputs struct {
	RunID      string     `json:"run_id"`
	Mode       string     `json:"mode"`
	Config     RunSource  `json:"config"`
	Fixture    *RunSource `json:"fixture,omitempty"`
	Scenario   *RunSource `json:"scenario,omitempty"`
	Setup      *RunSource `json:"setup,omitempty"`
	Assertions *RunSource `json:"assertions,omitempty"`
	// Receiver and ClientAddress are a quick run's injection settings.
	Receiver      string `json:"receiver,omitempty"`
	ClientAddress string `json:"client_address,omitempty"`
	// Files names the captured input files beside the root sources, relative
	// to the scenario's directory where they are under it.
	Files []string `json:"files"`
	// RequiredEnvironment names the environment values the configuration read.
	RequiredEnvironment []string `json:"required_environment"`
}

// RunSource is one root source: its path or label, the saved revision it was
// submitted by, and its captured text.
type RunSource struct {
	Label    string                `json:"label,omitempty"`
	Artifact *platform.ArtifactRef `json:"artifact,omitempty"`
	SHA256   string                `json:"sha256"`
	Text     string                `json:"text"`
}

func runInputsOf(in *platform.RunInputs) RunInputs {
	source := func(s *platform.RunSource) *RunSource {
		if s == nil {
			return nil
		}
		return &RunSource{Label: s.Label, Artifact: s.Artifact, SHA256: s.SHA256, Text: s.Text}
	}
	return RunInputs{RunID: in.RunID, Mode: string(in.Mode), Config: *source(&in.Config), Fixture: source(in.Fixture), Scenario: source(in.Scenario),
		Setup: source(in.Setup), Assertions: source(in.Assertions), Receiver: in.Receiver, ClientAddress: in.ClientAddress,
		Files: in.Files, RequiredEnvironment: in.RequiredEnvironment}
}

// Diffs are the analysis results of a finished run (GET /runs/{id}/diffs).
type Diffs struct {
	Stages []platform.Stage       `json:"stages"`
	OTTL   []*platform.OTTLReport `json:"ottl"`
}
