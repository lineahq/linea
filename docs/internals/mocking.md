# Environmental mocking

Verified against Collector `v0.162.0` (2026-10-10). Code: `internal/debugger/mock/`,
`internal/debugger/runtime/mockhost.go`, the `mocks` section of
`internal/debugger/scenario/scenario.go`. Examples: `testdata/receivers/filelog/`,
`testdata/receivers/kubernetes/`, and the samples `container-logs` and
`pod-enrichment`. The k8sattributes processor findings were
verified against its v1.1.0 release (the stable line for v0.162.0) with
client-go v0.37.

## Model

A component whose behavior depends on something outside the Collector (a
node's files, the Kubernetes API, the clock) runs for real against a substitute for that
dependency: execution mode `dependency_mocked`. The Collector YAML is never
changed. Fixtures come from the scenario's `mocks` section.

A `mock.Provider` (one per dependency kind) does the following:

- It claims the configured components it handles (`Components`), which feeds
  `runtime.ExecutionModes` and therefore the topology that `inspect` shows.
  A claimed component is mocked even when the scenario supplies no fixtures:
  isolation does not depend on the scenario.
- It wraps their factories (`Receiver`, `Processor`, `Connector`). The
  runtime applies the wrapper instead of replay substitution; processors and
  connectors are still instrumented after wrapping.
- It prepares the dependency before the Collector starts (`Prepare`, which
  registers the hooks below on the `provider.Run`), waits for the components
  to load it once the Collector runs (`OnReady`, before any input is
  injected), drives it after inputs are injected (`OnInputsDone`, which
  returns when the component has settled), and cleans up (`OnClose`). The
  built-in providers implement an internal start, ready, drive, close
  lifecycle that `mock.PrepareBuiltin` maps onto these hooks.
- It reports a `Declaration` per component in `Result.mocks`: what was
  supplied, the limitations of the mock, and warnings such as fixtures nothing
  read.

Before any run, a provider's `Gaps` checks the spec statically (no
component starts) and reports components it would give nothing usable to
read; `mock.Gaps(cfg, spec, builtin.Providers, plugins)` collects them. Telemetry a mocked receiver
emits is recorded as an `input.injected` event
with mode `dependency_mocked` and source `mocked_receiver`, and is summarized
as an `inputs[]` entry with `mock: <provider>`. Stage analysis, assertions and
the UI therefore treat it like a replayed input.

Simulated endpoints are not a provider: the client endpoints of
custom components are substituted whatever the scenario says, and declared
with provider `endpoints`. See
[components.md](components.md#simulated-endpoints).

## Scenario section

```yaml
mocks:
  files:
    - path: /var/log/pods/ns_pod_uid/container/0.log   # deployment path, not a pattern
      existing: |          # present before the Collector starts (start_at decides)
        ...
      content: |           # appended while running: always read
        ...
      # file: raw.log      # instead of content: raw bytes, relative to the scenario
      # modified: 2026-10-10T10:00:00Z
  kubernetes:
    files: [cluster.yaml]  # kubectl get -o yaml output; multi-document, List expanded
    objects:               # inline manifests, same shape
      - {apiVersion: v1, kind: Namespace, metadata: {name: shop}}
  exporters:
    mode: capture          # the only mode; documents the default
inputs:
  - file: span.json
    client_address: 10.42.0.17   # sender IP (optionally :port) for connection association
    headers:                     # request headers; a value is a string or a list
      tenant: acme
defaults:
  headers:                       # sent with every input; an input's own header wins
    user-agent: shop-client/1.0
execution:
  drain_timeout: 2s        # quiet period after the last record (default below)
```

A scenario needs `inputs` or `mocks.files`. Paths containing glob characters
or `..`, and duplicate paths (with `\` and `/` treated alike), are rejected.

## Filelog receiver

- **Type name.** At v0.162.0 the factory type is `file_log`. `filelog` is a
  deprecated alias that still resolves to it, and the builder logs
  `"filelog" alias is deprecated; use "file_log" instead`. The `Type()` of
  the factory is `file_log`, so a check against `filelog` alone silently
  misses the receiver. A wrapping factory must keep the alias:
  `xreceiver.WithDeprecatedTypeAlias(f.(interface{ DeprecatedAlias()
  component.Type }).DeprecatedAlias())`. Without it, configs written as
  `filelog` fail to resolve.
- **Sandbox.** Each run creates a temporary directory, made canonical with
  `Abs` and `EvalSymlinks` so the paths that fileconsumer reports match. Each
  deployment path is mapped into it:
  - `/x` → `root/x`
  - `C:\x` → `drive-c/x`
  - `\\host\share` → `unc/host/share`
  - relative → `cwd/`

  The receiver's config copy gets its `include` and `exclude` patterns mapped
  the same way, so glob semantics are those of the real receiver. The mapped
  path is cleaned as if the sandbox were the filesystem root
  (`path.Clean("/" + rel)`), so `..` can never leave it. Before 2026-10-12 the
  mapping used `filepath.Join` alone, which cleans `..` against the host path
  of the sandbox: an `include` pattern such as `/var/log/../../../../x/*.log`
  made the real receiver read host files (`TestFilelogPatternsCannotLeaveTheSandbox`).
- **Path restore.** With `include_file_path` (or `_resolved`), the first
  operators are `add` operators with
  `EXPR({sandbox: deployment, ...}[replace(field, "\\", "/")] ?? field)`.
  They rewrite `log.file.path` (and `log.file.path_resolved`) to the
  deployment path, so operators such as the `container` parser (which derives
  `k8s.*` from `/var/log/pods/<ns>_<pod>_<uid>/<container>/`) behave as in
  production.
  - The operators are built as `confmap` maps and decoded with
    `operator.Config.Unmarshal`. That is the only way to get a correctly typed
    operator builder.
  - `log.file.name` is unaffected, because base names do not change.
- **Paths in the receiver's logs** are restored too (`filelog_paths.go`), in
  the message and in string, stringer and error fields, before entries reach
  the Collector's logger or become `component.log` events.
  fileconsumer logs "Started watching file" with the sandbox path, which
  differs on every run and made normalized reports differ.
- **Polls.** fileconsumer logs the debug message `matched files` (with a
  `paths` field) at the start of every poll, once the include and exclude
  patterns have been applied. The mock wraps the receiver's logger with a zap
  core that always enables Debug for this message and counts polls. The
  message is not forwarded, so the user's log level is unaffected. The first
  poll happens one `poll_interval` (default 200ms) after start.
- **start_at and timing.** `start_at: end` (the default) sets end offsets
  only for files present at the *first* poll. Files and content that appear
  later are read from the beginning.
  - The first poll logs `matched files` *before* it fixes the end offsets.
    Content appended right after that message was sometimes skipped as
    existing content (seen as an intermittent 0-record run). Content is
    therefore appended only after the *second* poll starts, which marks the
    end of the first.
  - `existing` is written before start, so `start_at` decides whether it is
    read, as on a first start without a checkpoint.
- **Drain.** After appending, the run waits for two more polls of every
  receiver. The first reads the content and the second confirms nothing new.
  It then waits for a quiet period with no emitted records: `drain_timeout`,
  or else the receiver's `force_flush_period` (default 500ms) plus
  `poll_interval`. That covers multiline entries held until a flush.
- **Limitations reported.**
  - Files are read from an isolated directory, and paths are restored.
  - File owner and permissions are those of the sandbox.
  - `storage` checkpoints are not loaded.
  - With `start_at: end`, existing content was skipped.
- **Warnings reported.** A receiver that was given no files, and files that
  no receiver read because no include matched them or an exclude removed
  them.
- **Reference runs** (`--compare`) read the same files from a sandbox of
  their own; see [Reference runs with mocks](#reference-runs-with-mocks).

## k8sattributes processor

- **Type name.** The factory type is `k8s_attributes`; `k8sattributes` is the
  deprecated alias, kept by the wrapper as for filelog.
- **Connection.** The processor builds its clients at Start, through
  `k8sconfig.CreateRestConfig`. With `auth_type: none` it reads
  `KUBERNETES_SERVICE_HOST` and `KUBERNETES_SERVICE_PORT` and connects to
  `https://host:port` with TLS verification off. `k8sconfig` is a contrib
  `internal` package and cannot be imported; the field is set with the
  untyped constant (`cfg.APIConfig.AuthType = "none"`).
  - The wrapper runs the processor on a copy of the config with auth `none`,
    no kubeconfig context and `wait_for_metadata: true`, whatever the user
    configured (`serviceAccount` in the example), so no credential is read
    and no live cluster is reached.
  - Each processor gets its own in-process TLS API server. Start sets the
    two variables to it under a process-wide mutex and restores (or unsets)
    them afterwards, so concurrent runs and the host environment are not
    affected.
- **API surface.** Pods use the typed clientset. Namespaces, ReplicaSets,
  Deployments, Nodes and the other owner kinds use the metadata client: it
  sends `Accept: ...;as=PartialObjectMetadataList` and expects kind
  `PartialObjectMetadataList`/`PartialObjectMetadata` with apiVersion
  `meta.k8s.io/v1`. Label and field selectors (`spec.nodeName`,
  `status.phase`, `metadata.name`/`namespace`) are applied by the server.
- **Watch-list.** client-go v0.37 enables `WatchListClient` by default.
  Informers first send `watch=true&sendInitialEvents=true`; the server must
  stream ADDED events and then a BOOKMARK whose metadata carries the
  annotation `k8s.io/initial-events-end: "true"`, or the informer never
  syncs. When watch-list fails, client-go falls back to list then watch. The
  fake server implements both, and keeps watches open with no further
  events: objects are fixed during a run.
- **Readiness.** `wait_for_metadata` only waits for the pod informer. The
  namespace, node and owner informers sync concurrently, so a span sent right
  after Start can miss namespace labels. `Ready` therefore waits until every
  fake server has answered no request for 100ms (watches count as answered
  once their initial events are flushed).
- **Deployment name.** `k8s.deployment.name` is derived from the
  ReplicaSet owner by stripping the pod's `pod-template-hash` label value.
  Without that label (always present on real Deployment pods) the attribute
  is missing, even though the owner is set.
- **Association.** `connection` association reads `client.FromContext(ctx).Addr`.
  An input's `client_address` (an IP, optionally with a port; host names
  are rejected) is attached to the injection context as a `*net.TCPAddr`.
  `resource_attribute` association (for example `k8s.pod.ip`) needs nothing
  extra.
- **Headers.** An input's `headers`, merged key by key over
  `defaults.headers` (names are case-insensitive and lowercased; the input
  wins), are the request headers it arrives with. Names must be HTTP tokens,
  not pseudo-headers or `-bin`, and every header needs a value. They apply
  to OTLP receivers only and reach the pipeline only with
  `include_metadata: true` (gRPC's when configured, else HTTP's), as
  `client.Info.Metadata`; the reference run sends them as gRPC metadata.
  The result names each input's headers, never their values, and marks
  `headers_dropped` when the receiver ignored them. See
  [batching.md](batching.md) for `metadata_keys`.
- **Declarations.** Supplied lists the objects of the kinds the processor
  requested (`Pod shop/checkout-...`). Limitations: objects are fixed, the
  API connection is replaced, `wait_for_metadata` was enabled (when the user
  had not), and unmatched telemetry passes through. Warnings: no objects
  declared, a requested kind with no objects, objects of kinds no extraction
  rule reads (for example a Node without `k8s.node.uid` extraction), and
  requests the fake server could not answer.
- **Manifests.** Supported kinds: Pod, Namespace, Node (v1); ReplicaSet,
  Deployment, StatefulSet, DaemonSet (apps/v1); Job, CronJob (batch/v1).
  Namespaced objects default to namespace `default`; a missing
  `resourceVersion` becomes `"1"`; duplicates and unsupported kinds are
  rejected.

## spanmetrics connector (simulated clock)

Verified against contrib `v0.162.0` (`internal/debugger/mock/builtin/clock.go`,
`TestClockFlushesAggregatedSpansAtShutdown`, `testdata/receivers/spanmetrics`).

- **Behavior.** The factory type is `span_metrics`; `spanmetrics` is its
  deprecated alias, so a provider must match both. The factory reads its
  clock with `clockwork.FromContext(ctx)` from the create context and makes
  its ticker in the constructor (`clock.NewTicker(metrics_flush_interval)`,
  1m by default). Each tick exports, even with nothing aggregated, through
  `ConsumeMetrics` on the connector's goroutine (no consume call is in
  progress, so stage analysis sees asynchronous emissions). `Shutdown`
  stops the ticker and never flushes: a real run of a moment exports
  nothing. With cumulative temporality a new series' first `calls` point is
  0 (`isFirst`) and the count follows at the next flush; delta reports the
  count at the first flush. Points carry `collector.instance.id`, taken
  from the Collector resource or else a random UUID per connector.
- **Mock.** The `clock` provider claims every spanmetrics connector
  (mode `dependency_mocked`, no fixtures, no gaps). It creates the real
  connector with a `clockwork` fake clock in the create context, starting
  at `mock.Spec.Clock`: the runtime sets it to the latest record timestamp
  (after any rebasing, so then the run's start)
  of the inputs (span start/end, log time/observed time, data point time),
  else the creation time. The clock stands still while inputs are
  processed. The wrapper's `Shutdown` (only after a successful `Start`)
  advances the clock by the flush interval, twice for cumulative and once
  for delta, waiting for each export to return before advancing again
  (a fake ticker's channel holds one tick). Then it calls the real
  `Shutdown`. The Collector shuts a connector down before the pipelines it
  feeds, so the exports reach the exporters.
- **Stalls.** An export that has not returned within 10s, or by the end of
  the shutdown context, ends the flush with a declaration warning. The real
  `Shutdown` then runs in the background, because it waits on an unbuffered
  channel for the export goroutine.
- **Declaration.** `supplied` lists the clock start and each flush time;
  limitations say that all inputs fall into one flush interval and flushes
  happen at the run's end.
- **Reference runs** run the connector on a simulated clock that starts at
  the same time, so both runs flush the same data points.

## Reference runs with mocks

Verified against Collector v0.162.0.

A reference run (`--compare`) builds the same mock providers from the
scenario's `mocks` as the instrumented run. They wrap the real factories
without instrumentation, so the reference Collector never reaches the
host's files, a real cluster or the wall clock for flushing.

- **Filelog.** A mocked receiver has no OTLP endpoint. Inputs addressed to
  it are injected where it delivers, as in the instrumented run, and the
  files it reads are reported as inputs.
- **Client addresses.** The real OTLP receiver sees a loopback connection.
  When an input declares `client_address`, the reference run records the
  local address of the connection it dials for that input. A wrapper around
  the OTLP receiver factory then replaces `client.Info.Addr` with the
  declared address before the next consumer runs. The receiver itself runs
  unchanged, as it would behind address translation. Without the rewrite,
  k8sattributes cannot associate the pod and the comparison differs.
- **Comparison.** When either run has mocks, both outputs are normalized
  before comparison:
  - attributes are sorted, because k8sattributes inserts them in map order;
  - log observed timestamps are cleared, because filelog stamps them when it
    reads;
  - `collector.instance.id` is masked.

  The comparison carries the note "mocked components: log observed
  timestamps, collector.instance.id and attribute order are not compared".

## Missing fixtures and fidelity indicators

A mocked component never silently reads nothing.

- **Before a run.** `inspect` (CLI and `POST /api/v1/inspect`) adds one
  `mock_fixture_missing` warning per gap, naming the component. The
  configuration stays valid, since a run still executes against the empty
  dependency. Without a scenario the warnings say what a scenario must
  supply. The gaps are:
  - filelog: no `mocks.files`, or no file matches an include pattern
    without matching an exclude. Matching uses `doublestar.Match` (the
    library fileconsumer globs with) on both sides after the sandbox
    mapping, so `\` and drive letters compare as in a run.
  - k8sattributes (unless `passthrough`): invalid objects, no objects, or
    no Pod.
  - A scenario whose `mocks` cannot be loaded (for example a relative
    `mocks.kubernetes.files` entry in editor text without a directory)
    gets a `mock_fixture_missing` warning carrying the error.
- **After a run.** Declarations carry the run's own evidence: what was
  supplied, the limitations, and warnings (unread fixtures, kinds watched
  but not declared, unserved API requests). `run --format text` prints
  each as `mocked <ref> (<provider>, dependency_mocked)` with `supplied:`,
  `limitation:` and `WARNING:` lines. Inputs show `(read from the <provider>
  mock)` and `from client <address>`.
- **Web client.** The graph labels `dependency mocked` components in the
  accent color, unlike `simulated` and `captured`, because the component
  itself runs. The run summary lists each declaration's warnings (warning
  level) and limitations (info) under Limitations. It also has a "Mocked
  dependencies" table of the supplied fixtures, and an input Source column
  (`scenario` or `<provider> mock`, plus the client address).

## Isolation evidence

`TestMockedRunsReachNoInfrastructure` (engine) is the exit
criterion for mocking, with production infrastructure present as decoys:

- A real host file sits at the path a filelog include pattern matches, with
  `start_at: beginning`. Only the scenario's file at that same deployment
  path is exported, and the host file is unchanged.
- `KUBERNETES_SERVICE_HOST`/`PORT` point at a listening TCP "cluster". The
  scenario still passes, the listener receives no connection, and the
  variables hold their values afterwards.

A quick run (no scenario) takes mocks too: `setup_yaml` is scenario
syntax with only `mocks`, `environment` and `execution`
(`scenario.ParseSetup`, which refuses `inputs` and `assertions`), and
`client_address` sets the sender address of the one payload. Without them
mocked components run against empty mocks: a filelog receiver reads nothing
(quick input is injected after it), and k8sattributes associates nothing.
The declarations warn, in wording that names no mode
(`TestQuickRunAppliesSetup`, `TestQuickRunRejectsScenarioOnlySetup`).

## Normalized reports

Mocked components make two things nondeterministic:

- Stanza stamps `observedTimeUnixNano` from the wall clock.
- Stanza, and the k8sattributes processor, build attributes from Go maps,
  so their order varies between runs.

The spanmetrics connector also stamps each data point with a random
`collector.instance.id`.

When `Result.mocks` is non-empty, `Report.Normalize` therefore clears the
observed timestamps of log records, sorts resource, scope, span, log record
and metric data point attributes by key (nested maps too), replaces a data
point's `collector.instance.id` with `instance-normalized`, and
re-addresses snapshots by their new content. Event snapshot
references and outputs are rewritten to match. Non-normalized reports keep
the values exactly as the Collector produced them.
