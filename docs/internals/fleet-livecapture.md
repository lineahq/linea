# Livecapture plugin internals

How the livecapture plugin authenticates peers, fences work and reports
metrics. The operator and API guide is [livecapture](../fleet/livecapture.md);
the generic plugin mechanisms are in [fleet-plugin-host.md](fleet-plugin-host.md).

## Authority and instances

- The plugin supervises its own active and standby instances and drains on
  request.
- One active authority per database. The database holds a stable authority
  ID, and an exclusive authority lock prevents an offline purge while a
  runtime owns it.
- A durable authority identity plus a new process instance fences stale
  requests, even if a restore rolls the generation back.
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
  interval.
- While core is unreachable, new streams are refused and capture stops with
  host authority.
- Peer links resolve at most 64 per 10 s pass, through `agents/resolve` with
  the introspected `credential_id` and the identity attributes. Only a
  unique match is linked.

## Control stream

- Each stream gets the complete desired state first, on every change and
  again every 3 s when nothing changed. The Collector ends a remote session
  as `control_lost` after 10 s without one, so a healthy stream never lets
  it expire.
- A change pings the database before sending. The 3 s repeat does not; it
  reads the database flag the desired state already checks.

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
- The runtime role cannot run DDL or delete operation history. Offline
  purge removes only operations past absolute expiry plus the guard and
  eligible expired leases, and keeps generation counters.

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
