# Artifact storage

`internal/debugger/artifact` defines the storage the platform depends on:
source revisions, the run journal with input bundles and evidence, captures,
the audit log, API tokens and web sessions. `internal/debugger/store/pg`
implements it on PostgreSQL and S3-compatible object storage; a team server
(`linea-server --config`) uses nothing else.
The platform owns lifecycle and evidence semantics; the repository stores
their versioned envelopes and blobs.

Operators need the first two sections (layout, limits and retention) and the
HTTP endpoints at the end. The rest describes the contract between the
platform and the store, for people who write or review a store.

## Storage layout

PostgreSQL holds every row; object storage holds every byte payload. A row
keeps the payload's digest (`sha256:<hex>`) and size, and the payload lives at
`<prefix>/sha256/<hex>` in the bucket (`store.objects.prefix`, empty by
default). Content addressing deduplicates: equal bytes are one object, counted
once against the storage limit however many rows name them.

| Table | Holds |
| --- | --- |
| `installation` | One row: the installation ID (the `workspace_id` of the artifact API), the revision count (`generation`) and the run and suite ordinals |
| `artifacts`, `revisions` | Source artifacts, their heads and immutable revision chains; revision content is an object |
| `runs` | The run journal: bounded application metadata, terminal flag, generation, evidence and input-bundle digests; both payloads are objects |
| `evaluations` | Recorded assertion evaluations of a terminal run; result and private assertion source are objects |
| `suites`, `suite_members` | Suite envelopes and their member runs |
| `tombstones` | Deleted run and suite IDs and deletion times |
| `captures`, `capture_segments` | Capture metadata and the digest, size and encoding of each immutable segment |
| `audit_head`, `audit_events` | The audit log |
| `tokens`, `sessions` | API token and web session records, hashes only |
| `runtimes`, `default_runtime` | The runtime catalog (name, identity, image or built binary and the build that made it, and a built runtime's manifest) and the team's default runtime |
| `builds`, `build_inputs`, `build_log` | Runtime builds: queue entries a builder claims under a lease, the digests of their inputs (manifest and source archives are objects), and the last 2,000 log lines; a done build's binary is an object |
| `changes`, `change_floor`, `run_events`, `jobs`, `logins`, `workers` | Coordination between instances: the change feed, live events of unfinished runs, interactive jobs, sign-ins in progress and connected workers |
| `schema_migrations` | Applied migration versions, names and SHA-256 checksums |

Opening the store ([`Open`](../../internal/debugger/store/pg/store.go))
connects, checks that the bucket exists, applies pending migrations and reads
or creates the installation row. Migrations are numbered SQL files embedded in
the binary (`internal/debugger/store/pg/migrations/`), applied forward only in
one transaction under an advisory lock, so instances starting together apply
each once. The store refuses to start when an applied file's checksum changed
or the database is ahead of the binary. One PostgreSQL cluster serves the
debugger database `linea`; fleet uses its own database and role in the same
cluster and the two never share tables.

The server config's `store` block names the connection (`database_url_file`, a
file holding a `postgres://` URL) and the object storage (`objects`:
`endpoint`, `bucket`, `prefix`, `region`, `path_style`, `access_key_file`,
`secret_key_file`); see [team-server.md](../debugger/team-server.md). The client is the
AWS SDK for Go v2 with static credentials and checksums only where S3 requires
them, so AWS S3, Ceph RGW, SeaweedFS and other S3-compatible services work.
Self-hosted stores usually need `path_style: true`.

### Writes

A writer uploads its objects before it commits the row that names them, so a
row never refers to a missing object. A failed or refused write, and a deleted
run, suite or capture, can leave an object no row references. Objects stay
until `Sweep` deletes the unreferenced ones older than a given age (one day by
default); `Sweep` checks each batch against the rows under the
installation lock, so it never removes an object a writer is about to
reference. The upkeep leader runs it in `Housekeep`. Reads verify the stored
size and SHA-256 of every object; a missing or changed object is `ErrCorrupt`.

A build's binary is the one object written outside the installation lock:
`StoreBuildBinary` copies the staged upload to its blob key without the lock,
then, under it, checks the lease and the limit again, confirms the copy is
still there when no row references its digest (a sweep that listed an older
object of that digest may have deleted it), and records it. A copy that check
refuses is left to `Sweep`. `SaveRuntime` takes the lock too when an entry
names a build, and refuses (`ErrConflict`) an entry naming a binary that the
build, while the store still has it, no longer holds.

The copy is conditional on the ETag the store hashed
(`x-amz-copy-source-if-match`). SeaweedFS honors it (the store tests), and
the client's request and its mapping of `PreconditionFailed`, also inside a
`200` response, and `NoSuchKey` are tested against the S3 protocol itself
(`TestS3CopyIsConditional`). An object store that ignores the condition would
store a staged object rewritten between the hash and the copy; only the
builder holding the presigned upload URL can rewrite it.

### Staged objects

Workers move large payloads through object storage instead of the worker
connection, through `artifact.Transfers`. `RunBundleURL` presigns
a read of a run's input bundle, and `StageURL` presigns a write of one staged
object at `<prefix>/staging/<name>` (a name is lower-case letters, digits and
`-`, at most 128). `TakeStaged` reads the object, checks its size and digest
against what the worker reported, and deletes it, also when the check fails
(`ErrCorrupt`; a missing object is `ErrNotFound`). Staged objects sit outside
`sha256/`, no row references them and they never count against `Usage`. A
worker that dies before its result is taken leaves one behind; `Sweep` deletes
staged objects older than one hour, whatever age it is given for digests, and
needs no installation lock for them.

Every write that checks a count or storage limit, or adds bytes, takes the
`installation` row lock first (`SELECT ... FOR UPDATE`), in every instance, so
limits hold across instances and no two such writes interleave. Status updates
of an unfinished run, metadata-only capture updates and token and session
writes lock only their own rows. The audit sequence is gapless: an append
locks the `audit_head` row until it commits, so sequence order is commit order.
Optimistic generations, unique hashes and tombstones are row constraints.

### Run journal

A run record holds the run ID, admission ordinal, generation, terminal flag,
bounded application metadata (8 KiB), optional evidence digest and size, and
optional immutable input-bundle digest and size. `AdmitRun` uploads the bundle
and inserts the run in one transaction; later lifecycle writes preserve that
binding. A terminal write publishes the terminal flag, metadata and evidence
reference together. Terminal records cannot change; unfinished updates check
the generation. The repository does not interpret Collector reports or replay
work. The platform stores schema-2 evidence archives: engine report 0.5,
operational events and base64 original snapshots (avoiding JSON whitespace
changes to snapshot reads) as JSON, gzip-compressed with the decoded length in
a gzip extra subfield (`OD`, 8 bytes little-endian). The repository stores,
digests and limits the compressed bytes. Reads require one gzip member that
decodes to exactly the declared length (at most deflate's 1032:1 ratio of the
stored size) with no trailing data, or the archive is `ErrCorrupt`. Schema-1
archives, the same JSON uncompressed, stay readable and are told apart by the
gzip magic; a compressed schema-1 or uncompressed schema-2 archive is
unsupported.

Evaluations are the recorded assertion evaluations of a terminal run. A record
holds the evaluation ID (`eval-` and 16 lower-case hex digits), run ID, a
per-run ordinal (1..n in recording order), bounded application metadata
(caller, created time, outcome) and the digests and sizes of two immutable
blobs: the result and the private assertion source. `SaveEvaluation` checks
the per-run cap (`Limits.EvaluationsPerRun`, default 100), blob limits and the
storage limit under the installation lock before writing; a refusal
(`ErrLimit`) writes no row and never evicts. Recording never changes the
parent run's record, generation, evidence or bundle. `Evaluations` lists
envelopes in ordinal order; `EvaluationResult` checks the result's digest and
size. The store offers no source read to the platform's public API.

A suite is an immutable envelope `{id, ordinal, metadata}`: the ID is `suite-`
plus 16 lower-case hex digits, the ordinal is 1..n in publication order, and
the metadata JSON holds caller, creation time, config label and ordered
members with label and either run ID or preparation error. A suite has no
blobs, completion writes or coordinator state. `SaveSuite` inserts the suite
and its member rows once, after checking the metadata (one to 32 members, each
either a run of the suite's project that exists, or a preparation error). The
member rows let a run delete be refused while a live suite lists the run.

`Limits.Suites` defaults to 1,000. Suites are never evicted; at the cap,
publication returns `ErrLimit` and writes nothing. The platform reserves the
suite-count slot before admitting members (`CheckSuiteCapacity`), so a count
refusal admits nothing. A failure at publication, or a crash between member
admissions and suite publication, leaves ordinary member runs without a suite.

Runs are listed a page at a time, newest first (`ListRuns`). Each
run row carries an index, the run's scenario, runtime name and result, which
every publication writes and the filters match exactly. Pages are keyed on
the admission ordinal: a `RunPage` holds up to `RunQuery.Limit` records (50
by default, at most 500) and a `NextCursor` that encodes the last ordinal and
a digest of the query's projects and filters, so it never continues another
query. A run stored before migration 005 has no index (`indexed = false`)
until the platform sets it at startup with `IndexRun`; meanwhile it lists
unfiltered and matches no filter. `RunQuery.Unfinished` selects only runs
that are not terminal and `RunQuery.Unindexed` only runs without an index:
the platform opens and reloads on these instead of reading every run.

Deletion keeps tombstones: `tombstones` maps a run or suite ID to its deletion
time only. `DeleteRun` removes a terminal run that no live suite lists, with
its recorded evaluations; `DeleteSuite` removes a suite and its member runs
once every member is terminal. A queued or running run, or a suite member
deleted alone, is `ErrConflict` and nothing is written. While its tombstone
exists, an ID is never live again: admitting it is `ErrConflict`. Run and
suite ordinals come from counters on `installation`, so they stay unique
(gaps are allowed). Housekeeping (`TrimTombstones`) deletes a tombstone once
it is a day older than the oldest change the feed retains: every instance
has read the deletion or reloads without it. IDs carry 64 random
bits, so an expired tombstone's ID is not admitted again in practice.
Deletion needs no storage reservation, so an installation over its storage
limit can always be reduced. It removes rows only; the freed objects (evidence,
bundle, evaluation result and source) leave object storage at the next
`Sweep`, and stop counting against `Usage` as soon as no row references them.
Revisions and their run origins are never changed.

### Captures

A capture is a row with metadata (at most 256 KiB), a generation, a closed
flag and a memory-only flag, plus appended segments. Immutable segments use
gzip (best-speed) only when it saves bytes; small or incompressible segments
keep their original representation. Segment `digest` and `bytes` cover the
stored bytes; `encoding: "gzip"` and `uncompressed_bytes` declare compressed
segments. Capture-record `bytes` is the total uncompressed serialized size.
Reads verify the stored SHA-256, gzip checksum and exact decoded size with
bounded decompression; trailing gzip members or bytes and unknown encodings
are `ErrCorrupt`. API, CLI, MCP and web record reads and derivations receive
the original bytes transparently. Metadata-only updates keep the same segment
references.

A segment is uploaded and referenced in the transaction that accepts it.
Capture segments count in the same storage limit as source and run objects.
Each open, non-memory-only capture reserves the rest of a 16 MiB uncompressed
allowance (`artifact.CaptureAllowance`), including JSON and base64 overhead,
until it closes; compression does not raise capture admission or decoded
memory limits. At most 10,000 segments per capture and 1,024 captures
(`artifact.CaptureHistory`) are kept; captures have no retention period and
stay until deleted. A memory-only capture keeps its metadata but no payload;
recovery reports its payload loss explicitly. Fixture and scenario saves use
the existing source origins with `kind: capture` and a bounded derivation
document. See [livecapture](../debugger/livecapture.md) for limits, storage and deletion
behavior.

### Projects

Every artifact, run, suite and capture has a `project_id`: 1 to
64 lowercase letters, digits and hyphens, enforced by a column constraint. A
new artifact, run, suite or capture must name its project; an update cannot
change it, and a suite's member runs are in the suite's project. `List` takes
an optional project filter (nil lists every project); its cursor is bound to
the filter. The platform puts a new record in the caller's project, `default`
when the request names none.

The project set (`015_projects.sql`) names the projects beyond the built-in
`default` and `library` and holds what is shared between them:

- `projects`: a project's ID, name, archived flag and settings
  (`default_runtime`, `storage_limit`, `retention_runs`, `retention_age` in
  nanoseconds), with creator and last updater. A built-in project gets a row
  on its first change.
- `library_audiences`: a library artifact's audience, project IDs or `*`
  alone, with who set it and when. No row shares the artifact with no
  project.
- `runtime_availability`: the projects a runtime runs work for, IDs or `*`
  alone. No row makes it available to every project.
- `project_set`: one row, the set's revision, which every write to the three
  tables raises; its trigger records a `projects` change, so other instances
  reload the set.

Writes take the family's advisory lock and compare the caller's expected
revision (a stale one is `ErrConflict`). Bounds: 1000 projects
(`MaxProjects`), 10000 audiences (`MaxAudiences`), one availability per runtime
(`MaxAvailabilities`), and lists of at most 1000 IDs; beyond them a write is `ErrLimit`. The repository checks shape only
(`artifact.ValidProject`, printable bounded names); the platform checks that
listed projects exist. The set is outside `StorageBytes` and is not exported.
`Repository` includes it as `Projects` (`ProjectSet`, `SaveProject`,
`SetAudience`, `SetAvailability`), covered by the contract suite
(`contract/projects.go`).

### Audit log

The platform's audit events live in `audit_events`, ordered by a
gapless sequence number. A field holds at most 256 bytes without control
characters, an event at most 16 resource IDs. Retention keeps the newest
100000 events and 90 days (`Limits.AuditEvents`, `Limits.AuditAge`): reads
apply both exactly, an append deletes events beyond the count, and every 256th
append also deletes events past the age. The audit log is outside
`StorageBytes` and is not exported. `Repository` includes it as `Audit`
(`AppendAudit`, `AuditEvents`), and the contract suite covers order, filters,
retention and close.

### API tokens

`tokens` holds the API token records: a random 32-hex ID, the SHA-256 of the
secret in hex (never the secret), the principal, its kind (`user` or
`service`), display name and groups, a label, the creator, creation and expiry
times and an optional revocation time. A field holds at most 256 bytes without
control characters, a record at most 64 groups, the table at most 1000 records.
Each write drops records expired more than 30 days ago. It is outside
`StorageBytes` and is not exported. `Repository` includes it as `Tokens`
(`SaveToken`, refusing a duplicate ID or hash; `TokenByHash`; `Tokens` by
principal, newest first; `RevokeToken`), covered by the contract suite.

### Role bindings

`role_bindings` holds the role bindings a team server manages beside its team
file: a random 32-hex ID, the subject kind (`group` or `principal`) and
subject, the role, the scope, the creator and the creation time, unique per
subject, role and scope. `role_binding_set` holds one row, the set's revision,
which every write increments; its trigger records a `role_bindings` change, so
other instances reload the set. Writes take the `role_bindings` advisory lock,
compare the caller's expected revision (a stale one is `ErrConflict`) and
apply one insert or delete. A field holds at most 256 printable characters, the
role at most 64; the table holds at most 1000 bindings (`ErrLimit`). Bindings
read oldest first. The repository checks shape only; the platform checks roles
and scopes, and skips (and logs) a stored binding the policy no longer
accepts. It is outside `StorageBytes` and is not exported. `Repository`
includes it as `RoleBindings` (`RoleBindings`, `AddRoleBinding`,
`RemoveRoleBinding`), covered by the contract suite.

### Web sessions

`sessions` holds a team server's OIDC web sessions: the SHA-256 of the cookie
secret in hex (never the secret), the principal, display name and groups at
sign-in, and creation and expiry times, with the same field bounds as tokens.
It holds at most 10,000 live sessions, and each write drops expired sessions;
signing out removes one. It is outside `StorageBytes` and is not exported.
`Repository` includes it as `Sessions` (`SaveSession`, refusing a duplicate
hash; `SessionByHash`, expired ones included; `DeleteSession`, idempotent),
covered by the contract suite.

## Revision and operation contract

Workspace, artifact and revision IDs are independent opaque random identities;
the workspace ID is the installation's ID. The content SHA-256 enables
deduplication and does not identify an artifact or grant access. References
include all three IDs; a revision cannot be read through another workspace or
artifact. Returned content is a fresh byte slice.

`Save` accepts original bytes for `configuration`, `fixture`, `scenario` or
`setup`, including invalid drafts. Saving never reads or overwrites a source
file and performs no Collector validation or dependency capture. Updates require
the current artifact generation **and** head revision. Stale updates return a
`Conflict` containing current metadata; changing the document kind is refused.
The new revision points to its immutable parent; existing revisions remain
addressable. Saves advance the saved artifact's head. A saved candidate creates
a new configuration artifact and leaves the base artifact's head, generations
and bytes unchanged. Actor comes from the trusted application boundary, not
JSON input. One transaction inserts the revision, advances the artifact head
and generation, and increments the installation's revision count (the
workspace generation of the interface).

Revisions may carry an immutable `origin` supplied only by the platform.
Kind `candidate`: `base_digest` is the lowercase SHA-256 hex of
the original base bytes, `base_revision_id` is optional, and `edit` is optional JSON for a statement edit
(`processor`, `signal`, `group`, `index`, `op`, and `text`/`expected` when set).
Complete-YAML candidates omit `edit`; candidates forbid `run_id`.
Kind `run` requires `run_id` (`run-` and 16 lowercase hex digits, shape checked
without requiring a journal run) and `base_digest`, the run bundle's SHA-256
in lowercase hex without a `sha256:` prefix. It forbids `base_revision_id`
and `edit`. Scenario-from-run saves create new scenario artifacts, preserving
existing heads, source bytes and run evidence.
The store validates the kind, digest,
optional revision ID, and valid JSON edit of at most 8 KiB before any write.
Origin pointers and edit bytes are copied at
the write/read boundaries. HTTP save requests remain strict: an `origin` field
is 400 `invalid_request` and publishes nothing. Revision responses expose the
optional origin as additive metadata.
Revision generations establish order; wall-clock timestamps do not. A host clock
correction can make a later revision's timestamp earlier without corrupting history.

`Get` returns current metadata; `Revision` returns an exact revision and content.
`List` returns metadata in artifact-ID order with 25 items by default, at most
100. Its cursor names the last ID returned and binds the installation, kind and
project filter, so artifacts saved meanwhile never disturb a walk and a
cursor survives a restart. `Revisions` lists an artifact's revisions newest
first; its cursor names the generation the next page starts below and stays
valid while the artifact gains revisions.

Defaults are 16 MiB per source blob and per run archive, 64 MiB per encoded
input bundle, 10,000 revisions, 100 retained runs (in-memory store only),
100 evaluations per run,
1,000 suites and 256 MiB of stored objects (the server config's `storage_limit`
and `run_limit` set `StorageBytes` and `RunBytes`; they are never persisted
and never raised by a request; a run limit of at least 2 MiB and at most the
storage limit). Evaluation results use `RunBytes`; private assertion sources
use `BlobBytes`. Run metadata is limited to 8 KiB. There is no catalog size
limit: `Limits.CatalogBytes` applies to the in-memory store only.

`Usage` is the bytes of every distinct object any row references, which may
exceed a lowered limit. A write is refused with `ErrLimit` when the bytes it
adds (objects no row references yet, counted once per digest), plus the room
reserved for unfinished work, would exceed `StorageBytes`. Each unfinished run
reserves a full `RunBytes` allowance, released by its terminal write, each
open capture reserves the rest of its allowance, and each queued or running
build without a stored binary reserves `BuildBytes` (zero in the store's
defaults; a team server sets an eighth of its storage limit, at most
`artifact.BuildAllowance`, 128 MiB), released when it stores its binary or
ends. Because one admission
reserves `RunBytes`, a storage limit needs at least that much free for a run
to be admitted. Storage reservation can reject admission before the execution
queue fills. An installation over its limit opens and serves its runs and
sources but refuses every new save or admission without writing. Stored run
evidence and evaluation results are read against their recorded,
digest-checked sizes, so a run limit lowered below an existing run still opens
it; only new writes meet the current `RunBytes`. On PostgreSQL, admission
counts the objects of evictable runs (terminal, not kept, in no suite, the
origin of no artifact's head revision) as free, and has no run count limit:
retention (`Retention.EvictRuns`) removes the oldest evictable
runs by count, age and storage, so history never refuses a run. Its storage
measure is estimated per run and repeated until nothing is due.
`Retention.PruneRuns` removes the evictable runs a query selects (projects,
admitted before, index filters) in one transaction and reports the bytes of
the objects only they referenced; a dry run measures the same way and
commits nothing. `KeepRun` changes no generation; the change feed reports
the run so other instances read the mark. A save is
still refused under storage pressure, and revisions are never evicted.
Revisions are never deleted. These limits cover storage; Collector capture
memory and query response budgets are separate. The limits do not bound the object store's own size: objects a
deleted row left behind occupy the bucket until `Sweep` removes them.

Save outcomes are linearized by the database transaction. A save cancelled
before it commits can leave an unreferenced object but cannot advance a head.

## Verification

`artifact.Repository` (`Artifacts`, `Runs`, `Captures`, `Audit`, `Tokens` and
`Sessions`) is the storage the platform depends on. The shared
contract in `internal/debugger/artifact/contract` checks what every
implementation must do through the interface alone: exact revisions across a
reopen, conflicts, projects, filtered paging, runs with bundles and evidence,
10,000 runs paged by every filter combination,
evaluations, suites, deletion, captures, limits and refusal after `Close`.
`TestContract` in `internal/debugger/store/pg` runs it against PostgreSQL and
object storage; the in-memory store (`internal/debugger/store/memory`) passes
the same suite and backs platform and API tests. Another implementation passes
the same `contract.Run`.

The pg tests also cover concurrent saves with the same expected head, a
gapless audit sequence under concurrent appends, storage limits and suite
capacity across several stores on one database, the stable installation ID,
migration checksum refusal, forward-only and concurrent migration, `Sweep`
deleting only old unreferenced objects, objects counted once per digest and
presigned URLs. They need PostgreSQL and S3-compatible storage:

```bash
scripts/debugger/test-store.sh [-run TestContract] [-v]
```

starts disposable `postgres:17` and `chrislusf/seaweedfs:4.51` containers on
random localhost ports, runs `go test ./internal/debugger/store/pg` against
them and removes them on exit. Run it before a release
([development.md](development.md#before-a-release)). Tests
that need the containers skip without them. The private bundle format and Go
read/decode operations are documented in [execution-bundles.md](execution-bundles.md).

## Platform and HTTP interface

`platform.Open` starts a service on any `artifact.Repository` and owns it from
then on. `Service.Close` waits for repository operations before closing the
repository. `New` provides an ephemeral service without one. Source operations
use `SaveArtifact`, `GetArtifact`, `GetArtifactRevision` and `ListArtifacts`.
Actor comes from the trusted application boundary (the authenticated principal on a
server, `local-owner` for the CLI's direct commands). Client JSON cannot supply it.

Every operation that saves a configuration or scenario (`SaveArtifact`,
candidates, scenarios from runs or captures, copies and library publishing,
CI imports) saves it under a YAML file name (`yamlLabel` in
`platform/artifact_label.go`): a label ending in `.yaml` or `.yml` is kept,
any other gets `.yaml` appended (`config.bin` becomes `config.bin.yaml`), so
the CLI, HTTP, MCP and the web client name saved work alike. Fixtures and
setups keep their labels.

`linea-server` always runs a team server, so artifacts,
runs and captures are kept in PostgreSQL and object storage; there is no
in-memory server mode. The in-memory store backs tests only. On one machine
the server is the Compose stack ([deploy-compose.md](../debugger/deploy-compose.md)). The CLI's direct
commands (`test`, `run`, `inspect`, `ottl` without `--server`) keep no run
history.

Start the local server (the Compose stack, see
[deploy-compose.md](../debugger/deploy-compose.md)), then explicitly save a source
document:

```sh
url=http://127.0.0.1:7070/api/v1/artifacts
workspace=$(curl -s "$url" | jq -r .workspace.id)
curl -s -X POST "$url" -H 'Content-Type: application/json' -d "$(jq -n \
  --arg ws "$workspace" \
  --arg content "$(base64 < samples/logs-cleanup/config.yaml | tr -d '\n')" \
  '{workspace_id: $ws, kind: "configuration", label: "logs cleanup",
    expected_generation: 0, content_base64: $content}')"
```

The same in PowerShell:

```powershell
$artifactURL = 'http://127.0.0.1:7070/api/v1/artifacts'
$artifactPage = Invoke-RestMethod -Uri $artifactURL
$artifactBody = @{
  workspace_id = $artifactPage.workspace.id
  kind = 'configuration'
  label = 'logs cleanup'
  expected_generation = 0
  content_base64 = [Convert]::ToBase64String(
    [IO.File]::ReadAllBytes('samples/logs-cleanup/config.yaml'))
} | ConvertTo-Json
$savedArtifact = Invoke-RestMethod -Uri $artifactURL -Method Post `
  -ContentType 'application/json' -Body $artifactBody
```

| Endpoint | Result |
| --- | --- |
| `POST /api/v1/artifacts` | `201 Saved`: create or advance a head with expected generation/head; `Location` names the immutable revision |
| `GET /api/v1/artifacts?kind=&limit=&cursor=` | `Page` including workspace identity and a bounded metadata list |
| `GET /api/v1/artifacts/{artifact}` | Current metadata including generation/head |
| `GET /api/v1/artifacts/{artifact}/revisions/{revision}` | Exact metadata and base64 original bytes, including invalid drafts |
| `GET /api/v1/artifacts/{artifact}/revisions?limit=&cursor=` | `RevisionPage`: the artifact and its revision metadata newest first, without content |
| `POST /api/v1/artifacts/export` | `{configuration_id, scenario_ids}` → `collector-ci.zip` (`application/zip` attachment), the Export for CI archive |
| `POST /api/v1/artifacts/import` | `{archive_base64}` → `ArtifactImport`: each archive file `created`, `revised` or `unchanged` |

An update supplies `artifact_id`, `expected_generation` and
`expected_head_revision_id` from its selected base. `409` includes the current
metadata for stale edits. Operation errors have `code` and `error`:
`invalid_request` (400), `not_found` (404), `conflict` (409),
`resource_exhausted` (413), `storage_error` (500), `workspace_required` or
`unavailable` (503). Raw content responses use `Cache-Control: no-store`; artifact
metadata responses also disable caching to avoid stale heads.

HTTP requests keep the existing 16 MiB total document bound; base64 overhead means
an HTTP source payload cannot use the full 16 MiB repository blob limit. These
operations save supplied bytes and never import a local path. The web client's
saved work and `linea artifacts` (`list`, `show`, `save`, `revisions`, `export`,
`import`) use them. Saving an artifact does not make it a reproducible
execution bundle. Runs persist
independently of named artifact saves; quick mode still needs no scenario.

Live capture uses linea-fleet's credentials and groups. Capture display
names/descriptions live in the capture metadata with their own revision; an
update appends no payload segment and does not alter retention.
