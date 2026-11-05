# Factory decoration

Verified against Collector `v0.162.0` (`processor`, `receiver`, `exporter`
modules `v1.68.0`).

## Type aliases (pitfall)

- `otlpexporter.NewFactory()` has type **`otlp_grpc`** with deprecated alias
  `otlp` (`xexporter.WithDeprecatedTypeAlias`). Configs that say `otlp:` under
  `exporters` resolve through the alias.
- `otelcol.MakeFactoryMap` registers the alias only if the factory implements
  the internal `componentalias.TypeAliasHolder`
  (`DeprecatedAlias() component.Type`). A wrapper built with plain
  `exporter.NewFactory` silently drops the alias, so `otlp:` configs then fail
  with an unknown type. Wrappers must be built with `xexporter.NewFactory` /
  `xreceiver.NewFactory` / `xprocessor.NewFactory` and
  `WithDeprecatedTypeAlias(alias)` whenever the wrapped factory reports one.
- Real factories call `componentalias.ValidateComponentType(f, set.ID)`, which
  accepts either the type or its alias, so delegating with the original
  `Settings` is safe.

## Processor wrapping rules

- Keep the real factory's `Type`, `CreateDefaultConfig` and per-signal
  stability; only register signals the real factory supports (unsupported
  signals report `component.StabilityLevelUndefined`).
- Ingress: wrap the returned `processor.Traces` and intercept `ConsumeTraces`;
  delegate `Start`, `Shutdown` and **`Capabilities`**. The service uses
  `Capabilities().MutatesData` to decide fanout cloning, so changing it changes
  semantics.
- Egress: pass the real factory a `consumer.NewTraces` that records then calls
  `next`, built with `consumer.WithCapabilities(next.Capabilities())`.
- Snapshot by serializing immediately (OTLP JSON). Processors mutate pdata in
  place, so holding references would show later state.
- Correlating output with input: the ingress wrapper puts an
  `ingressMark{owner, seq}` into the `ctx` it passes to the real
  `ConsumeX`. The egress consumer attributes an emission to that input only
  if the mark's owner is the same processor instance. Components never read
  the key; adding a context value does not change processing. See
  [stage-analysis.md](stage-analysis.md) for what this shows.
- `processor.Settings` carries the component ID but not the pipeline ID; a
  processor used in several pipelines produces one instance per pipeline that
  cannot be distinguished from `Settings` alone. The pipeline ID reaches only
  the component logger: `componentattribute.LoggerWithAttributes` calls
  `With` on the root core with one inline field holding the scope attributes,
  including `otelcol.pipeline.id`. The runtime wraps the root logger with
  `zap.WrapCore(instrument.WrapCore)`; the resulting `probeCore` remembers
  the attribute from `With`, and `pipelineOf(set.Logger)` reads it back by
  checking a probe entry through `Core().Check` (nothing is written). See
  `internal/debugger/instrument/pipeline.go`, verified by
  `TestPipelineOfReadsComponentLoggerAttributes` and, against the real
  Collector, `TestSharedProcessorIsAttributedPerPipeline`.
- The wrappers override `Start` and `Shutdown` to record
  `component.started` / `component.stopped`; `Shutdown` is recorded after the
  inner processor returns, so flush output (batch) precedes it.

## Verified results (2026-10-09)

- `instrument.Processor(transformprocessor.NewFactory(), rec)` produces output
  byte-identical to the undecorated processor and reports the same
  `MutatesData` (`TestProcessorPreservesTransformSemantics`), and the same
  holds inside a full Collector against a reference run (see
  [reference-runs.md](reference-runs.md)).
- `transform`, `filter` and `batch` are all decorated in the
  `testdata/traces/` pipeline; final output still equals the reference run
  (`TestInstrumentedRunMatchesReferenceCollector`).
- `xprocessor.NewFactory` is the right constructor for wrappers: it is the
  only public way to attach a deprecated alias.

## Connector wrapping (verified 2026-10-10, v0.162.0)

`instrument.Connector` decorates a connector factory with
`xconnector.NewFactory`. It keeps the type, the deprecated alias, the
default config and the stability for each signal pair, and covers all nine
pairs (profiles are not decorated).

- Ingress reuses the processor wrappers: `connector.Traces` and
  `processor.Traces` have the same method set. Its events have source
  `connector_wrapper` and no pipeline.
- Egress: the Collector passes the connector a router
  (`connector.LogsRouterAndConsumer` etc.) over the first consumer of each
  pipeline it feeds. The routing connector type-asserts it and calls
  `Consumer(ids...)` when it is built. The wrapper builds the same kind of
  router with `connector.NewLogsRouter` over recording consumers that wrap
  each `r.Consumer(id)`, keeping that consumer's `Capabilities`. Fan-out,
  and its cloning for mutating consumers, is therefore the Collector's own.
  When `next` is not a router, one recording consumer wraps it.
- The runtime does not inject at connectors (`injectionTarget` skips IDs
  under `connectors`), and the reference run's component check skips them.
  `TestExecuteFollowsRecordsThroughARoutingConnector` shows that the
  instrumented run equals the uninstrumented reference.
- The count connector (logs/traces/metrics → metrics) is decorated the same
  way; its `next` is a single metrics consumer or a router when it feeds
  several pipelines. `TestExecuteAttributesRecordsToCountedDataPoints`
  compares it with the reference.

## Receiver and exporter substitution (isolation)

Implemented in `internal/debugger/runtime/substitute.go`.

- **Receivers:** every receiver factory is replaced by a replay factory with
  the same type, alias, `CreateDefaultConfig` and stabilities. Its create
  functions store the `next` consumer keyed by `(component.ID, signal)` and
  return a no-op component, so no listener is opened. The fixture is injected
  by calling that `next` consumer, which is exactly where the real receiver
  would deliver decoded data. The OTLP receiver normally shares one instance
  across signals (`sharedcomponent`); the replay receiver does not need to.
- **Exporters:** every exporter except `debug` and `nop` is replaced by a
  capture factory (fail-closed: new network exporters are isolated by
  default). Capture is synchronous and reports `MutatesData: false`, like the
  real OTLP exporter. `debug` and `nop` keep their real factories, wrapped
  (`observedExporterFactory`, `internal/debugger/runtime/local_exporters.go`) so what
  reaches them is recorded first: an `exporter.captured` event with mode
  `real` and an output, as for a captured exporter, and in reference runs
  too, so the comparison covers them. Their own logs are not recorded.
  A pipeline's exporter fan-out calls its exporters in an order that varies
  between runs (v0.162.0), so a configuration with `debug` beside another
  exporter has a raw event order that varies too; `--normalize` puts it in
  canonical order.
- Because configs are still unmarshalled into the real component config types
  and validated by `confmap.Validate`, invalid receiver/exporter settings fail
  exactly as in a real Collector even though those components never run.
- Verified by `TestInstrumentedRunIsNetworkIsolated`: with the receiver
  endpoint already bound and the exporter pointing at a live OTLP sink, the
  run succeeds and the sink receives nothing.

## Factory maps contain aliases

`otelcol.Factories` maps include alias keys pointing at the same factory
(`otlp` and `otlp_grpc`). When rebuilding a map from wrapped factories,
de-duplicate by `factory.Type()` first, then call `otelcol.MakeFactoryMap`,
which re-adds alias keys. Wrapping each map entry separately would produce two
wrappers of the same type, which `MakeFactoryMap` rejects as duplicates (from
reading `otelcol/factories.go`; not exercised by a test).
