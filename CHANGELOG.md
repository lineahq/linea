# Changelog

Notable changes in each release. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/). Before 1.0, a minor release may
change APIs, report schemas and persisted formats; the release notes say how.

## Unreleased

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
  The server needs at least one sign-in method (the admin password, OIDC or
  trusted headers). The trusted-header "local" sign-in the Compose stack used
  is gone.
- The server creates its bucket at start when `store.objects.create_bucket` is
  true (off by default; the Compose stack turns it on).
- `GET /healthz` and `GET /readyz` answer without a credential: the second
  answers 503 once the server drains. `linea-server probe` checks `/readyz`
  (`--url` picks another) and is the Compose healthcheck.

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
