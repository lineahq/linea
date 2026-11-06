# Supported components

Verified against Collector and contrib `v0.162.0` on Windows (2026-10-10).
The set is pinned by `build/linea-runtime.yaml` and mirrored by
`internal/debugger/registry` (`TestModulesMatchOCBManifest`).

Processors and connectors run for real and are instrumented. Receivers are
replaced by replay inputs, and exporters other than `debug` and `nop` are
captured; `debug` and `nop` run for real, and what reaches them is recorded
as their output ([factory-decoration.md](factory-decoration.md)). The reference run
uses the unmodified factories ([reference-runs.md](reference-runs.md)).

## Conformance

`TestExtendedComponentsMatchTheReference` (`internal/debugger/engine`) runs each
config in `testdata/components/` instrumented and against the reference
Collector. It checks that the outputs are equal, the stage outcome and the
removed records, the change paths the stage must report, and that no record
lineage ends `unresolved`.

| Component | Config | What the analysis shows |
| --- | --- | --- |
| `redaction` | `redaction.yaml` | the masked attribute as modified; `summary: info` adds `redaction.masked.*` attributes |
| `groupbyattrs` | `groupbyattrs.yaml` | the attribute moved from spans to a new resource; see limitations |
| `probabilistic_sampler` | `probabilistic_sampler.yaml` | `sampling_percentage: 0` drops every span: `no_output`, all removed |
| `span` | `span.yaml` | `name` rebuilt from attributes |
| `metricstransform` | `metricstransform.yaml` | `metric.name` and an added label, matched by the renamed-metric tier |
| `cumulativetodelta` | `cumulativetodelta.yaml` | the first point of a series has no previous value and is removed |
| `deltatocumulative` | `deltatocumulative.yaml` + `delta_sums.json` | the running total in `value`, the stream's first `start_time_unix_nano`, temporality |
| `memory_limiter` | `memory_limiter.yaml` | passes data through below the limit |
| `forward` connector | `forward.yaml` | records followed through `logs/in → forward → logs/out`, into an `otlphttp` exporter |
| `otlphttp` exporter | `forward.yaml` | captured; the reference delivers to the OTLP/HTTP sink |

## Behavior worth knowing

- **memory_limiter depends on the host.** It refuses data when the
  process's memory use passes its limit, so a run on a loaded machine can
  differ. Use generous limits in replays.
- **groupbyattrs regroups resources.** Spans still match by trace and span
  ID, but the diff reports "resource(s) were merged or split; their resource
  and scope changes are not attributed". `report.grouping` explains the
  regrouping from the record lineage instead (`grouping.md`).
- **deltatocumulative keeps state per stream** for the run only. Each replay
  starts empty, so a delta's running total covers only the points injected
  in that run. Its `time.Now()` feeds stale-stream expiry only, not the output.

## Extensions

Verified against `v0.162.0` on Windows (2026-10-14). The distribution
includes `health_check`, `pprof`, `zpages`, `file_storage`, `basicauth`,
`bearertokenauth`, `oauth2client` and `headers_setter`, so configurations
that use them validate with the upstream settings types. No upstream
extension runs: `runtime.inertExtension` keeps each factory's type, default
configuration and stability and creates an extension whose Start and
Shutdown do nothing, in the instrumented run and the reference run alike.
Topology mode `simulated`, note "extensions do not run; their settings are
validated". A custom extension runs for real; see below.

- **Start-up dependencies.** A real `health_check`, `pprof` or `zpages`
  binds its endpoint at Start, and the service fails when the port is taken
  (`TestExtensionsDoNotRunDuringRuns` holds the port). Authenticators read
  their token or secret files and fetch tokens at Start; none of that
  happens.
- **Storage.** The inert `file_storage` implements the storage extension
  interface with `storage.NewNopClient()`: a filelog receiver with
  `storage:` or an exporter queue with storage starts as with an empty
  directory and keeps nothing between runs, which matches a fresh run.
- **file_storage validation reads the host.** Upstream `Config.Validate`
  stats `directory` (and the compaction directory) and fails when it does
  not exist, unless `create_directory` is set. The registry wraps the
  factory (`internal/debugger/registry/filestorage.go`): the upstream settings sit in
  an unexported field, which the Collector's validation walk skips
  (`confmap` validates exported fields only), and the wrapper validates a
  copy whose directories are replaced by an existing one. Its `Marshal`
  needs a value receiver: the marshal hook looks for `confmap.Marshaler` on
  struct values, and a pointer receiver silently marshals an empty map.
- **Exporter auth.** Captured exporters never send, so `auth:` has no
  effect on a run. The reference run cannot authenticate (see
  [reference-runs.md](reference-runs.md)).

## Custom extensions

Verified against `v0.162.0` on Windows (2026-10-15). A runtime built with
custom modules records the module of each factory in the Collector's own
`otelcol.Factories.<Kind>Modules` maps, as OCB's registry does (the
generated `internal/debugger/registry/components.go`, `modulesOf` adding deprecated
aliases). An extension whose module is not upstream (`manifest.Upstream`)
keeps its real factory in runs and reference runs (`runtime.runExtensions`);
topology mode `real`, note "a custom extension: it runs for real".

- **It cannot be wrapped.** Components find extensions by their concrete
  type: the capture processor does
  `host.GetExtensions()[id].(*livecaptureextension.Manager)` at Start and
  refuses to start with anything else, so the instance the Collector holds
  must be the real one.
- **Lifecycle from status reports.** `service/extensions.Extensions`
  reports `Starting`, then `OK` (or a permanent error) around each Start
  and `Stopping`, then `Stopped` (or a permanent error) around each
  Shutdown, synchronously, to every extension that is a
  `componentstatus.Watcher`, started or not (`NotifyComponentStatusChange`
  iterates the extension map). It logs "Extension is starting..." and
  "Extension started." but nothing per extension at shutdown. A run with a
  custom extension in `service.extensions` therefore adds the debugger's
  `linea_status` extension last through `yaml:` overrides
  (`statusWatcherURIs`); it records `component.started` and
  `component.stopped` (source `status_watcher`, mode `real`, the error of
  a permanent or fatal status) for custom extensions only. A run without
  one runs the configuration as written. The reference run starts the
  custom extensions without the watcher.
- **The in-process registry has no custom component**; every factory in
  it has an upstream module, so built-in runs are unchanged.

## Components' own logs and metrics

Verified against `v0.162.0` on Windows (2026-10-15). Code:
`internal/debugger/instrument/telemetry.go`. Every component created through a real
factory gets its own telemetry recorded: processors and connectors (their
wrappers), mocked receivers (`instrument.Receiver`, such as filelog) and
custom extensions (`instrument.Extension`, which wraps only the factory's
create, so the Collector still holds the real extension). Replay
receivers, capture exporters and inert extensions are the debugger's own
and are not observed.

- **Logs.** The component's `TelemetrySettings.Logger` is teed: the
  entry still reaches the Collector's logger, and each entry at info or
  above becomes a `component.log` event (`log.level`, `log.message`,
  `log.fields` as strings: text as it is, other values as JSON) with the
  component, its kind and pipeline. Debug entries are not recorded, so a
  chatty component cannot spend the evidence budget its telemetry needs.
  The Collector's own attributes (`otelcol.*`) and the debugger's pipeline
  probe are left out; a message or field value over 4 KiB is clipped with
  "…". Entries are observations: the evidence budget stops them, never
  lifecycle events.
- **Metrics.** Each component instance gets an SDK meter provider with a
  manual reader in place of the Collector's (a component created for
  several signals shares one, as it shares the Collector's). The readers
  are collected once the Collector has stopped into the report's
  `component_metrics`: one entry per data point (component kind,
  component, pipeline, name, unit, `sum`/`gauge`/`histogram`, attributes;
  `int_value` or `value`, histograms as `count` and `sum`). The processor
  helper's `otelcol_processor_incoming_items` / `outgoing_items` and a
  filter's `otelcol_processor_filter_logs.filtered` arrive this way. The
  Collector's own service telemetry is not recorded.
- **Normalized reports** clear the values of metrics measured in time
  units (`s`, `ms`, `us`, `ns`, `min`, `h`) and keep their counts, since
  durations differ between runs.
- **A Collector that fails to start** returns its error, not a report,
  so what its components logged before the failure is lost with it; the
  Collector's own logger still has the entries.

## Simulated endpoints

Verified against `v0.162.0` on Windows (2026-10-15); the `configgrpc` and
`confighttp` client and server shapes used here are the same at 0.160.0
and 0.161.0. Code: `internal/debugger/mock/endpoints.go`, `endpoint_sinks.go`,
`endpoint_factories.go`, `internal/debugger/runtime/endpoints.go`.

A custom processor, connector or extension runs for real, so whatever
client its configuration declares would reach the network. Before its
factory creates it, `mock.Endpoints.Redirect` walks a copy of its decoded
configuration and points every client at a loopback sink of the run.

- **Found by type, not by name.** The walk (reflection over exported
  fields with their mapstructure keys, pointers, slices, arrays, maps and
  `configoptional.Optional`, through its `Get`) finds every
  `configgrpc.ClientConfig` and `confighttp.ClientConfig` with an
  endpoint, embedded ones included: an `otlpexporter.Config` squashes its
  gRPC client, so its `delivery` is found at path `delivery`.
  Paths join keys with `::` and index slices (`routes[0]::client`); map
  entries are keyed by their key. A client held in a plain string, or
  behind an interface, is not found: the component's connection fails
  (runs have no network) and its logs say so.
- **The copy, not the YAML.** Only the structs, pointers, slices and maps
  on the way to a client are copied; the Collector's configuration and
  the user's YAML are unchanged. The copy's client has TLS off, `auth`
  and middlewares removed (the extensions they name are inert) and, for
  HTTP, no proxy; headers are kept, and an HTTP endpoint keeps its path
  and query (`https://h/ingest/v1/logs` becomes
  `http://127.0.0.1:<port>/ingest/v1/logs`). A component the extension
  builds from that configuration itself (the extension's manager creates its
  OTLP exporter from `config.Delivery`) uses the copy too.
- **Sinks.** One per component and path, on `127.0.0.1:0` only, shared by
  every instance of the component, stopped when the run ends. A gRPC sink
  serves the OTLP trace, metric and log export services (the gzip, zstd
  and snappy codecs `configgrpc` registers are accepted) and answers any
  other method `Unimplemented`; an HTTP sink (a `confighttp` server, so
  compressed bodies decode) accepts POST to any path ending in
  `/v1/traces`, `/v1/metrics` or `/v1/logs`, protobuf or JSON, and
  answers anything else 404. Refused calls are counted per method
  (`/pkg.Service/Method`, or `GET /path`).
- **Evidence.** What a sink receives is an `endpoint.received` event
  (component, `component_kind`, `endpoint` = the path, source
  `endpoint_sink`, mode `captured`, its snapshot) and, while the evidence
  budget keeps it, part of the report's `endpoint_outputs` (per
  component, endpoint and signal: batches, counts, data, as exporter
  outputs; canonicalized and normalized like them). The report's
  `simulated_endpoints` lists component, path, protocol, the configured
  endpoint and the refused calls; the mocks list a declaration per
  component (provider `endpoints`, mode `dependency_mocked`, refused calls
  as warnings). The component's topology mode becomes `dependency_mocked`
  with the note "its client endpoints are loopback OTLP sinks: <paths>"
  (after a custom extension's own note).
- **Lineage.** The data was encoded and sent, so nothing links a call to
  a record object. `analysis.Lineage` pairs each received record with the
  lineage that has it at one of its hops: identical (`exact`), a span
  with the lineage's IDs (`id`), then the stage diffs' log and data point
  keys (`high`, `medium`), each only when one lineage and one received
  record have the key. The pairing is the lineage's `sent`; a record no
  lineage or several have stays unpaired.
- **The reference run** simulates the same endpoints and keeps nothing:
  only exporter outputs are compared.
- `endpoint.received` events arrive when the component sends, which can
  be outside any consume call, so their order among other events is the
  observed one.

## Timer-driven components

These components emit on a timer, not when data arrives. A replay ends by
shutting the Collector down, so what they emit depends on how long the run
lasts and on what their shutdown does.

- `spanmetrics` connector (factory type `span_metrics`, alias
  `spanmetrics`): **supported on a simulated clock**
  ([mocking.md](mocking.md#spanmetrics-connector-simulated-clock)). It
  emits on its flush ticker; `Shutdown` stops the ticker without a final
  flush.
- `tail_sampling` processor: **supported on the real clock**, with the
  scenario timeline ([scenarios-and-api.md](scenarios-and-api.md#timeline)).
  It has no clock it can be handed: it decides on a `time.NewTicker(1s)` of
  its own and reads `time.Now()`. Behavior at contrib `v0.162.0`:
  - Traces are kept in `max(1, decision_wait in whole seconds)` batches;
    each tick decides the oldest batch. A trace is decided between
    `decision_wait` and `decision_wait` + 1s after its first span.
  - `Shutdown` **decides every pending trace** and forwards the sampled
    ones, unless `drop_pending_traces_on_shutdown: true`, which drops them.
    With the default, a run of a moment therefore sends what the policies
    sample, but at shutdown rather than after `decision_wait`.
  - With `drop_pending_traces_on_shutdown`, the run waits after the last
    input (settle) for `max(1, decision_wait s)` + 1s tick + 500ms margin,
    unless the scenario declares `execution.settle`.
  - A span arriving after its trace was decided is forwarded (or dropped)
    on its own at once, by the recorded decision: decided traces stay in
    memory until evicted by `num_traces`. This holds with and without
    `decision_cache`.
  - Defaults: `decision_wait` 30s, `num_traces` 50000, decision caches 0
    (disabled).
  - The topology notes this per processor.
  - Decisions are recorded and explained per trace
    ([sampling.md](sampling.md)).
  - `TestTailSamplingFollowsTheScenarioTimeline` runs
    `testdata/components/tail_sampling.yaml` (`decision_wait: 1s`,
    `drop_pending_traces_on_shutdown`) with
    `tail_sampling_late_span.yaml`, whose late child is sent 3s after its
    root. The sampled root and the late child arrive in two deliveries,
    equal to the reference Collector's.
