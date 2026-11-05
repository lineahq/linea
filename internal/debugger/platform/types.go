package platform

import (
	"io"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/hostfs"
	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/ottldebug"
	"github.com/lineahq/linea/internal/debugger/statementedit"
	"github.com/lineahq/linea/internal/debugger/topology"
)

// Results are the engine's canonical documents and storage
// records, named here so adapters depend on the platform alone. They are
// read-only: callers must not mutate them.
type (
	Report           = engine.Report
	Inspection       = engine.Inspection
	PreviewResult    = engine.OTTLEvaluation
	AssertionReport  = assertion.Report
	SuiteReport      = engine.TestReport
	BundleInfo       = engine.BundleInfo
	Versions         = engine.Versions
	ReportRuntime    = engine.ReportRuntime
	Event            = instrument.Event
	RunStatus        = runresult.Status
	Diagnostic       = config.Diagnostic
	Reference        = config.Reference
	Graph            = topology.Graph
	Stage            = analysis.Stage
	OTTLReport       = ottldebug.Report
	ArtifactKind     = artifact.Kind
	ArtifactMetadata = artifact.Metadata
	ArtifactRevision = artifact.RevisionData
	ArtifactPage     = artifact.Page
	// ArtifactRevisionPage is one page of an artifact's revisions, newest first.
	ArtifactRevisionPage = artifact.RevisionPage
	SavedArtifact        = artifact.Saved
	WorkspaceInfo        = artifact.Info
	StatementEdit        = statementedit.Edit
	// ArtifactRef names one saved revision; both IDs are required.
	ArtifactRef = engine.SourceRef
	// SourceRefs are the saved revisions a run's sources were submitted by.
	SourceRefs = engine.SourceRefs
	// EnvironmentPolicy is trusted composition policy (Options.Environment).
	EnvironmentPolicy = config.EnvironmentPolicy
	// PathPolicy is trusted composition policy (Options.Paths).
	PathPolicy = hostfs.Policy
)

// TimestampsRebased marks a report whose input timestamps were rebased.
const TimestampsRebased = engine.TimestampsRebased

// ModeStandIn is the execution mode of a component whose type is not in the
// distribution.
const ModeStandIn = topology.ModeStandIn

// ReferenceStandIn is the source of a reference that resolved to a stand-in
// string.
const ReferenceStandIn = config.SourceStandIn

// CurrentVersions describes this build: debugger, Collector, schemas, modules.
func CurrentVersions() Versions { return engine.CurrentVersions() }

// Encode writes a result as the canonical report JSON, byte-identical across
// adapters.
func Encode(w io.Writer, v any) error { return engine.Encode(w, v) }

// Source is a host path, inline text or a saved revision; exactly one is set.
// Host paths are for trusted local adapters. A revision's bytes
// are copied at admission; later heads never change the run. Label names
// inline text or a revision where an operation records sources (provenance,
// suite members), as a path names a file; it is never read.
type Source struct {
	Path     string
	Text     string
	Artifact *ArtifactRef
	Label    string
	// Captured while resolving a revision; never accepted from an adapter.
	origin *artifact.Origin
}

type CandidateRequest struct {
	Base      Source
	YAML      string
	Statement *StatementEdit
	Save      bool
	Label     string
	// Runtime names a built runtime by name, image reference or image ID;
	// empty or BuiltInRuntime is the debugger's own registry.
	Runtime string
}

type Candidate struct {
	Proposed          string
	Patch             string
	AffectedPipelines []string
	Valid             bool
	Diagnostics       []Diagnostic
	Saved             *SavedArtifact
}

// StatementTestRequest tests an unsaved candidate through an ordinary run.
type StatementTestRequest struct {
	Candidate CandidateRequest // Save must be false.
	// RunID tests the candidate on a retained run's recorded configuration
	// and inputs: Candidate.Base, Mode, Quick and Scenario
	// stay empty, the run is a scenario run of the recorded inputs, and
	// Candidate.Runtime defaults to the runtime the run's report names.
	RunID    string
	Mode     InputMode
	Quick    *QuickInput
	Scenario *ScenarioInput
	// Assertions replaces scenario assertions; empty keeps the scenario's own.
	// Quick assertions belong in Quick.Assertions.
	Assertions string
}

type StatementTest struct {
	Candidate     *Candidate
	Run           *Run // Nil for an invalid candidate; an admitted run is not awaited.
	HasAssertions bool // Without a declared output boundary there is no pass.
}

// QuickInput is a quick run: one OTLP JSON payload, optional setup and assertions.
type QuickInput struct {
	Fixture Source
	// Receiver selects the injection receiver when several feed its signal.
	Receiver string
	// ClientAddress is the sender IP address of the payload.
	ClientAddress string
	// Setup is scenario YAML without inputs or assertions: mocks, environment
	// values and execution limits.
	Setup string
	// Assertions is scenario v1 text containing only assertions and an optional
	// version. Empty means no assertions.
	Assertions string
}

// ScenarioInput is a scenario run, or scenario context for inspection.
type ScenarioInput struct {
	Scenario Source
	// Dir resolves relative input files of inline scenario text.
	Dir string
}

// RunRequest submits a run in an explicit input mode. Quick belongs to
// ModeQuick and Scenario to ModeScenario; fields of the other mode are
// rejected rather than ignored.
type RunRequest struct {
	Mode     InputMode
	Config   Source
	Quick    *QuickInput
	Scenario *ScenarioInput
	// Compare also runs an uninstrumented reference Collector.
	Compare bool
	// Normalize removes run-specific IDs and times from the report.
	Normalize bool
	// Runtime names a built runtime by name, image reference or image ID;
	// empty or BuiltInRuntime is the debugger's own registry.
	Runtime string
}

// InspectRequest validates a configuration. A scenario or a quick run's setup
// optionally supplies environment values and mocks.
type InspectRequest struct {
	Config   Source
	Scenario *ScenarioInput
	Setup    string
	// Runtime names a built runtime by name, image reference or image ID;
	// empty or BuiltInRuntime is the debugger's own registry.
	Runtime string
}

// PreviewRequest evaluates one transform processor's statements on an OTLP
// JSON payload without running the Collector. The result is never verified.
type PreviewRequest struct {
	Config    Source
	Scenario  *ScenarioInput
	Processor string
	Input     []byte
	// Runtime names a built runtime by name, image reference or image ID;
	// empty or BuiltInRuntime is the debugger's own registry.
	Runtime string
}

// AssertionsRequest evaluates an edited scenario's assertions against a run's
// recorded observations; only its assertions are used.
type AssertionsRequest struct {
	ScenarioYAML string
	Record       bool // Keep the evaluation; HTTP v1 never sets it.
}

// EvaluationInfo is listing metadata. It never holds the assertion source.
type EvaluationInfo struct {
	ID      string
	RunID   string
	Caller  Actor
	Created time.Time
	Outcome assertion.Outcome
}

// Evaluation is one evaluation's result. ID is empty unless recorded.
// Results are read-only, like the platform's other canonical documents.
type Evaluation struct {
	EvaluationInfo
	Recorded bool
	Result   *AssertionReport
}

// SuiteRequest runs the assertions of scenario files against one
// configuration file.
type SuiteRequest struct {
	Config    string
	Scenarios []string
	// Runtime names a built runtime by name, image reference or image ID;
	// empty or BuiltInRuntime is the debugger's own registry.
	Runtime string
}

// MaxSuiteMembers bounds one suite's queue reservation before preparation.
const MaxSuiteMembers = 32

type SubmitSuiteRequest struct {
	Config  Source
	Members []ScenarioInput
	// Runtime names a built runtime by name, image reference or image ID;
	// empty or BuiltInRuntime is the debugger's own registry.
	Runtime string
}

type SuiteMember struct {
	Label string
	RunID string // Empty when preparation failed.
	Error string // Preparation error; empty for admitted members.
}

// Suite is immutable admission metadata. Its outcome is derived from its runs.
type Suite struct {
	ID      string
	Project string
	Caller  Actor
	Created time.Time
	Config  string
	Members []SuiteMember
}

type SuiteMemberStatus struct {
	SuiteMember
	State   State
	Name    string
	Outcome assertion.Outcome
	Message string // Nonempty without an assertion report once terminal.
}

type SuiteStatus struct {
	Suite
	Done    bool
	Outcome assertion.Outcome
	Members []SuiteMemberStatus // Shadows Suite.Members with derived status.
}

// StageChanges are a finished run's stage outcomes, diffs and OTTL replays.
type StageChanges struct {
	Stages []Stage
	OTTL   []*OTTLReport
}

// EventCursor is a position in a run's event stream: an attempt and the last
// seq seen in it, or the zero cursor before any event. A requeued run
// executes again as a new attempt whose events the engine numbers from 1, so
// a seq alone does not say where a watcher stopped.
type EventCursor struct {
	Attempt int
	Seq     int
}

// RunWatch is one observation of a run: events after the requested cursor,
// whether the run has ended (with its outcome) and a channel closed on the next
// change. Watching never cancels or extends a run.
type RunWatch struct {
	Events []Event
	// Attempt is the attempt Events belong to. Restarted reports that the
	// cursor was in another attempt: the watcher drops the events it holds,
	// and Events start at Attempt's first.
	Attempt   int
	Restarted bool
	Terminal  bool
	End       End
	Changed   <-chan struct{}
}

// Cancellation reports a cancel request's effect. Effective is false when the
// run had already ended; repeating a cancellation is safe.
type Cancellation struct {
	Run       Run
	Effective bool
}

// SaveArtifactRequest saves original bytes as a new revision. A new artifact
// omits ArtifactID; an edit names the generation and head it expects.
type SaveArtifactRequest struct {
	WorkspaceID        string
	ArtifactID         string
	ExpectedGeneration uint64
	ExpectedHead       string
	Kind               ArtifactKind
	Label              string
	Content            []byte
}

// ListArtifactRevisionsRequest pages one artifact's revisions.
type ListArtifactRevisionsRequest struct {
	ArtifactID string
	Limit      int
	Cursor     string
}

// ListArtifactsRequest pages saved artifact metadata.
type ListArtifactsRequest struct {
	Kind   ArtifactKind
	Limit  int
	Cursor string
}

func (r RunRequest) engine() (engine.Request, error) {
	req := engine.Request{Config: r.Config.Path, ConfigYAML: r.Config.Text, ConfigLabel: r.Config.Label, Compare: r.Compare, Normalize: r.Normalize,
		Runtime: runtimeSelection(r.Runtime)}
	if q := r.Quick; q != nil {
		req.Fixture, req.FixtureJSON, req.FixtureLabel = q.Fixture.Path, q.Fixture.Text, q.Fixture.Label
		req.Receiver, req.ClientAddress, req.SetupYAML = q.Receiver, q.ClientAddress, q.Setup
		req.AssertionsYAML = q.Assertions
	}
	if s := r.Scenario; s != nil {
		req.Scenario, req.ScenarioYAML, req.ScenarioDir, req.ScenarioLabel = s.Scenario.Path, s.Scenario.Text, s.Dir, s.Scenario.Label
	}
	// Engine validation first keeps its messages for conflicting sources.
	if err := req.Validate(); err != nil {
		return engine.Request{}, err
	}
	switch r.Mode {
	case ModeQuick:
		if r.Scenario != nil {
			return engine.Request{}, errMode
		}
	case ModeScenario:
		if r.Quick != nil {
			return engine.Request{}, errMode
		}
	default:
		return engine.Request{}, errMode
	}
	return req, nil
}

func (r InspectRequest) engine() engine.InspectRequest {
	req := engine.InspectRequest{Config: r.Config.Path, ConfigYAML: r.Config.Text, ConfigLabel: r.Config.Label, SetupYAML: r.Setup}
	if s := r.Scenario; s != nil {
		req.Scenario, req.ScenarioYAML, req.ScenarioDir = s.Scenario.Path, s.Scenario.Text, s.Dir
	}
	return req
}

func (r PreviewRequest) engine() engine.OTTLRequest {
	req := engine.OTTLRequest{Config: r.Config.Path, ConfigYAML: r.Config.Text, Processor: r.Processor, Input: r.Input}
	if s := r.Scenario; s != nil {
		req.Scenario, req.ScenarioYAML, req.ScenarioDir = s.Scenario.Path, s.Scenario.Text, s.Dir
	}
	return req
}

func (r SaveArtifactRequest) artifact(actor string) artifact.SaveRequest {
	return artifact.SaveRequest{WorkspaceID: r.WorkspaceID, ArtifactID: r.ArtifactID, ExpectedGeneration: r.ExpectedGeneration,
		ExpectedHead: r.ExpectedHead, Kind: r.Kind, Label: r.Label, Content: r.Content, Actor: actor}
}
