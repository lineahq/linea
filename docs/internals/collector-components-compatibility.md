# Collector components on newer Collector releases

The `collector/livecapture` module requires Collector `v0.162.0` /
`v1.68.0` and OTTL `v0.162.0`. These are minimums. This note records how the
module behaves on later releases. Verified on 2026-11-03 against 0.163.0 and
0.164.0.

## Releases checked

| Release | Stable modules | OTTL | Module tests | OCB build and `validate` |
| --- | --- | --- | --- | --- |
| 0.163.0 | `v1.69.0` | `v0.163.0` | pass | pass, with the root-module replace |
| 0.164.0 | `v1.70.0` | `v1.3.0` | pass | pass, with the root-module replace |

- **Module tests:** a copy of `collector/livecapture` and `collector/provider`
  with every Collector module raised to the release, then `go build`,
  `go vet` and `go test ./...`. No source change was needed.
- **OCB build:** the unmodified repository module, through an OCB manifest
  at the release with `otlpreceiver`, `debugexporter`, `captureprocessor`,
  `livecaptureextension` and the contrib `opampextension` (0.164.1 for
  0.164). `otelcol-custom validate` accepts the minimal configuration from
  [the components README](../collector-components/README.md#minimal-configuration).
- OTTL left the `v0.x` line at 0.164: `pkg/ottl` `v1.3.0` belongs to Collector
  0.164.0 (`v1.2.0` was never tagged).

## The `statusutil` split

In 0.163.0, `go.opentelemetry.io/collector/internal/statusutil` became its own
module. The root module `go.opentelemetry.io/collector` at `v0.162.0` still
contains that package.

`livecaptureextension` imports `otlpexporter`, and `otlpexporter` `v0.162.0`
requires the root module, so `collector/livecapture/go.mod` lists
`go.opentelemetry.io/collector v0.162.0 // indirect`. No 0.163+ module
requires the root module, so a newer build keeps it at `v0.162.0` and fails:

```
ambiguous import: found package go.opentelemetry.io/collector/internal/statusutil in multiple modules
```

The fix is a manifest `replaces` entry that raises the root module to the
build's release:

```yaml
replaces:
  - go.opentelemetry.io/collector => go.opentelemetry.io/collector v0.164.0
```

Raising the module's own minimum to 0.163.0 or newer would remove the need for
it, but the root module shares the Collector release line with the debugger
(AGENTS.md "One Collector release line").

## OCB 0.163.0 install

`go install go.opentelemetry.io/collector/cmd/builder@v0.163.0` fails with
"module go.opentelemetry.io/collector@v0.163.0 found, but does not contain
package". The `cmd/builder` module exists at `v0.163.0`;
`go mod download` and `go build` in its module directory work. 0.164.0
installs normally.
