# Live capture in the debugger

How the debugger receives, stores and derives inputs from live captures.
Behind [livecapture.md](../debugger/livecapture.md). Storage tables and limits
are in [artifact-storage.md](artifact-storage.md#captures); the plugin and
the Collector components are documented under `docs/fleet/` and
`docs/collector-components/`.

## Ownership

- The debugger is a fleet client. Fleet owns Collector connections,
  credentials, groups, the raw-capture permission and the capture lifecycle.
  The debugger selects Collectors, asks fleet to capture and stores the records
  fleet delivers.
- A behavior the debugger needs from Collectors is a change to the fleet
  plugin, not to the debugger. Collectors run the unmodified `capture`
  processor and `livecapture` extension.
- The module `github.com/lineahq/linea/collector/livecapture` is not linked
  into the debugger. The OCB manifest (`build/linea-runtime.yaml`) and the
  built-in registry never name it. The root `go.mod` requires it only for the
  generated control-protocol package that the fleet plugin imports. The
  debugger has no Collector-facing listener.
- A real runtime built with the module runs the extension as a custom
  extension, unwrapped, so the processor finds its concrete
  `*livecaptureextension.Manager`. The clients its configuration declares,
  such as the delivery exporter, are pointed at loopback sinks of the run
  (see [components.md](components.md)). On the built-in registry the types are
  stand-ins: `capture` passes data through and `livecapture` is inert, so
  settings stay untyped.

## Fleet connection

- The token file holds one fleet API token, at most 4 KiB, surrounding
  whitespace trimmed. It is read once at startup and never accepted as an
  argument, logged or returned in errors.
- The token's credential needs fleet admin permission to create captures and
  channels; a reader credential can only discover.
- Without `--fleet-url`, inventory reports `enabled: false`, capture creation
  is refused and recorded captures stay readable.

## Discovery

- A Collector entry has its fleet peer ID (`collector_id`), connection
  `generation`, agent ID, fleet credential ID and the cluster, namespace, pod
  and Collector group it reports. `enabled` is false when the livecapture
  extension has capture disabled. Each point has pipeline, point, signal and
  sensitivity; `point_count` and `raw_point_count` summarize them.
- Inventory and point lists take `limit` (zero or omitted is 25, at most 100
  and the platform item budget) and fleet's opaque `cursor`. Responses are
  `no-store` and never contain credentials.
- The web client lists 25 entries and polls inventory every five seconds and
  sessions every two seconds in the foreground.

## Selection and bounds

- A selection resolves to at most 16 Collectors. Fleet captures only connected
  members of a group or selector that report every selected point and signal;
  the debugger's preflight applies the same rule. New members never join an
  admitted capture.
- Duration 5 to 280 seconds (default 60) keeps the capture and its delivery
  channel, which outlives it by 15 seconds, inside fleet's 5-minute bound.
- Filter limits: each Collector reports `filter_limits` on
  `GET /api/v1/collectors/{id}`: `max_conditions` across all contexts of one
  capture (not per context), `max_condition_chars` per condition and
  `max_regex_chars` per regular expression. A Collector that predates the
  report gets the version 1 limits, 16, 256 and 256.
- Preflight, the start request and fleet each check a target against its own
  limits. A filter no target fits is refused as `invalid_argument` naming the
  count or length and the Collector's limit. A group or selector keeps only
  the members it fits. The web form applies the strictest limit of the chosen
  Collectors and counts characters, not bytes.
- The debugger's fixed ceiling is 32 conditions per context, 4,096 bytes each
  and 16 KiB in total. The Collector's filter compiler stays authoritative for
  syntax.
- Fleet's ceilings apply to zero or omitted budgets, and larger values clamp
  down: 1 MiB/s, 5,000 records/s, 16 MiB and 10,000 records in total, 64 KiB per
  record. The Collector may clamp further. Negative values are refused.
- Default selection permits only points the Collector marks redacted. The web
  client offers raw points with a "needs fleet permission" label.
- At most 1,024 sessions exist; kept sessions hold their slots until deletion
  and are never evicted to admit a capture. List pages hold compact summaries
  (`target_count`, `active_count`, `failed_count`, `uncertain_count`); status
  returns the full selection and per-target state. HTTP and MCP waits return
  the latest state after at most 30 seconds; interrupting a wait stops
  observation only.

## States

- `state` is fleet's operation state: `starting`, `active`, `stopping`, then
  terminal `stopped`, `completed`, `expired`, `failed` or `interrupted`. The
  debugger adds terminal `uncertain` when it loses track of a capture fleet has
  not reported finished; its targets become `unknown`. A target fleet leaves
  unfinished in an ended operation (for example after `interrupted`) also
  becomes `unknown`, since the debugger cannot claim its outcome.
- Target states: `pending`, `active`, `stopping`, then terminal `completed`,
  `expired`, `limit_reached`, `failed`, `disabled` or `unknown`, with fleet's
  per-target stats (selected and enqueued records, filter errors) when
  reported. Enqueued records are not received records.
- `reason`: fleet's code (`absolute_expiry`, `collector_rejected`,
  `peer_disconnected`) or the debugger's loss reason `capture_channel_lost`,
  `fleet_no_longer_reports_capture`,
  `no_terminal_state_from_fleet_after_expiry` or
  `platform_restarted; capture_not_resumed`.
- `stop_reason`: `stop_requested`, `capture_storage_error`,
  `capture_storage_limit`, `capture_processing_limit` or
  `capture_catalog_changed`.
- The web client turns the codes into sentences and marks a capture with
  failed or unknown targets as incomplete.
- Stop asks fleet to stop and waits for its terminal report; terminal status
  does not prove that every record was delivered. If fleet never reports a
  terminal state, the debugger marks the capture `uncertain` 30 seconds after
  expiry; fleet still ends the capture at its expiry.
- A restart never resumes a capture: an unfinished capture recovers as
  `uncertain`. Graceful shutdown asks fleet to stop open captures and deletes
  their channels, waiting at most two seconds.

## Receiving telemetry

- For each capture the debugger creates a fleet capture channel and reads the
  batches fleet delivers on it. There is no OTLP listener.
- Batches are acknowledged with the records stored; a redelivered batch is
  acknowledged again without storing it twice. Repeated Collector deliveries
  stay repeated records because the protocol has no unique delivery
  identifier.
- The five capture routing tags (`capture.session.id`, `capture.point`,
  `capture.pipeline`, `capture.collector.pod`, `capture.cluster`) are removed
  from each resource on receipt. They are the Collector's, never the source's,
  so stored records keep exactly the source's resource attributes.
- Refused: batches from Collectors outside the capture, unselected signals or
  points, and wrong encodings.
- Bounds:

  | Bound | Value |
  | --- | --- |
  | Batch | 1 MiB; 8,192 records, resources or scopes |
  | Stored record | 64 KiB |
  | Session | 8 MiB and 10,000 records |
  | Payload memory, all captures | 64 MiB |

  Active captures reserve their full memory allowance; finished captures
  release it and are read from the store on demand.
- Reaching a limit stops the capture with `capture_storage_limit` or
  `capture_processing_limit`; a storage failure stops it with
  `capture_storage_error`. A changed point catalog stops it with
  `capture_catalog_changed`, because the stored sensitivity classification no
  longer applies. Session `data` reports received and rejected counts.

## Storage

- Every capture, raw included, is stored as immutable segments, gzip-compressed
  when that makes them smaller; reads return the original bytes. A server keeps
  metadata in PostgreSQL and segments in object storage.
- There is no retention period. A capture stays until deleted, bounded by
  `store.storage_limit`, which ordinary runs and sources share.
- Restart recovers committed records and frozen metadata. A service without a
  repository keeps captures memory-only and reports `lost_on_restart` after a
  restart.
- Raw points are stored exactly as the source sent them (minus the routing
  tags), unencrypted like any capture. "Redacted" is the Collector's declared
  point classification, not a guarantee that telemetry holds no sensitive
  value.
- Record pages default to 25 entries, at most 100, and honor the platform byte
  budget. The first page freezes a receipt watermark; following cursors never
  include later arrivals. Filters, limit, session and service lifetime bind
  the cursor. Oversize JSON payloads are omitted while their sequence and
  source stay selectable.
- Deletion invalidates reads with not found and only works on terminal
  captures. Saved derivatives and admitted runs keep their own copies.
- Capture details (`name`, `description`) are protected by `metadata.revision`;
  status reports and incoming records do not change it. Names may be cleared to
  restore the timestamp display. An update persists in the capture journal
  without rewriting payload segments. Data, selection, status and storage
  cannot be edited.
- Fleet groups are read-only in the capture view. Capturing a group resolves
  its members when the capture starts.

## Deriving inputs

- Materialization needs one exact source stream per input and a fixed
  watermark. An optional sequence list chooses up to 1,000 records. Omitted,
  it chooses every matching record up to the watermark and is refused above
  1,000.
- `config_yaml`, an explicit receiver and optional `setup_yaml` validate the
  destination. No candidate processors are removed or reordered.
- Records no longer carry the routing tags, so `retain_tags` has no effect;
  it is still accepted.
- Preserved: resource, scope and schema context, integer precision, nanosecond
  timestamps, metric metadata and exemplars.
- Fixture JSON is bounded to 4 MiB. `fixture_json` carries the canonical
  immutable bytes; use it instead of reserializing `payload` to keep its digest
  and values.
- At most two fixture, scenario or export derivations prepare concurrently;
  excess requests return resource exhausted without joining the engine queue.
  A scenario uses one slot for all its selected inputs.
- Scenario requests take `config_yaml`, 1 to 16 `inputs` of the selection
  shape, optional `setup_yaml` and `assertions_yaml`, and optional `save` and
  `label`. Export requests wrap exactly one request under `fixture` or
  `scenario` and never save. MCP wraps each operation's request as
  `{"session_id":"...","request":{...}}`.
- Source hashing prefixes each stored protobuf record with ASCII
  `sequence:byte_length:` in receipt order. It does not identify the original
  production batch or exporter request.
- A returned derivation includes the session, exact stream, sequences and
  watermark, tag policy, receiver, admission-time source identity and reported
  versions, source sensitivity, stored-source digest, destination config and
  setup digests and the canonical fixture digest. Saved fixture and scenario
  artifacts record these under a platform-authored `capture` origin.

### Capture origins

- Generated scenario YAML and fixture `setup_yaml` carry bounded
  `capture_origins` metadata (at most 64 origins, 128 KiB). The web input
  append preserves it with the document.
- Quick-run setup extraction, immutable input bundles, `GetRunInputs` and
  scenario export carry it through capture deletion and restart, with no new
  bundle format or named artifact. No run or bundle schema changed.
- Origins are private lineage metadata: they grant no capture permission and
  cannot verify production coverage or redaction. Their fixture digest
  identifies the original derivation; editing input bytes does not change it or
  imply an unchanged capture.
- Standalone OTLP JSON cannot carry scenario metadata, so fixture and export
  JSON responses include a companion `setup_yaml`, and the web client offers
  **Download input setup** beside the payload download. Scenario YAML exports
  include provenance directly. For API and CLI quick runs, pass `fixture_json`,
  `receiver` and `setup_yaml` together.
- Older strict scenario parsers reject the optional `capture_origins` field;
  removing it permits older readers at the cost of lineage.
- Loading a saved capture fixture by revision merges its origin into a derived
  quick setup before admission. Declared setup values, comments and the fixture
  bytes are preserved, and the source file is never written. Older origins can
  omit unavailable source identity or hash fields; loading them does not invent
  missing evidence.
- Captured subsets do not establish complete traces, original batching, global
  ordering, production timing or production-suffix execution. Assertions must
  be supplied and evaluated through ordinary debugger operations.

## Web client behavior

- **Add captured input** (overview) or **Inputs → Add input → From a
  capture...** opens a picker with **Existing captures** and **Start new
  capture** tabs. The empty Inputs tab offers the same source. Closing the
  picker preserves the draft; a running capture continues until stopped or
  expired.
- **Add selected to scenario** turns every selected source stream into one
  input. All streams prepare before one draft update, preserving existing
  inputs, comments, mocks, assertions and edits. A later preparation failure
  or cancellation adds nothing. Concurrent input edits are preserved; changes to
  destination config or setup need another review.
- Materialization is bounded to 16 streams, 1,000 records per stream and 4 MiB
  of newly prepared fixture data. Recorded capture data stays immutable.
- The collectors page manages collection, capture metadata and retained data.
  Its primary selected-record action is also **Add selected to scenario**,
  which appends to the current browser draft and opens the Inputs tab. It has no
  scenario setup or assertion editors; mocks, environment values, execution
  settings and assertions belong to the debugger. The input picker hides capture
  administration and separate-copy actions.
- Receiver dropdowns come from the platform's validated configuration graph and
  offer only receivers accepting the stream's signal; a unique choice is
  automatic. Missing configuration, malformed scenario YAML, invalid
  configuration or mocks, missing signal pipelines and failed checks show their
  reasons and repair links beside the selection. Missing signals offer direct
  deselection; adding the pipeline happens in the debugger. Checks and
  materialization use the current scenario's inherited setup. Config or setup
  edits invalidate compatibility before debounced revalidation. The platform
  stays the authoritative validator.
- Raw-derived drafts stay in browser memory and bypass localStorage, including
  later edits. Saving, exporting or submitting a run keeps a separate copy;
  capture deletion does not remove run-owned inputs. Fixture save or download
  on the collectors page keeps its sensitive-copy warning.
- In the collectors page's **New capture**, the Collector list groups by
  cluster; **Group by** offers namespace, Collector group or none. **Capture
  group** beside a metadata group resolves matching Collectors across all
  inventory pages. A fleet group can be captured from **Groups** or with
  `?group=<id>`. Only points present on every target are offered. **Check
  selection** runs bounded preflight and Start repeats it before asking fleet.
  Starting opens Capture history with that session selected.
