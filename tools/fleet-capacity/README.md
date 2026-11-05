# Docker capacity measurement harness

`run.sh` (Linux and macOS) and `run.ps1` (Windows) provision a unique disposable Compose project and its own private DB,
roles and deployment token. It uses pinned official Collector Contrib 0.162.0
processes, not simulated connections. Replicas share a bearer token and register
automatically. Locally assigned UUIDv7 identities stay stable across restart so
the reconnect phase can verify generation advancement without inventory churn.

For many connections on one development machine, use the separate
[lightweight OpAMP simulator](simulator.md). It places independent mock sessions
in one bounded generator container and verifies the same public server inventory.

## Shared-host functional checks

A POSIX shell with curl and python3 (`run.sh`) or PowerShell 7 (`run.ps1`), Docker Compose v2, Linux containers. Build the selected fleet image
with `--build` (`-Build`) or reuse the already built `local` image:

```sh
./tools/fleet-capacity/run.sh --functional-check --build
./tools/fleet-capacity/run.sh --functional-check --with-capture-fault
```

```powershell
./tools/fleet-capacity/run.ps1 -FunctionalCheck -Build
./tools/fleet-capacity/run.ps1 -FunctionalCheck -WithCaptureFault
```

The first mode fixes the count to two official Collectors and the steady phase
to 35 seconds. The second fixes it to one official reporting Collector plus one
unchanged livecapture Collector, still two running Collector processes total.
It additionally needs the optional plugin/generator images and the source-built
`linea-fleet-livecapture-compat:collector` image from
`scripts/fleet/livecapture/smoke-docker.sh` (`.ps1` on Windows). Use `--build` (`-Build`) to build fleet/plugin/
generator sequentially; that flag does not build or change Collector source.
`-CaptureCollectorImage` can select an explicitly prepared compatible image;
its actual image identity is recorded.

The capture fault opens a real public capture, polls its channel but deliberately
withholds ACK, and samples bounded queue occupancy. Core inventory/readiness
requests must succeed while a batch is pending, presence stays intact, and a
missing receipt must become visible uncertainty followed by withdrawn execution.
The driver discards the synthetic envelope and saves aggregate metrics only.
This phase establishes failure isolation, not livecapture throughput capacity.

Do not run many-process Collector load on a shared or constrained machine. The
bounded mock-session profiles are fine there. These functional checks test the
harness itself; their elapsed times are not production capacity.

## Dedicated-host profiles

Running without either execution-mode switch (`--functional-check`, `--dedicated-environment`) refuses before Docker, filesystem
writes or secret generation. On an agreed, otherwise suitable dedicated host:

```sh
./tools/fleet-capacity/run.sh --dedicated-environment --collectors 100 --duration-seconds 120 --build
./tools/fleet-capacity/run.sh --dedicated-environment --collectors 100 --duration-seconds 120 --with-capture-fault
# Only after reviewing the 100-Collector evidence and machine budget:
./tools/fleet-capacity/run.sh --dedicated-environment --collectors 1000 --duration-seconds 600 --with-capture-fault
```

```powershell
./tools/fleet-capacity/run.ps1 -DedicatedEnvironment -Collectors 100 -DurationSeconds 120 -Build
./tools/fleet-capacity/run.ps1 -DedicatedEnvironment -Collectors 100 -DurationSeconds 120 -WithCaptureFault
# Only after reviewing the 100-Collector evidence and machine budget:
./tools/fleet-capacity/run.ps1 -DedicatedEnvironment -Collectors 1000 -DurationSeconds 600 -WithCaptureFault
```

These are prepared profiles, not results already achieved. Increase only after
reviewing preceding results. The count is bounded at 1,000. Every reporting
Collector has 0.25 CPU, 128 MiB, 64 PIDs and one Go processor; fleet has two CPUs /
512 MiB and PostgreSQL one CPU / 512 MiB. Compose startup parallelism is four.
The optional capture component adds one CPU / 256 MiB, plugin two CPUs / 512 MiB
and its DB one CPU / 512 MiB. These are caps, not reserved CPU shares.

Dedicated runs reject daemon memory below `count * 128 MiB + 2 GiB`, or +3 GiB
with capture fault. This conservative sum-of-limits check does not guarantee
available memory; allow host/other-service headroom independently. Record the
daemon/hardware and avoid sharing it with unrelated workloads. There is no
automatic progression from 100 to 1,000.

`--image-tag` (`-ImageTag`) selects the fleet, plugin and generator images,
default `local`. `--build` (`-Build`)
builds source with pinned bases and bounded Go compilation. Collector source and
image must be available separately for the optional fault. No host Go runtime.

## Measurements and output

The harness measures total registration time (including bounded Compose startup),
current inventory across all pages, at least one additional report per Collector
over the steady phase, zero report-processing errors, stable connection counts,
and reconnect of the first 10% (minimum one), requiring generation advancement.
It does not force the official Collector to report faster than its normal
heartbeat. No pipeline workload or telemetry throughput is inferred.

Each run writes to ignored private `.local/fleet-capacity-<id>/`:

- `result.json`: functional/dedicated mode, pass/fail, counts, phase times,
  report/error deltas, reporting count and total Collector processes, driver HTTP p50/p95/max, source and harness checksum,
  daemon CPU/memory/version, actual fleet/plugin/input image IDs and pinned
  Collector image reference. HTTP samples include setup, probes, metrics and
  API reads; they are driver-observed request times, not an isolated endpoint benchmark.
- `registered.prom`, `steady.prom`, `reconnected.prom`, optionally
  `consumer-fault.prom`: core histogram/counter snapshots, including ownership/
  pool waits and processing duration. Use deltas within one process lifetime.
- Matching `.resources.jsonl`: Docker's point-in-time fleet and core DB CPU/RSS/
  network statistics. These snapshots are not peak-resource measurements; review
  daemon monitoring as well before choosing operating limits.
- Optional `plugin-consumer-fault.json`: aggregate delivery/queue/receipt metrics,
  without raw envelopes or filters. Optional phase metadata records successful
  core reads following a fresh pending-batch observation and maximum observed
  pending bytes. Cached/stale metadata does not qualify that assertion.

API bodies/config/secrets and raw telemetry are never printed. The directory is
private because it also holds generated credentials; publish only reviewed result
and aggregate metric/resource files. Inspect result status and phase gates, not
only a successful container launch. A failed run is not a supported capacity.

Cleanup removes only this project's containers, networks and volumes, preserving
private result files. No external/user DB is used, and no user networks are pruned.
Fixtures use explicit `10.245.*` subnets to avoid exhaustion of Docker's default
pools; `-NetworkOctet` selects a different unused `10.<octet>.*` range. Check for
conflicts with your Docker/VPN networks. A conflicting subnet fails visibly.
Concurrent profiles must use different network octets and machine budgets.

The separate full real-capture proof uses compact `10.244.240.*` fixture subnets,
also configurable with `-NetworkOctet`. It covers all signals, partial receipts,
overlap, stale generations, lifecycle, credential revocation, crash and restore.
Run tests sequentially. [Metric semantics and effective limits](../../docs/fleet/operations-metrics.md)
explain what the snapshots mean.

The many-process Collector profiles above are opt-in. No results from them are
published.
