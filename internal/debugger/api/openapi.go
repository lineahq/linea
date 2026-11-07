package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/lineahq/linea/internal/debugger/platform"
	"github.com/lineahq/linea/samples"
)

// The OpenAPI document is the API contract. Paths are declared
// below; component schemas are derived from the Go types by reflection over
// their json tags, so the contract cannot drift from the encoder. The
// generated document is committed as openapi.json for the web client's
// TypeScript generation; TestOpenAPIIsCurrent keeps it current
// (go test ./internal/debugger/api -run TestOpenAPIIsCurrent -update).

// openAPIDocument is the document served at /api/v1/openapi.json.
var openAPIDocument = mustOpenAPI()

func mustOpenAPI() []byte {
	doc, err := OpenAPI()
	if err != nil {
		panic(err)
	}
	return doc
}

// schemaRoots are the types named by the paths; every type they reference
// becomes a component schema too.
var schemaRoots = []any{
	platform.CollectorGroup{}, platform.CollectorGroupPage{}, platform.CollectorGroupMembersPage{}, platform.CaptureMetadata{}, platform.CaptureMetadataUpdate{},
	Request{}, InspectRequest{}, platform.Inspection{}, platform.Report{}, AuditPage{}, TokenRequest{}, CreatedToken{}, APIToken{}, Session{}, RoleBindings{}, RoleBindingRequest{},
	Projects{}, ProjectRequest{}, ProjectRevision{}, LibraryPage{}, SharingRequest{}, Sharing{}, CopyRequest{},
	OTTLRequest{}, platform.PreviewResult{}, AssertionsRequest{},
	Run{}, RunPage{}, RunPruneRequest{}, RunPruneResult{}, StorageUsage{}, RunInputs{}, platform.RunLog{}, platform.RunSummary{}, platform.EvidenceResult{}, EvaluationInfo{}, Evaluation{}, StatementTestRequest{}, StatementTest{}, Diffs{}, End{}, Restart{}, ConfigGraph{}, Error{}, platform.Event{}, samples.Sample{},
	SaveRequest{}, platform.SavedArtifact{}, platform.ArtifactMetadata{}, platform.ArtifactRevision{}, platform.ArtifactPage{}, ArtifactError{},
	platform.ArtifactRevisionPage{}, ExportForCIRequest{}, ImportRequest{}, platform.ArtifactImport{},
	CandidateRequest{}, Candidate{}, ScenarioFromRunRequest{}, ScenarioFromRun{}, OperationError{},
	SuiteRequest{}, Suite{}, SuiteStatus{}, platform.SuiteReport{}, ExportRequest{}, ScenarioExport{},
	ComparisonRequest{}, ComparisonRuns{}, platform.Comparison{},
	Runtime{}, RuntimeBuildRequest{}, RuntimeBuildSource{}, RuntimeBuild{}, RuntimeBuildPage{}, RuntimePushRequest{}, RuntimeImportRequest{}, RuntimeRegisterRequest{}, RuntimeDefault{}, RuntimeDefaultRequest{},
	RuntimeStorage{}, RuntimePruneRequest{}, RuntimePruneResult{},
	CollectorSupport{}, ManifestRequest{}, ComposedManifest{}, platform.CollectorCatalog{},
	UpgradeCheckRequest{}, platform.UpgradeCheck{}, ManifestRetargetRequest{}, RetargetedManifest{},
	ConfigBisectRequest{}, platform.Bisection{},
	platform.FleetOverview{}, platform.FleetAgentPage{}, platform.FleetAgentDetail{}, platform.FleetAgentConfig{}, platform.FleetEventPage{},
	platform.CollectorGroupSave{}, FleetDeleted{}, platform.FleetConfig{}, platform.FleetConfigPage{}, platform.FleetConfigCreate{}, platform.FleetConfigUpdate{},
	platform.FleetConfigVersion{}, platform.FleetConfigVersionPage{}, platform.FleetConfigVersionCreate{},
	platform.FleetAssignment{}, platform.FleetAssignmentPage{}, platform.FleetAssignmentCreate{}, platform.FleetAssignmentPreviewPage{},
	platform.FleetRollout{}, platform.FleetRolloutPage{}, platform.FleetRolloutStart{}, platform.FleetRolloutTargetPage{}, platform.FleetRolloutAction{},
	platform.Collector{}, platform.CollectorPage{}, platform.CapturePointPage{},
	platform.CaptureSelection{}, platform.CaptureValidation{}, platform.CaptureSession{}, platform.CaptureSessionPage{}, CaptureStopRequest{},
	platform.CaptureRecordsPage{}, platform.CaptureDeriveRequest{}, platform.CaptureFixture{}, platform.CaptureScenarioRequest{}, platform.CaptureScenario{}, platform.CaptureExportRequest{}, platform.CaptureExport{}, CaptureDeleted{},
}

// schemaNames names types whose own name is already taken by an older schema
// and would otherwise be prefixed with their package, or is too generic to
// say what it belongs to: the comparison's parts take their platform names.
var schemaNames = map[reflect.Type]string{
	reflect.TypeOf(platform.Comparison{}):            "RunComparison",
	reflect.TypeOf(platform.Actor{}):                 "Caller",
	reflect.TypeOf(platform.ComparisonReason{}):      "ComparisonReason",
	reflect.TypeOf(platform.ComparisonRun{}):         "ComparisonRun",
	reflect.TypeOf(platform.ComparisonExporter{}):    "ComparisonExporter",
	reflect.TypeOf(platform.ComparisonRecords{}):     "ComparisonRecords",
	reflect.TypeOf(platform.ComparisonRecord{}):      "ComparisonRecord",
	reflect.TypeOf(platform.ComparisonFate{}):        "ComparisonFate",
	reflect.TypeOf(platform.ComparisonStep{}):        "ComparisonStep",
	reflect.TypeOf(platform.ComparisonChange{}):      "ComparisonChange",
	reflect.TypeOf(platform.ComparisonRoute{}):       "ComparisonRoute",
	reflect.TypeOf(platform.ComparisonError{}):       "ComparisonError",
	reflect.TypeOf(platform.ComparisonAssertion{}):   "ComparisonAssertion",
	reflect.TypeOf(platform.ComparisonIgnoreRule{}):  "ComparisonIgnoreRule",
	reflect.TypeOf(platform.CollectorCatalog{}):      "CollectorCatalog",
	reflect.TypeOf(platform.CatalogComponent{}):      "CatalogComponent",
	reflect.TypeOf(platform.UpgradeCheck{}):          "UpgradeCheck",
	reflect.TypeOf(platform.UpgradeRuntime{}):        "UpgradeRuntime",
	reflect.TypeOf(platform.UpgradeConfiguration{}):  "UpgradeConfiguration",
	reflect.TypeOf(platform.UpgradeFeatureGates{}):   "UpgradeFeatureGates",
	reflect.TypeOf(platform.UpgradeGateChange{}):     "UpgradeGateChange",
	reflect.TypeOf(platform.UpgradeGateState{}):      "UpgradeGateState",
	reflect.TypeOf(platform.UpgradeScenario{}):       "UpgradeScenario",
	reflect.TypeOf(platform.UpgradeReason{}):         "UpgradeReason",
	reflect.TypeOf(platform.RunSummary{}):            "RunSummary",
	reflect.TypeOf(platform.Bisection{}):             "Bisection",
	reflect.TypeOf(platform.BisectTarget{}):          "BisectTarget",
	reflect.TypeOf(platform.BisectLayer{}):           "BisectLayer",
	reflect.TypeOf(platform.BisectVersion{}):         "BisectVersion",
	reflect.TypeOf(platform.BisectScenarioResult{}):  "BisectScenarioResult",
	reflect.TypeOf(platform.BisectRollout{}):         "BisectRollout",
	reflect.TypeOf(platform.BisectCulprit{}):         "BisectCulprit",
	reflect.TypeOf(platform.BisectCulpritScenario{}): "BisectCulpritScenario",
	reflect.TypeOf(platform.BisectReason{}):          "BisectReason",
	reflect.TypeOf(platform.EvidenceResult{}):        "EvidenceResult",
}

// OpenAPI returns the indented OpenAPI 3.1 document.
func OpenAPI() ([]byte, error) {
	g := &schemaGen{schemas: map[string]any{}, names: map[reflect.Type]string{}, owners: map[string]reflect.Type{}}
	for _, v := range schemaRoots {
		if _, err := g.schema(reflect.TypeOf(v)); err != nil {
			return nil, err
		}
	}
	doc := map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       "Linea API",
			"version":     platform.CurrentVersions().Product,
			"description": "Local-only API over the debugger engine. Requests must use a loopback Host, and POST bodies must be application/json.",
		},
		"paths":      paths(),
		"components": map[string]any{"schemas": g.schemas},
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func ref(name string) map[string]any { return map[string]any{"$ref": "#/components/schemas/" + name} }

func content(schema any) map[string]any {
	return map[string]any{"application/json": map[string]any{"schema": schema}}
}

func reply(desc string, schema any) map[string]any {
	r := map[string]any{"description": desc}
	if schema != nil {
		r["content"] = content(schema)
	}
	return r
}

func failure(desc string) map[string]any { return reply(desc, ref("Error")) }

func operationFailure(desc string) map[string]any { return reply(desc, ref("OperationError")) }

var (
	idParam  = map[string]any{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}}
	refParam = map[string]any{"name": "ref", "in": "path", "required": true, "schema": map[string]any{"type": "string"}, "description": "Snapshot reference, sha256:<hex>."}
)

// evidenceParams are GET /runs/{id}/evidence's query: the family, its
// parameters and the page.
var evidenceParams = func() []any {
	param := func(name, typ, desc string) any {
		return map[string]any{"name": name, "in": "query", "schema": map[string]any{"type": typ}, "description": desc}
	}
	return []any{
		map[string]any{"name": "family", "in": "query", "required": true, "description": "stage_changes, records, journey, statements, events or recommendations.",
			"schema": map[string]any{"type": "string", "enum": []string{"stage_changes", "records", "journey", "statements", "events", "recommendations"}}},
		param("pipeline", "string", "Pipeline ID for stage_changes and records (empty for a connector); optional filter for statements and recommendations."),
		param("component", "string", "The stage for stage_changes and records, the transform processor for statements, an optional filter for events and recommendations."),
		param("signal", "string", "Signal of a connector stage that has several."),
		param("side", "string", "For records: before (received) or after (emitted)."),
		param("entity", "string", "For journey: a record ID."),
		param("from_seq", "integer", "For events: first event sequence number."),
		param("to_seq", "integer", "For events: last event sequence number, inclusive; 0 is the end."),
		param("cursor", "string", "next_cursor of a previous page with the same run, family and parameters."),
		param("page_items", "integer", "Maximum items; the server's budget applies when larger or unset."),
		param("page_bytes", "integer", "Maximum encoded item bytes; the server's budget applies when larger or unset."),
	}
}()

// unknownRevision describes an artifact reference that does not resolve.
const unknownRevision = "A referenced saved artifact or revision does not exist on this server."

// noWorkspace describes artifact references sent to a server that keeps no saved work.
const noWorkspace = "Artifact references need a server that keeps saved work."

// forbiddenPath describes the refusal of a host file the request names.
const forbiddenPath = "The request names a host file, directly or through the configuration, and the server reads none; send its content inline or name a saved revision."

func op(id, summary string, params []any, body string, responses map[string]any) map[string]any {
	o := map[string]any{"operationId": id, "summary": summary, "responses": responses}
	if len(params) > 0 {
		o["parameters"] = params
	}
	if body != "" {
		o["requestBody"] = map[string]any{"required": true, "content": content(ref(body))}
	}
	return o
}

func paths() map[string]any {
	run := []any{idParam}
	p := map[string]any{
		"/api/v1/artifacts":                                 artifactCollectionPath(),
		"/api/v1/artifacts/{artifact}":                      artifactMetadataPath(),
		"/api/v1/artifacts/{artifact}/revisions/{revision}": artifactRevisionPath(),
		"/api/v1/artifacts/{artifact}/revisions":            artifactRevisionsPath(),
		"/api/v1/artifacts/export":                          artifactExportPath(),
		"/api/v1/artifacts/import":                          artifactImportPath(),
		"/api/v1/openapi.json": map[string]any{"get": op("getOpenAPI", "This document.", nil, "",
			map[string]any{"200": reply("The OpenAPI document.", map[string]any{"type": "object"})})},
		"/api/v1/config/validate": map[string]any{"post": op("validateConfig",
			"Validate a configuration with the debugger's Collector registry. An invalid configuration is a 200 with valid false.",
			nil, "InspectRequest", map[string]any{
				"200": reply("The inspection.", ref("Inspection")),
				"400": failure("Malformed request."),
				"403": failure(forbiddenPath),
				"404": failure(unknownRevision),
				"422": failure("The request could not be served, for example an unreadable file."),
				"503": failure(noWorkspace),
			})},
		"/api/v1/config/graph": map[string]any{"post": op("configGraph", "The static pipeline graph of a configuration.",
			nil, "InspectRequest", map[string]any{
				"200": reply("The graph, when the configuration is valid.", ref("ConfigGraph")),
				"400": failure("Malformed request."),
				"403": failure(forbiddenPath),
				"404": failure(unknownRevision),
				"422": failure("The request could not be served."),
				"503": failure(noWorkspace),
			})},
		"/api/v1/samples": map[string]any{"get": op("listSamples",
			"The embedded samples: a configuration, an OTLP JSON input for a quick run (fixture_json) and a scenario with its inputs inlined.",
			nil, "", map[string]any{
				"200": reply("The samples in display order.", map[string]any{"type": "array", "items": ref("Sample")}),
			})},
		"/api/v1/ottl/evaluate": map[string]any{"post": op("evaluateOTTL",
			"Evaluate a transform processor's statements on an OTLP JSON payload without running the Collector (the OTTL playground). The result is never verified: rerun to check it against the processor.",
			nil, "OTTLRequest", map[string]any{
				"200": reply("The evaluation; an invalid configuration is valid false with diagnostics.", ref("OTTLEvaluation")),
				"400": failure("Malformed request, an unknown processor or an undecodable input."),
				"403": failure(forbiddenPath),
				"404": failure(unknownRevision),
				"422": failure("The request could not be served."),
				"503": failure(noWorkspace),
			})},
		"/api/v1/runs": map[string]any{
			"post": op("createRun", "Capture and validate inputs, then queue a run. Runs execute one at a time.", nil, "Request", map[string]any{
				"202": reply("The queued run.", ref("Run")),
				"400": failure("Malformed request."),
				"403": failure(forbiddenPath),
				"404": failure(unknownRevision),
				"413": failure("Source capture, storage admission or evidence reservation exceeds its limits."),
				"422": failure("Sources could not be read or validated; no run was admitted."),
				"500": failure("The store could not durably accept the run."),
				"503": failure("The platform queue is full, execution admission is closed or artifact references were sent to a server that keeps no saved work."),
			}),
			"get": op("listRuns", "A page of retained runs, newest first, without reports but each with its summary when it has one. next_cursor continues the same filter; a team server keeps its runs across restarts.", []any{
				map[string]any{"name": "scenario", "in": "query", "schema": map[string]any{"type": "string"}, "description": "Only runs of this scenario name."},
				map[string]any{"name": "runtime", "in": "query", "schema": map[string]any{"type": "string"}, "description": "Only runs on this runtime name (built-in for the debugger's own)."},
				map[string]any{"name": "result", "in": "query", "schema": map[string]any{"type": "string", "enum": platform.RunResults},
					"description": "How execution ended for a run with a report, otherwise the run's state."},
				map[string]any{"name": "limit", "in": "query", "schema": map[string]any{"type": "integer", "minimum": 1, "maximum": platform.MaxRunPage, "default": platform.DefaultRunPage}},
				map[string]any{"name": "cursor", "in": "query", "schema": map[string]any{"type": "string"}, "description": "next_cursor of the previous page of the same filter."},
			}, "", map[string]any{
				"200": reply("A page of runs.", ref("RunPage")),
				"400": failure("An unknown result value, or an invalid limit or cursor."),
			}),
		},
		"/api/v1/runs/{id}": map[string]any{
			"get": op("getRun", "A run and, once done, its report without snapshot stores.", run, "", map[string]any{
				"200": reply("The run.", ref("Run")),
				"404": failure("Unknown run, or a deleted run (\"run was deleted\")."),
			}),
			"delete": op("deleteRun",
				"Delete a finished run: its report, evidence, captured inputs and recorded evaluations. Later requests naming it are 404 \"run was deleted\". Saved sources and their origins are unchanged. A run of a suite is deleted with its suite.",
				run, "", map[string]any{
					"204": map[string]any{"description": "The run was deleted."},
					"404": operationFailure("Unknown run, or a run already deleted."),
					"409": operationFailure("The run is queued or running (cancel it first), or belongs to a suite, which the message names."),
					"500": operationFailure("The deletion could not be published."),
					"503": operationFailure("The server is shutting down."),
				}),
		},
		"/api/v1/runs/{id}/report": map[string]any{"get": op("getReport",
			"The complete report, identical to the CLI's JSON output.",
			append(run, map[string]any{"name": "wait", "in": "query", "schema": map[string]any{"type": "boolean"},
				"description": "Block until the run finishes."}),
			"", map[string]any{
				"200": reply("The report.", ref("Report")),
				"404": failure("Unknown run."),
				"409": failure("The run has not finished, was cancelled before it started or was interrupted by owner exit."),
				"500": failure("Terminal evidence publication failed."),
				"422": failure("The engine could not run the request."),
			})},
		"/api/v1/runs/{id}/inputs": map[string]any{"get": op("getRunInputs",
			"The sources a run was admitted with, in any state, read from its captured inputs: the root configuration text, the quick input, setup and assertions or the scenario, with labels and saved revisions. Environment values and captured file contents are never returned; files and required environment values are named only. Sent with Cache-Control: no-store.",
			run, "", map[string]any{
				"200": reply("The run's inputs.", ref("RunInputs")),
				"404": failure("Unknown run, or a run without captured inputs."),
				"500": failure("The captured inputs could not be read."),
			})},
		"/api/v1/runs/{id}/log": map[string]any{"get": op("getRunLog",
			"The tail of a failed or partial run's Collector log: at most 64 KB of whole lines at info level and above, with every environment value the configuration read redacted. Only a run that failed, or whose execution ended partial or failed, keeps it; kept is false otherwise. It is deleted with the run. Sent with Cache-Control: no-store.",
			run, "", map[string]any{
				"200": reply("The run's log.", ref("RunLog")),
				"404": failure("Unknown or deleted run."),
			})},
		"/api/v1/runs/{id}/summary": map[string]any{"get": op("getRunSummary",
			"A finished run's compact summary, what MCP's linea_get_run returns: execution status, completeness, assertion counts, stages and outputs grouped and capped, and the first errors. Derived from the report only.",
			run, "", map[string]any{
				"200": reply("The summary.", ref("RunSummary")),
				"404": operationFailure("Unknown run."),
				"409": operationFailure("The run has not finished."),
			})},
		"/api/v1/runs/{id}/evidence": map[string]any{"get": op("queryRunEvidence",
			"One bounded page of a finished run's recorded evidence, as MCP's linea_get_execution_trace: stage_changes, records, journey, statements, events or recommendations. Never executes anything. A cursor continues only the same run, family and parameters; page sizes above the server's budgets are lowered and listed in page.clamped.",
			append(run, evidenceParams...), "", map[string]any{
				"200": reply("One page of evidence.", ref("EvidenceResult")),
				"400": operationFailure("A bad family, parameter, page size or cursor."),
				"404": operationFailure("Unknown run, stage, record or processor."),
				"409": operationFailure("The run has not finished or has no report."),
			})},
		"/api/v1/runs/{id}/evaluations": map[string]any{"get": op("listRunEvaluations",
			"The assertion evaluations recorded beside a run, in recording order, without their results. MCP's linea_evaluate_assertions records one; POST /runs/{id}/assertions does not.",
			run, "", map[string]any{
				"200": reply("The evaluations.", map[string]any{"type": "array", "items": ref("EvaluationInfo")}),
				"404": operationFailure("Unknown run."),
			})},
		"/api/v1/runs/{id}/evaluations/{evaluation}": map[string]any{"get": op("getRunEvaluation",
			"One recorded evaluation of the run with its assertion report. The scenario text it evaluated is never returned.",
			[]any{idParam, map[string]any{"name": "evaluation", "in": "path", "required": true, "schema": map[string]any{"type": "string"}}}, "", map[string]any{
				"200": reply("The evaluation.", ref("Evaluation")),
				"404": operationFailure("Unknown run, or no such evaluation of this run."),
			})},
		"/api/v1/runs/{id}/statements/test": map[string]any{"post": op("testStatementOnRun",
			"Test an unsaved candidate (one statement edit or complete proposed YAML) on the run's recorded configuration and inputs, as MCP's linea_test_statement with run_id: the recorded configuration is the base, the recorded inputs become a self-contained scenario run, and the runtime defaults to the one the run's report names. The run is queued, not awaited; nothing is saved or changed.",
			run, "StatementTestRequest", map[string]any{
				"200": reply("The candidate is invalid; no run was admitted.", ref("StatementTest")),
				"202": reply("The candidate and its queued run.", ref("StatementTest")),
				"400": operationFailure("Malformed request, an invalid edit, invalid assertions or not exactly one of edit and proposed_yaml."),
				"404": operationFailure("Unknown run or runtime."),
				"409": operationFailure("A stale edit (current_statement holds the current text), a run that read environment values, which are never recorded, or a run without a report naming its runtime."),
				"413": operationFailure("Storage admission or evidence reservation exceeds its limits."),
				"503": operationFailure("The platform queue is full or execution admission is closed."),
			})},
		"/api/v1/runs/{id}/cancel": map[string]any{"post": op("cancelRun",
			"Cancel a queued or running run. A running run still reports, with status cancelled. Send an empty JSON body.",
			run, "", map[string]any{
				"202": reply("The run.", ref("Run")),
				"500": failure("Queued cancellation could not be persisted."),
				"404": failure("Unknown run."),
			})},
		"/api/v1/runs/{id}/events": map[string]any{"get": op("streamRunEvents",
			"Server-sent events: recorded events, then live ones (event \"event\", data Event, id \"<attempt>:<seq>\"), then one \"end\" (data End). A run requeued after its worker was lost executes again: event \"restart\" (data Restart, id \"<attempt>:0\") then replaces the events received so far with the new attempt's. Last-Event-ID resumes after an id.",
			append(run, map[string]any{"name": "Last-Event-ID", "in": "header", "schema": map[string]any{"type": "string", "pattern": "^([0-9]+:)?[0-9]+$"}}),
			"", map[string]any{
				"200": map[string]any{"description": "The event stream.", "content": map[string]any{"text/event-stream": map[string]any{"schema": map[string]any{"type": "string"}}}},
				"404": failure("Unknown run."),
			})},
		"/api/v1/runs/{id}/snapshots/{ref}": map[string]any{"get": op("getSnapshot", "An OTLP JSON telemetry snapshot of the run.",
			[]any{idParam, refParam}, "", map[string]any{
				"200": reply("OTLP JSON.", map[string]any{"type": "object"}),
				"404": failure("Unknown run or snapshot."),
			})},
		"/api/v1/runs/{id}/assertions": map[string]any{"post": op("evaluateAssertions",
			"Evaluate the assertions of an edited scenario against a done run's recorded observations, without running again. The scenario's inputs are not used.",
			run, "AssertionsRequest", map[string]any{
				"200": reply("The assertion report.", ref("AssertionReport")),
				"400": failure("Malformed request."),
				"404": failure("Unknown run."),
				"409": failure("The run has no report."),
				"422": failure("The scenario is not valid."),
			})},
		"/api/v1/candidates": map[string]any{"post": op("createCandidate",
			"Validate a proposed configuration change from a base: complete proposed YAML or one transform statement edit, spliced into the base text without re-serializing it. Returns the proposed text, a unified patch and the affected pipelines. With save, a valid proposal becomes a new configuration artifact; the base, its head and host files are never changed.",
			nil, "CandidateRequest", map[string]any{
				"200": reply("The candidate, valid or not.", ref("Candidate")),
				"201": reply("The candidate, saved as a new configuration artifact.", ref("Candidate")),
				"400": operationFailure("Malformed request, an invalid edit or an unsupported statement shape."),
				"403": operationFailure(forbiddenPath),
				"404": operationFailure(unknownRevision),
				"409": operationFailure("The edit is stale: the statement no longer reads as expected. current_statement holds its current text."),
				"413": operationFailure(artifactLimitExceeded + "."),
				"422": operationFailure("The base configuration could not be read."),
				"503": operationFailure(noWorkspace + " Saving needs one too."),
			})},
		"/api/v1/runs/{id}/scenario": map[string]any{"post": op("createScenarioFromRun",
			"Derive a self-contained scenario from a run's captured inputs, in any state, with captured files inlined; optional assertions are appended and the name replaced. The run is never executed or changed. Environment values are never copied: required_environment names what a rerun must supply. With save, the scenario becomes a new scenario artifact.",
			run, "ScenarioFromRunRequest", map[string]any{
				"200": reply("The scenario.", ref("ScenarioFromRun")),
				"201": reply("The scenario, saved as a new scenario artifact.", ref("ScenarioFromRun")),
				"400": operationFailure("Malformed request, invalid assertions, or captured text that cannot be written as YAML."),
				"404": operationFailure("Unknown run, or a run without captured inputs."),
				"413": operationFailure(artifactLimitExceeded + "."),
				"500": operationFailure("The captured inputs could not be read."),
				"503": operationFailure("Saving needs a server that keeps saved work."),
			})},
		"/api/v1/runs/{id}/export": map[string]any{"post": op("exportScenario",
			"Export a run as a reviewable scenario file: the scenario createScenarioFromRun derives, after a comment header naming the source run, the build that admitted it, the required environment names, its reproducibility and what was left out (the configuration, host environment values and file contents captured by configuration providers). Inline inputs are recorded telemetry. Nothing is saved or written on the server.",
			run, "ExportRequest", map[string]any{
				"200": reply("The export.", ref("ScenarioExport")),
				"400": operationFailure("Malformed request, invalid assertions, or captured text that cannot be written as YAML."),
				"404": operationFailure("Unknown run, or a run without captured inputs."),
				"500": operationFailure("The captured inputs could not be read."),
			})},
		"/api/v1/suites": map[string]any{
			"post": op("createSuite",
				"Submit 1 to 32 scenarios against one configuration. Capacity for every member is reserved first; members are then prepared and admitted in order as ordinary scenario runs. A member that cannot be admitted carries its error instead of a run ID.",
				nil, "SuiteRequest", map[string]any{
					"202": reply("The suite.", ref("Suite")),
					"400": failure("Malformed request, or no configuration or 1 to 32 scenarios."),
					"403": failure(forbiddenPath),
					"404": failure(unknownRevision),
					"500": failure("The suite could not be recorded."),
					"503": failure("The run queue cannot hold every member (retry later), or " + noWorkspace),
				}),
			"get": op("listSuites", "Retained suites, newest first: each suite's admission metadata (configuration label and members with their run IDs or admission errors), without member states or outcomes; read those with getSuite.", nil, "", map[string]any{
				"200": reply("The suites.", map[string]any{"type": "array", "items": ref("Suite")}),
			}),
		},
		"/api/v1/suites/{id}": map[string]any{
			"get": op("getSuite",
				"A suite with each member's state and outcome, derived from its runs.",
				run, "", map[string]any{
					"200": reply("The suite.", ref("SuiteStatus")),
					"404": failure("Unknown suite, or a deleted suite (\"suite was deleted\")."),
				}),
			"delete": op("deleteSuite",
				"Delete a suite and its member runs, each as deleteRun does. Later requests naming the suite are 404 \"suite was deleted\".",
				run, "", map[string]any{
					"204": map[string]any{"description": "The suite and its runs were deleted."},
					"404": operationFailure("Unknown suite, or a suite already deleted."),
					"409": operationFailure("A member run is queued or running; cancel it first."),
					"500": operationFailure("The deletion could not be published."),
					"503": operationFailure("The server is shutting down."),
				}),
		},
		"/api/v1/suites/{id}/report": map[string]any{"get": op("getSuiteReport",
			"The test report of a suite, as linea test prints it: one result per member in order and the worst outcome. With ?wait=true the request blocks until every member finishes; ending the request never cancels them. Otherwise an unfinished suite is a 409.",
			append(run, map[string]any{"name": "wait", "in": "query", "schema": map[string]any{"type": "boolean"}}), "", map[string]any{
				"200": reply("The report.", ref("TestReport")),
				"404": failure("Unknown suite."),
				"409": failure("The suite is not finished."),
			})},
		"/api/v1/suites/{id}/rerun": map[string]any{"post": op("rerunSuite",
			"Admit a new suite with this suite's recorded inputs: the configuration's root text and each member's self-contained scenario, derived from its run's captured inputs as createScenarioFromRun does, on the runtime a member's report names. No scenario or input file is read again; nothing new is stored. Returns without waiting, as createSuite.",
			run, "", map[string]any{
				"202": reply("The new suite.", ref("Suite")),
				"404": operationFailure("Unknown suite."),
				"409": operationFailure("A member was never admitted, read environment values (which are never recorded) or ran another configuration, or no member's report names a runtime; the message says which."),
				"500": operationFailure("A member's captured inputs could not be read, or the suite could not be recorded."),
				"503": operationFailure("The run queue cannot hold every member (retry later)."),
			})},
		"/api/v1/suites/{id}/junit": map[string]any{"get": op("getSuiteJUnit",
			"A finished suite's test report as JUnit XML, as linea test --report writes it: one testsuite per member and one testcase per assertion; inconclusive is a failure, never skipped.",
			run, "", map[string]any{
				"200": map[string]any{"description": "The JUnit XML, as an attachment named <suite>.junit.xml.", "content": map[string]any{"application/xml": map[string]any{"schema": map[string]any{"type": "string"}}}},
				"404": operationFailure("Unknown suite."),
				"409": operationFailure("The suite is not finished."),
			})},
		"/api/v1/comparisons": map[string]any{"post": op("createComparison",
			"Run two configurations on one input: the before run is prepared as createRun does, and the after run on a copy of its captured inputs, so no input is read twice. Capacity for both is reserved first; both are admitted, or neither. Both configurations run unmodified. Returns without waiting; compare the runs with getComparison.",
			nil, "ComparisonRequest", map[string]any{
				"202": reply("The two queued runs.", ref("ComparisonRuns")),
				"400": failure("Malformed request."),
				"403": failure(forbiddenPath),
				"404": failure(unknownRevision),
				"413": failure("Source capture, storage admission or evidence reservation exceeds its limits."),
				"422": failure("Sources could not be read or either configuration could not be validated; no run was admitted."),
				"500": failure("The store could not durably accept the runs."),
				"503": failure("The platform queue cannot hold both runs (retry later), execution admission is closed or artifact references were sent to a server that keeps no saved work."),
			})},
		"/api/v1/runs/{id}/compare/{after}": map[string]any{"get": op("getComparison",
			"Compare two terminal runs from their recorded evidence: outcome same, different or inconclusive with reasons; per-exporter records; per injected record fates and field changes when the inputs are proven identical; routes, errors, assertions and ignore-rule counts. Nothing executes and nothing is persisted. Equal record counts alone never make two runs same.",
			[]any{idParam,
				map[string]any{"name": "after", "in": "path", "required": true, "schema": map[string]any{"type": "string"}, "description": "The run compared with the run {id}."},
				map[string]any{"name": "ignore", "in": "query", "schema": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "style": "form", "explode": true,
					"description": "OTTL paths whose differences are counted per rule but do not make the runs different, such as log.attributes[\"request.id\"]."},
				map[string]any{"name": "format", "in": "query", "schema": map[string]any{"type": "string", "enum": []string{"json", "markdown"}},
					"description": "markdown returns the comparison rendered as Markdown (text/markdown)."},
				map[string]any{"name": "wait", "in": "query", "schema": map[string]any{"type": "boolean"},
					"description": "Block until both runs finish; ending the request never cancels them."},
			}, "", map[string]any{
				"200": map[string]any{"description": "The comparison.", "content": map[string]any{
					"application/json": map[string]any{"schema": ref("RunComparison")},
					"text/markdown":    map[string]any{"schema": map[string]any{"type": "string"}},
				}},
				"400": failure("An ignore rule is not a path the comparison can match, or an unknown format."),
				"404": failure("Unknown run."),
				"409": failure("A run has not finished or has no report."),
			})},
		"/api/v1/runs/{id}/diffs": map[string]any{"get": op("getDiffs", "Stage outcomes, diffs and OTTL replays of a done run.", run, "", map[string]any{
			"200": reply("The diffs.", ref("Diffs")),
			"404": failure("Unknown run."),
			"409": failure("The run has no report."),
		})},
	}
	for path, item := range runRetentionPaths() {
		p[path] = item
	}
	for path, item := range runtimePaths() {
		p[path] = item
	}
	for path, item := range runtimeBuildPaths() {
		p[path] = item
	}
	for path, item := range runtimeStoragePaths() {
		p[path] = item
	}
	for path, item := range upgradePaths() {
		p[path] = item
	}
	for path, item := range bisectPaths() {
		p[path] = item
	}
	for path, item := range fleetPaths() {
		p[path] = item
	}
	for path, item := range captureManagementPaths() {
		p[path] = item
	}
	for path, item := range collectorPaths() {
		p[path] = item
	}
	for path, item := range capturePaths() {
		p[path] = item
	}
	for path, item := range captureDataPaths() {
		p[path] = item
	}
	for path, item := range auditPaths() {
		p[path] = item
	}
	for path, item := range tokenPaths() {
		p[path] = item
	}
	for path, item := range accessPaths() {
		p[path] = item
	}
	for path, item := range projectPaths() {
		p[path] = item
	}
	for path, item := range sessionPaths() {
		p[path] = item
	}
	p["/api/v1/captures/{id}"].(map[string]any)["patch"] = op("updateCaptureMetadata", "Edit name and description with expected revision. Payloads, provenance, selection and retention are immutable.", []any{idParam}, "CaptureMetadataUpdate", map[string]any{"200": reply("Capture with updated details.", ref("CaptureSession")), "400": operationFailure("Invalid metadata."), "403": operationFailure("Owner access required."), "404": operationFailure("Capture not found or expired."), "409": operationFailure("Metadata revision changed."), "500": operationFailure("Metadata publication failed.")})
	p["/api/v1/captures/{id}"].(map[string]any)["delete"] = op("deleteCaptureSession", "Delete a terminal capture after ingress grace; derived saved sources/runs remain.", []any{idParam}, "", map[string]any{"200": reply("Capture deleted.", ref("CaptureDeleted")), "404": operationFailure("Capture not found."), "409": operationFailure("Capture still open or reserved.")})
	for _, item := range p {
		for _, o := range item.(map[string]any) {
			o := o.(map[string]any)
			params, _ := o["parameters"].([]any)
			o["parameters"] = append(params, projectParam)
		}
	}
	return p
}

type schemaGen struct {
	schemas map[string]any
	names   map[reflect.Type]string
	owners  map[string]reflect.Type
}

var (
	timeType = reflect.TypeOf(time.Time{})
	rawType  = reflect.TypeOf(json.RawMessage{})
)

// schema returns the JSON schema of t, registering named structs as
// components.
func (g *schemaGen) schema(t reflect.Type) (map[string]any, error) {
	switch {
	case t == timeType:
		return map[string]any{"type": "string", "format": "date-time"}, nil
	case t == rawType:
		return map[string]any{}, nil
	}
	switch t.Kind() {
	case reflect.Pointer:
		return g.schema(t.Elem())
	case reflect.String:
		return map[string]any{"type": "string"}, nil
	case reflect.Bool:
		return map[string]any{"type": "boolean"}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}, nil
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}, nil
	case reflect.Interface:
		return map[string]any{}, nil
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return map[string]any{"type": "string", "contentEncoding": "base64"}, nil
		}
		items, err := g.schema(t.Elem())
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "array", "items": items}, nil
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return nil, fmt.Errorf("openapi: map key %s is not a string", t.Key())
		}
		values, err := g.schema(t.Elem())
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "object", "additionalProperties": values}, nil
	case reflect.Struct:
		return g.component(t)
	}
	return nil, fmt.Errorf("openapi: unsupported type %s", t)
}

func (g *schemaGen) component(t reflect.Type) (map[string]any, error) {
	if name, ok := g.names[t]; ok {
		return ref(name), nil
	}
	name := t.Name()
	if n, ok := schemaNames[t]; ok {
		name = n
	}
	if name == "" {
		return nil, fmt.Errorf("openapi: anonymous struct %s", t)
	}
	if other, taken := g.owners[name]; taken && other != t {
		pkg := t.PkgPath()[strings.LastIndex(t.PkgPath(), "/")+1:]
		name = strings.ToUpper(pkg[:1]) + pkg[1:] + name
	}
	g.names[t], g.owners[name] = name, t
	props := map[string]any{}
	required := []string{}
	if err := g.fields(t, props, &required); err != nil {
		return nil, err
	}
	sort.Strings(required)
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	g.schemas[name] = s
	return ref(name), nil
}

// fields adds t's JSON fields, inlining embedded structs as encoding/json
// does.
func (g *schemaGen) fields(t reflect.Type, props map[string]any, required *[]string) error {
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" || (!f.IsExported() && !f.Anonymous) {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				if err := g.fields(ft, props, required); err != nil {
					return err
				}
				continue
			}
		}
		if name == "" {
			name = f.Name
		}
		s, err := g.schema(f.Type)
		if err != nil {
			return fmt.Errorf("%s.%s: %w", t.Name(), f.Name, err)
		}
		props[name] = s
		if !strings.Contains(opts, "omitempty") && !strings.Contains(opts, "omitzero") && !nilable(f.Type) {
			*required = append(*required, name)
		}
	}
	return nil
}

// nilable types may encode as null, so they are never required.
func nilable(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Interface:
		return true
	}
	return false
}
