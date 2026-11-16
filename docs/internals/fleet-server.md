# Fleet server internals

How the fleet server keeps sessions, writes inventory and survives replica
and database loss. Operators need only the outcomes, which are in
[deployment](../fleet/deployment.md) and
[operational metrics](../fleet/operations-metrics.md). The config layers the
server sends are in [fleet-config-layers.md](fleet-config-layers.md).

## Replicas and presence

- Fleet servers are identical replicas that may share one database. Each
  replica registers itself, owns the sessions it terminates and heartbeats
  every 2 s.
- Presence is derived from the owning replica's heartbeat. Once a replica's
  heartbeat is 10 s old, its agents read as disconnected, with health and
  observation times preserved. The leader replica releases their ownership
  and writes `agent.disconnected` events.
- Connectivity, health and telemetry delivery are separate facts. An omitted
  health keeps its previous observation time. Removing a capability replaces
  the old capability bitmask.
- A second connection with an active UID and the same credential takes over
  the session and advances `generation`. Three takeovers within a minute flag
  `identity_conflict` (two processes sharing one UID and credential).
- The UID is bound to the credential that holds it while it is connected.
  `connect` (`internal/fleet/postgres/writer.go`) reads the stored credential
  under the row lock and returns `fleet.ErrUIDHeld` for another credential
  while the owner replica is set and alive; nothing in the row changes. The
  batch writer then records `agent.credential_refused` (subject the agent,
  actor the refused credential) outside the rolled back savepoint, at most
  once a minute per agent and credential on each replica. The gateway answers
  with an OpAMP `ServerErrorResponse` of type `BadRequest` and closes the
  socket. A disconnected or orphaned UID changes hands and writes
  `agent.credential_changed` with `{"previous_credential":"<id>"}` in the
  same transaction. The memory store applies the same rule without events.
- Graceful stop withdraws readiness, closes agent sockets evenly over
  `--drain-timeout`, so Collectors do not reconnect at once, and records
  disconnected presence.
- A replica that loses the database or its own registration fails readiness
  closed and stops within the heartbeat window. In the HA overlay it restarts,
  fences itself after an outage longer than the heartbeat window and returns
  under a new replica ID. It does not reclaim agents another replica took.

## Write path and admission

| Part | Mechanism |
| --- | --- |
| Pool | At most eight connections per replica, one held by the LISTEN session |
| Writer | A batched writer commits session changes in short transactions |
| Heartbeats | Unchanged heartbeats cause no writes. One bulk `last_seen` update per replica every 10 s |
| Backpressure | The writer is busy above 75 % of its queue (4,096) or when the queued work would take more than 500 ms at the last batch's speed; new sessions are then answered 503 with `Retry-After` |
| Handshakes | `--max-pending-sessions` (default 400) bounds handshakes between admission and first commit, before authentication, so admission follows the database's actual speed. It keeps up to two writer batches in flight per replica. A handshake waits up to 25 s for a slot, so opamp-go clients do not fall into long exponential backoff |
| Event bound | `--max-events` (default 100,000) must cover at least two events per agent so a full reconnect storm does not expire cursors |
| Pooling | Direct PostgreSQL or a session pooler. Transaction pooling is unsupported: the server relies on LISTEN/NOTIFY |
| Mutations | An admitted mutation finishes its short transaction even if the HTTP caller disconnects |

## Database roles and migrations

- The migrator role owns the schema. The runtime role gets schema usage,
  ledger reads, table-specific DML and event-sequence usage. It cannot
  create or alter tables, update the ledger, or create roles or databases.
  The runtime never runs DDL. Bootstrap uses the runtime connection.
- Roles are initialized separately and are not in a single-database dump.
- The migrator re-applies the runtime role's grants on every run, and
  verifies every installed checksum without DDL when repeated. Newer or
  unknown versions, missing ledger entries and modified migration content are
  rejected. There is no down-migration.
- The config management tables exist in every installation (durable schema
  version 12; version 12 adds the `operator` credential role and version 11
  made admin and reader credential expiry optional).

## Restore and failover

- A restore into another cluster is detected at startup (new cluster
  identity). Event cursors expire once (410) and agents must report again.
- Database failover keeps the cluster's system identifier, so event cursors
  stay valid. Each host lookup in the multi-host URL is bounded to 1 s, so a
  stopped primary whose name no longer resolves does not starve the connect
  timeout before the standby is tried. Disconnects whose write failed during
  the switch are retried.

## High-availability sizing notes

- HAProxy sizes its threads from the host's CPUs, not the container's cgroup
  quota. On a 16-CPU host it would run 16 threads on one CPU of quota, which
  is why `nbthread` in `haproxy.cfg` must equal the container's `cpus`.
- Sessions cost about 33 KiB each, so the 1 GiB replica limit covers 10,000
  sessions with margin. The per-agent budget checked with
  `go_memory_bytes / agent_connections` is 64 KiB.
- The multi-replica topology was tested with protocol clients on three
  replicas up to 20,000 sessions; real Collectors at that scale were not.

## Metrics notes

- Core metric collectors read local statistics only; the inventory summary in
  the scrape still does a bounded database read.
- Plugin-state metrics avoid waiting behind lifecycle persistence, and report
  that entry as unknown instead.

## Transition log

`internal/fleet/translog` is the only way the server writes about itself
(`docs/fleet/deployment.md` lists the lines). Its API is closed so that free
text cannot reach a line: events, reasons, states, listeners and field names
are `uint8` enumerations whose names exist only in the package, so a string
cannot be converted into one; the only strings are IDs and versions, checked
against a character set and a length and replaced by `invalid` otherwise. No
function takes an error, a message or a map. A nil `*Logger` discards, so
components work unwired; the logger reaches them by `SetLogger` (gateway, API
handler, plugin registry), `postgres.Options.Log` and `app.Config.Log`, not by
a global.

- Transitions are logged after the database committed them, never from inside
  a transaction that may roll back.
- Failed authentications only increment a counter per listener;
  `Summarize` flushes one `auth_failures` line per listener every 10 s from
  injected ticks, and once more at stop. `Limited` bounds an event that can
  repeat per agent (`config_withheld`) to 20 lines per window and reports the
  rest as `lines_suppressed`.
- `structure_test.go` parses `internal/fleet` and `cmd/linea-fleet` and fails
  on any import of `log` or `log/slog`, `fmt.Print*`, a `fmt.Fprint*` to a
  stderr value, `os.Stderr` or `os.Stdout` outside `main`, more than one
  write to `os.Stderr`, a conversion to a translog enumeration, and any log
  call whose event or fields are not built by translog constants and
  constructors.
- The one write to stderr outside the log is the final error line in
  `cmd/linea-fleet/main.go`. It prints usage, secret and validation errors,
  which this repository authors; a failure of the running service is mapped
  to a fixed sentence there, and its reason is in `process_stopped`.
- The plugin has its own helper in `internal/fleetplugins/livecapture/translog`
  (it may not import `internal/fleet`). Both write `time`, `level`, `msg`
  (the event name), `component`, `reason`, `count` and `<name>_ms` in the same
  shape.
