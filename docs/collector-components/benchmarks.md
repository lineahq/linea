# Testbed benchmarks

Developer documentation. The capture processor runs on the
[OpenTelemetry Collector Contrib testbed](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/testbed),
the harness behind the
[Collector benchmarks](https://opentelemetry.io/docs/collector/benchmarks/).
The scenarios copy contrib's OTLP 10k items per second tests, so the
baseline row compares directly with the published `OTLP` figures.

The [load and failure experiments](performance.md) cover other ground:
rate limits, slow and failing capture sinks, queue occupancy and memory
pressure. The testbed measures steady-state processor cost.

## Run

```sh
./scripts/collector/dev.sh Testbed
```

```powershell
./scripts/collector/dev.ps1 Testbed
```

Or run it directly. The module is outside `go.work` (contrib's testbed would
raise the workspace's dependency versions), so set `GOWORK=off`:

```sh
cd collector/tools/testbed
RUN_TESTBED=1 GOWORK=off go test -count=1 -v -timeout 60m -run 'TestCaptureTraces10kSPS/CaptureAll' .
```

| Variable | Effect |
| --- | --- |
| `RUN_TESTBED` | Required. Without it the tests exit at once, so `go test` stays cheap. |
| `TESTCASE_DURATION` | Load time per case, 15s by default. A capture session lasts at most five minutes, which caps it. |
| `LINEA_TESTBED_COLLECTOR` | Path to a prebuilt Collector with the capture components. Otherwise the test builds `collector/distribution` into a temporary directory. |

Each run writes `results/TESTRESULTS.md` (the testbed's table),
`results/benchmarks.json` (the
[github-action-benchmark](https://github.com/benchmark-action/github-action-benchmark)
format contrib publishes) and, per case, the Collector log, the backend logs
and the capture session's final status. A quick check before a release runs
every case for 10 seconds (`TESTCASE_DURATION=10s`).

## Scenarios

Three tests, one per signal: `TestCaptureTraces10kSPS`,
`TestCaptureLogs10kDPS` and `TestCaptureMetrics10kDPS`. Each runs five capture
modes:

| Mode | Pipeline | Session | Expected copies |
| --- | --- | --- | --- |
| `Baseline` | `[batch]`, no capture components | none | none |
| `Passthrough` | `[capture/ingress, batch]` and the `livecapture` extension | none | none |
| `CaptureAll` | same | no filter | every record |
| `CaptureFilter10pct` | same | `attributes["linea.selected"] == true` | every tenth record |
| `CaptureFilterNone` | same | a condition no record matches | none; every record is still evaluated |

and five reference modes, which run stock components on the same load. The
test distribution includes contrib's `attributes`, `filter` and `transform`
processors for them.

| Mode | Pipeline | Compare with |
| --- | --- | --- |
| `RefSecondExporterGzip` | `[batch]` and a second `otlp_grpc` exporter, gzip, to the capture backend | `CaptureAll`: every record sent twice, without a copy |
| `RefSecondExporterNone` | same, uncompressed | `CaptureAll` with `delivery.compression: none` |
| `RefFilterNone` | `[filter, batch]` with the `CaptureFilterNone` condition | `CaptureFilterNone` |
| `RefTransformSet` | `[transform, batch]`, one `set` of a record attribute | per-record OTTL work |
| `RefAttributesUpsert` | `[attributes, batch]`, one attribute upsert | a minimal per-record processor |

As in contrib, each case:

- sends 10,000 items per second over OTLP/gRPC in batches of 100, from
  contrib's perf data provider
- runs the Collector as a child process with `GOMAXPROCS=2`
- has the testbed sample its CPU and RSS every 3 seconds
- sends to an OTLP/gRPC mock backend, and fails unless that backend received
  every item sent

A metrics batch is 100 gauges of 7 data points, so items count data points.
Every generated record carries the `linea.selected` attribute, set on each
tenth record, so every mode sends the same payload.

The capture extension delivers copies over OTLP/gRPC to a second mock backend.
A case fails unless that backend received exactly the expected copies. The
processor uses `sensitivity: redacted`, so redaction is included in the
cost. The extension's rate and session budgets are raised so a 10k/s session
measures processor cost instead of hitting stock limits. Stock limits drop
copies above 5,000 records/s; see [performance](performance.md). The
Collector's own metrics are off (`service.telemetry.metrics.level: none`) in
every mode.

The resource limits in `capture_test.go` are regression guards with headroom
over the measured values below. A case fails when the Collector exceeds them.
The testbed samples CPU and RSS only when a limit is set.

## Results

One Linux run (`golang:1.27.2-bookworm` container on Docker Desktop WSL2,
4 CPUs, `TESTCASE_DURATION=15s`, Collector 0.162.0, 2026-11-04). Every case
delivered all sent items (about 150,000 spans or log records, or 1,050,700
metric data points) to the main backend, and exactly the expected copies to
the capture backend, with no drops. CPU is percent of one core. Repeated
runs on one host vary by about half a point.

| Case | Traces CPU % | Logs CPU % | Metrics CPU % | Traces RAM max MiB | Logs RAM max MiB | Metrics RAM max MiB |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| `Baseline` | 11.3 | 11.7 | 20.1 | 48 | 48 | 57 |
| `Passthrough` | 11.9 | 11.6 | 19.9 | 47 | 48 | 57 |
| `CaptureAll` | 14.9 | 14.9 | 32.7 | 50 | 51 | 60 |
| `CaptureFilter10pct` | 13.3 | 13.2 | 24.0 | 50 | 51 | 58 |
| `CaptureFilterNone` | 12.0 | 11.2 | 21.3 | 48 | 49 | 58 |
| `RefSecondExporterGzip` | 13.3 | 12.9 | 24.8 | 50 | 51 | 61 |
| `RefSecondExporterNone` | 12.1 | 12.3 | 21.5 | 48 | 48 | 58 |
| `RefFilterNone` | 12.0 | 11.9 | 21.1 | 48 | 48 | 58 |
| `RefTransformSet` | 12.1 | 12.1 | 21.6 | 50 | 50 | 72 |
| `RefAttributesUpsert` | 11.9 | 12.1 | 21.2 | 48 | 50 | 71 |

CPU is the average over the case.

What the run shows at 10k items per second:

- **Passthrough is free.** With no session, CPU and memory match the
  baseline within run-to-run noise.
- **Filtering costs what contrib's `filter` processor costs.** A session
  whose condition matches nothing evaluates every record, about half a point
  of a core for spans and logs and one point for metric data points, the same
  as `RefFilterNone` with that condition. `transform` and `attributes` cost
  about as much.
- **Cost follows what is copied.** Copying every record adds about 3.5 points
  of a core for spans and logs (about 30%), and 12.5 points for metrics
  (about 60%, since a metric batch holds seven times as many data points).
  Copying a tenth adds 1.5 to 4 points. Tight filters are the main lever.
- **Capturing everything costs 2 to 3 times a plain second exporter.**
  `RefSecondExporterGzip` sends every record twice by fanning out the same
  data: 1 to 2 points for spans and logs, 4.7 for metrics. Capture also
  copies each record (the original moves on while the copy is delivered
  later), redacts the copy, and measures its encoded size so byte budgets are
  never exceeded. Copying a tenth costs about as much as the plain second
  exporter.
- **Memory barely moves.** An active session adds at most about 3 MiB of
  peak RSS for its queue and delivery exporter.

The resource limits in `capture_test.go` are about twice the capture modes'
peaks: CPU 30% for traces and logs, 60% for metrics, and RAM 120 MiB.

A Windows 11 run of the same suite delivered the same counts with RAM peaks
of 35 to 48 MiB. Its CPU figures (0.4 to 2% of a core) are implausibly low,
so CPU limits never trip there; take CPU from Linux.

## Where the cost goes

CPU profiles of the Collector during the Linux `CaptureAll` cases (pprof
served from a profiling build, 20 seconds mid-run) split the capture
overhead into three parts:

- **Delivery compression.** The capture delivery is the stock OTLP exporter,
  whose default compression is gzip. The testbed's main exporter sends
  uncompressed, so gzip is extra work only the copies pay: about 40% of the
  metrics overhead and 15% of the spans overhead.
- **Delivering a second copy.** Proto sizing and marshaling, the exporter
  queue and the gRPC send cost about what the main pipeline's own export
  costs. This is the floor for capturing everything.
- **Selecting and copying.** The capture walk, its pdata copies and
  redaction were about a third of the metrics overhead. Redaction used to
  rewrite every string attribute and every data point was boxed for OTTL
  even without a filter; neither happens now. On testbed-shaped batches
  (`BenchmarkCaptureShapes` in `livecaptureextension`) this cut capture time
  by 18% for metrics and 10% for logs, and allocations by about 30%. Spans
  in this shape carry no string attributes, so they did not change.

The delivery's compression is set in the extension's `delivery` block
(see [configuration](configuration.md)). The `CaptureAll` cases with each
setting, from one earlier run on the same host (2026-11-03):

| `delivery.compression` | Traces CPU % | Logs CPU % | Metrics CPU % | Metrics RAM max MiB |
| --- | ---: | ---: | ---: | ---: |
| `gzip` (default) | 14.9 | 14.7 | 32.1 | 52 |
| `zstd` | 14.7 | 14.3 | 29.7 | 66 |
| `snappy` | 13.7 | 14.2 | 28.5 | 52 |
| `none` | 13.7 | 14.1 | 28.1 | 51 |

`snappy` costs about as little CPU as no compression. gzip trades CPU for
smaller requests, which matters when the capture endpoint is remote.

To profile in process, run the shape benchmarks with Go's profiler:

```sh
cd collector/livecapture
GOMAXPROCS=2 go test -run '^$' -bench BenchmarkCaptureShapes -cpuprofile cpu.out -memprofile mem.out ./livecaptureextension
```

## Platform notes

- On Windows the testbed cannot send SIGTERM, so it kills the Collector after
  each case and the result table's last column shows `exit status 1`. The
  case result is unaffected.
- Figures are not comparable across hosts. Compare modes from one run on one
  host, and compare the baseline with contrib's published `OTLP` row only as
  a sanity check.
