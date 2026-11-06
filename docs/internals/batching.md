# Batch processor: explaining each send

`report.batching` (report schema 0.15) explains every `batch` processor
instance: when it sent what it held and why. The engine builds it in
`internal/debugger/analysis/batching.go` (`analysis.Batch`), wired from
`internal/debugger/engine/batching.go`. `linea run` prints each send and its
trigger; the web client shows it on the processor's Batches tab
(`web/src/inspect/BatchingPanel.tsx`) and, for two runs, side by side in the
run comparison (`web/src/runs/batchingCompare.ts`). A stored report over its
storage limit drops it with the other derived sections.

## Verified behavior (batchprocessor v0.162.0, source and runs)

- Defaults: `send_batch_size` 8192, `timeout` 200ms, `send_batch_max_size` 0
  (no split), `metadata_cardinality_limit` 1000. Sizes count items (spans,
  log records, data points), not requests or bytes.
- Each request is appended with `MoveAndAppendTo`; equal resources are not
  merged, so a batch can carry more resource blocks than distinct resources.
- After appending, the processor sends while it holds at least
  `send_batch_size` items, each send capped at `send_batch_max_size`; a
  remainder below the size stays pending.
- The timer exists only when `timeout != 0` and `send_batch_size != 0`.
  Without it every request is sent on arrival. The timer is reset after a
  size send and after every tick, so a timer send comes `timeout` after the
  last send or tick, not after the oldest record arrived. In
  `samples/batch-timeline` a log that arrived 500ms after a size send left
  about 100ms later with `timeout: 300ms`.
- Shutdown sends what is pending with the timeout trigger, so
  `otelcol_processor_batch_timeout_trigger_send` counts a shutdown send as a
  timeout.
- `metadata_keys` (`multiShardBatcher`) makes one batcher per combination
  of the listed keys' values in `client.Info.Metadata`. Keys are lowercased
  and sorted; a missing key is a combination of its own. Each batcher has
  its own pending items, size count and timer, which starts when its first
  request arrives (one batcher's timer starts with the processor). Its
  sends carry only the listed keys in their context. Beyond
  `metadata_cardinality_limit` combinations a request is refused with a
  permanent error. In `samples/batch-metadata` acme's two requests filled a
  batch of 4 while globex and the request without a tenant left on their
  own timers; without `include_metadata` the same requests shared one
  batcher (size 4, then a timer send of 3).
- Headers reach `client.Info.Metadata` only with the receiver's
  `include_metadata: true`: confighttp v0.162.0 copies every request header
  and `Host`, configgrpc v1.68.0 every incoming metadata key (`:authority`
  as `host`). `client.NewMetadata` lowercases names and `Get` matches case
  insensitively.

## Scenario headers

- An input's `headers`, merged key by key over the scenario's
  `defaults.headers` (the input wins), are the request headers it arrives
  with. They are honored only for an OTLP receiver: with gRPC configured its
  `include_metadata` decides, else HTTP's; another receiver is an error.
- The instrumented run puts them in the injected request's `client.Info`;
  the reference run sends them as gRPC metadata to the real receiver,
  forcing gRPC `include_metadata` on when only HTTP sets it.
- `result.inputs[].headers` names the headers, never their values;
  `headers_dropped` marks an input whose receiver did not include metadata.

## Inference

- Arrivals are the stage's `stage.input` events and sends its
  `stage.output` events, with offsets since `component.started`.
- The timer is replayed from `component.started`. A send is `size` when the
  pending items reached `send_batch_size`, `timeout` when it falls on a
  replayed tick (tolerance: half the timeout, at most 25ms, for coarse
  clocks), `shutdown` when it is the last send after the last arrival of a
  completed run, and `immediate` without a timer. `split` marks a send of
  exactly `send_batch_max_size` items while more were pending.
- `sources` attribute each send's records to arrivals through lineage (an
  egress hop whose parent is an ingress hop at the same processor); `wait_ms`
  is the spread from arrival to send.
- `verification` compares the inferred size and timeout+shutdown counts with
  the processor's trigger metrics: `metrics_agree`, `metrics_differ`, or
  `unverified` when the run recorded none.
- Each arrival and send lists the `service.name` of its resource blocks in
  order (`services`, "" for a block without one), so the view can say where
  records came from and show a resource repeated inside a batch.
- With `metadata_keys`, each arrival is traced through lineage to the
  scenario inputs it came from, and their headers give its batcher
  (`batcher`, indexing `batchers`). Each batcher replays its own pending
  items and timer; a send belongs to the batcher of its source arrivals.
  `batchers[].metadata` holds the values of the listed keys only. An
  arrival from several inputs with different values, through an earlier
  batch processor, or from an aggregating connector is in a batcher marked
  `unknown`; a combination beyond the limit is `refused`.
- `Report.Normalize` clears offsets and waits and keeps counts and triggers.

## Limits

- A shutdown send is inferred; no event marks the start of shutdown.
- On clocks that stamp a whole send with one time, near-simultaneous events
  are ordered by sequence number.
- Headers come only from scenario inputs; a quick run declares none. The
  web Inputs panel edits them per input and for every input (Run settings),
  any header name, since which ones matter is the configuration's
  `metadata_keys`. Header values sit in the scenario as written, so a
  secret header is better replaced by a placeholder.
- Exporter batching (`sending_queue::batch`) never runs in a debug run,
  because exporters are replaced with capture factories.
