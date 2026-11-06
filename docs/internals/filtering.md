# Filter processor: what drops a record

Verified against contrib `filterprocessor` v0.162.0 and `pkg/ottl` v0.162.0
(source and probe runs, 2026-11-06). Releases 0.160.0 and 0.161.0 differ
only in the transform context constructor names (source diff), which
`internal/debugger/compat/ottl` absorbs. `report.filtering` (below) builds on
this behavior.

## Configuration forms

- `log_conditions`, `trace_conditions`, `metric_conditions`,
  `profile_conditions`: lists of `{context, conditions, error_mode}`
  (`internal/condition.ContextConditions`). A group's `error_mode` overrides
  the processor's. Without `context`, each condition's context is inferred
  on its own from its path prefixes and the group is split by context
  (resource, scope, then the record context). When any of these lists is
  set, the deprecated forms are ignored for that signal.
- Deprecated per-context lists: `logs.resource`, `logs.log_record`,
  `traces.resource`, `traces.span`, `traces.spanevent`, `metrics.resource`,
  `metrics.metric`, `metrics.datapoint`, `profiles.*`. They are parsed with
  the plain context parsers (`filterottl.NewBoolExprFor*`); in a probe run
  a `log.`-prefixed path (`IsMatch(log.body, ...)`) parsed and matched in
  `logs.log_record` too.
- Not OTTL: `logs.include` / `logs.exclude` match properties, `spans`,
  `metrics.include` / `exclude` and the `expr` language. A replay reports
  these as unsupported.
- `error_mode` defaults to `ignore`.

## Functions

The processor's function sets are exported:
`filterprocessor.DefaultResourceFunctions()`, `DefaultLogFunctions()`,
`DefaultSpanFunctions()`, `DefaultSpanEventFunctions()`,
`DefaultMetricFunctions()`, `DefaultDataPointFunctions()`,
`DefaultProfileFunctions()`. They wrap `internal/filter/filterottl`
(standard converters plus filter-only functions such as `HasAttrOnDatapoint`),
which cannot be imported. A replay parses conditions with the public context
parsers and these functions, so it uses exactly the processor's function set.

## Evaluation

- The conditions of one context form an `ottl.ConditionSequence` with
  logic `OR`: the first condition that is true drops the item; later ones
  are not evaluated.
- Order per resource: resource conditions first (true removes the whole
  resource block), then scope conditions (true removes the scope block),
  then record conditions. For spans, span conditions run first and span
  event conditions only on kept spans; a span event condition removes the
  event, never the span. For metrics, a metric condition removes the
  metric; data point conditions remove points, and a metric left with no
  points is removed.
- Empty scope and resource blocks are removed. When nothing is left, the
  processor returns `processorhelper.ErrSkipProcessingData`: the request
  ends there without an error.
- Each group in the `*_conditions` form runs in turn over what the previous
  groups kept.
- Errors: under `ignore` an erroring condition is logged and skipped, and
  the sequence continues; under `silent` the same without a log. Under
  `propagate` the item's sequence stops at the failing condition and
  returns false with the error, so the item is kept; the other items are
  still evaluated, and the processor returns the joined errors, so the
  pipeline drops the whole request.

## Probe runs

One request, resource `a` with logs `healthcheck ok`, `{"a":1}`, `plain`,
and resource `b` with `plain b`; each run with `--compare`, all equal to
the reference Collector:

| Configuration | Output |
| --- | --- |
| `log_conditions` without context: `resource.attributes["service.name"] == "b"`, `IsMatch(log.body, "^healthcheck")` | resource `b` removed whole; `healthcheck ok` dropped |
| `logs.log_record: ParseJSON(body)["a"] == 1`, `ignore` | `{"a":1}` dropped; the other bodies error and are kept |
| the same with `propagate` | no output at all; stage outcome `no_output` with the condition's error |
| `logs.log_record: IsMatch(log.body, "^healthcheck")` | `healthcheck ok` dropped |

## report.filtering

`internal/debugger/filterreplay` (the optional feature `filter_explanation`,
compiled in when the manifest has the filter processor) explains every
filter processor input whose outcome is known: one correlated output, or
none. It parses the configured conditions as the processor does (the
`*_conditions` groups through a parser collection with path context names,
the legacy lists with the plain context parsers, the processor's exported
`Default*Functions` sets and the scope converters), evaluates them in the
processor's order on a copy of the recorded input, and compares the result
with the recorded output by sorted context fingerprints.

- `replay_verified`: the evaluation reproduces the output (or, under
  `propagate`, the error and the missing output). Each removed item is listed
  in `drops` with its input position (`resource[0].scope[0].log_record[2]`)
  and the index of the first true condition. `removed` names the records
  it took with it, from the lineage hops at the processor's input (the
  record's position equals the drop's or lies under it): lineage ID, name
  and position. A span event drop removes no record.
- `replay_differs`: it does not; counts are kept, no drop is attributed.
- `unsupported`: include/exclude match properties, which the replay does not
  evaluate, or conditions that do not parse.

Per condition the entry counts the items it was evaluated on, the items it
removed (`matched`) with their records, and failed evaluations with the
first error. `records` and `dropped` are always the observation: input
records minus output records. The CLI prints the entry under `filtering:`
with up to three removed record names per drop; the web client shows it in
the filter processor's Filter tab, with each removed record opening its
path and, for several requests, each condition's counts summed over the
run's verified requests.
