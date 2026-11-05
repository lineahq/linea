# Optional livecapture server plugin

Optional server service for the existing Collector livecapture extension's
gRPC control v1 and OTLP/gRPC delivery. All capture behavior and storage live in
this package. It imports fleet's public extension SDK only, never core internals
or the debugger.

See the [installation, public API and recovery runbook](../../../docs/fleet/livecapture.md) and the
[OpenAPI schema](openapi.json).
Plugin 0.2.0 [metrics](../../../docs/fleet/operations-metrics.md#livecapture-metrics),
[paging](../../../docs/fleet/livecapture.md#peer-discovery-paging) and
[upgrade](../../../docs/fleet/livecapture.md#rotation-backup-restore-and-upgrade) extend
that contract without changing capture ownership or execution recovery.
Plugin 0.3.0 active/standby supervision and drain
preserve single-authority fencing and no replay, with API schema 1.2.0.
Plugin 0.4.0 (API schema 2.1.0, plugin API version 2) uses only generic fleet
host services: Collectors present their fleet agent credential, which the
plugin introspects through the host (`credentials.introspect`); its `capture`
label lists the collector groups it may serve. Peers that report the same pod
identity over OpAMP are linked to fleet agents (`agents.read`), so captures
can target fleet agents, groups or selectors. Plugin deployment tokens and
revocations are gone; revoke the fleet credential instead. API schema 2.2.0
records fleet's acting-for headers on captures (`acting_for`, plugin
migration 3) and refuses a busy Collector with the `collector_busy` code.
API schema 2.3.0 shows each peer's reported `filter_limits` and refuses a
filter larger than a target Collector compiles.
Docker Compose is the deployment path.

The service uses a separate PostgreSQL database, explicit checksummed migration
and restricted runtime role. A pinned advisory lock permits one active authority.
Operation metadata and target reservations survive crashes; raw/captured payloads
never enter storage. Interrupted executions never resume and uncertain leases
remain reserved through the original deadline plus a five-second expiry guard.

Focused development checks from the repository root:

```powershell
$env:GOMAXPROCS='2'
go test -p 1 ./internal/fleetplugins/livecapture/...
go vet -p 1 ./internal/fleetplugins/livecapture/... ./cmd/linea-fleet-livecapture ./cmd/linea-fleet-livecapture-client ./cmd/linea-fleet-livecapture-fixture
./scripts/fleet/livecapture/test-postgres.ps1
```

The last command creates/removes its own resource-bounded PostgreSQL fixture;
never supply a user's database to tests. `-Race` selects the Linux race runtime.
No full-suite, load or HA claim is implied.
