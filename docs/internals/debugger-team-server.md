# Team server mechanisms

How the team server behaves under the user documentation in
[team-server.md](../debugger/team-server.md): change feed, leases, worker
connection, presigned transfers, retention and fleet attribution. The package
map is in [architecture.md](architecture.md), the storage layout in
[artifact-storage.md](artifact-storage.md), and the worker and builder
protocols in [debugger-workers.md](debugger-workers.md).

## Authorization

- The built-in roles' permissions are declared in
  `internal/debugger/authz/authz.go`.
- A user's principal ID is `user:` followed by a hash of the issuer and user
  name; a service's is `service:<id>`.
- Managed role bindings and the project set live in PostgreSQL. Every instance
  applies a change within a second, from the change feed. A write names the
  revision it read; a stale one is a conflict.
- Server config bindings are listed with source `config` and IDs `config-N`.
- Audited operations: `AddRoleBinding`, `RemoveRoleBinding`,
  `CreateProject`, `UpdateProject`, `ArchiveProject`, `UnarchiveProject`,
  `SetLibraryAudience`, `CopyArtifact`, `SetDefaultRuntime`, `EvictRuns`.

## Storage

- Records are PostgreSQL rows; payloads are objects addressed by SHA-256.
- At startup the server applies its embedded migrations under an advisory lock
  and checks that the bucket exists.
- Retention and eviction (admission, kept runs, tombstones, sweep) are in
  [artifact-storage.md](artifact-storage.md). The loop runs every 5 minutes
  and soon after admissions; each pass is audited as the `system` principal
  with the number of runs removed for each reason.

## Runtime catalog

- `linea runtime register` starts the image's worker with the local Docker
  only to read its hello, then sends the name, the image reference, its
  registry digest and the hello to `POST /api/v1/runtimes/register`
  (permission `runtime.manage`). `--pull` pulls the image first. The server
  runs no container.
- The catalog records the hello's runtime identity: a digest over the worker's
  registry, gates and linked modules. Runs on a name go to connected workers
  whose hello reports that identity, whatever their `--name`. A worker whose
  identity no entry records connects but serves nothing.
- An entry built on the server keeps its build's manifest, as sent
  (`RuntimeEntry.Manifest`, at most 1 MiB, migration
  `016_runtime_manifest.sql`), so listing and reading the runtime return it
  after the build is trimmed. Entries saved before that migration have none;
  they are listed with their build's manifest while the build is kept (the
  build's manifest is in object storage, which the migration cannot read). A
  registered entry has no manifest.
- An entry is confirmed when a connected worker of its identity first says
  hello. Registering a name again replaces the entry. The catalog holds at most
  256 names. `runtime rm` is refused while runs on the entry are queued or
  running.
- Inspections and OTTL previews need one connected worker and otherwise fail as
  `unavailable`, as does a name outside the catalog. A server stores, pushes
  and imports no image; those operations are `unavailable`.
- A worker whose hello reports another worker protocol is refused
  (`unsupported_build`).

### Default runtime

- HTTP: `GET` and `PUT /api/v1/runtime-default`, body `{"name": "..."}`. MCP:
  `linea_get_default_runtime` and `linea_set_default_runtime`.
- Reading needs `runtime.read`, choosing `runtime.manage`. A name that is
  neither `built-in` nor in the catalog is 404 ("register or build it first").
- The choice is stored in PostgreSQL and followed by every instance through the
  change feed. The audit entry for `SetDefaultRuntime` lists the previous and
  the new default as resources.
- Removing the default's catalog entry is 409 `conflict`.
- The server config has no `runtime` key.

## Worker connection

- The route is `GET /v1/workers/connect`, outside `/api/`, so it skips
  principal authentication. The worker presents the registration token as a
  bearer token; the server compares it in constant time.
- Without `worker_token_file` the route answers 404. A wrong token is 401, and
  the worker stops instead of retrying. An upgrade carrying an `Origin` header
  is refused so browsers cannot open it.
- The server pings every 30 seconds so a gateway's idle timeout does not close
  the connection.
- Leases: the server claims queued runs of the worker's runtime up to its free
  capacity and renews their leases while it stays connected. If it
  disconnects, a run whose 30 s lease ends is requeued once, then lost. A
  requeued run executes as a new attempt: its event stream sends `restart`,
  and the new attempt's events replace the earlier ones. The worker reconnects
  with a backoff of 1 s to 30 s until stopped.
- An instance claims the runs it learned of from the change feed. As a safety
  net, every heartbeat (a third of the lease, 10 s) it also claims for each
  runtime its connected workers run without starting one: a worker's runtime,
  and a pool's own build and the runtimes it holds running. A run another
  instance queued that the feed somehow missed therefore waits at most one
  heartbeat for a free slot.
- Inspections and OTTL previews take a free worker slot ahead of queued runs.
  With instances the request is a row in PostgreSQL that any instance with a
  connected worker of the runtime claims; the instance holding the worker
  runs it at once. With no connected worker, or none answering within 30
  seconds, they fail as `unavailable`.
- Transfers: a run bundle over 1 MiB reaches the worker as a presigned URL
  valid 5 minutes. A report over 1 MiB leaves through a presigned upload that
  lives as long as the run's execution budget plus 5 minutes. The worker
  checks size and digest; the server verifies the report (at most 256 MiB)
  and deletes the staged object. Workers therefore need network access to the
  object store `endpoint` the server presigns for. Only the in-memory store
  (tests) sends everything inline.
- Connected-worker and slot counts of a catalog entry include the workers of
  every instance. A killed instance's workers count for up to one lease (30 s).
- Reports name a worker's runtime as kind `remote`, with its name and no
  endpoint; the built-in runtime is kind `built_in` wherever it runs.

### The built-in runtime on workers

- A worker whose hello reports this server's own release and registry
  (product and Collector version, modules, feature gates, providers)
  registers the built-in runtime. Requests that name `built-in`, or no runtime
  while it is the default, run on it.
- Each release keeps its own entry, so instances of two releases in a rolling
  upgrade each run the built-in runtime on workers of their own release. An
  entry of another release that no worker serves is removed when a worker
  registers its own.
- A pool worker of the release registers it too and runs it as its own build;
  a pool of an earlier debugger does not.
- The server logs `built-in runtime registered` when a worker registers it. It
  warns when a worker of its product and Collector version reports another
  registry, since that worker serves no built-in runtime.

## Instances

- Instances share PostgreSQL and the object store and need no sticky sessions.
  Each catches up from a change feed in PostgreSQL before every operation.
  Run events and interactive requests go through PostgreSQL. An OIDC sign-in
  begun on one instance may finish on another, and web sessions live in
  PostgreSQL.
- One instance, the leader, does housekeeping and run retention: the store
  sweep and trimming of the change feed, old tombstones and finished jobs. It
  leads until it stops or loses its database connection; another takes over
  within 5 minutes.
- MCP is stateless: every `POST /mcp` stands alone, `GET` and `DELETE` answer
  405.
- On SIGTERM (or Ctrl-C) `linea-server --config` drains. It stops claiming runs and
  jobs, waits up to `--drain-timeout` (default 25 s) for the runs on its
  workers, hands the rest back to the queue without spending an attempt, then
  closes. When time runs out it prints "drain: work still running after 25s is
  handed back to the queue". The process grace period must exceed the timeout;
  the chart uses `server.drainSeconds` plus 20 s.
- A killed instance loses its runs' 30 s leases; another instance requeues
  them once, as a new attempt.
- `--metrics-listen ADDR` serves the build queue's counts at `GET /metrics` on
  a listener of its own, without credentials, for a builder autoscaler.
- The connected CLI retries a report wait (`run`, `test`, suites,
  `runs report --wait`, compare waits) that gets a lost connection or a 502,
  503 or 504 without an operation error code, such as a load balancer's page,
  for up to 30 s of such failures, so it reaches another instance.

### Verification

`scripts/debugger/fanout-compose.sh` is the gate of the shared platform. It
starts the Compose stack as a disposable project (two instances and
`FANOUT_POOLS` pools of capacity 4), registers nothing, waits until a pool has
registered the built-in runtime and it is the only one, then sends
`FANOUT_RUNS` runs that name no runtime through `FANOUT_CLIENTS` concurrent
`linea test --server`. It restarts one instance gracefully once half of the
runs are submitted and fails unless every run passes. It removes the project,
volumes and its `.env` on exit. Run it before a release
([development.md](development.md#before-a-release)).

| Variable | Default |
| --- | --- |
| `FANOUT_RUNS` | 200 |
| `FANOUT_CLIENTS` | 8 |
| `FANOUT_POOLS` | 4 |
| `FANOUT_PORT` | 7079 |
| `FANOUT_SAMPLE` | `logs-cleanup` |

The edge network is fixed at `10.207.70.0/24`, so stop a running local stack
before running it. The script starts only `proxy` and `pool`.

## Fleet attribution

On every fleet call made for a signed-in principal, the team server also sends
who it acts for: the principal ID, display name and request ID as fleet's
`X-Fleet-Acting-ID`, `X-Fleet-Acting-Name` and `X-Fleet-Acting-Request`
headers.

- Fleet records them as `acting_for` on the events the call writes, and the
  livecapture plugin on the capture.
- They are attribution only: fleet still authorizes the debugger's token and
  decides nothing on the acting user.
- The debugger's audit log stays authoritative; fleet's events show the same
  request ID.
- The debugger's fleet event feed (`GET /api/v1/fleet/events`,
  `linea_list_fleet_events`) returns `acting_for` and the web client shows the
  acting user in place of the credential.
- Background work (capture delivery, data reads) sends no acting headers.
- The `capture.raw` check is the debugger's own. The plugin's `raw_principals`
  decides whether the debugger's credential may capture raw points at all.

## Sign-in details

- `/auth/login` starts the authorization code flow with PKCE; the provider
  redirects back to `/auth/callback`. The server keeps the session in its
  database and the browser holds a random cookie, so `POST /auth/logout` ends
  it at once.
- Requests that change something also send the session's CSRF token from
  `GET /api/v1/session` as `X-CSRF-Token`.
- `GET /api/v1/session` lists the permissions the principal holds,
  platform-wide and in at least one project. The web client uses the list to
  shape what it offers. The server still checks every request.
- `/auth/login` exists only with `oidc`. Without `oidc` or trusted headers the
  sign-in button of the 401 page leads to a 404.
- With `oidc`, bearer tokens (ID or access JWTs) are verified with the
  provider's keys and accepted only for the configured audiences.
- Refusals are logged as "authentication refused" with the response's
  `X-Request-Id`, the client address, the interface and the reason; the
  credential is never logged.
- API tokens: the server keeps only the secret's SHA-256. `create` prints the
  secret once on stdout and the token record on stderr.
