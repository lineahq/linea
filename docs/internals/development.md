# Development

Build, test and validation commands in full, and the detailed testing
guidelines. `AGENTS.md` lists the everyday commands and the rules for this
repository; this file is the complete reference.

On a shared or constrained development host the full suite is not run: use
focused, sequential checks (`GOMAXPROCS=2`, `-p 1`) as `AGENTS.md` describes.
The whole-suite commands below are for other machines.

Every PowerShell script under `scripts/` has a shell twin with the same
options spelled in long form (`--packages` for `-Packages`);
`scripts/gen-proto.sh` is shell only.
This file shows the shell form first and the Windows form after it. On
Windows the shell scripts run under Git Bash.

## Build and generate

- `scripts/install-tools.sh` (Windows: `scripts/install-tools.ps1`) installs
  the pinned OCB binary under `.tools/bin/`.
- `scripts/build-collector.sh` (Windows: `scripts/build-collector.ps1`)
  recreates and compiles the debug Collector from `build/linea-runtime.yaml`
  into the ignored `dist/linea-runtime/`. With `--generate-only` (Windows:
  `-GenerateOnly`) it generates the Go source without compiling. The script
  deletes only that ignored output before rebuilding; do not clean broad
  workspace paths by hand.
- `go generate ./internal/debugger/registry` regenerates the built-in
  registry (`internal/debugger/registry/components.go`), the OTTL variant
  binding and the feature set from the OCB manifest.
- `go generate ./internal/debugger/workersrc` embeds the worker source; run it
  before `go build ./cmd/linea` for a binary that builds runtimes or runs
  `linea build`.
- `scripts/gen-proto.sh` regenerates `collector/livecapture/controlpb` from
  `api/livecapture/control/v1/control.proto`.
- `go test ./internal/debugger/api -run TestOpenAPIIsCurrent -update`
  regenerates `internal/debugger/api/openapi.json` after a change to an API or
  report type; the test fails while the committed contract is stale.
- `go run ./tools/notices -title <name> [-extra name=file] -o <file>
  ./cmd/<binary>` writes a binary's third-party notices: the license and
  notice files of every module it links for `GOOS`/`GOARCH`, plus the Go
  distribution's license. It fails when a linked module has none, so a new
  dependency without license terms stops the release and the image builds.
  The release workflow and the five product Dockerfiles run it (built for the
  host with `GOOS= GOARCH=`, run with the target's); the web build writes the
  bundled packages' licenses to `licenses.md` (Vite `build.license`), which
  `linea-server`'s notices include with Monaco's `ThirdPartyNotices.txt`.
- `go mod tidy` reconciles module dependencies; review any resulting `go.mod`
  or `go.sum` changes before committing. The root `go.mod` carries `replace`
  directives for `collector/livecapture` and `collector/provider`, so tidy
  needs no temporary modfile (`go.work` replacements are not used by tidy).
  Keep them while those modules are unpublished.

The `capture` processor and `livecapture` extension
(`github.com/lineahq/linea/collector/livecapture`) are never linked into the
debugger: the OCB manifest and the built-in registry never name them. They run
in a runtime built with them as a custom module source (`runtime build
--source`), where they run for real with their client endpoints simulated; on
the built-in registry they are stand-ins. Live capture tests use the
in-process fleet fake (`internal/debugger/fleetcapture/fleetcapturetest`).

## Versions

- `release.ProductVersion` is `0.0.0-dev` unless the build sets it at link
  time with `-ldflags "-X
  github.com/lineahq/linea/internal/debugger/release.version=<version>"`.
  A plain `go build` is a dev build.
- A release is a `vX.Y.Z` tag with a `CHANGELOG.md` section.
  `.github/workflows/release.yml` takes the version from the tag: the images
  receive it as the `VERSION` build argument, and the CLI archives are built
  with the `-X` flag above.
- Fleet binaries do not link the debugger registry: `linea-fleet` and
  `linea-fleet-livecapture` take the release with `-X main.version=<version>`
  (the `VERSION` build argument of their images) and report `0.0.0-dev`
  otherwise (`linea-fleet --version`, `linea-fleet-livecapture version`). The
  plugin's manifest reports its own plugin version, `capture.Version`, not the
  release.
- The release builds images with `--sbom=true --provenance=mode=max`, signs
  each pushed digest with cosign keyless signing (the job's OIDC identity,
  hence `id-token: write`) and signs `SHA256SUMS` into
  `SHA256SUMS.sigstore.json`. [Installing Linea](../install.md#verifying-a-release) shows the verify
  commands.
- Release images are multi-platform, `linux/amd64` and `linux/arm64`, built
  with `docker buildx`. The Go stages of `deploy/docker/` run on
  `$BUILDPLATFORM` and cross-compile with `GOOS=$TARGETOS GOARCH=$TARGETARCH`;
  QEMU runs only the `RUN` steps of final stages. Keep new build stages that
  way. The `linea-builder` image compiles runtimes for its own
  architecture (`internal/debugger/container/native.go`).
- The release also tags the Go modules users build Collectors and plugins
  with: `collector/livecapture/vX.Y.Z` and `pluginsdk/vX.Y.Z` on the release
  commit, and `collector/provider/v<provider.Version>` unless that tag exists.
  The check job refuses the release when `collector/livecapture/go.mod` or
  the root `go.mod` does not require the current `provider.Version`, when the
  root `go.mod` does not require `collector/livecapture` and `pluginsdk` at
  the release version or the example plugin does not require `pluginsdk` at
  it (bump them in the release commit, as the chart), when the provider sources changed
  since its tag without a new version, or when the major version is 2 or
  more (the module path would need `/vN`).
- A runtime build stamps the building server's version into the worker and
  marks it `worker.custom=true`, so it never claims the built-in runtime. For a
  Collector release outside the support table it also sets
  `worker.unverified=true` (`compileArgs` in
  `internal/debugger/container/build.go`). See `runtime-builds.md`.

## Web client

- `scripts/build-web.sh [--binary]` (Windows: `scripts/build-web.ps1
  [-Binary]`) installs the locked web dependencies, regenerates the API types,
  lints,
  runs the web tests and builds the client into
  `internal/debugger/webui/static/app/`, which `go build` embeds. With
  `--binary` it also builds `dist/linea-server`
  (`dist/linea-server.exe` on Windows).
- In `web/`: `pnpm dev` (Vite with `/api` proxied to 127.0.0.1:7070, the
  Compose stack's proxy; start the stack first), `pnpm test` (Vitest),
  `pnpm typecheck`, `pnpm lint` (ESLint: TypeScript's recommended rules and
  React's rules of hooks; a disable comment says why), `pnpm gen:api`.

## The CLI

Direct commands run in process; with `--server URL` they run against a team
server.

- `go run ./cmd/linea inspect --config <yaml>` validates a config and prints
  its topology; exit code 1 when it is invalid.
- `go run ./cmd/linea test --config samples/<id>/config.yaml --scenario
  samples/<id>/scenario.yaml` runs a sample.
- `go run ./cmd/linea run --config <yaml> (--fixture <otlp.json> | --scenario
  <scenario.yaml>) [--compare] [--normalize] [--format text]` runs a replay;
  exit code 2 means the reference comparison differed. Use `--normalize` for
  golden files: repeated runs then produce identical bytes.
- Local `run`, `test`, `diff`, `upgrade-check`, `inspect`, `ottl` and
  `statement` never read the host environment: `${env:NAME}` takes the
  scenario/setup value, then a host value only for a repeatable
  `--import-env NAME`, then the URI default, else a stand-in. With `--server`,
  `--import-env` is ignored: the server reads no host environment variable.
- `inspect`, `run`, `test` and `diff` take `--server URL` to run against a
  team server (the Compose stack at `http://127.0.0.1:7070`, or a server's
  https URL with `$LINEA_TOKEN`) with the same output and exit codes; the CLI
  inlines the files a scenario names, so the server reads none of them.
  Without `--server` they are direct commands: in process, local Docker for
  runtimes, unrestricted host paths and `--import-env`.
- `inspect`, `run`, `test` and `diff` take `--runtime NAME` to use a built
  runtime (empty is the built-in registry; a runtime image reference also
  works, with its binary cached in `--runtime-cache DIR`). `diff` also takes
  `--after-runtime NAME` (default `--runtime`).
- `go run ./cmd/linea test --config <yaml> (--scenario <yaml>... | --tests
  <dir>) [--report junit.xml|report.json]` evaluates scenario assertions; exit
  codes 0 pass, 1 error, 3 fail, 4 inconclusive.
- `go run ./cmd/linea diff --before <yaml> --after <yaml> (--fixture
  <otlp.json> | --scenario <yaml>) [--ignore PATH]... [--format
  text|json|markdown]` runs both configurations on one input and compares
  them; `diff --server <url> --runs <before> <after>` compares two server
  runs. The text output follows each changed record through both runs. Exit
  codes 0 same, 2 different, 4 inconclusive, 1 error.
- `go run ./cmd/linea upgrade-check --config <yaml> --to RUNTIME [--from
  RUNTIME] [--scenario <yaml>... | --tests <dir>] [--ignore PATH]... [--format
  text|json|markdown] [--server URL]` checks a Collector upgrade; exit codes 0
  compatible, 2 configuration breaks or behavior changes, 4 inconclusive, 1
  error. See `docs/debugger/ci.md`.
- `go run ./cmd/linea ottl preview --config <yaml> --processor ID --input
  <otlp.json> [--scenario <yaml>] [--runtime NAME] [--format text|json]
  [--server URL]` previews one transform processor (never verified).
  `statement candidate --config <yaml> (--proposed <yaml> | --processor ID
  --signal SIGNAL --op replace|insert|append|delete [--group N] [--index N]
  [--text T] [--expected T]) [--save [--label L]]` validates a candidate
  without running it and exits 1 when it is invalid. Local and connected JSON
  match. `statement test --server <url> --run <id> (--proposed <yaml> | the
  edit flags) [--assertions <yaml>] [--runtime NAME]` tests the candidate on
  that run's recorded configuration and inputs, prints the queued test run,
  and exits 1 for an invalid candidate.
- `go run ./cmd/linea version [--format json]` prints the debugger, Collector,
  schema and component versions.

### Commands that need a server

- `go run ./cmd/linea runs --server <url> [--scenario NAME] [--runtime NAME]
  [--result RESULT] [--limit N] [--cursor C | --all] [--format text|json]`
  lists a page of a server's runs newest first with each finished run's
  summary. Text output ends with `next page: --cursor C` when there is one, JSON output is
  the `{runs, next_cursor}` page, and `--all` follows every page. An unknown
  result exits 1.
- `runs log --server <url> --run <id> [--format text|json]` prints the
  Collector log tail a failed or partial run kept, and says on stderr when the
  run kept none. `runs show|inputs|diffs|cancel --server <url> --run <id>
  [--format text|json]` read a run's state (`show --report [--wait]` prints the
  report), root sources and stage changes, or cancel it. `runs assert --run
  <id> --scenario <yaml>` evaluates a scenario's assertions on the recorded
  run. `runs snapshot --run <id> --ref REF --out <file>` writes one snapshot
  and never overwrites. `runs summary --run <id>` prints the compact summary.
  `runs evidence --run <id> --family FAMILY [--pipeline --component --signal
  --side --entity --from-seq --to-seq --cursor --page-items --page-bytes]`
  prints one bounded evidence page and the `--cursor` that continues it. `runs
  evaluations --run <id> [--evaluation ID]` lists the recorded evaluations or
  prints one. JSON is the server's bytes.
- `runs keep|unkeep --run <id>` protects a run from retention or lifts that.
  `runs prune [--before DATE] [--project P] [--scenario S] [--runtime R]
  [--result R] [--dry-run]` deletes the finished runs retention could evict
  (DATE is `2006-01-02` in UTC or RFC 3339; only an admin prunes). `runs
  storage` prints the storage use, run counts and retention policy.
- `go run ./cmd/linea suite list|show|report --server <url> [--suite <id>]
  [--wait] [--format text|json]` lists suites and prints one suite's members
  and its report; `report` exits as `test` does. `suite rerun --server <url>
  --suite <id> [--report <file.xml|file.json>] [--format text|json]` reruns a
  suite from its members' recorded inputs on the runtime their reports name,
  waits and prints the report with `test`'s exit codes. `suite junit --server
  <url> --suite <id> --out <file.xml>` writes a finished suite's JUnit XML and
  never overwrites a file. `suite delete --suite <id>` deletes a suite with its
  member runs, and `runs delete --run <id>` deletes a finished run outside any
  suite; `--format json` prints `{"deleted": true}`.
- `go run ./cmd/linea artifacts list|show|save --server <url>` lists saved
  artifacts (`--kind`, `--limit`, `--cursor`), shows one (`--artifact <id>
  [--revision <id>|head [--out <file>]]` prints or writes a revision's content
  and never overwrites) and saves a file's bytes unchanged (`--file <f> --kind
  KIND [--label L] [--artifact <id> --generation N --head R]`); a stale edit
  exits 1 and names the current head. `artifacts revisions --server <url>
  --artifact <id> [--limit N] [--cursor C] [--format text|json]` lists an
  artifact's revisions newest first. `artifacts export --server <url> --config
  <id> --scenario <id>... --out <file.zip>` writes the CI archive and never
  overwrites a file. `artifacts import --server <url> --archive <file.zip>
  [--format text|json]` imports it back and prints what it did with each file.
- `go run ./cmd/linea samples [--server <url>] [--format text|json]` lists the
  shipped samples.
- `go run ./cmd/linea export --server <url> --run <run-id> --out
  <scenario.yaml>` writes a server run as a reviewable scenario file (header
  plus YAML; never overwrites) for CI's `test`.
- `linea-server --fleet-url <url> --fleet-token-file <file> [--fleet-allow-http]`
  enables live capture through linea-fleet's `livecapture` plugin. The token
  is read from the file, never from an argument. `capture
  collectors|points|groups|start|...` use the shared service. See
  `docs/debugger/livecapture.md`.

## Runtimes

- `go run ./cmd/linea runtime build --name NAME --manifest <ocb.yaml>
  [--source MODULE=DIR]... [--git MODULE=URL[#REF[:SUBDIR]]]... [--go-image
  IMG] [--module-proxy URL] [--offline]` builds a runtime (a debug Collector
  image) in this machine's Docker and follows the log, cancelling on interrupt
  (exit 0 built, 1 failed or cancelled). With `--server URL` the build is
  queued on the team server's builders instead: the manifest and each module
  directory it builds from, and each `--source`, are packed and uploaded, and
  `--git`, `--go-image`, `--module-proxy` and `--offline` are refused.
  `runtime builds --server URL [--limit N] [--cursor C | --all]` lists a
  server's builds.
- To use a locally built image on a server, run `runtime push NAME TARGET`,
  start workers of that image (`linea-worker --connect`, see
  `docs/debugger/remote-workers.md`) and run `runtime register IMAGE --name
  NAME --server URL`. `runtime default [NAME] --server URL` reads or chooses
  the server's default runtime.
- `runtime list [--format json]`, `runtime rm NAME`, `runtime push NAME
  TARGET` and `runtime import REF [--pull]` manage the images. `runtime
  storage [--format json]` reports their disk use. `runtime prune [--stale]
  [--caches] [--binaries]` removes replaced images (`--binaries` only the
  image binaries that direct commands cached in `--runtime-cache`).
- `runtime versions` prints the support table, `runtime catalog [--version V]`
  a verified release's components, and `runtime manifest [--version V]
  [--preset all|built-in] [--config <yaml>] [--component KIND/TYPE]...
  [--source MODULE=DIR]... [--git MODULE=URL]... [--out FILE]` writes a
  manifest from a catalog, with an entry for each component found in the
  sources. `runtime retarget --manifest <ocb.yaml> [--version V] [--out FILE]`
  moves one to another verified release.
- `list`, `rm`, `versions`, `catalog`, `manifest` and `retarget` take
  `--server URL` to act on a team server's catalog; a server stores no images,
  so `push`, `import`, `storage` and `prune` with `--server` fail as
  `unavailable`. User documentation: `docs/debugger/runtime-builds.md`.

## The team server and the Compose stack

`go run ./cmd/linea-server --config <file> [--addr 127.0.0.1:7070] [--tls-cert-file
F --tls-key-file F | --allow-http] [--drain-timeout D] [--metrics-listen ADDR]
[--mcp] [--fleet-url URL --fleet-token-file F] [--verbose]` serves the HTTP API
and embedded web client of a team server.

- `--mcp` also mounts `/mcp` on the same listener and platform; clients
  present an API token.
- `--metrics-listen` serves the build queue's counts for a builder autoscaler
  at `http://ADDR/metrics` on a listener of its own, without credentials.
- This is the only server mode: `linea-server` without `--config` is refused, and there
  is no in-memory or loopback-only mode and no `linea up`.
- The server config names the PostgreSQL and object storage, the storage limits
  (`store.storage_limit`, `store.run_limit`), run retention (`retention.runs`,
  `retention.age`), authentication, role bindings and worker registration; see
  `docs/debugger/team-server.md`.
- `${env:...}` reads only declared scenario/setup values and URI defaults; any
  other name gets a stand-in value.
- Requests read no host file: the API and MCP have no path fields (400), a
  file a scenario names is 403 `forbidden`, and a `${file:}` reference stands
  in. Startup prints this policy.
- `.linea/` is ignored for optional local state; do not put source code or
  fixtures there.

On one machine the server is the shared-platform stack: two `linea-server --config`
instances behind HAProxy, PostgreSQL, SeaweedFS, a pool worker that runs the
built-in runtime, and a builder; nothing to register. Details are in
`docs/debugger/deploy-compose.md`. It is the only way to run the web client,
HTTP API and MCP on one machine (`pnpm dev` in `web/` proxies to it):

```sh
# once: .env in deploy/compose/linea/ (docs/debugger/deploy-compose.md#starting-it)
LINEA_VERSION=dev docker compose -f deploy/compose/linea/compose.yaml up -d --build
docker compose -f deploy/compose/linea/compose.yaml down -v   # remove it and its data
```

## Go tests

- `go test ./...` runs all Go tests, including the fidelity test
  (`testdata/traces/`) that compares an instrumented run with a reference
  Collector over loopback OTLP.
- For a plain `go test ./...` on a machine with limited RAM, pass `-p 2` (or
  `-p 1`): each test binary links the whole Collector registry (about 1.6 GB
  peak on Windows), so the default parallelism of one package per CPU can
  exhaust memory. The full suite passes sequentially (`-p 1`, about 3
  minutes).
- `scripts/test-race.sh [--packages ./internal/debugger/platform] [--parallel
  2] [--cc gcc] [-- -run '<Test>' -count=1]` runs `go test -race -p
  <parallel>` with cgo enabled. `--packages` is comma separated; everything
  after `--`, or each `--test-arg`, is one `go test` argument. The C compiler
  defaults to `$CC_RACE`, `$CC`, then `cc`. On Windows, `scripts/test-race.ps1
  [-Packages ./internal/debugger/platform] [-TestArgs '-run','<Test>','-count=1']
  [-Parallel 2]` does the same; `-TestArgs` is an array, so each `go test`
  argument is its own element.
  - `--parallel` (`-Parallel`) caps the packages built and run at once
    (default 2): each race-instrumented Collector link peaks at several GB,
    and Go's default of one per CPU froze a 16 GB machine.
  - On Linux and macOS any gcc or clang does (`--cc` or `CC_RACE` picks it).
    On Windows the race detector needs a MinGW-w64 C compiler: Cygwin's
    `mingw64-x86_64-gcc-core` package (`x86_64-w64-mingw32-gcc.exe`; Cygwin's
    plain `gcc` is refused by Go). Pass `-CC` or set `CC_RACE` when it is not
    in the default Cygwin location.
  - The full suite takes about 20 minutes.
- Connected workers, one package at a time:

  ```sh
  # the worker side (connect_test.go: eight runs at once, presigned payloads,
  # a refused token)
  GOMAXPROCS=2 go test -p 1 -run 'TestAConnectedWorker|TestPayloadsTravel' \
    ./internal/debugger/worker
  # the pool and scheduler (worker_pool_test.go)
  GOMAXPROCS=2 go test -p 1 \
    -run 'TestOneConnectedWorker|TestInteractiveRequests|TestARunOnALostWorker|TestAWorkerIsRefused' \
    ./internal/debugger/platform
  # the storage half
  scripts/debugger/test-store.sh -run 'TestRunBundleURL|TestStagedObjectRoundTrip'
  ```

- Instance behavior on the memory store (`instances_test.go`):

  ```sh
  GOMAXPROCS=2 go test -p 1 \
    -run 'TestInstance|TestADrainedInstance|TestAnInspectionReaches|TestASignInCompletes|TestARequeuedRunAnotherInstance' \
    ./internal/debugger/platform
  ```

## Store, fan-out and performance

- `scripts/debugger/test-store.sh [-run TestContract] [-v]` runs the
  PostgreSQL store tests (`internal/debugger/store/pg`), including the shared
  repository contract (`internal/debugger/artifact/contract`), against
  disposable `postgres:17` and SeaweedFS (`chrislusf/seaweedfs:4.51`)
  containers on random localhost ports; extra arguments go to `go test`.
  Those tests skip without the containers; run them before a release
  ([Before a release](#before-a-release)). See `docs/internals/artifact-storage.md`.
  - The shared instances gate runs there too: `scripts/debugger/test-store.sh
    -run TestTwoInstancesAndThreeWorkersEndEveryRunWhenOneIsKilled -v` starts
    two platform services on one schema and three in-process workers, kills
    one instance while all slots run and checks every run ends.
  - `-run TestARunSubmittedOnOneInstance -v` logs what the shared queue adds
    to a run submitted on one instance, with its worker on the same or the
    other instance (submit to started, finished to seen done). It fails when a
    median exceeds 500 ms, as it would if change hints were lost.
- The fan-out gate, `scripts/debugger/fanout-compose.sh`, starts the Compose
  stack as a disposable project (two instances, `FANOUT_POOLS` pools of
  capacity 4, nothing registered), sends `FANOUT_RUNS` runs through
  `FANOUT_CLIENTS` concurrent `linea test --server`, restarts one instance
  gracefully once half of the runs were submitted, and fails unless every run
  passes. Defaults: 200 runs, 8 clients, 4 pools, port 7079, sample
  `logs-cleanup` (`FANOUT_RUNS`, `FANOUT_CLIENTS`, `FANOUT_POOLS`,
  `FANOUT_PORT`, `FANOUT_SAMPLE`). It needs Docker and Go, uses the stack's
  fixed `10.207.70.0/24` network (stop a running local stack first), and
  removes its project, volumes and secrets on exit. Run it before a
  release.
- `scripts/measure-performance.sh [--repeat 5] [--out <file>]` (Windows:
  `scripts/measure-performance.ps1 [-Repeat 5] [-Out <file>]`) builds the CLI,
  times the `testdata/perf/` workloads end to end (median, Collector startup
  included) against fixed performance targets, runs the
  `internal/debugger/engine` benchmarks and writes the results to a text file
  (default `.local/perf/`, which is git-ignored).

## Docker and native build tests

These run only with `LINEA_DOCKER_TESTS=1` (or `LINEA_NATIVE_TESTS=1`).

- `LINEA_DOCKER_TESTS=1 go test -p 1 -run TestABuilt
  ./internal/debugger/container` builds the built-in manifest in Docker and
  runs the samples on it. The first run downloads modules and compiles for
  minutes; Docker needs about 3 GB of memory.
- `LINEA_DOCKER_TESTS=1 go test -p 1 -run TestEveryReleaseRunsTheSamples
  ./internal/debugger/container` builds the built-in components at every
  release of the support table (`LINEA_RELEASES=0.160.0,...` narrows it), runs
  each sample's scenario and the `testdata/components/` reference comparisons
  on each worker, and builds a manifest without the transform processor, whose
  worker names its missing features.
- `LINEA_DOCKER_TESTS=1 go test -p 1 -run
  TestABuiltRuntimeWorksThroughEveryInterface ./cmd/linea` builds a runtime
  through the platform, registers its image with an in-process lab server and
  starts the image's worker in a container that dials the lab (Docker
  Desktop's `host.docker.internal`, or the host network on Linux). The direct
  CLI, HTTP and MCP then give the same evidence on it, apart from where it ran
  (container or connected worker), the evidence budget and the inlined
  scenario's digest, and the built-in runtime the same apart from the runtime.
- `LINEA_DOCKER_TESTS=1 go test -p 1 -run 'TestCustom|TestNewerReleases'
  ./internal/debugger/platform` builds the stamp processor from a directory
  and a git tag, refuses a moved core module, fails an offline build missing a
  module and builds unverified through a local module proxy.
- `LINEA_DOCKER_TESTS=1 go test -p 1 -run
  TestAnUpgradeBetweenBuiltReleasesAgreesEverywhere ./cmd/linea` builds a
  configuration's components at 0.160.0 and 0.162.0 offline and checks one
  upgrade directly and, on both images' workers connected to a lab server,
  through the CLI, HTTP and MCP: the same verdict, feature gates and runs
  apart from where they ran (it needs both releases' modules in the cache
  volume).
- `LINEA_DOCKER_TESTS=1 go test -p 1 -run PushesToARegistry
  ./internal/debugger/container` runs `registry:2.8.3` on a loopback port and
  exports images to it with htpasswd basic auth and with token auth (the test
  serves the token realm; the credentials come from a credential helper, the
  test binary as `docker-credential-fake`). It pushes nowhere else and takes
  seconds.
- `LINEA_NATIVE_TESTS=1 go test -p 1 -run TestANativeBuild
  ./internal/debugger/container` builds a two-component runtime with the
  host's `go` (linux, `container.GoVersion`; `go generate` is not needed, the
  test packs the checkout) the way `linea-builder` does, and with
  `LINEA_DOCKER_TESTS=1` also in the Go image, and checks that the two
  binaries are identical. `LINEA_NATIVE_WORKDIR` keeps the caches between
  runs. The unit tests of the native builder (`go test -run
  'Native|ModuleArchive' ./internal/debugger/container`, `go test
  ./cmd/linea-builder`) use a stand-in `go` and compile nothing.

Fleet and Collector component checks are listed in `AGENTS.md`
(`scripts/fleet/`, `scripts/collector/dev.sh`).

## Before a release

CI (`.github/workflows/ci.yml`) runs on pull requests and by hand, not
on push, and only builds the five product images (`linea-server`, `linea-worker`,
`linea-builder`, `linea-fleet`, `linea-fleet-livecapture`), the ones
`release.yml` publishes. Tests and the test-only images run locally,
so before tagging a release run these on a machine with Docker and enough
memory, one at a time, and record the result in the pull request:

- Each module in `go.work` (`.`, `collector/distribution`,
  `collector/livecapture`, `collector/provider`, `pluginsdk`, `collector/tools/testlab`,
  `examples/fleet-agent-summary-plugin`): `GOWORK=off go mod tidy -diff`,
  then `go build ./...`, `go vet ./...` and `go test -p 1 ./...`.
- The Collector 0.160 runtime with OTTL `since0160`: `go generate
  ./internal/debugger/workersrc`, then `linea runtime manifest --version
  0.160.0 --config samples/logs-cleanup/config.yaml --out m160.yaml`, `linea
  runtime build --name ci-160 --manifest m160.yaml` and `linea run --config
  samples/logs-cleanup/config.yaml --scenario
  samples/logs-cleanup/scenario.yaml --runtime ci-160`.
- `scripts/debugger/airgap-check.sh` (the standalone CLI without network: it
  builds `linea` for Linux and runs `inspect`, `test` and `run --compare` on
  every sample in a container started with `--network none`).
- `linea build --manifest build/linea-runtime.yaml --out <dir>/linea-custom`
  (native with the pinned Go, or `--docker`), then `<dir>/linea-custom
  version` and a sample's `test` with it; and once with `--os linux`, checked
  with `LINEA_BINARY=<dir>/linea-custom scripts/debugger/airgap-check.sh`.
- `scripts/debugger/test-store.sh -v` (the store and repository contract).
- `scripts/debugger/fanout-compose.sh` (the fan-out gate).
- In `web/`: `pnpm install --frozen-lockfile`, `pnpm test`, `pnpm build`.
- The test-only images, from the repository root: `docker build --file
  deploy/docker/<image>.Dockerfile .` for `linea-collector`,
  `linea-fleet-supervised-collector` and `linea-fleet-agent-summary-plugin`.
- `scripts/collector/dev.sh DockerTest` and the Compose lab commands in
  `docs/collector-components/testing.md`.
- `scripts/collector/dev.sh Testbed` (the capture processor testbed with
  `GOWORK=off`).

## Testing guidelines

Use Go's standard `testing` package and table-driven tests where inputs vary.
Name tests `Test<Behavior>` and place them beside the code they cover. Add
fixture-based tests for configuration fidelity, telemetry snapshots, and
blocked exporter behavior. A change to the OCB manifest must be validated by
rebuilding the Collector and starting `dist/linea-runtime/linea-runtime
--version` (`.exe` on Windows).

Adapter changes keep `openapi.json` byte-identical unless the HTTP shape is
meant to change; map new failures through platform error codes, not
adapter-side `errors.Is` checks on lower-layer sentinels. Platform operation
tests assert codes with `ErrorCode`. Platform scheduling tests use injected
executors and explicit channels for ordering, cancellation and shutdown; do
not depend on sleeps or Collector timing. Use an explicit no-op `Prepare` only
for scheduler/storage test doubles; the production default always captures.
Keep preparation cancellation, bounded pending work, shutdown cleanup and
source mutation after admission covered by tests.

Bundle tests cover exact bytes, semantic build identity (rebuild tolerance and
schema-1 exact-executable bundles), private metadata, quota reservations,
upgrades of stored schema-1 data and process exit around admission
publication. Never expose raw bundles in run metadata; they contain
source/environment values. Store tests cover concurrent writers, limits across
instances and refused publications. Preserve report JSON precision, original
snapshot bytes and operational SSE IDs independently of normalized report IDs.
Durable history never uses ephemeral history eviction; reject admission at
quota instead. Do not claim live recorder events survive a crash before
terminal publication or that run inputs are pinned.

Configuration snapshot tests compare upstream retrieval semantics (typed
values, defaults and empty versus unset), nested/relative file URIs, limits
and source mutation across real Collector planning/startup/reference
execution. Capturing config providers at execution start does not pin queued
inputs at admission.

Keep real engine and API/CLI/MCP parity coverage for both quick and scenario
modes (`cmd/linea/parity_test.go` drives one lab team server,
`cmd/linea/lab_test.go`: a server config with trusted-header sign-in, an in-memory
store and an in-process worker of the runtime `lab`).

Environment-policy tests cover declared/default/authorized host bindings,
nested references, immutable policy copies, schema-1 snapshot metadata and
enforcement on prepared/decoded requests. Empty-signal regressions preserve
explicit resource arrays and reference equivalence for traces, metrics and
logs in both modes.

Run the Go race detector for concurrency changes (`scripts/test-race.sh`;
`scripts/test-race.ps1` on Windows); record unavailable checks explicitly
instead of treating stress tests as a substitute.

Artifact storage tests run the shared repository contract on the in-memory and
PostgreSQL stores. Preserve old-head/new-head atomicity, exact source bytes
and corruption checks.
