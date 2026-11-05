# Fleet plugin host internals

How core decides plugin state and fences work. The wire contract is
[plugins](../fleet/plugins.md); this note explains the mechanisms behind it.

## Readiness and epoch

- Fleet probes each registered plugin's manifest every 1 s. The first
  matching answer makes it `ready`. The plugin's session poll then succeeds
  (it got 409 until then) and the plugin starts work under that epoch.
- `ready` lasts 3 s after the last successful probe. Public API calls are
  forwarded only while the plugin is `ready` and the last probe is under 3 s
  old.
- Fleet keeps one epoch per plugin. It changes the epoch on every lifecycle
  change, every plugin restart (new instance ID) and every loss of readiness.
- A request with an old epoch is refused with 409 on both sides. Work
  admitted before a drain, disable or restart cannot continue after it.
  In-flight public calls are canceled when the epoch changes.

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
  manifest and status polling (two concurrent calls). Host services allow two
  concurrent requests per plugin; a third gets 503.
- Introspection takes a token SHA-256, so Collector tokens never reach fleet
  over the host channel. Only agent credentials introspect as active.
- Core never forwards plugin traffic to Collectors and never sends commands
  for a plugin; reporting stays read-only.

## Compatibility

- Protocol `"3"` is fixed: routes, fields and bounds do not change within it.
  A breaking change gets a new protocol version and new route prefixes. Fleet
  reports a plugin whose manifest declares another protocol as
  `incompatible`.

Test coverage for the registry and host services is in [fleet
verification](fleet-verification.md#plugins).
