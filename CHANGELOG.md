# Changelog

Notable changes in each release. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/). Before 1.0, a minor release may
change APIs, report schemas and persisted formats; the release notes say how.

## Unreleased

**Upgrading from 0.2.0:** a Docker Compose stack moves its secrets from `.env`
into files, changes a few `.env` settings and needs one cleanup, described in
the first item below.

- The Docker Compose stack is one stack for one person and for a team; they
  differ only in `.env`. It runs five services (`postgres`, `seaweedfs`,
  `linea`, `pool`, `builder`). There are no `LINEA_AUTH` modes, no HAProxy,
  no `setup` or `bucket` service, no `LINEA_REPLICAS`, no fixed subnet and no
  stand-in TLS certificate. The secrets are files in a `secrets/` directory
  beside `compose.yaml`, which reach each service as Compose secrets (Compose
  cannot create a secret from the environment or inline content in a
  read-only service, and these are read-only); `.env` is optional and holds
  only settings. `pool` and `builder` wait for the server's healthcheck. The
  stack serves plain HTTP on `127.0.0.1:7070` (`LINEA_BIND`, `LINEA_PORT`);
  for other machines, put your HTTPS reverse proxy in front of it
  (`docs/debugger/deploy-compose.md` has a Caddy example). It runs one
  instance; several are the Helm chart's job. It needs Docker Engine 25 and
  Compose v2.24 or later.

  **Upgrading a stack from 0.2.0:**

  1. Fetch the new `compose.yaml` and `.env.example` (with
     `compose.fleet.yaml`, also `livecapture.json` and `fleet-plugins.json`).
  2. Move each secret from `.env` into its file, keeping the values:
     `LINEA_POSTGRES_PASSWORD` is fixed in PostgreSQL's volume, so it must
     stay exactly as it is. Then remove those lines from `.env`. The admin
     password is new (at least 16 characters); a stack without OIDC gets an
     empty `oidc_client_secret`.

     ```sh
     mkdir secrets && chmod 700 secrets
     for name in postgres_password s3_access_key s3_secret_key worker_token builder_token; do
       sed -n "s/^LINEA_$(echo $name | tr a-z A-Z)=//p" .env > secrets/$name
     done
     sed -n 's/^LINEA_OIDC_CLIENT_SECRET=//p' .env > secrets/oidc_client_secret
     openssl rand -hex 32 > secrets/admin_password
     sed -i.bak '/^LINEA_\(POSTGRES_PASSWORD\|S3_ACCESS_KEY\|S3_SECRET_KEY\|WORKER_TOKEN\|BUILDER_TOKEN\|OIDC_CLIENT_SECRET\)=/d' .env
     rm .env.bak
     ```

     Check that each file holds the value without quotes. All seven files
     must exist, `oidc_client_secret` empty without OIDC.
  3. Remove `LINEA_AUTH` and `LINEA_REPLICAS`, and move the settings of the
     old mode to their new names:

     | 0.2.0 | Now |
     | --- | --- |
     | `LINEA_POSTGRES_PASSWORD`, `LINEA_S3_ACCESS_KEY`, `LINEA_S3_SECRET_KEY`, `LINEA_WORKER_TOKEN`, `LINEA_BUILDER_TOKEN` | The files `secrets/postgres_password`, `s3_access_key`, `s3_secret_key`, `worker_token`, `builder_token` (step 2) |
     | `LINEA_AUTH=local` | Nothing: sign in as `admin` with the contents of `secrets/admin_password` |
     | `LINEA_PUBLIC_URL` (oidc) | `LINEA_AUTHENTICATION_OIDC_REDIRECT_URL`, the full URL: `<LINEA_PUBLIC_URL>/auth/callback` |
     | `LINEA_OIDC_ISSUER`, `LINEA_OIDC_CLIENT_ID`, `LINEA_OIDC_CLI_CLIENT_ID`, `LINEA_OIDC_GROUPS_CLAIM` | `LINEA_AUTHENTICATION_OIDC_ISSUER`, `_CLIENT_ID`, `_CLI_CLIENT_ID`, `_GROUPS_CLAIM` |
     | `LINEA_OIDC_SCOPES` (comma-separated) | `LINEA_AUTHENTICATION_OIDC_SCOPES` as a YAML list, such as `[openid, profile, email, groups]` |
     | `LINEA_OIDC_CLIENT_SECRET` | The file `secrets/oidc_client_secret` (step 2), read once `LINEA_AUTHENTICATION_OIDC_ISSUER` is set |
     | `LINEA_ADMIN_GROUP`, `LINEA_ENGINEER_GROUP`, `LINEA_VIEWER_GROUP` | One `LINEA_BINDINGS`, such as `[{group: admins, role: admin, scope: "*"}, {group: engineers, role: engineer, scope: "*"}]` |
     | `LINEA_TRUSTED_PROXIES` (proxy) | `LINEA_AUTHENTICATION_TRUSTED_HEADERS_PROXIES` as a YAML list, such as `[10.0.0.0/8]`. The server now checks the address it sees itself; HAProxy no longer filters. |
     | `LINEA_PROXY_ISSUER` | `LINEA_AUTHENTICATION_TRUSTED_HEADERS_ISSUER`; set the old value (`proxy` when unset) to keep principal IDs |
     | `LINEA_USER_HEADER`, `LINEA_GROUPS_HEADER`, `LINEA_NAME_HEADER` | `LINEA_AUTHENTICATION_TRUSTED_HEADERS_USER`, `_GROUPS`, `_NAME` |
     | `LINEA_STORAGE_LIMIT`, `LINEA_RUN_LIMIT` | `LINEA_STORE_STORAGE_LIMIT`, `LINEA_STORE_RUN_LIMIT` |
     | `LINEA_RETENTION_RUNS`, `LINEA_RETENTION_AGE` | Unchanged |
     | `LINEA_QUEUE_LIMIT` | `LINEA_LIMITS_QUEUE` |
     | `LINEA_BUILD_EXPORT` | `LINEA_BUILDS_EXPORT_REPOSITORY` |
     | `LINEA_TLS_CERT_FILE`, `LINEA_TLS_KEY_FILE` | Removed: terminate TLS in your reverse proxy |
     | `LINEA_BIND`, `LINEA_PORT`, `LINEA_VERSION` and the resource settings | Unchanged |

  4. With `compose.fleet.yaml`: the 0.2.0 overlay kept its secrets in the
     `linea_setup` volume, which the next step removes, and the new one reads
     files. Copy the three that must not change before removing it: the fleet
     token, which becomes the fleet admin token (`fleet_admin_token`) because
     fleet keeps its first admin, and the plugin's certificate and key,
     because Collectors trust that certificate. Create the other fleet files,
     the new `fleet_operator_token` included, as in
     `docs/debugger/deploy-compose.md#fleet-and-live-capture`, and replace
     `LINEA_CAPTURE_RAW=true` by the operator token's credential ID in
     `raw_principals` of `livecapture.json`.

     ```sh
     for f in linea/fleet_token:fleet_admin_token livecapture/tls_cert.pem:capture_tls.crt livecapture/tls_key.pem:capture_tls.key; do
       docker run --rm -v linea_setup:/setup:ro busybox cat "/setup/${f%%:*}" > "secrets/${f##*:}"
     done
     chmod 644 secrets/capture_tls.key
     ```
  5. Run `docker compose up -d --remove-orphans`, which removes the `setup`,
     `bucket` and `proxy` containers, then `docker volume rm linea_setup`. If
     Compose stops on the `edge` network, which no longer has a fixed subnet,
     run `docker compose down` first; it keeps the volumes.

  After this, `docker compose up -d` applies a change to `.env`, and
  `docker compose restart` applies a changed secret file. A stack with
  `compose.fleet.yaml` reads its secrets from files in the same directory
  (`docs/debugger/deploy-compose.md#fleet-and-live-capture`).
- `linea-server` is configured from the environment: `LINEA_` plus the config
  key's path in capitals, joined by `_` (`store.storage_limit` is
  `LINEA_STORE_STORAGE_LIMIT`). Secrets are read from files named by the
  `*_FILE` variables, lists and objects are YAML flow values, and a variable
  overrides the same key of the optional `--config` file. The database URL may
  be given without its password, with `store.database_password_file`.
  `docs/debugger/team-server.md` lists them.
- The server has a built-in admin account: the user `admin`, with the password
  in `LINEA_AUTHENTICATION_ADMIN_PASSWORD_FILE` (at least 16 characters), the
  admin role on everything and no binding to write. The web client has a
  `/sign-in` page with the password form and, with OIDC, "Sign in with" the
  provider; `linea login` asks for the password (`--password-stdin` for
  scripts) and stores an API token. Failed attempts are limited to 5 a minute
  per client address and 20 a minute in total, per instance. Unsetting the
  password turns the account off, and its sessions and tokens stop working.
  After rotating the password, run `linea-server revoke-admin`: it revokes
  the account's API tokens, ends its web sessions and records an audit event,
  and prints the counts.
  The server needs at least one sign-in method (the admin password, OIDC or
  trusted headers). The trusted-header "local" sign-in the Compose stack used
  is gone.
- The server creates its bucket at start when `store.objects.create_bucket` is
  true (off by default; the Compose stack turns it on).
- **Removed:** builder autoscaling and the build metrics endpoint.
  `linea-server --metrics-listen` is gone (an unknown flag refuses startup),
  and so are the chart's `builder.autoscaling` (KEDA `ScaledObject` or HPA),
  `server.metrics`, the `<release>-metrics` Service and the server's
  NetworkPolicy. Builders run `builder.replicas` pods. **Upgrading:** remove
  `builder.autoscaling` and `server.metrics` from your values, and set
  `builder.replicas` to the builders you want; the build queue's counts are
  now among the server's OTLP metrics (`linea.runtime.builds`).
- `linea-server` exports metrics about itself over OTLP with the
  OpenTelemetry SDK when the standard `OTEL_EXPORTER_OTLP_ENDPOINT` (or
  `OTEL_METRICS_EXPORTER=otlp`) is set: run queue size, claim wait, lease
  renewals, losses and expiries, connected workers and builders, runtime
  builds, storage readiness and audit append failures
  (`docs/debugger/team-server.md` lists them). `GET /readyz` answers 503
  `storage unavailable` after the database has not answered for a whole run
  lease, while `/healthz` stays 200.
- `linea-server` exports spans about itself over OTLP under the same rules
  (`OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` or
  `OTEL_TRACES_EXPORTER=otlp`): one span per HTTP request named by its
  route, and one `linea.run` span per run attempt with a `linea.run.publish`
  child and a `linea.run.execute` child. Under it, one `linea.worker.<phase>`
  span per phase the worker timed and sent back with the result: the
  bundle, the configuration, the Collector run, the analysis and the result.
  The worker exports nothing itself. A run attempt links to the request that
  submitted the run on whichever instance claims it. Each runtime build
  attempt is a `linea.build` span linked to the request that asked for it,
  with `linea.build.execute` (one `linea.builder.<phase>` span per phase the
  builder timed: downloading the inputs, the build, uploading the binary,
  exporting the image) and `linea.build.publish`. Database statements and
  object storage calls made for a request, a run or a build are client spans
  with the statement's placeholders, never its values, keys or bucket. An
  incoming W3C `traceparent` continues its trace. Spans carry IDs,
  routes, states and error codes, never queries, headers, error messages or
  telemetry (`docs/debugger/team-server.md` lists them). With only the shared
  endpoint set, set `OTEL_TRACES_EXPORTER=none` to export metrics alone.
- A server with a database holds less memory per finished run. The runs
  it keeps in memory after running or reading them are bounded by size as
  well as count: 256 MiB of decoded evidence, but always the 8 used last. A
  run that just finished counts as used, so it stays for whoever opens it
  next. A finished run no longer keeps its raw bundle in memory; downloads
  read it from storage. Other runs are read from storage when asked for, as
  before.
- `GET /healthz` and `GET /readyz` answer without a credential: the second
  answers 503 until the storage is open and once the server drains.
  `linea-server probe` checks `/readyz` (`--url` picks another) and is the
  Compose healthcheck.
- The server no longer exits when its database or bucket is not reachable at
  start: it listens, says why on stderr and retries after 1 s, doubling up to
  every 30 s, answering other requests 503 with `Retry-After` meanwhile.
- The Helm chart follows the server. `admin.enabled`, `admin.password` and
  `admin.existingSecret` set up the built-in admin account: unset, it is on
  when `config.authentication` has neither `oidc` nor `trusted_headers`, with
  a generated password kept across upgrades (Secret `<release>-linea-admin`,
  key `password`); `config.bindings` is then optional. The server's probes
  are `GET /readyz` and `GET /healthz`. The server creates the bundled
  SeaweedFS bucket (`store.objects.create_bucket`), so the bucket Job is gone,
  and `store.objects.createBucket` does the same for your own store (off by
  default). With the bundled PostgreSQL the server config holds a URL without
  the password and `store.database_password_file` names it, so the `store`
  Secret has no `database-url` key then. `docs/debugger/deploy-kubernetes.md`
  has the details.
- The Helm chart's builders reach public addresses only by default:
  `networkPolicy.builderEgress` now excludes the private, shared, loopback
  and link-local ranges, so a manifest cannot point a build at the cluster,
  the private network or the cloud metadata endpoint. **Upgrading:** a
  builder that uses an internal module proxy, git host or registry needs a
  `builderEgress` rule for it. The team server docs say what a project
  isolates: one server is one trust boundary.
- The debugger's fleet token is now a fleet **operator** credential, not a
  fleet admin: `linea-server` can no longer issue fleet credentials or run
  plugin lifecycle operations. The Compose overlay and the Helm chart now
  generate a separate fleet admin token that stays with whoever administers
  fleet, and a fleet operator token that the server uses. `linea-fleet
  bootstrap` installs the operator from `LINEA_FLEET_OPERATOR_TOKEN` (or
  `_FILE`) after the admin, named by `--operator-name` (default
  `linea-server`), and prints `fleet operator created` or `fleet operator is
  current`.

  **Upgrading:** the token a stack or release already has stays fleet's
  admin; bootstrap finds it current. A new operator token is installed on the
  next bootstrap.
  - Helm: nothing to do. The chart keeps the existing token in the Secret
    `<release>-linea-fleet` under `token` (now the admin token), generates
    `operator-token` in the same Secret, and gives the server the operator
    token. Read the admin token with `kubectl get secret ... -o
    jsonpath='{.data.token}'`.
  - Compose: rename `secrets/fleet_token` to `secrets/fleet_admin_token`,
    create `secrets/fleet_operator_token` with `printf 'of1_%s_%s\n'
    "$(openssl rand -hex 16)" "$(openssl rand -hex 32)"` (a different token), run
    `scripts/debugger/compose-fleet.sh` and restart `linea`. If you set
    `raw_principals` to the server's credential ID, change it to the second
    `_`-separated field of the operator token.
- Fleet rollouts: a wave passes only once one of its targets succeeded or an
  operator accepted its failures with `:resume`. A wave whose agents are all
  offline, incompatible or unmatched waits for the wave timeout and then
  pauses with reason `threshold`; before, it advanced at once and could
  complete a rollout no agent had applied. `:resume` restarts the current
  wave's timeout.
- Live capture: a Collector still reserved by an earlier capture is refused
  with `collector_busy` also after a stop, reconnect or restart (it was a
  plain `capture_request_refused`). A stop before the session was sent frees
  the Collector at once and can answer `stopped` directly. An operation
  leaves `starting` once every target is active or already ended. A
  Collector that reconnects replaces its old peer once that peer missed
  reports for 3 s, instead of being refused as a duplicate for up to 6 s.
- The live-capture plugin always serves TLS on its control and delivery
  listeners: the `test_insecure` setting is removed, and a configuration that
  still names it is refused. **Upgrading:** delete `test_insecure` from the
  plugin's JSON configuration.
- Fleet scopes management credentials to plugins. A credential reaches a
  plugin's status and API only with a `plugin_access` grant for that plugin
  (`read`, `write` or `admin`, at most 16 plugins); the role grants none, and
  fleet answers 403 without a grant, also for an unknown plugin. Fleet sends
  the level as `X-Fleet-Caller-Access`, which replaces `X-Fleet-Caller-Role`
  in host protocol 4 (`pluginsdk.Caller` reads it). Acting-for headers need
  the new `delegate` flag, else 400 `acting_for_not_delegated`. An agent
  credential gets a second token, `plugin_token` (`ofp1_...`), that Collector
  components present to plugins: introspection matches only it, and fleet
  refuses it. Migration 014 adds the columns. Live capture lets `read` list
  and `write` or `admin` write; the example plugin's group count needs `write`
  (`write_required`). `linea-fleet bootstrap` takes `--plugin-access` for the
  first admin and `--operator-plugin-access` and `--operator-delegate` for the
  deployment operator, and a repeat applies changed grants. The debugger's
  Compose overlay and Helm chart grant the admin `livecapture=admin` and
  `linea-server` `livecapture=write` with delegate. Memory mode grants its
  management token `read` on every plugin and issues no plugin tokens.

  **Upgrading:** reissue agent credentials whose Collectors use a plugin and
  set the livecapture extension's headers to the new `plugin_token`
  (`LINEA_FLEET_PLUGIN_TOKEN` in the examples); the plugin refuses the agent
  token. Grant `plugin_access` to each management credential that calls a
  plugin. A plugin of your own reads `X-Fleet-Caller-Access` instead of the
  role.
- Fleet binds an agent's OpAMP instance UID to the credential that holds it.
  While the agent is connected, a connection with another credential and the
  same UID is refused with an OpAMP error response and recorded as
  `agent.credential_refused` (and in
  `linea_fleet_agent_credential_refused_total`). Once the agent is
  disconnected, another credential may take the UID over only when the
  holding credential is revoked or expired, or when the new one has the same
  labels and remote config opt-in (a rotation); fleet then records
  `agent.credential_changed`. Any other connection is refused as for a held
  UID. Collectors that share a UID across credentials (cloned identities) now
  see one of them refused.

- Plugin host protocol 4 replaces protocol 3 without a fallback. The routes
  move from `/fleet-plugin/v3/...` and `/fleet-host/v3/...` to `v4`, and a
  manifest declares `protocol_version` `"4"`. A stale instance or epoch is now
  answered with **421 Misdirected Request**, by a plugin and by the fleet host
  listener, where both used 409: 409 is left to conflicts in a plugin's own
  domain (live capture's `collector_busy`, a changed retry). Fleet never
  forwards a plugin's 421 to an API caller: it answers 503 with
  `Retry-After: 1`. Every receiver ignores JSON fields it does not know, in
  both directions (the manifest, host requests and host answers), so fields
  may be added within a protocol version. Fleet serves the session route from
  its own host slot, and a plugin polls it with an HTTP client of its own
  (`pluginsdk.HostClient.SessionHTTP`), so a plugin's other host calls cannot
  make it lose its session. The live capture plugin and the example plugin
  end the session at once on a 421 or a changed epoch, and after two
  timeouts or transport errors in a row, where one failed poll ended every
  capture. The live capture public API no longer lists the epoch under 409.

  **Upgrading:** upgrade fleet and its plugins together; a plugin built for
  protocol 3 is `incompatible` until it is rebuilt on `pluginsdk` 4. Live
  capture 0.5.0 (unreleased) and its `api_version` 3 do not change, so
  registrations stay as they are. A plugin of your own changes its paths and
  `ProtocolVersion` (the SDK constants do both), answers 421 instead of 409
  for a mismatch (`pluginsdk.AuthorityMatches`, `pluginsdk.StatusMisdirected`)
  and passes a `SessionHTTP` client.

- The live capture plugin logs its state transitions (plugin 0.5.0). It
  writes one JSON line on stderr per transition, never per request: start and
  stop with the version, the database authority, the host session and its
  epoch changes with a reason, a degraded, recovered or lost database, the
  cause of every fatal exit, each capture operation's state change, each
  Collector connection and link change, and one summary line per listener
  every 10 s for rejected credentials. A line holds only IDs, enum reasons,
  counts and durations; a token, filter, attribute value, payload or error text
  cannot reach it, and a test checks that no other logging call exists. A
  failed command no longer prints its error text: it logs one line with an enum
  `cause`. The fields are in `docs/fleet/livecapture.md`.

- The Helm chart's fleet Service `<release>-linea-fleet` carries OpAMP
  only, and `fleet.deploy.service.type` applies to it alone. The management
  API and the plugin host API move to `<release>-linea-fleet-internal`, which
  is always `ClusterIP`, so exposing OpAMP no longer exposes them. The server
  reaches fleet there. `fleet.deploy.livecapture.tls.pluginAPI: true` serves
  the plugin's API to fleet over TLS; the chart's generated certificate now
  also names `<release>-linea-livecapture-plugin`.

  **Upgrading:** anything that reached the management API through
  `<release>-linea-fleet` uses `<release>-linea-fleet-internal`. To turn on
  `tls.pluginAPI` with a certificate the chart generated earlier, delete the
  Secret `<release>-linea-livecapture-tls` first and give Collectors the new
  `tls.crt`.

- Fleet keeps administrative changes in an audit record: credential, group,
  config, assignment, rollout and plugin lifecycle changes are written to
  `fleet.audit` (migration 015) in the same transaction as their event. The
  leader keeps it for 400 days and at most one million rows, so agent churn
  that trims events cannot evict a revocation. Admins read it through
  `GET /api/v1/audit`.
- Fleet deletes agent and reader credentials revoked more than 30 days ago,
  in bounded batches with a `credential.deleted` event each, so revoked
  metadata no longer counts against the 1,000-credential cap. Admin and
  operator credentials stay. **Upgrading:** the runtime role gains DELETE on
  `fleet.credentials`; rerun the migration job's grant.
- A config assignment must name at least one credential label, because agent
  attributes are self-reported. An assignment stored without labels matches
  no agent, and the debugger's fleet config path refuses a label-less
  assignment. **Upgrading:** add `credential_labels` to every assignment that
  should keep reaching agents.
- A group edit that changes the desired config of more connected agents than
  `--config-unstaged-limit` (default 10) is refused with 409
  `too_many_affected`, unless the request carries `max_affected` at least that
  large. Group edits are not staged by rollouts; the docs list what a rollout
  cannot stage. The refusal's error body carries `affected`, the number of
  agents the edit would reconfigure. The debugger passes it on as a conflict
  with `affected` (HTTP and MCP), takes `max_affected` on a collector group
  save, and the web client asks inline to confirm before resending.
- Fleet serializes group, config, credential and plugin row creations that a
  row cap bounds, so concurrent creations can no longer pass the cap together.
- The live capture plugin introspects the credential of every new control
  stream instead of reusing a cached admission, so a revoked credential cannot
  reconnect from the cache. Delivery requests still use the cache.
- The live capture plugin reports transition log lines dropped on a full
  queue as `log_lines_dropped_total`.
- Fleet stores a Collector build's component set only from credentials with
  remote config, and only when the list hashes to the reported build hash as
  the Collector's OpAMP extension computes it. An agent whose build stays
  unresolved gets no config (reason `unverified_components`); new counters
  report withheld sends and refused lists. The agent simulator reports
  hashes in the same scheme.
- The agent listener bounds unauthenticated connections apart from sessions:
  a slot is freed once the peer presents an agent credential, so
  `--max-connections` bounds both separately. Every gateway refusal closes the
  connection, the listener offers HTTP/1.1 only, and a new
  `tcp_unauthenticated_connections` gauge reports slot use
  (`tcp_connections` now counts all open connections).
- Fleet can send the largest allowed remote config: opamp-go checks sent
  messages against the receive limit, so the gateway gives it the larger of
  `--max-message-bytes` and the largest remote config and enforces
  `--max-message-bytes` on received messages itself.
- Fleet's replica heartbeat owns one database session outside the pool and
  the leader jobs run apart from it, so an exhausted pool or a slow reap no
  longer fences a healthy replica. Each replica opens at most nine sessions.
- Failure holds and component sets that agents cause run only while the
  report writer is not backlogged, at most two at a time per replica; a
  refused write is retried with a later message.
- A restore rewrites retained events in its own transaction with a 4-minute
  statement and lock timeout before registration, so a restore with many
  events can register. Migrations and offline administration get a 5-minute
  statement timeout, a 10 s lock timeout and a 10-minute overall bound.
- Fleet applies migrations 013 and 014: the schema version is now derived
  from the migration list, which had stayed at 12.
- Live capture `admin` access lists every owner's running captures
  (`GET v1/captures`), reads their metadata and stops them; channel reads and
  ACKs stay owner-only. `max_operations_per_owner` and
  `max_channels_per_owner` bound one credential's share and default to the
  plugin's limit of 32.
- A reconnecting Collector replaces its live capture peer once that peer has
  gone 3 s without a report, instead of being refused as a duplicate until
  the stale sweep retires it. A duplicate that still reports is refused.
- `linea-fleet` writes a transition log: one JSON line on stderr per state
  change and none per request, with `time`, `level`, `msg` (the event name),
  `component` (`fleet`) and fields from a closed set of IDs, enumerated
  reasons, counts and durations, so no credential, configuration, attribute
  value, telemetry or error text can reach it. It covers start and stop with
  the stop reason, replicas registering, being reaped or fencing themselves,
  the leader lease, database loss and recovery, plugin state and epoch
  changes, rollout transitions, withheld configs, credential creation and
  revocation, and one `auth_failures` summary per listener every 10 s. The
  events and fields are in `docs/fleet/deployment.md`. The `ready:` line on
  stderr is replaced by `process_ready`, and a failure of the running
  service now prints one fixed sentence after its `process_stopped` line
  instead of the error text. **Upgrading:** nothing to change; a log shipper
  that matched the `ready:` line matches `"msg":"process_ready"`.

- Live capture never makes a Collector wait (plugin 0.5.0, plugin API
  version 3, API schema 3.0.0). The plugin answers every export at once and
  buffers at most 2 MiB and 64 batches per channel (32 MiB in all); a batch
  that does not fit or is not read within 10 s is dropped and counted on its
  target's new `delivery` counters, and the capture goes on instead of
  ending with `receipt_uncertain` or `delivery_capacity`. An ACK is
  `{batch_id}`. The web client marks a capture with lost records as
  incomplete. A Collector ends a remote session as `control_lost` after 10 s
  without a desired state (the plugin repeats it every 3 s), drains its
  queues for at most 2 s, and logs one warning when a capture panic ends a
  session. **Upgrading:** fetch the new `livecapture.json` and
  `fleet-plugins.json` (or `registration.json`) with the 0.5.0 image: fleet
  accepts the plugin only when its registration says version 0.5.0 and
  `api_version` 3.

- The live capture plugin deletes unused peers (plugin 0.5.0, plugin
  database schema 4). A peer with no connected Collector, no lease and no
  retained operation is deleted 24 h after it was last seen, so rolling pods
  can no longer fill the 10,000-peer table and block new Collectors. A peer
  that returns starts above every generation ever deleted. **Upgrading:**
  run the plugin's migration step (`migrate`), which adds migration 004 and
  grants the runtime role `DELETE` on `capture.peers`; the plugin refuses to
  start on schema 3.

- The live capture purge runs beside the running plugin (`linea-fleet-livecapture
  purge`): it no longer needs the plugin stopped, and it removes only
  operations in a terminal state. A 429 from the plugin no longer carries
  `Retry-After: 1`, which promised a wait that a full operation table (cleared
  only by purge) or full slots never kept.

- The live capture plugin no longer exits on one slow database answer (plugin
  0.5.0). Only its maintenance loop pings the database, once a second; API
  requests, deliveries and control wakes read the result. A ping that waits
  behind other statements is not counted. The plugin exits after three failed
  pings in a row, or at once when the connection is closed. Compose now
  restarts it (`restart: unless-stopped` in `compose.livecapture.yaml`). The
  credential recheck runs every 10 s even when link wakeups arrive between
  rechecks.

- The live capture plugin links Collectors to fleet agents fairly (plugin
  0.5.0, API schema 3.0.0). It tries the peer whose last attempt is oldest
  first, so peers that cannot link no longer keep others from being tried,
  and retries an unlinked peer after 2 s, doubling to 60 s. `GET v1/peers`
  shows `link.state` (`pending`, `linked`, `no_match`, `ambiguous`), and the
  plugin counts attempts by outcome in `peer_link_linked_total`,
  `peer_link_no_match_total`, `peer_link_ambiguous_total` and
  `peer_link_error_total`.

- The live capture plugin no longer writes to its database while holding its
  lock (plugin 0.5.0). Operation state is decided under the lock and written
  by one writer in order, so a Collector reconnect storm or a slow database
  no longer stalls control streams, delivery and the API. A read returns the
  decided state at once. Concurrent identical creates answer like a replay
  instead of 409. The control handshake mints the peer generation in one
  statement outside the lock. The transition log reports a state once it is
  written.

- The live capture plugin bounds its gRPC connections (plugin 0.5.0). Both
  listeners ping a quiet connection after 30 s and close it when the ping goes
  unanswered for 10 s, close a connection without streams after 30 s and cap
  request headers at 16 KiB; delivery connections are replaced after 5
  minutes. A delivery request with a missing, malformed or recently refused
  credential is refused before its OTLP body is read.

- Report schema 0.16: each topology component lists what a run does not show
  of the real component (`not_shown`, a code and a sentence each): export
  outcomes of captured exporters (every export succeeds, so errors, retries,
  queues, backpressure and failover are not exercised), `memory_limiter`
  measuring the debugger's process instead of your Collector, receiver
  intake, and the processing or routing of stand-ins. `linea run` and
  `linea inspect` print it; the web client shows it in the component's mode
  tooltip and How it runs view. Reports of schema 0.8 to 0.15 stay readable.
- Runtime self-check: every runtime checks, when it starts, that the
  Collector internals the instrumentation relies on behave as verified
  (pipeline attribution, connector routes, emission context, object identity,
  fan-out copies, extension status, lifecycle). `linea runtime list`,
  `linea version`, the web client's runtime detail and the API's runtime
  (`self_check`) show the result, and reports record it
  (`runtime.self_check`). A failed check refuses nothing.
- Run warnings (report `warnings`, also in the run summary): a failed
  self-check of the run's runtime, a processor the Collector gave no pipeline
  ID (`unattributed_stage`) and connector deliveries without a pipeline
  (`unattributed_route`). `linea run`, the web run overview and MCP's run
  summary show them.
- Runtime build IDs start with `build-` (`build-` and 16 hex digits) instead
  of `run-`, so a build ID is no longer mistaken for a run ID. Builds admitted
  before keep their `run-` IDs and stay readable.
- A team server reads a build's manifest from object storage once and keeps
  it, instead of on every status request, and checks a stored runtime binary
  with a metadata request instead of opening it.
- Team server traces show where a request spends its time between storage
  calls: `linea.run.prepare` (preparing a run before admission),
  `linea.inspect` (inspecting a configuration on a runtime), and
  `linea.run.wait` and `linea.suite.wait` (a request waiting for runs to
  end). `s3 Open` now lasts until the object is read and closed. An upgrade
  check, a comparison and a comparison of two runs read the change feed once
  and bind each runtime once, instead of once per step.
- **Fixed:** on Collector 0.165.0, runs recorded no start or stop for custom
  extensions, and the runtime self-check failed `extension_status`. 0.165.0
  reports component status through
  `extensioncapabilities.ComponentStatusWatcher` instead of
  `componentstatus.Watcher`; the debugger's watcher now answers both.
- **Fixed:** a team server refuses a runtime source that is not an
  `https://` repository before it runs git. Some git versions looked a local
  path up first and answered whether it existed on the server's host.
- **Fixed:** team server instances whose clocks disagree no longer requeue
  each other's live runs or builds. The database store moves every lease
  time onto the database's clock by an offset it measures every 30 seconds.
- **Fixed:** `limits.runs_per_principal` holds across the instances of a
  team server. Each instance counted only its own runs, so a principal could
  hold the limit once per instance. Admission now counts the principal's
  unfinished runs in the database (migration `017_run_principal.sql`); runs
  admitted before the migration count toward no principal.
- **Fixed:** a run's state changes only once the database holds it. A run
  whose outcome the database does not accept stays running and is written
  again until it is accepted, rather than ending as an error only one
  instance shows; cancelling a queued run then answers with a persistence
  error and the run is cancelled once the database accepts it. A slow
  database or a large input bundle being stored no longer holds up lease
  renewal, other submissions or other runs, and a run
  claimed while a single-instance service closes is cancelled instead of
  never ending.
- **Fixed:** a suite or a comparison is admitted whole or refused whole.
  Its members are admitted in one database transaction, which counts the
  principal's runs on every instance, so a suite no longer exceeds
  `limits.runs_per_principal` when other instances hold the principal's
  runs, and a refused admission leaves none of its runs behind. A suite's
  members start once all of them are prepared, not one by one.
- **Fixed:** the connected CLI sends a request again, a submission included,
  when it is refused with a 503 that carries no operation error code: an
  instance still waiting for its storage, or a load balancer with no instance
  ready. Such a refusal handled nothing. It waits the response's
  `Retry-After`, for up to 30 s of such refusals, so a run submitted while an
  instance restarts reaches it or another instance instead of failing.

- `fleet.plugin_lifecycle` can hold each plugin's epoch for all core
  replicas to share (groundwork for several replicas; fleet schema version
  13, **run `migrate` before starting the new release**). The row gains
  `epoch`, `instance_id` and `seen_at`, and a plugin that was probed but never
  changed has a row at revision 0. The store applies fixed
  transitions on it, each conditional on the epoch the caller read, with a
  `fleet_plugin` hint on every change; the memory store applies the same
  rules, so plugin lifecycle (drain, disable, enable) now also works in
  memory mode, without surviving a restart.
- Every fleet replica now takes a plugin's state and epoch from that shared
  row, so all replicas show one epoch and a drain on one is seen by all. The
  session route reads the row on every request; other paths read a cache
  refreshed by the `fleet_plugin` hint and every 500 ms. One lost probe no
  longer fences the plugin: a refusal rotates the epoch at once, and a plugin
  no replica reached for 3 s rotates once. New log reason `shared_state`;
  `plugin_epoch_changed` for a refusal now says `probe_failed`.

## [0.2.0]

**Upgrading from 0.1:** a Docker Compose stack needs `LINEA_AUTH=local` in
`.env` to keep the previous behavior, and fleet needs `migrate` before the
new release starts (schema version 11). Both are described below.

- Using capture records in the web client: "New configuration and scenario"
  now starts a configuration of its own (OTLP in, one pipeline per captured
  signal) with no receiver to map, instead of joining the open
  configuration. Every destination makes one input per signal, however many
  streams the records came from. Adding to the open scenario when no
  receiver accepts a signal offers to add a pipeline for it to the
  configuration draft.
- The Docker Compose team server is configured by `.env` for any use, not
  only one person on loopback: `LINEA_AUTH` chooses sign-in (`local`, `oidc`
  through an identity provider, or `proxy` behind an authenticating proxy),
  and `LINEA_BIND`, `LINEA_PORT`, `LINEA_TLS_CERT_FILE` / `LINEA_TLS_KEY_FILE`,
  the role groups, storage and run limits, retention, the queue limit and
  image export are settings too. The setup service writes the server and
  HAProxy configs from them and stops with the variable's name on a bad
  value. **Existing stacks:** add `LINEA_AUTH=local` to `.env` to keep the
  previous behavior; without it the stack does not start. Workers and the
  builder now reach the server through HAProxy's own internal frontend.
  The stack runs one server instance by default; `LINEA_REPLICAS=2` keeps
  the previous two.
- `linea login` against a server with no CLI sign-in says so; when the
  server already signs requests in (the Compose stack's `local` mode), it
  says no sign-in is needed and exits 0.
- CLI text output is structured into headed sections and aligned tables,
  and `linea completion bash|zsh|fish|powershell` completes commands, flags
  and values.
- Fleet admin and reader credentials may be issued without `expires_at` and
  then stay valid until revoked; agent credentials still require an expiry.
  `bootstrap` issues its admin without expiry, so a team server connected
  with it no longer loses fleet after 90 days. Fleet schema version 11:
  **run `migrate` before starting the new release.**
- Fleet `bootstrap` installs a token the deployment generated, given in
  `LINEA_FLEET_BOOTSTRAP_TOKEN` (or `_FILE`), and is then safe to repeat. The
  debugger's Compose overlay (`compose.fleet.yaml`) generates
  `linea-server`'s fleet token in its setup volume and installs it this way;
  existing stacks keep their token.
- The Helm chart runs fleet and the live-capture plugin with
  `fleet.deploy.enabled` and connects the server to them: databases and
  roles in the bundled PostgreSQL (or your own, named in a Secret),
  migrations in init containers, a generated server token installed as
  fleet's first admin, generated plugin tokens and a self-signed plugin
  certificate, all kept across upgrades.
- The debugger checks its fleet connection on request: **Fleet → Connection**
  in the web client, `linea fleet connection`, `GET /api/v1/fleet/connection`
  and the MCP tool `linea_check_fleet_connection` report each step (fleet
  with the server's credential, the livecapture plugin, config management)
  with its result and error code. Without a fleet the page shows how each
  deployment connects one.
- Fleet config management can be turned on in the debugger's deployments:
  `LINEA_FLEET_CONFIG_MANAGEMENT=true` in the Compose stack's `.env`, or
  `fleet.deploy.configManagement: true` in the Helm chart. Both default to
  off, as before.
- Raw capture can be granted in the debugger's deployments:
  `LINEA_CAPTURE_RAW=true` in the Compose stack's `.env`, or
  `fleet.deploy.livecapture.rawCapture: true` in the Helm chart, puts the
  server's fleet credential into the plugin's `raw_principals`. Both default
  to off. A capture of raw points that fleet refuses now says the server
  lacks the grant instead of "fleet: capture access denied".

- Collector 0.165.0 joins the runtime support table. The debugger explains
  the new `queue_batch` processor like the batch processor (sends, triggers,
  partitions, client context, verification against its metrics), and the
  batching recommendations name its options. Reports gain batch settings
  `sizer`, `cache_size` and `unbatched`, the batcher field `evicted` and the
  send trigger `evicted`.
- New recommendation `batch.exporter_batches`: a batch or queue_batch
  processor in a pipeline whose every exporter already batches in its own
  `sending_queue::batch`.

## [0.1.1]

- Releases carry third-party notices, which 0.1.0 lacked: every CLI archive
  has `THIRD_PARTY_NOTICES.txt` beside `LICENSE`, and every image has both in
  `/licenses/`, with the license and notice files of everything that binary
  links. `linea-server`'s also cover the bundled web client, whose licenses
  it serves at `/licenses.md`. Nothing else changes; use 0.1.1 instead of
  0.1.0.

## [0.1.0]

First public release.

- Debugger: `inspect`, `run`, `test`, `diff`, `upgrade-check` and
  `config-bisect` on unchanged Collector configurations, with stage analysis,
  reference-Collector comparison, scenario assertions and JUnit output; a web
  client, HTTP API and MCP server on a team server; runtimes for Collector
  0.160.0 and later and custom components.
- Debugger CLI: `linea` runs standalone with the in-process Collector and no
  server, or against a team server with `--server`. `linea build --manifest
  <ocb.yaml>` compiles the CLI with a manifest's Collector release and
  components on this machine (its own `go`, or `--docker`), for this or
  another platform (`--os`, `--arch`); the result runs them in process
  without Docker or a server, and its `version` names the custom build
  (`build` in `--format json`).
- Debugger team server: the `linea-server` binary and image
  `ghcr.io/lineahq/linea-server`. It links no Collector: every request runs on
  connected workers (`linea-worker`), which must be the same release as the
  server, and `linea-builder` compiles runtimes. Projects with their own
  runtimes, retention and storage limits; a shared library of
  configurations, fixtures and scenarios; role bindings per project; one
  server config file. Requests read no host file or host environment: every
  file comes from the client. A Helm chart, with optional bundled PostgreSQL
  and SeaweedFS (off by default), and a Docker Compose stack in the single
  file `deploy/compose/linea/compose.yaml` with its secrets in a `.env` beside
  it (Docker Engine 26 and Compose v2.30 or later).
- Fleet: OpAMP fleet server with inventory, groups and selectors, optional
  config management, and process-separated plugins with a public SDK, the
  standard-library-only Go module `github.com/lineahq/linea/pluginsdk`.
- Collector components: the `capture` processor and the `livecapture`
  extension, and the live-capture fleet plugin. Linea ships no Collector:
  build your own with OCB from the Go module
  `github.com/lineahq/linea/collector/livecapture`, tagged
  `collector/livecapture/v0.1.0`, and the contrib `opamp` extension.

[0.2.0]: https://github.com/lineahq/linea/releases/tag/v0.2.0
[0.1.1]: https://github.com/lineahq/linea/releases/tag/v0.1.1
[0.1.0]: https://github.com/lineahq/linea/releases/tag/v0.1.0
