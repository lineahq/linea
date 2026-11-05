# Captured execution bundles

Verified against Collector `v0.162.0` / confmap `v1.68.0` (2026-10-12).
This preserves accepted inputs; it does not establish complete reproduction of
every component's runtime dependencies.

`engine.Prepare` captures quick and scenario drafts without named saves. The
platform encodes a private bundle before admission (schema 5 for the built-in
runtime, schema 6 for another; see below). The repository
stores the immutable bundle and inserts the queued run record that references
its digest and length in one operation (in the PostgreSQL store, one
transaction that also uploads the object). Terminal publication, queued
cancellation and recovery of runs interrupted by a restart preserve that
reference. An orphan object is never adopted as a run.

The bundle retains:

- Sealed config snapshot: original root, logical provider URIs and exact
  file/env retrieval bytes/types, including defaults and empty/unset values.
  Snapshot schemas 2 and 3 also record capture policy and binding origins;
  schema 1 stays readable.
- Original fixture/scenario/setup bytes, optional quick-run assertion bytes and
  scenario input, file-mock and Kubernetes manifest files. File roots reference
  the captured map without duplicating bytes. Original path labels and absolute
  scenario base are preserved.
- Explicit quick/scenario mode, receiver, client address, comparison and
  normalization options. Timeline, assertions, environment and mock settings
  stay in the original scenario/setup YAML; quick assertions have a separate
  `assertions_base64` source. All are decoded fresh for execution. Setup still
  refuses assertions.
- Product, Collector, Go, report/event/inspection schemas, component modules,
  feature gates, OS/architecture and the semantic build identity (main module,
  VCS revision and dirty flag, non-VCS build settings and a digest of the linked
  module set). The executable's SHA-256 is recorded only as information. Runtime
  run IDs, observers, loggers and creation times are excluded.

Equal captures, labels, settings and builds encode identically. The whole bundle
has a SHA-256 identity; no individual secret hashes are published. `Run.bundle`
exposes only schema, digest, encoded size, mode, reproduction status/reason and
optional environment capture policy. Schema-1 snapshots omit the latter.
Engine reports preserve API/CLI report parity. Quick-run assertions add an
optional `provenance.assertions` source hash and use the existing `assertions`
report field; reports without assertions retain their existing bytes.

## Reading and explicit execution

`Service.GetRunBundle(ctx, runID)` is an explicit private-source Go operation.
It returns copied bytes for retained ephemeral runs or verified repository bytes
for runs in a repository (the PostgreSQL store checks the object's digest and
size on every read). Unknown runs return `ErrUnknownRun`; runs without a
captured bundle and source-free injected test executors return
`ErrBundleUnavailable`. Named source revisions remain separate. No HTTP bundle
download/import endpoint or CLI export command exists; those interfaces need an
explicit export/access policy.

The archive contains private source, fixture and environment values. Linea does
not encrypt it. In a team server it is an object in the private object-storage
bucket, named by its SHA-256 digest; the run's PostgreSQL row holds only the
digest and size. Reads go through the platform, which authorizes the caller on
the run's project (`GetRunBundle` needs run read access and is audited). Bundles
never appear in run metadata, list operations or exports. Keeping the bucket
private and its credentials restricted is the operator's job. Presigned object
URLs for a run's bundle go only to the workers that execute it
(`RunBundleURL`, [artifact-storage.md](artifact-storage.md#staged-objects));
no client receives one.
Treat a bundle like source/secret data; never include its contents in metadata
or logs.

`engine.InspectBundle` validates the envelope/config snapshot and computes identity
without source-file reads or requiring the original executable. Recovery uses it
to verify metadata bindings. Upgrading the debugger does not prevent reading stored
bundle bytes/history of the known schema.

`engine.ReadBundleParts` uses the same envelope/config validation to return
private captured quick/scenario sources, receiver/client address, absolute
scenario directory and dependency map without distribution checks. File-backed
roots are resolved from that map, with no host reads. `HostEnvironment` contains
only sorted, unique config binding names requiring host authorization, including
snapshots of unknown origin, never values or URIs. `CreateScenarioFromRun` uses these
parts to derive verified scenario YAML after upgrades; it does not decode a
request for execution. Parts remain private sources, never run metadata or logs.

Livecapture-derived setup/scenario documents may include bounded
`capture_origins` metadata: admission-time source identity/versions, sensitivity
classification, source/fixture digests, exact selection and destination mapping.
The existing private source bytes retain it through admission, restart, input
retrieval and scenario export, including unsaved quick inputs. No bundle format
change is needed. Resolving a saved capture fixture copies its origin into
derived quick setup before admission. It is declared lineage, not transport
authorization or production proof; the recorded fixture digest continues to
identify the original derivation after an input is edited. Fixture JSON requires
its companion setup to carry origins.

`engine.DecodeBundle(ctx, bytes)` requires a matching build identity, versions,
architecture and feature gates. It validates and reconstructs a prepared request
from captured sources. Explicitly submitting it creates a new run ID and cannot
overwrite the original. `engine.Execute` rechecks compatibility before resolving
config or starting a Collector. Frozen config/input readers refuse uncaptured
dependencies. Unknown schemas, malformed/mixed modes, missing root references,
source bounds and incompatible builds fail explicitly.

Bundle schemas 2 and later compare the semantic build identity read from the Go build
info, not the executable bytes, so a rebuild of the same source and dependency set
replays its own history; a different revision, dirty flag, build setting or
module set is rejected as `ErrDistribution`, never silently attempted. Limitation:
unstamped development builds (`go run`, `go test`) and dirty trees carry no
distinguishing revision, so source edits that keep the same dependencies share
an identity. Release builds should be stamped from a clean tree.

Schema 3 adds separately captured quick-run assertions; scenario assertions
remain in their original scenario YAML. Schemas 1 and 2 cannot carry separate
assertion bytes and reject them as `ErrBundleSchema`. Both older schemas still
decode and re-encode to their own bytes.

Schema 4 records the run's limits (`request.limits`: execution deadline,
evidence bytes, events), the platform's budget policy at admission. They are
safe metadata (`BundleInfo.Limits`) and travel with the request when the bundle
is decoded, so an explicit execution of a captured bundle runs under the
limits it was admitted with. The bundle records the policy, not the effective
deadline: that also depends on the scenario's declared `execution.timeout`
and is recorded, with any clamp, in the report's `limits`. Schema-4 limits
must be present, canonical and without clamps; older schemas carry none and
reject them as `ErrBundleSchema`.

Schema 5 records how inline sources were named and where they came from:
`request.config_label`, `fixture_label` and `scenario_label` (provenance paths
for inline text), and `request.sources`, the saved revisions (`artifact_id`,
`revision_id`) the platform copied the config, fixture or scenario from. The
captured bytes remain the source of truth; the references are safe metadata
(`BundleInfo.Sources`, IDs only) and the engine never resolves them. A
reference describes inline text only. Older schemas carry neither and reject
them as `ErrBundleSchema`.

Schema 6 records the target runtime (`runtime`: `id`, the runtime identity
a worker announces, and `name`, how the debugger knows it). The
platform writes it only for a run on a built or connected runtime; built-in bundles
are still schema 5, byte for byte. A worker accepts only a bundle naming its
own runtime (`DecodeBundleForRuntime`) and then skips the distribution
comparison, since its build is the target; the in-process engine refuses a
bundle that names a runtime (`ErrDistribution`, `unsupported_build`). A
schema-6 bundle without a runtime, or an older schema with one, is
`ErrBundleSchema`.

Schema-1 bundles (written before the build identity) stay readable and keep their
original rule: execution requires the exact executable digest, which must be
readable. They re-encode to their own bytes, so stored bundle metadata still
verifies on recovery. Cross-version replay and portable CI execution remain later
work. Same-build input replay does not promise deterministic output from clocks,
timers or components.

## Reproduction status and bounds

Current bundles say `unverified`, because component runtime file/environment
dependencies are not fully classified. A TLS certificate path in a component
config, for example, is not a `file:` config-provider retrieval. Reference OTLP
transports can instantiate those upstream settings. The runtime prerequisite
check [rejects known reference transport
prerequisites](runtime-prerequisites.md) before admission, including TLS files
and explicit HTTP proxies; substituted instrumented transports remain usable.
Remaining component dependencies are reported as stated gaps rather than
rejected; full classification is deferred and is needed only before claiming
`self_contained` or enumerating `requires_external_values`. The explicit
[environment binding policy](environment-bindings.md): core services default to
declared values/defaults and host imports require authorization. Local commands
import a host variable only for `--import-env`, and `linea-server` reads none; frozen config providers never consult the environment.

Source limits remain separate: 16 MiB/128 config-provider URIs and 16 MiB/128
absolute non-config file paths, with inline sources sharing the latter byte budget.
Encoded bundles are additionally capped at 64 MiB, including JSON/base64 overhead.
`artifact.Limits.BundleBytes` of the repository can impose a smaller limit; raising it
cannot override the engine cap. Bundle bytes count against the repository's
storage limit (`artifact.Limits.StorageBytes`, 256 MiB by default) while
terminal evidence reservations remain intact. Limits reject admission without a
visible run. The scheduler executes a run again from
its bundle only after a restart or an ended lease
([run-lifecycle.md](run-lifecycle.md#run-states)). A bundle goes when
its run is deleted (by hand, by retention or by prune); `Store.Sweep` removes
unreferenced objects older than a day, and the instance that leads the shared
repository's upkeep runs it every five minutes.

These are storage/source bounds, not decoded-memory or CPU budgets. Ephemeral
history and the in-memory store retain bundles in memory; recovery validates
archives sequentially and later retrieval reads from the repository. Finer
resource/query budgets remain open. Capture is not a global atomic host
transaction.
