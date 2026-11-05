# Architecture

Package map of the debugger: what each package owns, the contracts between them.
`AGENTS.md` holds the rules; this file holds the detail. Update it in the same
change as the code it describes.

The root Go module is declared in `go.mod`.

## Go packages

- `cmd/linea/` — CLI subcommands (`inspect`, `run`, `test`, `diff`,
  `upgrade-check`, `config-bisect`, `runs`, `suite`, `artifacts`, `export`,
  `ottl`, `statement`, `samples`, `runtime`, `capture`, `fleet`, `audit`,
  `token`, `access`, `project`, `library`, `login`, `logout`, `build`, `version`), one file per command (several
  have more); calls platform operations with caller `cli` and formats their
  results, or with `--server` sends them to a team server through
  `internal/debugger/client` (`connected.go`, `runtime.go`; `runtime default` is
  `runtime_default.go`). Every connected command takes `--project` (default
  `$LINEA_PROJECT`, `connected.go`), which the client sends as the `project`
  query parameter (`Client.InProject`). It has no server mode: the team server is
  `cmd/linea-server`. The direct commands (no `--server`) compose the platform in process, unrestricted
  for host paths and `--import-env`. That platform is composed with
  `Runtimes: platform.DockerRuntimes(...)` (lazy; no Docker call until a runtime
  is used), whose `ImageCache` is `--runtime-cache` (default
  `platform.DefaultImageCache()`); `runtime prune --binaries` empties it with
  `platform.PruneImageCache`; its tests point `APPDATA` and `XDG_CONFIG_HOME` at
  a temporary directory in `TestMain`; `cmd/linea` never imports
  `internal/debugger/container` (its Docker tests do, for image names).
- `cmd/linea-server/` — the team server binary: a thin `main` that calls
  `server.Main`. It takes `--config <file>` (required) and the flags
  `--addr`, `--tls-cert-file`/`--tls-key-file` or `--allow-http`,
  `--drain-timeout`, `--metrics-listen`, `--mcp`, `--fleet-url`,
  `--fleet-token-file`, `--fleet-allow-http` and `--verbose` (debug logs), and
  no subcommand. It links no Collector (no `engine/local/install`): every
  request runs on connected workers. It is always a team server on PostgreSQL
  and object storage, with no Docker; `linea serve` no longer exists.
- `cmd/linea-worker/` — the worker process for the built-in registry, in two
  modes: the debugger starts it and talks over stdin/stdout
  (`internal/debugger/worker.Serve`), or
  `--connect URL --name N [--capacity C] [--ca-file F] [--token-file F]` dials a
  team server with the registration token (also from `LINEA_WORKER_TOKEN`; see
  `internal/debugger/worker/`), or with `--pool` as a pool worker that serves
  any server-built runtime binary for its platform, one child process per
  runtime (`--capacity` default 8, `--warm` 4, `--idle` 10m, `--cache` 5GiB,
  `--cache-dir`; `--name` defaults to `pool`). It listens on no port: there is
  no `--listen`, and no TLS flags.
- `cmd/linea-builder/` — the runtime builder:
  `--connect URL --token-file F [--name N] [--capacity C] [--ca-file F]`
  `[--work-dir D] [--cpus N]` dials `/v1/builders/connect`
  (`worker.ServeBuilds`) and, per build job, downloads the manifest and uploaded
  sources from presigned URLs (size and SHA-256 checked, sources read with
  `container.ModuleArchive`), picks the verified release's dependency set from
  the support table, builds with `container.Native` and the embedded worker
  source, and PUTs the binary to the job's presigned URL (refused above
  `BinaryLimit`) with its hello. It refuses to start unless `go` on PATH is
  `container.GoVersion` and the worker source is embedded, holds no database or
  bucket credentials, and redacts URL credentials and presigned signatures from
  every log line and error it sends. When the job names an export (the team
  file's `builds.export.repository`), it also pushes the binary as an image with
  `container.ImageExport` after the upload, with the Docker config of
  `--registry-auth` (read at each export: `credHelpers`, `credsStore` running
  `docker-credential-<name>`, or `auths`) and the `--registry-plain-http` hosts;
  a failed push is the result's `ExportError` and the build stays done.
  `deploy/docker/linea-builder.Dockerfile` packages it on Alpine with git and
  the toolchain of the pinned Go image; the Compose stack runs it (`docs/debugger/deploy-compose.md`).
- `collector/provider/` — the provider SDK (`docs/debugger/provider-sdk.md`): a
  nested module `github.com/lineahq/linea/collector/provider` (`Version` 0.1.0)
  importing only the standard library. A component module's package exporting
  `func DebuggerProvider() provider.Provider` drives its components during a run
  (`Run`: settings from `mocks.providers.<name>`, loopback listeners for
  declared endpoints, `Record`, timeline hooks, evidence). The root module
  requires it with `replace => ./collector/provider`; within a minor version
  only additions.
- `internal/debugger/engine/` — transport-neutral `Request` -> `Report`,
  Collector execution and analysis, the only report serializer and
  execution-bundle encoding. `Prepare` captures source/dependency bytes without
  starting a Collector; execution decodes fresh inputs/mocks from that private
  capture. Product adapters schedule runs through the platform; direct engine
  calls serve low-level tests. `ReadBundleParts` reads private captured sources
  for scenario derivation without execution-distribution checks or host reads.
  `Request.Limits` (never client JSON) is recorded in bundle schema 4 and, as
  effective values with clamps, in the report's `limits` (report schema 0.6; 0.7
  added `matching.object`; 0.8 added `references`, where each `${scheme:...}`
  value came from, stand-ins included; 0.9 added `runtime`, the runtime a run
  executed on: `built_in`, or `container` with its name, image ID, Collector
  version and manifest digest, stamped by the platform; 0.10 added the runtime's
  `missing_features`; 0.11 added kind `remote` (a connected worker; its
  `endpoint` is only set in reports stored before workers dialed the server);
  0.12 added `component_metrics` and `component.log` events, and
  `endpoint_outputs`, `simulated_endpoints`, `endpoint.received` events and
  lineage `sent`; 0.13 added `runtime.providers`,
  `simulated_endpoints[].provider` and topology components' `provider`
  (`runtime.DrivingProviders`); 0.14 added kind `image` (a runtime image a
  direct command read from a registry and ran natively: its `name` is the
  reference by digest and its `image_id` that digest); `ReadableReportSchema`
  reads 0.8 to 0.14). Bundle schema 5 records source labels and
  `Request.Sources` (saved revision IDs, never resolved by the engine;
  `BundleInfo.Sources`). Bundle schema 6 records the target runtime
  (`Request.Runtime`); built-in bundles stay schema 5, `DecodeBundle` refuses a
  bundle naming a runtime and a worker decodes with `DecodeBundleForRuntime`; it
  resolves scenario file names with the path rules of the host the bundle
  records (`hostpaths.go`: a Windows host's bundle in a Linux worker).
  Inspection and OTTL previews are a host capture (`CaptureInspection`,
  `CaptureOTTL`, no factories) completed by a registry (`CompleteInspection`,
  `CompleteOTTL`). Both results carry `runtime`, the runtime that answered,
  which the platform sets (`answeredBy`; inspection schema 0.2); `Decode` is the
  strict JSON decoder for reports crossing a process boundary. The engine
  links no Collector: executing, factory-based preparation, completing an
  inspection or preview and the built-in runtime's description go through the
  process's `Collector` (`collector.go`: factories, changed gates, providers,
  execution modes, wall-clock reads, reference prerequisites, `Run`,
  `RunReference`), which `Use` sets once; without one that work fails with
  `ErrNoCollector` (platform code `unavailable`), while preparing, capturing and encoding for a runtime
  still work, so a bundle records no gates. See
  `docs/internals/execution-bundles.md` and
  `docs/internals/runtime-prerequisites.md`.
- `internal/debugger/engine/local/` — this build's in-process Collector
  (`local.Collector`): the generated registry, the runtime, the debugger's
  mock providers and the manifest's feature set. It does not import the
  engine; `engine/local/install` passes it to `engine.Use` in `init`, and
  `cmd/linea` and `cmd/linea-worker` import that for its side effect (the
  adapter boundary allows `cmd/linea` only that). Tests of the packages that
  execute call `engine.Use` in `collector_test.go`.
- `internal/debugger/server/` — what `linea-server` runs: `Main` (flags, exit
  codes, drain on SIGTERM), `config.go` (the server config: parse, validate,
  compose the store, sign-in, role bindings and limits), `serve.go` (HTTP, TLS,
  the optional metrics listener, MCP, the web client), `limits.go` (HTTP
  bounds) and `capture.go` (the live-capture fleet flags).
- `internal/debugger/platform/` — the application service every interface uses:
  a scheduler (`scheduler.go`) that claims runs from the repository's queue
  (`artifact.Queue`; an ephemeral service uses `memory.Queue`) under a renewed
  lease (`Options.Lease`, clock `Options.Now`) and executes through executors
  (the service itself, one run at a time, and each connected worker, below),
  bounded admission, run handles, cancellation through the queue, events,
  ephemeral or repository-backed history (`platform.Open`), source artifacts,
  captured input bundles, the environment capture policy
  (`Options.Environment`), the host path policy (`Options.Paths`, zero means
  inline content only; see `docs/internals/path-policy.md`) and the run budgets
  (`Options.Budgets`: deadline, evidence bytes, events and query pages; requests
  only lower them, and a run past its evidence budget is `partial` with
  `result.truncation`), execution on the built-in engine in process or on a
  connected worker (below), and an optional runtime store (`Options.Runtimes`,
  `runtimes.go`: `DockerRuntimes` in production, Docker's image store being the
  runtime list). A runtime selection that is an image reference
  (`container.IsImageReference`; `runtimes_image.go`) is refused with
  `invalid_request` on a team server; in a direct command a store with
  `PullImage` reads the image into `DockerOptions.ImageCache` and runs its
  binary as a stdin/stdout child with a minimal environment (`binaryEnv`),
  refusing a hello whose Collector release is not the image's label; an image
  for another platform runs in Docker when Docker has it. Reports name it as
  kind `image`, the reference by digest and the digest as `image_id`. Connected
  workers (`worker_pool.go`): `Service.ConnectWorker` is the handler of
  `/v1/workers/connect` (`Options.WorkerToken`; without it the route answers
  404). The runtime catalog (`runtime_catalog.go`) maps a name to a runtime
  identity: `RegisterRuntime` records the hello that `ReadRuntimeImage` read
  from an image with the local Docker store, a connected worker's hello confirms
  the entries of its identity, and a request's `runtime` resolves through the
  catalog before stored runtimes, binding to the workers of that identity
  whether or not one is connected. The catalog is the repository's
  `artifact.Runtimes`, or `memory.Runtimes` without one. A team server has no
  runtime store, so a name outside the catalog is `unavailable`. With
  `Options.BuiltInOnWorkers` (a team server) `built-in` is a runtime too
  (`Service.selects`): a connected worker whose hello sets `BuiltIn` and
  reports the server's `ProductVersion` registers it under
  `built-in@<product version>`
  (`builtin_runtime.go`), a name no request or registration can take, listed and
  bound as `built-in`; it removes the entries of other releases no connected
  worker serves. The default runtime, the runtime of every request that names
  none, is the team's choice (`default_runtime.go`): `GetDefaultRuntime` and
  `SetDefaultRuntime` (`runtime.read`, `runtime.manage`; type
  `DefaultRuntimeInfo{Name, SetBy, Set}`), `built-in` until one is set. The name
  must be `built-in` or a catalog entry (`not_found` otherwise), and each choice
  is audited with the previous and the new default, and removing the default's
  entry is `conflict`. The Service caches the choice, loads it at `Open`
  (recover step) and reloads it on a `ChangeDefaultRuntime` change or when the
  feed cursor expires. Role bindings (`role_bindings.go`) follow the same
  pattern: the effective policy is the server config's `authz.Policy` extended
  (`Policy.Extend`) by the managed bindings, held in an `atomic.Pointer` that
  every permission check loads, loaded at `Open` and on a `ChangeRoleBindings`
  change. `ListRoleBindings`, `AddRoleBinding` and `RemoveRoleBinding` need
  `access.manage`; server config bindings are listed as `config-N` (source
  `config`) and are `forbidden` to remove, a write names the revision it
  read (`conflict` when stale), granting what the server config already grants is
  `conflict`, and a removal that would take the caller's own `access.manage`
  is `conflict`. A caller holding `access.manage` only in some projects
  (`managedProjects`) sees, grants and removes the bindings of those projects
  only, and grants only roles whose every permission it holds there. Projects
  (`projects.go`) follow the same pattern too: the repository's project set
  (projects, library audiences, runtime availability, one revision) is held
  in an `atomic.Pointer`, loaded at `Open` and on a `ChangeProjects` change.
  `Caller.Project` is the project a request works in, `default` when empty;
  `workIn` resolves it for a write, and `authorizeIn` refuses a caller
  without the permission there, any write to `library` without
  `library.publish`, and new work in an archived project (`conflict`;
  deleting stays allowed). An unknown project is `not_found` only to callers
  who hold the permission in scope `*`, `forbidden` to others.
  `ListProjects`, `CreateProject`, `UpdateProject`, `ArchiveProject` and
  `UnarchiveProject` need `project.manage` to change (listing shows a caller
  the projects it holds a permission in, with those permissions); writes name
  the set's revision; a caller without any permission is refused the list. A project's settings feed retention
  (`retentionPolicy`: its run count, age and storage limit replace the
  installation's for its runs) and runtime choice (its default runtime before
  the server's). Sharing (`sharing.go`): `ListLibrary` pages the `library`
  project's configurations, fixtures and scenarios, narrowed to the items
  shared with the caller's project (a publisher working in `library` sees
  all, and non-publishers see only the audience entries they can read);
  `SetLibraryAudience` (`library.publish`) and `SetRuntimeAvailability`
  (`runtime.manage`) set the audience and the availability, each an ID list
  or `*` alone of existing projects; `CopyArtifact` saves a revision the
  caller can read into the caller's project as a new artifact with the
  revision's origin (into `library`, it publishes). `usableIn` lets work use
  its own project's artifacts and the library items shared with its project;
  `projectRuntime` resolves the runtime work in a project uses (the named
  one, else the project's default, else the server's) and, with
  `runtimeAvailable`, refuses one the project may not use (`forbidden`) for
  runs, suites, `InspectConfiguration`, `PreviewTransformation` and
  `CheckUpgrade`, which all resolve the caller's project first. Without a repository they are `unavailable`. Each worker that says hello (its `--name` only labels it)
  is an executor with its own worker identity in the queue: the scheduler claims
  queued runs of its runtime up to its free capacity, the service renews the
  leases of its runs on the heartbeat while it stays connected, and a disconnect
  leaves them to expire and be requeued. A requeued run records only its current
  attempt's events, which the engine numbers from 1; `WatchRun` takes an
  `EventCursor` (attempt and seq) and reports `Restarted` when the cursor was in
  an earlier attempt. Inspections and OTTL previews on a pooled runtime take a
  free slot ahead of queued runs (`unavailable` when no worker of the runtime is
  connected to any instance, see the shared instances below). Bundles and
  results over `worker.InlineLimit` move through presigned URLs of
  `artifact.Transfers` when the repository implements it. A pooled runtime
  stamps reports with kind `remote` and its name (no endpoint), except the
  built-in runtime, which a worker of the server's release serves: kind
  `built_in`, named `built-in`. Its runs, inspections and previews capture with
  the service's imported names only, never the whole host environment
  (`remotePolicy`, `prepareRemote`). There is no server-dialed remote runtime:
  the server never dials a worker, keeps no connection file and reads no
  kubeconfig. A pooled name shadows a built runtime of the same name; without a
  runtime store a name no worker has is `unavailable`. Shared instances
  (`instances.go`, `interactive_jobs.go`): on a repository-backed service the
  repository is the truth and each instance's in-memory view is a cache. It
  catches up from the change feed (`Repository.Changes`, table `changes`) before
  every operation and follows it in the background, so suites, runs and catalog
  changes show on every instance. The run cache (`run_cache.go`) holds
  unfinished runs, suite members and the `Options.RunCache` (default 1,000)
  terminal runs read most recently; `GetRun`, `WatchRun` and every run reader
  load a cold run from the repository (`load`), listing reads pages without
  caching, and the feed updates cached runs and adopts unfinished ones but skips
  other terminal runs. Opening and a feed reload read unfinished runs only
  (`RunQuery.Unfinished`), plus the runs stored before run indexes to index them
  once (`RunQuery.Unindexed`), so startup and memory follow the work in flight,
  not history. A run the platform cannot decode fails the reads that name it
  (`storage_error`), not opening. Engine events of a run reach other instances
  through `run_events` (an outbox: `AppendRunEvents`, `RunEvents`). An
  interactive request that needs a worker slot is a row in `jobs`: an instance
  with a connected worker of the runtime claims it and the posting instance
  waits for the answer (a local fast path when it has the worker itself; a team
  server gives up after 30 s). OIDC sign-ins in progress are rows in `logins`
  (`SaveLogin`, `TakeLogin`, taken once, at most 1000), so the callback may land
  on any instance. One instance, the upkeep's leader, runs the optional
  `artifact.Upkeep` (`Lead`, `Housekeep`) every 5 minutes: the store sweep,
  trimming the change feed to the newest 100,000 changes, trimming the
  tombstones the feed no longer reports, finished jobs and expired worker
  records. The leader holds a session advisory lock on a dedicated connection
  until it closes or loses it; the next instance to ask then leads.
  `Service.Drain` (called by `linea-server --config` on SIGTERM with
  `--drain-timeout`) stops claiming runs and jobs, waits for the runs of its
  workers, then hands the rest back with `Queue.Release`, which does not spend
  an attempt (`runs.released`; the requeue limit counts `attempt - released`). A
  killed instance's runs expire with their leases and another instance's
  heartbeat requeues them. Each instance records its connected workers in the
  repository (`artifact.Presence`, `presence.go`) under its own ID, valid for
  one lease, on every connection, disconnection and heartbeat; Close removes the
  record. A catalog entry's worker and slot counts sum the unexpired records of
  every instance, so a killed instance's workers drop out within a lease. A
  pool's record also lists the runtimes it holds running (`PresentWorker.Warm`,
  migration `010_pool_presence.sql`), written again whenever its `pool_state`
  changes (`followWarm`); `warmElsewhere` reads those of other instances
  (`Presence.WarmRuntimes`) at most once a second. The team-mode MCP handler is
  stateless (every POST stands alone; GET and DELETE answer 405). A request's
  `Runtime` names a built runtime (empty or `built-in` is the registry, in
  process except on a team server); the platform binds it at admission (missing
  is `unavailable`, another worker protocol `unsupported_build`), keeps one
  worker per image and stamps the report. Builds (`runtime_builds.go`) check the
  manifest first (a path, text or saved artifact of kind `manifest`; releases
  before 0.160 are `unsupported_collector_version`, a release outside the
  support table `Options.Versions`, default `versions.Embedded()`, builds
  unverified), then queue in their own slot (4 queued, 20 kept, last 2000 log
  lines, in memory only) so a build never delays a run; a runtime in use is not
  deleted (`conflict`). A team server, which has no runtime store, queues builds
  in the repository instead (`server_builds.go`): `runtime.build` admits,
  sources are uploaded tar archives checked at admission (regular files and
  directories, relative paths, `go.mod` of the named module at the root), the
  build key (manifest and source digests, Go image, builder version) reuses a
  done build of identical inputs, at most 2 unfinished builds per principal and
  20 in total, callers read and cancel their own builds and `runtime.manage`
  all, `ListRuntimeBuilds` pages, and upkeep expires lost builders' leases (2
  attempts) and keeps the last 100 finished builds. `ChangeBuild` on the change
  feed wakes watchers on every instance. Builders (`builders.go`) dial
  `ConnectBuilder` with `Options.BuilderToken` (404 without it, or without
  object storage that implements `artifact.BuildTransfers`); the instance
  holding the connection claims builds up to the builder's capacity, renews
  their leases, flushes their logs, cancels a build on request or lost lease,
  checks the result (binary digest and size, protocol, product and Collector
  versions, verification, platform), stores the binary with `StoreBuildBinary`,
  and registers the runtime under the build's name with its `binary` and `build`
  (`registerBuilt`, before the build is published done; reused builds too). A
  builder that sends `drain` (`linea-builder` on its first SIGTERM) gets no new
  claim; once its builds ended the instance answers `drain` and closes the link,
  and the builder exits rather than dial again. A second SIGTERM cancels its
  builds, which return to the queue when their leases end. `BuildMetrics`
  (`build_metrics.go`) counts the queued and running builds
  (`Builds.CountBuilds`, reused for 10 s) and the builders connected to this
  instance with their slots; it takes no caller and names nothing, and
  `api.Metrics` serves it as Prometheus text on `linea-server --metrics-listen`, never
  on the API listener. Pool workers (`worker_pool.go`): a worker whose hello
  carries `Pool` is an executor for every catalog entry with a `binary` of its
  platform (refused without `artifact.BuildTransfers`). A run on such an entry
  is pinned at admission to its binary (`artifact.RunBinary`,
  `Admission.Binary`; migration `009_run_binaries.sql` keeps the binary
  referenced until the run finishes), and a pool claims it with
  `Queue.ClaimBinary`, which takes only runs pinned to its platform. Each
  request to a pool names the binary with a presigned URL (`RuntimeBinaryURL`,
  15 minutes). The scheduler offers a runtime's runs to dedicated workers first,
  then to pools with it warm (`pool_state`), then pools with a free warm place,
  then the rest (`poolRank`). With shared instances, a pool that would have to
  start a runtime skips it for `poolGrace` (2 s after its oldest run queued)
  while a pool of another instance holds it warm, then claims it anyway; a timer
  wakes dispatch when the grace ends. A pool answer marked
  `worker.ErrSuspectBinary` records the binary on its catalog entries
  (`Runtimes.MarkSuspect`, `RuntimeEntry.Suspect`, migration
  `011_suspect_binaries.sql`; cause without URLs, at most 512 bytes); `bind`
  then refuses the runtime and `poolBinary` fails runs pinned to it before they
  reach a pool, both `unavailable`, naming the cause. `SaveRuntime` clears the
  mark (`suspect.go`). Inspections and OTTL previews take a pool slot the same
  way. Presence records a pool under `pool:<os>/<arch>`, and a catalog entry
  with a binary counts the pools of its platform among its workers and slots. A
  pool whose hello sets `Pool.Own` also runs its own build: it is an executor of
  its hello's identity for runs and requests with no binary, claims that key
  with `Queue.Claim` instead of `ClaimBinary`, registers the built-in runtime
  like a worker of this release, and is counted under that identity too
  (`PresentWorker.Own`, migration `013_pool_own_runtime.sql`;
  `WorkerCount.OwnWorkers`/`OwnSlots` let `entryCount` count it once for an
  entry with a binary). Runtime storage (`runtime_storage.go`):
  `GetRuntimeStorage` names each image `named`, `replaced`, `tagged` or `stale`
  (the store's `Get` by name decides) with its size, the build caches and
  left-over containers (`RuntimeHousekeeping`, an optional store interface);
  `PruneRuntimes` removes only what it is asked (replaced, stale, caches;
  stopped containers always), keeps images in use and what Docker refuses with
  its reason, and refuses caches while a build is active. `ListRuntimes` hides
  replaced images. Custom module sources (`runtime_sources.go`) are directories
  read under the path policy at admission (the manifest's `path:` and local
  replaces relative to it or `ManifestDir`) or git repositories cloned with host
  `git` when the build starts, their ref resolved to a commit the runtime and
  its reports record (`manifest_sources` diagnostics). `collector_versions.go`
  serves the support table, catalogs and `ComposeManifest`
  (`internal/debugger/catalog`); `ComposeManifestRequest.Sources` (directories
  under the path policy or git repositories cloned at once) add an entry for
  each component package `manifest.Discover` finds in them (`customComponents`,
  `ComposedManifest.Custom`, and the debugger providers found,
  `ComposedManifest.Providers`). `RuntimeInfo.Providers` comes from the image
  label (name and package) or a connected worker's hello. `upgrades.go` holds
  `RetargetManifest` (a manifest moved to a verified release, never saved or
  built) and `CheckUpgrade`: the configuration inspected on two different
  runtimes, their feature gates (a worker's `Hello.Gates`; for a runtime on
  connected workers, the hello it was bound from, kept by `poolWorker`), and, only when both
  accept it, each scenario run on both through `SubmitComparison` and compared
  with `CompareRuns`, one pair at a time; a caller that stops waiting cancels
  the pair. The verdict is `internal/debugger/upgrade`'s. `bisect.go` holds
  `BisectFleetConfig`: it reads the config layers fleet sends a managed agent,
  pages the searched config's stored versions between the good and bad bounds
  (at most 100), and for each probed version rebuilds the agent's configuration
  as the OpAMP supervisor merges it (`internal/debugger/bisect.Compose`; the
  other layers at the versions fleet sends now, optional local supervisor files
  underneath), inspects it and runs every scenario on it. An invalid
  configuration or an inconclusive run skips the version. Once a last good and
  first bad version are known, the culprit diffs their bodies and compares the
  changed scenarios' runs; versions are matched to fleet's rollouts. Bodies are
  admin-only in fleet and never stored by the debugger. A `Source` is a path,
  inline text or a saved revision (`ArtifactRef`, both IDs required), plus an
  optional `Label` for inline text; every Source-taking operation copies a
  revision's bytes before preparation (`sources.go`), so later heads never
  change a run, and a candidate records its referenced base in origin
  `BaseRevision`. `GetRunInputs` returns a run's admitted sources from its
  bundle (dependency files and environment by name only). `GetRunLog`
  (`run_log.go`) returns the Collector log tail a failed or partial run keeps in
  its evidence archive: `engine.Request.Log` (`engine/runlog.go`) tees
  info-level and higher Collector logs into a 64 KB `LogTail` of whole lines,
  with every environment value the snapshot captured redacted. A worker keeps
  its own tail per execute and returns it in the result frame.
  `CreateScenarioFromRun` derives verified scenario YAML from captured admission
  evidence and optionally saves a new scenario artifact with a run origin;
  original runs and existing artifact heads stay unchanged. `TestStatement`
  composes an unsaved candidate with ordinary quick/scenario submission, returns
  without waiting and declares whether assertions provide an output boundary; it
  never saves or supplies its own verification path. `GetRunSummary` and
  `QueryEvidence` answer from a terminal run's report through
  `internal/debugger/evidence` (bounded pages, opaque cursors bound to one run,
  family and parameters); they never execute or replay. `DeleteRun` and
  `DeleteSuite` (`deletion.go`) remove finished runs and suites with their
  evidence, leaving tombstones: reads within a day are `not_found` "run was
  deleted" (`deletionMemory`), later ones `not_found` as an unknown run; queued
  and running runs, and a suite member deleted alone, are `conflict` (the
  message names the suite). Every adapter maps them. Durable suites reserve
  queue capacity before preparing members and derive outcomes from their runs on
  read. Ephemeral services keep the newest 20 suites and their member runs
  beyond the history limit. The composing application owns and closes it;
  adapters never close an injected service. Platform operations
  (`operations.go`: context and `Caller` first, typed `*Error` codes, explicit
  quick/scenario run requests) are the single definition of each capability;
  adapters (HTTP, CLI, MCP) only map them, and
  `TestAdaptersMapPlatformOperationsOnly` fails on any other internal import
  (`internal/debugger/analysis` and `internal/debugger/assertion` types and
  constants excepted). Never refuse an unmodified Collector config to improve a
  reproducibility label. Keep quick and scenario modes first-class.
- `internal/debugger/linkboundary/` — `TestCollectorFreePackagesLinkNoImplementation`
  lists the dependencies of every package the team server may link and fails on
  a Collector component implementation (`x*` API packages excepted), a
  `k8s.io` package, the generated registry or the in-process executor. The
  Collector data model and component APIs are allowed. Add a package to
  `collectorFree` once it is free.
- `internal/debugger/artifact/` — the `Repository` interface the platform
  depends on, its shared contract suite (`contract/`), records, limits and
  errors. `internal/debugger/store/pg` implements it on PostgreSQL (rows) and
  S3-compatible object storage (content-addressed payloads), with embedded
  migrations applied under an advisory lock: immutable revision chains,
  conflict-checked heads, platform-authored revision origins, each artifact's
  project, the run journal (runs, recorded assertion evaluations, immutable
  suites, each run and suite in a project, and the tombstones of deleted runs
  and suites, whose unreferenced objects `Sweep` removes), input bundles and
  evidence archives (the platform writes them gzip-compressed, archive schema 2;
  schema 1 stays readable), capture segments, the audit log, tokens and
  sessions. For shared instances (migration `004_shared.sql`) the contract also
  holds the change feed (`Changes`, `changes` and `change_floor`), the chosen
  default runtime (`Runtimes.DefaultRuntime` and `SetDefaultRuntime`, which
  returns the previous default; type
  `artifact.DefaultRuntime{Name, SetBy, Set}`; migration
  `012_default_runtime.sql`, table `default_runtime`, one row; change kind
  `default_runtime`, change ID `default`; `DeleteRuntime` returns `ErrConflict`
  for the default's entry, and the pg store takes the same `runtimes` advisory
  lock in both), live run events (`run_events`), interactive `Jobs`, in-progress
  OIDC `Logins` (`MaxLogins` 1000) and `Queue.Release` (`runs.released`), plus
  the optional `Upkeep` interface (`Housekeep`) the leader calls. Runs are
  listed in pages from the store (migration `005_run_listing.sql`): `ListRuns`
  filters on each run's index (scenario, runtime name, result), which every
  publication writes, and pages newest first on the admission ordinal with an
  opaque cursor bound to its query. The platform's `ListRuns` takes a `RunQuery`
  and returns a `RunPage`. It reads the page from the repository and decodes a
  run that this instance does not cache from storage. At startup it indexes runs
  stored before migration 005 (`RunQuery.Unindexed`, `IndexRun`). Retention
  (migration `006_retention.sql`, `artifact.Retention`) removes the oldest
  terminal runs that are not kept (`KeepRun`, `RunRecord.Kept`), in no suite and
  the origin of no artifact's head revision (`revisions.origin_run`), in batches
  of `EvictionBatch` through the same deletion as `DeleteRun`. The platform runs
  a pass every 5 minutes and after admissions (`Options.Retention`); with shared
  upkeep only its leader does, nudged by the admissions it reads on the change
  feed. It forgets the evicted runs and audits each pass as the `system`
  principal with a count per reason (`count`, `age`, `storage`). Manual
  retention adds `PruneRuns`, which removes the evictable runs a filter selects
  and reports the bytes only they referenced, with a dry run that measures the
  same way, and `RunStats`. The platform operations `KeepRun`, `UnkeepRun`,
  `PruneRuns` and the extended `GetStorageUsage` sit on them; a run's owner or
  an admin may keep, unkeep or delete it, and only an admin prunes. The shared
  contract suite covers all of it on both stores. Runtime builds (migration
  `007_builds.sql`, `artifact.Builds`) are durable jobs: `AdmitBuild` stores the
  manifest and source archives as objects and queues the build, or records it
  done at once from a done build of the same key; builders claim, renew, log to
  and finish builds under leases with attempts, as workers do runs; a build
  keeps its last 2,000 log lines; `ExpireBuilds` and `TrimBuilds` serve upkeep,
  and every write records `ChangeBuild`. The build binaries
  (`008_runtime_binaries.sql`, `artifact.BuildTransfers`, pg only):
  `BuildInputURL` presigns an input for the builder, and `StoreBuildBinary`
  hashes the staged upload, copies it to its blob key if it is unchanged,
  without the installation lock, and then rechecks the lease, the storage limit
  and the copy under the lock; the running build's staged binary and every
  catalog entry's `binary` are referenced objects, counted in storage and kept
  by `Sweep`. Each unfinished build without a binary reserves
  `Limits.BuildBytes`, and `SaveRuntime` refuses an entry naming a binary its
  build no longer holds. `internal/debugger/store/memory` implements the same
  contract in memory for tests (it is not a server mode: `linea-server` always needs a
  server config). See `docs/internals/artifact-storage.md`.
- `internal/debugger/statementedit/` — pure transform-statement YAML location
  and byte splicing for replace, insert, append and delete; decoded verification
  refuses unsafe shapes without re-serializing the configuration. Platform
  candidates validate proposals and optionally save a new configuration
  artifact.
- `internal/debugger/hostfs/` — the path policy: confines request-triggered host
  reads to allowed roots through `os.Root` (no `..`, symbolic link or junction
  escapes; relative paths refused under roots); `Unrestricted` reads the host
  without confinement. `ReadTree` (`tree.go`) reads a bounded directory tree
  without following links. It imports nothing from this repository. See
  `docs/internals/path-policy.md`.
- `internal/debugger/api/` — local HTTP adapter over the platform: config
  validation and graph, asynchronous runs with SSE events (id `<attempt>:<seq>`,
  a `restart` event before a requeued run's new attempt), snapshots and diffs,
  OTTL evaluation (the playground), assertion re-evaluation, `/api/v1/artifacts`
  (with `GET .../{id}/revisions`, `POST .../export` and `POST .../import`),
  suites (`/api/v1/suites`, a newest-first list as `GET /api/v1/suites`, with
  `/report?wait=true` for the test report, `POST /suites/{id}/rerun` and
  `GET /suites/{id}/junit`), candidates (`POST /api/v1/candidates`) and
  scenarios from runs (`POST /api/v1/runs/{id}/scenario`), comparisons
  (`POST /api/v1/comparisons`, `GET /api/v1/runs/{id}/compare/{after}` with
  `ignore`, `format=markdown` and `wait=true`; schema `RunComparison`, each
  changed record with its `lineage_id` and `path` in both runs and `diverges_at`
  , its parts named as their platform aliases in `openapi.go` `schemaNames`) and
  exports (`POST /api/v1/runs/{id}/export`), the last three failing with the
  coded `OperationError` body (`current_statement` on a stale edit), as do a
  run's `GET .../summary`, `GET .../evidence` (`QueryEvidence` with the family,
  its parameters and the page as query parameters) and
  `GET .../evaluations[/{evaluation}]` (`run_queries.go`; an evaluation never
  returns its scenario text), `POST .../statements/test` (`statement_tests.go`:
  `TestStatement` with `RunID`, 202 with the queued run or 200 for an invalid
  candidate), and `DELETE /api/v1/runs/{id}` and `DELETE /api/v1/suites/{id}`
  (MCP's `linea_delete_run` and `linea_delete_suite` in `tools_deletion.go` and
  CLI `runs delete` and `suite delete` apply the same rules; run retention
  (`run_retention.go`): `PUT` and `DELETE /api/v1/runs/{id}/keep` (`KeepRun`,
  `UnkeepRun`, answering the run with `kept`), `POST /api/v1/runs/prune`
  (`PruneRuns`, `dry_run`) and `GET /api/v1/storage` (`GetStorageUsage`), with
  MCP's `linea_keep_run`, `linea_unkeep_run`, `linea_prune_runs` and
  `linea_get_storage_usage` (`tools_retention.go`) and CLI
  `runs keep|unkeep|prune|storage`; captures and Collector groups are deleted
  through every adapter too); runtimes (`runtimes.go`: `/api/v1/runtimes`
  (catalog runtimes with their connected workers, then stored ones, by name),
  `/runtimes/import`, `POST /runtimes/register`, `GET` and
  `PUT /api/v1/runtime-default`, `/runtimes/{name}` and `/push`, where a server
  without a runtime store answers 503), runtime builds (`runtime_builds.go`:
  `POST /api/v1/runtime-builds`, JSON or a multipart upload with the
  `X-Linea-Upload` header, a `request` part and one tar part per module named by
  `sources[].part`; the paged list, `/{id}`, the `/{id}/events` SSE stream of
  `build` events ending with `end`, and `/{id}/cancel`; MCP
  `linea_build_runtime`, `linea_get_runtime_build`, `linea_list_runtime_builds`
  and `linea_cancel_runtime_build`; CLI `runtime build --server`, which packs
  local module directories, and `runtime builds --server`),
  `GET /api/v1/runtime-storage` and `POST /api/v1/runtime-storage/prune`
  (`runtime_storage.go`), `GET /api/v1/collector-versions` and
  `/{version}/catalog`, `POST /api/v1/manifests`,
  `POST /api/v1/manifests/retarget` and `POST /api/v1/upgrade-checks`
  (`upgrades.go`; blocks, `format=markdown`, ending the request abandons the
  check), `POST /api/v1/config-bisects` (`bisect.go`; blocks, `format=markdown`,
  503 without a fleet connection), and a `runtime` field on runs, inspections,
  OTTL, suites, candidates and comparisons (`after_runtime` too);
  `GET /api/v1/runs/{id}/inputs` (`GetRunInputs`) returns a run's admitted
  sources; live capture is served by `captures.go`, `capture_data.go` and
  `capture_management.go` (`/api/v1/captures`, `.../validate`, `.../{id}` with
  `stop`, `records`, `export`, `fixtures` and `scenario`) and `collectors.go`
  (`/api/v1/collectors`, `.../{id}`, `.../capture-points`, and
  `/api/v1/collector-groups/{id}/members`); `GET /api/v1/audit-events`
  (`audit.go`) pages the audit log; `POST`/`GET /api/v1/tokens` and
  `DELETE /api/v1/tokens/{id}` (`tokens.go`) create (the secret only in that 201
  answer, `no-store`), list and revoke API tokens; `GET`/`POST
  /api/v1/role-bindings` and `DELETE /api/v1/role-bindings/{id}?revision=N`
  (`access.go`) list, add and remove role bindings; `GET`/`POST
  /api/v1/projects`, `PUT /api/v1/projects/{id}`, `POST
  /api/v1/projects/{id}/archive` and `.../unarchive`, `GET /api/v1/library`,
  `PUT /api/v1/library/{artifact}/audience`, `POST
  /api/v1/artifacts/{artifact}/copies` and `PUT
  /api/v1/runtimes/{name}/availability` (`projects.go`) manage projects and
  sharing; every operation reads its project from one `project` query
  parameter (two are `invalid_request`); `GET /api/v1/session`
  (`session.go`) names the signed-in principal and a web session's CSRF token;
  `Options.RemoteAccess` (team mode) answers any Host, where authentication
  rather than a loopback Host keeps it private; a team server mounts the
  platform's OIDC handler at `/auth/` beside it; v1 wire shapes are adapter DTOs
  (`runs.go`) and `errors.go` holds the one code-to-status table; it imports
  only `internal/debugger/platform`, `samples` and `internal/debugger/assertion`
  outcome types. Its OpenAPI contract `internal/debugger/api/openapi.json` is
  generated from the Go types.
- `internal/debugger/mcp/` — MCP adapter over an injected platform, using the
  official Go SDK v1.8.0 and Streamable HTTP. The composing application owns the
  platform; adapters never close it. `linea-server --mcp` mounts `/mcp` beside HTTP on
  the same platform; clients present an API token, checked by the team
  authenticator (there is no loopback Host/Origin guard). Inspection tools
  validate configurations, list/get pipelines and preview OTTL (always
  unverified). Run tools submit explicit quick/scenario runs, wait without
  cancelling on timeout or disconnect, read status, cancel and retrieve recorded
  assertions from a run or ordered suite; run results carry `web_url` when `linea-server`
  embeds the web client. Statement tests validate unsaved edits or complete YAML
  through real quick/scenario runs, returning a patch and recorded statement
  counts; without assertions they are never a pass. Scenario creation inlines
  captured inputs and mock files, names required host environment bindings and
  optionally saves a new artifact with a run origin; its `export` field is the
  reviewable file with its header. Suite submission waits only to observe
  members. `linea_compare_configs` and `linea_compare_runs` (`tools_compare.go`)
  submit a configuration pair on one input and compare runs, returning the
  comparison and its Markdown; waiting never cancels either run.
  `linea_list_runtimes`, `linea_build_runtime` (with `sources`) and
  `linea_get_runtime_build` (`tools_runtime.go`) list and build runtimes,
  waiting only to observe, `linea_get_default_runtime` and
  `linea_set_default_runtime` read and choose the default runtime, and
  `linea_compose_manifest` writes a manifest from a catalog;
  `tools_runtime_manage.go` maps the rest of runtime management: get, delete,
  push and import a runtime, list and cancel builds, retarget a manifest, read
  the support table, a release catalog and runtime storage, and prune (a connect
  result never carries the token); `linea_list_artifact_revisions`,
  `linea_export_artifacts_for_ci` (zip as `archive_base64`) and
  `linea_import_artifacts` (`tools_artifacts.go`) list saved revisions and move
  saved work to and from CI; `linea_list_audit_events` (`tools_audit.go`) pages
  the audit log; `linea_get_run_inputs`, `linea_get_snapshot` (base64),
  `linea_list_suites` and `linea_create_candidate` (`tools_run_reads.go`) read a
  run's root sources, one recorded snapshot and the retained suites, and
  validate a candidate without running it; `linea_list_artifacts`,
  `linea_get_artifact` (a revision's content as `content_base64`),
  `linea_save_artifact` (a stale edit is a `conflict` carrying `current`) and
  `linea_list_projects`, `linea_list_library` and `linea_copy_artifact`
  (`tools_projects.go`) list the caller's projects and the library and copy an
  artifact into the client's project, which the `Linea-Project` header or
  `?project=` on the MCP URL names (`withProject` copies the parameter into
  the header); `linea_list_samples` (`tools_saved.go`, `internal/debugger/mcp` and
  `cmd/linea` may import `samples`, as `internal/debugger/api` does);
  `linea_list_evaluations` and `linea_get_evaluation` (`tools_assert.go`) read
  the evaluations `linea_evaluate_assertions` recorded; `linea_compare_versions`
  (`tools_upgrade.go`) runs an upgrade check and returns it with its Markdown,
  and ending the call abandons it; `linea_bisect_config` (`tools_bisect.go`)
  runs a fleet config bisect and returns the bisection with its Markdown;
  inspection, run, statement, suite and comparison tools take `runtime`, and run
  results carry the report's `runtime`. Inline assertion evaluations are
  recorded beside the run without executing or changing it. The fixed,
  argument-free `develop_ottl_transformation` prompt explains this workflow. The
  server `Instructions` are fixed text (`standins.go`): inline or path sources,
  stand-ins and how a scenario supplies real values, and that inconclusive is
  never a pass. Inspection and run results name `stand_in_components` and
  `references`; their text only counts stand-ins. Terminal runs carry the
  compact `summary` (`Run.Summary()` from the platform, from the report only,
  capped, with incomplete runs stated; `summary.go` only derives its text facts;
  endpoint outputs and component log counts appear only when present) with
  `otel://runs/...` resource URIs (`resources.go`). `linea_get_execution_trace`
  (`tools_trace.go`) reads bounded `QueryEvidence` pages; the run, stage and
  statements resource templates return the same JSON as `linea_get_run` and that
  tool, and failed reads carry the tool error object. No tool returns reports;
  `linea_get_snapshot` returns one snapshot only by its report reference.
  Results are structured JSON; the text is one line of server-computed facts
  (validity, mode, state, cancellation effect, counts, error code), never
  config, IDs, diagnostics or telemetry values. Errors map platform codes,
  diagnostics (always present on `validation_failed`), retryability and conflict
  references and the current statement in one place.
- `internal/debugger/client/` — the CLI's connected mode: speaks HTTP v1 with
  `internal/debugger/api` types to a loopback server (the Compose stack; no
  proxy, no redirects), or with an API token (`NewWithToken`, `$LINEA_TOKEN`)
  also to a team server over https, presenting it as a bearer token on every
  request; `Login` signs in with the OIDC device flow the server names at
  `/auth/cli`, `Refresh` renews it, and `credentials.json` in the user's config
  directory keeps one sign-in per server (`login.go`); sends file arguments
  inline with the argument as label, splits `test` into suites of 32 and cancels
  its submitted runs on interrupt (both runs of a `diff`). `cmd/linea` may
  import it (boundary test).
- `internal/debugger/fleetcapture/` is the debugger's client of linea-fleet: the
  livecapture plugin API (peers, capture points, channels, captures, batch
  delivery and receipts) through fleet's authenticated plugin ingress, and the
  core group and agent reads that resolve capture targets, and (`configs.go`) an
  agent's desired config layers, stored configs, their versions and rollouts for
  the config bisect. It sends the token from `--fleet-token-file` and never logs
  or returns it. `fleetcapturetest` is an in-process fake of that API for tests.
  The debugger runs no Collector listener; fleet owns Collector connections,
  credentials, groups and raw-capture permission (its `raw_principals`).
  `internal/debugger/platform/collectors.go` projects fleet peers and catalogs
  as bounded Collector pages; `collector_groups.go` reads fleet groups and their
  members. `fleet.go` and `fleet_configs.go` manage fleet through the same
  connection: the overview, agents (marked `capture_linked` with their Collector
  IDs), config retry, the event stream, group create, replace and delete at a
  revision, configs and their versions, assignments with a preview, and rollouts
  with their targets and controls. They pass fleet's revisions as `If-Match` and
  map fleet's refusals to platform codes; `fleetcapture.Manage` refuses fleet's
  credential and plugin routes, so the debugger never handles a token other than
  its own. HTTP serves them under `/api/v1/fleet/...` and
  `/api/v1/collector-groups` (`internal/debugger/api/fleet.go`), the CLI as
  `fleet <command>` (`cmd/linea/fleet.go`), MCP as the fleet and collector group
  tools (`tools_fleet.go`, whose texts carry counts, states and IDs only) and
  the web client as the Fleet destination (`web/src/fleet/`). `captures.go` owns
  admission (1–16 targets, 5–280 seconds, fleet's budget ceilings), the
  per-capture channel and fleet operation, stop, expiry and loss (`uncertain`).
  `capture_runner.go` reads the channel and acknowledges each batch with the
  records `capture_data.go` stored. Reconnection or restart never resumes a
  capture. `capture_metadata.go` edits only capture name and description with an
  expected metadata revision, using payload-free checkpoints. The repository
  stores capture segments using bounded, lossless gzip when smaller; raw
  captures are stored like redacted ones. Data ingestion, bounded reads and
  fixture/scenario derivation stay independent of the run queue. See
  `docs/debugger/livecapture.md`.
- `internal/debugger/standin/` — factories for component types outside the
  distribution: untyped settings that marshal redacted, receivers and exporters
  that do nothing, pass-through processors, connectors that forward same-signal
  data, inert extensions. Config resolution and runs add them for missing types;
  the topology mode is `stand_in`, unknown types are `unsupported_component`
  warnings, and assertions on data that passed a stand-in processor or
  connector, or a component using stand-in values (`stand_in_values`), are
  inconclusive. It imports nothing from this repository.
- `internal/debugger/registry/` — upstream component factories of the OCB
  manifest: `components.go` (`Factories` with each kind's `<Kind>Modules` map,
  as OCB records them) is generated from
  `build/linea-runtime.yaml` by `go generate ./internal/debugger/registry`
  (`gen/`) and `TestComponentsAreGeneratedFromTheManifest` fails while it is
  stale; tests also enforce parity with the manifest and pinned versions. It
  includes eight common extensions; `file_storage` validates without requiring
  its directory on this host (`filestorage.go`, a generator override). The same
  generator (`-ottl`, `-features`) writes
  `internal/debugger/compat/ottl/variant.go` and
  `internal/debugger/featureset/featureset.go` for the manifest's release and
  components, `-release` writes `internal/debugger/release/collector.go`, and
  `-gates` writes `gateowners.go` from the worker's import
  graph: who links each feature gate (the Collector, components by
  `componentTypes`, or only the debugger), giving `FeatureGates()` a `scope`
  (`gates.go`; a runtime build regenerates it in the container).
  `-providers FILE` with repeatable `-provider PKG` writes `providers.go`
  (`Providers()`: the module providers compiled in, `registry.Provider`), empty
  for the built-in registry.
- `internal/debugger/release/` — what names this build without linking a
  component: `ProductVersion` (set with `-X` at `VersionVariable`,
  `0.0.0-dev` otherwise), the generated `collector.go` (`CollectorVersion` and
  `Modules` of the manifest) and the feature gate types the registry returns
  (`FeatureGate`, `Gate` and the `GateScope*` values). A runtime build
  regenerates `collector.go` from its manifest, so a worker reports its own
  release.
- `internal/debugger/compat/ottl/` — the OTTL construction the replays use for
  every release from 0.160 on (package `ottlcompat`): a fixed facade
  (`compat.go`: transform context constructors and `Functions`) bound by the
  generated `variant.go` to one variant package `since<NNNN>`, named by the
  first release it applies to (`since0160` is a nested module with its own
  `go.mod`, since it needs `pkg/ottl` v0.160; `manifest.OTTLVariants` lists the
  variants and a test keeps them equal to the directories).
- `internal/debugger/features/` — the optional analysis features: transform
  statement replay, count aggregation, routing and tail_sampling explanations,
  each compiled in only when the manifest has its component. Feature packages
  (`internal/debugger/ottlreplay`, `internal/debugger/aggregation`,
  `internal/debugger/routing`, `internal/debugger/sampling`) provide their
  function in `init`; `internal/debugger/featureset` (generated) imports those
  of the manifest and `engine/local` imports it. A runtime without a feature skips
  that analysis; its hello and reports name it in `missing_features`, and a
  build warns.
- `internal/debugger/manifest/` — OCB manifest parsing and registry generation :
  the release from `otelcol_version` or the core components, entries, replaces,
  warnings for OCB-only fields, and `Discover` (`discover.go`): a module
  source's component packages, those whose `NewFactory()` returns a Collector
  `<kind>.Factory`, with the type their `NewFactory` states (a literal, a
  constant or mdatagen's `metadata.Type`); `DiscoverProviders` (`providers.go`)
  the packages exporting the SDK's `DebuggerProvider`, their name read from the
  returned literal, and the SDK version the source requires, and
  `GenerateProviders` writes the registry's `providers.go`; it imports nothing
  from this repository.
- `build/versions/` — the support table (package `versions`): `Oldest` (0.160.0)
  and each verified release's directory (`build/versions/<version>/`: upstream's
  otelcol-contrib `manifest.yaml` unchanged, the dependency set
  `dependencies.mod`/`.sum`, `unsupported.yaml` and the catalog `catalog.json`,
  which records each component's `deprecated_type` alias), embedded: 0.160.0,
  0.161.0, 0.162.0, 0.163.0 and 0.164.0 (core 0.164.0 with contrib 0.164.1,
  upstream's release pairing).
  `scripts/dependency-set.sh --version V [--catalog-only]` (Windows:
  `dependency-set.ps1 -Version V [-CatalogOnly]`) generates a release's set and
  catalog in the pinned Go image (the first run downloads about 5 GB), resolving
  the root module with only the release's OTTL variant and without the root's
  own OpenTelemetry requirements, so the release's components choose every
  OpenTelemetry library version. `build/manifest.go` embeds the built-in
  manifest (`build.Manifest`).
- `internal/debugger/catalog/` — pure manifest composition from a catalog:
  presets `all` and `built-in`, a configuration's components and picked
  `<kind>/<type>` ones, custom components from supplied sources (`custom.go`,
  `AddCustom`: entries spliced into the text after their kind's, comments kept;
  a configuration's type a custom component has is not unknown), selecting a
  component by its deprecated type too (with a warning naming the current one)
  and naming unknown ones; `Retarget` (`retarget.go`) moves a manifest's core
  and contrib module versions to another release's catalog in the text, keeping
  comments, naming missing and custom modules.
- `internal/debugger/upgrade/` — pure upgrade checks: `Assess` turns two
  inspections, two gate lists and per-scenario comparisons into the verdict
  `compatible`, `configuration_breaks`, `behavior_changes` or `inconclusive`
  (schema `upgrade_check_schema` 0.1) and `Markdown()`. A version difference
  alone is never a behavior change, and gate changes never alter the verdict; a
  gate is listed when the Collector links it or a configured component does (its
  `scope`), never when only the debugger does (the mock filelog receiver), and
  gates without a scope fall back to their ID.
- `internal/debugger/bisect/` — pure fleet config bisection: `Compose` merges
  config layers as the OpAMP supervisor does (maps by key, lists and scalars
  replaced, `service::extensions` concatenated without duplicates, `${...}`
  kept), `Search` probes the bounds and halves the range, skipping versions it
  cannot judge, `Judge` makes any failed assertion bad, and `Assemble` gives the
  verdict `found`, `no_regression`, `already_bad` or `inconclusive` (schema
  `bisect_schema` 0.1) with `Markdown()`. See
  `docs/internals/fleet-config-layers.md`.
- `internal/debugger/worker/` — out-of-process execution: protocol 2,
  length-framed JSON over a worker's stdin/stdout (hello with the runtime
  identity, the runtime's feature gates and its providers,
  execute/inspect/OTTL/cancel requests, events, results, errors keeping their
  engine sentinels). `Serve` is the worker side and never reads host files;
  it and `Identity` describe and run the in-process Collector (`engine.Use`),
  so the package links none and `Identity` fails with `ErrNoCollector` in a
  process without one;
  `Client` (`Start`) drives one worker process and implements
  `platform.Runtime`. A worker that exits fails its requests as
  `engine.ErrRuntimeUnavailable` with its log tail. Its tests use the test
  binary as the worker and compare reports byte for byte with the in-process
  engine; the platform-on-a-worker tests are the external `platform_test.go`
  (package `worker_test`), since the platform imports the worker. The WebSocket
  framing both ends of a connected worker share (`websocket.go`) is the
  upgrader, the `stream` that carries protocol 2 frames as binary messages, the
  idle timeout and the server URL a worker is given (`parseServerURL`: `https`,
  or `http` to any host the operator chose; a CA needs `https`). A debugger
  never dials a worker, so there is no listening side. Connecting workers
  (`connect.go`, `transfer.go`): `Connect` is the worker side of `--connect`. It
  dials `ConnectPath` (`/v1/workers/connect`) with the registration token as a
  bearer token, says hello with its runtime `name` and `capacity`, serves
  requests (each with its own engine, one beyond its capacity refused as
  `invalid_request`) and reconnects with backoff from 1 s to 30 s; a 401 stops
  it. `Accept` is the server side: it checks the token (`ConstantTimeEqual`),
  refuses upgrades carrying an `Origin`, upgrades and returns a `Client` the
  server pings. A bundle over `InlineLimit` (1 MiB) arrives as `bundle_url` with
  `bundle_digest` and `bundle_size`, and a report over it leaves through
  `result_url`, checked by size and digest on both sides; the wire error kind
  `runtime_unavailable` carries `engine.ErrRuntimeUnavailable`. The URLs come
  from `artifact.Transfers` (`RunBundleURL`, `StageURL`, `TakeStaged`),
  implemented by `store/pg`; `store/memory` does not implement it and sends
  everything inline. Pool workers (`pool.go`): `ServePool` dials `ConnectPath`
  with the worker token like `Connect`, but says hello with `Hello.Pool`
  (platform, warm count) and serves any runtime named by `message.Binary` on
  execute, inspect and ottl requests; a request beyond `Capacity` across all
  runtimes is refused as `invalid_request`. A request without a binary runs the
  pool's own executable, the built-in runtime of its release: the hello sets
  `Pool.Own` when the pool found its executable, the child starts on demand and
  counts against `Warm` and `Idle` like any runtime, and its failures never mark
  anything suspect. A pool that cannot find its executable refuses such requests
  as unavailable. The pool downloads the binary to a cache directory (size and
  sha256 checked while it streams, renamed to its hex digest), starts it as a
  stdin/stdout worker (environment without `LINEA_*`, stderr forwarded to its
  log), checks its hello (protocol, and `Runtime` equal to `Binary.Identity`)
  and relays the request frame under a fresh child-side ID, with `Binary`
  cleared and the bundle and result URLs unchanged. Its events and result are
  relayed undecoded under the server's ID, and a server `cancel` becomes a
  cancel of the child's request. A digest that fails any check is suspect for
  the life of the process (`engine.ErrRuntimeUnavailable`, no new fetch), and
  every answer it causes carries `wireError.Suspect`, which the client decodes
  as `ErrSuspectBinary`; a child that exits fails its requests the same way and
  is started again by the next one. `Warm` (soft cap on running children, least
  recently used idle first), `Idle` and `CacheBytes` (least recently used
  binaries no child uses) bound it, and it sends `pool_state` (the identities it
  runs, read by `Client.Warm`) whenever they change. Its tests run fake runtime
  binaries built from `testdata/fakechild`, not a Collector. Builders
  (`builder.go`) have their own route and protocol (`BuilderPath`
  `/v1/builders/connect`, `BuilderProtocol` 1, the same framing): `ServeBuilds`
  is the `linea-builder` side, saying hello with its product and Go versions,
  platform, name and capacity, running `build` jobs up to its capacity under
  their deadlines, sending `build_log` lines and a result, and cancelling a
  build on `cancel` or a lost connection. `AcceptBuilder` is the server side: a
  401 with realm `linea-builders` on a wrong token, and a policy close on
  another protocol or product version; `BuilderConn.Build` sends a job and waits
  for its result, sending a cancel when its context ends.
  `BuildCLI` (`cli_builds.go`) compiles the CLI itself for `linea build`: only
  a direct composition (no repository builds, unrestricted paths) accepts it,
  the manifest goes through the same `buildInputs` as runtime builds, and it
  waits for `container.Native.BuildCLI` (or `Docker.BuildCLI`) instead of
  queueing. Compile failures are `validation_failed`, no Go or Docker
  `unavailable`.
- `internal/debugger/workersrc/` — the worker's source embedded in the debugger
  for runtime builds: `go generate ./internal/debugger/workersrc` packs the
  module's packages `cmd/linea-worker` imports, every OTTL variant, every
  feature package and the registry generator (for gate owners) (linux view, no
  tests), the packages of `cmd/linea` for linux, windows and darwin (for
  `linea build`; never the archive itself, so a built CLI has none) with
  `go.mod`/`go.sum`, and the provider SDK as `collector/provider/`,
  into `static/worker-source.tar.gz`, generated and ignored by git like the web
  client; without it `Embedded` returns `ErrNotBuilt`. Tests pack the checkout
  with `Pack`.
- `internal/debugger/container/` — the docker CLI: `Build` writes a build module
  (the worker source with the manifest's generated registry, release line,
  OTTL variant binding and feature set, the source's `go.mod` as the dependency set; modules
  off it are a `ModuleError` naming both versions; with a verified release's
  `DependencySet` every module is pinned to it, custom `Sources` are added as
  replaces, a source whose `go.mod` requires a Collector module above the
  release is refused before resolving (`checkSources`, in `Check` and `Build`)
  and a custom module moving a Collector module off the release is refused after
  resolving; missing modules are named in `BuildError`; the provider SDK is
  required and replaced by the embedded `collector/provider/`, a source
  requiring a newer SDK is refused (`checkSDK`), and the sources' providers are
  compiled into `providers.go` and named in the label `io.linea.providers`),
  generates its gate owners and compiles it in the pinned official Go image
  (`GoImage`) with the user's module source and the cache volumes
  `linea-gomod`/`linea-gocache` (`Offline` is `--network none` and
  `GOPROXY=off`), packages a runtime image `FROM scratch`
  (`linea-runtime:<name>`, labels `io.linea.*`) and keeps it only once its
  worker says hello. `Start` runs a worker container (no network, read-only
  root, `/tmp` tmpfs, user 65534, memory limit); `Images` (untagged ones too),
  `Inspect` and `Remove` read Docker's store, the runtime list; `storage.go`
  measures and removes the two cache volumes and lists or removes stopped
  `linea-build-*` and `linea-worker-*` containers, refusing any other name. Its
  Docker tests run only with `LINEA_DOCKER_TESTS=1`. `Native.Build`
  (`native.go`) is the same build without Docker, for `linea-builder`: the plan,
  resolve and compile sequence is shared (`buildModule`, and the go commands
  `resolveArgs`, `genArgs`, `compileArgs`, from which the Docker scripts are
  written), and only the step runner differs. The runner extracts the build
  module into a fresh directory under `WorkDir` (files stamped with the
  extraction time, as `tar -m` does in the image), runs the go commands as host
  processes in their own process group with the container's environment
  (`CGO_ENABLED=0 GOOS=linux`, `GOARCH` the host's, `GOTOOLCHAIN=local`,
  `GOGC=25`, `GOENV=off`, `GOWORK=off`, caches and temporary files under
  `WorkDir`, the module variables of the process and, for a private module
  source, its `NETRC`, `GOAUTH`, `SSL_CERT_FILE` and `SSL_CERT_DIR`
  (`moduleAccessEnv`, native builds only, since they name the builder's files);
  nothing else of the host's environment), writes the binary to a file, hashes
  it streaming, and starts it once for its hello with the same check as the
  Docker path. `GoVersion` is the Go release of `GoImage` (a test keeps them
  equal); binaries are identical to the Docker build's only with that toolchain,
  which `linea-builder` checks at start. `ImageExport` (`export.go`) pushes a
  built binary as the runtime image the Docker build makes (`FROM scratch`,
  `/worker` 0755, user 65534:65534, entrypoint `/worker`, the `runtimeLabels`
  labels) to an OCI registry over the distribution API, without a Docker daemon:
  one gzipped layer, blobs a registry already holds are not sent again, basic or
  bearer challenges answered from a Docker config's credentials (the host's
  `credHelpers` helper, else `credsStore`, else `auths`), and the result named
  by the manifest's digest. `ImagePull` (`pull.go`) reads such an image back
  without Docker: it resolves a reference (an index to the host's platform),
  refuses an image for another platform (`ErrPlatform`; only Linux hosts run
  images natively) or one without `io.linea.runtime` and a single layer holding
  `/worker` (`ErrNotRuntime`), checks the manifest, config and layer against
  their digests, and keeps the binary and an `image.json` in a cache directory
  named by the resolved digest, written to a temporary directory and renamed
  into place. A reference by digest that is cached reads no registry.
  Credentials are the Docker client's (`DockerConfig`, with credential helpers);
  loopback registries are reached over http. `PruneImages` empties the cache.
  `ModuleArchive` is the reader of an uploaded module source: the platform
  checks uploads with it and the builder unpacks them with it.
  `LINEA_NATIVE_TESTS=1` runs the build for real (see
  `docs/internals/development.md`).
- `internal/debugger/config/` — Collector-backed config resolution, validation,
  compatibility diagnostics, the redacted effective config and immutable
  provider snapshots shared by planning, startup and reference comparison.
  Private snapshots contain source/environment values and are never ordinary run
  metadata. A reference always resolves (`references.go`): the scenario
  (`environment`, `mocks.files` by path), then the host under the policy, then a
  stand-in string with a reason, never reading what the policy refuses.
  `CaptureSnapshot` captures without component factories, for a runtime that
  resolves elsewhere. See `docs/internals/config-resolution.md` and
  `docs/internals/environment-bindings.md`.
- `internal/debugger/topology/` — static pipeline graph and component execution
  modes derived from a resolved config.
- `internal/debugger/instrument/` — processor and connector factory decorators,
  pipeline attribution, the evidence `Budget` shared by the recorder, exporter
  capture and the reference run (only observation stops; lifecycle events are
  always kept), components' own logs (`component.log` events) and metrics
  (`component_metrics`, `telemetry.go`), the status watcher of custom extensions
  (`extension.go`), and the versioned event recorder (event schema 0.6,
  content-addressed snapshots, `from_seq` call causality, tail_sampling
  decisions recorded through the processor's decision hooks, run-scoped record
  object numbers in `objects`; see `docs/internals/record-objects.md`). It
  links no component implementation: the tail_sampling hook lives in
  `instrument/tailsampling`, which the runtime registers on its recorder
  (`SetDecisionHook`).
- `internal/debugger/runtime/` — in-process Collector execution,
  receiver/exporter substitution for isolation, inert extensions in every run
  (`extensions.go`: settings validate, nothing starts, `file_storage` hands out
  a client that keeps nothing; an exporter with `auth:` is a reference
  prerequisite; an extension whose module, from the registry's `<Kind>Modules`
  maps, is not upstream runs for real, unwrapped, and a run with one in the
  service adds the `linea_status` watcher, recording its
  `component.started`/`component.stopped` from the Collector's status reports;
  tests add components through `factoriesOf`), custom processors, connectors and
  extensions created with their client endpoints simulated (`endpoints.go`:
  `endpoint_outputs`, `simulated_endpoints`, mode `dependency_mocked`), the
  local `debug` and `nop` exporters running for real with what reaches them
  recorded as outputs (`local_exporters.go`: events with mode `real`, in
  reference runs too), the run timeline (input waits and the settle derived for
  timer-driven processors, and the OTTL `Now()` reads that make a config
  wall-clock dependent), reference runs (against the same mocks, with declared
  client addresses) and run status.
- `internal/debugger/runresult/` — the options of a run (`Options`, `Input`;
  `runtime` aliases them) and what it produces, as pure data: `Result`
  with its status, inputs, outputs, truncation, settle and endpoint outputs,
  output comparison (`Compare`), the normalization of mocked outputs, and
  pipeline-order and metric-order canonicalization for normalized reports.
  Every interface reads it without linking the executor.
- `internal/debugger/analysis/` — stage outcomes, structural diffs, connector
  routes and per-record lineage computed from recorded events (never re-executes
  components); records are paired by their recorded object first (confidence
  `object`), then logs and metrics by content with stated confidence; golden
  analyses of events without objects are in its `testdata/without-objects/`.
  Lineage continues through aggregations into the data points derived from
  counted records, and pairs what simulated endpoints received with lineages by
  content only (`endpoints.go`, lineage `sent`).
- `internal/debugger/aggregation/` — replays aggregating connectors (count) on a
  recorded input to attribute input records to emitted data points, verified
  against the observed output.
- `internal/debugger/routing/` — explains routing connectors: replays the real
  connector on a recorded input with one sink per rule, verified against the
  recorded deliveries, to say which rule routed each record (report `routing`,
  report schema 0.5).
- `internal/debugger/sampling/` — explains tail_sampling decisions: replays the
  processor (the real factory) on each trace's evaluated spans, in full to
  verify the recorded decision and per policy, with the evidence each policy
  read (duration, status codes, attribute values).
- `internal/debugger/ottldebug/` — the types of per-statement OTTL replay
  reports.
- `internal/debugger/ottlreplay/` — per-statement OTTL replay of transform
  processor configs with the upstream OTTL APIs (through
  `internal/debugger/compat/ottl`), verified against observed output; the
  `ottl_replay` feature.
- `internal/debugger/scenario/` — scenario YAML (schema version 1) loading,
  including the assertion schema (targets: an exporter, a processor instance or
  a simulated endpoint, `endpoint: {component, path}`), the `mocks` section
  (with `mocks.providers.<name>`, opaque settings handed to module providers as
  JSON), the timeline (`inputs[].after`, `execution.settle`) and
  `execution.timestamps`. Pure scenario-from-run conversion inlines captured
  dependencies, verifies payloads and mock bytes, and refuses non-UTF-8 text
  rather than writing binary YAML.
- `internal/debugger/mock/` — mock providers: components that depend on the
  environment run for real against a substitute (mode `dependency_mocked`), such
  as the filelog receiver reading the scenario's files from a per-run sandbox,
  the k8sattributes processor querying an in-process Kubernetes API serving the
  scenario's objects, and the spanmetrics connector running on a simulated clock
  that flushes when the run ends; each reports what it supplied and its
  limitations. Those three live in `mock/builtin` (`Providers`), which links
  the components they substitute; the executor passes them to `mock.Build` and
  `mock.Gaps`, so `mock` itself keeps only types. `endpoints.go` finds the `configgrpc`/`confighttp` clients in a
  custom component's decoded configuration by type and hands its factory a copy
  pointed at loopback OTLP sinks (`endpoint_sinks.go`: OTLP export served,
  anything else refused and counted; `endpoint_factories.go` wraps the
  factories). Every provider shares one lifecycle (`session.go`): a `Session`
  prepares the built-in mocks (`mock.PrepareBuiltin`) and the module providers
  (`plugin.go`: `Plugin`, `CheckSettings`) through the SDK's `Run`, runs their
  hooks (ready, each input, inputs done, close), serves the endpoints they
  declare on loopback listeners and turns their evidence into declarations; a
  hook's error or panic fails the run naming the provider, a failing close is a
  warning. `internal/debugger/runtime/plugins.go` instantiates
  `registry.Providers()` (`Plugins`, refusing empty or duplicate names).
- `internal/debugger/assertion/` — evaluates scenario assertions against
  recorded run observations (exporter outputs, processor events, endpoint
  outputs) into Pass/Fail/Inconclusive/Error.
- `internal/debugger/evidence/` — pure projections of a terminal run's report:
  the run summary (with endpoint outputs and component log counts, an
  error-level entry a `component` error) and the query families (stage changes,
  records, journey, statements, events) cut into byte- and item-bounded pages
  with oversize entries and opaque cursors; it reads only the report and its
  recorded snapshots.
- `internal/debugger/comparison/` — pure comparison of two terminal runs from
  their reports (comparison): outcome `same`/`different`/`inconclusive` with
  coded reasons, per-exporter record multisets, per-injected-record fates and
  field changes by OTTL path when the inputs digests or input identities match
  (records no injected record leads to are paired by content, with medium
  confidence), routes, errors, assertions, counted ignore rules, and
  `Markdown()`. Platform `CompareRuns` (`comparisons.go`) supplies each run's
  mode, build and `BundleParts.InputsDigest` and, when the digests differ, the
  digest of `scenario.InputIdentity` of the self-contained scenario the run
  derives to; it never executes or persists. `SubmitComparison` admits a before
  and an after configuration on one captured input (`engine.ShareInputs`), both
  or neither.
- `internal/debugger/authz/` — what a principal may do: the permission catalog,
  roles, (subject, role, scope) bindings and a default-deny policy, which
  `Extend` widens by more bindings without changing the original. Only the
  platform calls it; adapters see its decisions as `forbidden` errors. It
  imports nothing from this repository.
- `internal/debugger/provenance/` — bounded input lineage values (capture stream
  and input selections) shared by scenarios and the platform's capture inputs.
  Metadata never grants transport permissions or changes replay.
- `internal/debugger/oidctest/` — a fake OpenID Connect provider for tests
  (discovery, authorization code with PKCE, device flow, refresh tokens).
- `internal/debugger/webui/` — embeds the built web client (`static/app/`,
  generated and ignored by git) and serves it with client-route fallback.
- `internal/debugger/telemetry/` — signal-agnostic OTLP payloads, JSON encoding,
  typed record flattening and fingerprints, record timestamp shifting (rebasing)
  and `Objects`, the only reader of the pdata record wrappers' object pointers
  (layout-guarded, off with the pdata pooling gate).


## Repository folders

`samples/` holds the samples users can pick in the web client (Go package
`samples`, embedded and served by `GET /api/v1/samples`): `index.yaml` lists
them, and each `samples/<id>/` has `config.yaml`, `scenario.yaml` and the
quick-run payload `input.json` (mock fixtures are inline in the scenario).
`TestSamplesRunAsDocumented` runs each one
and checks the outcome `index.yaml` declares, so update both together.

`examples/` holds copyable integration examples: `examples/ci/github-actions.yml`
runs `test` and a pull-request `diff` with a pinned debugger build, documented
in `docs/debugger/ci.md`. `TestCIExampleRunsOnASample` (`cmd/linea`) parses it
and runs its commands on a sample; keep each linea command on one line
with quoted variable arguments so the test can read it. Package
`examples/ci` embeds the workflow for the platform's Export for CI,
whose archive keeps the workflow's default `CONFIG` and `TESTS` paths and
points `SCENARIO` at the first exported scenario.
`examples/kubernetes/base/` deploys connecting workers of a runtime:
a ServiceAccount, a Deployment running `--connect` with the token from the
Secret generated from an ignored `token` file (as `LINEA_WORKER_TOKEN`), the
local container's limits, no Service or probe, and a NetworkPolicy that
admits no ingress and egress only to the team server and DNS. It is
documented in `docs/debugger/remote-workers.md`; the debugger never applies
it.


Keep user-provided Collector YAML, fixtures, scenarios, and generated snapshots
separate from source code. Test fixtures live in `testdata/`: assertion
scenarios used as regression examples in `testdata/multisignal/tests/`, the
acceptance inputs in `testdata/acceptance/`, and the transformation problem in
`testdata/paging/`, the environmental-mocking scenarios in
`testdata/receivers/`, and the component conformance configs in
`testdata/components/` (each checked against the reference Collector by
`TestExtendedComponentsMatchTheReference`, except the tail_sampling timeline
scenario, run by `TestTailSamplingFollowsTheScenarioTimeline`, and the
wall-clock filter, run by `TestRebasedTimestampsFollowTheRunsClock`), the
lineage inputs in `testdata/lineage/` (records only their objects tell apart,
run by `TestExecuteFollowsRecordsByObject`), and the performance workloads W1 to
W4 in `testdata/perf/` (kept runnable by `TestPerformanceWorkloadsRun`; timed,
never asserted, by benchmarks and `scripts/measure-performance.sh`; `.ps1` on
Windows), and the lab configuration in `testdata/usability/` (unmodified, with a
trace and a metric input, run through MCP, HTTP and the CLI on a lab team server
by `TestLabConfigWorksThroughEveryInterface`), and the custom `stamp` processor
module of the runtime build tests in `testdata/runtimes/stampprocessor/`. Do not
alter an input Collector configuration to make it debuggable.

