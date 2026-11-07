# Configuration resolution

Verified against `otelcol v0.162.0`, `confmap v1.68.0`.

- `otelcol.NewConfigProvider(otelcol.ConfigProviderSettings{ResolverSettings:
  confmap.ResolverSettings{URIs, ProviderFactories}})` then
  `provider.Get(ctx, factories)` returns `*otelcol.Config`. The Collector itself
  then runs `confmap.Validate(cfg)` (recursive `Validate()` on every component
  config plus `service` pipeline checks); call the same function to match its
  validation exactly.
- Unmarshalling uses only `factory.CreateDefaultConfig()`, so decorated
  factories that reuse the real default config validate identically.
- The OCB manifest ships `env`, `file` and `yaml` providers. Additional `yaml:`
  URIs are merged after the file and can override single keys with `::`
  paths, e.g. `yaml:exporters::otlp::endpoint: 127.0.0.1:4317`. The debugger
  never does this to the user's config; only the reference run uses it, to
  redirect network endpoints, and a recommendation's candidate run, to apply
  the suggested change beside the unchanged file (below).
- A `yaml:` URI whose value is a list replaces the list at that key; it is
  not merged element by element. `yaml:service::pipelines::logs::processors:
  [a, b]` gives the pipeline exactly `[a, b]`
  (`TestVerifyRecommendationRunsTheChangeOnTheRunsInputs`, verified against
  `otelcol v0.162.0` on 2026-11-07).

## Candidate overrides

A sealed snapshot normally holds one URI, the root. A recommendation's
candidate run (`engine.CandidateRequest`) layers its change as `yaml:` URIs
after the root (`Snapshot.WithOverrides`): the user's file is captured and
digested unchanged, and the overrides are recorded beside it. A snapshot with
overrides is written as schema 4; one without stays schema 3, so existing
bundles and new ordinary runs read and write as before. `DecodeSnapshot`
accepts several URIs only at schema 4, and only `yaml:` URIs after the root.

## Scenario environment

Scenario `environment` values are supplied through a wrapper around the
upstream `env` provider (`internal/debugger/config/env_provider.go`), registered under
the same `env` scheme. For `${env:NAME}` and `${env:NAME:-default}` it returns
the scenario value when NAME is declared (parsed as YAML, like the upstream
provider) and otherwise delegates to the real provider, so process
environment and defaults behave as usual. The process environment is never
modified, so concurrent runs cannot leak values into each other.

`TestResolveUsesScenarioEnvironment` covers the precedence. The instrumented
and the reference run receive the same values.

Resolved configuration contains the substituted values. Anything that prints
resolved configuration (the OTTL replay prints statement text) can therefore
show environment values; do not put secrets in scenario environments that
feed OTTL statements.

## Verified behavior (2026-10-09)

- `Resolve` reports unknown component types, OTTL parse errors in
  `transform` statements, and undefined pipeline references before any
  component is built (`TestResolveRejectsInvalidConfig`). OTTL statements are
  parsed during `transformprocessor` config validation, not only at create
  time.
- Reusing live `Resolved.URIs` does **not** guarantee the same configuration:
  files/environment can change between planning and Collector startup. Execution
  passes a sealed provider snapshot alongside the unchanged logical URIs.

## Sealed execution providers

Verified against Collector `v0.162.0`, confmap/providers `v1.68.0`.
`CaptureSource` runs the same Collector expansion, component diagnostics, typed
unmarshal and validation. Its provider instances share one retrieval cache across
the diagnostic and typed passes. Each exact file/env URI is captured once;
references discovered inside another provider value use the same cache. A
`Snapshot` then supplies only those file/env responses to later planning and
Collector startup. Uncaptured dependencies fail with `ErrUncaptured`; frozen
execution has no filesystem/process-environment fallback.

The platform calls `engine.PrepareWithEnvironment` before admission to create
one snapshot, then passes it to instrumented and reference execution. Direct
engine calls prepare at invocation; direct runtime callers capture once per
invocation. Reference isolation can append pure `yaml:` overrides as before; any
new file/env reference inside an override still needs a captured dependency.
Inspection and playground operations retain live resolution.

Preserve upstream retrieval semantics rather than marshalling an effective config:
configopaque fields in the latter are redacted and factory defaults/aliases can
change its representation. `NewRetrievedFromYAML` preserves both typed `AsRaw`
values and original `AsString` representations. Unset env with no default gives
nil YAML; an explicitly empty default or scenario override gives an empty string.
The snapshot records that distinction. File capture uses the upstream cleaned
path and YAML parser, with bounded regular-file reads instead of unbounded
`os.ReadFile`. Upstream relative file-provider URIs resolve against process CWD,
not the root config directory; frozen URI keys preserve that behavior even after
CWD changes. Original root bytes (including CRLF) supply report provenance.

Initial bounds: 16 MiB total root/provider URI and content bytes, 128 distinct
file/env URIs. They bound materialization, not expanded config or Collector
memory. Private snapshot JSON is schema 3 (schema 2 stays readable) and includes
source/environment values; it is not an API metadata projection or an
individual-secret fingerprint. Decode checks schema, fields, root binding and
bounds. Snapshot data is immutable through its public methods; byte reads are
copies. They are stored in private run-owned [execution
bundles](execution-bundles.md). Schema 1 remains readable with unknown env
origins. Snapshots retain capture policy and per-retrieval binding origins. Core
services default to declared values/defaults; explicit host allowlists and the
unconfined host policy are described in [environment
bindings](environment-bindings.md). Capture is per URI, not an atomic
filesystem/environment transaction.

## Unavailable references

Verified against confmap `v1.68.0`. A reference always resolves: the scenario
first (`environment` for `${env:}`, a `mocks.files` entry with the same path
for `${file:}`), then the host under the composition's policy, then the
stand-in string `linea-stand-in-<first 8 hex digits of the URI's
SHA-256>`, with a reason (`missing`, `unreadable`, `outside_roots`, `unbound`,
`unsupported_scheme`). The suffix is derived from the URI because the resolver
expands map entries in Go map order, so a counter would differ between
captures. The configuration root itself never stands in.

The resolver fixes its providers at construction and fails a URI whose scheme
has none with `scheme "x" is not supported for uri "x:..."` (`resolver.go`).
The capture adds a stand-in provider for each such scheme and resolves again
(at most 16 schemes); values already captured are not reread. Nothing is
fetched for those schemes. Frozen replay registers a provider for every extra
scheme in the snapshot.

The stand-in is a plain string. Where a setting needs another type (a port, a
duration, a whole section from `${file:}`), the Collector's typed decoding
fails; the error becomes an `invalid_configuration` diagnostic whose hint names
the stand-ins the message quotes, or all of them. `Snapshot.References` lists
every reference with its source and the components whose settings contain it
in the root text; references nested in referenced content list none.

`TestCapturedProvidersMatchUpstreamAndSurviveSourceMutation`,
`TestCapturedEnvironmentRetainsDefaultsTypesAndEmptySemantics` and the relative
URI/bounds tests cover provider behavior. The real engine regression changes the
root config, deletes its included file and changes the environment on `run.started`
before Collector startup: the original plan, captured provenance and both
execution outputs still agree. Separate engine/platform preparation tests cover
frozen fixtures/mocks and queued source deletion in both modes. Bundle tests cover
restart, same-build explicit execution and binary mock-byte round trips. Component
runtime prerequisites and complete reproduction classification remain unfinished.
