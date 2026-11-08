# Batching processors: explaining each send

`report.batching` (report schema 0.15) explains every `batch` and
`queue_batch` processor instance: when it sent what it held and why. The
engine builds it in `internal/debugger/analysis/batching.go`
(`analysis.Batch`), wired from `internal/debugger/engine/batching.go`. `linea run` prints each send and its
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

## queue_batch (queuebatchprocessor v0.165.0, source)

Contrib 0.165 adds the `queue_batch` processor, exporterhelper's queue and
batcher as a processor. `report.batching` explains it like `batch`, with
`settings.type: queue_batch` and the batch section's settings in the same
fields (`send_batch_size` is `batch::min_size`, `send_batch_max_size`
`batch::max_size`, `timeout` `batch::flush_timeout`, `metadata_keys`
`batch::partition::metadata_keys`), plus `sizer` and `cache_size`. Verified
against the exporterhelper and queuebatchprocessor sources at v0.165.0:

- Its configuration is exporterhelper's `QueueBatchConfig`. The factory turns
  batching on (`batch` defaults: `min_size` 8192, `flush_timeout` 200ms,
  `max_size` 0, `sizer` items, `partition::cache_size` 10000,
  `partition::idle_timeout` 90s) with `block_on_overflow: true`,
  `num_consumers: 1` and `queue_size: 10`. `batch: {enabled: false}` turns
  batching off: every request is queued and sent as it is
  (`settings.unbatched`, every send `immediate`). The batch sizer is `items`
  or `bytes` (the serialized request); when only the queue's `sizer` is set,
  the batch inherits it. The engine reads the settings by marshaling the
  configuration; the sizer marshals only through a pointer, so it is read
  from the struct (`batchSizer`).
- The batcher (`partitionBatcher`) merges each request into the pending
  batch with `MergeSplit`: resource blocks are moved, not merged, as in the
  batch processor. It sends while the pending size is at least `min_size`,
  in chunks of at most `max_size`, and keeps a last chunk below `min_size`.
  With `min_size` 0 every request is sent on arrival.
- Its timer starts with the processor and restarts after every tick, like
  the batch processor's, and also whenever a pending batch starts: a request
  reaching an empty batcher, or the remainder after a size send. The replay
  (`shards.arrive`) and the one-batcher comparison (`oneBatcher`) follow it.
- With `partition::metadata_keys` (`multiBatcher`) it keeps one partition
  per combination of the listed keys' values, matched case-insensitively,
  with no cardinality refusal: beyond `partition::cache_size` partitions it
  flushes and removes the least recently used one (`trigger: evicted`,
  `batchers[].evicted`). Partitions idle for `partition::idle_timeout` are
  removed too; the replay does not model that.
- Context: a send of one request keeps that request's context. A send that
  joins several requests gets `context.Background()`, or with
  `metadata_keys` a context holding only the listed keys' metadata from the
  first request. So `batch.drops_client_context` applies to queue_batch for
  joined sends only and suggests `batch::partition::metadata_keys`; an
  unbatched queue_batch drops nothing. The persistent queue (`storage`)
  keeps span context, client metadata and address, not auth or gRPC metadata.
- Metrics are `otelcol_processor_queuebatch_*`. There is no per-trigger
  count: `verification` compares the number of sends and the items sent
  with the `batch_send_size` histogram (count and sum), which the batcher
  records only with batching on.
- `bytes` sizes are measured in the replay as the size of each arrival's
  serialized snapshot; an arrival without a recorded snapshot has no byte
  size, and a note says a size send may then be counted as a timeout send.

`TestQueueBatchIsExplained` (Docker, 2026-11-08) runs
`samples/batch-timeline`'s scenario through a 0.165.0 runtime with
`queue_batch` at `min_size` 4, `max_size` 6 and `flush_timeout` 300ms: the
replay sends every log in four batches (size, timeout, a split size send,
shutdown) and its sends and items agree with the `batch_send_size`
histogram at the default telemetry level.

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
