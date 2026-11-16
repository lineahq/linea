# Docker installation and operation

Deploy the fleet server with Docker Compose v2 and Linux containers. No host
Go, debugger, extension or frontend is needed.

- PostgreSQL 17, Go build and distroless non-root images are pinned by
  digest. Tested on Docker Desktop Linux/amd64.
- Ports, volumes and secrets belong to the selected Compose project.
- Base Compose is plugin-free and works alone. Plugins use separate
  registrations and credentials ([plugins](plugins.md)); the [livecapture
  plugin](livecapture.md) adds its own module, image, database and Compose
  override. Plugin readiness does not change core readiness.
- Fleet servers are identical replicas that may share one database. The base
  file runs one; [`compose.ha.yaml`](#high-availability) adds more behind a
  TCP load balancer and a PostgreSQL streaming standby.

## First installation

- Shell commands run on Linux, macOS and Git Bash. On Windows, use PowerShell
  7 (`pwsh`) for the PowerShell commands: the secret factories need
  `$IsWindows` and `[Convert]::ToHexString`, which Windows PowerShell 5.1
  lacks. Docker commands do not depend on it.
- Run the Compose commands from `deploy/compose/fleet`. Script and secret
  paths are relative to it.
- The fleet image is `ghcr.io/lineahq/linea-fleet:${FLEET_VERSION}`, by
  default this checkout's release. To build from the checkout, set
  `FLEET_VERSION=dev` on every command and run `docker compose build fleet`
  instead of `pull`.
- No env file is needed: every variable has a default. `compose.yaml` fixes
  the project name `linea-fleet`, so `--project-name linea-fleet` changes
  nothing. Pass `--env-file` (and the same project name) only once you set
  variables.

Linux, macOS and Git Bash:

```sh
sh ../../../scripts/fleet/new-deployment-secrets.sh
docker compose pull fleet           # or, with FLEET_VERSION=dev: docker compose build fleet
docker compose up -d --wait postgres
docker compose run --rm -T migrate
umask 077
docker compose run --rm -T bootstrap > ../../../.local/deployment/admin.json
docker compose up -d --wait fleet
```

Windows, PowerShell 7:

```powershell
..\..\..\scripts\fleet\new-deployment-secrets.ps1
docker compose pull fleet
docker compose up -d --wait postgres
docker compose run --rm -T migrate
$fleetBootstrap = docker compose run --rm -T bootstrap | ConvertFrom-Json
$fleetBootstrap | ConvertTo-Json -Depth 4 | Set-Content ../../../.local/deployment/admin.json
docker compose up -d --wait fleet
```

Secrets:

| Topic | Detail |
| --- | --- |
| Factory | The shell version needs OpenSSL; PowerShell uses .NET. Both create random database passwords and connection files in an ignored private directory and refuse to overwrite it |
| Permissions | Windows restricts the directory ACL to owner and SYSTEM. Unix uses a 0700 parent and readable individual mounts for container UID 65532. Git Bash on Windows cannot restrict the ACL with `umask` or `chmod`: prefer PowerShell 7, or restrict the directory yourself |
| Protection | File-based Compose secrets are plaintext host files. Protect the directory and its backups; never commit or print them |
| Admin token | Valid until revoked; it does not expire. `admin.json` is an operator copy of the one-time disclosure: store it in your secret manager |
| Admin expiry | Startup requires an active admin. If all admins are revoked or expired, stop fleet and run offline bootstrap again. Bootstrap refuses while an active admin exists. Admins issued before schema version 11 keep their 90-day expiry |
| Deployment token | With `LINEA_FLEET_BOOTSTRAP_TOKEN` (or `_FILE`), bootstrap installs a token the deployment generated (`of1_`, 32 and then 64 lowercase hexadecimal characters, joined by `_`) instead of generating one, and prints only `fleet admin created` or `fleet admin is current`. Repeating it changes nothing but the plugin grants while that admin is active; it refuses when another admin is active or that token's credential was revoked |
| Deployment operator token | With `LINEA_FLEET_OPERATOR_TOKEN` (or `_FILE`) as well, bootstrap installs that token as an operator credential, named by `--operator-name` (default `linea-server`), after the admin, and prints only `fleet operator created` or `fleet operator is current`. The operator credential does not expire. Repeating it changes nothing but the grants below while that operator is active; it refuses when the token's credential has another role or token, was revoked or has expired. The debugger's Compose overlay and Helm chart keep the admin token for whoever administers fleet and give `linea-server` the operator token |
| Bootstrap grants | `--plugin-access plugin=level[,...]` grants the first admin plugin access (`read`, `write` or `admin`); no role reaches a plugin without one. In the fleet Compose file, set `FLEET_BOOTSTRAP_PLUGIN_ACCESS`, for example `livecapture=admin`. `--operator-plugin-access` and `--operator-delegate` grant the deployment operator plugin access and the acting-for headers. With deployment tokens, a repeat sets the grants to what the flags say and records `credential.updated`. The debugger's Compose overlay and Helm chart grant the admin `livecapture=admin` and `linea-server` `livecapture=write` with delegate |
| Rotation | Changing secret files does not rotate passwords in an existing database. Coordinate database password changes with connection-file updates |

Storage and roles:

- The named `fleet-data` volume holds durable state. Keep it with the secret
  directory. `docker compose down` preserves named volumes.
- Migrations are explicit and offline. The migrator owns the schema. The
  runtime role cannot run DDL, update the migration ledger, or create roles
  or databases. Bootstrap uses the runtime connection.

### Secrets outside the checkout

The factories write to `.local/deployment` by default. To keep secrets
elsewhere:

1. Pass a directory that does not exist yet (first argument of the shell
   script, `-Directory` in PowerShell).
2. Set `FLEET_SECRET_DIR` to the same absolute path.
3. Write the env file outside the checkout too. Forward slashes work on
   Windows.

The paths below are examples.

```sh
sh ../../../scripts/fleet/new-deployment-secrets.sh /srv/fleet/secrets/deployment
echo 'FLEET_SECRET_DIR=/srv/fleet/secrets/deployment' > /srv/fleet/compose.env
fleet_compose="--project-name linea-fleet --env-file /srv/fleet/compose.env"
docker compose $fleet_compose pull fleet
docker compose $fleet_compose up -d --wait postgres
docker compose $fleet_compose run --rm -T migrate
docker compose $fleet_compose run --rm -T bootstrap > /srv/fleet/secrets/deployment/admin.json
docker compose $fleet_compose up -d --wait fleet
```

In PowerShell 7:

```powershell
..\..\..\scripts\fleet\new-deployment-secrets.ps1 -Directory C:\fleet\secrets\deployment
Set-Content C:\fleet\compose.env 'FLEET_SECRET_DIR=C:/fleet/secrets/deployment'
$fleetCompose = @('--project-name','linea-fleet','--env-file','C:\fleet\compose.env')
docker compose @fleetCompose pull fleet
docker compose @fleetCompose up -d --wait postgres
docker compose @fleetCompose run --rm -T migrate
$fleetBootstrap = docker compose @fleetCompose run --rm -T bootstrap | ConvertFrom-Json
$fleetBootstrap | ConvertTo-Json -Depth 4 | Set-Content C:\fleet\secrets\deployment\admin.json
docker compose @fleetCompose up -d --wait fleet
```

For the [livecapture plugin](livecapture.md#install-with-docker), add
`FLEET_LIVECAPTURE_SECRET_DIR` to the same env file.

## Configuration and exposure

Optional Compose variables (environment or a protected env file):

| Variable | Default | Purpose |
| --- | --- | --- |
| `FLEET_SERVER_ID` | `local-fleet` | Stable authority bound to this database |
| `FLEET_API_PORT` | `8080` | Host loopback management port |
| `FLEET_AGENT_PORT` | `4320` | Host loopback OpAMP port |
| `FLEET_SECRET_DIR` | `../../../.local/deployment` | Secret files for this installation |
| `FLEET_VERSION` | this checkout's release | Image tag: a published release, or `dev` for a local build |

- Use the same `--env-file` and `--project-name` on every command of a
  customized installation. Secret paths resolve from the Compose project
  directory.
- A database cannot silently change server ID. Restore with the original
  identity.

### Server flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `--storage` | `postgres` | `postgres` (durable) or `memory` (development) |
| `--server-id` | required | Stable operator-assigned identity |
| `--tls-cert`, `--tls-key` | none | Serve both listeners over HTTPS/WSS |
| `--max-agents` | | Inventory capacity (1 to 100,000). At the limit, registration evicts the longest-disconnected agent before refusing |
| `--max-connections` | | Agent sessions per replica (1 to 10,000), and separately agent TCP connections that have not yet presented an agent credential. A replica holds at most twice this many agent connections |
| `--max-message-bytes` | 1 MiB | Largest OpAMP message an agent may send (1 KiB to 16 MiB); a larger one closes the session. Fleet sends up to the larger of this and 1,029 KiB, the largest remote config (8 layers of 128 KiB) |
| `--max-pending-sessions` | 400 | Handshakes per replica between admission and first commit (1 to 10,000) |
| `--max-events` | 100,000 | Retained events (1 to 1,000,000); keep at least two per agent |
| `--drain-timeout` | 20 s | Window for closing agent sockets on stop (0 to 30 s) |
| `--agent-attributes` | none | Up to 16 extra attribute keys to store |
| `--store-health-errors` | off | Store redacted health error text |
| `--agent-retention` | `168h` | Delete agents disconnected this long; `0` keeps them; at most `8760h` |
| `--plugins-config` | none | [Plugin registration](plugins.md#register-a-plugin) file |
| `--plugin-listen` | `127.0.0.1:8081` | Private plugin host listener |
| `--config-management` | off | [Remote config management](config-management.md) (PostgreSQL only) |
| `--config-allowed-components` | any | `kind:type` list config versions may use |

### Network exposure

- PostgreSQL has no published port and uses an internal Docker network.
- Fleet publishes HTTP and WebSocket on host loopback. For remote access,
  either put a trusted TLS ingress in front of both listeners, or set
  `--tls-cert` and `--tls-key` with read-only certificate mounts.
- Fleet has no mTLS. The default internal HTTP acknowledgement is confined to
  this Compose deployment.
- Core has no cookie auth or CORS, and rejects browser-origin WebSockets.
- Agents send bearer auth to `/v1/opamp`; polling is unsupported. API clients
  use a reader, operator or admin token on the management listener.
- The agent listener speaks HTTP/1.1 only. A connection counts against the
  unauthenticated bound until it presents an agent credential, and every
  refusal (401, 403, 404, 405, 426, 503) closes it. Fleet has no limit per
  source address: the ingress or load balancer in front of the agent listener
  must limit connections and connection rate per source, so one source cannot
  fill the unauthenticated bound and keep new Collectors out.
- Only process probes are anonymous. Metrics need fleet read access.

### Container hardening

- The image runs as UID 65532 with a read-only root, dropped capabilities,
  `no-new-privileges`, bounded memory, CPUs and PIDs, and ephemeral `/tmp`.
- Docker uses `LINEA_FLEET_DATABASE_URL_FILE`. The direct environment
  variable is for non-container development. Setting both is an error.
- Connection strings never become flags or log lines.

### Docker address pools

If `up` fails with `all predefined address pools have been fully subnetted`,
Docker has no free subnet for the project's networks. Before the first `up`:

1. Write an override file with fixed subnets that no existing network uses
   (check `docker network inspect`; do not delete unrelated networks).
2. Pass it as the last `-f` on every command.

The base file has `database` and `ingress`. The livecapture overlay adds
`plugins`, `capture-database` and `capture-ingress`. Omit networks you do not
run. Example `networks.yaml`:

```yaml
networks:
  database:
    ipam: {config: [{subnet: 10.241.10.0/24}]}
  ingress:
    ipam: {config: [{subnet: 10.241.11.0/24}]}
  plugins:
    ipam: {config: [{subnet: 10.241.12.0/24}]}
  capture-database:
    ipam: {config: [{subnet: 10.241.13.0/24}]}
  capture-ingress:
    ipam: {config: [{subnet: 10.241.14.0/24}]}
```

## Transition log

`linea-fleet` writes one JSON line on stderr for each state transition and
nothing else: no line per request, per agent report or per telemetry batch.
Metrics (see [operational metrics](operations-metrics.md)) say how much;
this log says what changed and why. Read it with `docker compose logs fleet`.

```json
{"time":"2026-11-15T10:02:11.482Z","level":"ERROR","msg":"replica_fenced","component":"fleet","replica":"5f0c...","reason":"heartbeat_lapsed"}
```

Every line has `time`, `level`, `msg` (the event name) and `component` (always
`fleet`; the live capture plugin writes `livecapture` in the same shape). The other fields come from a
closed set, so a line can never carry a credential, a configuration, an
attribute value, telemetry or an error message:

| Field | Value |
| --- | --- |
| `replica`, `plugin`, `credential`, `actor`, `rollout`, `assignment`, `agent` | An ID: at most 128 characters of letters, digits and `. _ : @ -`. Anything else is written as `invalid` |
| `version` | The release |
| `reason`, `state`, `listener`, `of` | A fixed name from the tables below |
| `agents`, `replicas`, `targets`, `waves`, `wave`, `incompatible`, `suppressed`, `count` | A count |
| `uptime_ms`, `outage_ms`, `duration_ms` | A duration in whole milliseconds |

| Event | Level | Fields | When |
| --- | --- | --- | --- |
| `process_started` | info | `version` | Configuration accepted, before the database opens |
| `process_ready` | info | | Both listeners are bound. This replaces the old `ready:` line, which also named the listener addresses; they are the flags you passed |
| `process_stopped` | info or error | `version`, `reason`, `uptime_ms` | The process finished stopping. Error level when it stopped because of a failure. The only account of why a failed process exited |
| `replica_registered` | info | `replica` | This process registered in `fleet.replicas` |
| `replica_deregistered` | info | `replica` | A graceful stop removed the registration |
| `replica_reaped` | warn | `replica` | This replica, as leader, removed another replica that stopped heartbeating |
| `agents_released` | warn | `agents` | The leader released agents that dead replicas held |
| `replica_fenced` | error | `replica`, `reason` | This replica can no longer prove liveness and stops. Reasons: `heartbeat_lapsed`, `registration_reaped` |
| `leader_gained`, `leader_lost` | info | `replica`, `reason` (lost only) | The leader lease changed. Reasons: `lease_unavailable`, `replica_fenced`, `shutdown` |
| `database_lost` | warn | `replica`, `reason` | Heartbeat writes began to fail. Reasons: `timeout`, `sqlstate`, `connection`. Written once per outage |
| `database_recovered` | info | `replica`, `outage_ms` | Heartbeat writes succeeded again |
| `plugin_state_changed` | info | `plugin`, `state`, `reason`, `actor` (lifecycle only) | A plugin's state changed. States: `ready`, `unavailable`, `incompatible`, `enabled`, `draining`, `disabled`. Reasons: `probe_ok`, `probe_failed`, `probe_lost` (the 3 s seen window closed), `probe_incompatible`, `instance_changed`, `lifecycle_enable`, `lifecycle_drain`, `lifecycle_disable`, `shared_state` (read from the shared row after another replica changed it), `restored` (at start, from the stored state) |
| `plugin_epoch_changed` | info | `plugin`, `reason` | The plugin's host epoch rotated and its admitted calls were cancelled. Reasons: `instance_changed`, `probe_failed` (the plugin refused), `probe_lost` (no replica reached it for 3 s), `lifecycle_enable`, `lifecycle_drain`, `lifecycle_disable`, `shared_state` (another replica changed the shared row) |
| `rollout_created` | info | `rollout`, `assignment`, `actor`, `targets`, `waves` | A rollout was admitted |
| `rollout_state_changed` | info | `rollout`, `state`, `reason`, `actor` | A rollout moved to `running`, `paused`, `rolling_back`, `completed`, `rolled_back` or `cancelled`. Reasons: `operator`, `threshold`, `controller` |
| `rollout_wave_released` | info | `rollout`, `wave`, `targets`, `incompatible` | The controller released a wave |
| `config_withheld` | warn | `agent`, `reason` | A config the server would send was held back. Reasons: `incompatible_components` (the agent's build lacks a component the config uses), `unverified_components` (the agent's build is not resolved: its component list did not match its hash, never arrived, or the store is full), `agent_reported_failure` (the agent reported the config failed, so the server holds it). One line per session and plan; at most 20 per 10 s |
| `credential_created`, `credential_revoked` | info | `credential`, `actor` | An admin created or revoked a credential, by ID |
| `credential_updated` | info | `credential`, `actor`, `reason` | A credential's remote config permission changed (`remote_config_enabled`, `remote_config_disabled`) |
| `auth_failures` | warn | `listener`, `count`, `duration_ms` | Failed authentications in the last 10 s on one listener (`management`, `agent`, `plugin_host`). One line per listener and only when the count is above zero, so a scan or a bad token is one line every 10 s, not one per request. Also written when the process stops |
| `lines_suppressed` | warn | `of`, `suppressed` | Lines of a rate-limited event (`config_withheld`) dropped in the last 10 s |

Reasons of `process_stopped`: `signal` (SIGINT or SIGTERM, graceful),
`shutdown`, `config_invalid`, `database_unavailable`,
`schema_or_identity_conflict` (the schema is not current or the database
belongs to another server ID: run `migrate`), `no_admin_credential` (run
`bootstrap`), `tls_identity_failed`, `listen_failed`, `plugins_invalid`,
`listener_failed`, `replica_fenced`, `shutdown_incomplete`, `internal`.

Lines for a rollout, a credential or a plugin lifecycle change are written
after the database committed it. A replica logs only its own view: the leader
writes `rollout_wave_released` and the reaper lines, and the replica that
served an API request writes that request's `credential_*` line. Join the
replicas' logs by `time` and ID.

The log never contains the error behind a failure. Usage and configuration
mistakes (an unknown flag, a missing secret, a value out of range) are
refused before the service starts with one plain message on stderr and exit
status 1. When the service itself fails, the last line of output is the
`process_stopped` line, followed by one fixed sentence.

## Restart and database failure

```sh
docker compose stop fleet
docker compose up -d --wait postgres
docker compose up -d --wait fleet
docker compose ps
```

| Event | Behavior |
| --- | --- |
| Graceful stop | Readiness is withdrawn, agent sockets close evenly over `--drain-timeout`, disconnected presence is recorded. Compose allows 30 s (`stop_grace_period`) |
| Crash | History stays durable. After the replica's heartbeat is 10 s old its agents read as disconnected (health and observation times preserved) and the leader replica releases them with `agent.disconnected` events |
| Reconnect | Increments `generation`. Event cursors survive restarts; refresh snapshots only after an event 410 |
| Reconnect storm | A full write queue answers new sessions 503 with `Retry-After`. A handshake waits up to 25 s for a slot before refusal, so opamp-go clients avoid long backoff. `--max-pending-sessions` bounds handshakes in progress |
| Silent agent | An agent session that sends no message for 90 s (three of the Collector's 30 s heartbeats) is closed and the agent reconnects; every received message renews the deadline. An agent configured with a heartbeat interval of 90 s or more is closed repeatedly |
| Database or own-registration loss | Readiness fails closed and the process stops within the heartbeat window. Compose does not restart it: restore the database, then start fleet explicitly |

Connect to PostgreSQL directly or through a session pooler. Transaction
pooling is unsupported (the server uses LISTEN/NOTIFY). Set `--max-events` to
at least two events per agent so a full reconnect storm does not expire
cursors.

## High availability

`compose.ha.yaml` is the recommended availability tier. It runs
`FLEET_REPLICAS` (default 3, at most 8) identical replicas behind an HAProxy
TCP load balancer, and adds `postgres-standby`, a hot standby fed by
streaming replication through a slot.

- Replicas are not published; the load balancer publishes the agent and API
  ports.
- Replicas reach PostgreSQL through a multi-host URL
  (`postgres:5432,postgres-standby:5432` with
  `target_session_attrs=read-write`), so they find the writable server.
- The primary creates the replication role and slot only when it initializes
  an empty volume. The standby clones the primary on first start.

```sh
bash ../../../scripts/fleet/new-deployment-secrets.sh      # also writes the replication and HA URL secrets
ha="-f compose.yaml -f compose.ha.yaml"
docker compose $ha pull fleet
docker compose $ha up -d --wait postgres postgres-standby
docker compose $ha run --rm -T migrate
docker compose $ha run --rm -T bootstrap > ../../../.local/deployment/admin.json
docker compose $ha up -d --wait fleet lb
```

| Event | Behavior |
| --- | --- |
| Replica loss | Only its agents reconnect. The load balancer marks it down on the first failed connect and spreads them with least-connections. Each replica's `--max-connections` must leave room for the survivors: at least agents / (replicas - 1). A replica that exits restarts (`restart: unless-stopped`); after a database outage longer than the heartbeat window it fences itself and returns under a new replica ID, without reclaiming agents another replica took |
| Database failover | An operator action (or Patroni, or a managed service): stop the old primary for good, then promote the standby, as below. Replicas reconnect on their own and agent sessions stay open. Event cursors stay valid |
| Replica turns unready | The load balancer closes no sessions. A draining or fencing replica closes its own sockets, spread out |

```sh
docker compose $ha stop postgres
docker compose $ha exec postgres-standby psql -U postgres -d fleet -c "SELECT pg_promote(true, 30)"
```

Never start the old primary beside the promoted one. Rebuild it as a new
standby from an empty volume.

The standby in `compose.ha.yaml` replicates asynchronously: a failover can
lose the writes the old primary committed in its last moments, a credential
revocation among them, and the revoked token then works again. After a
failover, revoke again any credential revoked shortly before it; the lost
`credential.revoked` event is gone too, so compare against your own record. To lose no committed write, set
`synchronous_standby_names` on the primary; every write then waits for the
standby, and writes stop while the standby is down.

Plugins behind replicas: every replica probes each plugin directly and keeps
its state in one shared row, so all replicas show one epoch. The plugin must
reach the host listener through the load balancer, whose `plugin_host`
frontend (`:8081`, HTTP, per-request balancing over ready replicas) a plugin
overlay puts on the internal plugin network; point the plugin's host
endpoint at `http://lb:8081`.
`examples/fleet-agent-summary-plugin/compose.ha.yaml` does this for the
example plugin, and `scripts/fleet/smoke-ha-plugin.sh` checks it: one epoch on
every replica, no rotation when a replica dies, a drain seen by all. The Helm
chart runs one fleet pod and needs none of this.

Sizing:

| Part | Guidance |
| --- | --- |
| Load balancer | Relays TCP only: one CPU and 512 MiB carried 20,000 sessions. Keep `nbthread` in `haproxy.cfg` equal to the container's `cpus`. `maxconn` (45,000 agents) must exceed the fleet with room for reconnect overlap. Idle timeouts are 90 s, above the heartbeat and event long-poll |
| Replicas | About 33 KiB per session: the 1 GiB limit covers 10,000 sessions with margin. Size `--max-connections` for the survivors of a replica loss |
| PostgreSQL | Each replica opens at most nine connections (a pool of eight and one reserved for its heartbeat), so eight replicas need 72 plus replication and administration, within the default `max_connections` of 100. A faster disk or more primary CPU shortens reconnect storms directly. Heartbeats cause no writes |

## Inventory and retention

| Topic | Behavior |
| --- | --- |
| Attributes | Fleet stores an allowlist of agent description attributes (service, host, OS, Kubernetes workload and cloud placement keys) and drops the rest. `--agent-attributes key1,key2` adds up to 16 |
| Health errors | Can contain endpoints or secrets, so stored only with `--store-health-errors`, redacted and truncated to 256 bytes |
| Effective config | Never stored or logged. Fleet keeps its SHA-256 and a component and pipeline summary; configurations over 256 KiB are only hashed |
| Agent retention | Disconnected agents are deleted after `--agent-retention`. The leader replica deletes at most 500 per run, removes them from static groups with a revision bump and writes one `agents.expired` event |
| At `--max-agents` | Registration evicts the longest-disconnected agent before refusing |
| Interrupted mutations | An admitted mutation finishes its short transaction even if the HTTP caller disconnects. Reconcile with caller-chosen IDs and group revisions. A token disclosure cannot be repeated |

## Config management

Optional and off by default. With `--config-management` (PostgreSQL only),
fleet sends versioned config to opted-in supervised Collectors. The
[operator guide](config-management.md) covers the API, layers, rollouts,
holds, limits and metrics. To start fleet with the example supervised
Collector:

```sh
docker compose -f compose.yaml -f compose.supervised.yaml up -d --wait fleet
# issue an agent credential with "remote_config": true, then:
LINEA_FLEET_AGENT_TOKEN=<token> docker compose -f compose.yaml -f compose.supervised.yaml up -d collector
```

## Backup

Stop fleet for an installation recovery point. Create a custom-format dump in
PostgreSQL and copy it as a binary file:

```sh
docker compose stop fleet
docker compose exec -T postgres pg_dump -U postgres -d fleet -Fc -f /tmp/fleet.dump
docker compose cp postgres:/tmp/fleet.dump ../../../.local/deployment/fleet.dump
docker compose up -d --wait fleet
```

- Keep the secret directory, the original server ID, the matching
  image/source revision and the dump.
- Dumps contain credential hashes, inventory, groups and events: protect them
  as sensitive metadata.
- Roles are initialized separately and are not in a single-database dump. See
  PostgreSQL's [dump/restore
  boundaries](https://www.postgresql.org/docs/17/app-pgdump.html).

## Restore into a fresh deployment

Use a new Compose project and volume with the runtime stopped. Reuse the
saved secret files so initialization creates the expected roles. Put the
original `FLEET_SERVER_ID`, unused host ports and the saved secret path in a
protected restore env file (keep local ones under `.local/`):

```sh
docker compose --project-name fleet-restore --env-file restore.env up -d --wait postgres
docker compose --project-name fleet-restore --env-file restore.env cp ../../../.local/deployment/fleet.dump postgres:/tmp/fleet.dump
docker compose --project-name fleet-restore --env-file restore.env exec -T postgres pg_restore -U postgres --exit-on-error --dbname fleet /tmp/fleet.dump
docker compose --project-name fleet-restore --env-file restore.env run --rm -T migrate
docker compose --project-name fleet-restore --env-file restore.env up -d --wait fleet
```

- Restore before migrating the empty volume. The dump recreates schema,
  ledger, owners and grants; the migration job verifies compatibility and
  runtime grants.
- Startup detects the new cluster, expires event cursors once and requires
  renewed reporting. The first replica to start moves every retained event
  ahead of new ones in one transaction of its own, allowed up to four
  minutes, before it registers; other replicas wait for it. Serving
  statements stay bounded at 2 s.
- Migrations and offline administration (`migrate`, `bootstrap`) run with a
  5-minute statement timeout, a 10 s lock timeout and a 10-minute overall
  bound.
- Original API tokens work if they were active at backup and are still
  unexpired. Bootstrap is unnecessary while an admin remains.
- Restore returns credential validity to the backup point. Reconcile or
  revoke keys created or revoked after it before reopening ingress.

## Upgrade and rollback

1. Keep the prior image/source revision, secrets and a verified backup.
2. Stop fleet (every replica), then pull or build the image of the new
   `FLEET_VERSION`.
3. Run migrations, start fleet, and check readiness and authenticated
   inventory, groups and credential metadata with an independent HTTP client.

- The current durable schema is version 12 (version 12 adds the `operator`
  credential role; version 11 lets admin and reader credentials be valid
  until revoked). Migrations only go forward, and
  `docker compose run --rm -T migrate` also re-applies the runtime role's
  grants.
- The config management tables exist in every installation and stay unused
  unless `--config-management` is on.
- Repeating a migration verifies every installed checksum and does no DDL.
  Newer or unknown versions, missing ledger entries and modified migration
  content are rejected.
- There is no down-migration, and earlier binaries cannot run a newer schema.
  Roll back with the earlier binary and a pre-upgrade backup in a fresh
  volume.
- The in-memory inventory has no durable format to import.

Acceptance scripts and what they cover are in [fleet
verification](../internals/fleet-verification.md).
