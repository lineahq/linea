# Compatibility

What each Linea part must match when you install or upgrade it. Linea does
not support mixed versions across a protocol change: when a protocol version
below changes, upgrade both sides together. The release notes in
`CHANGELOG.md` say when one changes.

## Version table

| Interface | Between | Current | Rule | Checked in |
| --- | --- | --- | --- | --- |
| Product version | `linea-server` and `linea-builder` | release tag | Equal. A builder from another release is refused at connect, so optional fields of the builder protocol, such as a result's `phases`, need no version. | `internal/debugger/platform/builders.go` |
| Worker protocol | `linea-server` and `linea-worker`, pool workers and runtime images | 2 | Equal. A runtime image or worker on another protocol is refused and listed as stale; build it again with the new server. Optional fields, such as a result's `phases`, are added within a version: an older worker sends none and an older server ignores them. | `internal/debugger/worker/protocol.go` (`ProtocolVersion`) |
| Collector release line | Debugger built-in runtime and the release table | 0.162.0 built in; runtimes 0.160.0 to 0.165.0 verified | A runtime builds for any release from 0.160.0; one outside the support table builds as unverified. Every runtime runs a [self-check](debugger/runtime-builds.md#self-check) at start; a failed check warns on each run and refuses nothing. | `build/linea-runtime.yaml`, `build/versions/` |
| Collector components | `collector/livecapture` and your Collector | Collector 0.162.0 / `v1.68.0`, OTTL 0.162.0 | Minimums. Later releases verified up to 0.165.0. | [Newer releases](internals/collector-components-compatibility.md) |
| Live-capture control protocol | `livecapture` extension and the live-capture plugin | `livecapture.control.v1` | Additive. A Collector ignores fields it does not know; a desired state that must not be ignored names its `required_capabilities`, and a Collector lacking one rejects it. | `api/livecapture/control/v1/control.proto` |
| Plugin host protocol | Fleet server and every plugin | 4 | Equal. A plugin whose manifest declares another protocol is not registered and shows as incompatible. Fields are added within a version: every receiver ignores fields it does not know. | `pluginsdk/protocol.go` (`ProtocolVersion`) |
| Plugin API version | Fleet plugin configuration and the plugin | live capture: 3 | Equal to the `api_version` in the fleet plugin configuration. | `internal/fleetplugins/livecapture/config.go` |
| Plugin version | Fleet plugin configuration and the plugin | release | Equal to the `version` in the fleet plugin configuration. | `internal/fleet/plugins/registry.go` |
| Debugger HTTP API | `linea-server` and the web client, connected CLI and MCP clients | `/api/v1` | The web client ships inside the server. Use a connected CLI from the same release. | `internal/debugger/api/openapi.json` |
| Fleet HTTP API | Fleet server and the debugger's `--fleet-url` | `/api/v1` | The debugger uses fleet's management API and the plugin API through fleet. | `internal/debugger/fleetcapture` |

## Databases

Each part keeps its own schema and migrates it forward on start. None runs
on a schema newer than its binary, so roll back only together with a
database restore.

| Database | Owner | Newer schema |
| --- | --- | --- |
| Debugger | `linea-server` | Refused at start (`ErrUnsupportedSchema`) |
| Fleet | `linea-fleet` | Refused at start |
| Live capture | `linea-fleet-livecapture` | Refused at start |

Fleet schema version 13 adds the shared plugin state (`epoch`, `instance_id`
and `seen_at` on `fleet.plugin_lifecycle`), and version 14 adds
`plugin_access`, `delegate` and `plugin_hash` to `fleet.credentials`. Run the fleet `migrate` step before
starting the new release; an older fleet server refuses the newer schema.

## Upgrade order

1. Debugger: stop the builders and workers, upgrade `linea-server`, then
   start the builders and workers of the same release. Rebuild runtime
   images when the worker protocol changed; the runtimes view lists them as
   stale.
2. Fleet: upgrade the fleet server and its plugins together when the plugin
   host protocol changed; otherwise in any order, with the plugin
   configuration's `version` updated to match.
3. Collectors: the control protocol is additive, so Collectors with the
   components can be upgraded on their own schedule.
