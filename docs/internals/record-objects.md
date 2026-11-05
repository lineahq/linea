# Record objects

Verified against pdata `v1.68.0` and Collector `v0.162.0` (2026-10-13). Code:
`internal/debugger/telemetry/objects.go`,
`internal/debugger/instrument/recorder.go`,
`internal/debugger/runresult/canonical.go`.

## Why

Content matching cannot follow a record across a stage that changes the
fields records are told apart by (see [stage-analysis.md](stage-analysis.md)):
twenty logs sharing one timestamp and body are indistinguishable once a
processor hashes the attribute that differed. Most processors change records
in place, so the record a stage passes on is the same pdata object it
received. The recorder numbers those objects; nothing is added to the
telemetry.

## pdata layout

- A record wrapper (`plog.LogRecord`, `ptrace.Span`, `pmetric.NumberDataPoint`,
  `HistogramDataPoint`, `ExponentialHistogramDataPoint`, `SummaryDataPoint`)
  is a struct whose first field is the unexported `orig`, a pointer to the
  record's proto object. A record slice's first field `orig` is a pointer to
  a slice of pointers (`*[]*LogRecord`).
- `telemetry.Objects` reads that pointer with `unsafe`, in `telemetry.Records`
  order. It refuses (returns false) unless every wrapper and slice has this
  layout, checked once by reflection; `TestRecordWrapperLayoutIsVerified`
  fails when a pdata upgrade changes it.

## What keeps an object and what does not

| Operation | Objects |
| --- | --- |
| Changing fields, attributes or body in place | kept |
| `RemoveIf` | the kept records' pointers move; removed objects are `Reset` (returned to a pool only with the pooling gate) |
| `MoveAndAppendTo`, `Sort` | pointers move, objects kept |
| `CopyTo`, `Clone` | copies; into an empty destination they are new objects, into a non-empty one the destination's existing objects are overwritten and reused |
| `AppendEmpty` | a new object |

Observed with the real processors (`TestStagesInPlaceKeepRecordObjects`,
`TestCopyingStageGivesNewObjects`): `attributes` (hash), `transform`
(`set(log.body, ...)`), `filter` (the removed records' objects are absent from
its output) and `batch` (two inputs merged into one output) keep every passed
record's object; `groupbyattrs` copies records into new resource groups, so its
outputs are new objects.

The Collector's fan-out consumer (`internal/fanoutconsumer`) clones the data
for every mutating consumer except the last, and lets read-only consumers
share the original. So a branch may receive new objects, and several
read-only branches can hold the same objects.

## Numbering

- `Recorder.RecordTelemetry` numbers the objects of every telemetry event
  (injection, mocked receiver output, stage input and output, connector
  delivery, exporter capture, sampling decision) in `Event.Objects` (since event
  schema 0.5), from 1 per run, in first-seen order. Numbers are taken only
  when the event is kept by the evidence budget, and the JSON is charged to
  it like the rest of the event.
- The recorder keeps each numbered object reachable until the run ends
  (`ReleaseObjects` after the terminal lifecycle event), so a removed record's
  memory cannot hold another record during the run. Events recorded after the
  release carry no numbers.
- With the alpha feature gate `pdata.useProtoPooling` enabled, removed objects
  return to a pool and may be reused by other records, so nothing is numbered
  (`TestPoolingTurnsObjectsOff`).
- Normalized reports: canonicalization reorders events and sorts metric
  snapshots. Data point numbers move with their points
  (`telemetry.SortMetricsOrder`). Which of several mutating branches keeps a
  fanned-out emission's objects varies between runs (map order), so where
  exactly one delivery holds them, they are given to the canonically last
  delivery by swapping its numbers in every later event; a clone is the same
  records in the same order, so this is a valid assignment, not necessarily
  the observed one. Then all numbers are renumbered in first-seen canonical
  order, so repeated normalized runs stay byte-identical
  (`TestExecuteNormalizedReportsAreIdentical`, the shared-exporter case
  fanning one input out to two pipelines).

## Analysis

`internal/debugger/analysis` reads the numbers as its first matching tier (see
[stage-analysis.md](stage-analysis.md)): across a stage, a record before and
a record after with the same number are the same record. Stage diffs,
deferred reconciliations and lineage all use it; spans keep their ID tier
first. An unchanged object match counts as `exact`; a changed one is
confidence `object`, which the web client shows as "same record". Events
without numbers match by content exactly as before (report schema 0.7).

## Limits

- A component that overwrites an existing record object with another record's
  content (a `CopyTo` into a non-empty slice) is followed by object, not by
  content.
- A component that copies records (`groupbyattrs`, a cloning fan-out branch)
  breaks the object chain; matching then falls back to content.
