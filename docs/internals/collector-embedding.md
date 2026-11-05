# Embedding the Collector in-process

Verified against `go.opentelemetry.io/collector/otelcol v0.162.0`.

## Lifecycle

- `otelcol.NewCollector(otelcol.CollectorSettings)` then `col.Run(ctx)` blocks
  until shutdown. `Run` resolves config, calls `confmap.Validate(cfg)`, builds
  `service.New(...)`, starts it and sets state `StateRunning`.
- There is no "started" callback; poll `col.GetState()` for `StateRunning`
  while also watching the `Run` error (startup failures return from `Run` with
  state `StateClosed`).
- `col.Shutdown()` signals the run loop; `Run` returns after
  `service.Shutdown`, which stops receivers first, then processors, then
  exporters. The batch processor flushes pending data on shutdown, so a replay
  can be completed by injecting data and then shutting down.
- Do not call `col.Shutdown()` until `Run` is known to have begun (for
  example once `GetState()` reports `StateRunning`). `Run` calls
  `wg.Add(1)` on entry and `Shutdown` calls `wg.Wait()`; with no ordering
  between them the race detector reports a data race (verified 2026-10-13,
  `go test -race` on Windows, Go 1.27.2). `Run` already watches its context,
  so a run cancelled during startup waits for `Run` to return without calling
  `Shutdown`; the Collector then starts its components and stops them again.
  `TestRunCancelledBeforeStartupStopsEveryComponent` covers this.
- `Run` installs OS signal handlers (SIGHUP always; SIGINT/SIGTERM unless
  `DisableGracefulShutdown`). Set `SkipSettingGRPCLogger: true` to avoid
  replacing the process-global gRPC logger.

## Internal telemetry isolation

- `service.New` fails if `Factories.Telemetry` is nil.
- The OCB default `otelconftelemetry` factory's default config starts a
  Prometheus endpoint on port `8888`, and user `service::telemetry` can export
  internal telemetry over OTLP. Both break offline isolation and make two
  concurrent runs collide.
- `telemetry.NewFactory(createDefaultConfig)` with no options yields a nop
  logger, empty resource, and noop meter/tracer providers. Reusing
  `otelconftelemetry.NewFactory().CreateDefaultConfig` keeps the user's
  `service::telemetry` section unmarshalled and validated with the real schema
  while nothing is emitted. `telemetry.WithCreateLogger` can supply a debugger
  logger.
- Verified against 0.162.0 (2026-10-24, `TestRunLogRedactsEnvironmentValues`):
  the logger supplied this way receives the service's startup lines and the
  `debug` exporter's output (its default `use_internal_logger: true`), with
  attribute values as processed. A run's log can therefore hold telemetry
  values and resolved configuration values, which is why the run log tail
  redacts environment values.

## Verified driver pattern (2026-10-09)

`internal/debugger/runtime/runtime.go` `runCollector`:

1. `otelcol.NewCollector` with `Factories` returning prebuilt factories,
   `DisableGracefulShutdown: true` (the CLI owns signal handling) and
   `SkipSettingGRPCLogger: true`.
2. `go col.Run(ctx)`; poll `GetState()` every 5ms until `StateRunning`, failing
   fast if `Run` returns first. If `ctx` ends first, only wait for `Run`.
3. Drive the run (inject or send the fixture).
4. `col.Shutdown()` and wait for `Run` to return. All flushed output has then
   reached the exporters.

Configuration errors surface from `Run` during startup, but the debugger
resolves and validates first (see [config-resolution.md](config-resolution.md))
so users get the error before any component is built.

`runtime/debug.ReadBuildInfo()` in a test binary lists linked module versions;
`internal/debugger/registry` uses this to assert the root `go.mod` links exactly the
versions the OCB manifest pins.
