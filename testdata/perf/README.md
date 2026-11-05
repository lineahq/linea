# Performance workloads

Fixed inputs for measuring performance. `scripts/measure-performance.ps1` times
them; `TestPerformanceWorkloadsRun` (`internal/debugger/engine/perf_test.go`) keeps
them valid and runnable. No test asserts a duration.

| Workload | Configuration | Input | Measures |
| --- | --- | --- | --- |
| W1 | `components-100.yaml` | — | static graph generation, 100 components (target under 2 s) |
| W2 | `sync-logs.yaml` | `logs-100.json` | small fixture replay, 100 records, synchronous (target under 5 s) |
| W3 | `sync-logs.yaml` | `logs-1000.json` | the same pipeline at 1,000 records |
| W4 | `components-100.yaml` | `logs-100.json` | the large report (about 13 MB of JSON) for the web and workspace storage |

How they were generated (keep the files byte-stable; regenerate only on
purpose and re-record the baseline):

- `components-100.yaml`: one `otlp` receiver, 96 `attributes/p<i>` processors
  (each upserts `k<i>` = `v<i>`) in 8 logs pipelines of 12, and three
  exporters (`otlp/a`, `otlp/b`, `debug`) shared by every pipeline.
- `sync-logs.yaml`: `samples/logs-cleanup/config.yaml` without `batch`, so
  every stage is synchronous.
- `logs-100.json` and `logs-1000.json`: the four log records of
  `samples/logs-cleanup/input.json` repeated in order to 100 and 1,000
  records in one resource and scope. One record in four is a health check the
  filter drops, so W2 delivers 75 and W3 750; W4 delivers 100 per pipeline,
  800 to each captured exporter.
