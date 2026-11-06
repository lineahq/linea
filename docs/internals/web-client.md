# Web client

Code: `web/` (React SPA), `internal/debugger/webui/`
(embedding), `internal/debugger/api/` (the API it calls; see
[scenarios-and-api.md](scenarios-and-api.md)).

## Architecture

- An application shell (`src/ui/AppShell.tsx`) holds a rail with up to eight
  destinations (`ui/destinations.ts`, listed in `AppShell.tsx`): Debugger (`/`,
  `/runs`), OTTL playground (`/playground`), Live capture (`/captures/...`),
  Fleet (`/fleet/...`), Runtimes (`/runtimes/...`), Saved work (`/saved/...`),
  Audit log (`/audit`) and Access (`/access`). The rail hides the destinations the caller's role
  cannot use. Every route is declared in `src/router.tsx`. The rail badge counts
  captures that have not finished; Runtimes counts runtime builds in progress
  (`buildsQuery` in `runtimes/pages/queries.ts`, read every 3 s while one runs,
  no badge on a server that cannot build). The look comes from the design system
  in `src/ds/` (`tokens.css`, `components.css`, `od-*` classes) and the icon set
  `ui/Icon.tsx`.
- React 19 and TypeScript, built by Vite 8 (Rolldown). TanStack Router holds the
  selection in the URL (`?run=&node=&input=&tab=&editor=`), so a view survives a
  reload. TanStack Query fetches and caches API data.
- The workspace (configuration and scenario text) is a client-side copy. The
  user's files are never written: Export for CI, the one download, holds saved
  revisions (see the Save menu below). Drafts persist in `localStorage` only as
  a convenience.
- API types come from the committed OpenAPI contract: `pnpm gen:api` writes
  `web/src/api/schema.d.ts` (generated, committed so a type check needs no Go
  toolchain). `web/src/api/types.ts` names the schemas. When a Go API or report
  type changes: regenerate `openapi.json`
  (`go test ./internal/debugger/api -run TestOpenAPIIsCurrent -update`), then
  run `pnpm gen:api`.

## Serving

`pnpm build` writes to `internal/debugger/webui/static/app/`, which is ignored by git
and embedded with `//go:embed all:static`. The embed pattern targets
`static/`, which always contains a committed README, so `go build` works
without a web build. Such a binary serves a page that explains how to build
the client, and the API still works. Paths outside `/api/` that don't name a
file get `index.html` (client routes). Hashed `assets/` are cached as
immutable; `index.html` is `no-cache`.

For development, start the Compose stack (`docs/debugger/deploy-compose.md`) and run
`pnpm dev` beside it. Vite proxies `/api` to `127.0.0.1:7070`, the stack's
proxy, which signs in one local admin. `linea-server` has no other mode, so there is no
unauthenticated local server to proxy to.

## Offline: Monaco without a CDN

`@monaco-editor/react` loads Monaco from jsDelivr by default. That breaks
offline use (no external calls), so `src/monaco.ts` passes the bundled
`monaco-editor` to `loader.config({ monaco })`.

Verified with monaco-editor 0.57.0, whose ESM layout differs from older
releases: the package `exports` maps `monaco-editor/<path>` to
`esm/vs/<path>.js`. The UI imports:

- `monaco-editor/editor/editor.api`: the API without the bundled language
  services;
- `monaco-editor/languages/definitions/yaml/register` and
  `.../javascript/register`: Monarch tokenizers only, which need no worker;
- `monaco-editor/editor/editor.worker?worker`: the base editor worker.

JSON is read-only in the UI and is highlighted with the javascript
tokenizer (`JSON_LANGUAGE`), which avoids the JSON language worker.

## Editor diagnostics

Collector diagnostics name components, not lines. `src/editor/locate.ts`
parses the text with `yaml` (which keeps node ranges) and places each marker
on the component's key, found from `diagnostic.component` or a
`processors::<id>` path in the message. If no component is found, the marker
goes on line 1. YAML syntax errors are found client-side as the user types
and take precedence, because the Collector would only report the first one.
Validation is debounced by 350 ms. Invalid scenario text gives a
`scenario_ignored` warning instead of failing validation (see
[scenarios-and-api.md](scenarios-and-api.md)).

## Graph

`src/graph/LaneGraph.tsx` draws the topology as one lane per pipeline:
receivers, then processors, then exporters and connectors. Extensions sit in a
row above the lanes. Its pure model, `src/graph/lanes.ts`, is unit-tested:

- Receivers, exporters and connectors keep one node key each. Processors are
  keyed per pipeline (`<pipeline>|processor/<id>`), because the Collector
  builds one instance per pipeline and the debugger observes each instance
  separately.
- A connector is drawn in the lane that feeds it. A lane that consumes it
  shows a hand-off button that selects the same node.
- The search box and the signal filter dim what does not match; nothing is
  hidden or re-laid out.
- Each node carries its execution mode (`modeInfo`: real, mocked, captured,
  simulated, stand-in or unsupported) as the legend explains it, and the run's
  counts when a run is selected.

`src/graph/layout.ts` supplies the node keys, `LayoutNode` and the edges
that the annotations and paths use. React Flow is not a dependency.

## Runs and annotations

Verified against debugger API 1 / Collector v0.162.0.

- `useRun` reads `GET /runs/{id}`. While the run is queued or running, it
  follows `GET /runs/{id}/events` with an `EventSource`. It batches events
  per animation frame, because a run can emit hundreds of events in
  milliseconds. A `restart` event, sent when a run requeued after its
  worker was lost executes again, drops the events shown so far; the new
  attempt's follow. On the `end` event it invalidates the run query. Once the
  run has finished, the query never goes stale. Events then come from
  `report.result.events`, so a reload doesn't replay SSE.
- Node stats are derived from events, keyed like the layout. Processors are
  keyed `pipeline|processor/id`, and receivers and exporters by ref.
  "Dropped" is the sum of the removed items in the stage diffs. It is not
  input minus output, since that would count batch deferral as loss.
- An edge's label is what a connector delivered into the edge's
  pipeline (out of a connector), else the source processor's output
  count, else the target processor's input count.
- Connector events (`source: connector_wrapper`) map to the shared
  `connector/<id>` node, never to a processor key, although a connector's
  `stage.output` carries the pipeline it delivered into.
- Limitations (`runs/limitations.ts`) come only from the report. They are
  never inferred from the configuration, so they describe what this run
  actually did. Mock declarations (`result.mocks`) contribute their warnings
  and limitations, prefixed with the component and provider. The run
  summary's "Mocked dependencies" table lists what each one supplied.
- A run's More menu offers Export for CI. It opens the run's configuration and scenario
  (`runExportDraft` in `workspace/drafts.ts`): the run's own sources, saved
  ones still linked, or the self-contained scenario from
  `POST /runs/{id}/scenario` when the run read files beside its scenario
  (`runExportNeedsScenario`). It then shows `ExportForCI.tsx`, the same
  export as the Save menu.

## Telemetry inspector

Verified against debugger API 1 / Collector v0.162.0.

- **Payloads.** Run views omit snapshot bodies, and
  `GET /runs/{id}/snapshots/{ref}` returns them as OTLP JSON. A ref looks
  like `sha256:<hex>` and is sent through `encodeURIComponent`. Go's
  `PathValue` returns the unescaped ref, and an escaped `%3A` resolves
  (HTTP 200). Refs are content addressed, so the client caches them with
  `staleTime: Infinity`.
- **Stage navigation follows `from_seq`.** Previous goes to the emission
  that called the input (shown at its stage input). Next lists every event
  the output called: one button per branch where a receiver or connector
  fanned out.
  - Reports without `from_seq` (schema 0.2) fall back to refs. Previous is
    the latest earlier `stage.output` in the pipeline, or the
    `input.injected`, with the same ref. Next is the first later
    `stage.input` or `exporter.captured` with the output's ref.
  - A `deferred` input has no output of its own. It follows the stage's
    first emission after it, which may merge several inputs.
- **Changes view** (`inspect/recordViews.ts`, `RecordChanges.tsx`). One
  row per record of the stage input, then one per record the stage created.
  A row says only where the change is ("Changed in resource and record"),
  with a "N changes" pill; it opens to the evidence, fields grouped by
  Resource / Scope / Record with the OTTL path of each (`log.attributes["k"]`,
  `resource.attributes["k"]`, `instrumentation_scope.name`,
  `datapoint.value_int`), the OTLP type, and Before/After. Shared resource and
  scope edits appear on every record they apply to, in their own level.
  Metric-level edits apply to each data point. "Show all N fields" reads the
  unchanged fields from the recorded payload.
  - Dropped and created records are listed from the side they exist in. A
    dropped input and a created output can share a positional entity, so the
    row id is `<side>:<row id>`.
  - A transform adds a "By" column (S0, S1...) from `Report.ottl` statement
    `changes`, and a statements key. Per-record drop causes are not
    available: the replay counts statements per input, not per record.
  - Without a diff, both snapshots are listed as recorded.
- **Connectors** (`RoutingList.tsx`, `inspect/routing.ts`). The Routing tab
  of a routing connector lists one row per record that passes the input.
  With `Report.routing` verified (`replay_verified`) the row says which rule
  routed it ("Matched rule 0 · copied to logs/pager, and the original
  continued to the default", "No rule matched · sent to the default") and
  opens to the routing table in order: each rule's condition and action with
  matched / no match / not reached (an earlier move took the record), then
  the default. Otherwise the row only names the pipelines that received the
  record and a note says why rules are not shown. Deliveries link to what that
  pipeline did with the record. The condition value a rule read is not
  available.
- **Aggregations** (`inspect/aggregation.ts`, `AggregationList.tsx`). For a
  connector input with a `Report.aggregations` entry (count), the Aggregation
  tab shows the emitted data points in the shared record list instead of
  routes: one row per point (metric, attributes, value) that opens to the
  records it counted, and one row per record no point counted ("Not counted
  by any metric"), with a verification badge above. Records and verified
  points link to their journeys. Records are named by the lineage whose hop
  at the connector input contains the counted position (a span event is shown
  as `<span> · event[e]`). Without a verified replay no records are
  attributed and the row says so. Why a record was not counted is not
  recorded, so the view does not state a reason.
- **Journeys** (`inspect/journey.ts`, `JourneyPanel.tsx`, `?trace=<lineage
  id>`).
  - Without a selection, the Records tab (`debugger/RecordsTab.tsx`) lists
    one row per record with where it ended, with fate filter pills and search.
    `JourneyPanel` also holds a routing matrix (every injected record × the
    exporters it reached, ✓ unchanged, ✓~ changed, and where its other paths
    stopped), but `RecordsTab` mounts the panel only for a followed record.
  - Data points an aggregating connector emitted are rows of their own
    (marked Σ). A record's `aggregated` hop links to the points it went
    into; a point's journey lists the records it was counted from.
  - With a selection, it shows the hop tree. A processor's input and output
    are one row with the changes and match confidence. Branches show where
    a fan-out or connector copied the record. Clicking a hop opens that
    stage with the record selected.
  - While `trace` is set, the graph dims nodes and edges off the path. It
    marks nodes that changed the record (~), delivered it (✓) or stopped it
    (✕), and a banner names the record and its ends.
  - "Follow record" (stage and payload views, and each record in the
    Changes view) maps (event seq, entity) to the lineage whose hop is at
    that seq with that ref, or that span.
- **Entity names** follow `internal/debugger/analysis`.
  - Spans: `span:<trace>:<span>`.
  - Log records and metrics: index paths into the *before* snapshot,
    `resource[i].scope[j].log_record[k]` / `.metric[k]`.
  - To show the after side of a positional record, the client takes the
    record at the same position, after skipping removed records earlier
    in its scope. This is right only for processors that keep record
    order. That is the same assumption behind the analysis's positional
    matches, which it reports with lower confidence.
- **Large payloads.** The tree and event list are virtualized
  (`@tanstack/react-virtual`, fixed row heights). The Monaco diff editor
  uses `hideUnchangedRegions`, so long payloads collapse to their changes.
  Every list of records, rows or links renders 25 at a time (`RecordList`,
  `SignalExplorer`, `JourneyPanel`, `ChangesTable` and a data point's counted
  records, through `inspect/pages.ts` `pageOf` where they do not use
  `RecordList`), and the technical views' record picker lists at most 200
  options (`capOptions`). A new view over records, snapshot nodes or events
  must page, virtualize or cap the same way; every such view follows the rule.
- **Embedding pitfall.** The client is embedded at `go build` time. After
  `pnpm build`, rebuild the binary (or use `pnpm dev`), or `linea-server` keeps
  serving the old client.

## Sampling decisions

- A tail_sampling processor opens on its Decisions tab (Changes only
  reconciles its buffer), built from
  `report.sampling` (matched by processor and pipeline), in the shared record
  list: one row per trace whose sentence names the deciding policy ("Policy
  slow sampled it", "No policy sampled it"), with kept/dropped/undecided
  filter pills. A row opens to the ladder of policies: each one's own result,
  the evidence it read and its rule, with the deciding policy marked. There is
  no clock view. `sampling.ts` holds the pure logic; during a live run the tab
  says decisions are explained when the run ends. Details:
  [sampling.md](sampling.md).

## OTTL debugger and statement edits

- **Where it lives.** For a transform stage input, the Telemetry tab opens on an
  OTTL view built from `report.ottl`, matched by processor and input seq. For
  each statement it shows matched, not matched, unchanged, errors and changes,
  with the verification state, which says whether to trust them.
- **Editing.** The client sends a `replace` statement edit, with the statement's
  text from the run as `expected`, to `POST /api/v1/candidates` and puts the
  returned `proposed_yaml` in the configuration draft; the server's
  `internal/debugger/statementedit` splices the text. A proposal that does not
  validate is applied with the first diagnostic shown.
  - Only the statement's scalar is replaced, so comments, ordering and the
    quoting of everything else stay as they were.
  - The original quote style is tried first. A plain scalar falls back to single
    quotes and then double quotes; for example `a #b` needs quotes because ` #`
    would start a comment. Block scalars become quoted.
  - Each candidate is re-parsed and accepted only if the document equals the
    original with just that value changed (`doc.setIn`).
  - The edit is refused if the statement in the editor no longer reads as the
    report says it did, i.e. the editor changed since the run: the 409 carries
    `current_statement`, which the tab quotes.
- **Actions.** "Preview on this input" calls the playground with the edited
  config and the stage's input snapshot. It shows the edited statement's counts
  and the changes it would make, and can compare them side by side with the
  observed output. "Test on the whole run" sends the edit to
  `POST /runs/{id}/statements/test`: the server edits the run's recorded
  configuration (not the draft) and runs the run's inputs, inlined in one
  scenario, on the run's runtime with its assertions (`ottl/statementTest.ts`,
  tested). The result opens the test run or compares it with the original record
  by record: the platform proves the inlined input identical by its input
  identity. "Apply to config" writes the edit into the editor; the file on disk
  is never written. "Apply and rerun" also starts a run with that text right
  away, without waiting for the store update.

## OTTL playground

`/playground` (`playground/PlaygroundPage.tsx`, logic in
`playground/playground.ts`, tested) tries OTTL statements on a pasted OTLP
JSON payload, apart from the debugger: no open configuration, scenario or
run takes part, and nothing is run or saved. It uses the debugger's
`WorkspacePanes`: the input on the left, the statements above the result.
Its split ratios are kept under their own key (`od.playground.panes`) and
start with a short statements pane (`defaults`).

- **Input.** The payload's top-level key names the signal (`resourceLogs`,
  `resourceSpans` or `resourceMetrics`); anything else is explained instead of
  sent.
- **Statements.** One per line; blank lines and `#` lines are skipped. The page
  writes the smallest configuration around them: an OTLP receiver,
  `transform/playground` with `error_mode` and the flat `<signal>_statements`
  list (the processor infers each context from the paths), the debug exporter
  and one pipeline. Each statement is written as a JSON string, a YAML
  double-quoted scalar, so its text round-trips. Verified on the built-in
  Collector (0.162.0) with the three examples.
- **Evaluation.** `POST /ottl/evaluate` (`PreviewTransformation`, also MCP
  `linea_preview_ottl` and CLI `ottl preview`) runs 400 ms after the edits
  settle, on the runtime selected beside Run (the team's default when none is);
  the result line names the runtime that answered, from the evaluation's
  `runtime` (`evaluatedOn`, tested: "Evaluated on Built-in · Collector 0.162.0,
  without a run"), with the unverified badge when it is, and falls back to the
  selected name or "the default runtime" when the response names none. It adds
  no second runtime picker. A configuration that does not compile shows its
  diagnostics. Otherwise the result says in one line what happened to the input
  records (`recordSummary`: "2 log records: 1 changed, 1 unchanged"), lists each
  statement as S0, S1… (the key the records' By column uses; the title names its
  editor line) with its counts and errors, and shows the records with the
  debugger's `RecordChanges`, fed the evaluation's diff, output and replay: each
  record by its content, its changed fields with before, after and the statement
  that changed them, plus the recorded output and raw JSON diff disclosures. The
  replay's notes are not shown: they ask for a rerun, which the playground does
  not have. It is a preview, never a test.
- **Examples and copy.** Logs, Traces and Metrics load a payload with
  statements; replacing text the user wrote asks inline first (`replacesWork`).
  "Copy processor" copies the processor block to paste under `processors:`. The
  draft is kept in `localStorage` (`od.playground`) as a convenience only.

## Input pane: one document, configuration and assertion builders

- **One document.** `web/src/workspace/InputPane.tsx` edits the configuration
  and one document, the scenario: its inputs, mocks, assertions and YAML. All
  five tabs are always shown (`web/src/workspace/editorTabs.ts`) so what is
  possible stays in view; `?editor=` names the tab (`input` and `form` open
  Inputs, `scenario` opens YAML). There is no mode switch: `runPlan`
  (`web/src/input/quickInput.ts`) runs a document with one inline input and no
  assertions as a quick run (`fixture_json`, `receiver`, `client_address`, and
  the document's mocks and settings as `setup_yaml` through `scenarioSetup`),
  and anything else as a scenario. The interface never names the two (see the
  [glossary](../debugger/glossary.md)): the document bar counts inputs and
  assertions ("1 input"), and the action is always "Run". Any input can also run
  on its own (`runFields(ws, i)`): inline data as a quick run with the
  document's mocks, a file input as a scenario of that input without assertions.
  `runBlocker` disables Run with the reason as its title.
- **Separate quick drafts.** Workspaces stored before the single document had a
  separate quick draft (`inputJSON`, `quickSetupYAML`, `runMode`, ...).
  `web/src/workspace/migrate.ts` folds it into the document on load: alone it
  becomes a one-input document (`quickDoc`); beside a scenario draft the input
  joins the scenario first and its setup keys fill only what the scenario lacks.
- **Replacing drafts.** Opening a configuration or scenario file, a
  configuration template or "Empty configuration" goes through `useReplaceDraft`
  (`web/src/config/ConfigTools.tsx`), which asks inline before replacing unsaved
  edits through the shared `useReplaceDrafts`.
- **Configuration.** An empty configuration shows a start screen
  (`ConfigStart`): starting pipelines per signal (`CONFIG_TEMPLATES` in
  `web/src/config/templates.ts`, OTLP to `batch` to `debug`), a file, or writing
  it by hand. "+ Insert component" lists the distribution's components by kind
  (`COMPONENT_SNIPPETS`), each with a body that runs; the insert bar picks the
  pipelines it joins (only those of a signal it accepts). `insertComponent`
  edits the document through `yaml` nodes, so comments and formatting survive:
  processors go before `batch` (`memory_limiter` first), receivers and exporters
  join the chosen pipelines, a second instance is named `type/2`, a missing
  section is added in the usual order with one blank line around it, and
  connectors are exported from the chosen pipeline into new output pipelines (a
  router or forward takes over the exports, a counter adds a metrics pipeline
  with the same exporters). Every template and every snippet inserted into the
  all-signals starter and a sample was checked with `linea inspect`. The editor
  reveals the inserted lines.
- **Configuration status.** The line under the editor (`configStatus`,
  `debugger/configStatus.ts`, tested) counts the problems found as the user
  types; for a valid configuration it says what it declares and, from the
  inspection's `runtime`, which runtime checked it: "3 pipelines · 8 components
  · checked on Built-in · Collector 0.162.0". It names nothing when the
  inspection carries no runtime.
- **One source of truth.** The Inputs, Assertions and Mocks forms and the YAML
  tab edit the same scenario text. `web/src/scenario/scenarioDoc.ts` reads a
  model from the parsed document and writes each edit back through it
  (`parseDocument`, node edits, `toString`); `useDocEdit` applies edits to the
  text as it is when they run, so buffered editors can commit late. Comments,
  key order and untouched formatting survive. Text that does not parse makes the
  forms read-only until the YAML tab fixes it. Shared form pieces (`Section`,
  `CommitInput`, `TextBlock`, `Menu`) are in `web/src/scenario/fields.tsx`.
- **Topology refs are not IDs.**
  `Graph.pipelines[].receivers/processors/ exporters` hold component refs
  (`exporter/otlp/pager`); scenarios use IDs (`otlp/pager`). Resolve them
  through `Graph.components` (`ref` to `id`, filtered by `kind`, because
  connectors appear among receivers and exporters). Target choices are each
  exporter per signal and each processor instance (processor in pipeline).
- **Inputs** (`web/src/input/InputsPanel.tsx`). Cards, one per input, each with
  its receiver (only those that feed its signal), a one-line summary of its
  data, and run, duplicate, move and remove actions. An input starts from a
  payload template (`web/src/input/templates.ts`: application logs, Kubernetes
  container logs, HTTP and failed request traces, service metrics, with
  timestamps near now; the receiver's signals first), an OTLP JSON file or
  pasted JSON, inlined as a literal block (`data: |`) because a browser cannot
  reveal a local file's path, or a file path the server reads. A scenario opened
  in the browser has no directory, so relative input files do not resolve (the
  server preloads no scenario). Waits between inputs (`after`) and a client
  address for k8sattributes are per input; the document's name and execution
  settings (`settle`, timestamps) are in Run settings.
- **Assertions** (`web/src/scenario/AssertionBuilder.tsx`, logic in
  `checks.ts`). Each assertion is a card: a name (empty uses the sentence
  `describeAssertion` writes), a target (exporters, then processor instances)
  and rows of checks: record count, records dropped (processor targets only),
  and attribute or resource attribute is set, is removed, equals a typed value
  (string, int, double or bool, written so the Go decoder reads the same type)
  or has a type. Rows are read from and written back to the condition keys
  (`readChecks`, `setChecks`); a row writes only when complete (`checkProblem`)
  and contradictory rows are flagged (`conflicts`). Conditions with comments or
  shapes rows cannot show open as YAML, and any card can switch to YAML.
  Templates (count at an exporter, attribute removed, attribute equals,
  processor drops records) start a card; "Suggest from last run" proposes the
  counts each captured exporter received and the drops of each processor that
  dropped records in the selected done run (drops only when it completed), for
  targets the configuration still has. Attribute keys are suggested from the
  inputs. "Check against run" posts the scenario to
  `POST /api/v1/runs/{id}/assertions` and shows each result on its card, hidden
  once the text changes. A `scenario_ignored` diagnostic about an assertion
  shows on this tab instead of Mocks.
- **Mocks** (`web/src/mocks/MocksPanel.tsx`). Always available: they live in the
  document and a quick run sends them as `setup_yaml`. One "+ Mock" menu adds
  every kind: first what this configuration reads (an example file per filelog
  receiver, "Pods for the input" per k8sattributes processor, the missing
  `${env:...}` variables), then file templates (log, CRI container log, JSON
  log, empty), Kubernetes templates (Pod, Namespace, Node, ReplicaSet), pasted
  or opened manifests and a variable. The empty panel shows the same choices, or
  says the configuration runs without mocks when nothing reads them. A group
  (Files, Kubernetes objects, Environment variables) appears once it has
  entries, says which components read it, or is marked "not read".
  - Files are cards: deployment path (committed on blur), content written while
    the Collector runs and optional existing content (from the card menu), as
    literal blocks. An example file has a path that the include patterns match
    and the exclude patterns do not, in the format the operators parse (CRI
    lines for the `container` operator, JSON for `json_parser`).
  - Kubernetes objects are cards that open one at a time to a form: name,
    namespace, UID, and for a Pod its IP (`status.podIP` and the first
    `status.podIPs` entry) and node; labels and annotations as key = value rows,
    with one-click chips for the keys the processor's `extract` rules read from
    that kind. "Edit as YAML" edits one object; a `List` or non-mapping labels
    are YAML only. Duplicate gives the copy its own name, UID and IP. Form edits
    change only their manifest paths (`mocks/objects.ts`, tested). "Pods for the
    input" builds a Pod per pod identity in the inputs (`k8s.pod.name`,
    `k8s.pod.uid`, `k8s.pod.ip`, `k8s.namespace.name`), with the labels and
    annotations the `extract` rules read, a ReplicaSet owner and
    `pod-template-hash` label when deployment or replicaset metadata is
    extracted, and the Namespace or Node when their labels are. Without an
    identity it uses the client address association and sets an address on
    inputs that lack one. Manifests can also be pasted or opened
    (`kubectl get -o yaml` output, several documents, `List` items expanded);
    each object needs a `kind` and `metadata.name`. `mocks.kubernetes.files` are
    listed but never read: the server reads no host file, so the manifests
    must be inlined.
  - Environment: the `${env:NAME}` references in the configuration that the
    setup does not define are listed with "+ Add missing"; values are masked
    until "show values" is ticked, and unused variables are marked. The
    workspace draft, setup text included, persists in localStorage.
  - "Handled automatically" lists simulated receivers and captured exporters,
    which need no mocks. Suggestions are pure functions in
    `web/src/mocks/suggest.ts`, driven by the inspection's redacted effective
    config (`effective.receivers[id]`, `effective.processors[id]`). Removing the
    last entry also removes the emptied `mocks` mappings.

## Samples and validation

- **Samples.** `samples/` (Go package `samples`) embeds `index.yaml` and,
  per sample, `config.yaml`, `input.json` and `scenario.yaml`.
  `samples.List` serves them through `GET /api/v1/samples`. It rewrites
  each scenario's `file:` inputs to literal-block `data:` with yaml.v3 node
  edits, keeping comments, because a browser has no directory to resolve
  them against. Mock fixtures in samples are written inline
  (`mocks.files[].content`, `mocks.kubernetes.objects`) for the same reason,
  and a sample whose scenario reads only mocked files may have no inputs
  (`container-logs`). On disk the files stay plain, so the CLI runs them as
  they are (`linea test --config samples/<id>/config.yaml
  --scenario samples/<id>/scenario.yaml`). `TestSamplesRunAsDocumented`
  runs every sample as a scenario and as a quick run. It checks the
  expected outcome (`expect: fail` marks a sample with a bug to find) and
  that all OTTL replays are `replay_verified`.
- **Picking one.** The empty workspace lists the samples in the graph
  placeholder, and the top bar has a Samples menu. Loading a sample replaces
  the configuration and the document with its scenario, asking first if
  they hold other content, so its assertions, and a bug's failing one,
  show up.
- **Validation** sends the document with the configuration, so
  `${env:NAME}` values and mocks apply; invalid document text is reported
  as `scenario_ignored` and the configuration is still validated.

## Interface rules and file map

Rules for the web client, kept here from `AGENTS.md`. Follow
them when changing a view; change them only deliberately. User-facing
words follow the [glossary](../debugger/glossary.md).

Never show these older words in the interface:

| Don't show | Show instead |
| --- | --- |
| Draft, scenario draft | The configuration, the scenario, plus their save state |
| Quick run, scenario run | Run |
| Turn into test | Add assertions |
| Use as scenario draft, Use and save | Add assertions, then Save |
| Open as draft, Open sources as draft | Open; Open the run's configuration and scenario |
| Export scenario file, Download scenario, Download configuration | Export for CI, the one download |
| Resume current draft | Continue where you left off |
| Run input, Run scenario | Run |

`web/` is the web client (React, TypeScript, Vite, TanStack Router and Query,
Monaco), managed with pnpm and pinned by `web/pnpm-lock.yaml`.
`web/src/workspace/drafts.ts` (tested) defines dirty drafts against their
loaded/saved baselines and turns run inputs or saved revisions into workspace
patches; `runs/RunInputs.tsx` opens a run's pinned inputs as drafts, creates a
scenario from the run in the scenario draft, and `workspace/SavedSources.tsx`
lists, opens and saves saved sources (nothing without an artifact repository).
Never replace a dirty draft without an inline confirmation. `GET /runs` returns
a page, `{runs, next_cursor}`, not an array. `runs/runList.ts` polls the run
list every 5 s while the page is visible: `runListQuery` reads the newest page
for the drawers, pickers, comparisons, start view and header counts
(`runs/runPages.ts`, tested: `firstPageRuns`, and `runCountText`, which marks a
count as `50+` while older runs follow), and `runPagesQuery` is the run list
page's infinite query. Every key starts with `['runs']`, so one invalidation
refreshes them all. The run list (`/runs`, `debugger/RunListPage.tsx`) and the
Investigate actions delete a finished run after an inline confirmation
(`useDeleteRun`, wording from `runs/deletion.ts`, tested), and a deleted run's
page says it was deleted. Suites are listed (`/saved/suites`, `GET /suites`) but
cannot be deleted from the web. Run retention: a run's header has a Keep toggle
(`debugger/KeepButton.tsx`, wording and error text in `runs/keep.ts`, tested)
that shows `run.kept` and calls `PUT`/`DELETE /runs/{id}/keep`; the returned
mark goes into the cached `['run', id]` (the response has no report, so only
`kept` is merged, `withKept`) and the run lists are invalidated. Its tooltip
says "Kept runs are never removed by retention or prune." Every PUT carries
`Content-Type: application/json` and an empty `{}` body (`call` in
`api/client.ts` does this for POST, PUT and PATCH). `/runs/storage`
(`debugger/RunStoragePage.tsx`, view model `debugger/runStorage.ts`, tested) is
the run history's storage page, reached from "Storage" in the All runs header
and from the runtimes' Storage page, which measures Docker's disk instead. It
shows `GET /storage` (bytes used against the limit in an `od-meter`, runs, kept
runs, oldest run, the retention policy in words) and the prune form: admitted
before (a date, the start of that day in the browser's time zone), result (the
run list's finished results, `PRUNE_RESULTS`), scenario, runtime and an optional
project. "Preview" sends a `dry_run` and states "N runs, X freed"; "Prune" is
enabled only while the form still holds the previewed values and the preview
found runs (`canPrune`), and asks inline ("Cancel" or "Delete N runs", never
"Keep", which here means the kept mark). A finished prune clears the preview,
refreshes the usage and the run lists. Admin is not in the session's permission
list, so the form is disabled for a role without `run.delete` (which an admin
always holds) and a 403 reads "Only an admin may prune runs." The run overview
lists stand-in components, components with stand-in values and unavailable
references with their remedy in an open "Stand-ins" section (`runs/standins.ts`,
tested), never in a collapsed disclosure; the pipeline graph marks `stand_in`
nodes. Simulated endpoints are listed in an open "Simulated endpoints" section
too. The graph shows every configured extension in a row above the pipelines,
each with its execution mode (`laneModel`'s `extensions: 'running'` option shows
only running ones); an extension opens on "Activity" (`ExtensionActivity.tsx`:
its endpoints, lifecycle and log entries paged by 25, its metrics) and "Data"
(what its endpoints received), view model `inspect/extensions.ts` (tested). Its
Runtime row names the report's runtime and states the optional analyses it lacks
(`missing_features`, `runtimes/runtimes.ts`) and its module providers
(`providerLines`); an endpoint a provider served says so (`endpointText`). The
runtime list names providers too. `debugger/ComparisonView.tsx` is the
whole-page "Compare runs" state (`/?run=<a>&compare=<b>`). It is reached from
the runs drawer, the run list or "Compare with…". The earlier admitted run is
the baseline (list order, since a pair shares its creation time). It reads the
newest page of the run list and reads a compared run older than that page on its
own (`runs/comparedRuns.ts`, merged newest first by `withRuns`), so only a
deleted or unreadable run is reported as not in the list. It shows the platform
comparison: eligibility and every inconclusive reason in the open, exporter
outputs, and one row per changed injected record that opens to where the record
ended, "Follow in both" and its fields per exporter. Follow in both lines up the
platform's path of the record in each run (`runs/followBoth.ts`, tested), marks
the step `diverges_at` names, and links to follow the record in either run
(`/?run=<id>&trace=<lineage_id>`). When the two runs' recorded inputs differ (by
SHA-256, `runs/inputDiff.ts`, tested), an "Input differences" card names them
and "Configuration diff" shows each as a unified diff, read from
`GET /runs/{id}/inputs`. Its view model, `runs/compare.ts`, is pure and tested.
"Compare with…" (`CompareMenu`) also runs a finished run's input with another
configuration, the open one or one of the five newest saved ones, beside the
run's own recorded configuration: one `POST /comparisons` on the run's runtime
with the scenario derived from the run (`runs/configCompare.ts`, tested), then
the comparison of the two new runs, which share an input and so are comparable.
An old `?tab=compare` link still opens `runs/ComparePanel.tsx` in the
Investigate pane. Do not add a comparison mode to the Changes tab.
`web/src/runtimes/` lists, builds, pushes and imports runtimes and checks
upgrades. A build is a job queued on the team server: `RuntimeForm.tsx`
(component picker `CatalogPicker.tsx`, `catalogPicks.ts`; manifest text;
`retarget` through "Move to <release>") queues it over `/api/v1/runtime-builds`,
builders compile it natively and the binary is registered under the build's name
for the pool workers. The client never builds anything itself and never reads
the user's disk except through the folder a user picks or drops.
`RuntimeSelect.tsx` beside Run is the only place a runtime is chosen (workspace
`runtime`, sent by runs, validation, OTTL previews and candidates). The empty
value sends no runtime, so the team's default answers: its option reads "Default
· <name> · Collector <version>" (`defaultRuntimeQuery`,
`GET /api/v1/runtime-default`; just "Default" while unknown), and when the
default is not built-in an explicit "Built-in · Collector <version>" option
(value `built-in`, never missing) keeps built-in choosable. `SuiteLauncher`
shares `runtimeOptions`; the providers and mocks wording resolve the empty value
to the default (`effectiveRuntime`). An unverified runtime, the default
included, shows its badge while selected, and a selected runtime that is gone
stays listed as not found. The Runtimes destination (`runtimes/pages/`) never
chooses the runtime of the workspace, with two exceptions: "Run on <name>" on a
finished build's page selects the new runtime and opens the debugger, and "Make
default" sets the team's default (below), which leaves the workspace selection
as it is.

- `/runtimes` (`RuntimesPage.tsx`, view model `runtimeList.ts`) lists built,
  imported and catalog runtimes with the selected one beside the list. A team
  server lists the built-in runtime too, served by its workers; that entry is
  the built-in row (its workers chip and Workers fact), never a second row, and
  `runtimeOptions` likewise offers built-in once. It
  pushes, imports and removes after an inline confirmation. "Build a runtime"
  opens the builder card (`RuntimeForm`) and "Import image" the import card;
  the builder card also shows the CLI line (`CLI_STEPS`:
  `linea runtime build --server URL ...`). A runtime a build produced is a
  catalog runtime with a binary: its type reads "Built runtime" and its
  Binary fact gives size, digest and platform. When a pool worker found the
  binary wrong, its chip reads "Suspect binary" and a Suspect fact names the
  pool, the time and the cause. Other catalog runtimes are
  registered with `linea runtime register` and confirmed by their workers'
  hello, so the page only removes them and has no connect or register form.
  Below the list, a Builds card lists the newest builds (the caller's own;
  `runtime.manage` sees all), ten at a time with "Show older builds"
  (`runtime-builds` cursor), each a link to its build page with a state chip;
  it is read every 3 s while one has not finished.
  - The team's default runtime: the row that is the default
    (built-in when the server says so, a catalog runtime by `default` or
    name; `isDefault` in `runtimeList.ts`) carries a "Default" chip, and
    its detail pane a Default fact: "Requests that name no runtime run on it",
    then "· chosen by <set_by> <when>" or, while no one chose, "until the team
    chooses another". "Make default" appears on the built-in row and on
    catalog rows that are not the default, never on a local container (the
    server accepts only `built-in` and catalog names). It asks inline ("Make
    <name> the default runtime? Every run, validation and preview that names
    no runtime runs on it, for the whole team.", Cancel / Confirm), calls
    `PUT /api/v1/runtime-default`, shows the server's refusal in the pane and
    refreshes `runtime-default` and `runtimes` on success. It needs
    `runtime.manage` and stays visible but disabled with the reason without
    it. Remove on the default is disabled with "This is the default runtime;
    make another runtime the default first." (the server answers 409).
  - Your own components: the server reads nothing from the user's machine and
    refuses `dir`, `git` and module-proxy settings. A module the manifest
    builds from a local directory (`path:` entry, or a replace to a
    directory; `buildSources.ts`) gets a folder slot (directory picker or a
    dropped folder; `readFolder.ts`). The folder is checked as it arrives
    (safe relative paths, 20000 files and 64 MiB per module, 64 MiB for the
    build, at most 32 modules, a root `go.mod` declaring that module) and
    packed in the browser by `tar.ts`: an uncompressed ustar of directories
    and regular files, sorted, with fixed modes and no owner or time, so an
    unchanged folder packs to the same bytes and a build of it can be reused.
    `.git`, `.hg`, `.svn` and `node_modules` are left out, as the CLI does.
    Browsers show a symbolic link as the file or folder it points to. A build
    without local modules is a plain JSON `POST`; one with them is a
    multipart `POST` (a `request` part and one `sourceN` tar part per module)
    with the `X-Linea-Upload` header the server requires as its CSRF guard
    (`api.uploadRuntimeBuild`). The page says that custom component code runs
    on the pool workers (`TRUST_NOTE`).
  - A custom component is added to the manifest with a `path:` entry (the
    form shows a snippet) and then gets its folder slot.
  - Rebuild: a build's GET returns its manifest as sent (`manifest_yaml`;
    the list and the event stream do not), never its sources. The form is
    remembered per browser (`buildMemory.ts`, localStorage, the 20 newest,
    bounded; name, manifest and release), the picked folders only for the
    page session (memory, the 5 newest). `/runtimes?rebuild=<id>` opens the
    builder seeded from that build; for a build another browser started it
    seeds the name, release and the server's manifest and asks for the
    folders again (a server without `manifest_yaml` leaves the manifest to
    be written).
    If the form holds a manifest or folders, the seed waits behind an inline
    "Replace the runtime you are writing" confirmation (`REPLACE_FORM`).
- `/runtimes/builds/<id>` (`BuildPage.tsx`, view model `buildView.ts`, stream
  logic `buildFollow.ts` and `useBuildLog.ts`, tested) follows one build:
  state chip, elapsed time, the log with line numbers and the manifest as
  sent (this browser's copy, else the read's `manifest_yaml`, which
  `follow` keeps through the events). The build facts and the log share the
  first row; the manifest spans the full width below it. The build is read once
  (`GET ...?log_from=N`), then followed over server-sent events (`EventSource`
  on `/events?log_from=N`, as
  runs are): a `build` event carries the new log lines only and the event id
  is the next line, so a reconnect resumes without repeats; the `end` event
  closes the stream and refreshes the runtime and build lists. If the
  browser gives up on the stream, the build is read again (it may be gone) and
  the stream reopened from the first unread line. The server keeps the last
  2000 log lines; earlier ones are announced as not kept. Leaving closes the
  stream and never cancels the build. A finished build is worded by outcome
  (a reused binary says nothing was compiled); "Run on <name>" selects that
  runtime in the workspace (the open draft stays as it is) and opens the
  debugger, or the origin when `from` names one; Rebuild opens the builder as
  above. Cancel and Rebuild need `runtime.build` and stay visible but disabled
  with the reason for a viewer.
  The detail pane lists the runtime's components by kind
  (`RuntimeComponents.tsx`, `runtimeComponents`): its manifest's entries,
  named by the release's catalog when the release is verified, otherwise by
  package; entries the catalog lacks are marked custom. The built-in runtime's
  list comes from the `built-in` preset manifest. A runtime registered with
  `linea runtime register` is listed without a manifest and shows no list.
- `/runtimes/upgrade` (`UpgradeCheckPage.tsx`, `upgradeMachine.ts`,
  `upgradeView.ts`, tested) checks the configuration and scenario drafts from
  one runtime to another. Its pickers name the check's sides only. The check
  is one request bound to the page: while it runs, a banner says so and
  leaving asks first (`useBlocker`, inline) and cancels it.
- `/fleet/bisect` (`fleet/pages/ConfigBisectPage.tsx`, view model
  `fleet/bisect.ts`, tested) finds the fleet config version that
  made the open scenario fail on an agent, on the runtime chosen beside Run.
  It reuses the upgrade check's request machine (`upgradeMachine.ts`, generic
  over the result and subject): one request bound to the page, a banner while
  it runs, and leaving asks first and cancels it. The result words the
  verdict, shows the culprit's diff in the shared `.unified` styles with
  "Open comparison" links, and lists the versions newest first with their
  runs and rollouts. Fleet's agent pane and a config's page link to it.
- `/runtimes/storage` (`StoragePage.tsx`, `storage.ts`) measures disk
  space on request and prunes behind an inline confirmation
  (`PRUNE_CACHES_WARNING` for caches). An older image that only a pushed
  reference keeps is listed as tagged, without actions.
- A page opened from the debugger carries `from`, the in-app location to
  return to (`returnPath.ts`, which accepts only same-app paths). It offers
  the way back to the experiment.

RuntimeSelect's "Runtimes…" is a link to `/runtimes` carrying `from`, the
current in-app location (`useLocation`), so the Runtimes destination can offer
the way back. Do not add a second
runtime picker or hide "unverified".
`web/src/api/schema.d.ts` is generated from `internal/debugger/api/openapi.json` by
`pnpm gen:api` and committed. Pure logic (layout, diagnostics placement,
annotations, diffs, scenario YAML edits, quick-input run modes)
lives in plain `.ts` modules with Vitest tests beside them (`*.test.ts`);
this includes grouped changes, record journeys, routing matrices and
aggregation tables.

`web/src/inspect/SignalExplorer.tsx` is the shared read-only signal viewer for
quick-input previews and recorded receiver, processor, connector and exporter
snapshots. Its plain TypeScript adapter (`signals.ts`) preserves analysis entity
IDs and resource/scope context, decodes OTLP values, and formats timestamps
without losing nanosecond precision. Keep presentation changes in the client;
the recorded payloads remain the source of truth.
Viewer interaction tests use Vitest's per-file `jsdom` environment in
`SignalExplorer.test.ts`; the remaining pure-logic tests run in Node.

The Changes tab uses `RecordChanges.tsx`, built from the shared record-view
parts: `RecordList.tsx` (filter pills with counts, one open row at a time,
pagination, external focus), `RecordRow.tsx` (the row, whose whole surface is
the only click target) and `FieldTable.tsx` (fields grouped by Resource, Scope
and Record, each with its OTTL path, value type and Before/After values).
`recordViews.ts` (tested) is the pure view model over the diff and the recorded
payloads. The Decisions, Aggregation and Routing tabs reuse the same list and
row rather than adding another layout. A row states the answer in one sentence
and must not repeat the diff ("Changed in resource and record", not the edited
names); the opened panel holds the evidence. Lists of recorded records (Changes)
have column headings (`columns` on `RecordList`, labels from `recordColumns`):
the badge is a cell under "Severity", "Status" or "Type" by signal, and service
is its own column; a badge never appears without its heading. Only changed
fields show by default; "Show all N fields" reveals the whole record. Do not add
alternate comparison modes, summary cards or toolbars beyond the filter pills.
Keep shared resource/scope edits in their own level (with how many records share
them), distinct from record edits and added fields distinct from new records.
Dropped refs index the input; created refs index the output. Preserve types,
empty strings and match confidence (only medium-confidence matches are called
out). Matching totals include explicitly added/removed records; only the
remainder is uncertain. Never invent counterparts for unmatched or buffered
records, and never claim which statement dropped a record: the replay counts
statements per input, not per record. A transform adds a "By" column and a
statements key from the OTTL replay; editing stays in the Statements tab
(`OTTLPanel`, whose edits are server candidates; `candidates.ts` words their
failures), which the Changes view links to. A buffered (deferred) processor
input, as with batch or tail_sampling, shows the stage's reconciliation instead:
all buffered inputs against all later emissions, concatenated in event order by
`deferred.ts` (tested), whose refs map back to their own events for following
records. Without a diff, both snapshots are listed as recorded records with no
claims about changes. Large change sets and snapshots paginate: every view over
records, snapshot nodes or events pages (25, `inspect/pages.ts`), virtualizes or
caps what it renders. The overview helpers (`GroupedChanges.tsx`,
`ChangesOverview.tsx`, `changeOverview.ts`) remain tested but are not the
primary UI; `grouped.ts` and `structuredDiff.ts` still supply `refEntity`,
`diffValue` and `otlpValue`. Component interaction tests use `jsdom`
(`RecordChanges.test.ts`); `recordViews.test.ts` is pure logic.

Keep one secondary `RawJSONDiff.tsx` disclosure beside recorded Before/After
comparisons, including processor Statements and the selected connector route.
It compares complete snapshots, including metadata, and mounts its read-only
Monaco editor only when opened. Missing snapshots are explained, never replaced
with empty objects or presented as deletions. `DiffViews.tsx` detaches diff
models before disposing them; preserve the lifecycle regression test.
Follow record actions resolve actual lineage by event sequence and entity ID:
dropped records use the input, new records use the output, and metric points
remain individually addressable. Returning from a journey selects the right
page and highlights the record without hiding other changes. Allow externally
focused readable data to return to all records without adding another picker.

`web/src/workspace/WorkspacePanes.tsx` owns the resizable input, pipeline and
information panes. Only split ratios are persisted (`linea.panes.v1`; another
page using the panes passes its own `storageKey` and `defaults`); keep pointer
and keyboard resizing, small-viewport bounds and unavailable-storage behavior
covered by its tests and `panes.test.ts`. The panes are the left pane (inputs,
or the run's whole-run views), the graph and the bottom pane (the selected
component, or the run's progress). The product UI dropped pane expand and
restore. `inspectorTabs.ts` defines the information architecture. The whole-run
Overview, Records (record journeys) and Timeline (`INVESTIGATE_TABS`) live in
the Investigate pane (`debugger/InvestigatePane.tsx`). A failed run, or one
whose execution ended partial or failed, adds a Collector log disclosure to the
Overview (`RunLogPanel.tsx`; `keepsRunLog` and `runLogView` in `runLog.ts`,
tested) from `GET /runs/{id}/log`. It opens when the log has lines and shows the
server's redacted text unchanged. A done run's Overview lists the assertion
checks recorded beside it ("Recorded checks", `RecordedChecks.tsx`,
`debugger/evaluations.ts`, tested; `GET /runs/{id}/evaluations`), such as an
agent's `linea_evaluate_assertions`, each opening to its results; the web's own
checks are not recorded, and nothing shows when there are none. Component views
live in the bottom pane (`debugger/ComponentPane.tsx`; before a run,
`PREPARE_TABS` explains how the component will run and shows its configuration).
Compare runs is its own state. Processors open directly on Changes, with
optional Statements for transform; tail_sampling opens on Decisions
(`SamplingPanel.tsx`, a trace-per-row record list whose opened row is the ladder
of policies; logic in `sampling.ts`) and a Configuration tab; extensions open on
Activity, then Data; receiver/exporter/connector views retain Data, with Routing
(`RoutingList.tsx`, logic in `routing.ts`: a row per record that says which rule
routed it and opens to the rules in table order, only when the replay is
verified; otherwise it shows the deliveries and says why) or Aggregation
(`AggregationList.tsx`: a row per data point that opens to the records it
counted, plus a row per record not counted) where applicable. A single Run
overview link provides run context. Old processor telemetry bookmarks resolve to
Changes. Avoid duplicate data / overview tabs and competing navigation controls;
overview disclosures must not conceal critical errors, warnings or failed
checks. The pane, overview, telemetry and journey interaction tests use per-file
`jsdom`.

`web/src/workspace/InputPane.tsx` edits the configuration and one document
(the scenario: inputs, mocks, assertions, YAML); all five tabs are always
shown (`editorTabs.ts`, tested), and Mocks is always available. There is no
mode switch: `runPlan` (`input/quickInput.ts`) runs a document with one
inline input and no assertions as a quick run (its mocks and settings sent
as `setup_yaml`), anything else as a scenario, and any input can run alone.
`workspace/migrate.ts` (tested) folds separate quick drafts into the
document. `input/InputsPanel.tsx` edits inputs as cards started from payload
templates (`input/templates.ts`), files or pasted JSON.
`config/ConfigTools.tsx` holds the configuration start screen, templates
and "+ Insert component"; `config/templates.ts` (tested) inserts a component
snippet into the chosen pipelines through `yaml` nodes, keeping comments, and
never edits a configuration except on the user's request. Opening or
starting a draft goes through `useReplaceDraft`, which confirms inline
before replacing unsaved edits. Every replacement of drafts, from any view,
uses `workspace/useReplaceDrafts.tsx`: `request(drafts, what, apply)`
applies at once when none of the named drafts is dirty and otherwise renders
one confirmation (`confirm`, compact in menus) whose default is keeping the
draft. Do not write a second confirmation. `scenario/AssertionBuilder.tsx` edits
assertions as a target plus rows of checks, from templates or "Suggest from
last run" (counts and drops the selected run observed); the endpoints the
selected run simulated are targets too (`targetChoices(graph, report)`);
its logic is in
`scenario/checks.ts` (tested), and the YAML stays the escape hatch for
shapes rows cannot show. Forms write through `scenario/useDocEdit.ts`.
`web/src/mocks/` holds the Mocks panel (one "+ Mock" menu, file and
Kubernetes object cards, objects edited in a form or as YAML), `suggest.ts`,
pure and tested, which proposes files, Pods and environment values from the
effective config and the input, and `objects.ts` (tested): object form
edits at their manifest paths and the file and object templates. Its
"Module providers" group edits `mocks.providers.<name>` as YAML, one card
per provider (`setProviderSettings`, `removeProvider` in
`scenario/scenarioDoc.ts`), offers the selected runtime's providers
(`runtimeProviders` in `runtimes/runtimes.ts`), shows what a provider's
check found missing and says when the runtime lacks a provider;
a check's proposed settings (diagnostic `provider_settings`, the SDK's
`Finding.Settings`) are added in one click, keys the scenario lacks only
(`addProviderSettings`, `proposals`); `providers.ts` (tested) is its view
model; the components a provider drives are those whose topology
component names it (`provider`).


### Debugger states

`debugger/Debugger.tsx` is the `/` route. `debugger/state.ts` (tested)
derives one of five states from the URL and the drafts: start, prepare,
running, investigate and compare.

- A run in the URL is always investigate, or running or compare, even while it
  loads or after it is gone.
- `?start=true` reopens the start state over a loaded draft. Without it, a
  configuration means prepare.
- `StartView.tsx` (logic `start.ts`) offers the starting actions, the bundled
  examples and, for returning users, what they left open, their runs, test
  results and saved work. Each list fails on its own. **New** picks a
  configuration template (`config/templates.ts`) and opens it with an empty
  scenario, both clean and saved nowhere (`newPatch` in `workspace/drafts.ts`).
  Replacing unsaved work from the start state asks through `useReplaceDrafts`.
  "Continue where you left off" shows the save state of the configuration and
  the scenario, when they were edited, and their own last run with its evidence:
  the workspace store keeps `editedAt` (set when the configuration or scenario
  text changes) and `lastRunId` (set when a run starts), in this browser only.
  Runtime builds that are queued or running show there too (`activeBuilds`), and
  a failed build is listed under "Needs attention" (`attention`); both link to
  the build page.
- The header (`WorkbenchHead.tsx`) names the open configuration and scenario
  with their save states (`saveState` in `headModel.ts`, shown by
  `SaveIdent.tsx`): "Not saved" until saved in saved work (a file or an example
  is not), "Unsaved changes" once edited since, otherwise "Saved · revision N"
  (or "earlier revision"). It holds Start page, Save (`SaveMenu.tsx`: revisions
  and Export for CI, never the original files; there are no single-file
  downloads). Each Save section has a Name field, prefilled with the saved
  name, else the scenario's `name:`, else the file name (`newLabel` in
  `workspace/drafts.ts`); a revision saved under a changed name renames the
  saved work, since a save's label replaces the artifact's. Every saved name
  ends in `.yaml` or `.yml`: `yamlName` appends `.yaml` to any other (so
  `config.bin` saves as `config.bin.yaml`), and the field says "Saves as …"
  when it does. The platform applies the same rule to every save
  (`yamlLabel`, [artifact-storage.md](artifact-storage.md)), so the two must
  match. Then come the
  readiness chip, the reference comparison toggle, the runtime
  picker and Run or Cancel.
- Export for CI (`ExportForCI.tsx`, `openExportPlan` in `saved/ciArchive.ts`)
  exports what is open: it first saves whichever of the configuration and
  scenario is not saved or has changed, then downloads `collector-ci.zip` of
  their head revisions and shows the command the workflow runs. It refuses an
  earlier revision, since the archive would hold the newest one.
- The runs drawer (`RunsDrawer.tsx`) lists the newest ten runs, and `/runs`
  lists all of them. Both read each run's facts from the list's `summary`
  (`runFacts.ts` falls back to it without a report), so neither reads a run per
  row. `/runs` pages: `useInfiniteQuery` over `runPagesQuery` follows
  `next_cursor` (default page of 50), loading the next page when a sentinel
  after the table scrolls into view (`IntersectionObserver`) or when the visible
  "Load more" button is used. Duplicates between pages (a run that shifted while
  new ones arrived) are dropped by id, first occurrence kept
  (`flattenRunPages`). A poll re-reads every loaded page from the first, so
  deleted and finished runs stay right; it stops once more than four pages are
  loaded (`RUN_LIST_POLL_MAX_PAGES`), since a cursor is only valid with the
  filters it came from and a reader that deep is reading history. The Scenario,
  Runtime and Result filters go to the server and are part of the query key, so
  changing one restarts at the first page; the search and Mode apply in the
  browser to the pages loaded so far. All five live in the URL
  (`/runs?scenario=&runtime=&result=&mode=&q=`, `debugger/runListSearch.ts`,
  tested), replacing the history entry as they change. The filter options come
  from the newest page of the unfiltered list, and the run count reads "50+
  runs" while older ones follow.
- "Add assertions" (`AddAssertions.tsx`, `testDraft.ts`) adds assertions from
  what a finished run observed to the scenario the run used, or to a new
  scenario made from a run without one (`testBase`), which becomes the open
  scenario. A suggested assertion is added only when confirmed. It neither saves
  nor exports: Save does both.

Capture-first handoff: a capture page used without a loaded configuration
stores the chosen streams in `sessionStorage` (`capture/handoff.ts`, key
`linea.capture-handoff.v1`, one hour). It holds IDs and sequence
numbers only, never record contents. The Debugger takes the handoff once on
load (`CaptureHandoff.tsx`) and shows a bar that survives the start state.
Once a configuration is open, "Add as inputs" opens the capture page's "Use
records" drawer with the destination fixed to the open scenario.

### Team server sessions and permissions

On a team server (`linea-server --config`, the only server mode,
[team-server.md](../debugger/team-server.md)), `AppShell` reads
`GET /api/v1/session` (`ui/session.ts`, `sessionQuery`).
- Pages render only once the session is known, so a page's first request that
  changes something (the playground evaluates on load) carries the CSRF token.
- A 401 replaces the whole page with a sign-in page. It links to
  `/auth/login?return_to=<location>` (`signInHref` in `ui/access.ts`, tested).
  It does so whatever the server's sign-in methods are; `/auth/login` exists
  only with `oidc`.
- The session's CSRF token is kept in `api/client.ts` (`setCSRFToken`). Every
  request other than GET sends it as `X-CSRF-Token`; only a web session has a
  token.
- The settings menu names the principal and, for a web session, offers Sign out
  (`POST /auth/logout`).

The session lists the caller's permissions. Only the shell fetches
it; views read it from the cache (`useDenial`). While the list is unknown,
everything is offered, and the server checks every request either way.

Role-based visibility:

- An action the role lacks stays visible but disabled. Its title names the
  missing permission (`denial`). A link is swapped for a disabled button
  with the same look (`ui/Permitted.tsx`).
- A destination the role cannot use is hidden (`DESTINATION_PERMISSION`):
  the OTTL playground needs `run.submit`, the Audit log `audit.read`, Access
  `access.manage`.
- A principal with no permissions gets "No access yet" instead of the page.

Gated actions:
- `run.submit`: Run, Cancel, Run with my changes, Compare with…, run
  suite, rerun suite, From a capture…, Use as inputs (deriving inputs
  inspects the destination configuration).
- `run.delete`: deleting runs, Keep/Kept, and the run history's prune form
  (which the server further limits to admins and, for keep, to the run's
  owner).
- `artifact.write`: Save, Import.
- `capture.start`: New capture, Duplicate, Configure capture, Capture this
  group, Start and Check, Stop, Delete capture.
- `fleet.config.write`: group, config and assignment changes.
- `fleet.rollout`: Retry config, rollouts and their controls.
- `runtime.manage`: import, push, register and remove a runtime, make a
  runtime the team's default, share it with projects, clean up storage.
- `project.manage`: the Projects page's changes.
- `library.publish`: Publish and Share… on the Library page.
- `runtime.build` (engineer, admin): Build a runtime and Build, and on a
  build's page Cancel build and Rebuild. A viewer can still prepare a
  manifest in the form.

A new write action uses `useDenial` the same way. `ui/access.test.ts`
checks each built-in role. `ui/AppShell.test.ts` and
`fleet/pages/FleetPages.test.ts` render a viewer and an engineer.

`/audit` (`audit/AuditPage.tsx`, view model `audit/auditView.ts`, tested)
pages the audit log 50 events at a time, newest first. Principal, operation
and outcome are filtered on the server.

`/access` (`access/AccessPage.tsx`, view model `access/accessView.ts`, both
tested) lists `GET /role-bindings`: subject, role (its permissions in the
title), scope, a source chip and who added it when. Server config bindings have
no action. "Grant" adds a binding from the form (group or principal, a role
from the response's `roles`, everywhere or one project chosen from the
projects that are not archived, `grantableProjects`), checked by
`formProblem` before it is enabled. "Remove" asks inline ("Keep binding" /
"Remove binding"). Every change sends the revision the page loaded. A refusal
shows the server's reason without its code prefix (`refusalText`); a 409 also
reloads the list. The form is local state that only a successful grant clears,
so a reload never replaces what was typed. `/access?grant=<project>`
(`parseAccessSearch`) opens the form scoped to that project; it only sets the
form's first state.

`/access/projects` (`access/ProjectsPage.tsx`, tested; `ACCESS_NAV` links the
two Access pages) lists `GET /projects` with each project's settings
(`retentionText`). The form creates or edits one project; `projectForm`,
`projectFormProblem` and `projectRequest` (`ui/project.ts`, tested) convert
days to seconds and MiB to bytes. Editing another project while the form has
unsaved changes asks inline ("Keep editing" / "Discard changes"). Archive
asks inline; restore does not. Every change sends the project set's revision
the page loaded; a 409 reloads it. The page needs `project.manage`. For an
`access.manage` holder, "Who has access" lists each project's own bindings
and how many apply everywhere (`projectMembers`, `memberText`), and Grant
access links to `/access?grant=<id>`.

Projects:

- `api/client.ts` keeps the work project (`setWorkProject`) and adds it as
  `?project=` to every request, download and event stream URL
  (`withProject` in `ui/project.ts`). A path that already names a project,
  such as a copy into the library, keeps its own.
- `ui/ProjectMenu.tsx` is the rail's project switcher (`projectsQuery`,
  `useWorkProject`). It lists the projects the session holds a permission in
  (`workProjects`), and shows only when there is more than one. The choice is
  read from `?project=` in the URL, then the `linea.project` localStorage key
  (per-viewer; reads and writes are guarded), else `default`
  (`chooseProject`). Switching resets every query except `session` and
  `projects`, so no page shows another project's data.
- `/saved/library` (`saved/LibraryPage.tsx`) lists `GET /library`: the items
  shared with the work project, or for `library.publish` holders the whole
  library ("Show"). "Copy into project" copies an item
  (`POST /artifacts/{id}/copies`, needs `artifact.write`). A publisher
  publishes the project's saved configuration, fixture or scenario into the
  library and edits an item's audience ("Share…", `toggleShare`: `*` alone or
  project IDs). Only one audience is edited at a time; the others' Share…
  buttons are disabled meanwhile, so a choice is never replaced.
- The Runtimes page shows each runtime's projects (`RuntimeAvailability`,
  `availabilityText`) and, with `runtime.manage`, edits them
  (`runtimes/pages/RuntimeShare.tsx`), sending the set's revision.

### Saved work

`web/src/saved/` covers several pages:

- `/saved` (`SavedWorkPage.tsx`, `collection.ts`) is the searchable list of
  saved sources, with `kind` and `q` in the URL. Scenarios with assertions are
  tests. Selected scenarios run as a suite (`SuiteLauncher.tsx`,
  `suiteRequest.ts`).
- `/saved/suites` and `/saved/suites/<id>` list suites (`GET /suites`) and show
  one suite's result (`suiteView.ts`). Each failed or undecided assertion leads
  into its member run. A finished suite downloads as JUnit (`junitHref`) and
  reruns from its recorded inputs, opening the new suite; `rerunBlocker`
  disables Rerun while the suite runs or when a member was never admitted, and a
  server refusal shows inline.
- `/saved/library` is the library ([Projects](#team-server-sessions-and-permissions));
  `savedSection` maps it to the header's Library link.
- `/saved/<id>` (`SourceDetailPage.tsx`, `history.ts`) lists the revisions
  newest first from `GET /artifacts/{id}/revisions`, 20 per page, and reads only
  the selected revision's content.
- Export for CI and Import: the suite launcher (`SuiteLauncher.tsx`) downloads
  `collector-ci.zip` for the selected scenarios and a saved configuration
  (`ciExportBlocker` in `ciArchive.ts` refuses the unsaved open configuration),
  and the Saved work header's Import… uploads an archive and reports what it
  created, revised or left unchanged (`importSummary`).

A draft opened from a revision goes through `useOpenAsDraft`, which confirms
inline before replacing a dirty draft. Without an artifact repository the pages
say why (`NoWorkspace.tsx`).

### Collectors route

The collectors web route (`web/src/capture/`) is a fleet client: it lists
fleet's Collectors and groups and never manages credentials, groups or raw
permission, which belong to fleet. Without a fleet connection the Collectors
page explains the `linea-server --fleet-url --fleet-token-file` setup. The bounded
inventory page groups by cluster, namespace or Collector group, with counts and
collapsible groups; search and regrouping must preserve capture drafts.
`groupCapture.ts` resolves whole-group rosters across bounded inventory/point
pages before offering shared points. Group requests freeze 1–16 explicit target
IDs and use the existing shared admission operation; never truncate targets or
skip unavailable members. Dirty-draft replacement must restore both the request
and reviewed group context. `CaptureSelectionForm` maps point checkboxes,
duration and sampling to the shared selection; JSON is optional advanced
editing. A fleet group can be captured as a whole (`group_id`;
`fleetGroups.ts`). `CaptureMetadataEditor` edits capture details without
touching records; it refuses stale revisions and preserves unsaved edits on
conflicts. Raw points are offered with a "needs fleet permission" label; fleet
refuses a capture the debugger's credential may not take raw. `CaptureControls`
owns admission and automatically observed session history; `CaptureInputPicker`
is the shared native dialog reached from the debugger header, the empty Inputs
view and Inputs → Add input → From a capture. It embeds `CollectorsPage` in
selection mode, with existing/new capture tabs, and returns to the mounted
scenario after append. Keep admin/copy actions on /collectors. `CaptureDataPane`
appends all chosen streams using one `appendCaptureInputs` draft update after
bounded, sequential platform fixture materialization. Preserve existing
inputs/comments/mocks/assertions; abort pending reads on unmount and refuse
destination config/setup drift. The collectors page contains no mocks or
assertion editors, nor a separate scenario-replacement flow. Its append opens
the debugger Inputs tab. Destination dropdowns use the platform graph and shared
receiver signal mapping; never offer unchecked free-text destinations. Show the
actual configuration diagnostics, missing configuration and draft repair links,
and direct unsupported-signal deselection next to the action. Checks and fixture
requests inherit setup only from the debugger scenario. `CaptureDialog` uses a
native modal for draft replacement and consequential actions. Preserve
session-only raw drafts, bounded paging and honest stop status: `phase.ts` maps
fleet's states and reason codes to text, and a stopping capture waits for the
Collectors to release it. The product UI splits the destination into pages
(`capture/pages/`): Captures (`/captures`), capture detail (records, targets,
stop and "Use records" with `UseRecordsDrawer.tsx`) and New capture
(`/captures/new`, search `collectors`, `group` or `from`, parsed by
`newCaptureSearch.ts`). Collectors and groups live under Fleet; the old
`/collectors` paths redirect to Fleet and Live capture (`movedRoutes` in
`router.tsx`). New capture asks before leaving with an unstarted, edited form
(`useBlocker`; "Keep editing" or "Discard and leave"). Their view models
(`phase.ts`, `captureForm.ts`, `collectorView.ts`, `detailView.ts`,
`recordView.ts`, `fleetGroups.ts`, `addInputs.ts`, `format.ts`) are pure and
tested. The `CollectorsPage` is reused only inside `CaptureInputPicker`.

`CaptureRecords` is the one record list, on the capture page and in
`CaptureDataPane` (`guide={false}`). A whole row opens the record and shows
`SignalDetails` (no search, no second click); the checkbox only selects, and a
Data column marks raw records. Its filter (words and kind), "Select all N" /
"Select N matching" and samples (`SAMPLES` in `recordView.ts`: first 100, 100
spread, up to 10 per kind) cover the whole capture: `useCaptureScan` reads it
through the existing records endpoint, 100 per page, client-side (no new
shape). Selections over 200 records or 1 MiB warn and offer a sample of 100;
the fixture limits still apply. A finished capture with nothing selected
shows "Next steps": it is already stored, nothing to save, and offers a sample
or all records. "Use records" defaults to New scenario (`newScenarioPatch`),
which replaces the open scenario only after `useReplaceDrafts` confirms.
The capture aside (`.capture-aside`) is fixed-width and never scrolls
sideways.

Fleet (`fleet/`) is its own destination with `FleetHead`'s tabs:
Agents (`/fleet`, the one list of Collectors with a pane for health,
components, config state with "Retry config", and the agent's Collectors
with "Configure capture"), Groups (create, edit at the loaded revision,
delete after an inline confirmation; the selector field suggests attribute
keys from fleet's first 100 agents and, after `key=`, the key's values with
their agent counts from fleet's overview, and fixed groups pick agents by
name; on a fleet without the livecapture plugin members list no Collectors
and the group offers no capture), Configs and a config's page
(`/fleet/configs/<id>`: versions, each version read-only with its change from
the one before, a new version drafted from the version in view, which asks
before a dirty draft is discarded, rename and delete), Assignments
(`?config=&version=` preselects; creating one starts its rollout; another
version or removal starts a rollout too), Rollouts and a rollout's page
(waves, policy, targets by outcome, and only the controls the state allows;
roll back and cancel ask first), Events (a feed that reads from fleet's last
cursor every 5 seconds while visible, newest first, at most 500) and Config
bisect. `FleetGate` explains a missing fleet connection or config
management. The view model `fleetView.ts` (names, chips, group and rollout
forms, tallies, event words, refusal hints) is pure and tested; the pages are
tested in `fleet/pages/FleetPages.test.ts`. Writes need the server's fleet
credential to have fleet's admin role; fleet's own check is the only gate.
Credentials and plugin lifecycle are not shown.
Run explicit capture test files with `--maxWorkers=1 --no-file-parallelism`.
