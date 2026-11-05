# OTTL replay

Verified against `pkg/ottl` and `transformprocessor` `v0.162.0` (2026-10-09;
logs and metrics 2026-10-10). Code: `internal/debugger/ottlreplay/` (`replay.go`,
`runners.go`; the report types are in `internal/debugger/ottldebug/`).

## How the transform processor runs statements

From `transformprocessor/internal/{common,traces,logs,metrics}`:

- Each `trace_statements`, `log_statements` or `metric_statements` group is
  parsed by an `ottl.ParserCollection`. With an explicit `context`, the
  processor calls `ParseStatementsWithContext(ctx, statements, true)`, which
  **rewrites** statements so that paths carry the context name
  (`delete_key(attributes, ...)` becomes `delete_key(span.attributes, ...)`).
  Without a `context`, it calls `ParseStatements` with context inference.
- Groups run in configuration order over the whole batch. A span group runs per
  span: first the group `conditions`, then every statement in order on that
  span, then the next span. Resource and scope groups do the same per resource
  and per scope.
- Error modes (`ottl.StatementSequence.Execute`): `ignore` logs and continues
  with the next statement, `silent` continues without logging, `propagate` stops
  and returns the error. The processor then returns the error, the batch is not
  forwarded and the caller sees the failure. A group's `error_mode` overrides
  the processor's.
- `Statement.Execute` returns `(result, conditionMatched, err)`. An error with
  `conditionMatched=false` came from the where clause; with `true` it came from
  the function.
- Without `shared_cache`, every entity gets a fresh cache.
- Each signal has its own parser collection with only its contexts: traces
  resource, scope, span (and spanevent); logs resource, scope, log; metrics
  resource, scope, metric, datapoint, exemplar. Context inference only chooses
  among the signal's contexts.
- Iteration order is resource, scope, item. Logs, metrics and data points are
  iterated **by index with the length re-read each step**
  (`for k := 0; k < metrics.Len(); k++`), so a metric statement that appends
  metrics (`copy_metric`, `extract_sum_metric`, ...) also runs on the appended
  metrics later in the same group. Exemplars, resources and scopes use `All()`.
- Data point context visits Sum and Gauge number points, Histogram,
  ExponentialHistogram and Summary points. Exemplar context visits the exemplars
  of Sum, Gauge, Histogram and ExponentialHistogram points; Summary points have
  none.
- `flatten_data` (logs, behind the `transform.flatten.logs` feature gate) gives
  every log record its own resource and scope before the statements run and
  regroups them afterwards (`pdatautil.FlattenLogs` / `GroupByResourceLogs`).
- Plain (flat) statement lists are turned into one group without a context
  during `Config.Unmarshal`. Context inference then requires context-prefixed
  paths: `set(attributes["x"], 1)` fails validation, while
  `set(span.attributes["x"], 1)` works.

## Function sets

- Span, log, metric, datapoint, exemplar:
  `transformprocessor.Default{Span, Log,Metric,DataPoint,Exemplar}Functions()`
  are exported and are exactly the processor's sets (converted with
  `ottl.CreateFactoryMap`).
- Resource and scope: the processor uses an internal helper. Replay rebuilds it
  from public packages, through the release variant in
  `internal/debugger/compat/ottl` (`ottlcompat.Functions`; from 0.162 that is
  `xottlfuncs.WithExperimentalConverters(xprofilefuncs.WithProfileConverters(ottlfuncs.StandardFuncs[K]()))`).
- Group `conditions` are compiled with `filterottl` from the contrib
  `internal/filter` module, which cannot be imported. Replay does not
  approximate them; such groups are `unsupported`.

The replay links the same `pkg/ottl` module as the processor, because a Go
build has exactly one version of each module.

## Replay design

1. Take the transform's recorded `stage.input` snapshot (a clone).
2. Parse each group with a parser collection that mirrors the processor's (same
   contexts, functions, rewrite flag and error mode). A custom statement
   converter returns the parsed `[]*ottl.Statement[K]` instead of a
   `StatementSequence`, so statements can be executed one at a time.
3. Iterate entities exactly like the processor. Before each statement, capture
   the entity together with its resource and scope (item statements may write to
   them). After it, diff that state. Entities:
   - span: `span:<trace_id>:<span_id>`;
   - log: `resource[i].scope[j].log_record[k]` (`analysis.DiffLogRecord`);
   - metric: the scope's whole metric list is captured, so appended metrics show
     as `metric[m]` `metric` added (`analysis.DiffMetrics`);
   - datapoint, exemplar: the owning metric is captured and diffed per data
     point (`analysis.DiffMetric`); an exemplar change shows as `exemplars`
     modified on its data point. Record matched / not matched counts, errors
     with their phase, and the changes.
4. Compare the final replay telemetry with the processor's correlated
   `stage.output`, byte for byte:
   - `replay_verified`: equal, or both rejected the input.
   - `disagrees`: different; `mismatch` holds the diff from actual to replay
     (`analysis.DiffPayloads`: span identity, or content matching with
     confidence for logs and metrics). Statement results must not be read as
     attribution then.
   - `not_verifiable`: a group was not replayed, so later groups are
     `not_evaluated` too.

## Verified results

- The `testdata/traces/` config is `replay_verified`: a conditional `set` matched 1 of 3
  spans, `delete_key` matched all 3 but changed only 2 (reported as
  `matched_without_change: 1`), and the rename matched 1
  (`TestReplayExplainsTraceStatements`).
- Logs: resource and log groups (set from a field, delete_key, conditional
  body rewrite, an ignored merge_maps error) are `replay_verified`
  (`TestReplayExplainsLogStatements`); a flat `log_statements` list infers
  the `log` context (`TestReplayInfersLogContext`). The `testdata/multisignal/` config's
  `transform/logs` is `replay_verified` end to end.
- Metrics: `copy_metric` on 1 of 4 metrics yields 5 metric visits (the
  copy is visited); datapoint context visits all 7 points of the 5 metrics
  (including the Summary); exemplar context visits 3 exemplars. All
  `replay_verified` (`TestReplayExplainsMetricStatements`). Error modes
  behave as for traces (`TestReplayRespectsMetricErrorMode`).
- `merge_maps` with a non-map source fails at function phase. With `ignore`
  the next statement still runs on every span. With `propagate`, replay and
  processor both reject the input, and the remaining statements are counted
  `not_evaluated` (`TestReplayRespectsErrorMode`).
- A fabricated "actual" output is detected as `disagrees`
  (`TestReplayReportsDisagreement`).

## Limitations

- `profile_statements`, the `spanevent` context, group `conditions`,
  `shared_cache` and logs `flatten_data` are reported as unsupported; later
  groups are then `not_evaluated`.
- Statement text is the resolved config, so `${env:...}` values appear
  substituted.
- The OTTL debug logs are not captured.

## Evaluation without an observed output (playground)

`ottlreplay.Evaluate` runs the same replay as `ottlreplay.Replay` but has no
processor output to check against. It backs `POST /api/v1/ottl/evaluate`
and the web UI's "Preview on this input".

- The verification is always `not_verifiable`. The note says to rerun to
  verify against the processor.
- When processing stops under `error_mode: propagate`, there is no output,
  because the processor would drop the input. This is the same pairing
  `Replay` treats as verified (both rejected).
- An edited statement that does not parse fails Collector config
  resolution, not the replay. The endpoint reports that as `valid: false`
  with the upstream diagnostic, for example
  `unable to parse OTTL statement ...`.
- Verified against Collector contrib v0.162.0
  (`TestEvaluateMatchesTheProcessor`): for an edit and for a propagate
  error, the evaluation's output equals the real transform processor's.

## Statement groups in the configuration

Verified against `transformprocessor` v0.162.0, `config.go` `Unmarshal`:

- A `<signal>_statements` list of plain strings ("basic" style) becomes a
  single group, index 0, containing those statements.
- A list of maps ("advanced" style) gives one group per item, in order.
- Mixing the two styles is rejected.

The web client uses this to find statement *j* of group *g* in the YAML.
In basic style it is `processors.<id>.<field>[j]`; in advanced style it is
`processors.<id>.<field>[g].statements[j]`.
