# Livecapture plugin internals

How the livecapture plugin authenticates peers, fences work and reports
metrics. The operator and API guide is [livecapture](../fleet/livecapture.md);
the generic plugin mechanisms are in [fleet-plugin-host.md](fleet-plugin-host.md).

## Authority and instances

- The plugin supervises its own active and standby instances and drains on
  request.
- One active authority per database. The database holds a stable authority
  ID, and an exclusive authority lock keeps a second runtime and a migration
  out while a runtime owns it. The operation purge does not take that lock
  (see Online purge).
- A durable authority identity plus a new process instance fences stale
  requests, even if a restore rolls the generation back.
- Database liveness is decided only by `maintain`, which pings once a second
  and keeps the flag every other path reads (`dbOK`). The ping waits for the
  working connection for at most 1 s, and that wait is not a sample: a
  connection occupied by other statements gives no result, neither a success
  nor a failure. Only the ping itself, with 2 s from the moment it owns the
  connection, can fail. The process exits after three failed pings in a row,
  or at once when the driver reports the connection closed (it never
  reconnects, so a later ping cannot succeed). A failed `Save` of an
  operation still ends the process. After an exit, Compose restarts the
  plugin (`restart: unless-stopped`).
- Startup after a restore records previous live operations as interrupted,
  retains uncertain leases, reconnects peers with empty desired state and
  never replays capture.

## Credential checks

- The plugin hashes the bearer token and introspects the SHA-256 through
  core. It caches the answer for at most 10 s (rejections 2 s, never past
  `expires_at`; at most 1,024 hashes) and clears the cache when the epoch
  changes.
- It re-introspects open connections every 10 s. Revoking, expiring or
  relabeling a credential in core closes affected streams within that
  interval. The loop keeps a deadline (10 s after the last recheck started), so
  link wakeups and early timers neither skip a round nor delay the next.
- While core is unreachable, new streams are refused and capture stops with
  host authority.
- Peer links resolve at most 64 per pass, through `agents/resolve` with
  the introspected `credential_id` and the identity attributes. Only a
  unique match is linked. Each peer keeps its last attempt and next due
  time. A pass takes the due peers with the oldest last attempt (never-tried
  first), and every outcome, a host error included, moves both, so the
  same peers cannot win every pass. After an attempt that did not link, the
  peer waits 2 s, doubling to 60 s; a linked peer refreshes after 60 s.
  The loop sleeps until the earliest due peer or the next credential
  recheck, but at least 1 s between passes. `link.state` on the peer
  (`pending`, `linked`, `no_match`, `ambiguous`) is the last answer; a host
  error does not change it.

## Control stream

- Each stream gets the complete desired state first, on every change and
  again every 3 s when nothing changed. The Collector ends a remote session
  as `control_lost` after 10 s without one, so a healthy stream never lets
  it expire.
- Neither a change nor the 3 s repeat pings the database: both read the
  database flag the desired state checks. Plugin API requests and OTLP
  deliveries read it too.

## Delivery and reservations

- Before forwarding, the plugin validates the current credential, session,
  peer generation, pipeline, point, signal and expiry.
- The plugin answers each valid export at once and never makes the Collector
  wait for the consumer. It appends the batch to the channel's buffer (2 MiB
  and 64 batches per channel, 32 MiB in all) or, when it does not fit or the
  consumer is gone, drops it. A batch still unacknowledged after 10 s is
  dropped when the consumer next reads or a batch arrives. Every drop counts
  on the target's `delivery` and in the drop metrics; closing a channel
  drops and counts what it still holds.
- `next` offers the oldest pending batch until it is acknowledged; an ACK for
  a batch that is not pending is a no-op, so the plugin keeps no receipt
  history.
- A Collector's reservation releases only after its terminal ACK, or the
  deadline plus a five-second guard. Uncertainty therefore cannot admit
  overlapping captures.
- Payloads exist only in bounded memory and are never logged. The database
  keeps metadata only: owners, hashes, reservations, generations and the
  authority ID.
- The runtime role cannot run DDL or delete operation history. The purge
  (migration role) removes terminal operations past absolute expiry plus the
  guard that no lease refers to, and expired leases, and keeps peers and
  generation counters.

## Online purge

`Purge` runs while the plugin owns the database, so the 10,000-operation cap
is not an outage: `dial` opens a session without `pg_try_advisory_lock`, checks
the migration ledger and deletes in one transaction. The guards that make it
safe against a running authority:

- The plugin saves an operation row only while the operation is not terminal,
  or while one of its targets still holds a lease (`Save` releases a lease in
  the same transaction that records the target as released). The purge
  deletes only terminal operations that no lease refers to, so it never
  removes a row the plugin will save again; a missing row would make `Save`
  fail with `ErrPrecondition`, which is fatal. The maintenance loop also ends
  and forgets every operation at its absolute expiry, so none is saved once
  the 5 s guard has passed.
- The expiry guard stays: expiry plus 5 s must not be after `--before`, and
  `--before` may not be in the future. Leases are deleted only after their own
  expiry, which is the rule `Insert` already applies.
- Peers are not touched, so the purge cannot race the peer retention below.
  The purge needs `DELETE` on operations and leases, which only the migration
  role has.
- `Retry-After`. No 429 the plugin sends carries one. A full operation table
  clears only by purge; full operation or channel slots clear when captures
  end or expire, which the plugin cannot promise by a time; fleet forwards
  only its own `Retry-After: 1` and drops any other value from a plugin
  (`internal/fleet/plugins/api.go`). A value of 1 would be a promise the
  plugin cannot keep.

## Peer retention

`capture.peers` holds one row per peer ID (a hash of credential, cluster,
namespace, pod and group) and caps at 10,000 rows. Without retention every
new pod name would be a permanent row and the cap would end the feature for
everyone.

- `maintain` runs `Repository.MaintainPeers` once a minute (again on the next
  tick when a pass deleted a full batch of 500). One call, through the store's
  single connection, first sets `seen_at=now()` for the peers connected in
  this process, then deletes up to 500 peers that are idle for more than 24 h
  (`peerRetention`), not connected, hold no lease and appear in no
  operation's `targets`.
- `seen_at` is also set when a peer connects (`NextGeneration`). A peer
  that connects between the plugin's snapshot of connected peers and the
  delete has a fresh `seen_at`, so it is kept. Plugin calls to the database
  are serialized, so no other writer in the process races the delete.
- Generations. A row's generation increments on every connect, so while the
  row exists its numbers never repeat. A delete would restart a returning peer
  at 1, so the same statement raises `metadata.generation_floor` to the
  highest deleted generation, and a new row starts at `generation_floor+1`.
  Every generation issued after a delete is therefore above every generation
  that was ever issued for a deleted peer, even though nothing durable can
  still refer to one (no lease, no retained operation by the delete rule).
- A failed pass is counted as a storage error, does not end the plugin and is
  retried; only the liveness ping is fatal.
- Migration 004 adds `peers.seen_at` (existing rows start at the migration
  time, so they are kept for at least 24 h), `peers_seen_idx` and
  `metadata.generation_floor`. The runtime role gets `DELETE` on
  `capture.peers` and `UPDATE (generation_floor)` on `capture.metadata`
  (`runtimeGrants` in `store.go`); the grants are applied by `migrate`, not by
  the checksummed migration files.
- Peers named by a retained operation stay until the operation purge removes
  the operation; the operation cap of 10,000 is separate.

## Transition log

The plugin's own `translog` package (`internal/fleetplugins/livecapture/translog`,
standard library only; it is a copy apart from fleet core's and is not in
`pluginsdk`) is the only place that writes a log line. The events and fields
are in the operator guide, [Transition log](../fleet/livecapture.md#transition-log).

- Closed by construction. An event is an `Event` constant and an attribute
  comes from a typed constructor: IDs (`Operation`, `Peer`, `Credential`,
  `Agent`, `Authority`, `Instance`, `Epoch`, each refused unless 1 to 128
  characters of letters, digits and `._-`, otherwise logged as `invalid`),
  enums (`Why` and `Cause` take a `Reason`, `From` and `To` a `State`,
  `LinkState` a `Link`, `OnListener` a `Listener`), `Count`, `Elapsed` and
  `Version`. No function takes an error, a free message, a map or an
  interface. A string from outside an enum maps through a closed lookup
  (`ParseReason`, `ParseState`, `ParseLink`) that answers `unknown`; the stored
  operation reason and state go through it.
- Causes without text. `causeError` (`cause.go`) carries a `Reason` beside an
  error and unwraps to it, so `errors.Is` still sees the sentinel. `WithCause`
  attaches one and `CauseOf` reads it, else maps the sentinels, else `failed`;
  it never reads error text. `Server.fatalLocked(cause)` is the only way to end
  the plugin, so every fatal exit has a cause, and `Run` returns the error it
  sent.
- Where the lines come from. `checkHost` (called by `monitorHost`) logs the
  host session after it releases the lock; `observeStorageLocked` the degraded
  and recovered database and `maintain` its loss; `saveLocked` every operation
  transition once it has saved (the last logged state is
  `activeOperation.logged`; the two paths that update the database directly, in
  `Get` and `Stop`, log themselves); `Watch` the peer connect and disconnect;
  `recordLink` link changes; `Run` the authority; `main` the process, standby
  and command failures. A peer the plugin closes records why
  (`peer.closeReason`, set once by `retirePeerLocked`).
- Authentication failures. `FailureSummary` counts rejections per listener with
  an atomic increment. `maintain` calls `Tick` with its ticker's time every
  250 ms, and a window of 10 s writes one line per listener with a count;
  `Server.Close` flushes the open window. The clock is the caller's, so tests
  pass times instead of sleeping.
- Never waits. `New` puts the JSON handler behind a 256-line queue and one
  writer goroutine. A full queue drops the line (`Logger.Dropped`) instead of
  blocking, because lines are written under the server lock and a stuck stderr
  must not stall a Collector. `main` closes the logger with a 2 s bound so the
  last lines get out.
- Structural test. `translog/audit_test.go` parses the plugin tree and
  `cmd/linea-fleet-livecapture`: no `log` or `slog` import, no `os.Stderr`
  except as the writer of `translog.New`, no print builtin; every `.Log` call
  names a `translog.Event*` constant and only `translog` constructor
  attributes; nothing converts into the helper's types; the helper's exported
  functions take no error, map or interface, and a string only where it is
  checked. A second test feeds it code that breaks each rule, to prove it
  fails.

## Metrics snapshots

- The private Prometheus route stays available when core host authority is
  unavailable. The public JSON route still follows normal lifecycle and
  readiness gates.
- Neither collector queries storage, reads payloads or catalogs, waits for a
  consumer or creates execution.
- If admission holds the metadata lock, the private scrape uses its last
  immutable aggregate snapshot. `metadata_snapshot_available`,
  `metadata_snapshot_stale` and `metadata_snapshot_age_seconds` tell cached
  metadata from current observations. Before the first successful snapshot,
  the metadata gauges are absent and `available` is zero.
- Atomic delivery and storage counters are always current.

Tests for the plugin are listed in [fleet
verification](fleet-verification.md#livecapture-plugin).
