# Configuration environment bindings

Verified 2026-10-12 against Collector `v0.162.0` and
envprovider/confmap `v1.68.0`. This policy governs configuration-provider
retrievals. Component runtime environment and file reads are covered by
[runtime prerequisites](runtime-prerequisites.md).

## Application policy

`platform.Options.Environment` is trusted application policy. Its zero value
allows only scenario/setup `environment` values and explicit `env:` URI defaults.
An undeclared reference without a default never reads the host: it
resolves to the stand-in string `linea-stand-in-<hash>` (reason
`unbound`), reported in the run's `references` and as an inspection warning
`reference_stand_in`. An unrelated process variable cannot override a declared
default under this policy.

Quick mode still accepts config, payload and optional setup in one action. For
example, `SetupYAML: "environment: {DEPLOY_ENV: test}\n"` supplies a binding without
authoring a scenario or saving named artifacts. Scenarios use their existing
`environment` map. Neither mode mutates process environment to apply these values.

An application can explicitly import named host variables:

```go
service := platform.New(platform.Options{
    Environment: config.EnvironmentPolicy{Host: []string{"DEPLOY_ENV"}},
})
defer service.Close()
```

The service clones and canonicalizes the allowlist at construction. It permits
at most 128 names, each at most 256 bytes and matching upstream's
`^[a-zA-Z_][a-zA-Z0-9_]*$` name rule. `LegacyHost: true` explicitly permits all
referenced host names for compatibility; combining it with an allowlist is invalid.
Invalid policy returns `ErrEnvironmentPolicy` during preparation. Policy is never
selected by an HTTP run payload. An adapter using an injected service follows its
owner's policy. Custom preparation is a trusted testing/composition seam and must
preserve the policy when used in production.

## Retrieval and compatibility

| Source | Behavior |
| --- | --- |
| Declared scenario/setup value | Takes precedence, including an explicitly empty string |
| Authorized host import | Captures the current value once per exact URI; retains upstream YAML typing and empty-host semantics |
| URI default | Used when no declared/authorized host value exists; an empty default remains an empty string |
| Authorized but unset host name without default | Captures upstream's nil YAML result; it does not become a later host lookup |
| Unbound name without default | A stand-in string (binding `stand_in`, reason `unbound`), without consulting the host |

Nested provider references follow the same policy and existing source budgets.
Capture remains per exact URI, not an atomic snapshot of the whole environment.
Binding-denial errors identify the missing name without adding its value. General
Collector diagnostics retain existing source-text behavior; raw URI defaults and
source text remain sensitive data. A setting that cannot use a stand-in (a
port, a duration, a whole section) fails validation with a hint naming the
reference and its reason.

No CLI command or `linea-server` mode selects `LegacyHost`:
local `run`, `test`, `diff`, `upgrade-check`, `inspect`, `ottl`
and `statement` read a host value only for a repeatable
`--import-env NAME`; `linea-server` has no such flag and reads no host environment
variable for a request. Only an HTTP adapter that creates its own service
(`api.New` without an injected platform) selects `LegacyHost`. Core services
default to declared-only policy. Direct `engine.Prepare` and `config.CaptureSource`
retain their low-level compatibility defaults; `PrepareWithEnvironment` and
`CaptureSourceWithEnvironment` apply explicit policy. Inspection and the playground
(`InspectWithPolicy`, `EvaluateOTTLWithPolicy`) resolve through the same
capture with the service's policy (environment and host paths, see
[path-policy.md](path-policy.md)); an unbound name is a `reference_stand_in`
warning in their result. `engine.Inspect` and `EvaluateOTTL` keep `LegacyHost` and
unrestricted paths.

## Immutable evidence and replay

New private config snapshots use schema 3 (older snapshots use schema 2),
recording the capture policy and each env retrieval's origin (`declared`,
`default`, `host`, `host_unset`, `stand_in`); schema 3 also records `${file:}`
values read from the scenario (`scenario`) and stand-ins for any scheme with
their reason. A stand-in never needs host authorization. Values remain private.
Run-owned bundle envelopes keep their own schema
([execution-bundles.md](execution-bundles.md)); run journals stay schema 2. Safe
`Run.bundle.environment` discloses policy as `declared_only`, `allowlisted_host`
or `legacy_host`, without names, values or individual hashes. It describes
authorized capture policy, not a count of actual host reads. For decoded
requests it remains the original capture policy; the receiving service
separately authorizes the captured origins. It is not an admission-policy audit
record.

An existing snapshot/prepared request is checked against the admitting service's
policy too. This includes requests decoded from durable bundles. Permission to
read old history does not authorize submitting its captured host bindings to a
stricter service. Authorization uses captured origin/name; it never rereads the
current host value. Exact-build replay compatibility remains independently required.

Schema-1 snapshots remain readable and retain their original bytes and historical
metadata. Their origins are unknown, so references require explicit host import
authorization before submission to a strict service. No origin is invented during
recovery; schema-1 metadata omits the policy field. Unknown nested snapshot schemas
are reported as unsupported bundle schemas. Older binaries cannot read schema-2
snapshots; rolling back code does not downgrade stored run data.

This does not establish complete reproduction. Reference OTLP transports can still
instantiate runtime TLS files; mocked components substitute particular dependencies
without proving that every nested setting is host independent. Bundles therefore
retain `reproducibility: unverified`. These runtime prerequisites must be classified
before `self_contained` or `requires_external_values` can be promised.
