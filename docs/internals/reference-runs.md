# Reference runs

Verified against Collector `v0.162.0` on Windows (2026-10-09).

## What the reference is

`runtime.RunReference` runs the **unmodified upstream factories** (no
decoration, no substitution) in a real `otelcol.Collector`:

- Inputs are sent in order over loopback OTLP/gRPC to the real `otlp`
  receivers they target (each used receiver gets its own free port), with
  the same scenario environment as the instrumented run.
- `debug` and `nop` run as in the instrumented run, observed alike, and
  what reached them is compared with the instrumented outputs.
- Each non-local exporter (anything except `debug` and `nop`) must be the real
  OTLP gRPC exporter (`otlp`/`otlp_grpc`) or the OTLP/HTTP exporter
  (`otlphttp`/`otlp_http`). The gRPC exporter delivers to an in-process OTLP
  gRPC sink (`ptraceotlp.RegisterGRPCServer` etc.) on `127.0.0.1:0`. The HTTP
  exporter delivers to an in-process HTTP sink (`POST /v1/traces|metrics|logs`,
  protobuf or JSON by `Content-Type`, gzip accepted) that replies with an
  empty export response. (Verified 2026-10-10.)
- Only internal telemetry is isolated, so two runs never collide on `:8888`.

The user's YAML is not changed. Endpoints are redirected by appending
`yaml:` provider URIs after the `file:` URI:

```
yaml:receivers::otlp::protocols::grpc::endpoint: 127.0.0.1:<free>
yaml:receivers::otlp::protocols::http::endpoint: 127.0.0.1:<free>   # only if http was configured
yaml:exporters::otlp::endpoint: 127.0.0.1:<sink>
yaml:exporters::otlp::tls::insecure: true
# an otlphttp exporter instead:
yaml:exporters::otlphttp::endpoint: http://127.0.0.1:<sink>
yaml:exporters::otlphttp::traces_endpoint: http://127.0.0.1:<sink>/v1/traces   # and metrics_, logs_
yaml:exporters::otlphttp::compression: none
```

The per-signal endpoints are overridden too because a user's
`traces_endpoint` takes precedence over `endpoint`.

Setting `protocols::grpc::endpoint` enables gRPC even if only HTTP was
configured; that only adds an input path and does not change processing.

## Transport prerequisites

Verified 2026-10-12: reference endpoints are plaintext. Default engine preparation
rejects known unsupported TLS files, host trust/device settings, receiver TLS,
gRPC CA PEM and explicit HTTP proxies before admission. Direct runtime calls also
preflight before any listeners/sinks/mocks start. The same config remains usable
through instrumented transport substitutions without comparison. Inline HTTP TLS
material stays captured and is parsed by upstream startup. See
[runtime-prerequisites.md](runtime-prerequisites.md) for exact scope, compatibility
and the component dependencies still awaiting classification.

## Comparison rule

`runresult.Compare` compares, per exporter ID and signal, the concatenation of
all delivered batches as canonical OTLP JSON. When the bytes differ, it
compares the sorted multisets of `telemetry.ContextFingerprints`: each record's
typed fields together with its resource and scope. Equal multisets count as
equal, with a `notes` entry ("same records in a different order or
grouping"), because pipelines sharing an exporter flush in nondeterministic
order at shutdown (see [run-lifecycle.md](run-lifecycle.md)) and grouping
into resources may differ. Batch counts are reported but not compared,
because the real exporter's sending queue may split or merge requests
differently from the synchronous capture exporter. A run status mismatch is
a difference (`TestCompareIgnoresDeliveryOrder` covers both cases).

Input acceptance is compared too: for each input, both runs must either
accept it or reject it. Error texts are not compared, because a gRPC client
sees a status message while in-process injection sees the Go error.

## Findings

- Final telemetry is byte-identical between the instrumented run and the
  reference for `otlp -> transform -> filter -> batch -> otlp` with all three
  processors decorated. The OTLP proto round trip
  over gRPC (with default gzip) is lossless relative to pdata JSON.
- The comparison is not vacuous: removing one OTTL statement from the reference
  config makes it report a difference
  (`TestCompareDetectsSemanticDifference`).
- A transform with `error_mode: propagate` and a failing statement makes the
  pipeline return an error: the OTLP receiver answers the gRPC call with an
  error and in-process injection returns it. Both runs reject the input and
  no exporter receives data (`TestExecuteReportsRejectedInput`).
- Collector shutdown completes the run: the batch processor (timeout 200ms)
  flushes on shutdown and the OTLP exporter's in-memory queue drains before
  `Run` returns, so no sleeps are needed. Both runs together take well under a
  second.
- The server side of the sink must have the gRPC gzip codec registered
  (`google.golang.org/grpc/encoding/gzip`) because the OTLP exporter defaults to
  gzip compression.
- `ptraceotlp.GRPCServer` has an unexported method with a pointer receiver on
  `UnimplementedGRPCServer`; implementations must embed it and be registered
  as pointers.

## Limitations

- Reference runs support only OTLP or mocked receivers and OTLP gRPC or
  OTLP/HTTP exporters (plus `debug`/`nop`). Mocked components (filelog,
  k8sattributes, spanmetrics) run against the same mocks as the instrumented
  run. Declared client addresses are passed to the pipeline, and the outputs are
  compared after normalization
  ([mocking.md](mocking.md#reference-runs-with-mocks)). Upstream extensions are
  inert in the reference run too (custom ones run), so an otlp or otlphttp
  exporter with `auth:` is a reference prerequisite (setting `auth`); custom TLS
  is not redirected.
- Order and grouping are deliberately ignored, so a processor whose only effect
  is reordering records cannot be told apart from the reference (none of the
  supported processors relies on order).
