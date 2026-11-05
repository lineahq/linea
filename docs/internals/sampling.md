# Tail sampling decisions

Verified against contrib `v0.162.0` (`processor/tailsamplingprocessor`),
2026-10-10. Code: `internal/debugger/instrument/sampling.go` and `instrument/tailsampling`,
`internal/debugger/sampling/`, `internal/debugger/analysis/sampling.go`, web
`src/inspect/sampling.ts` and `SamplingPanel.tsx`.

tail_sampling keeps or drops whole traces by policies evaluated on all the
spans received for a trace, so a value diff of its input and output cannot
say why a record disappeared. The report's `sampling` section does.

## Recording decisions

- `Config.Options` (mapstructure `-`, in-code only) accepts
  `WithSampledHooks` and `WithNonSampledHooks`. A `DecisionHook` receives
  the trace ID and `*TraceData` with `FinalDecision`, `PolicyName` and
  `ReceivedBatches`: exactly the spans the policies evaluated. The options
  **replace** the processor's hook lists; YAML configs have none, so the
  instrumented factory adds one hook to both.
- Hooks fire in `releaseSampledTrace` (before forwarding) and
  `releaseNotSampledTrace`, which also covers `Dropped` (a drop policy
  matched) and traces over `maximum_trace_size_bytes`.
- Each call is recorded as a `stage.decision` event (event schema 0.4)
  with `decision {trace_id, outcome, policy}` and the evaluated spans as
  its snapshot.
- A span arriving after a **not-sampled** decision calls the non-sampled
  hook again, with the batches already released (empty). It is recorded
  without a snapshot: a repeated decision, nothing evaluated. A late span
  of a sampled trace is forwarded without a hook call; one of a dropped
  trace is dropped with a warning log and no hook call.
- Decisions made in one batch are released in map order;
  `Result.Canonicalize` orders each decision with the events that follow
  it in its pipeline group by trace ID (see
  [run-lifecycle.md](run-lifecycle.md)).

## Explaining decisions (`internal/debugger/sampling`)

- The policy evaluators are in `internal/debugger/sampling` of the contrib module and
  cannot be imported. The replay therefore runs the real factory: a copy of
  the configuration with the decision hooks as options, `decision_wait` 1h,
  `drop_pending_traces_on_shutdown` off and no decision cache. It consumes
  the evaluated spans and shuts down: `Shutdown` closes `workChan`, and the
  loop decides every pending trace synchronously before `doneChan` closes,
  so the decision is known without waiting.
- Per decided trace: once with every policy, compared with the recorded
  outcome and deciding policy (`replay_verified`, `replay_differs`,
  `replay_failed`), and once per top-level policy alone, giving that
  policy's own result (`sampled`, `not_sampled`, `dropped`).
- Evidence is computed from the evaluated spans, as each evaluator reads
  them: the latency policy measures the earliest start to the latest end
  across all spans (`>` threshold, `<=` upper threshold); attribute
  policies read span and resource attributes; status codes are counted per
  span.
- Rate and bytes limits and composite budgets depend on all traffic; the
  replay evaluates each trace alone and says so in a note.
- Traces with input spans but no decision are `pending_dropped` when the
  run completed with `drop_pending_traces_on_shutdown`, else `pending`.
- At most 200 traces are replayed per processor; later decisions are listed
  without policy results.
- Precedence at this release: a drop policy beats everything; otherwise
  any sampling policy keeps the trace. With `sample_on_first_match`,
  evaluation stops at the first sampling policy in configuration order.

## Display

The tail_sampling processor has a Decisions tab: per trace the decision,
the deciding policy, late spans, and for each policy column its result on
its own with the evidence (for example `lasted 40ms`,
`span statuses: UNSET`). The Changes tab's note for a buffered input links
to it.

## Decision timing on a scenario timeline

Verified against v0.162.0, 2026-10-16. The processor's one-second ticker
starts with the processor. A trace whose first span arrives just after the
start is decided at the tick `decision_wait` + 1 s later, so with
`decision_wait: 2s` the decision lands at about 3.00 s. A late input
declared `after: 3s` is then injected at about 3.01 s, on that same tick,
so whether its spans arrive before or after the decision depends on
scheduling. This made `TestWorkerReportsEqualTheInProcessEngine` flaky on
the `trace-sampling` sample. A late input meant to follow a decision needs
`after` of at least `decision_wait` + 2 s (`settleFor` uses the same model:
the batches, plus one tick, plus a margin).
