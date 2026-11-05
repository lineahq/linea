# Run lifecycle, status and reproducibility

Verified against Collector `v0.162.0` (2026-10-09). Code:
`internal/debugger/runtime/runtime.go`, `internal/debugger/runresult/canonical.go`,
`internal/debugger/engine/normalize.go`, `internal/debugger/engine/provenance.go`.

## Run states

A run moves `queued` → `assigned` → `running` → one of `done`, `error`,
`cancelled` or `lost` (code: `internal/debugger/platform/scheduler.go`).

- `assigned`: a worker claimed the run under a lease and has not started it.
  `attempt` counts the claims.
- The worker renews its leases every third of the lease (`DefaultLease`
  30 s). A lease that ends while its run is unfinished returns the run to the
  queue once; the requeue bumps the run's generation, so the old holder can
  no longer publish. A lease that ends on the second attempt makes the run
  `lost`.
- Cancelling a run no worker holds ends it at once as `cancelled`; a held
  run is marked in the queue and its worker stops it at its next renewal.
- After a restart, an unfinished run with an input bundle is scheduled
  again: a queued one at once, a held one once its lease ends. A run without
  a bundle, or missing from the queue, ends as `error` with `interrupted`.

Tests: `TestRecoveredRunsResumeFromTheirBundles`,
`TestRunIsLostWhenItsLastLeaseEnds`,
`TestQueueCancellationStopsTheRunningRun` (platform) and
`TestPlatformRequeuesARunItsInstanceAbandoned` (`store/pg`).

## Run status

`runtime.Run` returns an error only when the Collector cannot be built or
started; a run that started always yields a `Result` with a status, and the
terminal event kind follows it (table below).

| Status | Condition | Terminal event |
| --- | --- | --- |
| `completed` | clean shutdown, no run error | `run.completed` |
| `partial` | `context.Cause(ctx)` is `context.DeadlineExceeded` (scenario `execution.timeout`), or the evidence budget was reached | `run.failed` (`run.completed` when only the budget was reached) |
| `cancelled` | the context was cancelled for another reason | `run.cancelled` |
| `failed` | the Collector reported an error while running | `run.failed` |

- `context.Cause` is required: `context.WithTimeout` reports
  `DeadlineExceeded` as the cause, plain cancellation reports `Canceled`.
- Inputs are injected one by one, each after its `after` wait; injection
  stops when the context is done, and `inputs[].injected` says which inputs
  were submitted. After the last input (and the mock providers' drive) the
  run waits its settle, then shuts the Collector down
  ([scenarios-and-api.md](scenarios-and-api.md#timeline)). A context that
  ends during a wait makes the run `partial` or `cancelled` like any other.
- Cancellation still shuts the Collector down, so every started processor
  records `component.stopped` (`TestCancelledRunShutsDownCleanly`).
- Only `completed` makes missing stage output final; analysis reports
  `unresolved` otherwise (`TestExpiredRunIsPartial`).
- `Options.Observer` receives each event as it is recorded, under the
  recorder lock, so it must return quickly (hook for live streaming).

## Pipeline order is not deterministic

`graph.StartAll` orders nodes with `topo.SortStabilized` (receivers last,
then by node ID), so startup events are stable. Two other orders follow map
iteration and change between runs:

- **Fan-out.** A receiver used by several pipelines delivers through a
  `fanoutconsumer` whose consumer list comes from
  `componentGraph.From(nodeID)` (gonum, map-backed). Each input is processed
  by the pipelines one after another, in an order fixed for the run but
  different between runs. Observed with `testdata/multisignal/fanin.yaml`
  (`traces/all` and `traces/web` swap).
- **Shutdown.** `graph.ShutdownAll` uses plain `topo.Sort`: independent
  pipelines stop, and their batch processors flush, in varying order.
  Observed with `testdata/multisignal/config.yaml` (traces and logs swap).

Both shift every subsequent `seq`, and pipelines sharing an exporter deliver
to it in varying order.

`Result.Canonicalize` fixes this for normalized output only. It splits the
log after startup into one segment per `input.injected` plus the shutdown
phase (from the first `component.stopped`), stable-sorts each segment by
(signal, pipeline), and attributes pipeline-less events (injections,
exporter captures) to the preceding event of their signal. It then
renumbers `seq` and `id` and remaps `input_seq` and `from_seq`. Exporter
outputs get their resource entries sorted by content
(`telemetry.SortResources`).

Connectors deliver into their pipelines in map order too. A connector's
input belongs to the pipeline that delivered to it. A pipeline a connector
feeds sorts after the pipeline that fed it (key: signal of the root
pipeline, root, connector depth, pipeline), so every delivery still precedes
what the receiving pipeline did with it, whatever the pipeline names
(`TestCanonicalizeKeepsConnectorDeliveriesFirst`). A pipeline's exporter
fan-out also calls its exporters (including connectors) in varying order:
adjacent exporter captures and connector inputs with the same `from_seq`
are ordered by component name. Metrics a component builds from maps (the
count connector) are sorted with `telemetry.SortMetrics`, in outputs and in
snapshots ([aggregation.md](aggregation.md)); an event's record object
numbers move with its sorted data points, a fanned-out emission's objects go
to its canonically last delivery, and all object numbers are renumbered in
first-seen canonical order (`TestCanonicalizeReordersObjectsWithSortedPoints`,
`TestCanonicalizeGivesFanOutObjectsToOneBranch`,
[record-objects.md](record-objects.md)). tail_sampling releases
the traces it decides in one batch in map order: each `stage.decision`
with the events that follow it in its pipeline group is ordered by trace
ID (`TestCanonicalizeOrdersSamplingDecisionsByTrace`; contrib v0.162.0,
[sampling.md](sampling.md)). The routing and count
samples' normalized reports are byte-identical across runs
(`TestExecuteNormalizedReportsAreIdentical`).

The engine canonicalizes before stage analysis, so stage and OTTL seqs match
the event log. The result is a valid linearization, not the observed order,
which is why raw (non-normalized) reports keep the observed order. A batch
processor flushing on its own timer during the run is timing-dependent and is
not canonicalized.

## Normalized reports

`Request.Normalize` / `--normalize` canonicalizes both runs and then
`Report.Normalize` replaces run IDs (`run-normalized`), event IDs and times.
Repeated executions of a deterministic scenario then encode byte-identically
(`TestExecuteNormalizedReportsAreIdentical`, with the `testdata/multisignal/`
scenario and the shared-exporter `fanin.yaml`; both cases fail without
canonicalization). Runs with a mocked receiver additionally clear log observed
timestamps and sort attributes ([mocking.md](mocking.md#normalized-reports)).
Runs with rebased input timestamps have the shift undone
([scenarios-and-api.md](scenarios-and-api.md#timestamps)). Golden artifacts are
generated with `--normalize`.

Non-normalized reports keep exporter output as delivered, so
`runresult.Compare` ignores record order and grouping (see
[reference-runs.md](reference-runs.md)).

## Empty signals

Empty-signal evidence (Collector `v0.162.0`, pdata `v1.68.0`, verified 2026-10-12):
pdata emits `{}` for completely empty signals. The debugger records the explicit
empty `resourceSpans`, `resourceMetrics` or `resourceLogs` array so snapshots stay
independently decodable. Reference OTLP exporters suppress such empty batches;
comparison treats those calls as no delivered telemetry. Resource/scope metadata
is preserved even when there are no records. Both quick and scenario regressions
cover traces, metrics and logs.

## Provenance

`Report.provenance` records what reproducing a run needs: debugger and Collector
versions (`release.ProductVersion`, set from the release tag at link time, and
`CollectorVersion`, tested against the OCB manifest `dist` section), the
component module list, feature gates whose state differs from their stage
default (alpha and deprecated default off, beta and stable on), SHA-256 of the
config and the fixture or scenario file, environment variable names (never
values, which may be secrets) and the settings `receiver`, `timeout` and
`compare`. Provenance is not affected by normalization; it is deterministic
already. Platform preparation captures root source bytes before admission. These
hashes use the same captured bytes that execution decodes, rather than rereading
paths during execution. Platform `Run.bundle` identifies the complete private
capture, including config-provider retrievals and declared input/mock
dependencies, separately from report provenance. Admission stores these bytes in
the repository before acceptance. Its reproducibility remains `unverified` until
runtime prerequisites are classified; see [execution
bundles](execution-bundles.md).
