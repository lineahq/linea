# Provider SDK

A provider is code in a component's own module that drives the component
during a debugger run and describes what it did: a capture session started
over a control service, a lease taken from a coordinator, a cache filled before
the first record. A runtime build compiles it into the worker beside the
components. The SDK is `collector/provider/`; its Go documentation
(`collector/provider/provider.go`) is the reference for each method. Design
and build mechanics are in [provider SDK
internals](../internals/debugger-provider-sdk.md).

| | |
| --- | --- |
| Module | `github.com/lineahq/linea/collector/provider`, package `provider`, version `provider.Version` (0.1.0) |
| Dependencies | Standard library only |
| Entry point | A package of your module exports `func DebuggerProvider() provider.Provider` |
| Build | A runtime build finds the package in the supplied sources and compiles it in ([custom components](runtime-builds.md#debugger-providers)) |
| Newer SDK | A module requiring a later SDK than the debugger embeds is refused at build |

## Provider fields

| Field | Meaning |
| --- | --- |
| `Name` | Required, unique in a runtime. Names the provider in runtimes, reports and `mocks.providers`. |
| `Components` | The `{Kind, Type}` pairs it acts for. `Prepare` runs only when the configuration uses one. Processors, connectors and extensions run for real; receivers and exporters are replaced, so a provider has nothing to drive there. |
| `Endpoints` | Client endpoints it serves itself, by path (`control`, `routes[0]::client`): a configgrpc or confighttp client (pointed at the listener, TLS, auth and proxies off) or a string field holding an address (host and port replaced). |
| `Prepare(ctx, Run)` | Once per run, before the Collector starts: read settings, take listeners, register hooks. |
| `Check(Static)` | Optional, at inspection: findings about missing settings, never running anything. `Static` has the components, the settings and `Pipelines()` (each pipeline's ID, signal and processors with their configuration). A `Finding` may carry `Settings`, a value marshalling into a JSON object that the debugger offers to add under `mocks.providers.<Name>` (diagnostic `provider_settings`). |

`Provider` is a struct, so new optional fields never break a provider.

## The Run interface

Only the debugger implements `Run`.

| Method | Meaning |
| --- | --- |
| `Components()` | The configured instances of its types in the run, with their effective configuration (`confmap` keys, secrets redacted). |
| `Settings(&target)` | The scenario's `mocks.providers.<Name>` as JSON. Absent settings leave target unchanged. |
| `Listener(component, path)` | The loopback listener a declared endpoint points at. Undeclared paths and components of other types are errors. |
| `Record(component, path, signal, otlp)` | An OTLP protobuf export request the served endpoint received; it becomes an endpoint output that assertions check. |
| `Supplied`, `Limitation`, `Warning` | Evidence per component. |
| `Logf` | The run's log, never the report. |

Hooks, in run order:

| Hook | When |
| --- | --- |
| `OnReady` | Collector running, before the first input |
| `OnInput(Input{Index, Receiver, Signal})` | Before each input, after its `after` wait |
| `OnInputsDone` | Before settle and shutdown |
| `OnClose` | After shutdown, also when the run failed; reverse registration order |

Failures:

- An error or panic in `Prepare` or a hook fails the run with "provider NAME
  (PACKAGE): ...".
- A failing `OnClose` is a warning on the declaration, since the evidence is
  complete.
- A served endpoint nothing connected to is a warning.

A provider's component runs for real and unmodified (mode
`dependency_mocked`). The run's `mocks` declarations name the provider with
what it reported. Providers act only through the component's own endpoints.

## Scenario

Settings sit beside the other mocks, opaque to the debugger (any YAML, handed
over as JSON). The scenario schema stays 1.

```yaml
mocks:
  providers:
    livecapture:
      sessions: [...]
```

- A name the runtime has no provider for fails the run in the worker and is an
  inspection diagnostic, not an admission error.
- In the web client, the Mocks panel's "Module providers" group edits these
  settings as YAML, one card per provider of the runtime selected beside Run. It
  shows what a provider's `Check` found missing and warns when the runtime
  lacks a provider the scenario names. "Add what <provider> proposes" adds
  only the keys the scenario lacks.

## Reports and listings

- Reports carry `runtime.providers`; a simulated endpoint a provider served
  names it (`simulated_endpoints[].provider`).
- `runtime list` (CLI, HTTP, MCP, web) names each provider with its package;
  `runtime manifest` names the providers found in the sources.

## Compatibility

- v0: within a minor version only additions. A provider written against v0.N
  builds with every debugger embedding v0.N or later.
- A breaking change is a new major module path (`provider/v2`); the debugger
  keeps building providers of the previous major for at least one release of
  the support table.
- Hook order and the evidence methods are part of the contract; `Logf` output is
  not.

## Isolation

Provider code runs in the worker with the components: no network, read-only
filesystem, the run's deadline. It gets loopback listeners and its own
settings, never host files or the host environment. It is trusted as much as
the components in the same module and nothing more.

## Examples

- `testdata/runtimes/stampprocessor/stampdebug` drives the `stamp` processor:
  `Check` requires a `note`, and the declaration reports the configured value,
  the number of inputs and the note.
- `collector/livecapture/livecapturedebug` serves the livecapture extension's
  `control` stream with the scenario's sessions and reports their statuses.

## Not supported

- Providers for receivers that emit their own telemetry (needs Collector
  types).
- A simulated clock for providers.
- Loading providers into the built-in runtime without a build.
