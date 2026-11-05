# Lightweight OpAMP saturation profile

`saturation.sh` runs many independent authenticated WebSocket sessions from
one small Go process (`linea-fleet-simulator`) against a disposable
Compose deployment. Each session is an official
[opamp-go](https://github.com/open-telemetry/opamp-go) WebSocket client set up
like the Collector's OpAMP extension: it reports status and health, and the
client itself does the handshake, the full-state exchange, the 30 s heartbeat,
reconnects and their backoff (`Retry-After` included). Like a real agent it has
no response timeout: it notices a lost server only when the socket closes. The
simulator paces starts and planned restarts (stop, then start under the same
or, for churn, a fresh identity), toggles health and observes the clients
through their callbacks and a byte-counting connection wrapper, which also
reads the HTTP status of refused handshakes. A heartbeat other than 30 s
declares `ReportsHeartbeat`, which opamp-go requires to apply it. All sessions
share one deployment bearer token. No Collector processes are involved: these
are protocol-client results, kept separate from real-Collector evidence.

For config management the simulator can act like a Collector run by the
OpAMP Supervisor. `-accept-config` declares `AcceptsRemoteConfig` and
`ReportsRemoteConfig`. Each offered config is reported `APPLYING`, then after
`-apply-delay` either `APPLIED` or, for `-fail-percent` of the agents,
`FAILED`. The failing agents are fixed per agent and config. The last status
survives planned restarts, as the Supervisor's persisted state does.
`-builds N` reports `AvailableComponents` from N builds with the components of
a typical contrib Collector. `-minimal-percent` of the agents run instead a
build without the `filelog` receiver, so a version that uses it is withheld
from them. Without `-accept-config`, opamp-go drops offered configs, as it does
for a reporting Collector.

Bash, Docker Compose v2 and Linux containers. No host Go runtime. From the
repository root, one step at a time:

```bash
AGENTS=1000 STORM=1 bash tools/fleet-capacity/saturation.sh            # builds both images
SKIP_BUILD=1 AGENTS=2500 STORM=1 bash tools/fleet-capacity/saturation.sh
SKIP_BUILD=1 AGENTS=5000 STORM=1 bash tools/fleet-capacity/saturation.sh
SKIP_BUILD=1 AGENTS=10000 STORM=1 bash tools/fleet-capacity/saturation.sh
# UID-churn soak against capacity and retention bounds:
SKIP_BUILD=1 AGENTS=2000 CHURN_RATE=20 STEADY=900 MAX_AGENTS=4000 RETENTION=2m bash tools/fleet-capacity/saturation.sh
```

These stepped, bounded mock-session profiles are allowed on a shared
development host: run one at a time, step up only after the previous step
passed, and never alongside another load profile. The
[real-Collector profiles](README.md) keep their dedicated-environment guard.

## Phases and gates

1. **Ramp** at `CONNECT_RATE` (default 200/s). `ready` holds while the driver
   checks `/api/v1/summary` and snapshots metrics, container resources and
   PostgreSQL counters (after 11 s, because table statistics arrive late).
2. **Steady** heartbeats every `HEARTBEAT` (30 s) for `STEADY` (120 s), with no
   health changes. Presence must hold throughout. Gate: the batched writer
   receives **no items**, no events or agents appear, there are no report errors,
   and agent row updates stay within one per agent per 10 s `last_seen` flush.
   Transactions per second (including read-only statements), tuple writes and
   WAL bytes per second are recorded to compare across steps.
3. **Planned reconnects** of `RECONNECT_PERCENT` (1 %) of sessions.
4. **Storm** (`STORM=1`): the driver reads the event stream to its end, then
   SIGKILLs the replica and restarts it. Every session must reconnect within
   `STORM_TIMEOUT`; the gate is under 60 s from the first lost session. The
   pre-crash event cursor must still return 200, and the summary must show every
   session connected again.
5. **Churn soak** (`CHURN_RATE` > 0): during steady reporting, that many sessions
   per second disconnect and return under a fresh instance UID. Every 15 s the
   driver samples inventory, events, table sizes and replica memory. Gate:
   inventory never exceeds `MAX_AGENTS`, retention expired agents, and the
   second half grows neither inventory nor memory by more than a quarter over
   the first half's peak.

Every snapshot checks the memory budget: at 1,000 or more
sessions, the heap live at the last GC plus goroutine stacks
(`linea_fleet_go_heap_live_bytes` + `linea_fleet_go_stack_bytes`) per connection
must stay at or below 64 KiB, and `linea_fleet_go_memory_bytes` within
`GOMEMLIMIT`. Total Go memory per connection is not the budget: it includes
garbage awaiting the next GC, up to `GOGC` times the live heap, and doubled on
a replica right after a reconnect storm while live heap plus stacks stayed at
about a third of the budget per session. The run fails at the
first breached gate, OOM kill or simulator error.

## Bounds and resources

The simulator accepts 1–20,000 sessions, at most 1,000 connections/s, 200
churned identities/s, 64 KiB synthetic agent-description padding and one hour
of steady reporting. The steady stream (`agents / heartbeat`, plus health
changes) and two exchanges per new or churned session must stay within 5,000
messages/s. There is no client-side report pacing: as with real agents, storm
recovery is limited by server admission and the clients' own backoff. The
handshake timeout is opamp-go's 45 s.

Caps: replica two CPUs and `FLEET_MEMORY` (1 GiB, `GOMEMLIMIT` 896 MiB),
PostgreSQL one CPU / 512 MiB, simulator `SIM_CPUS` (two; `ha.sh` four above
10,000 sessions) and `SIM_MEMORY` (1 GiB; `ha.sh` 2 GiB above 10,000),
256 PIDs, read-only, non-root, no capabilities. Each opamp-go client costs the
simulator about 72 KiB, so 20,000 sessions need about 1.4 GiB. At 20,000
sessions the four-CPU simulator saturates during reconnect storms and slows
their recovery: 8.4 s and 16.8 s for the kill and fence storms, against
7.6 s and 15.7 s with eight CPUs. Fixed subnets `SUBNET.1.0/24`
and `SUBNET.2.0/24` (default `10.248`) avoid exhausted Docker address pools.
`FLEET_ARGS` passes extra replica flags; `TAG` selects images; `KEEP=1` leaves
the project running for inspection.

## High availability profile

`ha.sh` runs the same simulator against `compose.yaml` plus
[`compose.ha.yaml`](../../deploy/compose/fleet/compose.ha.yaml): `REPLICAS` (default 3) replicas behind
the HAProxy TCP load balancer and a PostgreSQL streaming standby. Sessions
enter through the load balancer only.

```bash
AGENTS=3000 bash tools/fleet-capacity/ha.sh                  # builds both images
SKIP_BUILD=1 AGENTS=10000 bash tools/fleet-capacity/ha.sh
SKIP_BUILD=1 AGENTS=20000 bash tools/fleet-capacity/ha.sh
```

The simulator runs three storm windows (`-storms 3`). The driver opens each
storm, causes the fault, then writes `storm_end` (`storm_end-2`, `storm_end-3`)
once the fault is over. A storm ends when every session lost in it is back and
3 s have passed since the last loss, so a window can close with no loss at all.

1. **Ready and steady**: sessions spread over every replica; the steady gate is
   the same as above, summed over replicas.
2. **Replica kill**: SIGKILL one replica. Only its sessions may drop, and they
   must be back within 60 s on the survivors.
3. **Fencing**: freeze one replica (`docker pause`) until the leader reaps it,
   then thaw it. It must fence itself (exit and come back under a new replica
   ID) without reclaiming any agent; its sessions return within 60 s.
4. **Database failover**: read the event stream to its end, stop the primary,
   promote the standby. Replicas reach it through the multi-host URL. The old
   event cursor must return 200, a write must succeed, the cluster
   identifier must be unchanged, and no replica may restart. Lost sessions,
   if any, must return within 60 s.
5. **Presence**: after the simulator closes every session, the summary must show
   0 connected and no agent may still be owned.

`ROLLOUT=1` adds config management: replicas run with
`--config-management` and the sessions with `-accept-config`, four builds and
5 % on a minimal build. A four-wave creating rollout of version 1 runs through
the replica kill, and a rollout to version 2, which uses `filelog`, through
the database failover; each storm stays open until its rollout completes
(`STORM_TIMEOUT` defaults to 600). Gates: both complete with every compatible
target succeeded and the minimal build `incompatible`, nothing failed or held,
each session offered each config it can run exactly once, and no replica above
220 asynchronous sends/s.

Each replica gets `max_connections = AGENTS / (REPLICAS - 1) + 64` (capped at
10,000), so the survivors of a kill can hold every session. Caps per replica
are the same as above; the load balancer and the standby get one CPU and
512 MiB each. The default subnet is `10.247`. Output goes to
`.local/fleet-ha-<id>/`, as for the saturation profile.

## Output

Private ignored `.local/fleet-sat-<id>/` keeps `result.json` (inputs, limits,
source revision, image IDs, phase observations, per-phase snapshots and gate
results), `observations.jsonl`, the metric, resource and PostgreSQL snapshots
per phase, `soak.jsonl` during churn and the last 200 replica log lines. There
are no tokens, identities, payloads or telemetry in result files; the
directory also holds fixture secrets under its private parent. A failed run
establishes no capacity result.
