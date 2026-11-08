# Runtime builds: versions, sizes and container latency

Findings about building runtimes, with Go 1.27.2. The debugger is pinned to Collector core
v1.68.0 / v0.162.0 and contrib v0.162.0. The sections on later releases name
the release they were verified against.

## The debugger against older Collector releases

A scratch copy of the debugger was pinned to 0.161 and 0.160 (contrib and
core unstable modules `v0.16x.0`, core stable `v1.6x.0`) and compiled with
`go build ./...` and `go vet ./...` (test files included).

- Stable contrib modules break the version mapping: `k8sattributesprocessor`
  is `v0.160.0` in 0.160, `v1.0.0` in 0.161 and `v1.1.0` in 0.162. A
  manifest rewrite that only replaces `v0.162.0` leaves `v1.1.0`, which
  requires core v1.68.0, and minimum version selection quietly pulls the core
  back to 0.162. Version sets must come from the release's manifest, never
  from rewriting version strings.
- `go mod tidy` resolves a missing package from the latest release (it
  pulled 0.163 modules and raised core to v1.69.0). Version-set builds must
  run with every module pinned and `GOPROXY=off` or `-mod=readonly`, never
  tidy against the network.
- **0.161** (core v1.67.0): one break. `pkg/ottl/xottl` and
  `pkg/ottl/contexts/xprofile` do not exist before 0.162; the debugger's OTTL
  replay uses their experimental and profile converters (the version-specific
  OTTL calls are in `internal/debugger/compat/ottl`, see below). Without
  them everything compiles, vets and `TestSamplesRunAsDocumented` passes.
- **0.160** (core v1.66.0): the 0.161 break plus two API changes in
  `pkg/ottl`, all in the packages that replay OTTL (13 compiler errors):
  - the context constructors are `ottl<context>.NewTransformContextPtr`
    (0.161 renamed them `NewTransformContext`; 8 contexts);
  - `SchemaURLItem` lives in the internal package
    `contexts/internal/ctxcommon` (0.161 exported it as
    `ottl.SchemaURLItem`); a local interface with `SchemaUrl` and
    `SetSchemaUrl` replaces it.

  With those three changes it compiles, vets and the samples pass.

All differences between 0.160 and 0.162 fall in the packages that replay OTTL
with the upstream APIs; the runtime, instrumentation, analysis, config and
mock packages compile unchanged across the three releases. The
version-specific compatibility package for OTTL construction
(`internal/debugger/compat/ottl`) contains them.

## Deprecation warnings as diagnostics

Verified by reading the 0.162.0 sources.

- `Unmarshal` and `Validate` have no logger. Components that detect a
  deprecated setting while unmarshalling keep it and log later:
  `config/confighttp` collects `deprecationWarnings` (`client.go:236-252`,
  `server.go:228`) and logs them in `ToClient`/`ToServer`, called from a
  component's creation or `Start`.
- Most deprecation warnings are logged at creation, through the
  `TelemetrySettings.Logger` the factory receives:
  - deprecated component type aliases, by the service builders
    (`service/internal/builders/builders.go:33-41`,
    `"%q alias is deprecated; use %q instead"`, called from every
    `Create*`);
  - `spanmetrics` (`connector.go:136-138`, `DimensionsCacheSize`);
  - `routing` (`router.go:233-236`, the `request` context).
- The debugger's inspection never creates components (config resolution
  stops at `provider.Get` and `Validate`), so it sees none of these. Runs do
  create them, but `internal/debugger/runtime` gives components `zap.NewNop()` unless
  the CLI runs with `--verbose`, so the warnings are discarded.

They can become diagnostics with a recording `zapcore.Core` teed into the
logger components receive (like `instrument.WrapCore`), keeping `Warn` and
above with the component taken from the builders' logger fields. Inspection
would need a creation pass (build the pipelines with receivers and exporters
substituted, never start them). Limits: warnings logged at `Start` or on
data are missed by such a pass (runs see them); messages are not a stable
contract, so a diagnostic carries the text, not a structured "deprecated"
code; entries without component fields cannot be attributed; stand-ins log
nothing.

## Building every component in Docker

The debugger's worker (`cmd/linea-worker`) was built with every core
and contrib component of the 0.162.0 `otelcol-contrib` manifest (250
modules) in `golang:1.27.2-bookworm` (852 MB), capped at 4 CPUs and 5 GB of
memory, with the registry generated from that manifest
(`internal/debugger/registry/gen`).

- Upstream's own manifest needs three changes before it builds here:
  - It has no `dist.otelcol_version`; `internal/debugger/manifest` derives
    the version from the core modules.
  - It replaces `go.opentelemetry.io/obi` with a local source tree that a
    release script prepares. The OBI receiver (Linux eBPF) was excluded.
  - Keep its `replaces` (`hamba/avro` is replaced by a maintained fork).
- `go get` of the 250 modules grew the module graph from
  301 to 1091 requirements (1840 modules in `go list -m all`). No core or
  contrib module moved off 0.162.0 / 1.68.0. Fifteen third-party modules the
  debugger also uses moved up, for example `golang.org/x/exp`,
  `otelhttp` v0.70.0 to v0.71.0 and `genproto/googleapis/rpc`.
- Module cache: 5.1 GB extracted, of which 1.1 GB is the download cache (the
  zips and `.mod` files a `GOPROXY=file://` directory serves). A builder image
  that ships only the download cache and extracts on demand is about 2 GB
  rather than 6 GB.
- Compiling takes memory, not just time.
  `github.com/DataDog/datadog-api-client-go/v2/api/datadogV2` (generated code)
  was killed by the 5 GB limit with two packages compiling at once, and again
  alone. It compiled under 5 GB with GOGC=25 and `-gcflags=<that
  package>=-c=1` (one compiler backend worker). Setting the flag for that
  one package keeps every other package's build cache valid. The builder sets
  both (`heavyPackages` and `compileArgs` in
  `internal/debugger/container/build.go`).
- A full build takes tens of minutes of compiling; a relink after a manifest
  change with a warm cache takes minutes. The Go build cache reached 6.4 GB.
- The worker binary is 512 MB (`-trimpath`, not stripped); the default
  manifest's worker is 122 MB.
- **The full worker panics at start-up** under Go 1.27.2:
  `crypto: RegisterHash of unknown hash function` in
  `github.com/SermoDigital/jose/crypto` (2018), which `asapauthextension`
  imports. Go 1.27 refuses hash IDs it does not know. A runtime build must
  therefore start the worker and wait for its hello before the build counts
  as successful. A component that cannot start under the builder's Go
  toolchain fails the build with the panic. Without `asapauthextension`, all
  other 249 components start. Which Go toolchain upstream used for its
  0.162.0 release was not checked.

## Offline subset builds

A runtime for a manifest that uses only some of the components (the
debugger's own `build/linea-runtime.yaml`) built in a container with
`--network none` and `GOPROXY=off` from the full set's module cache:

- **With the full set's `go.mod`**: it builds offline into a 122 MB worker. The builder resolves every build
  against the full dependency set's requirements.
- **With the debugger's own `go.mod`**: it does not build. Eleven imports need
  versions the cache does not hold: the lower versions of the fifteen modules
  above. A runtime built from the default manifest therefore links slightly
  different third-party versions than the built-in runtime (and has another
  runtime identity), unless the builder image also carries the debugger's own
  versions, about fifteen extra modules.

## Container round trips

A long-lived worker in a `FROM scratch` image (`--network none`, read-only
root, non-root user), driven by `internal/debugger/worker.Client` through
`docker run -i`, runs the samples about as fast as the in-process engine: the
bundle and the report cross two pipes and the Docker relay, which adds a few
milliseconds per run. Starting a container and receiving its hello takes a
fraction of a second for the default manifest and seconds for a worker with
every component, whose package initialization is much larger. Idle worker
memory is tens of MiB at most, so the per-run memory limit comes from the run,
not the worker.

Long-lived containers are therefore cheap enough that a run on a custom
runtime is not noticeably slower than a built-in run, while starting a
container per run would add up to seconds.

## Building and running in Docker

`internal/debugger/container` drives the `docker` CLI. Verified with
`golang:1.27.2-bookworm` (by digest), building the debugger's own manifest with
the build container capped at 3 GB and 2 CPUs:

- The build module goes into the Go container as a tar on standard input,
  and the worker binary comes back on standard output (`cat`). With the
  compiler's output sent to standard error, nothing has to be mounted from
  the host. `docker build -` with a tar context on standard input works from
  Windows and packages the `FROM scratch` image without a build directory.
- A rebuild with the warm cache volumes works with `--network none` and
  `GOPROXY=off`, image and start check included.
- With every module pinned by the source's `go.mod` and `-mod=readonly`, the
  build downloads exactly the dependency set and never moves a version.
- The worker in the container gives the same normalized reports as the
  in-process engine for every sample in quick and scenario modes. It does
  so only after one fix. A bundle captured on Windows records Windows
  absolute paths (`D:\...\input.json`), which Linux's `filepath.IsAbs`
  does not treat as absolute, so the worker could not find a scenario's
  input files. The engine resolves scenario file names with the rules
  of the host the bundle records (`internal/debugger/engine/hostpaths.go`).
- A run container with `--network none` completes a configuration whose
  `otlp` exporter names a public address. The exporter is substituted and
  captured, as in process, so no connection is attempted.
- A worker that panics in `init` makes `worker.Start` fail with its log. The
  build reports it as the stage "starting the worker" and removes the image.

## Versions in a built runtime

- Every binary reports `release.ProductVersion`: `0.0.0-dev` unless the build
  sets it at link time with `-X
  github.com/lineahq/linea/internal/debugger/release.version=<version>`
  (`release.VersionVariable`; a leading `v` is dropped). A plain `go build`
  is a dev build. A release takes the version from its tag
  (`.github/workflows/release.yml`): the `VERSION` build argument of the
  images and the `-X` flag of the CLI archives.
- A runtime build stamps the building server's `ProductVersion` into the
  worker with the same flag and into the image label
  `io.linea.product-version` (`compileArgs` in
  `internal/debugger/container/build.go`). It also sets
  `github.com/lineahq/linea/internal/debugger/worker.custom=true`, so the
  worker's hello never claims the built-in runtime (`Hello.BuiltIn`). A builder dialing a server must
  carry the server's product version, or the server closes the connection.
- For a Collector release outside the support table, `compileArgs` also sets
  `github.com/lineahq/linea/internal/debugger/worker.unverified=true`. The
  worker's hello then reports `Verified: false`, which the platform checks
  against the build's own verification (`builders.go`) and shows as `verified`
  on the runtime.

## CLI builds

`linea build` (`platform.BuildCLI`, `container.Native.BuildCLI` or
`Docker.BuildCLI` in `internal/debugger/container/cli.go`) reuses the runtime
build's plan: `cliPlan` calls `plan` with the same manifest, dependency set
and sources, so module checks, verification and custom modules behave
exactly as for a runtime, then adds one generated file and compiles
`./cmd/linea` from the embedded source instead of `./cmd/linea-worker`.

- The embedded source holds `cmd/linea` for linux, windows and darwin, but
  never the source archive itself, so a built CLI has no `workersrc` and
  refuses `build` and `runtime build`. `cliPlan` refuses a source without
  `cmd/linea/main.go` (a debugger built before the CLI was packed).
- `cmd/linea/custom_build.go` (`cliStampFile`) sets `customBuild` with the
  build's verification and each custom module's module, origin and commit;
  `version` prints it and adds `build` to its JSON. The release CLI has no
  such file. `stampOrigin` drops a git URL's userinfo, since the stamp is
  compiled into the binary.
- `cliLDFlags` links with `-s -w` and stamps the building CLI's
  `release.ProductVersion`, as runtime builds stamp the worker; there is no
  `worker.custom` flag, because the CLI's in-process runtime is reported by
  its Collector release and verification rather than as the built-in one.
- The gate generator always runs for the host; the CLI compiles for
  `GOOS`/`GOARCH`. A CLI for the host is started once (`checkCLI`): its
  `version --format json` must name the manifest's Collector release and
  every manifest module, or the build fails. A cross build sets
  `CLI.Checked` false. The binary is written beside `out` and moved over it
  only after the check (`moveFile` copies across volumes).
- Failures after the steps start are the runtime build's `BuildError` with
  `What: "CLI"` (`cliError`), so messages read "CLI build failed while
  compiling". A native build needs `go` to be `container.GoVersion`; the
  platform maps a missing or different go, and missing Docker, to
  `unavailable`.

Verified with Go 1.27.3 on Windows (native) and in the pinned image,
2026-11-05: CLIs at 0.160.0, 0.162.0 (with the stamp processor from a
directory) and 0.164.0 (unverified), and a linux/amd64 cross build.

## Custom modules and module sources

Verified with Go 1.27.2 in the pinned image, Collector v0.162.0:

- A custom module's own `go.mod` requirements join the build's module graph,
  so they can move a Collector module (MVS picks the higher version). The
  builder therefore resolves with `go list -mod=mod -deps` first and
  compares the resolved `go.mod` with the release before compiling with
  `-mod=readonly`.
- With module graph pruning, a requirement that no imported package needs
  is never loaded: an offline build succeeds although that module is not in
  the cache. Only an imported missing module fails, with
  `module lookup disabled by GOPROXY=off`.
- From a container on Docker Desktop (Windows), `host.docker.internal`
  reaches a server bound to the host's 127.0.0.1, so a test module proxy
  needs no wider listener. A directory laid out like
  `$GOMODCACHE/cache/download` served over HTTP is a working `GOPROXY`
  (with `GOSUMDB=off`).
- A private proxy over TLS (Go 1.27.2 on Linux,
  `TestNativeModuleDownloadsUseTheBuildersCredentialsAndCA`): the go command
  sends netrc credentials only over HTTPS and matches a `machine` against
  the URL's host *with* its port, so `machine 127.0.0.1` gives nothing to
  `https://127.0.0.1:41427` (401); `machine 127.0.0.1:41427` works.
  `SSL_CERT_FILE` naming the proxy's CA is enough for the handshake;
  without it the download fails on the certificate. Go reads
  `SSL_CERT_FILE` and `SSL_CERT_DIR` on Linux only.
- `git clone --no-checkout --config core.autocrlf=false` followed by
  `rev-parse REF^{commit}` (falling back to `origin/REF` for a branch) and a
  detached checkout gives the commit's bytes for any branch, tag or commit
  of a local bare repository.

## One debugger for every release from 0.160

Verified against the upstream sources of pkg/ottl and the transform
processor at v0.160.0, v0.161.0 and v0.162.0:

- The OTTL transform context constructors changed twice: 0.160 has
  `NewTransformContextPtr` (and keeps `ottl.SchemaURLItem` internal, so the
  debugger declares its own interface with the same methods), 0.161 renamed
  it `NewTransformContext`, and 0.162 kept that name.
- From 0.162 the transform processor's function sets and filterottl's
  converters wrap the standard ones with
  `xottlfuncs.WithExperimentalConverters` and
  `xprofilefuncs.WithProfileConverters`; before 0.162 they are the standard
  sets. A replay that skips the wrapping on 0.162 fails to parse statements
  using the experimental converters.

How the debugger absorbs this:

- Every OTTL API that differs lives in `internal/debugger/compat/ottl/since<NNNN>`,
  named by the first release it applies to. The generated
  `internal/debugger/compat/ottl/variant.go` imports the newest variant at or before
  the manifest's release (`manifest.OTTLVariant`); a release newer than the
  table gets the newest variant, and only a failing build shows that a new
  variant is needed. No build tag selects a variant, so `go vet` and the IDE
  see one consistent package.
- `since0160` is a nested module: it needs `pkg/ottl` v0.160, which the
  root module (on 0.162) cannot require. The worker source packs its Go
  files without its `go.mod`, so in a build it is an ordinary package of
  the build module, compiled against the release's dependency set.
- Explanations that replay a component with its own code (transform,
  count, routing, tail_sampling, filter) are optional features: they compile in only
  when the manifest has the component, through the generated
  `internal/debugger/featureset`. A runtime without one skips that analysis and says
  so (`missing_features` in its hello and reports); a build warns.
- Limits: the environment mocks (filelog, k8sattributes, spanmetrics), the
  tail_sampling decision hooks in `internal/debugger/instrument` and the run
  timeline still import their components unconditionally, so every
  runtime's build carries those modules whatever the manifest names.

Pitfall, verified with Go 1.27.2 in the pinned image: the build archive
stamps every file with one fixed time, and Go's file cache in the shared
build cache volume trusts a file's path, size and modification time. The
0.160 and 0.161 `variant.go` have the same size (they differ only in
`0160`/`0161`), so the 0.161 build compiled the 0.160 variant from the
cache, although `go list` without that cache showed the right import.
Builds therefore extract the archive with `tar -m` (extraction time); the
compiled packages are still reused, keyed by content.

Dependency sets of older releases, verified for 0.160.0 and 0.161.0: `go get` of
the release's component modules downgrades only those modules, so the root
module's own requirements (`pkg/ottl` and the core libraries at 0.162) stayed,
and 0.160's filterprocessor then imported `pkg/ottl/contexts/ottlprofile` that
0.162's `pkg/ottl` no longer has; `go mod tidy` went looking at 0.163.
`scripts/dependency-set.sh` (`.ps1` on Windows) therefore drops the root's
`go.opentelemetry.io/...` and `github.com/open-telemetry/...` requirements first
and pins `tools.go`'s OCB to the release. The resulting sets carry pdata 1.66.0,
1.67.0 and 1.68.0 for 0.160, 0.161 and 0.162.

### 0.163 (2026-10-27)

Verified against upstream's v0.163.0 releases manifest and the pkg/ottl,
transform processor and internal/filter sources at v0.163.0. The releases
patches 0.163.1 and 0.163.2 change only CI permissions, the Go toolchain
and the CA bundle of upstream's binaries; their component lists are
identical, so the table uses 0.163.0.

- Core 0.163.0 (stable modules v1.69.0), contrib 0.163.0; `pkg/ottl` is
  still v0.163.0 and `k8sattributesprocessor` v1.2.0.
- The OTTL constructors and function sets match 0.162, so `since0162`
  serves 0.163.
- The OBI receiver already uses the `go.opentelemetry.io/obi/collector`
  module, as in 0.164.
- The catalog adds `adaptive_tail_sampling`, `rolling_span_latency` and
  `googlecloudpubsubpush` and renames `sqlserver` to `sql_server`.

`TestEveryReleaseRunsTheSamples` with `LINEA_RELEASES=0.163.0`
passes.

### 0.164 (2026-10-27)

Verified against upstream's v0.164.0 releases manifest and the pkg/ottl,
transform processor and internal/filter sources at v0.164.1:

- The release pairs core 0.164.0 (stable modules v1.70.0) with contrib 0.164.1.
  Contrib 0.164.0 is unusable: its modules require `pkg/ottl` and
  `processor/k8sattributesprocessor` v1.2.0, which were never published (contrib
  #52356). `pkg/ottl` is a v1 module from this release line (v1.3.1 here; 0.163
  still had v0.163.0), with the same import path.
- The OTTL context constructors and the transform/filter function sets
  (`WithExperimentalConverters(WithProfileConverters(...))`) are unchanged from
  0.162, so `since0162` serves 0.164 and no new variant is needed.
- The OBI receiver's module is `go.opentelemetry.io/obi/collector` (v0.15.2,
  since 0.163), still built from a local `go.opentelemetry.io/obi` replace; the
  `ebpf-profiler` replace is gone. `unsupported.yaml` keys the exclusion and
  type by the new module path.
- The catalog adds `adaptive_tail_sampling`, `partitioning`,
  `rolling_span_latency` and `googlecloudpubsubpush` and renames `sqlserver` to
  `sql_server` and `googleclientauth` to `google_client_auth` (old types remain
  deprecated aliases).
- The dependency set keeps every core and contrib module on the release, except
  the stale indirect `pkg/pdatautil` v0.160.0 and `semconv` pseudo-version, as
  0.162's does. `asapauthextension` stays unsupported.
- Gates removed in 0.164 (`ottl.set.allowNil`,
  `processor.filter.defaultErrorModeIgnore`, Prometheus
  `PermissiveLabelSanitization`) come from the release's own registry, so a
  0.164 runtime's gate list reflects them without debugger changes.

`TestEveryReleaseRunsTheSamples` with `LINEA_RELEASES=0.164.0`
built the built-in components at 0.164.0 and ran every sample and the
componentsDir reference comparisons.

### 0.165 (2026-11-08)

Verified against upstream's v0.165.0 releases manifest and the transform
processor and queuebatch processor sources at v0.165.0:

- Core 0.165.0 (stable modules v1.71.0) pairs with contrib 0.165.0;
  `pkg/ottl` is v1.4.0. The OTTL constructors and function sets match 0.162,
  so `since0162` serves 0.165.
- The transform processor drops `flatten_data` (`Config.FlattenData`) for
  `flatten.log` (`Config.Flatten.Log`, from 0.164). The replay reads both
  fields by name (`ottlreplay/flatten.go`, `ottl-replay.md`), so one replay
  compiles against every release.
- The catalog adds `queue_batch` (`batching.md`) and drops
  `asapauthextension`, which upstream's manifest comments out (contrib
  #51868); `unsupported.yaml` no longer lists it.
- No release module raises the umbrella `go.opentelemetry.io/collector`
  module any more, so `collector/livecapture`'s requirement left it at the
  root's release, which still holds `internal/statusutil` (its own module
  since 0.163) and made the import ambiguous. The dependency-set scripts pin
  the umbrella to the release.

`TestEveryReleaseRunsTheSamples` with `LINEA_RELEASES=0.165.0` built the
built-in components at 0.165.0 and ran every sample and the componentsDir
reference comparisons. Its lean runtime (without the transform processor)
also lacks the filter processor, so it reports `filter_explanation` missing
too; the test's expected list predated that feature.
`TestQueueBatchIsExplained` built a 0.165 runtime with `queue_batch`.

## Upgrade checks

Verified against the catalogs of 0.160.0 and 0.162.0 (2026-10-15).

- Between 0.160 and 0.162 the contrib catalog removes the mezmo exporter and
  the wavefront receiver, renames the `grafanacloud` connector to
  `grafana_cloud` and the `awss3` receiver to `aws_s3` (the module path of
  the latter stays `awss3receiver`, so a retargeted manifest keeps building
  it; both keep their old name as a deprecated alias, see below), and adds
  the `sd_notify` extension. `runtime retarget` names removed modules as
  missing.
- The catalogs list component modules only. The manifest's telemetry entry
  is `go.opentelemetry.io/collector/service`, which no catalog lists, so
  retargeting gives it `v<release>`, the version it is released with.
- Feature gates come from `featuregate.GlobalRegistry()` in the worker
  (`Hello.Gates`, absent from older workers, so the check says the gates
  were not compared). On a team server the runtime's connected worker (or its
  stored hello, for pool workers) supplies them; before `poolWorker` kept them,
  every check there listed both sides as unknown. A gate's ID does not say who reads it, and the
  global registry holds every gate the binary links: each runtime links
  the filelog receiver through `internal/debugger/mock`, so `filelog.*` and
  `stanza.*` gates appear on runtimes whose configuration reads no files.
  The build therefore generates `internal/debugger/registry/gateowners.go`
  (`internal/debugger/registry/gen -gates`, run in the build container before
  `go build`): it lists the worker's packages for linux, finds the
  `featuregate` `Register`/`MustRegister` calls and resolves their IDs
  (literals, constants, cross-package constants, concatenations), and
  records whether the Collector's own packages (the registry's imports
  outside the components and this module) link the registering package,
  or which component packages do. Each `Gate` then carries a `scope`:
  `collector` (always listed), `component` with the `<kind>/<type>` names
  linking it, deprecated aliases included (listed only when the
  configuration uses one), or `debugger` (linked only by the debugger's
  own packages, never listed). A gate without a scope, from a worker
  built before scopes, falls back to the ID heuristic: the type segment
  matched to a configured type after lowercasing and dropping
  underscores, by prefix. A gate change never alters the verdict: whether
  it matters depends on the data, which the scenarios check.

## Deprecated component types

Verified against the catalogs of 0.160.0, 0.161.0 and 0.162.0 (2026-10-15).

- Upstream renames component types to snake case (`otlp` to `otlp_grpc`,
  `otlphttp` to `otlp_http`, `filelog` to `file_log`, `k8sattributes` to
  `k8s_attributes`, about 60 per release) and keeps the old name as a
  deprecated alias: `metadata.yaml` declares `deprecated_type`, and the
  factory registers under both. A configuration naming the old type still
  resolves (the built-in 0.162 runtime runs `exporter/otlp/backend` with
  the `otlp_grpc` factory).
- The catalog records the alias (`deprecated_type`); a catalog without it
  put every old name under "Not in the catalog", so a manifest composed from
  a configuration left the component out and the built runtime ran it as a
  stand-in. Composition selects the component by either name and warns
  that the manifest includes the current one.
- `scripts/dependency-set.sh` writes its script to a file in the container.
  `scripts/dependency-set.ps1` (Windows) feeds the script on standard input:
  Windows PowerShell prepends a UTF-8 byte order mark and CRLF line ends,
  which the container strips before `sh` reads it.
