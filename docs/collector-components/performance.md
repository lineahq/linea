# Load and failure experiments

Developer documentation. The Collector lab includes a load and failure
fixture for the capture components. It measures:

- real OTLP throughput, Collector CPU and RPC latency
- queue occupancy and heap, with capture absent, inactive and selecting
- that capture never loses or alters main-pipeline data

Results depend on the host and the payload. Measure your own deployment
before relying on any figure.

## Run the experiments

```sh
./scripts/collector/dev.sh DockerPerf
```

```powershell
./scripts/collector/dev.ps1 DockerPerf
```

Or select experiments and extend their duration:

```sh
docker compose -f deploy/compose/collector/compose.perf.yaml run --build --rm -T perf -perf-seconds 20 -perf-cases inactive-60000,raised-60000,raised-180000,outage-4k
```

Output is JSON lines: throughput, request latency, Collector CPU/RSS/heap,
capture queue occupancy and drop counters.

## How it measures

| Topic | Detail |
| --- | --- |
| Isolation | The fixture publishes no ports and has no external network. It starts a fresh Collector subprocess for each experiment. |
| Sinks | Separate counted OTLP/gRPC sinks for original and captured data. The main sink requires untagged originals; the capture sink requires tagged copies. |
| Failures | The runner fails on a routing violation, or if any successfully acknowledged input is missing from the main sink. It also checks selective capture percentages and the total-record budget. |
| Resources | Four-CPU quota and 768 MiB memory limit. The Collector and the load/sink helper both use `GOMAXPROCS=2`. |
| CPU | Measured separately from the helper, from the Collector's Linux process CPU time. One core means one CPU-second per wall-clock second. |
| RSS | From the Collector process's `/proc` entry |
| Heap, capture duration, queue occupancy, exporter failures | From the real Collector's Prometheus self-telemetry |
| Sampling precision | Memory and queue peaks are sampled every 200 ms and can miss shorter peaks. First-drop and fill times have the same precision. |
| Requests | 100 spans, logs or gauge datapoints each, cycling equally over the three signals, with a single ingress capture point |
| Target rate | **Total original records per second**, not per signal |
| Load | Twelve synchronous workers apply backpressure when the Collector cannot sustain the target. Sources do not retry failed calls. |
| Reported rate | Achieved throughput and RPC latency, including failed calls, not the requested rate |
| Main pipeline | The normal batch processor and a real OTLP exporter |
| Payload | Each batch has distinct trace IDs and selection indices. The same immutable batches are reused between requests to exclude generation cost. |
| Duration | Eight seconds per case by default, with startup warmup outside the measurement window |

Not measured: arbitrary telemetry entropy (payload strings are repetitive
and compress well with the stock capture exporter's gzip), network bandwidth
limits, real disks, and the interactive viewer's JSON overhead. Longer soak
tests with representative deployment data remain necessary.

## What the experiments show

| Finding | Detail |
| --- | --- |
| Limits bind before the main pipeline does | With stock limits, capture reaches its record-rate or byte-rate limit and reports `rate_records` or `rate_bytes` drops. Dropping at a limit is intentional capture loss, never main-pipeline loss. Each processor call becomes one copy grouped by resource and scope, so both limits sit close together for small records. Selecting both ingress and egress spends the budget twice per original record. |
| Capture is not free | Nonblocking export does not remove the synchronous work in the processor: filtering, copying, redaction, tagging and admission. Copying every record at raised limits costs noticeable Collector CPU and RPC latency. Resource-level rejection is much cheaper than a per-record condition, so tight filters are the first control to use. |
| Slow or unavailable capture sinks fill the queue | New copies are dropped with `queue_full` without waiting for space; every accepted original still reaches the main pipeline. Each signal gets a third of `max_queue_bytes`. Queue admission is not fair between records, services or capture points. |
| Stopping a session does not wait for the queue | Copies already queued are still delivered within the exporter's retry budget (at most five seconds). Enqueue success is not delivery success, and the exporter's failed-send instruments may count repeated attempts. |
| Do not raise the queue cap to improve completeness | The `outage-64m` experiment raises the queue to 64 MiB under a 128 MiB memory limiter: input RPCs are refused before the queue fills, so shared-process memory pressure affects normal telemetry. The default 8 MiB queue caused no input refusals. |

## On-device memory

- The extension queue is in memory, not persistent storage, and a restart
  loses it.
- `max_queue_bytes` bounds queued **encoded protobuf bytes**, not Go heap or
  RSS. It excludes copies being built, in-flight exporter batches,
  marshaling, compression and other Collector pipeline state.
- Small records retain more Go objects per encoded byte than large ones, so
  retained heap can be several times the queue's encoded size. Size the
  Collector's memory for that, and for concurrent copy work.
- The viewer has its own rolling 500-entry / 8 MiB JSON buffer. Evicting its
  oldest entries is intentional and is not a Collector queue failure.
  Pausing or disconnecting the viewer can lose old entries, and large
  payloads reach the byte bound sooner. A metric entry can hold several data
  points, so entries per second and records per second differ.

Run the queue heap benchmark from the test image:

```sh
docker compose -f deploy/compose/collector/compose.yaml --profile test run --build --rm --entrypoint go -w /src/collector/livecapture tests test -run=^$ -bench=BenchmarkQueueShapes -benchmem -benchtime=100ms -count=3 ./livecaptureextension
```
