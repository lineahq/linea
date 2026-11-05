# Workers, pools and builders

Mechanisms under [running workers](../debugger/remote-workers.md) and
[building debug Collectors](../debugger/runtime-builds.md). Package-level
detail (`worker_pool.go`, `builders.go`, `internal/debugger/worker/`) is in
[architecture.md](architecture.md); the server side of the worker connection is
in [debugger-team-server.md](debugger-team-server.md#worker-connection).

## Worker protocol

- All worker modes (standard input and output, `--connect`, `--pool`) speak
  worker protocol 2.
- The worker image `deploy/docker/linea-worker.Dockerfile` builds
  `linea-worker` from this module and carries the label
  `io.linea.runtime=default` that `linea runtime register` accepts.

## Pool worker

A pool says hello with its name, capacity, platform (`GOOS/GOARCH`) and the
number of idle runtimes it keeps, over the same route and token as any worker.
The server names the runtime binary in each request it sends.

- For each runtime in use the pool downloads the binary once through a
  presigned URL, checks its size and sha256 digest while it streams, keeps it
  in the cache directory and starts it as a child process: a normal
  standard-input-and-output worker, one child per runtime.
- The pool forwards each request to its child and relays the child's events and
  result. The child fetches the run bundle and uploads a large report itself,
  and a cancel reaches the child.
- A pool also runs its own build, the built-in runtime of its release, since
  `linea-worker` links the built-in components. A request that names no
  binary starts the pool's own executable as a child, with nothing to
  download. The child starts with the first such request and stops when idle.
  A pool of the server's release registers the built-in runtime, so a team
  server with only a pool runs requests that name no runtime.
- A worker says it is the built-in runtime of its release with
  `Hello.BuiltIn` (`built_in`). Every runtime build links its worker with
  `-X github.com/lineahq/linea/internal/debugger/worker.custom=true`
  (`compileArgs`), so only the release's own `linea-worker` sets it. The flag
  is not part of the runtime identity. The server binds a worker as
  `built-in` when it sets the flag and reports the server's `ProductVersion`;
  it compares no registry, since it links none.
- The pool announces the runtimes it holds running to the server after every
  change, so the server can prefer a pool that has one warm. With several
  server instances, each records its pools' warm runtimes in the shared
  database. For up to 2 s after a run is queued, a pool that would have to
  start the runtime leaves the run to a pool of another instance that holds it
  running.
- `--name` defaults to `pool`.

### Guarantees

| Situation | Behavior |
| --- | --- |
| A binary whose size or digest differs from what the server announced, that does not start, whose protocol is not the server's, or whose hello names another runtime than the request's | The request fails as `unavailable` and the binary is distrusted until the pool restarts: later requests fail at once without a new download. The pool tells the server, which records the binary as suspect on its catalog entry (the pool's platform, the time and the cause; shown by the Runtimes page and `GET /api/v1/runtimes` as `catalog.suspect`). Runs on it then fail at once as `unavailable`, naming the cause, on every pool, until the runtime is built or registered again. |
| A cached binary | Hashed before its first start in the process; a damaged one is fetched again. |
| A failed download (object store unreachable) | Not held against the binary. |
| Concurrent first requests for one runtime | Share one download and one start. |
| A child that exits | Fails the requests it ran as `unavailable` and is dropped; the next request starts it again. |
| A child that runs a request | Never stopped for `--warm` or `--idle`, and its binary is never evicted. |
| A child's environment | The pool's without every `LINEA_*` variable, so the registration token never reaches it. Its standard error goes to the pool's log line by line (cut at 4 KiB) with a short runtime prefix. |
| Connection lost | The pool cancels what runs, stops every child (closing its input, then killing it after 10 s) and keeps the cache. It reconnects like any worker. |

## Builders

A builder (`linea-builder`) dials `/v1/builders/connect` with its own token and
reaches the build inputs and the binary output only through presigned URLs. It
holds no database or bucket credentials. Limits, reuse, shutdown, environment
and image export are in
[building debug Collectors](../debugger/runtime-builds.md#building-on-a-team-server)
and [Docker Compose](../debugger/deploy-compose.md#the-builder).

- Hello: platform, Go version, capacity. A builder of another release than the
  server is refused.
- Inputs: the manifest and one tar per uploaded module.
- Build identity: manifest, sources, Go image and builder version.
- `deploy/docker/linea-builder.Dockerfile` copies the toolchain
  (`/usr/local/go`) of the exact Go image the CLI builds in
  (`container.GoImage`) onto Alpine with git; runtimes build with
  `CGO_ENABLED=0`, so the image carries no C toolchain.
- Registry credentials for export come from the Docker `config.json` that
  `--registry-auth` names (`auths`, or `credHelpers` and `credsStore` through
  `docker-credential-<name>`).

## Local builds

- A custom module's requirements are checked against the release before
  anything resolves. On a verified release a module written for 0.162.0 built
  at 0.161.0 is refused at once ("a custom module needs a newer Collector than
  0.161.0") rather than failing on a package the later release removed.
- A git source is resolved to its commit when the build starts.

## Manifests

- `runtime manifest --source` and `--git` add an entry for every component
  package found in the module: a package with a `NewFactory()` returning a
  Collector receiver, processor, exporter, connector or extension `Factory`.
  Test files, `internal`, `testdata`, `vendor` and nested modules are skipped.
  The entries name the module at `v0.0.0` without `path:`.
- The debugger reads `dist.otelcol_version` (or derives it from the core
  components), the component lists, `replaces` and local `path:` entries of a
  manifest.

## Runtime storage

- Only the debugger's own objects are touched by a prune: images with its
  runtime label, its two cache volumes and containers named `linea-build-*` or
  `linea-worker-*`.
- The image state (`named`, `replaced`, `tagged`, `stale`) is decided by the
  store's `Get` by name; `ListRuntimes` hides replaced images.
