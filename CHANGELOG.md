# Changelog

Notable changes in each release. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/). Before 1.0, a minor release may
change APIs, report schemas and persisted formats; the release notes say how.

## Unreleased

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

[0.1.1]: https://github.com/lineahq/linea/releases/tag/v0.1.1
[0.1.0]: https://github.com/lineahq/linea/releases/tag/v0.1.0
