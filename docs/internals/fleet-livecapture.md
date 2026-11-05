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

## Delivery and reservations

- Before forwarding, the plugin validates the current credential, session,
  peer generation, pipeline, point, signal and expiry.
- A Collector's reservation releases only after its terminal ACK, or the
  deadline plus a five-second guard. Uncertainty therefore cannot admit
  overlapping captures.
- Payloads exist only in bounded memory and are never logged. The database
  keeps metadata only: owners, hashes, reservations, generations and the
  authority ID.
- The runtime role cannot run DDL or delete operation history. Offline
  purge removes only operations past absolute expiry plus the guard and
  eligible expired leases, and keeps generation counters.

## Metrics snapshots

- The private Prometheus route stays available when core host authority is
  unavailable. The public JSON route still follows normal lifecycle and
  readiness gates.
- Neither collector queries storage, reads payloads or catalogs, waits for an
  export receipt or creates execution.
- If admission holds the metadata lock, the private scrape uses its last
  immutable aggregate snapshot. `metadata_snapshot_available`,
  `metadata_snapshot_stale` and `metadata_snapshot_age_seconds` tell cached
  metadata from current observations. Before the first successful snapshot,
  the metadata gauges are absent and `available` is zero.
- Atomic delivery and storage counters are always current.

Tests for the plugin are listed in [fleet
verification](fleet-verification.md#livecapture-plugin).
