# Design references

Developer documentation. Read before implementing the Collector components.
The implementation takes design pointers from OpenTelemetry's remotetap
processor. These are design references, not dependencies. OTTL and the stock
OTLP exporter remain upstream dependencies.

## OpenTelemetry `remotetapprocessor`

Source: contrib
[`remotetapprocessor`](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/v0.162.0/processor/remotetapprocessor),
pinned to `v0.162.0` (`factory.go`, `processor.go`, `channelset.go`).

| Aspect | Finding |
| --- | --- |
| Useful pattern | A pass-through processor with `MutatesData: false` and explicit lifecycle hooks for all three signals |
| Not adopted | Its channel writes block and its rate limiter selects batches. Neither meets this component's isolation or record-selection requirements. |

## Architectural choices

- Capture state is shared across signals.
- The processor resolves its control extension through `host.GetExtensions()`
  in `Start`.
- Control workers stop with the component lifecycle.
- An extension-owned manager holds the state, instead of a global processor
  map or report manager.

## Verified Collector behavior (0.162.0)

- The Collector does not pass a pipeline ID to processor factories. Named
  pipelines therefore need an explicit `pipeline` field and a distinct
  processor instance per pipeline.
- Default pipeline IDs are the signal names.
