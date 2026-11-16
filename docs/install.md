# Installing Linea

| You want | Install |
| --- | --- |
| To debug and test configurations on your machine | [The CLI](#the-cli) |
| The web client, HTTP API or MCP | [A team server](#a-team-server), with Docker Compose or Kubernetes |
| To manage a fleet of Collectors over OpAMP | [The fleet server](fleet/README.md#deploy-with-docker) |
| To capture telemetry from running Collectors | [The Collector components](collector-components/README.md#build-a-collector-with-the-components) in your own Collector build |

## The CLI

Download the archive for Linux, macOS or Windows from the
[releases page](https://github.com/lineahq/linea/releases) and put `linea` on
your `PATH`. Check it with:

```sh
linea version
```

To complete commands and flags with Tab, load `linea completion bash`,
`zsh`, `fish` or `powershell` in your shell profile
([shell completion](debugger/README.md#shell-completion)).

That is all the direct commands (`inspect`, `run`, `test`, `diff`) need: they
run the Collector inside the `linea` process, with no Docker and no server.
Docker is only needed to build runtimes for other Collector releases or custom
components ([runtimes](debugger/runtime-builds.md)).

`go install github.com/lineahq/linea/cmd/linea@vX.Y.Z` does not work: the root
module keeps `replace` directives for its nested modules, which `go install`
refuses. [Build it from source](#from-source) instead.

## A team server

The web client, the HTTP API and MCP run on a team server: `linea-server` with
PostgreSQL, object storage, and workers that run the Collector. It is the
same server however you run it, for yourself or for a team; its
configuration, not the platform, decides how people sign in, where it listens
and what it keeps ([the server config](debugger/team-server.md#the-server-config)).

| Run it with | Configured by | Guide |
| --- | --- | --- |
| Docker Compose, on one machine | Secret files and an optional `.env` beside `compose.yaml` | [Docker Compose](debugger/deploy-compose.md) |
| Kubernetes | The Helm chart's values | [Kubernetes](debugger/deploy-kubernetes.md) |

### With Docker Compose

Docker Compose starts all of it from one stack. It needs Docker Engine 25 or
later with Compose v2.24 or later.

In an empty directory, fetch `compose.yaml` and `.env.example` from a
release's tag, create the secret files in a `secrets/` directory with random
values, and start it. The same stack serves you alone and a team; an optional
`.env`, started from `.env.example`, sets the sign-in, address and limits
([configuration](debugger/deploy-compose.md#configuration)).

```sh
curl -fsSLO https://raw.githubusercontent.com/lineahq/linea/vX.Y.Z/deploy/compose/linea/compose.yaml
curl -fsSLO https://raw.githubusercontent.com/lineahq/linea/vX.Y.Z/deploy/compose/linea/.env.example
mkdir secrets && for name in postgres_password s3_access_key s3_secret_key worker_token builder_token admin_password; do
  openssl rand -hex 32 > secrets/$name
done && touch secrets/oidc_client_secret
chmod 700 secrets
docker compose up -d
```

On Windows, in PowerShell:

```powershell
Invoke-WebRequest https://raw.githubusercontent.com/lineahq/linea/vX.Y.Z/deploy/compose/linea/compose.yaml -OutFile compose.yaml
Invoke-WebRequest https://raw.githubusercontent.com/lineahq/linea/vX.Y.Z/deploy/compose/linea/.env.example -OutFile .env.example
New-Item -ItemType Directory secrets | Out-Null
$rng = [Security.Cryptography.RandomNumberGenerator]::Create()
foreach ($name in 'postgres_password','s3_access_key','s3_secret_key','worker_token','builder_token','admin_password') {
  $b = [byte[]]::new(32); $rng.GetBytes($b)
  [IO.File]::WriteAllText("$PWD\secrets\$name", -join ($b | ForEach-Object { $_.ToString('x2') }))
}
[IO.File]::WriteAllText("$PWD\secrets\oidc_client_secret", '')
docker compose up -d
```

The file from a tag pulls that release's images. The seven secret files must
exist before the first start; `oidc_client_secret` stays empty without an
identity provider. Keep `secrets/postgres_password` as long as you keep the
data: PostgreSQL takes its password when its volume is created
([secrets](debugger/deploy-compose.md#secrets)).

Open `http://127.0.0.1:7070` and sign in as `admin` with the contents of
`secrets/admin_password`. On the start page, **Try an example**
opens one of the shipped samples with its configuration and scenario, ready
to run, so you see a full report before you bring a configuration of your
own. MCP clients connect to `http://127.0.0.1:7070/mcp`. For a team, set up an
identity provider and put your HTTPS reverse proxy in front
([sign-in](debugger/deploy-compose.md#signing-in),
[other machines](debugger/deploy-compose.md#reaching-it-from-other-machines)).

The CLI can use the same server. Without `--server`, a command runs the
Collector inside the `linea` process and keeps nothing. With it, the command
sends the run to the team server: a worker runs it and the server keeps it, so
it appears in the web client's run history, where you can open its report and
save its configuration and scenario. `linea login` asks for the admin password
once and stores an API token.

```sh
linea login --server http://127.0.0.1:7070
linea samples get traces-redact
linea test --server http://127.0.0.1:7070 \
  --config traces-redact/config.yaml --scenario traces-redact/scenario.yaml
linea runs --server http://127.0.0.1:7070
```

The stack is a server instance, PostgreSQL, SeaweedFS, a pool worker and a
runtime builder; idle, together they use about 260 MiB of memory. The CPU and
memory figures in `compose.yaml` are limits, not reservations: a run or a
runtime build may use up to 4 CPUs. Docker refuses a container whose CPU limit
is above the machine's CPU count, so with fewer than 4 CPUs add lower limits
to a `.env` beside `compose.yaml`, for example `LINEA_POOL_CPUS=2` and
`LINEA_BUILDER_CPUS=2`.

Stop it with `docker compose down`; add `-v` to remove the data too.
[Running with Docker Compose](debugger/deploy-compose.md) has every setting.

### With Kubernetes

Each release attaches the Helm chart to its GitHub release. See
[Kubernetes](debugger/deploy-kubernetes.md) and the
[team server](debugger/team-server.md) guide.

Before an upgrade, read [Compatibility](compatibility.md): the server,
builders and workers must match, and runtime images need a rebuild when the
worker protocol changes.

## What a release publishes

- `linea` CLI archives for Linux, macOS and Windows, with `SHA256SUMS`;
- container images on `ghcr.io/lineahq/` for `linux/amd64` and
  `linux/arm64`: `linea-server`, `linea-worker` and `linea-builder` for the
  team server, `linea-fleet` and `linea-fleet-livecapture` for fleet;
- the Helm chart, as a release asset;
- third-party notices: every CLI archive holds `THIRD_PARTY_NOTICES.txt`
  beside `LICENSE`, and every image holds both in `/licenses/`, with the
  license and notice files of everything that binary links (for
  `linea-server` also the bundled web client, which it serves at
  `/licenses.md`);
- Go module tags: `vX.Y.Z` for the root module, `pluginsdk/vX.Y.Z` for the
  fleet plugin SDK, `collector/livecapture/vX.Y.Z` for the Collector
  components and `collector/provider/vA.B.C` for the provider SDK.

The Docker Compose stack is not a release asset: fetch `compose.yaml` and
`.env.example` from the release's tag, as above.

### Verifying a release

The release workflow signs every image and `SHA256SUMS` with
[cosign](https://docs.sigstore.dev/) keyless signing, and attaches an SBOM
and build provenance to every image. To verify them with cosign 2:

```sh
cosign verify ghcr.io/lineahq/linea-server:X.Y.Z \
  --certificate-identity-regexp '^https://github.com/lineahq/linea/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
cosign verify-blob SHA256SUMS --bundle SHA256SUMS.sigstore.json \
  --certificate-identity-regexp '^https://github.com/lineahq/linea/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum --check --ignore-missing SHA256SUMS
```

## From source

You need Git and Go 1.27 or later; the team server also needs Node.js with
pnpm for the web client.

```sh
git clone https://github.com/lineahq/linea.git
cd linea
go generate ./internal/debugger/workersrc
go build ./cmd/linea
```

`go generate` embeds the worker source, which `linea` needs to build runtimes
for other Collector releases and to build itself with other components
(`linea build`, [A CLI of your own](debugger/runtime-builds.md#a-cli-of-your-own)).
For `linea-server`, build the web client first
(`pnpm --dir web install --frozen-lockfile && pnpm --dir web build`), which
`go build ./cmd/linea-server` embeds.

To run the Compose stack with images built from the checkout, create the
secret files as above in `deploy/compose/linea/secrets/` and run, from the
root:

```sh
LINEA_VERSION=dev docker compose -f deploy/compose/linea/compose.yaml up -d --build
```

`dev` keeps the local images from taking a release's tag.
[Development](internals/development.md) has every build and test command.
