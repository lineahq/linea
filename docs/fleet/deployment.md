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
| Admin token | Expires in 90 days. `admin.json` is an operator copy of the one-time disclosure: store it in your secret manager |
| Admin expiry | Startup requires an active admin. If all admins expire, stop fleet and run offline bootstrap again. Bootstrap refuses while an active admin exists |
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
| `--max-connections` | | Agent TCP connections per replica (1 to 10,000) |
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
  use a reader or admin token on the management listener.
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

Sizing:

| Part | Guidance |
| --- | --- |
| Load balancer | Relays TCP only: one CPU and 512 MiB carried 20,000 sessions. Keep `nbthread` in `haproxy.cfg` equal to the container's `cpus`. `maxconn` (45,000 agents) must exceed the fleet with room for reconnect overlap. Idle timeouts are 90 s, above the heartbeat and event long-poll |
| Replicas | About 33 KiB per session: the 1 GiB limit covers 10,000 sessions with margin. Size `--max-connections` for the survivors of a replica loss |
| PostgreSQL | Each replica opens at most eight connections, so eight replicas need 64 plus replication and administration, within the default `max_connections` of 100. A faster disk or more primary CPU shortens reconnect storms directly. Heartbeats cause no writes |

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
  renewed reporting.
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

- The current durable schema is version 10. Migrations only go forward, and
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
