# Collector checks in CI

Two commands make a Collector configuration testable in CI. Both run the
debugger locally on the runner, with every exporter captured in-process: no
backend, no network and no `linea-server`.

| Command | What it does |
| --- | --- |
| `test` | Runs regression scenarios against the configuration and reports their assertions. |
| `diff` | Runs two configurations on one scenario and says whether the exporters received the same records. |

Examples:

- [`examples/ci/github-actions.yml`](../../examples/ci/github-actions.yml) is a
  complete GitHub Actions workflow using both. A test runs its commands on the
  `logs-cleanup` sample, so it keeps working as the commands change.
- [`examples/ci/github-actions-server.yml`](../../examples/ci/github-actions-server.yml)
  is the same workflow with the runs sent to a team server ([Run on a team
  server](#run-on-a-team-server)). No test runs it.

**Export for CI** in saved work (`POST /api/v1/artifacts/export`) builds a zip
with this workflow, the chosen saved configuration as `collector/config.yaml`
and the scenarios under `collector/tests/`. Unpack it at the repository root and
set the two pinned-build values below. Its `linea.json` records which saved
revisions it holds, so **Import** of the edited files saves them as new
revisions of the same artifacts.

| Interface | Export | Import |
| --- | --- | --- |
| CLI | `linea artifacts export --server URL --config ID --scenario ID... --out collector-ci.zip` | `linea artifacts import --server URL --archive collector-ci.zip` |
| MCP | `linea_export_artifacts_for_ci` | `linea_import_artifacts` |

## Install a pinned build

Pin the debugger to one release or commit, never a moving branch: an exported
scenario reproduces its source run's outcome only with the same build (see
[Export a regression scenario for CI](README.md#export-a-regression-scenario-for-ci)).

The simplest pin is a release archive. Each release publishes
`linea_<version>_<os>_<arch>` archives (`.tar.gz`, and `.zip` for Windows) with
a `SHA256SUMS` file:

```sh
VERSION=0.1.1
curl -fsSL -o linea.tar.gz \
  "https://github.com/lineahq/linea/releases/download/v$VERSION/linea_${VERSION}_linux_amd64.tar.gz"
tar -xzf linea.tar.gz        # unpacks linea_0.1.1_linux_amd64/linea
```

- The example workflows install the release that `LINEA_VERSION` names the same
  way, and check the archive against `SHA256SUMS` before they unpack it.
- Each release sets `LINEA_VERSION` in the examples to its own version. Pin the
  version you tested and raise it on purpose.
- The CLI needs neither Go nor the web client on the runner.

## Regression scenarios: `test`

```sh
linea test --config collector/config.yaml --tests collector/tests --report junit.xml
```

- `--tests` runs every `*.yaml` and `*.yml` scenario of the directory in name
  order; `--scenario FILE` (repeatable) names them instead.
- Write scenarios by hand, or export one from a run you checked in the web
  client or through MCP:

```sh
linea export --server https://linea.example.com --run <run-id> --out collector/tests/regression.scenario.yaml
```

Review an exported file before committing it: its inputs and mock files are
recorded telemetry, inline. Its header names the environment variables the check
needs. Set them in the workflow and pass `--import-env NAME` for each
(`linea test --import-env OTEL_TEAM ...`). The debugger never reads the host
environment otherwise: a name without `--import-env` gets a stand-in value, and
assertions that depend on it are inconclusive (exit 4).

| Exit code | Meaning |
| --- | --- |
| 0 | Every assertion passed |
| 1 | Error: a configuration or scenario could not run |
| 3 | An assertion failed |
| 4 | Inconclusive: the evidence cannot decide an assertion |

`--report junit.xml` writes one test suite per scenario and one test case per
assertion; `--report FILE.json` writes the JSON report. An inconclusive
assertion is a JUnit failure of type `inconclusive`, never a skipped test,
because CI systems count skipped tests as passing.

## Pull request comparison: `diff`

```sh
git show "origin/$BASE_REF:collector/config.yaml" > base-config.yaml
linea diff --before base-config.yaml --after collector/config.yaml \
  --scenario collector/tests/regression.scenario.yaml --format markdown
```

Both configurations run unmodified on byte-identical captured inputs: the
scenario's inputs and mock files are read once and shared. `--fixture FILE.json`
compares a quick run of an OTLP JSON payload instead. The comparison reads only
the two runs' recorded evidence:

- per exporter, whether both runs delivered the same multiset of records, each
  with its resource and scope (equal counts alone are never the same);
- per injected record, where it ended in each run and which fields changed, by
  OTTL path, at the exporters both runs delivered it to;
- changed routes, recorded errors and assertion outcomes.

| Exit code | Meaning |
| --- | --- |
| 0 | Same: every exporter received the same records |
| 1 | Error: a configuration or input could not run |
| 2 | Different |
| 4 | Inconclusive: a run did not complete, its evidence was truncated, different builds ran, or the inputs were not identical |

| Option | Effect |
| --- | --- |
| `--ignore PATH` (repeatable) | OTTL paths, such as `log.attributes["build.id"]`, whose differences are counted in the result but do not make the runs different |
| `--format` | `text` (default, a short listing), `markdown`, or `json` (the platform's comparison document) |

A pull request that changes behavior on purpose is expected to be `different`.
The example puts the Markdown in the job summary and fails the job only on 4 or
1. Fail on 2 as well for changes that must not alter output, such as a
refactoring.

`git show` writes the base configuration elsewhere, so files a configuration
reads through relative `${file:...}` references are resolved from there: keep
such references absolute, or compare checked-out copies of both branches. A pull
request that adds the configuration has no base version, and `git show` fails
the step.

## Your own Collector build: `--runtime`

By default both commands run the debugger's built-in Collector. To test with the
Collector version and components you ship, run them on a runtime built from your
OCB manifest. The runner needs no Docker and no access to a team server, only to
your registry:

```sh
# Once, on a team server whose server config sets builds.export.repository:
linea runtime build --server https://linea.example.com --name prod --manifest collector/ocb.yaml
#   built runtime prod (Collector 0.162.0)
#   exported image registry.example.com/team/runtimes@sha256:4f1c...
# In CI:
linea test --runtime registry.example.com/team/runtimes@sha256:4f1c... --config "$CONFIG" --tests "$TESTS" --report junit.xml
```

A `--runtime` value with `/`, `:` or `@` is an image reference. linea:

1. reads the image without Docker, with the credentials a Docker client would use
   (`DOCKER_CONFIG` or `~/.docker/config.json`, credential helpers included);
2. checks it against its digests and refuses an image that is not a Linea
   runtime;
3. keeps its binary in `~/.cache/linea/runtimes` (the user cache directory;
   `--runtime-cache DIR` elsewhere);
4. runs the binary as a child process and checks that it reports the Collector
   release the image's labels name.

- A reference by digest that is cached reads no registry, so caching that
  directory between jobs (as the example workflow does) lets later jobs run
  offline. A tag is resolved on every run.
- `linea runtime prune --binaries` empties the cache.
- When the export fails, the build still succeeds and `runtime build` prints a
  warning with the reason.
- Runtime images hold a Linux binary of the builder's architecture (linux/amd64
  on the usual builder), which runs natively only on a Linux host of that
  architecture. On another host, or with Docker on the runner, import the image
  and name the runtime: `linea runtime import REF --pull`, then `--runtime NAME`.
- A runtime run from its binary has the runner's rights, like the built-in
  Collector. A runtime in Docker runs without network, on a read-only file
  system, as an unprivileged user. Build custom components you do not trust into
  an image you run in Docker.

| Topic | Detail |
| --- | --- |
| Commands | `test`, `diff`, `run`, `inspect`, `ottl preview`, `statement candidate` and `upgrade-check` take `--runtime` (`diff --after-runtime`, `upgrade-check --from` and `--to`). Without it they use the built-in Collector, so carry the flag in every CI command. |
| Release | The runtime runs the Collector release of its manifest. When you write the manifest with `runtime manifest` or `runtime retarget`, pass `--version` to pin the release; otherwise they use the newest verified one ([Which releases build](runtime-builds.md#which-releases-build)). |
| Protocol | linea refuses a runtime whose worker speaks another protocol than this debugger build (`unsupported_build`), so pin the debugger and the runtime images together. |
| Upgrade | `diff --runtime A --after-runtime B` runs the base configuration on one runtime and the pull request's on another. |
| Example | The workflow passes `--runtime "$RUNTIME"`; empty is the built-in Collector. |
| `--server` | `--runtime` names a runtime of the server's catalog; an image reference is refused there. |

## Collector upgrades: `upgrade-check`

Before moving to a new Collector release, check that your configuration still
loads and your scenarios still behave the same on it. Build a runtime of the
release you run today and one of the target (`runtime retarget` moves your
manifest to the target's module versions), then:

```sh
linea runtime retarget --manifest collector/ocb.yaml --version 0.162.0 --out ocb-0.162.yaml
linea runtime build --name next --manifest ocb-0.162.yaml
linea upgrade-check --from prod --to next --config "$CONFIG" --tests "$TESTS" --format markdown > upgrade.md
```

The check:

- validates the configuration on both runtimes and names the diagnostics the
  target adds or no longer reports;
- lists the feature gates of the configuration's components (and the
  Collector's own) whose stage or default changed;
- when both runtimes accept the configuration, runs each scenario on both on one
  captured input and compares the runs. A difference in the Collector version
  alone never makes a scenario different.

| Exit code | Verdict |
| --- | --- |
| 0 | `compatible`: the configuration loads and every scenario behaves the same |
| 1 | Usage, a missing runtime, the same runtime twice, or another error |
| 2 | `configuration_breaks` (the target refuses the configuration) or `behavior_changes` (a scenario's output or assertions differ) |
| 4 | `inconclusive`: a scenario could not be compared, or the current runtime already refuses the configuration; never a pass |

- `--format json` prints the check (`upgrade_check_schema` 0.1); `--format
  markdown` a pull request comment. `--server` sends the check to a team server.
- Feature gate changes are listed for review and never change the verdict.
- `--from` defaults to the built-in runtime; `--to built-in` names it as the
  target.

## Run on a team server

To run the commands on your team server's workers instead, for example on a
runtime built from your OCB manifest and registered there, add `--server URL`
and present an API token:

```sh
export LINEA_TOKEN=...   # from the CI system's secret store
linea test --server https://linea.example.com --config collector/config.yaml --tests collector/tests --report junit.xml
```

The example workflow reads the URL from the repository variable `LINEA_SERVER`
and the token from the secret `LINEA_TOKEN`. Make the token with
`linea token create --server URL --label ci --service ci --expires-in-days 90`
and bind `service:ci` to a role that may submit runs in the server config ([API
tokens](team-server.md#api-tokens)). Exit codes and `--report` are the same as
for direct runs. What differs:

- The CLI reads the configuration, the scenarios and the files the scenarios
  name on the runner and sends them inline. The server reads no runner path.
- The server reads no host environment variable. `--import-env` is ignored:
  declare the values in the scenario.
- The token is sent only over https, or to a loopback URL. The runner must reach
  the server.
- `--runtime NAME` names a runtime registered on the server
  ([runtimes](runtime-builds.md#using-a-runtime-on-a-team-server)); the runner
  needs no Docker. A run queues until a worker of that runtime is connected.
- Runs are kept in the server's history with their reports, so a failed check
  can be opened in the web client.

## Other CI systems

Nothing in the commands is specific to GitHub Actions: build the pinned
debugger, run `test` and `diff`, and map the exit codes as the tables say.
