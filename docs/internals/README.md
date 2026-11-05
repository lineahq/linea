# Internals

Developer documentation, not user documentation. User documentation lives in
`docs/debugger/`, `docs/fleet/` and `docs/collector-components/`; the rules for this
repository are in `AGENTS.md`.

Update these notes in the same change as the code that depends on them. When a
finding is disproven or a Collector upgrade changes it, correct the note
rather than appending a contradiction.

## Debugger design and development

| Document | Topic |
| --- | --- |
| [architecture.md](architecture.md) | Package map: what each debugger package owns and the contracts between them |
| [cli-and-server.md](cli-and-server.md) | Which binary links the Collector, `engine.Use`, the link boundary test, the built-in runtime on workers, the CLI on its own and `linea build` |
| [development.md](development.md) | Every build, test and Docker command, versioning, and the full testing guidelines |
| [artifact-storage.md](artifact-storage.md) | The storage contract, PostgreSQL and object-storage layout, run publication and limits |
| [execution-bundles.md](execution-bundles.md) | Durable private run inputs, exact-build compatibility, retrieval and reproduction limits |
| [environment-bindings.md](environment-bindings.md) | Trusted environment capture policy, declared/default/host bindings and replay authorization |
| [path-policy.md](path-policy.md) | Every request-triggered host read and where the allowed-roots path policy is enforced |
| [runtime-prerequisites.md](runtime-prerequisites.md) | Reference transport preflight, mode-aware dependency boundaries and remaining component classification |
| [scenarios-and-api.md](scenarios-and-api.md) | Scenario schema v1 (inline inputs, timeline waits, timestamp rebasing), text sources, the HTTP API, SSE and its OpenAPI contract |
| [run-lifecycle.md](run-lifecycle.md) | Run status, cancellation, pipeline-order canonicalization, normalized reports, provenance |
| [assertions.md](assertions.md) | Assertion schema, targets, Pass/Fail/Inconclusive/Error rules, the test command, JUnit mapping |
| [mocking.md](mocking.md) | Mock providers, the scenario `mocks` section, filelog sandboxing and timing, normalizing mocked runs |
| [topology-and-compatibility.md](topology-and-compatibility.md) | Static topology, execution modes, compatibility diagnostics, effective config |
| [stage-analysis.md](stage-analysis.md) | Event model, stage outcomes, structural diffs, content identity for logs and metrics |
| [record-objects.md](record-objects.md) | pdata record objects: which operations keep them, how the recorder numbers them, pooling and limits |
| [fleet-config-layers.md](fleet-config-layers.md) | How fleet assembles an agent's configuration from its config layers, and what the config bisect rebuilds when it probes one version |
| [runtime-builds.md](runtime-builds.md) | The debugger against older Collector releases, building every component in Docker, offline subset builds, container round trips, link-time versions, deprecation warnings |
| [web-client.md](web-client.md) | Web client architecture, embedding, offline Monaco, editor diagnostics, graph layout, runs, telemetry inspector, OTTL debugger, scenario form, samples and quick input, and the interface rules |
| [debugger-team-server.md](debugger-team-server.md) | Team server mechanisms: authorization, storage, runtime catalog, worker connections, instances, fleet attribution, sign-in |
| [debugger-workers.md](debugger-workers.md) | Worker protocol, pool workers, builders, local builds, manifests and runtime storage |
| [debugger-livecapture.md](debugger-livecapture.md) | Live capture in the debugger: fleet connection, discovery, bounds, states, storage and derived inputs |
| [debugger-provider-sdk.md](debugger-provider-sdk.md) | Provider SDK design, module releases, runtime behavior, discovery and reports |
| [debugger-kubernetes-chart.md](debugger-kubernetes-chart.md) | Helm chart implementation: pods, certificates, network policies, builder autoscaling, bundled stores |

## Collector internals

Durable findings about the OpenTelemetry Collector internals this project
relies on. Record facts that are expensive to rediscover: API shapes at the
pinned release, behaviors verified by experiment, and pitfalls. Every
Collector note names the release it was verified against (currently `v0.162.0`
/ `v1.68.0` for stable modules).

| Note | Topic |
| --- | --- |
| [collector-embedding.md](collector-embedding.md) | Running `otelcol.Collector` in-process; lifecycle, telemetry, isolation |
| [factory-decoration.md](factory-decoration.md) | Wrapping processor/receiver/exporter factories without changing semantics |
| [config-resolution.md](config-resolution.md) | Resolving unchanged YAML through the Collector; sealed file/env providers shared by planning and execution |
| [components.md](components.md) | Supported component set, its conformance tests, component behavior, inert extensions and timer-driven components |
| [reference-runs.md](reference-runs.md) | How the uninstrumented reference Collector run is built and compared |
| [aggregation.md](aggregation.md) | Count connector semantics, its verified replay, lineage across an aggregation, metric sorting |
| [routing.md](routing.md) | Routing connector semantics, replaying the real connector to attribute each record to a rule, verification |
| [sampling.md](sampling.md) | Recording tail_sampling decisions with its hooks, replaying policies to explain them |
| [ottl-replay.md](ottl-replay.md) | How the transform processor executes OTTL and how replay mirrors and verifies it |

## Fleet

| Document | Topic |
| --- | --- |
| [fleet-server.md](fleet-server.md) | Replicas and presence, write path and admission, database roles and migrations, restore, sizing, metrics |
| [fleet-plugin-host.md](fleet-plugin-host.md) | Plugin readiness and epoch, lifecycle store, ingress, compatibility |
| [fleet-config-management.md](fleet-config-management.md) | Reconcile and send paths, send rule timing, measurements |
| [fleet-livecapture.md](fleet-livecapture.md) | Livecapture plugin authority, credential checks, delivery and reservations, metrics snapshots |
| [fleet-verification.md](fleet-verification.md) | Source development and acceptance scripts for fleet, plugins and config management |

## Collector components

| Document | Topic |
| --- | --- |
| [collector-components-compatibility.md](collector-components-compatibility.md) | The components on Collector releases after 0.162.0 and the `statusutil` split |
