# Fleet verification

The checks that back the fleet docs. Run focused checks sequentially and
follow the rules in [development.md](development.md) (shared machines,
`GOMAXPROCS=2`, `-p 1`, no full suite or load tests). Tests with the real
Collector are kept distinct from unit fixtures.

## Source development

Go 1.27 or later is needed only for source development. From the repository
root:

```sh
scripts/fleet/check.sh
scripts/fleet/test-postgres.sh
scripts/fleet/test-postgres.sh --race
```

```powershell
.\scripts\fleet\check.ps1
.\scripts\fleet\test-postgres.ps1
.\scripts\fleet\test-postgres.ps1 -Race
```

```bash
bash scripts/fleet/smoke-docker.sh
```

- The PostgreSQL tests create a disposable database container and also run
  several replicas against one database. Race tests use a two-CPU / 2 GiB
  Linux container.
- The Docker smoke uses a uniquely named Compose project, a real Collector
  and an independent HTTP client. It covers authorization, crash recovery,
  group persistence, revocation, full dump/restore and fail-closed database
  outage behavior. It removes only its own fixtures.

## Acceptance scripts

| Script | Covers |
| --- | --- |
| `scripts/fleet/smoke-docker.sh` (bash) | A unique Compose project with private temporary secrets, removed afterwards. Collector Contrib 0.162.0 and independent HTTP; negative roles; narrow runtime grants; a Collector with a failing component reported unhealthy in PostgreSQL inventory; kill and restart with disconnected presence and a surviving cursor; group history; revocation; dump and restore (a fresh-database restore preserves membership and a revocation present in the dump); database-loss shutdown. `SKIP_BUILD=1` reuses the image; `SUBNET` selects fixed subnets when Docker's address pools are exhausted |
| `tools/fleet-capacity/saturation.sh` | Bounded single-replica saturation, storm and churn profile |
| `tools/fleet-capacity/ha.sh` | Replica kill, fencing, database failover and presence through `compose.ha.yaml` (see [the simulator guide](../../tools/fleet-capacity/simulator.md)) |
| `scripts/fleet/smoke-supervisor.sh` | One official supervisor with the contrib Collector, started from `compose.supervised.yaml`, against fleet with `--config-management`: good, invalid and crashing configs, a version using a component the build lacks, a retry, a supervisor restart and release to the local config. It then adds a second supervisor for a two-wave rollout that pauses on a failing second wave and is rolled back. Uses subnets `.205` and `.206`, removes its own resources |

`scripts/fleet/test-postgres.sh` (`.ps1` on Windows) additionally checks
atomic publication rollback, omitted fields, unsigned capability bits,
generation fencing, cursor gaps, expiry, and schema and checksum rejection.
With two and three replicas against one database it checks cross-replica
takeover, fencing, identity conflicts, revocation and event hints, crash
reaping, leader failover, graceful deregistration, epoch rotation and
migration refusal. `-Race` runs in a two-CPU / 2 GiB Linux container. These
functional checks make no capacity claim.

## Plugins

- `scripts/fleet/check.sh` (`.ps1` on Windows) covers the registry and host
  services with in-test plugins (`internal/fleet/plugins`).
- The PostgreSQL inventory test covers credential labels, lookup by hash and
  introspection.
- The example plugin was run against fleet in memory mode (readiness, API
  forwarding, role checks, refusals, restart with epoch rotation) and through
  its Compose overlay with PostgreSQL (container hardening, admin API, drain
  and enable fencing).

## Livecapture plugin

The module has its own tests, Docker smoke and `e2e-fleet.sh`, which proves
the Collector-facing pattern with an unchanged Collector extension
(`scripts/fleet/livecapture`).

| Script | Covers |
| --- | --- |
| `scripts/fleet/livecapture/test-postgres.sh --race` (`.ps1 -Race` on Windows) | Creates and removes a bounded disposable database |
| `scripts/fleet/livecapture/smoke-docker.sh` (`.ps1` on Windows) | Builds and runs the test-only `linea-collector` image, the public consumer and isolated databases |
| `scripts/fleet/livecapture/e2e-fleet.sh` (bash) | The fleet path end to end: the test-only Collector image (`COLLECTOR_IMAGE`, built from `deploy/docker/linea-collector.Dockerfile`) beside a contrib OpAMP Collector, credential labels, linking, group and selector captures, label refusal and core revocation |

Fixture secrets and output stay in ignored private directories. No fixture
is installed in the persistent fleet.

## Config management

The real Collector distribution is exercised by
`scripts/fleet/smoke-supervisor.sh`: the official supervisor 0.163.2 with the
contrib Collector 0.163.2, started from `compose.supervised.yaml`. It covers
applied, invalid, crashing and incompatible configs, holds and retries,
supervisor restarts, unmanaging and a paused and rolled-back two-wave
rollout. Scale and failover are measured separately with protocol clients
(official opamp-go, no Collector processes) at 10,000 agents on three
replicas.
