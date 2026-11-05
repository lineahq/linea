# Fleet config management internals

How the server decides to send configuration. The operator guide is
[config-management](../fleet/config-management.md); the layer merge and the
config bisect are in [fleet-config-layers.md](fleet-config-layers.md).

## Reconcile and send paths

- The server stores desired state in PostgreSQL and queues no commands. The
  replica that holds an agent's session sends its config when the desired
  hash differs from the hash the agent reports.
- Admin writes and the rollout controller issue `NOTIFY fleet_config`. Each
  replica's gateway refreshes its reconciler cache on notify and every 60 s.
  The reconciler reads assignments and managed credentials from the database.
- The gateway runs `Reconcile` per session, on a report or status change and
  in a paced sweep of the replica's own sessions. There are two send paths:
  in the reply to a report (`path=response`), or pushed asynchronously after
  an assignment changes (`path=async`, at most 200/s per replica).
- The rollout controller runs on the leader replica only. After a leader
  change the next leader resumes from the database.

## Send rule timing

- A hash is not sent again for 30 s while the agent has not reported it.
- Delivery waits up to 10 s after a reconnect for the agent's full state, and
  up to 10 s for a requested component list.
- A failed or locally rolled-back hash is held and not sent again until the
  desired config changes or `POST /api/v1/agents/{id}/config:retry` clears it.
- Config version bodies are cached per replica (64 MiB bound).

## Without the flag

- The gateway holds no reconciler and advertises no remote-config capability.
  Its replies carry only the instance UID, capabilities and report-state
  flags. Reporting never reaches any send node.
- Fleet sends no restart, package or connection-settings commands in any
  mode.

## Measurements

Replica restarts, kills and database failovers lose nothing and send nothing
twice. This was measured with 10,000 protocol clients (official opamp-go, no
Collector processes) on three replicas. Real Collector behavior is exercised
by `scripts/fleet/smoke-supervisor.sh`; see [fleet
verification](fleet-verification.md#config-management).
