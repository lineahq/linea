# Component runtime prerequisites

The first boundary covers reference OTLP transports. Verified against
Collector `v0.162.0`, configtls `v1.68.0` on Windows, 2026-10-12. This is a partial
classification; bundles continue to declare reproduction `unverified`.

## Reference transport preflight

The instrumented run substitutes OTLP receivers and network exporters. Their
transport settings are validated but their TLS files and proxy connections do
not run. Reference comparison starts real OTLP transports, with endpoints
redirected to plaintext loopback listeners and sinks. These paths have different
runtime prerequisites even when they share the same captured configuration.

`runtime.CheckReferencePrerequisites` checks resolved upstream config types for
receivers/exporters used by pipelines, including deprecated type aliases. It does
not inspect arbitrary strings for possible paths. Connectors and unused
transports are excluded. Error selection is stable by component ID and setting.

| Used reference component | Setting | Current behavior |
| --- | --- | --- |
| OTLP receiver | `protocols.grpc.tls`, `protocols.http.tls` | Reject configured TLS, including inline credentials: the reference client/sinks use plaintext. Both configured server protocols start even when input uses only gRPC. |
| OTLP gRPC/HTTP exporter | `tls.ca_file`, `tls.cert_file`, `tls.key_file` | Reject uncaptured files before component creation, including paths that an insecure transport might otherwise ignore. No file probing or temporary certificate copy. |
| OTLP gRPC/HTTP exporter | `tls.include_system_ca_certs_pool`, enabled `tls.tpm` | Reject host trust-store/device prerequisites. |
| OTLP gRPC exporter | `tls.ca_pem` | Reject: upstream enables TLS when a CA is present even with the reference `insecure: true` override; the sink uses plaintext. |
| OTLP HTTP exporter | `proxy_url` | Reject: an explicit proxy can redirect loopback delivery outside the local sink. |
| OTLP gRPC/HTTP exporter | `auth` | Reject: authenticator extensions do not run, so the reference exporter cannot authenticate. |
| OTLP HTTP exporter | Inline TLS PEM without external settings above | Permit: values are captured in the private config snapshot. Upstream still validates/parses them during startup. |

The public Go error `ReferencePrerequisiteError` identifies component kind, ID,
setting and reason, and wraps `ErrReferencePrerequisite`. It omits setting values,
paths, PEM data and proxy addresses. The caller can choose execution without
reference comparison; this does not verify the original transport or promise
complete runtime isolation.

Default platform preparation applies the check when `Compare` is requested,
before a run ID, bundle publication or history entry exists. Prepared and decoded
requests are checked too. Direct `runtime.RunReference` calls enforce the same
boundary before mock providers, reference listeners or sinks start. Ordinary
instrumented runs retain these configs without reading transport files.

HTTP returns the existing `422` error response. CLI `run --compare` returns exit
code 1 without a report. Both quick and scenario modes use this same path, without
requiring saved artifacts or new request fields. No report, bundle, OpenAPI or
storage schema changes are needed.

Comparison failures are explicit admission errors, not delayed startup/transport
errors. TLS settings that the reference run would ignore and explicit local
proxies are conservatively refused for comparison. User YAML is preserved; we do
not silently clear credentials or proxy settings to make a run succeed.

## Evidence and remaining classification

Pinned `configtls.Config.loadTLSConfig` loads CA material and certificates,
including reload support; `loadCertificate` can open a TPM. For clients,
`LoadTLSConfig` returns early only when insecure and no CA is present.
`confighttp.ClientConfig.ToClient` loads TLS config even for a plaintext URL, and
applies an explicit proxy independently of the destination endpoint. Redirecting
an endpoint alone therefore does not seal these prerequisites.

Existing captures and mocks cover different boundaries:

| Boundary | Implemented control | Remaining work |
| --- | --- | --- |
| Config `file:` / `env:` providers | Immutable provider snapshots and explicit environment policy | Broader platform access/resource policy |
| Fixture/scenario/setup/mock source files | Bounded capture before admission | Decoded-memory/CPU budgets |
| Filelog input files | Per-run sandbox populated from captured mock fixtures | Report stanza expressions that read the host (`env()`) as an `unverified` reason |
| Kubernetes API | In-process fake API and redirected authentication/address | Report `filter.node_from_env_var` as an `unverified` reason, or supply the node through the mock |
| Reference OTLP transports | Preflight above | Remaining transport settings and dependency reachability |
| Processors/connectors, analysis replays | Pinned real factories, recorded evidence and declared timing/mock limits | Complete transitive runtime dependency classification |

Concrete follow-up evidence: k8sattributes `v1.1.0` reads the environment both in
`FilterConfig.Validate` and `withFilterNode` for `node_from_env_var`. The stanza
`v0.162.0` expression helper exposes `os.Getenv`. These reads are outside confmap
environment bindings; scenario/setup `environment` is not a process-wide sandbox.
The reachable operator set and other transitive component dependencies still
need verification. No `self_contained` classification or complete
host filesystem/environment/network isolation is claimed by this preflight.

Further classification is deferred. A component's direct host read becomes a
stated `unverified` reason or is supplied by a mock or declared binding. It is
never a reason to reject an unmodified configuration that the instrumented run
can execute; rejection stays limited to the reference comparison boundary above.

Regression coverage checks used/unused transports, aliases, stable diagnostics,
absence of private setting values, direct runtime calls, decoded bundles,
run admission and real substituted execution in both input modes. HTTP and
CLI tests cover the same rejection and instrumented fallback behavior; existing
full-suite comparisons retain reference fidelity for supported configurations.
