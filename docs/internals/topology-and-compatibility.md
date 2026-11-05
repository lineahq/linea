# Topology, execution modes and compatibility

Verified against Collector `v0.162.0` (2026-10-09). Code: `internal/debugger/topology/`,
`internal/debugger/config/diagnostics.go`, `runtime.ExecutionMode`,
`engine.Inspect`.

## Static topology

`topology.Build(*otelcol.Config, ModeFunc)` reads only the resolved
`service::pipelines`, so no telemetry is needed.

- Components are identified by `<kind>/<id>` refs, for example
  `processor/attributes/scrub`. The kind is part of the ref because a
  connector and an exporter may share an ID.
- A component in several pipelines is listed once, with every pipeline
  (`Shared()`). The Collector builds **one processor instance per
  pipeline** (see `service/internal/graph/processor.go`, where the node ID is
  derived from pipeline ID and component ID). Receivers and exporters are one
  instance per signal and are fanned in and out.
- Edges run receiver → first processor → … → exporter within each
  pipeline, one edge per receiver and per exporter.
- Configured but unused components are listed in `unused`: the Collector
  does not build them, so they cannot fail at runtime.
- An ID listed under `connectors` is a connector even when it appears in a
  pipeline's `receivers` or `exporters`.

## Execution modes

`runtime.ExecutionModes(cfg, factories)` is the single source of the isolation
policy. Both the topology and the run itself use it. Components a mock provider
claims (the filelog receiver, the k8sattributes processor, the spanmetrics
connector) are `dependency_mocked` ([mocking.md](mocking.md)). All others follow
`runtime.ExecutionMode`:

| Kind | Mode | Why |
| --- | --- | --- |
| receiver `filelog` / `file_log` | `dependency_mocked` | the real receiver reads the scenario's files from a sandbox |
| processor `k8sattributes` / `k8s_attributes` | `dependency_mocked` | the real processor queries an in-process API serving the scenario's objects |
| connector `spanmetrics` / `span_metrics` | `dependency_mocked` | the real connector runs on a simulated clock ([mocking.md](mocking.md#spanmetrics-connector-simulated-clock)) |
| receiver | `simulated` | fixtures are injected at the receiver's consumer; network intake and decoding do not run |
| processor | `real` | the upstream factory runs, decorated for snapshots |
| exporter `debug`, `nop` | `real` | cannot send data off the host; what reaches it is recorded |
| other exporters | `captured` | replaced by local capture |
| connector | `real` | the upstream factory runs, decorated for routes (`count`, `forward` and `routing` are in the distribution besides `spanmetrics`, below) |
| extension | `simulated` | "extensions do not run; their settings are validated" |
| extension from a custom module | `real` | "a custom extension: it runs for real" |
| any type outside the distribution | `stand_in` | a stand-in runs in its place (below) |

A component whose settings use a reference's stand-in string keeps its
mode; the topology lists those references in its `stand_in_values`.

## Compatibility diagnostics

The Collector's own unmarshal stops at the first unknown type, with a
message such as `'receivers' unknown type: "kafka" for id: "kafka" (valid
values: [otlp])`. To report every problem at once, `config.ResolveURIs` first
expands the configuration with the providers, without interpreting it
(`confmap.NewResolver(...).Resolve`). It then checks every key under
`receivers`, `processors`, `exporters`, `connectors` and `extensions` against
the factory maps. Each unknown type is one `unsupported_component`
**warning** (`Resolved.Warnings`) that lists the supported types, and
resolution continues with a stand-in factory for it (`internal/debugger/standin`).
Other
failures, such as invalid YAML (the message includes `line N`), OTTL parse
errors and dangling pipeline references, stay as the Collector reports them.
`config.AsDiagnostics` turns those into a single `invalid_configuration`
diagnostic.

## Stand-ins for unknown types

Verified against `v0.162.0` (2026-10-14). `standin.Add` registers, per
missing type, a factory whose configuration (`standin.Config`) accepts any
settings without interpreting them. The settings sit in an unexported field,
so the Collector's validation walk skips them, and they marshal with every
value replaced by `[REDACTED]`: without the settings types nothing tells a
secret from a plain value. Config resolution adds the factories from the raw
configuration; runs and reference runs add them from the resolved
configuration (`standin.FromConfig`), before the usual decoration:

| Kind | Stand-in | In a run |
| --- | --- | --- |
| receiver | receives nothing, every signal | fixtures are injected as for any receiver |
| processor | passes data through, every signal | instrumented like any processor |
| exporter | sends nothing, every signal | captured like any non-local exporter |
| connector | forwards same-signal data; emits nothing into another signal | instrumented like any connector |
| extension | inert | inert like every extension |

A connector between two signals cannot know what the real one derives, so its
downstream pipeline receives nothing. Assertions on data that passed a
stand-in processor or connector, or a component configured with stand-in
values, are inconclusive ([assertions.md](assertions.md#outcomes)).
Reference runs refuse unknown receivers and exporters through their existing
prerequisites.

## Effective configuration

`config.Effective` does what `otelcol print-config --mode redacted` does:
`confmap.New().Marshal(cfg)`. It includes every default. `configopaque.String`
values, such as OTLP exporter `headers`, marshal as `[REDACTED]`. Values a
component does not type as opaque are not redacted.

## Shared exporters

An exporter used by pipelines of different signals receives payloads of
each signal. Run outputs are therefore keyed by exporter **and** signal
(`Output.Signal`). Merging them once panicked in
`telemetry.Concat`.

## Verified

- `TestBuild*` (topology), `TestResolveDiagnosesUnsupportedComponents`,
  `TestInspect*`.
- `TestExecuteMultisignalScenarioMatchesReference`: the resource, attributes
  (`delete`, `hash`), filter (spans and log records), transform (log
  context) and batch processors, sharing two pipelines and one exporter,
  give output identical to the uninstrumented reference Collector.
