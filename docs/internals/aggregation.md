# Aggregation (count connector)

Verified against contrib `connector/countconnector` `v0.162.0` (2026-10-10).
Code: `internal/debugger/aggregation/`,
`internal/debugger/analysis/aggregation.go`,
`internal/debugger/analysis/lineage.go`.

## What the connector does

- It consumes logs, traces (spans and span events) or metrics (metrics and
  data points) and emits delta, monotonic, integer sum metrics on a metrics
  pipeline. With no metrics configured it uses the defaults
  (`log.record.count`, `trace.span.count`, `trace.span.event.count`,
  `metric.count`, `metric.datapoint.count`).
- Per input resource with anything counted it emits one output resource
  (attributes copied) with one scope named
  `github.com/open-telemetry/opentelemetry-collector-contrib/connector/countconnector`.
- An item is counted by a metric when any of the metric's `conditions`
  matches (no conditions: always). Each configured attribute resolves from
  the item's attributes, then the scope's, then the resource's, then
  `default_value`. An item lacking a configured attribute with no default is
  skipped by that metric.
- **Order is random.** Metrics and data points come from map iteration, so
  their order differs between runs (see normalization below).
- Timestamps come from the counted items; with none it uses `time.Now()`.
- A condition that errors (for example `ParseJSON` on a non-JSON body)
  makes the connector return the error and emit nothing for the payload.

## Replay and verification

`aggregation.Count` re-evaluates the connector's resolved configuration on
the recorded `stage.input` with the upstream OTTL parsers (the standard and
`xottlfuncs` converters, the same contexts and condition converter as the
connector) and tallies each item into a data point key
(`metric`, attribute fingerprint) per resource. It then compares that with
the connector's recorded delivery:

- `replay_verified`: same resources, keys and values. Each
  `AggregatedPoint.records` names the input positions counted
  (`…log_record[k]`, `…span[k]`, `…span[k].event[e]`, `…metric[m]`,
  `…metric[m].data_point[d]`), and `uncounted` lists items no metric counted.
- `replay_differs`: points are listed without records, with a note.
- `unsupported`: a condition the replay cannot parse; nothing is attributed.

When the connector emitted nothing (a condition error), the aggregation has
no `output` and a note. The engine explains every count connector input
with outcome `emitted` or `no_output` (`Report.aggregations`).

Tests: `TestCountReplayMatchesTheConnector` runs the real connector factory
on fixtures for default logs, traces with events and metrics, conditions
with attributes and defaults, span attributes, data point conditions and a
condition error; `TestCountAttributesRecordsToPoints` and
`TestCountReportsADifferingOutput`.

## Lineage across an aggregation

- A connector stage is keyed by its *input* signal (the connector event's
  `stage.input`), so a logs → metrics connector's outputs belong to the
  same stage as its input. Before this the outputs keyed a separate
  `metrics` stage and the input looked like `no_output`.
- A record reaching an aggregating connector ends `aggregated` with
  `into`: the IDs of the data point lineages it was counted into, or
  `uncounted`. With an unverified aggregation it ends `aggregated` with a
  note and no `into`; with nothing emitted, `dropped`.
- Each verified data point gets a lineage of its own: ID
  `<delivery seq>:<point ref>`, `origin: aggregated`, `connector`, and
  `sources` (the lineages counted into it). Its first hop is the connector's
  `stage.input`; it is then followed like any record through every pipeline
  the connector delivered into.
- A connector that changes signal without an aggregation (one the debugger
  does not replay) ends the record `unresolved` with a note.

Tests: `TestLineageEndsRecordsAtAnAggregation` and, against the reference
Collector, `TestExecuteAttributesRecordsToCountedDataPoints` on
`samples/log-metrics`.

## Normalization

`telemetry.SortMetrics` orders metrics per scope (name, type, unit,
description, then their points) and data points per metric (attribute
fingerprint, timestamps, value). `Result.Canonicalize` applies it to
exporter outputs and to every metrics snapshot, re-keying changed snapshots
by their new content (`sha256:`), before analysis, so record positions in
normalized reports refer to the sorted order.
