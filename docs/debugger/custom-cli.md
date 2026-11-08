# A CLI with your components

The released `linea` CLI runs the built-in Collector: release 0.162.0 with the
built-in components. `linea build` compiles a `linea` CLI whose in-process
Collector is the one your [manifest](runtime-builds.md#writing-a-manifest)
describes: another verified release, fewer or more components, or your own
component modules.

The result is one binary. Its direct commands (`inspect`, `run`, `test`,
`diff`, `ottl`) run that Collector in its own process, with no Docker, no
worker, no server and no network. Use it where a runtime would be awkward:
on a laptop without Docker, in a CI job, or on an air-gapped machine.

| You want | Use |
| --- | --- |
| Your components on your machine or in CI, with no Docker | A CLI from `linea build` (this page) |
| Your components on a team server, from the web client, HTTP or MCP | A [runtime](runtime-builds.md), built locally or by the server's builder |
| Two releases side by side (`upgrade-check`) | Two [runtimes](runtime-builds.md) |

## Build one

You need the released `linea` CLI, a manifest, and either Docker or the
pinned Go release on `PATH` (`linea version` prints it as "built with").

```sh
# Start from the components your configuration uses.
linea runtime manifest --config collector.yaml --out mine.yaml

# Compile in the pinned Go image: only Docker is needed.
linea build --manifest mine.yaml --docker --out ./linea-mine

# Or with this machine's go, when it is the pinned release.
linea build --manifest mine.yaml --out ./linea-mine
```

The build prints the Collector release, the target platform, the size and the
SHA-256 of the binary. Without `--out` it writes `linea-custom` (or
`linea-custom.exe` for Windows) in the current directory, and replaces the
file only after the build succeeded.

| Flag | What it does |
| --- | --- |
| `--manifest FILE` | The OCB manifest. Relative `path:` and `replaces` directories are read beside it. Required. |
| `--out FILE` | The CLI to write. |
| `--docker` | Compile in the pinned Go image instead of with this machine's `go`. |
| `--go-image IMG` | With `--docker`, another Go image to compile in. |
| `--os`, `--arch` | Build for `linux`, `darwin` or `windows` on `amd64` or `arm64`. The default is this machine's. |
| `--source MODULE=DIR` | A module's source directory, holding its `go.mod`. Repeatable. |
| `--git MODULE=URL[#REF[:SUBDIR]]` | A module's git repository, cloned with this machine's `git` at `REF` (default `HEAD`). Repeatable. |
| `--module-proxy URL` | The `GOPROXY` for this build. Empty passes your module environment through. |
| `--offline` | Build from the module cache only, without network access. |

A native build keeps its module and Go caches in `linea/build` under your
user cache directory, so a second build of a similar manifest is fast and an
`--offline` build can reuse what an earlier build downloaded.

Releases follow the same rules as runtime builds ([which releases
build](runtime-builds.md#which-releases-build)). A verified release builds
with the dependency set the debugger was verified with. A release newer than
every verified one builds **unverified**: the build, the CLI's `version` and
every report it writes say so. Releases before 0.160.0 are refused.

### With your own components

Name your module in the manifest and point the build at its source:

```yaml
dist:
  otelcol_version: 0.162.0
receivers:
  - gomod: go.opentelemetry.io/collector/receiver/otlpreceiver v0.162.0
processors:
  - gomod: example.com/acme/stampprocessor v0.1.0
exporters:
  - gomod: go.opentelemetry.io/collector/exporter/debugexporter v0.162.0
```

```sh
linea build --manifest mine.yaml --docker --source example.com/acme/stampprocessor=../stampprocessor
linea build --manifest mine.yaml --docker --git example.com/acme/stampprocessor=https://git.example.com/acme/components#v0.1.0:stampprocessor
```

A module may also implement the [provider SDK](provider-sdk.md) to drive its
components during a run; the CLI compiles its provider in. A custom module's
`go.mod` must not require a Collector module above the manifest's release:
the build is refused, naming the module and both versions, before anything
compiles. Your component code runs during the build and in the CLI, so build
only sources you trust.

### For another machine

`--os` and `--arch` cross-compile, so you can build a Linux CLI on Windows or
macOS for a CI runner or an air-gapped host:

```sh
linea build --manifest mine.yaml --docker --os linux --arch amd64 --out ./linea-mine
```

A build for this machine starts the new CLI once and checks that it reports
the manifest's Collector release and modules. A cross build cannot start it,
so it says so; run `linea-mine version` on the target to check it there.

## Check what you built

```text
$ ./linea-mine version
linea 1.2.0
  collector: 0.162.0
  built with: go1.27.3
  report schema: 0.14
  inspection schema: 0.2
  event schema: 0.6
  build: custom, made with linea build
  source: example.com/acme/stampprocessor from ../stampprocessor

COMPONENTS
  MODULE                                               VERSION
  go.opentelemetry.io/collector/receiver/otlpreceiver  v0.162.0
  example.com/acme/stampprocessor                      v0.1.0
  ...
```

The first line is the version of the `linea` CLI that built it. `--format
json` adds a `build` object: `verified`, and `custom_modules` with
each replaced module's `module`, `source` and, for a git source, `commit`. A
git URL's credentials never reach the binary. The released CLI has no `build`
object.

## Use it

The built CLI is the whole `linea` CLI, so every command and flag in the
[CLI reference](README.md#commands) works the same way:

```sh
./linea-mine inspect --config collector.yaml
./linea-mine test --config collector.yaml --scenario scenario.yaml
./linea-mine run --config collector.yaml --fixture input.json --compare
./linea-mine diff --before base.yaml --after collector.yaml --scenario scenario.yaml
```

- **Reports.** Each report names the CLI's Collector release and whether it is
  verified, as a report from the built-in runtime does. A configuration that
  uses a component your manifest left out fails inspection with
  `unknown type`, as it would in a Collector built from the same manifest.
- **Missing evidence.** A manifest without `connector/count`,
  `connector/routing`, `processor/tail_sampling` or `processor/transform`
  builds a CLI without count aggregation, routing explanation, sampling
  explanation or OTTL replay. The build warns, and reports say which
  evidence is unavailable.
- **Air-gapped machines.** Copy the one file. Direct commands make no network
  connection; `scripts/debugger/airgap-check.sh` checks this for a Linux CLI
  with `LINEA_BINARY=./linea-mine` (it runs every sample in a container
  without a network).
- **CI.** Build once per manifest change, keep the binary as a pipeline
  artifact or in your own package store, and call it as in
  [Collector checks in CI](ci.md). The exit codes are the same.
- **Team servers.** `--server` commands work and send the request to the
  server. The server then runs it on its own runtimes, not on the CLI's
  Collector; use `--runtime` to pick one of the server's.

## Limits

- One Collector release per CLI: the manifest's.
- The built CLI has no embedded worker source, so it cannot run `build` or
  `runtime build`. Keep the released CLI for building.
- A team server refuses CLI builds; only the CLI builds CLIs.
- A native build needs exactly the pinned Go release. Another `go` is refused
  with `a native build needs Go <version>, this machine's go is <version>`;
  use `--docker` or install that release.

## Troubleshooting

| Message | What to do |
| --- | --- |
| `a native build needs Go …` | Add `--docker`, or install the named Go release. |
| `Collector <version> has no catalog` | The release is before 0.160.0 or not yet published. Pick a release `linea runtime versions` lists. |
| `a custom module needs a newer Collector than …` | Build at the release the module's `go.mod` requires, or lower its requirements to the manifest's release. |
| `module lookup disabled by GOPROXY=off` | An `--offline` build is missing a module. Build once with network access, or point `--module-proxy` at a proxy that has it. |
| `CLI build failed while compiling` | The tail of the Go output follows: usually a component that does not build at this release. |
| `the CLI is for linux/amd64, so the build did not start it` | Not an error: run the CLI's `version` command on the target. |
