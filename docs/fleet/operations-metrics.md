# Operational metrics and capacity limits

Core and the livecapture plugin expose Prometheus metrics. A reproducible
capacity harness lives in `tools/fleet-capacity/`.

- Each replica exposes its own series.
- Capacity beyond the measured runs is not claimed.
- Keep metrics credentials and endpoints on protected transport, following
  the deployment's TLS and ingress policy.

## Core metrics

`GET /metrics` uses a fleet reader or admin bearer and Prometheus text format.
No agent or plugin credential grants it. Series use only fixed
listener, state, read-write and bucket dimensions: never agent IDs, plugin
IDs, principals, paths, SQL, errors, secrets or payloads.

All names start with `linea_fleet_`.

### Requests and inventory

| Metric | Meaning |
| --- | --- |
| `http_requests_total`, `http_errors_total` | Management requests, and responses with status 400 or above |
| `http_duration_seconds` | All management HTTP handling, including probes, auth failures and scrapes |
| `agents{presence}` | Inventory count by connected, disconnected or unknown presence |
| `report_duration_seconds`, `report_errors_total` | OpAMP protobuf processing, including session lock and storage. Errors include rejected publication |
| `agents_expired_total` | Disconnected agents this replica deleted by retention or capacity eviction |
| `attributes_dropped_total`, `config_summaries_skipped_total` | Description attributes outside the allowlist or bounds, and effective configurations too large to summarize (only hashed) |

### Connections and admission

| Metric | Meaning |
| --- | --- |
| `agent_connections`, `agent_connection_limit` | Current authenticated sockets, and the configured ceiling |
| `agent_admission_refused_total` | Refused gateway admissions: connection ceiling, no handshake slot within 25 s, or writer backpressure |
| `agent_admission_held_total` | Handshakes that waited for a slot |
| `agent_handshakes_pending`, `agent_handshake_limit` | Handshakes between admission and first commit, and `--max-pending-sessions`. Pending at the limit during a storm means admission is the bound |
| `tcp_connections`, `tcp_connection_limit`, `tcp_refused_total` | Agent and management TCP budgets, including incomplete handshakes |
| `agent_takeovers_total`, `agents_reaped_total` | Sessions this replica took over from another connection, and agents the leader released from crashed replicas |
| `replica_leader`, `replica_heartbeat_age_seconds` | 1 while this replica holds the leader lease, and the age of its last successful heartbeat |

### Memory

| Metric | Meaning |
| --- | --- |
| `go_heap_objects_bytes`, `go_memory_bytes`, `goroutines` | Live heap objects, all Go-managed memory and goroutines of the replica. Divide by `agent_connections` to check the 64 KiB per-agent budget |
| `go_heap_live_bytes`, `go_stack_bytes` | Heap marked live by the last GC, and goroutine stacks. Heap objects also include garbage awaiting the next GC; these two are the steady cost of the sessions held |

### Database

| Metric | Meaning |
| --- | --- |
| `database_query_duration_seconds`, `database_query_errors_total` | Local pgx query, row and exec duration and errors. No query text is collected |
| `database_write_duration_seconds`, `database_write_errors_total`, `database_flush_errors_total` | Write transactions (writer batches and administration), their failures, and failed bulk `last_seen` flushes |
| `writer_queue`, `writer_queue_limit` | Session writer backlog and its ceiling (4,096) |
| `writer_batches_total`, `writer_batch_items_total` | Items per batch is `batch_items / batches` |
| `database_connections`, `database_connection_limit` | Acquired and idle pool connections; at most eight per replica |
| `database_pool_wait_seconds_total`, `database_pool_waits_total`, `database_pool_canceled_total` | Driver statistics for contended pool acquisition and cancellation |

### Config management

Present only with `--config-management`; see the [operator
guide](config-management.md#monitor).

| Metric | Meaning |
| --- | --- |
| `config_sends_total{path=response\|async}` | Configs sent in replies to reports, or pushed after a change (at most 200/s per replica) |
| `config_holds_total`, `config_incompatible_total` | Failure holds set, and sends withheld because the agent's build lacks a component type |
| `config_component_sets_requested_total`, `config_component_sets_stored_total` | Builds whose component list was asked for, and lists stored |
| `config_reconcile_errors_total`, `config_storage_errors_total`, `rollout_controller_errors_total` | Send decisions or hold writes that failed (nothing is sent then), and failed rollout controller ticks on the leader |
| `config_body_cache_bytes` | Config version bodies cached by the replica (64 MiB bound) |

### Plugins

| Metric | Meaning |
| --- | --- |
| `plugins{state=...}` | Fresh counts of ready, unavailable, incompatible, disabled and draining plugins. Unknown when lifecycle metadata is contended |
| `plugin_api_inflight`, `plugin_api_refused_total`, `plugin_api_errors_total`, `plugin_api_duration_seconds` | Opaque plugin ingress work, saturation, request and upstream errors, and duration |

### Reading the series

- Duration series are histograms with cumulative buckets at 1, 5, 10, 25, 50,
  100, 250 and 500 ms, 1, 2, 5 and 10 s, and +Inf, plus `_sum` and `_count`.
- Counters and histograms reset on process restart.
- Snapshot families are independently consistent. A scrape is not a
  transaction across every counter.
- Query time includes row consumption. Transaction time overlaps queries: do
  not add them.
- Database collectors read local statistics only. The inventory summary in
  core's scrape still uses a bounded database read.
- Watch latency distributions together with refused admissions, errors,
  waiters and current occupancy. A ready process or an open connection alone
  does not prove fresh reports or telemetry delivery.

## Livecapture metrics

The plugin publishes two forms of one aggregate snapshot.

| Form | Access | Notes |
| --- | --- | --- |
| Public `GET /api/v1/plugins/livecapture/api/v1/metrics` | Fleet reader or admin | JSON with `gauges`, `counters`, `durations` and `limits`. Needs no capture-owner privilege and exposes no owner or source IDs |
| Private `GET http://livecapture-plugin:8090/metrics` | Separate `metrics_token` bearer on the protected plugin network | Prometheus text. The token grants only this route. Host, service, management and agent credentials do not grant scrapes. Without `metrics_token_file` the route refuses access |

Prometheus names start `linea_fleet_livecapture_`. The JSON uses the
suffixes. Limits are `limit_*` gauges in Prometheus and the JSON `limits`
object.

| Family | Covers |
| --- | --- |
| Gauges | Peers, channels, current in-memory operations by state, host and storage readiness, draining state, pending bytes and batches, retained receipts, inflight deliveries, connections, storage waiters. Operation gauges are not historical database totals |
| Counters | Admitted, refused and withdrawn captures; delivery errors and capacity; uncertain receipts; first and duplicate ACKs; accepted and rejected records by signal; TCP refusals; storage errors; peers closed because their fleet credential was revoked, expired or no longer allows their group (`peers_credential_closed_total`) |
| Durations | Delivery (authentication, queueing and consumer receipt wait) and storage operations (excluding their separately measured gate wait) |
| Snapshot freshness | `metadata_snapshot_available`, `metadata_snapshot_stale`, `metadata_snapshot_age_seconds` |

- Identical ACK retries do not count accepted records twice.
- Storage errors include semantic refusals as well as unavailability.
- Core does not aggregate or interpret these metrics. Scrape the plugin
  separately, or read its JSON snapshot through the API.
- Mechanics (snapshot caching, lock behavior) are in [livecapture
  internals](../internals/fleet-livecapture.md#metrics-snapshots).

## Effective ceilings and overload

| Ceiling | Behavior |
| --- | --- |
| Agents | Core keeps its configured agent and inventory limits. TCP agent capacity includes unfinished handshakes. Overload before HTTP authentication closes the transport |
| Management TCP | Separate 128-connection budget |
| New agent sessions | Admitted before authentication while fewer than `--max-pending-sessions` (default 400) handshakes are between admission and first commit. A handshake waits up to 25 s for a slot (`agent_admission_held_total`); beyond that, or while the writer queue is full, it is refused with 503 and `Retry-After` |
| PostgreSQL | At most eight pool connections per replica, one held by the LISTEN session. Capture uses a different database |
| Plugin ingress | Two read and two write slots per registration, independent of manifest and status polling. Saturation returns 429 with `Retry-After: 1`; an unready or failed authority returns 503. A plugin's bounded `Retry-After: 1` on 429 is preserved |
| Plugin `max_peers` | Default 256, range 1 to 4,096, restart required. The control TCP listener allows that plus 16 temporary handshake slots; authenticated peer admission is still limited to `max_peers`. Duplicate live identities are refused. This replaces the old 64-connection control bottleneck |
| Plugin data and HTTP | Separate 64-connection budgets each |
| Excess TCP peers | Closed promptly. Authenticated gRPC capacity failures use `ResourceExhausted` |

- Use bounded retries with jitter and keep operation IDs. A timeout is not
  proof that a mutation was never admitted.
- Raising one ceiling does not raise delivery, consumer or storage budgets,
  and does not show the machine can sustain it. The [livecapture
  runbook](livecapture.md#bounds-and-operating-limits) lists the other fixed
  limits.

## Related

- [Capacity harness](../../tools/fleet-capacity/README.md): execution modes,
  resource budgets, output interpretation and the dedicated-host profiles.
- [OpAMP simulator](../../tools/fleet-capacity/simulator.md): exercises
  hundreds of connections in one bounded generator and records client
  response latency and generator pacing separately from server processing
  duration.
- Peer paging: [Peer discovery paging](livecapture.md#peer-discovery-paging).
  Core and plugin upgrade commands: [Rotation, backup, restore and
  upgrade](livecapture.md#rotation-backup-restore-and-upgrade).
