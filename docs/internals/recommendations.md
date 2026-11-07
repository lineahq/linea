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
| `confidence` | `static` (the configuration alone), `observed` (this run's events). A verification (below) gives `verified_on_input` (the changed configuration gave the same output on this run's input) or `differs_on_input` (it changed the output); the stored report keeps the rule's own confidence. |
| `pipeline` | The pipeline it is about; empty when it concerns the whole configuration or a processor shared by several pipelines. |
| `components` | Topology refs (`processor/filter/health`). |
| `summary` | One self-contained sentence or two, with the counts. |
| `evidence` | `{section, ref}` pointers into the report: `section` names a report field (`topology`, `stages`, `filtering`, `ottl`, `batching`, `grouping`, `sampling`); `ref` is a component ref (or `pipeline/<id>` in `topology`), with `#condition[i]`, `#group[g].statement[s]`, `#trace[<id>]` or `#policy[<name>]` (a tail_sampling policy by its configured name) for an item inside it. |
| `benefit` | `{metric, count}`: what the run shows the change saves. Metrics: `evaluations_without_effect`, `records_without_effect`, `records_not_processed`, `resource_blocks_merged` (resource blocks the batch sent that a regrouping after it would merge into others). |
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
| `filter.after_batch` | ordering | a filter that dropped records runs after a batch processor in the same pipeline | `swap` moving the filter before the batch; a blocker names the first processor between them the filter's placement checks (below) do not clear |
| `filter.move_earlier` | ordering | a filter that dropped records in a logs or traces pipeline gives every compared record the same verdict at the input of the processors before it (placement checks, below) | `swap` moving the filter before the nearest processors that cleared; the first processor that did not is a blocker |
| `ottl.statement_never_matched` | redundancy | a transform statement's where clause was false on every item it was evaluated on | `remove_statement` |
| `ottl.statement_no_change` | redundancy | a statement matched items and changed none (`matched_without_change` equals `matched`) | `remove_statement` |
| `ottl.duplicate_statement` | redundancy | as above, for a statement with the same text as the one before it in its group | `remove_statement` |
| `attribute.dead_write` | redundancy | a processor set an attribute on records that a later processor in the same pipeline removed or set again on every one of them, with nothing between reading it (dead writes, below) | none |
| `processor.no_effect` | redundancy | a synchronous processor that ran for real emitted every input with an empty diff | none |
| `grouping.no_effect` | redundancy | groupbyattrs moved no attribute and merged no resource | none |
| `batch.size_never_reached` | inefficiency | a batch processor with `send_batch_size` sent only on its timeout | none |
| `batch.tiny_batchers` | inefficiency | a batch processor's `metadata_keys` split its arrivals into two or more batchers that sent fewer items per send than one batcher replayed on the same arrivals would have | none; a blocker leaves `metadata_keys` to the user |
| `batch.drops_client_context` | risk | (static) a component after a batch processor is configured to read from the request's client context (the client address, auth data, gRPC metadata, or a metadata key the batch's `metadata_keys` does not list), which the batch does not pass on; one per reader | `set_option metadata_keys` (the existing keys plus the read ones) when only metadata keys are lost and one batch precedes the reader; else a `swap` moving a processor reader before the first batch when only batch processors sit between them; otherwise a blocker |
| `grouping.after_batch` | ordering | groupbyattrs runs before a batch processor. Observed when the two are adjacent and the batch's sends repeat resources (`Resources` > `DistinctResources`), with benefit `resource_blocks_merged` (their difference); static otherwise, with what the run could not show in `coverage` | `swap` moving groupbyattrs right after the batch |
| `sampling.late_spans` | risk | spans reached tail_sampling after their trace's decision | `set_option decision_wait`, value left to the user |
| `sampling.policy_never_decides` | redundancy | a tail_sampling policy, evaluated alone, was `not_sampled` on every trace whose replay is verified | none |
| `sampling.policy_shadowed` | redundancy | a policy sampled verified traces that another policy, whose result depends on the trace alone, sampled on its own on every verified trace | none |
| `sampling.late_in_pipeline` | inefficiency | processors before tail_sampling handled spans of traces it did not keep; one per processor (not memory_limiter, stand-ins or mocks) | none: moving tail_sampling changes what its policies see |

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
- `sampling.late_spans` cites at most ten traces as evidence; so do the other
  sampling rules. `sampling.policy_never_decides` also cites its policy and
  `sampling.policy_shadowed` both policies (`#policy[<name>]`).
- `batch.tiny_batchers` replays the known batchers' recorded arrival offsets
  through one batcher with the processor's settings (the batchprocessor
  v0.162.0 loop in `batching.md`), so it applies no threshold; it stays
  silent without offsets (normalized reports). Batchers are named by index:
  their metadata values are request headers.
- `batch.drops_client_context` reads what each component after the first
  batch processor takes from the client context from its configuration
  (`contextLosses` in `internal/debugger/engine/clientcontext.go`). Verified
  against contrib v0.162.0 and k8sattributes v1.1.0 (source, 2026-11-07):
  - batchprocessor's `newShard` sends with
    `client.NewContext(context.Background(), ...)` holding only the
    `metadata_keys` values, lowercased; the client address, auth data and
    gRPC metadata are gone behind it, and so is every other metadata key.
    A read is kept only when it is a metadata key the batch lists
    (case-insensitively); behind chained batches the first that loses a
    read is named.
  - `attributes` and `resource` actions with `from_context`:
    `client.address`, `auth.<key>`, `metadata.<key>`, or a bare key, which
    reads metadata (`coreinternal/attraction`).
  - `transform`, `filter` and the `routing` connector: OTTL paths
    `otelcol.client.addr`, `otelcol.client.auth[.attributes["k"]]`,
    `otelcol.client.metadata["k"]` and `otelcol.grpc.metadata["k"]`, found in
    the configured strings; a routing route with `context: otelcol` may omit
    the prefix, and one with the deprecated `context: request` reads
    `request["k"]` from gRPC metadata or client metadata.
  - `k8sattributes` reads the connection address when `pod_association` is
    empty or a source is `from: connection` (`extractPodID`).
  - An exporter whose `auth.authenticator` is a `headers_setter` extension:
    each header's `from_context` reads metadata, `from_attribute` an auth
    attribute.

  Header names appear in summaries; their values never do. Whether requests
  carry these values depends on the receivers (`include_metadata`) and the
  clients, which `coverage` says.
- `grouping.after_batch` counts exactly: groupbyattrs' output has no schema
  URLs and no grouping keys left on its records, so the resource blocks a
  send of the batch right behind it holds are those grouping would merge,
  and its distinct resources what is left after merging.
- The sampling rules rest on tail_sampling v0.162.0's decision
  (`evaluateDropPolicies`, `makeDecision`, verified 2026-11-07): a drop
  policy drops whatever the others decide, any other sample keeps the trace,
  and no other policy can veto a sample. So a non-drop policy changes no
  decision on a trace another policy samples alone. Shadowers are restricted
  to policy types whose result depends on the trace alone (not
  `rate_limiting`, `bytes_limiting`, `composite`, `and`, `not`, `drop`),
  preferring `always_sample`. Policies are matched to their results by name,
  so policies sharing a name are skipped. `sampling_strategy: span-ingest`
  is not in the report, so the rules cannot tell it apart.
- `sampling.late_in_pipeline` counts the spans of unkept traces that reached
  tail_sampling plus those removed on the way (stage diffs' removed spans
  name their trace); a stage on the way without diffs, such as batch, makes
  it a lower bound ("at least"), and a stage that added spans leaves the
  processor out.

## Filter placement checks

The engine (`placeFilters` in `internal/debugger/engine/placement.go`)
computes, for each filter that is not first in a logs or traces pipeline and
dropped records in requests whose replay is verified, whether it could run
before each processor in front of it, nearest first, stopping at the first it
cannot. `recommend.Placement` carries the result to the rules, which only
read it. A processor blocks the move when:

- the filter's error mode is `propagate`, or its conditions cannot be listed
  for their reads (`filterreplay.Reads`; see `filtering.md`);
- the processor did not run for real (stand-in or mock), is a
  `memory_limiter` (it guards the pipeline), reads the environment
  (`k8sattributes`, `resourcedetection`), or is a type whose per-record effect
  is not known (anything but `transform`, `attributes`, `resource`,
  `redaction`, `filter`, `span`, `probabilistic_sampler`);
- its configuration may write a field the filter's conditions read, at the
  same level, with the same attribute key unless either side names none
  (`configuredAccess` in `internal/debugger/engine/access.go`). This holds
  whatever this input showed: an `insert` of a key every record already
  had, or a statement whose where clause matched nothing, changes nothing
  here and may on the next input. `resource` and `attributes` write their
  actions' keys (a `pattern` or `extract` action, any key); `transform`
  writes the paths its editors take as setters (`ottlreplay.Access`:
  statements parsed with editors that hand over their arguments, so a
  `delete_key(log.attributes, "k")` writes all of `attributes`); `span`,
  `redaction` and `probabilistic_sampler` count as writing every field they
  may touch; `filter` writes nothing. A configuration whose writes cannot be
  listed blocks the move;
- a request reached its input with an error;
- a record the filter evaluated cannot be followed back through the
  processor by lineage at high confidence;
- the processor changed, on a record the filter evaluated, a field the
  filter's conditions read (resource, scope or record level, attribute key);
- evaluating the filter (`filterreplay.Verdict`) on the processor's input
  gives any record a different verdict than the recorded one.

The configuration check keeps a move from resting on what this input
happened to exercise; the change check after it catches a processor whose
listed writes are wrong; the verdict check catches reads the listing cannot
see. Coverage says how
many records and requests were compared: another input may hold records whose
verdict depends on what the skipped processors change.

## Dead writes

The engine (`deadWrites` in `internal/debugger/engine/deadwrites.go`) reads
lineage: for each processor output hop where an attribute key was added or
modified (at the resource, scope or record, by the change's entity), it
follows the record on through the same pipeline. `recommend.DeadWrite`
carries the result to `attribute.dead_write`. On one record the write is:

- live when a processor on the way may read the key by its configuration
  (`configuredAccess`: `transform` reads every path its statements and
  group conditions name that is not a setter target; `attributes` and
  `resource` read the keys of insert, update, hash, extract and convert
  actions and any `from_attribute`; `filter` its conditions' reads; a type
  or configuration that cannot be listed reads everything), or when the
  record leaves the pipeline with it;
- dead when a later processor's output removes or modifies the key first;
- neither when the record was dropped on the way;
- unknown when a hop on the way was matched only by content similarity
  (medium confidence) or the path ends unresolved.

A write is reported when it was dead on at least one record, live and
unknown on none, and one processor undid it every time. No change is
suggested: whether the value was meant to reach the output decides between
removing the write and fixing the later processor.

## Candidate verification

Verification is on demand: each one is one more run, so nothing verifies by
default. `platform.VerifyRecommendation` takes a finished run and the
position of one of its recommendations:

1. `engine.CandidateOverrides` expresses the change as provider overrides.
   Only `swap` is verifiable: one `yaml:service::pipelines::<id>::processors:`
   URI with the new list as JSON (a list replaces the list). Any other change,
   or none, is `invalid_request` before anything runs.
2. `engine.CandidateRequest` reads the run's captured bundle and layers the
   overrides on its configuration snapshot (`Snapshot.WithOverrides`,
   snapshot schema 4; `config-resolution.md`). Inputs, setup, limits and
   environment come from the bundle, never from the host; the user's
   configuration and its digest are unchanged.
3. The candidate is an ordinary run in the run's mode, bound by the run's
   runtime name; if it binds another build (a project default changed), it is
   cancelled and the verification is a `conflict`.
4. It waits for the candidate (ending the request cancels it) and compares
   the run (before) with it (`CompareRuns`): `same` gives
   `verified_on_input`, `different` gives `differs_on_input`, `inconclusive`
   no confidence. A failed candidate returns its error and no comparison.

`samples/logs-cleanup` verifies (`filter.move_earlier`, same output);
`samples/filter-order` differs: its filter reads an attribute transform/tag
sets, so `filter.after_batch`, which moves it before batch and so before
transform/tag (the blocker says so), drops nothing in the candidate
(`TestVerifyRecommendationReportsAChangedOutput`).

The result (`RecommendationVerification`) names the run, the recommendation,
its rule, the overrides, the candidate run and the comparison. Nothing links
the two runs in storage: the candidate's snapshot records its overrides, and
the stored report of the run is not changed.

## Interfaces

- CLI: `linea run` and `linea inspect` print a `recommendations:` section
  (`writeRecommendations` in `cmd/linea/text.go`), one line per
  recommendation with its change, savings, limits and coverage.
- Web: the run's Recommendations tab lists them
  (`web/src/debugger/recommendations.ts`, tested), each with a link to the
  components it names; the Overview shows only a one-line hint that opens
  the tab.
- Verification: the web card's "Verify on this input" (`web-client.md`);
  `linea run --verify-recommendations` (local or `--server`)
  verifies every recommendation with a change one after another, prints the
  raised confidences in the report and a `verifications:` section (on stderr
  beside `--format json`); `POST /runs/{id}/recommendations/{n}/verify`; MCP
  `linea_verify_recommendation`.
- MCP and HTTP: the `recommendations` evidence family of
  `linea_get_execution_trace` and `GET /runs/{id}/evidence`, optionally
  filtered by `pipeline` (keeping configuration-wide recommendations) and
  `component` (a configuration ID the recommendation names). Item refs are
  `recommendation:<n>`, the position in `report.recommendations`.
