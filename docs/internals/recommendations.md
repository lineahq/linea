# Recommendations

`internal/debugger/recommend` turns a report's recorded evidence into
reviewable suggestions: work the run shows was avoidable, settings the input
never exercised, and orderings that risk data. It reads the report's
sections and nothing else. It never runs the Collector and never changes the
user's configuration: a suggested change is data the user may apply to their
own file.

## Shape

`Recommend(Input)` applies every rule and returns `report.recommendations`
(report schema 0.15). `Static(graph)` applies the same rules to the topology
alone; `linea inspect` and `InspectConfig` return those as
`Inspection.recommendations` (inspection schema 0.3). Rules whose sections are
absent produce nothing, so only the configuration rules fire without a run.

A recommendation has:

| Field | Meaning |
| --- | --- |
| `rule` | Stable rule ID, such as `filter.condition_never_matched`. |
| `kind` | `ordering`, `redundancy`, `inefficiency`, `lossy` or `risk`. |
| `confidence` | `static` (the configuration alone), `observed` (this run's events). `verified_on_input` and `differs_on_input` are reserved for a later phase that reruns the changed configuration on the same input. |
| `pipeline` | The pipeline it is about; empty when it concerns the whole configuration or a processor shared by several pipelines. |
| `components` | Topology refs (`processor/filter/health`). |
| `summary` | One self-contained sentence or two, with the counts. |
| `evidence` | `{section, ref}` pointers into the report: `section` names a report field (`topology`, `stages`, `filtering`, `ottl`, `batching`, `grouping`, `sampling`); `ref` is a component ref (or `pipeline/<id>` in `topology`), with `#condition[i]`, `#group[g].statement[s]` or `#trace[<id>]` for an item inside it. |
| `benefit` | `{metric, count}`: what the run shows the change saves. Metrics: `evaluations_without_effect`, `records_without_effect`, `records_not_processed`. |
| `change` | The suggested change, when one follows from the evidence: `swap` (`processors`, the pipeline's new processor list), `remove_statement` (`component`, `index`, and `group` for a transform statement; `text`), `set_option` (`component`, `option`, and `value` when the evidence decides it; empty leaves it to the user). |
| `blockers` | Why a stronger suggestion was not made. |
| `coverage` | What the input did not exercise. |

Recommendations are listed in catalog order (below), and within a rule by
pipeline and components. They are derived analysis: a stored report cut for
size drops them with the other omitted sections (`withoutAnalysis`), and the
`recommendations` evidence family then answers `analysis_omitted`.

No rule suggests removing a whole processor. `processor.no_effect`,
`filter.no_effect`, `grouping.no_effect` and `config.unused_component` carry
no change: one input says too little about whether a component is needed.

## Rules

Observed rules count only what the report verified: filter requests whose
replay is `replay_verified`, transform replays that are `replay_verified`
with groups `replayed`. Requests that were not verified are left out and named
in `coverage`.

| Rule | Kind | Fires when | Change |
| --- | --- | --- | --- |
| `config.memory_limiter_not_first` | risk | a memory_limiter is not the first processor of a pipeline | `swap` moving it first |
| `config.unused_component` | redundancy | a component is in `topology.unused` | none |
| `batch.chained` | redundancy | a pipeline has two or more batch processors | none |
| `filter.no_effect` | redundancy | a filter processor received records, dropped none and no request failed | none |
| `filter.condition_never_matched` | redundancy | in verified requests of a filter that dropped something, a condition was evaluated and never matched, without errors | `remove_statement` of that condition |
| `filter.after_batch` | ordering | a filter that dropped records runs after a batch processor in the same pipeline | `swap` moving the filter before the batch; processors between them become a blocker, since moving it may change what they see |
| `ottl.statement_never_matched` | redundancy | a transform statement's where clause was false on every item it was evaluated on | `remove_statement` |
| `ottl.statement_no_change` | redundancy | a statement matched items and changed none (`matched_without_change` equals `matched`) | `remove_statement` |
| `ottl.duplicate_statement` | redundancy | as above, for a statement with the same text as the one before it in its group | `remove_statement` |
| `processor.no_effect` | redundancy | a synchronous processor that ran for real emitted every input with an empty diff | none |
| `grouping.no_effect` | redundancy | groupbyattrs moved no attribute and merged no resource | none |
| `batch.size_never_reached` | inefficiency | a batch processor with `send_batch_size` sent only on its timeout | none |
| `sampling.late_spans` | risk | spans reached tail_sampling after their trace's decision | `set_option decision_wait`, value left to the user |

Details the rules hold to:

- A transform or filter shared by several pipelines is one recommendation
  with an empty `pipeline`, its counts summed over the pipelines.
- Never-matched summaries say that if the input holds items the statement or
  condition should act on, it is wrong rather than unneeded: a broken where
  clause looks the same as an unneeded one.
- `ottl.statement_no_change` and `ottl.duplicate_statement` skip statements
  that write `cache[...]`, which change no item by design.
- `processor.no_effect` leaves out types that pass data on unchanged by
  design or cannot act in a debug run (`batch`, `memory_limiter`,
  `k8sattributes`, `resourcedetection`) and types with rules of their own
  (`filter`, `transform`, `groupbyattrs`, `tail_sampling`), and stand-in or
  mocked components.
- `batch.size_never_reached` applies no threshold. When the arrivals span
  time, it states the input's rate, how long `send_batch_size` takes to fill
  at that rate and the average batch the timeout sent. When everything
  arrived at once, it says the rate cannot decide between the settings.
- `sampling.late_spans` cites at most ten traces as evidence.

## Interfaces

- CLI: `linea run` and `linea inspect` print a `recommendations:` section
  (`writeRecommendations` in `cmd/linea/text.go`), one line per
  recommendation with its change, savings, limits and coverage.
- Web: the run overview lists them under "Recommendations"
  (`web/src/debugger/recommendations.ts`, tested), each with a link to the
  components it names.
- MCP and HTTP: the `recommendations` evidence family of
  `linea_get_execution_trace` and `GET /runs/{id}/evidence`, optionally
  filtered by `pipeline` (keeping configuration-wide recommendations) and
  `component` (a configuration ID the recommendation names). Item refs are
  `recommendation:<n>`, the position in `report.recommendations`.
