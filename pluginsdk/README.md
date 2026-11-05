# pluginsdk

`github.com/lineahq/linea/pluginsdk` is the Go SDK for fleet plugins. It
holds the host protocol 3 wire types, route and header constants, protocol
bounds, bounded HTTP helpers and `HostClient` for the host services. It uses
only the standard library and never imports fleet internals.

| Resource | Use |
| --- | --- |
| [docs/fleet/plugins.md](../docs/fleet/plugins.md) | The full contract: registration, lifecycle, every route with headers, limits and status codes, deployment, security checklist. Read it first |
| [examples/fleet-agent-summary-plugin](../examples/fleet-agent-summary-plugin) | A small, complete plugin on this package, with a Dockerfile, a registration and a Compose overlay. Copy it to start |
| [internal/fleetplugins/livecapture](../internal/fleetplugins/livecapture) | The production plugin: serves an unchanged Collector extension over gRPC and introspects Collector credentials |

The protocol is plain HTTP/JSON. A plugin in another language implements the
same routes; this package and the example are the reference.

## Add it to a module

The SDK is its own Go module, `github.com/lineahq/linea/pluginsdk`, with no
dependencies outside the standard library and Go 1.22 as its minimum. Every
Linea release tags it as `pluginsdk/vX.Y.Z`:

```
go get github.com/lineahq/linea/pluginsdk@vX.Y.Z
```

It brings none of the debugger's or fleet's dependencies into your module.
Its version follows the Linea release and the host protocol it implements
(`ProtocolVersion`); before 1.0 a minor release may change it.

To build against a checkout, as [the
example](../examples/fleet-agent-summary-plugin) does, replace the module
with the checkout's directory:

```
replace github.com/lineahq/linea/pluginsdk => ../../pluginsdk
```

In Docker, copy `pluginsdk/` and your plugin directory into the build stage
(see the example Dockerfile).

## What a plugin does with it

```go
instance := pluginsdk.RandomID() // one per process start
host := pluginsdk.HostClient{
    Endpoint:   endpoint,  // pluginsdk.Endpoint("http://fleet:8081", allowInsecure)
    Token:      hostToken, // the plugin presents this to fleet
    InstanceID: instance,
    HTTP:       pluginsdk.NewHTTPClient(),
}
```

| Step | What to do |
| --- | --- |
| 1. Serve the manifest | On `GET pluginsdk.ManifestPath`, after checking the service token, answer `pluginsdk.Manifest{ID, Version, ProtocolVersion: pluginsdk.ProtocolVersion, InstanceID: instance, APIVersion}`. Values must equal the registration. Answer 503 when the plugin cannot do its work (for example its database is down): fleet marks it unavailable |
| 2. Keep a session | Poll `host.Session(ctx)` about every 500 ms. It fails with 409 until fleet's probe has seen this instance (up to about 1 s after start), and with 403 while the plugin is drained, disabled or not ready. When the epoch changes or the session fails, stop everything admitted under the old epoch and drop state derived from it |
| 3. Call host services | With the current session: `host.Agents`, `host.ResolveAgents`, `host.ResolveGroup`, `host.Introspect`. Each request sends the instance and epoch headers |
| 4. Serve status and the API | Check the service token, the caller headers and, on API routes, that instance and epoch equal your current session (409 otherwise) |

The example's `watchSession` and `ServeHTTP` implement all four steps in
about 100 lines.

## Reference

### Routes and headers

| Constant | Value | Use |
| --- | --- | --- |
| `ProtocolVersion` | `"3"` | `Manifest.ProtocolVersion` |
| `ManifestPath` | `/fleet-plugin/v3/manifest` | Plugin serves; fleet probes every second |
| `StatusPath` | `/fleet-plugin/v3/status` | Plugin serves; behind `GET /api/v1/plugins/{id}/status` |
| `APIPath` | `/fleet-plugin/v3/api/` | Prefix of the plugin's opaque public API |
| `SessionPath` | `/fleet-host/v3/session` | Host; `HostClient.Session` |
| `AgentsPath`, `AgentsResolvePath`, `GroupsResolvePath`, `IntrospectPath` | `/fleet-host/v3/...` | Host services |
| `CallerIDHeader`, `CallerRoleHeader` | `X-Fleet-Caller-ID`, `X-Fleet-Caller-Role` | Fleet sets them on status and API calls |
| `ActingIDHeader`, `ActingNameHeader`, `ActingRequestHeader` | `X-Fleet-Acting-ID`, `-Name`, `-Request` | Optional, validated; fleet forwards them on API calls. `ReadActing` parses them; attribution only |
| `InstanceHeader`, `EpochHeader` | `X-Fleet-Plugin-Instance`, `X-Fleet-Plugin-Epoch` | Plugin sends them to the host; fleet sends them on API calls |
| `AgentsRead`, `CredentialsIntrospect` | `agents.read`, `credentials.introspect` | Registration permissions |

### Bounds

| Constant | Value | Meaning |
| --- | --- | --- |
| `RequestTimeout` | 750 ms | Manifest, status and host requests, both directions |
| `MaxRequestBytes` | 4096 | Host POST bodies |
| `MaxAgentPageBytes` | 1 MiB | One directory page |
| `MaxResolveTerms` | 8 | Attribute terms per `ResolveRequest` |
| `MaxResolveAgents` | 100 | `ResolveRequest.Limit` maximum (default 16) |
| `MaxGroupAgents` | 1000 | `GroupResolveRequest.Limit` maximum and default |

### Types

| Type | Wire use |
| --- | --- |
| `Manifest` | Manifest answer: `id`, `version`, `protocol_version`, `instance_id`, optional `api_version` |
| `Session` | Session answer: `plugin_id`, `instance_id`, `epoch`, `revision`, `permissions` |
| `Agent`, `Health`, `AgentPage`, `AgentQuery` | Directory page and its query (`after`, `limit` 1-100, `selector`, `connected`) |
| `ResolveRequest`, `ResolveResponse` | `{credential_id?, attributes, limit?}` → `{agent_ids, more}` |
| `GroupResolveRequest`, `GroupResolveResponse` | `{group_id \| selector, connected?, limit?}` → `{agent_ids, revision}` |
| `IntrospectRequest`, `Introspection` | `{token_sha256}` → `{active, credential_id, role, labels, expires_at}` |

Fleet decodes the manifest and its own requests strictly, and the SDK decodes
host answers strictly. Do not add fields to these types in your plugin.

### Functions

| Function | Does |
| --- | --- |
| `HostClient.Session` | Fetches the session and checks its instance and epoch |
| `HostClient.Agents` | One directory page; follow `NextAfter` until empty |
| `HostClient.ResolveAgents` | Connected agents matching all attributes, optionally only those that connected with one credential |
| `HostClient.ResolveGroup` | Members of a group ID or selector; 422 instead of truncation |
| `HostClient.Introspect`, `IntrospectHash` | Hashes a presented bearer token and asks about it; the token never leaves the plugin |
| `HostClient.Headers` | The instance and epoch headers for a session |
| `StatusOf(err)` | HTTP status of a host refusal, or 0 for transport, size and decoding failures |
| `NewHTTPClient` | Client with 750 ms timeouts, two connections, no redirects and no compression |
| `RequestJSON`, `GetJSON` | Bearer request, require 200, read at most `maxBytes`, strict decode |
| `Endpoint` | Validates a base URL: HTTPS, or HTTP to loopback unless allowed |
| `RandomID`, `ValidUID` | 32-hex instance IDs; also the format of epochs, agent and group IDs |
| `ValidID`, `ValidVersion`, `ValidToken` | Registration formats: plugin ID, versions, 64-hex tokens |

Errors never carry upstream response text, so they are safe to log. Log the
status from `StatusOf`, never tokens, request bodies or telemetry.

### Handling host errors

| `StatusOf` | Meaning | Typical handling |
| --- | --- | --- |
| 0 | Timeout, transport failure, oversized or malformed answer | Retry later; answer 503 to your caller |
| 400 | Malformed request, invalid selector or ID | Your caller's input or a bug; answer 400 |
| 401 | Wrong host token | Configuration error; fail readiness |
| 403 | Not enabled and ready, or permission not granted | Wait for the next session; check the registration |
| 404 | Unknown route or group | Answer 404 for unknown groups |
| 409 | Stale instance or epoch | Your session ended; drop the work and refresh the session |
| 422 | Group larger than the limit | Answer 409 or 400 to your caller (fleet does not forward 422) |
| 503 | Host busy or its database unavailable | Retry with backoff |

## pluginsdk/metrics

`metrics.Duration` is an optional, zero-value-ready latency histogram with
fixed buckets from 1 ms to 10 s. `Snapshot().Write(w, name)` writes Prometheus
text. Use compile-time metric names and fixed-cardinality dimensions only.

## Compatibility

Host protocol 3 is fixed: routes, field names and bounds do not change within
it. A breaking change gets a new protocol version and new route prefixes.
Fleet reports a plugin whose manifest declares another protocol as
`incompatible`.
