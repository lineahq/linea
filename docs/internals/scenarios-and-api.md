# Scenarios, engine and API

Code: `internal/debugger/scenario/`, `internal/debugger/engine/`,
`internal/debugger/api/`.

## Scenario schema version 1

This is the implemented subset of the scenario format. Example:
`testdata/traces/scenario.yaml`.

```yaml
version: 1
name: checkout-traces
inputs:                      # injected in order
  - receiver: otlp           # optional when one receiver feeds the signal
    signal: traces           # optional; checked against the file content
    file: traces.json        # relative to the scenario file
    format: otlp-json        # optional; the only supported format
  - signal: logs
    after: 2s                # optional wait before this input; see Timeline
    data: {resourceLogs: [...]}  # inline OTLP JSON instead of file
environment:                 # ${env:NAME} values; see config-resolution.md
  CHECKOUT_DEPLOYMENT_ENV: scenario
execution:
  timeout: 30s
  settle: 3s                 # optional wait after the last input
  timestamps: rebase         # optional; default recorded; see Timestamps
assertions: []               # see assertions.md
```

Each input has exactly one of `file` and `data`. `data` is either a string
holding OTLP JSON or the same document written as a YAML mapping (it is
re-encoded as JSON, so OTLP JSON pasted into YAML works unchanged).
`scenario.Parse(raw, dir)` parses scenario text: with no directory (text
from the web editor or the API's `scenario_yaml`) a relative `file` is an
error that says to use `data` or an absolute path.

Decoding is strict (`KnownFields`). Sections the debugger does not implement are
rejected instead of being silently ignored, so a scenario never appears to test
something it does not. `mocks`, inputs' `client_address` and
`headers`, `defaults.headers` and `execution.drain_timeout` are specified in [mocking.md](mocking.md).
`assertions` is specified in [assertions.md](assertions.md).

## Timeline

Components that act on a timer (`batch` timeouts, `tail_sampling`
decisions) see inputs at the times a run sends them, so a scenario can
space them out. All waits are real wall-clock time.

- `inputs[].after` (Go duration, positive) waits before the input is sent,
  measured from the previous input. It is reported as `inputs[].after` in
  the result.
- `execution.settle` (Go duration, positive) waits after the last input
  before the Collector shuts down. Without it the run derives a settle from
  the config: currently only a `tail_sampling` processor with
  `drop_pending_traces_on_shutdown` needs one
  ([components.md](components.md#timer-driven-components)). A declared
  settle replaces the derived one, even when shorter. The result's
  `settle` gives the duration and its reason (`execution.settle`, or the
  components it was derived for); it is absent when the run did not wait.
- A quick run's `setup_yaml` may declare `execution.settle`; it has no
  `after`.
- The sum of the waits must fit in the run's timeout: otherwise the run is
  refused before it starts, with an error asking to raise
  `execution.timeout` (`TestWaitsLongerThanTheTimeoutAreRefused`).
- Reference runs follow the same timeline, so a comparison stays like for
  like.
- The batch processor sends what it holds when its `timeout` expires: an
  input sent after a longer wait leaves in a batch of its own
  (`TestInputsAfterAWaitLeaveTheBatchSeparately`).

## Timestamps

Input records keep the timestamps written in the scenario
(`execution.timestamps: recorded`, the default). An expression that
compares them with the wall clock, such as a filter condition
`time < Now() - Duration("1h")`, then gives a different outcome depending
on when the run happens: a fixture recorded months ago is entirely "stale".

- `execution.timestamps: rebase` shifts every set input timestamp (span
  start, end and event times; log time and observed time; data point start
  time, time and exemplars) by the same offset, so that the latest one falls
  at the run's start, truncated to whole seconds. Spacing between records is
  kept, and unset timestamps stay unset. Files read by a mocked filelog
  receiver are not rewritten.
- The engine shifts the inputs once, before both the instrumented and the
  reference run, so the comparison stays like for like.
- The report's `timestamps` gives the mode, `latest_input` (as written),
  `offset` (the run's start minus `latest_input`: the shift when rebased,
  the inputs' age otherwise) and `wall_clock`: each processor or connector
  expression calling OTTL `Now()` (`runtime.WallClockReads`, searching the
  marshalled component configs of the pipelines' processors and
  connectors).
- Inspect reports each wall-clock read as a `wall_clock` warning unless the
  scenario or quick-run setup rebases. Run views and the CLI warn likewise.
- Normalized reports subtract the offset from all recorded telemetry again
  and omit `offset`, so they stay byte-identical across runs
  (`TestNormalizedRebasedReportsAreIdentical`). Timestamps a component
  computed from `Now()` remain run-dependent.
- `TestRebasedTimestampsFollowTheRunsClock` runs
  `testdata/components/wall_clock.yaml` with `wall_clock_rebase.yaml`: as
  recorded the filter drops both 2024 logs, rebased only the one two hours
  older than the latest.

## Engine

Both adapters submit through the platform. `engine.Prepare(ctx, Request)` captures
config providers, root source bytes, input files and mock dependencies before
admission, without starting a Collector or rewriting YAML. `engine.Execute` then
parses fresh pdata/mock objects from those private captures. Direct engine callers
prepare at invocation. Relative scenario paths retain their preparation-time base.
Preparation failures return submission errors without creating a run handle;
runtime startup/execution errors after admission remain retained run errors.

`engine.Execute(ctx, Request)` performs:

1. Instrumented run.
2. Stage analysis.
3. OTTL replay for transform stages.
4. Assertion evaluation, when the scenario declares assertions.
5. With `compare`, the reference run and the comparison.

It returns `engine.Report` (`report_schema` 0.14, `engine.ReportSchemaVersion`).
`engine.Test` runs several scenarios for the `test` command. `engine.Encode` is
the only serializer, so CLI JSON output and API responses are byte-identical
(`TestAPIMatchesCLIOutput`). The report is deterministic for deterministic
pipelines: running it twice gives byte-identical output.

## Text sources

`engine.Request` and `engine.InspectRequest` take the configuration as a
path (`config`) or as text (`config_yaml`), and the scenario as a path
(`scenario`) or text (`scenario_yaml`). A quick run has no scenario: it
takes one OTLP JSON payload as a path (`fixture`) or as text
(`fixture_json`), with an optional `receiver`, `client_address` and
`setup_yaml` (mocks, environment and execution in scenario syntax; see
[mocking.md](mocking.md)). Exactly one of `fixture`, `fixture_json`,
`scenario` and `scenario_yaml` is accepted, and `receiver`,
`client_address` and `setup_yaml` only with a fixture. An inspection takes
at most one of `scenario`, `scenario_yaml` and `setup_yaml`; provenance
records the setup as `setup`. An inline fixture runs exactly like the same file:
same provenance hash and outputs
(`TestExecuteRunsInlineFixtureLikeAFixtureFile`). Configuration text resolves through
the upstream `yamlprovider` as a `yaml:<text>` URI (`config.Source`), the
same provider the OCB manifest ships, so it is unmarshalled and validated
exactly as a file would be (verified v1.68.0: the provider wraps the whole
remainder after `yaml:` with `confmap.NewRetrievedFromYAML`). Provenance
hashes text inputs with an empty path. The API has no path fields: scenario
text names no host file, so its inputs and mocks must be inline (the connected
CLI inlines them). Invalid scenario
or setup text in an inspection request produces a `scenario_ignored` warning
("scenario not applied" or "quick run not applied"), and the
configuration is still validated without the scenario's environment. Mocked
components the scenario gives nothing to read produce `mock_fixture_missing`
warnings ([mocking.md](mocking.md)). `Request.RunID` and
`Request.Observer` (both `json:"-"`) let a caller pick the run ID and receive
events as they are recorded.

## API

Code: `internal/debugger/api/`. The contract is
`internal/debugger/api/openapi.json` (OpenAPI 3.1), also served at
`GET /api/v1/openapi.json`. Paths are written by hand in `openapi.go`; component
schemas are generated by reflection over the Go types' json tags (fields with
`omitempty` or of nilable kinds are optional), so the contract follows the
encoder. `TestOpenAPIIsCurrent` fails when the committed file is stale;
regenerate it with
`go test ./internal/debugger/api -run TestOpenAPIIsCurrent -update`.

The table covers the core endpoints; `openapi.json` lists all of them, and
`architecture.md` names the endpoint groups the table leaves out.

Every operation works in one project, named by the `project` query parameter
(`default` when absent; naming it twice is 400). A run, suite, artifact or
capture of another project is refused as forbidden or not found, as if it did
not exist. Project and library endpoints are listed in `architecture.md`.

| Endpoint | Purpose |
| --- | --- |
| `POST/GET /api/v1/artifacts` | Explicitly save source revisions or list saved metadata in the server's store; separate from run history |
| `POST /api/v1/artifacts/{artifact}/copies` | `CopyRequest{revision_id?, label?}` → 201 `SavedArtifact`: copy a revision into the request's project as a new artifact with the revision's origin; into `library` it publishes and needs `library.publish` |
| `GET /api/v1/library[?kind&limit&cursor]` | `LibraryPage{revision, items[{artifact, audience, shared_by?, shared?}], next_cursor}`: library items shared with the request's project; with `project=library`, the whole library for publishers |
| `GET /api/v1/artifacts/{artifact}[/revisions/{revision}]` | Current source metadata / exact immutable revision bytes (base64) |
| `GET /api/v1/artifacts/{artifact}/revisions` | Revision metadata newest first, paged |
| `POST /api/v1/artifacts/export` / `POST /api/v1/artifacts/import` | Export for CI as a zip / import such a zip as new artifacts or revisions (see `docs/debugger/ci.md`) |
| `GET /api/v1/samples` | `[]Sample`: the embedded samples (`samples/`), each with its configuration, scenario (inputs inlined as `data`) and quick-run input text, and the outcome it is expected to have |
| `POST /api/v1/config/validate` | `InspectRequest` → `Inspection` (an invalid config is a 200 with `valid: false`) |
| `POST /api/v1/config/graph` | `InspectRequest` → `{valid, diagnostics, topology}` |
| `POST /api/v1/ottl/evaluate` | `OTTLRequest` → `OTTLEvaluation`: the OTTL playground. It replays one transform processor on an OTLP JSON `input` without a run. The result is always `not_verifiable`; an invalid config (for example an unparseable statement) is a 200 with `valid: false` |
| `POST /api/v1/runs` | `engine.Request` → 202 with the queued `Run` |
| `GET /api/v1/runs[?scenario&runtime&result&limit&cursor]` | `RunPage{runs, next_cursor}`: a page of the caller's runs, newest first, each with its list summary and without its report. Filters match exactly; `limit` is 1 to 500 (default 50); `cursor` is the previous page's `next_cursor` for the same filters, which is absent on the last page. A bad limit, an unknown result or a cursor of other filters is 400 |
| `GET /api/v1/suites` | Retained suites, newest first, as a bare array of `Suite` admission metadata (`id`, `created`, `config` label, members with `run_id` or `error`); no member states, outcomes or submitted text. `GET /api/v1/suites/{id}` derives those |
| `GET /api/v1/runs/{id}` | `Run`: state, times, error, and the report without snapshot stores |
| `GET /api/v1/runs/{id}/report[?wait=true]` | The complete report, byte-identical to `run` JSON output |
| `GET /api/v1/runs/{id}/log` | `RunLog{run_id, kept, log, truncated}`: a failed or partial run's Collector log tail, 64 KB of whole lines with environment values redacted; `kept: false` for other runs; `Cache-Control: no-store` |
| `POST /api/v1/runs/{id}/cancel` | Cancel a queued or running run |
| `DELETE /api/v1/runs/{id}`, `DELETE /api/v1/suites/{id}` | Delete a finished run, or a suite with its member runs: 204; 409 `OperationError` for a queued or running run or a suite member alone; later reads are 404 "run was deleted" / "suite was deleted" for a day, then 404 as unknown. Only a run's owner or an admin deletes a run |
| `PUT /api/v1/runs/{id}/keep`, `DELETE /api/v1/runs/{id}/keep` | Keep a run, or make it subject to retention again: 200 with the `Run` (`kept` is absent when false). Needs the run's owner or an admin (403 `OperationError` otherwise). Like every PUT, the request needs `Content-Type: application/json`, though it has no body |
| `POST /api/v1/runs/prune` | `RunPruneRequest{before?, project?, scenario?, runtime?, result?, dry_run?}` → `RunPruneResult{runs, bytes, dry_run}`: deletes the finished runs that are not kept, in no suite and the origin of no saved source's current revision. `dry_run` counts what the same request would delete. Admin only (403); a bad `before` or `result` is 400 |
| `GET /api/v1/storage` | `StorageUsage{limit, run_limit, used, runs, kept_runs, oldest_run?, retention{runs, age_seconds}}`: stored bytes against the limits, run counts and the retention policy (zero: unbounded); 503 on a server without saved work |
| `GET /api/v1/runs/{id}/events` | SSE: recorded then live events, then `end` |
| `GET /api/v1/runs/{id}/snapshots/{ref}` | One OTLP JSON snapshot (`sha256:<hex>`) |
| `GET /api/v1/runs/{id}/diffs` | Stage outcomes, diffs and OTTL replays |
| `POST /api/v1/runs/{id}/assertions` | `AssertionsRequest{scenario_yaml}` → `AssertionReport`: re-evaluates an edited scenario's assertions against a done run's recorded events and snapshots, without running again (`engine.EvaluateAssertions`). The whole scenario must parse; its inputs are ignored, so the result holds only while the inputs are unchanged. 409 before the run is done, 422 for an invalid scenario |

Run states: `queued`, `assigned`, `running`, `done` (a report exists; its
`result.status` tells completed/partial/cancelled/failed), `error` (the
engine returned an error, for example an invalid configuration),
`cancelled` and `lost` (see [run-lifecycle.md](run-lifecycle.md#run-states)).
Each run starts its own in-process Collector in a worker: a connected worker
on a team server, or the CLI's own process for a direct command. A team server keeps run history in PostgreSQL and object
storage, bounded by its retention policy (`retention` in the server config); a
service without a repository keeps only the newest 20 finished runs
(`platform.DefaultHistoryLimit`) in memory. Scheduling and history belong to
`internal/debugger/platform` rather than the HTTP adapter, and the same service
schedules CLI runs and suite members. See the [architecture](architecture.md)
for service ownership. Request validation (`Request.Validate`) happens before
queueing, so ambiguous requests are a 400, not a failed run.

SSE: each event is `event: event`, `id: <seq>`, `data: <instrument.Event>`. A
client that reconnects with `Last-Event-ID` gets only later events; the
browser's `EventSource` does this automatically. The stream ends with one
`event: end` whose data is `{state, status, error}`. Everything the stream
carries can also be fetched afterwards, independently of the stream. Snapshots
are content-addressed and therefore immutable, so they are served with a
long-lived private cache header.

Status codes: 400 malformed or ambiguous requests (unknown fields and data
after the one JSON document are rejected; trailing whitespace is fine),
403 forbidden (an authorization or path-policy refusal), 404 unknown run,
snapshot or endpoint, 409 report of an unfinished run, 415 non-JSON POST, 422 a
request that cannot be served (an unreadable file, an engine error), 503 when
the platform waiting queue is full (default 32 waiting runs) or execution
admission has closed. Rejected admission creates no run. A disconnected observer
does not cancel submitted work.

`linea-server` stores source artifacts in PostgreSQL and object storage, as its team
file names them (there is no in-memory server mode). Artifact
operations have typed error codes, conflict-checked heads and no-store
responses. See the [artifact contract](artifact-storage.md) for limits and
storage.

### Browser hardening

The API must not be usable from web pages the browser visits:

- Every request to `linea-server` is authenticated (API token, OIDC session or trusted
  headers from a configured proxy) and authorized by the server config's bindings.
  `linea-server` accepts any `Host` (`Options.RemoteAccess`); authentication, not a
  loopback Host check, keeps the API private. The Compose stack's proxy signs
  every request in as one local admin, so it refuses other `Host` names
  (`docs/debugger/deploy-compose.md`, Identity). Requests name no host
  file.
- An API handler built without `RemoteAccess` (tests, in-process adapters)
  answers 403 for a `Host` that is not `localhost` or a loopback IP.
  `linea-server` is never composed that way.
- POST bodies must be `Content-Type: application/json`. A cross-origin page
  can send that only after a CORS preflight, which the API never answers, so
  "simple" form or `text/plain` requests cannot start runs.
