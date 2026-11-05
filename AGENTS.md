# Repository Guidelines

Linea is a debugger platform for OpenTelemetry Collector pipelines. This
repository holds three parts that share one Go module and one `go.work`:

- **Debugger** (`cmd/linea`, `cmd/linea-server`, `cmd/linea-worker`,
  `cmd/linea-builder`): runs an unchanged Collector
  configuration on recorded or scenario input, records every stage, and
  explains the result through a CLI, an HTTP API with a web client, and MCP.
  Docs: `docs/debugger/`.
- **Fleet** (`cmd/linea-fleet`, `cmd/linea-fleet-simulator`, `pluginsdk/`): a
  standalone OpAMP and HTTP fleet server with optional plugins, including the
  live-capture plugin (`cmd/linea-fleet-livecapture`). Docs: `docs/fleet/`.
- **Collector components** (`collector/`): the `capture` processor and
  `livecapture` extension, a test-only distribution that bundles them, and
  the provider SDK. Linea ships no Collector; users build their own. Docs: `docs/collector-components/`.

Keep this file current and short. Detail belongs in `docs/`, whose
`README.md` is the entry point with the overview diagram:

- `docs/internals/` (index in `README.md`): developer notes kept apart from
  the user docs.
  - `architecture.md`: what every debugger package owns and its contracts.
  - `development.md`: every build, test and Docker command, and the full
    testing guidelines.
  - `web-client.md`: web client architecture and its interface rules.
  - The other notes record verified Collector internals and design findings.
- `docs/install.md`: installing the CLI and a team server, release assets
  and building from source. The root `README.md` presents the product and
  links here; keep install detail out of it.
- `docs/debugger/`: debugger user and operator documentation.
- `docs/fleet/README.md`, `docs/fleet/plugins.md`, `docs/fleet/deployment.md`:
  fleet overview, plugin contract and operation.
- `docs/collector-components/README.md`: Collector components, configuration and tests.

Update the doc a change touches in the same commit.

## Layout

| Path | Contents |
| --- | --- |
| `cmd/linea/` | Debugger CLI, one file per command: `inspect`, `run`, `test`, `diff`, `upgrade-check`, `config-bisect`, `runs`, `suite`, `artifacts`, `export`, `ottl`, `statement`, `samples`, `runtime`, `capture`, `fleet`, `audit`, `token`, `access`, `project` (with `library`), `login` (with `logout`), `build`, `version`; runs the in-process Collector, no server |
| `cmd/linea-server/` | Team server binary: HTTP API, web client and MCP; links no Collector (flags in `docs/internals/architecture.md`) |
| `cmd/linea-worker/` | Worker process for runtimes (stdin/stdout, or `--connect` to dial a team server) |
| `cmd/linea-builder/` | Runtime builder: dials a team server and compiles runtimes natively with the pinned Go toolchain |
| `cmd/linea-fleet/`, `cmd/linea-fleet-simulator/` | Fleet server and the OpAMP agent simulator |
| `cmd/linea-fleet-livecapture*` | Live-capture fleet plugin, its client and its fixture |
| `internal/debugger/server/` | What `linea-server` runs: server config, flags, HTTP, TLS, metrics listener, drain |
| `internal/debugger/platform/` | The application service every debugger interface uses: run queue, history, artifacts, runtimes, captures |
| `internal/debugger/api/`, `mcp/`, `client/` | HTTP, MCP and connected-CLI adapters over the platform |
| `internal/debugger/engine/` | `Request` to `Report`: Collector execution, analysis, report and bundle encoding |
| `internal/debugger/runtime/`, `instrument/`, `mock/`, `standin/` | In-process Collector, factory instrumentation, mocks, stand-in components |
| `internal/debugger/artifact/`, `store/pg/`, `store/memory/` | The storage contract and its shared suite, the PostgreSQL and S3-compatible object store a team server uses, and the in-memory store for tests |
| `internal/debugger/analysis/`, `ottlreplay/`, `routing/`, `sampling/`, `aggregation/` | Evidence derived from recorded events |
| `internal/debugger/worker/`, `container/` | Out-of-process runtimes: the worker protocol (stdin/stdout, or a worker that dials the team server) and Docker runtimes |
| `internal/debugger/registry/` | Built-in component factories, generated from the OCB manifest |
| `internal/debugger/compat/ottl/since0160/` | OTTL variant for Collector 0.160, its own module (see below) |
| `internal/fleet/` | Fleet server: `api`, `gateway` (OpAMP), `inventory`, `postgres`, `configmgmt`, `plugins`, `auth`, `app`, `agentsimulator`, `fleet` (shared types and store contract) |
| `internal/fleetplugins/livecapture/` | Live-capture plugin library: API, authorization, storage, migrations, gRPC/OTLP handling |
| `pluginsdk/` | Own module, standard library only: public Go SDK for fleet plugins (`pluginsdk/metrics` for metrics helpers) |
| `collector/livecapture/` | Own module: `captureprocessor`, `livecaptureextension`, `livecapturedebug`, generated `controlpb` |
| `collector/distribution/` | Own module: OCB builder config and generated test-only Collector distribution (never released) |
| `collector/provider/` | Provider SDK, a nested module that imports only the standard library |
| `collector/tools/` | Collector test tools: `testlab` and `testbed` (capture processor benchmarks on the Collector Contrib testbed) are their own modules; `smoke` and `capture-smoke` are stdlib-only `main` files without a `go.mod`, which the Collector image builds |
| `api/livecapture/control/v1/control.proto` | The one source of the live-capture control protocol; generated Go lives in `collector/livecapture/controlpb` |
| `build/` | OCB manifest `linea-runtime.yaml` and the release support table `versions/` |
| `web/` | Web client (React, TypeScript, Vite, pnpm): shell `src/ui/`, design system `src/ds/`, one folder per destination (`debugger`, `playground`, `capture`, `fleet`, `runtimes`, `saved`, `audit`, `access`) |
| `samples/`, `examples/`, `testdata/` | User-pickable samples, CI, Kubernetes, fleet plugin and live-capture examples, test fixtures |
| `deploy/docker/` | One `<image>.Dockerfile` per image; the build context is the repository root |
| `deploy/charts/linea/` | Helm chart for the debugger team server |
| `deploy/compose/fleet/`, `deploy/compose/collector/`, `deploy/compose/linea/` | Compose files for the fleet server (and its overlays), for the Collector lab and for the debugger's shared-platform stack |
| `scripts/`, `scripts/fleet/`, `scripts/collector/` | Debugger, fleet and Collector scripts |
| `tools/fleet-capacity/` | Fleet capacity harness and agent simulator runs |
| `tools/notices/` | Generator of each shipped binary's `THIRD_PARTY_NOTICES.txt` (release archives and images) |
| `docs/` | One folder per part: `debugger/`, `fleet/`, `collector-components/`; developer notes in `internals/` |

## Modules and go.work

The root module is `github.com/lineahq/linea`. Nested modules:
`collector/livecapture`, `collector/distribution`, `collector/provider`,
`collector/tools/testlab`, `collector/tools/testbed`,
`pluginsdk`, `examples/fleet-agent-summary-plugin`,
`internal/debugger/compat/ottl/since0160` and the test fixture
`testdata/runtimes/stampprocessor`. `go.work` lists the root and every
nested module except the last three; `collector/tools/testbed` stays out
because contrib's testbed would raise the workspace's dependency versions,
so run it with `GOWORK=off`. The root `go.mod` reaches
`collector/livecapture`, `collector/provider` and `pluginsdk` through
`replace` directives. `pluginsdk` imports only the standard library, so a
plugin requires it without the root module's dependencies; the example
plugin requires only `pluginsdk`.

`internal/debugger/compat/ottl/since0160` needs `pkg/ottl` v0.160, which the
root module (on 0.162) cannot require, and it has no `go.sum` of its own.
Runtime builds pack its Go files without its `go.mod` and compile it as a
package of the build module. Test it that way, by building a 0.160 runtime
(see "Before a release" in `docs/internals/development.md`).

Run `go build`, `go vet` and `go test` per module (`go -C <dir> ...`); `./...`
in the root does not enter nested modules.

## Rules

- **Never alter a user's Collector configuration** to make it debuggable,
  and never refuse an unmodified one to improve a reproducibility label.
  Keep user YAML, fixtures and scenarios separate from source code.
- **Platform first.** Each debugger capability is one operation in
  `internal/debugger/platform/operations.go`. Adapters (HTTP, CLI, MCP) only
  map platform operations and error codes;
  `TestAdaptersMapPlatformOperationsOnly` enforces the import boundary. Keep
  quick and scenario modes first-class in every interface.
- **Contracts are generated.** After changing a debugger API or report type,
  run `go test ./internal/debugger/api -run TestOpenAPIIsCurrent -update`, then
  `pnpm gen:api` in `web/`. Otherwise `openapi.json` stays byte-identical.
  After changing `control.proto`, run `scripts/gen-proto.sh`.
- **One Collector release line.** Pin all Collector modules to the OCB
  manifest's release (currently 0.162.0); never use `@latest`. After a
  manifest change, run `go generate ./internal/debugger/registry` and update
  `go.mod` in the same commit.
- **Sensitive data.** Never expose raw bundles, environment values or capture
  secrets in run metadata, list operations or exports. Never log credentials,
  raw or effective configuration, environment, or telemetry.
- **Contract changes need explicit review.** Treat new platform exports, error
  codes, persisted formats, HTTP or MCP shapes, host reads and new web
  interface rules as contract changes: flag them instead of deciding alone.
- **Record verified behavior.** Record verified Collector behavior in
  `docs/internals/`, naming the release it was verified against.
- **Web views.** Read the interface rules in
  `docs/internals/web-client.md` before changing a view. Never replace
  a dirty draft without an inline confirmation, and keep pure logic in tested
  `.ts` modules.
- **Fleet core stays generic.** Keep the debugger, Collector runtime
  registries and capture domains out of fleet core. Live capture is entirely
  plugin-owned: APIs and schemas, domain authorization, gRPC/OTLP endpoints,
  execution, delivery, storage and dependencies. Core and `pluginsdk` provide
  generic plugin mechanisms only, without live-capture constants, types,
  routes or identity-specific branches. Plugins must work with unmodified
  Collector extensions.
- **Plugins use only the SDK.** `internal/fleetplugins/livecapture` imports
  `pluginsdk` and the generated `collector/livecapture/controlpb` package, and
  never `internal/fleet` or debugger packages. It keeps all capture schemas,
  policy, state and storage, uses its own database, migration and runtime
  roles and checksummed migrations, persists metadata only, and keeps telemetry
  in bounded memory. One active database authority; no automatic execution
  replay after recovery.
- **Fleet reporting is read-only toward agents.** Reporting must never
  dispatch configuration, restart, package or capture commands. The only
  exception is opt-in config management: with `--config-management`, the
  reconcile rule in `internal/fleet/configmgmt`, called from
  `internal/fleet/gateway/config.go`, may send remote config to agents whose
  credential allows it. Nothing else may send it.
- **Fleet isolation and bounds.** Preserve separate agent and management
  credentials and listeners. Bound inventory, connections, message sizes and
  API result pages. Docker Compose is the fleet deployment path: preserve
  distinct migration and runtime roles, explicit migrations, a non-root
  runtime and persistent storage.
- **Databases.** Never run database tests or restore fixtures against a
  user's database. Use the disposable, resource-bounded test scripts and
  explicitly supplied disposable fixture URLs.

## Testing

Do not run the full test suite on a shared or constrained machine.

- Run focused Go tests one at a time:
  `GOMAXPROCS=2 go test -p 1 -run '<Test>' ./internal/debugger/<pkg>`. Each
  debugger test binary links the whole Collector registry (about 1.6 GB). Run Go
  checks sequentially with `-p 1`; avoid parallel tests and load or stress runs.
- Race detector. Always pass both the package and the test arguments, because
  it defaults to the whole module:

  ```sh
  scripts/test-race.sh --packages ./internal/debugger/<pkg> -- -run '<Test>' -count=1
  ```

  On Windows, `scripts/test-race.ps1` takes the same as an array:
  `-Packages ./internal/debugger/<pkg> -TestArgs '-run','<Test>','-count=1'`.
  One string reaches `go test` as a single argument and fails to build.
- Web: `pnpm test <file>` in `web/`. Run capture tests with
  `--maxWorkers=1 --no-file-parallelism`.
- Docker tests run only with `LINEA_DOCKER_TESTS=1`. See
  `docs/internals/development.md` for the list.
- Debugger store: `scripts/debugger/test-store.sh` runs the pg store tests and
  the shared repository contract against disposable PostgreSQL and SeaweedFS
  containers.
- Debugger fan-out: `scripts/debugger/fanout-compose.sh` starts the Compose
  stack as a disposable project and sends 200 runs through concurrent
  `linea test --server` while restarting one instance. It needs Docker and runs
  minutes; do not run it on a shared or constrained machine.
- Fleet: `scripts/fleet/check.sh` for focused checks,
  `scripts/fleet/test-postgres.sh` for PostgreSQL tests (disposable container),
  `scripts/fleet/smoke-docker.sh` for the Docker smoke. Live capture plugin:
  `scripts/fleet/livecapture/`. Collector components:
  `scripts/collector/dev.sh DockerTest`. Capture processor benchmarks:
  `scripts/collector/dev.sh Testbed` (minutes of 10k items/s load; not on a
  shared or constrained machine). On Windows use the `.ps1` twin where one
  exists.
- Use Go's `testing` package with table-driven tests, named `Test<Behavior>` and
  placed beside the code they cover. Scheduling tests use injected executors and
  channels, never sleeps. Assert platform errors with `ErrorCode`.
- Report unavailable checks explicitly; a stress test does not stand in for the
  race detector. Distinguish protocol-client tests from tests with the real
  Collector distribution.

## CI

`.github/workflows/dco.yml` checks every pull request commit for a
`Signed-off-by` trailer (`CONTRIBUTING.md`). `.github/workflows/ci.yml` runs on
pull requests (and by hand from the Actions tab), not on push, and only builds the product images (`linea-server`,
`linea-worker`, `linea-builder`, `linea-fleet`, `linea-fleet-livecapture`)
with the repository root as context. Tests and the test-only images run
locally: before a release, run the checklist in "Before a release" in
`docs/internals/development.md` (every module, the 0.160 runtime, the store,
the fan-out gate, web, the test-only images, Collector live capture and the
testbed).

`.github/workflows/release.yml` runs on a `vX.Y.Z` tag that has a `CHANGELOG.md`
section. The tag is the version: builds set `release.ProductVersion` with `-X`
(`release.VersionVariable`), images take it as the `VERSION` build argument,
and every other build reports `0.0.0-dev`. Runtime builds stamp the building
server's version into the worker, and for a Collector release outside the
support table also set `worker.unverified=true` (`compileArgs` in
`internal/debugger/container/build.go`). The workflow pushes the five product images
(not the test-only `linea-collector`, `linea-fleet-supervised-collector` and
`linea-fleet-agent-summary-plugin`) to
`ghcr.io/<owner>/<image>:<version>` for `linux/amd64` and `linux/arm64` with
an SBOM, provenance and a cosign signature, builds the `linea` CLI archives
(`SHA256SUMS` signed with cosign), packages
the Helm chart, tags the `collector/livecapture`, `collector/provider` and `pluginsdk` Go
modules (`docs/internals/development.md`) and creates the GitHub release.

## Everyday commands

```sh
# Direct commands run in process; no server is needed.
go run ./cmd/linea inspect --config <yaml>
go run ./cmd/linea run --config <yaml> --scenario <yaml> [--compare] [--normalize]
go run ./cmd/linea test --config samples/<id>/config.yaml --scenario samples/<id>/scenario.yaml
# The web client, HTTP API and MCP need a team server; locally that is the
# Compose stack (docs/debugger/deploy-compose.md). `linea-server` refuses to
# start without --config.
LINEA_VERSION=dev docker compose -f deploy/compose/linea/compose.yaml up -d --build   # after a .env in deploy/compose/linea/ (docs/debugger/deploy-compose.md)
go generate ./internal/debugger/registry      # after an OCB manifest change
go generate ./internal/debugger/workersrc     # before building a binary that builds runtimes
./scripts/build-web.sh               # web tests and build, embedded by go build
./scripts/build-collector.sh         # debug Collector into ignored dist/
```

```powershell
./scripts/build-web.ps1
./scripts/build-collector.ps1
```

## Style and commits

- Use idiomatic Go formatted with `gofmt`. Name files by responsibility, and
  export identifiers only where another package needs them.
- Keep Collector-version adaptation and factory instrumentation out of
  HTTP, CLI and UI code.
- Write Conventional Commits, `type(scope): imperative summary`, such as
  `feat(runtime): capture processor egress`. Keep each commit focused, and
  add a body for behavior or compatibility changes. Do not commit secrets.
- In a pull request, list the validation commands. Call out any change to
  Collector fidelity, offline isolation or sensitive telemetry.
