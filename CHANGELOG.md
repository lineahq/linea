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
- Fleet binds an agent's OpAMP instance UID to the credential that holds it.
  While the agent is connected, a connection with another credential and the
  same UID is refused with an OpAMP error response and recorded as
  `agent.credential_refused` (and in
  `linea_fleet_agent_credential_refused_total`). Once the agent is
  disconnected, another credential may take the UID over, and fleet records
  `agent.credential_changed`. Collectors that share a UID across credentials
  (cloned identities) now see one of them refused.

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
