# Example plugin: agent-summary

A small, complete fleet plugin on [`pluginsdk`](../../pluginsdk/README.md) and
the standard library. Copy it to start a new plugin. It is optional and not
part of the fleet build.

| File | Purpose |
| --- | --- |
| `main.go` | Manifest, status and API routes; service-token, caller and epoch checks; host session loop with epoch fencing; directory paging and group resolution |
| `go.mod` | Own module; requires only `pluginsdk`, which `replace` points at `../../pluginsdk` |
| `deploy/docker/linea-fleet-agent-summary-plugin.Dockerfile` | Distroless, non-root image built from the repository root |
| `registration.json` | Fleet registration for the Compose overlay |
| `compose.yaml` | Compose overlay: registration, secrets, internal plugin network, hardened service |

Public API, through fleet at `/api/v1/plugins/agent-summary/api/`:

| Route | Role | Result |
| --- | --- | --- |
| `GET v1/summary` | reader, admin | `{agents, connected, healthy, observed_at}`, refreshed every 10 s and at each new session; 503 until the first refresh |
| `POST v1/groups/count` | admin | `{"group_id"}` or `{"selector"}` → `{connected_members, group_revision}`; 404 unknown group, 409 more than 1000 members |

Run it locally or with Compose as described in
[docs/fleet/plugins.md](../../docs/fleet/plugins.md#try-the-example). Checks:

```sh
GOMAXPROCS=2 go -C examples/fleet-agent-summary-plugin vet -p 1 .
docker build -f deploy/docker/linea-fleet-agent-summary-plugin.Dockerfile -t agent-summary-plugin:dev .
```
