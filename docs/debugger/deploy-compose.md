# Running a team server with Docker Compose

`deploy/compose/linea` runs a [team server](team-server.md) on one machine:
for one person on their own computer, or for a team behind its identity
provider. It is one stack; a person and a team differ only in `.env` (how people sign in,
the address and the limits) and in the identity provider's client secret. It
is the same server as on [Kubernetes](deploy-kubernetes.md). The CLI's direct commands
(`run`, `test`, `diff`, `inspect` without `--server`) need no server.

## What it runs

| Service | What it is |
| --- | --- |
| `postgres` | PostgreSQL 17: runs, queue, change feed, sessions, sign-ins, catalog |
| `seaweedfs` | S3-compatible object storage for bundles, reports and evidence |
| `linea` | `linea-server`, one instance, on `LINEA_BIND:LINEA_PORT` (`127.0.0.1:7070`), plain HTTP. It migrates its database and creates its bucket at start. |
| `pool` | `linea-worker --connect --pool`: runs the built-in runtime and the runtimes the builder makes ([pool worker](#the-pool-worker)) |
| `builder` | `linea-builder --connect`: compiles the runtimes that server builds ask for ([builder](#the-builder)) |

Secrets are files in a `secrets/` directory beside `compose.yaml`, which
Compose mounts under `/run/secrets` only in the services that need them
([secrets](#secrets)). SeaweedFS reads its S3 credentials from its secret
files through a small `sh -c` entrypoint, because it takes its admin identity
only from environment variables. Nothing is generated, copied or derived at
start.

## Quick start

You need Docker Engine 25 or later with Compose v2.24 or later. In an empty
directory, fetch `compose.yaml` and `.env.example` from a release's tag, which
pins that release's images:

```sh
curl -fsSLO https://raw.githubusercontent.com/lineahq/linea/vX.Y.Z/deploy/compose/linea/compose.yaml
curl -fsSLO https://raw.githubusercontent.com/lineahq/linea/vX.Y.Z/deploy/compose/linea/.env.example
```

```powershell
Invoke-WebRequest https://raw.githubusercontent.com/lineahq/linea/vX.Y.Z/deploy/compose/linea/compose.yaml -OutFile compose.yaml
Invoke-WebRequest https://raw.githubusercontent.com/lineahq/linea/vX.Y.Z/deploy/compose/linea/.env.example -OutFile .env.example
```

Create the seven secret files, six with random values and the identity
provider's client secret empty:

```sh
mkdir secrets && for name in postgres_password s3_access_key s3_secret_key worker_token builder_token admin_password; do
  openssl rand -hex 32 > secrets/$name
done && touch secrets/oidc_client_secret
chmod 700 secrets
```

```powershell
New-Item -ItemType Directory secrets | Out-Null
$rng = [Security.Cryptography.RandomNumberGenerator]::Create()
foreach ($name in 'postgres_password','s3_access_key','s3_secret_key','worker_token','builder_token','admin_password') {
  $b = [byte[]]::new(32); $rng.GetBytes($b)
  [IO.File]::WriteAllText("$PWD\secrets\$name", -join ($b | ForEach-Object { $_.ToString('x2') }))
}
[IO.File]::WriteAllText("$PWD\secrets\oidc_client_secret", '')
```

`.env` is optional: the stack starts without one. To change a setting, copy
`.env.example` to `.env` and edit it ([configuration](#configuration)).

Start it:

```sh
docker compose up -d
```

Open `http://127.0.0.1:7070` and sign in as `admin` with the contents of
`secrets/admin_password`. Keep `secrets/postgres_password` as long as the data:
PostgreSQL takes its password once, when its volume is created.

Nothing needs registering. The `pool` service runs the `linea-worker` image of
the server's own release: when it connects it registers the built-in runtime,
so requests that name no runtime already run on it. The built-in runtime is
the default runtime until the team chooses another with `linea runtime
default NAME --server http://127.0.0.1:7070` ([the default
runtime](team-server.md#the-default-runtime)).

From the CLI:

```sh
linea login --server http://127.0.0.1:7070
linea samples get logs-cleanup
linea test --server http://127.0.0.1:7070 \
  --config logs-cleanup/config.yaml --scenario logs-cleanup/scenario.yaml
```

`linea login` asks for the admin password and stores an API token.
MCP is at `http://127.0.0.1:7070/mcp`.

To build the images from a checkout instead, create `secrets/` (and `.env`, if
you use one) in `deploy/compose/linea/`, where Git ignores `secrets/`, and
run, from the root of the checkout:

```sh
LINEA_VERSION=dev docker compose -f deploy/compose/linea/compose.yaml up -d --build
```

`dev` keeps your images from taking a release's tag. The first build compiles
the web client, the server and the worker, which links the whole Collector
registry. Use the same `LINEA_VERSION` on every later command for the stack.

## Configuration

An optional `.env` beside `compose.yaml` holds every setting a person
changes, and no secret. `.env.example` documents each line.

The server is configured from the environment: a variable is `LINEA_` plus
the server config key's path in capitals, joined by `_`
(`store.storage_limit` is `LINEA_STORE_STORAGE_LIMIT`). Every variable, the
value syntax (lists and objects are YAML flow values) and the rules for
secrets are in the
[environment variables reference](team-server.md#environment-variables). An
empty variable counts as unset, which is why `compose.yaml` can pass the
optional ones as `${NAME:-}`.

`compose.yaml` fixes the storage, the object store and the two tokens, and
passes these from `.env` to `linea`:

| Settings | Variables |
| --- | --- |
| Address | `LINEA_BIND` (`127.0.0.1`), `LINEA_PORT` (`7070`) |
| Secrets | `LINEA_SECRETS_DIR` (`./secrets`), the directory of the [secret files](#secrets) |
| Admin account | `LINEA_AUTHENTICATION_ADMIN_PASSWORD_FILE`, on by default; set it empty to turn the account off ([the admin account](#the-admin-account)) |
| OIDC sign-in | `LINEA_AUTHENTICATION_OIDC_` plus `ISSUER`, `CLIENT_ID`, `REDIRECT_URL`, `CLI_CLIENT_ID`, `AUDIENCES`, `SCOPES`, `GROUPS_CLAIM`, `NAME_CLAIM`, `SESSION_TTL` |
| Proxy sign-in | `LINEA_AUTHENTICATION_TRUSTED_HEADERS_` plus `PROXIES`, `ISSUER`, `USER`, `GROUPS`, `NAME` |
| Roles | `LINEA_BINDINGS` |
| Bounds | `LINEA_STORE_STORAGE_LIMIT` (`4GiB`), `LINEA_STORE_RUN_LIMIT` (`32MiB`), `LINEA_RETENTION_RUNS`, `LINEA_RETENTION_AGE`, `LINEA_LIMITS_QUEUE`, `LINEA_LIMITS_RUNS_PER_PRINCIPAL`, `LINEA_LIMITS_CAPTURES_PER_PRINCIPAL`, `LINEA_LIMITS_CONNECTIONS`, `LINEA_LIMITS_GIT_CLONE_SIZE` |
| Image export | `LINEA_BUILDS_EXPORT_REPOSITORY` ([image export](#image-export-optional)) |
| Resources | [Resources](#resources) |
| Release | `LINEA_VERSION`, the image tag of `linea-server`, `linea-worker` and `linea-builder`; by default the file's release |

To set another key, add its variable under `environment:` of the `linea`
service in `compose.yaml`. A secret key takes a file secret and its `_FILE`
variable, as the existing ones do.

### Secrets

Each secret is one file in `secrets/` beside `compose.yaml` (set
`LINEA_SECRETS_DIR` in `.env` for another directory), mounted read-only under
`/run/secrets` in the services that need it. They are files because Compose
cannot create a secret from an environment variable or inline content in a
`read_only: true` service, which `linea`, `pool` and `builder` are. Use
`openssl rand -hex 32` for each (64 characters): the tokens need at least 32
characters and the admin password at least 16. Readers trim trailing spaces and
line breaks.

| File | Mounted in | Used for |
| --- | --- | --- |
| `postgres_password` | `postgres`, `linea` | The database user `linea`. Taken once, when the `postgres-data` volume is created; changing it later does not change the database's password. |
| `s3_access_key`, `s3_secret_key` | `seaweedfs`, `linea` | The object store's credentials. SeaweedFS takes the same values as its admin identity, which its entrypoint reads from the files into `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`. |
| `worker_token` | `linea`, `pool` | The pool worker's token |
| `builder_token` | `linea`, `builder` | The builder's token; it must differ from the worker's |
| `admin_password` | `linea` | The built-in admin account's password (used unless `.env` turns the account off) |
| `oidc_client_secret` | `linea` | Your identity provider's client secret (used when `LINEA_AUTHENTICATION_OIDC_ISSUER` is set) |

- All seven files must exist before the first start, even the ones a setup
  does not use: `oidc_client_secret` may be empty without an identity
  provider, and `admin_password` stays when the admin account is off.
- On Linux the containers run as uid 65532 and must be able to read the
  files. The default mode, 0644 from a umask of 022, is enough.
  `chmod 700 secrets` protects the directory on the host and does not affect
  the containers, which get the files themselves through bind mounts.
- Docker makes a directory in place of a missing file, and the service that
  reads it stops with an error naming it. Stop the stack, remove the
  directory (`rmdir secrets/NAME`), create the file, and start again.

### Applying a change

After changing a setting in `.env`, run `docker compose up -d`: Compose
recreates the services whose environment changed. After changing a secret
file, run

```sh
docker compose restart
```

because services read their secrets when they start, and a bind mount shows
the new file after a restart. The exception is `postgres_password`, which
PostgreSQL took when its volume was created: keep it as long as the data.

## Signing in

The server needs at least one sign-in method: the admin password, OIDC or
trusted headers. With none, `linea` stops and says so. They can be combined.
People sign in to the web client; API tokens
([`linea token`](team-server.md#api-tokens)) and the CLI's sign-in serve
scripts and CI.

### The admin account

The server has one built-in account, on by default: the user `admin`, with
the admin role on everything and no binding to write. Its password is the
contents of `secrets/admin_password`, at least 16 characters.

- Web: `/sign-in` shows a password form and, with OIDC, **Sign in with** your
  provider.
- CLI: `linea login --server URL` asks for the password; `--password-stdin`
  reads it from standard input for scripts ([signing
  in](team-server.md#signing-in)).
- Failed attempts are limited to 5 a minute per client address and 20 a minute
  in total, per instance. The server counts the address it sees: behind a
  reverse proxy that is the proxy's.

To turn the account off, once someone signed in through your identity provider
holds the admin role, set `LINEA_AUTHENTICATION_ADMIN_PASSWORD_FILE=` (empty)
in `.env` and run `docker compose up -d`. The account's sessions and API
tokens stop working. Keep the `admin_password` file: Compose still mounts it.

### An identity provider (OIDC)

Register `https://<your host>/auth/callback` with the provider, put its client
secret in `secrets/oidc_client_secret` (the server reads that file once
`LINEA_AUTHENTICATION_OIDC_ISSUER` is set), then set in `.env`:

```sh
LINEA_AUTHENTICATION_OIDC_ISSUER=https://idp.example.com
LINEA_AUTHENTICATION_OIDC_CLIENT_ID=linea
LINEA_AUTHENTICATION_OIDC_REDIRECT_URL=https://linea.example.com/auth/callback
# A public client for linea login's device sign-in (optional):
LINEA_AUTHENTICATION_OIDC_CLI_CLIENT_ID=linea-cli
LINEA_BINDINGS=[{group: linea-admins, role: admin, scope: "*"}, {group: engineering, role: engineer, scope: "*"}]
```

Write the secret file before you run `docker compose up -d`, which applies
the issuer. The redirect URL is the one people reach, so it is the reverse proxy's
([reaching it from other machines](#reaching-it-from-other-machines)). The
other OIDC settings, such as scopes and the groups claim, are in
[the server config](team-server.md#the-server-config).

### An authenticating proxy

For a team behind a proxy that authenticates people already, such as
oauth2-proxy or a company gateway, the server reads the user and groups from
headers, but only on connections from the addresses you name:

```sh
LINEA_AUTHENTICATION_TRUSTED_HEADERS_PROXIES=[10.0.0.0/8]
LINEA_AUTHENTICATION_TRUSTED_HEADERS_ISSUER=oauth2-proxy
LINEA_AUTHENTICATION_TRUSTED_HEADERS_USER=X-Forwarded-User
LINEA_AUTHENTICATION_TRUSTED_HEADERS_GROUPS=X-Forwarded-Groups
```

The server checks the source address as Docker delivers it. A proxy on the
same host usually appears as the gateway address of the `ingress` network, not
as its own address, and Docker Desktop shows a gateway address for every
client. Check which address arrives before trusting a prefix, keep
`LINEA_BIND` on an address only your proxy reaches, and make the proxy set or
remove the identity headers on every request it forwards.

### Roles

`LINEA_BINDINGS` binds groups (or principals) to roles ([roles](team-server.md#roles)).
The admin account needs no binding. A flow list replaces any list from a
`--config` file.

## Reaching it from other machines

The stack serves plain HTTP on `127.0.0.1:7070`. For other machines, put the
reverse proxy that serves your HTTPS in front of it. The server's cookies are
`Secure`: a browser keeps them over `https`, and over plain `http` only on
`127.0.0.1` or `localhost`, so sign-in over `http` to any other address does
not stick.

A proxy on the same host reaches the default `127.0.0.1:7070`. A proxy on
another machine needs `LINEA_BIND` set to an address it can reach (for
example the host's private address); firewall the port to that proxy.

For example, Caddy, which gets and renews its certificate itself:

```text
linea.example.com {
	reverse_proxy 127.0.0.1:7070
}
```

Traefik and nginx work the same way. Four things matter in any of them:

- Pass the `Host` header unchanged (Caddy does; nginx needs `proxy_set_header
  Host $host;`). The password sign-in refuses a request whose `Origin` is not
  the `Host`.
- Report waits (`?wait=true`) are long polls that last as long as a run. Leave
  the read timeout at 10 minutes or more; the CLI sends a timed-out wait
  again.
- Pass WebSocket upgrades if workers outside the stack dial the server
  through the proxy. Workers inside it use `http://linea:7070` on the
  internal `edge` network and never meet the proxy.
- With OIDC, the redirect URL is the proxy's `https://<host>/auth/callback`.

## Health

The server answers `GET /healthz` while it runs and `GET /readyz` while it
also takes new work (it answers 503 once it drains), both without a
credential. `linea-server probe` is the container's healthcheck against
`/healthz`; `pool` and `builder` start once `linea` is healthy.
`docker compose ps` shows the state.

## Resources

The CPU and memory figures are limits, not reservations. Idle, the whole stack
uses about 260 MiB; the pool and the builder use their budgets only while they
run Collectors or compile a runtime. Docker refuses a container whose CPU
limit is above the host's CPU count, so the defaults need at least 4 CPUs; on
a smaller machine lower them. Set these in `.env`:

| Variable | Default | Effect |
| --- | --- | --- |
| `LINEA_SERVER_CPUS` | `2` | CPUs for the server |
| `LINEA_SERVER_MEMORY` | `1g` | Memory for the server |
| `LINEA_SERVER_GOMEMLIMIT` | `768MiB` | The server's Go memory limit; keep it about three quarters of `LINEA_SERVER_MEMORY` |
| `LINEA_POOL_CAPACITY` | `4` | Runs the pool executes at once |
| `LINEA_POOL_CPUS` | `4` | CPUs for the pool and the runtimes it runs |
| `LINEA_POOL_MEMORY` | `4g` | Memory for the pool and the runtimes it runs |
| `LINEA_BUILDER_CPUS` | `4` | CPUs for a runtime build, a whole number |
| `LINEA_BUILDER_MEMORY` | `5g` | Memory for a runtime build, swap included. Compiling every core and contrib component needed 5 GiB in testing; a manifest with fewer components needs less. |

For example, on a 2-CPU, 8 GB machine:

```sh
LINEA_SERVER_CPUS=1
LINEA_POOL_CAPACITY=2
LINEA_POOL_CPUS=2
LINEA_POOL_MEMORY=2g
LINEA_BUILDER_CPUS=2
LINEA_BUILDER_MEMORY=3g
```

PostgreSQL and SeaweedFS keep fixed limits of 1 CPU and 512 MiB each.

Scaled pools (`--scale pool=N`) share the `pool-cache` volume, which is safe only
while they run nothing but the built-in runtime. For more pools that run built runtimes, add services with a volume each.

## Networks

| Network | Members | Internal |
| --- | --- | --- |
| `database` | postgres, linea | yes |
| `storage` | seaweedfs, linea, pool, builder | yes |
| `edge` | linea, pool, builder | yes |
| `ingress` | linea | no (publishes the port, and is the server's way out to an identity provider) |
| `egress` | builder | no (the builder's way out: the module proxy) |

Workers dial the server at `http://linea:7070` on `edge`, and read and write
payloads over 1 MiB directly in SeaweedFS through presigned URLs, which name
`http://seaweedfs:8333`. The pool and the builder have no route out of the
stack except the builder's `egress`. Compose cannot limit `egress` to chosen
hosts; a host firewall can.

## The pool worker

`pool` runs `linea-worker --connect --pool` ([pool
workers](remote-workers.md#pool-workers)). It dials the server with the worker
token and runs one child process per runtime in use, up to `LINEA_POOL_CAPACITY`
requests at once across them:

- its own executable for the built-in runtime, started with the first request
  that needs it and stopped when idle;
- for each runtime the builder makes, the binary it downloads through a
  presigned SeaweedFS URL.

| Topic | Detail |
| --- | --- |
| Network | `edge` and `storage`, no egress. |
| Budget | `LINEA_POOL_MEMORY` and `LINEA_POOL_CPUS` (4 GiB and 4 CPUs by default, [resources](#resources)) for the pool and its children together. It keeps up to 4 idle runtimes warm (`--warm`), stops one idle for 10 minutes (`--idle`) and keeps its cache within 5 GiB (`--cache`); edit the service's command to change them. It sets no `GOMAXPROCS` or `GOMEMLIMIT`, which its children would inherit. |
| Disk | The named volume `pool-cache`, mounted at `/cache`, holds the downloaded binaries. It is mounted executable, unlike the hardened `/tmp`, because the children run from it, and it survives restarts so they start without a download. Removing the volume only costs the downloads. The image creates `/cache` owned by its user (65534), which a new volume copies. |

## The builder

`builder` runs `linea-builder`, which compiles the runtimes that server builds
queue, natively with the Go toolchain of its image
(`deploy/docker/linea-builder.Dockerfile`, with the toolchain of the exact Go
image the CLI builds in; the builder refuses to start with another release). It dials the
server with its own token, the `builder_token` secret (which must differ from
the workers' `worker_token`), reads the manifest and the uploaded sources and writes the
binary through presigned SeaweedFS URLs. It holds no database or bucket
credentials.

| Topic | Detail |
| --- | --- |
| Network | `edge` and `storage` like a worker, and alone on `egress`, the stack's only way to the Internet besides the server's own route out: the module proxy. `GOPROXY` and the other Go module variables come from its environment, as do `NETRC` and `SSL_CERT_FILE` for a proxy that needs credentials or a private CA ([runtime builds](runtime-builds.md#building-on-a-team-server)). Server builds take uploaded sources, not git. |
| Budget | `LINEA_BUILDER_MEMORY` (swap included, 5 GiB by default) and `LINEA_BUILDER_CPUS` (4), one build at a time ([resources](#resources)). |
| Disk | The named volume `builder-work` holds the Go module and build caches, which make rebuilds fast, and each build's directory, removed when the build ends. It is mounted executable, unlike the hardened `/tmp`, because builds start the generator and the worker they compile. Removing the volume only costs the caches. |
| Storage | A runtime binary (about 85 MiB for the default manifest, more with more components) counts toward `LINEA_STORE_STORAGE_LIMIT` (4 GiB here, 256 MiB by default). |

### Image export (optional)

With `LINEA_BUILDS_EXPORT_REPOSITORY` (`builds.export.repository`), the builder
also pushes each runtime it builds as an image, `FROM scratch` like `linea
runtime build` makes, tagged with the build's name, to that repository over
`egress`. The build records the image by digest; a failed push is recorded as
the build's export error and leaves the build done.

- The builder holds the registry credentials: mount a Docker `config.json`
  into `builder` (a file secret, for example) and pass `--registry-auth` with
  its path.
  `--registry-plain-http <host>` allows a registry without TLS.
- `auths` work as they are. A `credHelpers` or `credsStore` entry needs
  `docker-credential-<name>` on the builder's PATH, which the `linea-builder`
  image does not ship: build your own image from it.
- Workers never pull the image: it is for CI and dedicated workers.
- Linea never deletes an exported image. Each export moves the runtime's tag,
  and the previous image stays untagged until the registry's own retention of
  untagged images removes it.

## Images

- `deploy/docker/linea-server.Dockerfile` is the server image, as on
  [Kubernetes](deploy-kubernetes.md).
- `deploy/docker/linea-worker.Dockerfile` builds `linea-worker` from this
  module, so it runs the built-in runtime's Collector release and components
  (`build/linea-runtime.yaml`).

For other releases or components, see [Other runtimes](#other-runtimes).

## Other runtimes

For another Collector release or components, build a runtime on the server. The
builder compiles it, the server registers it, and the pool runs it:

```sh
linea runtime build --server http://127.0.0.1:7070 --name mine --manifest mine.yaml
```

Select it with `--runtime mine`. Requests still run on the built-in runtime
unless they name `mine`; to make `mine` the default, run `linea runtime default
mine --server http://127.0.0.1:7070`. See [using a runtime on a team
server](runtime-builds.md#using-a-runtime-on-a-team-server).

A runtime image built and pushed elsewhere needs workers of its own, on the
stack's `edge` and `storage` networks with the `worker_token` secret, for
example as a service of a second Compose file passed after the stack's
(`docker compose -f compose.yaml -f compose.runtime.yaml up -d`). The stack's
own top-level `secrets` defines `worker_token`, so the service only lists it.
[Running workers](remote-workers.md) says how such
an image joins the catalog.

```yaml
# compose.runtime.yaml
services:
  worker-mine:
    image: registry.example.com/team/linea-runtime:mine
    command: [--connect, "http://linea:7070", --name, mine, --capacity, "4", --token-file, /run/secrets/worker_token]
    secrets: [worker_token]
    networks: [edge, storage]
    depends_on:
      linea:
        condition: service_healthy
    restart: unless-stopped
```

## Fleet and live capture

`compose.fleet.yaml` adds the [fleet server](../fleet/README.md) and its
[live-capture plugin](../fleet/livecapture.md) to the stack and connects
`linea-server` to fleet, so [live capture](livecapture.md) works in the web
client, CLI and MCP. It uses the stack's PostgreSQL with a database each
(`linea`, `fleet`, `livecapture`) and roles that reach only their own database.
Its secrets are more files in the stack's `secrets/` directory, and its
settings are two files that ship beside the overlay; the overlay's header
lists them all:

| File in `secrets/` | Meaning |
| --- | --- |
| `fleet_database_url`, `fleet_migrate_database_url` | Full URLs of the roles `fleet_app` and `fleet_migrator` on the database `fleet`, with random hex passwords: `postgres://fleet_app:<password>@postgres:5432/fleet?sslmode=disable` |
| `capture_database_url`, `capture_migrate_database_url` | The same for `capture_app` and `capture_migrator` on `livecapture` |
| `fleet_token` | `linea-server`'s fleet token, `of1_<32 hex>_<64 hex>` |
| `capture_host_token`, `capture_service_token`, `capture_metrics_token` | The plugin's tokens |
| `capture_tls.crt`, `capture_tls.key` | The plugin's TLS certificate and key. Collectors need the certificate as their CA. The plugin runs as uid 65532, so the key must be readable by it (`chmod 644`). |

| Settings file | Meaning |
| --- | --- |
| `livecapture.json` | The plugin's settings. `raw_principals` is a list of credential IDs, `[]` by default: captures select only points a Collector marks redacted. Who may read raw data is still the debugger's `capture.raw` permission ([raw capture](livecapture.md#raw-capture-and-permissions)). To keep your settings apart from the file an upgrade replaces, copy it and set `LINEA_CAPTURE_CONFIG` in `.env` to the copy's path. |
| `fleet-plugins.json` | The plugin list fleet loads |

Create the files once, in the directory with `compose.yaml`:

```sh
url() { printf 'postgres://%s:%s@postgres:5432/%s?sslmode=disable\n' "$2" "$(openssl rand -hex 32)" "$3" > "secrets/$1"; }
url fleet_database_url fleet_app fleet
url fleet_migrate_database_url fleet_migrator fleet
url capture_database_url capture_app livecapture
url capture_migrate_database_url capture_migrator livecapture
printf 'of1_%s_%s\n' "$(openssl rand -hex 16)" "$(openssl rand -hex 32)" > secrets/fleet_token
for name in capture_host_token capture_service_token capture_metrics_token; do
  openssl rand -hex 32 > secrets/$name
done
openssl req -x509 -newkey rsa:2048 -nodes -days 825 -subj /CN=livecapture-plugin \
  -keyout secrets/capture_tls.key -out secrets/capture_tls.crt \
  -addext 'subjectAltName=DNS:livecapture-plugin,DNS:localhost,IP:127.0.0.1' \
  -addext 'basicConstraints=critical,CA:TRUE' && chmod 644 secrets/capture_tls.key
```

To let the debugger capture raw points, add `linea-server`'s credential ID to
`raw_principals` in `livecapture.json`: it is the second `_`-separated field
of the fleet token (`cut -d_ -f2 secrets/fleet_token`).

Start it from the repository root with `scripts/debugger/compose-fleet.sh`
(add `--build` with `LINEA_VERSION=dev`), and pass both files to every later
`docker compose` command: `-f compose.yaml -f compose.fleet.yaml`. The script
checks that every secret and configuration file exists before it starts
anything, and names a missing one. After changing a fleet secret file, run the
script again, which sets the roles' passwords, and then restart the services
that read it, with both files, as for any secret. After changing
`livecapture.json` or `fleet-plugins.json`, restart `livecapture-plugin` or
`fleet`.

Fleet serves plain HTTP and OpAMP over `ws://`. Put TLS in front before
binding its ports beyond loopback. Fleet only reports unless config
management is turned on ([config management](../fleet/config-management.md)).
**Fleet → Connection** in the web client shows whether the connection works. To keep
fleet on a PostgreSQL of its own, use `deploy/compose/fleet` instead and start
`linea-server` with `--fleet-url` ([Connecting to
fleet](livecapture.md#connecting-to-fleet)).

## Stops and upgrades

The stack runs one instance. A stop or restart makes the API unavailable until
the server is back, a few seconds; queued runs wait. Several instances, for
availability while one restarts, are the [Kubernetes chart](deploy-kubernetes.md)'s
job.

| Event | Behavior |
| --- | --- |
| `docker compose stop` or `restart` | The server drains: it stops claiming work, lets what runs on its workers finish for up to `--drain-timeout` (25 s here), then hands the rest back to the queue without spending an attempt. `stop_grace_period` (45 s) leaves room for that. |
| Server killed | It loses its runs' leases; the restarted server requeues them after 30 s, once, as a new attempt. |
| CLI waits | A report wait that gets a lost connection, or a 502, 503 or 504, is sent again, for up to 30 s of such failures. |

To upgrade, fetch `compose.yaml` and `.env.example` from the new release's tag
(with the fleet overlay, also `compose.fleet.yaml`, `livecapture.json` and
`fleet-plugins.json`, keeping your `raw_principals` unless they are in your
own copy named by `LINEA_CAPTURE_CONFIG`), create any secret file
the new `compose.yaml` lists that you lack, add to `.env` any setting you want
from the new `.env.example`, and run:

```sh
docker compose pull
docker compose up -d
```

The server migrates its database at start. The server, the pool and the
builder must be of one release; `LINEA_VERSION` sets all three. The release
notes in [`CHANGELOG.md`](../../CHANGELOG.md) say what an upgrade needs.

## Stopping and removing it

From the directory with `compose.yaml` (in a checkout, add
`-f deploy/compose/linea/compose.yaml`):

```sh
docker compose down        # keep the data
docker compose down -v     # remove the volumes too
```

The data is in the volumes `postgres-data` (records, queue, sessions) and
`seaweedfs-data` (payloads). Keep `secrets/postgres_password` as long as you
keep the volumes: the database password is set when its volume is created.
