# The CLI and the team server

Since the split (branch `feat/cli-server-split`, 2026-11), the debugger ships
as separate binaries, and only some of them link a Collector. This note
describes the mechanisms that keep the boundary. Package detail is in
[architecture.md](architecture.md), the worker protocol in
[debugger-workers.md](debugger-workers.md), and runtime and CLI builds in
[runtime-builds.md](runtime-builds.md).

## Which binary links what

| Binary | Links a Collector | Runs |
| --- | --- | --- |
| `linea` (`cmd/linea`) | Yes, the built-in registry (or a manifest's, after `linea build`) | Direct commands in process; Docker runtimes as containers; `--server` commands on a team server |
| `linea-server` (`cmd/linea-server`, `server.Main`) | No | HTTP API, web client and MCP; every run, inspection and preview goes to a connected worker |
| `linea-worker` (`cmd/linea-worker`) | Yes, the built-in registry | One runtime over stdin/stdout, `--connect` to a server, or `--pool` for server-built runtime binaries |
| `linea-builder` (`cmd/linea-builder`) | No | Runtime builds with the pinned Go toolchain for a server |

The images follow the binaries: `linea-server`, `linea-worker` and
`linea-builder` (`deploy/docker/`). A runtime image is the worker linked
with a manifest's components (entrypoint `/worker`), so the same image runs
under the CLI (as a container) and on a team server (as a connected worker,
`--connect URL --name N` with `LINEA_WORKER_TOKEN`).

## How the Collector is plugged in

- `internal/debugger/engine` links no Collector. Execution, inspection,
  OTTL evaluation and the reference run go through the `engine.Collector`
  set once by `engine.Use`; without one they fail with `ErrNoCollector`
  (platform code `unavailable`), while preparing, capturing and encoding a
  request for another runtime still work.
- `internal/debugger/engine/local` is the in-process Collector (the
  generated registry, the runtime, the mocks, the manifest's features).
  `engine/local/install` calls `engine.Use` in `init`. `cmd/linea` and
  `cmd/linea-worker` import it for that side effect; `cmd/linea-server` and
  `cmd/linea-builder` do not. Tests of executing packages call `engine.Use`
  in their `collector_test.go`.
- `internal/debugger/linkboundary` (`TestCollectorFreePackagesLinkNoImplementation`)
  lists the dependencies of every package the server may link and fails on a
  Collector component implementation, a `k8s.io` package, the registry or
  the executor. The Collector data model and component APIs are allowed.
  `TestAdaptersMapPlatformOperationsOnly` lets `cmd/linea` import only
  `engine/local/install` beyond the platform and adapters.

## The built-in runtime on a team server

The server knows the built-in release (`internal/debugger/release`, generated
from the OCB manifest) without linking it. With `Options.BuiltInOnWorkers` it
lists `built-in` as a runtime, bound to connected workers whose hello sets
`BuiltIn` (`built_in`) and reports the server's `ProductVersion`
(`builtin_runtime.go`). Only the release's own `linea-worker` sets it: every
runtime build links `-X .../worker.custom=true`. A pool worker of the server's
release serves the built-in runtime from its own executable.

A runtime on connected workers is bound with the hello it was registered or
confirmed with (`pooledRuntime`). Reports name it as kind `remote`. Its
feature gates come from that hello (`poolWorker.FeatureGates`), so an
upgrade check on a team server compares gates as a direct one does.

## The CLI on its own

- Direct commands compose the platform in process, unrestricted for host
  paths and `--import-env`, with Docker runtimes for `--runtime`.
  `cmd/linea` has no server mode; `linea serve` is gone.
- `scripts/debugger/airgap-check.sh` builds the Linux CLI, or
  takes `LINEA_BINARY`, and runs `inspect`, `test` and `run --compare` on
  every sample in a container with `--network none`.
- `linea build` (`platform.BuildCLI`) compiles `cmd/linea` with a manifest's
  components: the runtime build's plan, a generated
  `cmd/linea/custom_build.go` stamp read by `version`, and for a release
  outside the support table `-X .../release.unverified=true`, so
  `release.Verified` and every report of that CLI say unverified. Only a
  direct composition accepts the operation. User guide:
  `docs/debugger/custom-cli.md`.

## Tests across the boundary

The `cmd/linea` lab (`startLab` in `lab_test.go`) runs `server.Main` in process
on the memory store with a registration token (`lab.WorkerToken`). The Docker
parity tests (`TestABuiltRuntimeWorksThroughEveryInterface`,
`TestAnUpgradeBetweenBuiltReleasesAgreesEverywhere`) register a runtime image
with the lab and start that image's worker in a container dialing it
(`startImageWorker`; `host.docker.internal` on Docker Desktop, the host
network on Linux). A direct run and a server run agree apart from the
runtime's placement, the evidence budget (the store's per-run allowance less
1 MiB on the server, 64 MiB in the CLI) and the digest of a scenario the
connected CLI sends with its files inlined.
