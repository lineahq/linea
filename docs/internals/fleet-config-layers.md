# Fleet config layers and the config bisect

Findings behind the fleet config bisect: how an agent's
configuration is assembled from linea-fleet's config layers, and what the
debugger rebuilds when it probes one version of a layer.

## What fleet stores and sends

- A fleet config is a name with immutable, numbered versions. Fleet keeps
  the newest 100 versions of each config; older ones are pruned. Reading a
  version's body needs an admin credential.
- `GET /api/v1/configs/{id}/versions` lists versions newest first; `after`
  pages to versions older than the given one.
- `GET /api/v1/agents/{id}/config` returns the layers fleet sends a managed
  agent: each layer is an entry named `NN-<name>` with its config ID and the
  version sent. An agent that only reports its configuration has hashes, no
  bodies, so it cannot be bisected (`invalid_request`). A layer list fleet
  marks `too_many_layers` is a `conflict`.
- Rollouts carry from and to versions; the bisect only uses them to annotate
  versions, and a failed read becomes a note.

## How the OpAMP supervisor merges

Verified against opampsupervisor v0.163.0. The supervisor merges its local
`config_files` in order, then the remote config entries in entry name order
(hence fleet's `NN-` prefixes), with confmap's merge:

- maps merge key by key; lists and scalars are replaced by the later
  document;
- `service::extensions` lists are concatenated without duplicates;
- `service::telemetry::resource::attributes` lists merge by attribute name
  (first position, last value);
- `${...}` references stay unexpanded; the Collector expands them at startup.

The supervisor also adds its own documents: its OpAMP extension, its own
telemetry settings and, without remote config, a no-op pipeline. They never
touch the data path, so `internal/debugger/bisect.Compose` leaves them out.

## What the bisect rebuilds

For each probed version of the searched config, `Compose` merges the local
files given with the request (at most 8, in order), then every layer fleet
sends the agent now, with the searched layer's body replaced by the probed
version's. Other layers stay at the versions fleet sends now, so a regression
caused by an interaction with an older version of another layer is not
reproduced.

Limits and assumptions:

- Without the agent's local supervisor files, the rebuild only has fleet's
  layers; a note says so. Pass them with `--local` (CLI) or `local` (HTTP,
  MCP) when the supervisor merges local files under fleet's layers.
- At most 100 versions per search and 16 scenarios. A bound fleet has
  pruned is `not_found`; pruned versions inside the range are noted.
- The search assumes that once a version is bad every later one is bad, as
  git bisect does. A version whose configuration does not load or whose runs
  are inconclusive is skipped; when only skipped versions separate a good and
  a bad one, the verdict is `inconclusive` with both named.
- The debugger never stores version bodies; they are read per bisect and
  only the culprit diff is returned.
