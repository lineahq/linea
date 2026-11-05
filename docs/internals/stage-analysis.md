# Stage analysis

Verified against Collector `v0.162.0` with `transform`, `filter`, `batch`,
`attributes` and `resource` decorated (2026-10-09), and the `routing` and
`count` connectors (2026-10-10). Code: `internal/debugger/instrument/`,
`internal/debugger/analysis/`.

## Event model (schema 0.6)

`instrument.EventSchemaVersion` versions the event shape. Every event has `id`
(`<run_id>:<seq>`), `run_id`, `seq`, `time`, `kind`, `source` (`orchestrator`,
`processor_wrapper`, `connector_wrapper`, `replay_receiver`, `capture_exporter`,
`mocked_receiver`, `status_watcher`, `component_telemetry`, `endpoint_sink`)
and, where applicable, `component`, `component_kind`, `pipeline`, `signal`,
`mode` (`real`, `simulated`, `captured`, `dependency_mocked`), `input_seq`,
`from_seq`, `counts`, `snapshot`, `error`, `decision`, `objects`, `log` and
`endpoint`.

Schema 0.6 added `component.log` events: one per entry a component
created through a real factory logs at info or above, with `log` (`level`,
`message`, `fields` as strings) and `component_kind` (`receiver`,
`processor`, `connector`, `extension`), which the status watcher's
lifecycle events of custom extensions carry too. Stage analysis ignores
them. See [components.md](components.md#components-own-logs-and-metrics).
It also added `endpoint.received`: what a simulated endpoint of a
custom component received, with `component`, `component_kind`,
`endpoint` (the client's path in the component's configuration) and the
snapshot, source `endpoint_sink`. Stage analysis ignores it; lineage pairs
its records by content (`sent`). See
[components.md](components.md#simulated-endpoints).

Schema 0.5 added `objects` on telemetry events: the run-scoped number of each
record's pdata object, in record order (see
[record-objects.md](record-objects.md)). Older events decode without it.

Schema 0.4 added `stage.decision`: a tail_sampling processor's decision for
one trace, with the evaluated spans as its snapshot (see
[sampling.md](sampling.md)). Stage analysis ignores it.

Schema 0.3 added `from_seq` and connector events:

- `from_seq` on `stage.input` and `exporter.captured` is the seq of the
  emission (`input.injected`, a mocked receiver's recording, or a
  `stage.output`) whose call delivered the payload. Every emitter records its
  event first and passes `instrument.WithEmission(ctx, seq)` to the consumer
  it calls, and every ingress reads `instrument.EmissionOf(ctx)`. This is
  exact causality: snapshot equality cannot tell apart the branches of a
  fan-out carrying the same unchanged payload, and seq order says nothing
  about which call caused which.
- A connector's `stage.input` has no pipeline (the Collector builds one
  connector instance per connector and signal pair, shared by the pipelines
  it joins). Each delivery is a `stage.output` whose `pipeline` is the
  pipeline delivered into and whose `input_seq` is the connector input.

Kinds: `run.started`, `run.completed` / `run.failed` /
`run.cancelled`, `component.started`, `component.stopped`,
`input.injected`, `stage.input`, `stage.output`, `stage.error`,
`exporter.captured`, `stage.decision`, `component.log`,
`endpoint.received`.

- Payloads are stored once per content in `Result.snapshots`, keyed
  `sha256:<hex>` of the OTLP JSON; events reference them by `snapshot`.
  An unmodified pass-through stage therefore costs no extra payload.
- `pipeline` comes from the component logger (see
  [factory-decoration.md](factory-decoration.md)); events of shared
  receivers and exporters carry no pipeline.
- `stage.error` records an error returned by the stage itself, linked to its
  `stage.input` by `input_seq`. An error that came back from a downstream
  consumer is not the stage's own: the egress consumer marks the call when
  downstream fails, and the ingress side checks the mark (`errors.Is` on the
  same error value). `TestProcessorRecordsOwnErrorsOnly` covers both cases.
- Seqs depend on lifecycle order; see [run-lifecycle.md](run-lifecycle.md)
  for why the shutdown phase is canonicalized in normalized reports.

## Synchronous vs asynchronous emission

The ingress wrapper adds a context value naming the processor instance and
the `stage.input` seq. The egress consumer sets `input_seq` only if the value
names the same instance.

Observed at `v0.162.0`:

| Processor | Emission | Evidence |
| --- | --- | --- |
| `transform` | synchronous: output emitted inside `ConsumeTraces`, with the caller's context | `TestInstrumentedRunCapturesStageEvents` |
| `filter` | synchronous; when every span is dropped it emits nothing (it returns `processorhelper.ErrSkipProcessingData` internally, and the caller sees success) | same test; `TestAnalyzeClassifiesStageOutcomes` |
| `batch` | asynchronous: flushes from its own goroutine with a context that does not carry the ingress value, so emissions are uncorrelated | same test |

The owner check matters: a downstream processor's egress runs inside an
upstream processor's `ConsumeTraces` and sees that processor's context value
too. Without the check, every downstream emission would be attributed to the
first processor's input.

## Outcomes

`analysis.Analyze(events, snapshots, completed)` keys stages by
(component, pipeline, signal), one per Collector processor instance, and
attaches each `stage.error` to its input. It classifies each `stage.input`:

- `emitted`: a correlated output exists, with a structural diff.
- `no_output`: the run completed and the stage emitted nothing at all. The
  diff lists every record as removed. This is not called "filtered" because a
  rejected input (OTTL `error_mode: propagate`) also emits nothing; see the
  run's `inputs[].error` to tell them apart.
- `deferred`: no correlated output, but the stage emitted asynchronously.
  For such stages, `reconciliation` diffs everything the stage received
  (uncorrelated inputs) against everything it emitted.
- `unresolved`: the run did not complete, so absence of output proves
  nothing.

`completed` is true only after a clean Collector shutdown. Shutdown flushes
the batch processor, so the batch output seen afterwards is final.

## Structural diff

- Spans are matched by trace ID and span ID, never by position. Identities
  that occur more than once are left unmatched and reported as unresolved.
- A resource (or scope) is diffed only when its matched spans map
  one-to-one onto a single counterpart resource (or scope). Merges and
  splits are reported in `unresolved` with no attributed resource change.
- Attribute maps are compared by key with typed values (`Int` 1 vs `Str`
  "1" is `type_changed`), and nested maps are compared recursively, so a
  change reports its own path (`attributes["m"]["a"]`). Slices and bytes are
  compared as whole values. Span events and links are compared as whole
  collections because they have no identity of their own.
- Log records and metric data points have no stable ID, so their diffs
  have `identity: content` and every match is confidence-qualified
 . See below; records are paired by their object first.

## Content identity for logs and metrics

`telemetry.Records` flattens every span, log record and data point into a
typed field map (`time_unix_nano`, `body`, `attributes`, ...; data points
also carry `metric.name`, `metric.type`, `metric.unit`, ... and
type-specific fields such as `value`, `bucket_counts`, `quantile_values`).
`telemetry.Fingerprint` encodes such a map canonically and type-preserving
(`Int 1`, `Double 1` and `Str "1"` differ; key order does not matter).

Records are matched in tiers; a later tier sees only records still
unmatched:

| Tier | Logs key | Data point key |
| --- | --- | --- |
| `object` | the record object (`Event.objects`, see [record-objects.md](record-objects.md)) | the same |
| `exact` | whole record | whole record (including metric fields) |
| `high` | time, observed time, trace ID, span ID (skipped when all are empty) | metric name, start time, time, attributes; then metric name, time, attributes |
| `medium` | body, severity number, event name | metric name, start time, time; then start time, time and the value fields (`value`, `count`, `sum`, `bucket_counts`) |

- `object` pairs a record before the stage with the record after it that is
  the same pdata object, whatever the stage changed. Object numbers are
  unique, so the tier never guesses. An object match whose records are
  identical counts as `exact`; changed ones count in `matching.object` and
  their changes carry `confidence: object`. Events without objects (event
  schema before 0.5, a copying stage, the pooling gate) give the tier nothing
  to match and the content tiers decide as before
  (`TestEventsWithoutObjectsAnalyzeAsBefore`, whose golden analyses were
  written by the analysis before it read objects). Report schema 0.7 added
  `matching.object` and the confidence value.
- In `high` and `medium` a key matches only if it occurs exactly once among
  the unmatched records on each side. Identical records (`exact`) are
  interchangeable and are paired in order.
- Metric type is in no key, so `convert_sum_to_gauge` shows as a
  `metric.type` change instead of a removal plus an addition.
- The second `high` data point key leaves out the start time, which
  temporality conversions rewrite (`deltatocumulative` sets each point's
  start time to its stream's first start time).
- The last `medium` key leaves out the metric name, so a point of a renamed
  metric (`metricstransform`, `set(metric.name, …)`) matches by its
  timestamps and value, and the rename shows as a `metric.name` change.
  (Verified against `v0.162.0`, 2026-10-10.)
- Changes on non-exact matches carry `confidence`; `matching` counts the
  matches per tier and the unmatched records.
- Leftovers are listed as `removed` only when nothing is left unmatched
  after, and as `added` only when nothing is left unmatched before (refs
  index the before or after snapshot respectively). Otherwise the diff
  reports them in `unresolved` and claims nothing.
- Resource and scope changes use the same rule as spans: a resource (or
  scope) is diffed only when its matched records map one-to-one onto a
  single counterpart (`diffGroups`, shared with traces).

Verified on `testdata/multisignal`: the logs `filter/health` stage reports
"healthcheck ok" removed, `attributes/scrub` reports `high` confidence
changes and `resource` an exact match with a resource attribute change
(`TestExecuteMultisignalScenarioMatchesReference`; unit cases in
`TestDiffLogsMatchesWithConfidence` and `TestDiffMetricsMatchesDataPoints`).
With objects, the same `attributes/scrub` changes are `object` matches; unit
cases for objects in `TestStagesFollowRecordsByObject` (changed, removed,
buffered, unchanged and copied records).

## Connector stages and routes

Stages have `kind` `processor` or `connector`. A connector stage is keyed
with an empty pipeline and its input's signal, so the outputs of a
connector that changes signal (count: logs → metrics) belong to the stage
of the input that caused them. Each input lists its deliveries as `routes`
(`pipeline`, `seq`, `items`), with outcome `emitted` and no `diff`: a route
may copy, so the input is not diffed against the concatenation of its
outputs. Lineage matches records per route instead.

A connector that emits on its own goroutine into another signal
(spanmetrics: spans in, metrics on a timer) has two stages: the metrics
emissions are uncorrelated and form the metrics stage (no inputs,
`asynchronous`). Its trace stage is then `asynchronous` too, its inputs are
`deferred` (not `no_output`, which would list every span as removed), and
it has no reconciliation, which would compare spans with metrics. Lineage
ends those spans `aggregated` without `into` ("emitted later, outside any
consume call"); `TestSpanMetricsOutputIsAttributedToTheConnector`.

## Record lineage

`analysis.Lineage` follows every record of every `input.injected` payload
through the run (`Report.lineage`, ID `<injection seq>:<ref>`):

- Between components, hops are linked by `from_seq`, and positions are kept,
  since a delivery carries the payload it was given.
- Across a processor, input records are matched to the records of the
  stage's outputs with the same tiers as stage diffs. Spans match by ID
  (confidence `id`), then by object. Logs and data points match by object
  (`object`, or `exact` when unchanged), then by content (`exact`, `high`,
  `medium`). A `stage.output` hop carries the changes on that record,
  including its resource and scope.
- Across a connector, each route is matched separately, so a record copied
  into two pipelines becomes two branches.
- Deferred stages (batch) are matched through one combined match over the
  stage's uncorrelated inputs and its emissions. Without a clean shutdown,
  the buffered output is not final, and the path ends `unresolved`.
- A path ends `delivered` (exporter), `dropped` (the stage did not emit it,
  and no output record is left unmatched, which is when stage diffs list
  removals), `not_routed` (a connector delivered it nowhere), `unresolved`
  (it could not be followed with confidence), `unobserved` (passed to a
  component the debugger does not record), or at an aggregating connector
  `aggregated` / `uncounted` (see [aggregation.md](aggregation.md), which
  also covers the data point lineages derived from counted records).
- `deliveries` and `stops` summarize the ends. A delivery is `changed` when
  any stage on the way changed the record.

Tests: `TestLineageFollowsFanOutDropsAndDeferredStages`,
`TestLineageLeavesUnmatchedRecordsUnresolved`, and, against the reference
Collector, `TestExecuteFollowsRecordsThroughARoutingConnector` on
`samples/log-routing`. That sample's error log is delivered to
`otlp/archive` and `otlp/pager` (`+page.priority`, `-user.email` with `high`
confidence), its debug log is dropped at `filter/debug`, and the audit log
reaches only `otlp/audit`.
`TestExecuteFollowsRecordsByObject` runs `testdata/lineage/shared-timestamp-logs.json`
(twenty logs sharing their timestamp, five bodies and every attribute but
`enduser.id`) through `samples/logs-cleanup`: all twenty reach `otlp/backend`,
followed through `attributes/scrub` by object with `enduser.id` hashed;
before objects, all twenty ended `unresolved` there. It also follows the
renamed points of `samples/metrics-rename` by object.

## Limitations

- Each correlated input currently has one output. Multiple correlated
  outputs (one-to-many) would be concatenated before diffing.
