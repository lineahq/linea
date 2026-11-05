# Validation

Developer documentation: what the automated checks for the capture
components cover, and how to run their microbenchmarks. The
[load and failure experiments](performance.md) measure throughput, CPU,
latency and memory with a real Collector.

## Automated checks

`./scripts/collector/dev.sh DockerTest` (`dev.ps1` on Windows) runs them.

| Area | Covered |
| --- | --- |
| Tests and vet | Component unit and transport integration tests with `go test -race -count=1`, plus `go vet` for the components, the test lab helper and the distribution modules |
| Filters | Runtime OTTL compilation, read-only restrictions, resource/span/log/metric/datapoint filtering, runtime filter errors |
| Copies | Copy ownership and original immutability, scope/resource/schema preservation, nested redaction, valid OTLP copies for all five metric types |
| Sessions | Expiry; full-state reconciliation; in-place updates without a capture gap or renewed budgets or TTL; updates that bring the deadline forward; independent pod capture for a group; kill switch (including while an exporter is starting); raw authorization revocation |
| Limits | Pod-independent trace and log sampling; rate, size and total limits; grouped prefix admission per batch; exact byte budgets; panic isolation; bounded failure diagnostics |
| Delivery | Real internal OTLP/gRPC delivery; a byte-queue outage and overflow test; stop timing with a full queue; queue occupancy bounded across repeated stop/start cycles; accurate pod-wide queue occupancy. A real OpenTelemetry SDK reader verifies that the queue gauge preserves standard exporter observations. |
| Local and control paths | Strict development HTTP decoding and lifecycle; outbound control stream loss, reconnect, identity and build-version reporting, local expiry during disconnection, rejected desired states reported on a stream that stays up; a session wire round trip |
| Runtime | A race-instrumented Collector subprocess and the static scratch runtime as UID 10001 on a read-only filesystem. Baseline smoke covers all three signals over OTLP/gRPC and OTLP/HTTP JSON. |
| Active smoke | All three signals at two points, tagged and redacted copies, explicit stop, queue overflow while ingress is unavailable, and byte-identical production OTLP payloads before, during and after capture |

The smoke ingress, backend and streaming control server are test fixtures
only. They are not product services or an implementation of the debugging
platform.

## Microbenchmarks

Run against a freshly built test image:

```sh
docker compose -f deploy/compose/collector/compose.yaml --profile test run --rm --entrypoint go -w /src/collector/livecapture tests test -mod=readonly -run '^$' -bench . -benchmem -count=3 ./...
```

- The benchmarks use a noop telemetry provider and cover inactive
  pass-through, one selected copy (with and without protobuf encoding), an
  active filter that matches nothing, and 100-span batches with 0, 1, 10 and
  100% selected.
- The copy comparison includes a selected copy in both paths, because public
  pdata APIs do not directly marshal only one selected span out of a batch.
  It supports choosing CopyTo for ownership transfer; it is not a comparison
  with a custom zero-copy protobuf encoder.
- Selection benchmarks use an immediate discard sink to isolate capture work
  and avoid measuring network speed or unbounded fake storage.
- A unit test also enforces the inactive zero-allocation property.
- Nanosecond overhead and microbenchmarks do not establish CPU percentage or
  throughput targets for a deployed Collector.

## Outage memory measurement

`BenchmarkQueueOutage` uses:

- the actual internal stock exporter and an unreachable ingress
- an 8 MiB byte queue
- distinct, approximately 4 KiB span payloads
- raised configured rates so the queue fills promptly

It forces GC before and after filling, then measures retained heap and
saturated enqueue work. It reports queue occupancy against the configured
cap, the additional retained Go heap after the fill, and the time and
allocations of a saturated capture operation.

- The heap delta includes queue-held pdata, bookkeeping and transport
  allocations created during the fill. It excludes initialization already
  present at the baseline and transient allocations collected before
  measurement.
- Resource sharing, attribute shapes, exemplar counts and record sizes
  change the overhead. This is a measured workload result, not a universal
  fixed allowance or a peak-RSS guarantee.
- The bounded encoded queue remains the configured budget. Size deployment
  memory for Go object overhead and concurrent copy work.

## Self-telemetry and audit

| Topic | Detail |
| --- | --- |
| Instruments | Sessions active, selected and dropped records, enqueued bytes, queue bytes, filter duration and errors, session terminations, control connectivity |
| Labels | Configured point and signal names, or bounded reason codes. Session IDs are excluded. |
| Queue bytes | Summed from the stock exporter's live observations, without changing its queue implementation |
| Stock exporter | Retains its standard sent, failed and queue metrics |
| Collecting | Enable the Collector's own telemetry readers in the deployment. The synthetic fixture disables internal metric export. |
| Audit logs | Start and end logs contain principal, target, points, filters, limits, deadline, outcome and totals. Capture payloads and recovered panic values are excluded. Exporter drain errors appear as a boolean, not remote error text. |

## What to validate in your deployment

These local checks do not establish production CPU, throughput,
activation-p95, delivery-p95 or peak-memory targets. Measure the actual
deployed distribution and representative workloads with capture absent,
inactive, and 0/1/10/100% selection, including slow ingress.

These need the deployment environment:

- non-OTLP receiver integration
- OTLP/HTTP protobuf active capture
- Operator-managed Kubernetes rollout
- mTLS with deployment credentials
- compatibility with the existing platform's control schema

The processor uses signal consumers directly and has no OTLP receiver
dependency. Platform RBAC, ingress session validation, viewer JSON and
completeness behavior, and retention are responsibilities of the debugging
platform, outside the Collector components.
