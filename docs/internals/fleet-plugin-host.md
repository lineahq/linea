# Fleet plugin host internals

How core decides plugin state and fences work. The wire contract is
[plugins](../fleet/plugins.md); this note explains the mechanisms behind it.

## Readiness and epoch

- Fleet probes each registered plugin's manifest every 1 s. The first
  matching answer makes it `ready`. The plugin's session poll then succeeds
  (it got 421 until then) and the plugin starts work under that epoch.
- `ready` lasts 3 s after the last successful probe. Public API calls are
  forwarded only while the plugin is `ready` and the last probe is under 3 s
  old.
- Fleet keeps one epoch per plugin. It changes the epoch on every lifecycle
  change, every plugin restart (new instance ID) and every loss of readiness.
- A request with an old instance or epoch is refused with 421 on both sides.
  Work admitted before a drain, disable or restart cannot continue after it.
  In-flight public calls are canceled when the epoch changes.
- Core never forwards a plugin's 421 (`ServeAPI`) and never maps it to a
  conflict: it calls `Registry.refresh` and answers 503 with `Retry-After: 1`.
  `refresh` is the one place that reloads the plugin's state after a plugin
  saw another authority than core sent. Today the in-memory entry is the only
  state, so it re-reads that; it is the seam where shared plugin state will
  be loaded. 409 passes through, because it now always means a conflict in the
  plugin's own domain.

## Lifecycle store

- Admin lifecycle (`enable`, `drain`, `disable`) is stored in PostgreSQL with
  a revision, so `draining` and `disabled` survive crashes, restarts and
  restores. Memory mode has no lifecycle store.
- The operator `enabled` ceiling is checked first: `enable` needs it.
- Plugin-state metrics and lifecycle persistence do not wait on each other;
  contended state is reported as unknown.

## Ingress

- The host listener exists only when registrations are configured.
- Each registration has its own host and service token; the host token alone
  identifies the plugin on the host listener.
- Public ingress has two read and two write slots per plugin, independent of
  manifest and status polling. Fleet reads the request body (64 KiB, 2 s
  deadline) before it takes a slot, so a slow client cannot hold one.
- Status polling has its own two slots and refuses a third call. The manifest
  probe has a dedicated slot and connection pool, so status calls cannot
  starve it and drop the plugin out of `ready`. Host services allow two
  concurrent requests per plugin, and the session route has a slot of its own
  (`entry.sessions`), so the session poll never waits for them and they never
  wait for it. The plugin side mirrors it: a dedicated HTTP client for the
  session poll, because the SDK client has two connections per host.
- Receivers ignore unknown JSON fields in both directions: the manifest and
  host answers go through `pluginsdk.RequestJSON`, and host requests through
  `decodeHost`, neither of which disallows unknown fields. The registration
  file stays strict: it is operator configuration, not protocol, and a typo
  there should fail startup.
- Session loss policy of a plugin (live capture, the example): a refusal or
  421 answer or a changed epoch ends the session at once; a timeout or
  transport error (`pluginsdk.Unreachable`) needs two polls in a row, about
  1.5 s.
- Introspection takes a token SHA-256, so Collector tokens never reach fleet
  over the host channel. Only agent credentials introspect as active.
- Core never forwards plugin traffic to Collectors and never sends commands
  for a plugin; reporting stays read-only.

## Compatibility

- Protocol `"4"` is fixed: routes, field names and bounds do not change within
  it, and fields may be added because receivers ignore unknown ones. A
  breaking change gets a new protocol version and new route prefixes. Fleet
  reports a plugin whose manifest declares another protocol as
  `incompatible`. Protocol 4 replaced 3 outright (421 for a stale instance or
  epoch, unknown fields ignored, a session slot); there is no fallback.

Test coverage for the registry and host services is in [fleet
verification](fleet-verification.md#plugins).
