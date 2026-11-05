# Running the shared platform with Docker Compose

`deploy/compose/linea` runs a [team server](team-server.md) on one machine. It
is the shared platform at small scale, and the stack the instance behavior was
checked on. It is also the local way to run the debugger's web client, HTTP API
and MCP: `linea-server` needs a server config and has no in-memory or
loopback-only mode. The CLI's direct commands (`run`, `test`, `diff`, `inspect`
without `--server`) need no server.

| Service | What it is |
| --- | --- |
| `postgres` | PostgreSQL 17: runs, queue, change feed, sessions, sign-ins, catalog |
| `seaweedfs` | S3-compatible object storage for bundles, reports and evidence, bucket `linea` |
| `setup` | One-shot: checks `.env` and writes each service's secrets and configuration ([secrets and configuration](#secrets-and-configuration)) |
| `bucket` | One-shot: creates the bucket, keeps an existing one |
| `linea` | `linea-server --config`, two instances (`LINEA_REPLICAS`) |
| `proxy` | HAProxy on `127.0.0.1:7070`, the only published port |
| `pool` | `linea-worker --connect --pool`: runs the built-in runtime and the runtimes the builder makes, capacity `LINEA_POOL_CAPACITY` (4) ([pool worker](#the-pool-worker)) |
| `builder` | `linea-builder --connect`: compiles the runtimes that server builds ask for ([builder](#the-builder)) |

## Starting it

The stack is one file, `compose.yaml`, which holds the server config and the
load balancer's config too, and it takes its secrets from a `.env` file beside
it. It needs Docker Engine 26 or later with Compose v2.30 or later, for the
volume subpaths the services mount their secrets from.

Put five random values in `.env`. Each needs at least 16 letters, digits or
`. _ ~ -`, and the two tokens must differ:

```sh
for name in POSTGRES_PASSWORD S3_ACCESS_KEY S3_SECRET_KEY WORKER_TOKEN BUILDER_TOKEN; do
  echo "LINEA_$name=$(openssl rand -hex 32)"
done > .env
```

```powershell
$rng = [Security.Cryptography.RandomNumberGenerator]::Create()
'POSTGRES_PASSWORD','S3_ACCESS_KEY','S3_SECRET_KEY','WORKER_TOKEN','BUILDER_TOKEN' | ForEach-Object {
  $b = [byte[]]::new(32); $rng.GetBytes($b)
  "LINEA_$_=" + -join ($b | ForEach-Object { $_.ToString('x2') })
} | Set-Content -Encoding ascii .env
```

| Value | Used for |
| --- | --- |
| `LINEA_POSTGRES_PASSWORD` | The database user `linea`; set when the `postgres-data` volume is created |
| `LINEA_S3_ACCESS_KEY`, `LINEA_S3_SECRET_KEY` | The SeaweedFS identity the server uses |
| `LINEA_WORKER_TOKEN` | The pool worker's token |
| `LINEA_BUILDER_TOKEN` | The builder's token, which must differ from the worker's |

Then start the stack, from the published images or from source:

| From | Where | Command |
| --- | --- | --- |
| Published images | A directory holding `compose.yaml` from a release's tag, `https://raw.githubusercontent.com/lineahq/linea/vX.Y.Z/deploy/compose/linea/compose.yaml`, and `.env` | `docker compose up -d` pulls `ghcr.io/lineahq/linea-server`, `linea-worker` and `linea-builder` tagged `LINEA_VERSION`, by default that release. |
| Source | The root of a checkout, with `.env` in `deploy/compose/linea/` | `LINEA_VERSION=dev docker compose -f deploy/compose/linea/compose.yaml up -d --build`. `dev` keeps your images from taking a release's tag. The first build compiles the web client, the server and the worker, which links the whole Collector registry. |

Use the same `LINEA_VERSION` on every later command for the stack.

Nothing needs registering. The `pool` service runs the `linea-worker` image of
the server's own release: when it connects it registers the built-in runtime
and runs it as its own build, so requests that name no runtime already run on
it. The built-in runtime is the default runtime until the team chooses another
with `linea runtime default NAME --server http://127.0.0.1:7070` ([the default
runtime](team-server.md#the-default-runtime)). The server config sets no
runtime.

Then use it as any team server: the web client at `http://127.0.0.1:7070`,
`--server http://127.0.0.1:7070` on CLI commands, and MCP at
`http://127.0.0.1:7070/mcp`.

```sh
linea samples get logs-cleanup
linea test --server http://127.0.0.1:7070 \
  --config logs-cleanup/config.yaml --scenario logs-cleanup/scenario.yaml
```

Settings, all optional, in `.env` or the shell:

| Variable | Default | Effect |
| --- | --- | --- |
| `LINEA_PORT` | `7070` | The host port on `127.0.0.1` |
| `LINEA_VERSION` | the file's release | The image tag of `linea-server`, `linea-worker` and `linea-builder` |

### Secrets and configuration

The one-shot `setup` service runs before the others on every `up`. It checks
the `.env` values, then writes each service's secrets, and the server and
HAProxy configs from the file's `configs:` section, into that service's own
folder of the `setup` volume. Each service mounts only its folder, read-only,
at `/run/secrets` (HAProxy at `/usr/local/etc/haproxy`), so the hardened,
read-only services never see the other services' secrets. `setup` itself has
no network.

To change the server config or HAProxy's, edit them in `compose.yaml` and run
`docker compose up -d --force-recreate`, so the services read the new files.

### Resources

The CPU and memory figures are limits, not reservations. Idle, the whole stack
uses about 260 MiB; the pool and the builder use their budgets only while they
run Collectors or compile a runtime. Docker refuses a container whose CPU
limit is above the host's CPU count, so the defaults need at least 4 CPUs; on
a smaller machine lower them. Set these in `.env` or the shell:

| Variable | Default | Effect |
| --- | --- | --- |
| `LINEA_REPLICAS` | `2` | Server instances. One is enough on a single machine; two keep the stack up while one restarts. |
| `LINEA_SERVER_CPUS` | `2` | CPUs per server instance |
| `LINEA_SERVER_MEMORY` | `1g` | Memory per server instance |
| `LINEA_SERVER_GOMEMLIMIT` | `768MiB` | The instance's Go memory limit; keep it about three quarters of `LINEA_SERVER_MEMORY` |
| `LINEA_POOL_CAPACITY` | `4` | Runs the pool executes at once |
| `LINEA_POOL_CPUS` | `4` | CPUs for the pool and the runtimes it runs |
| `LINEA_POOL_MEMORY` | `4g` | Memory for the pool and the runtimes it runs |
| `LINEA_BUILDER_CPUS` | `4` | CPUs for a runtime build, a whole number |
| `LINEA_BUILDER_MEMORY` | `5g` | Memory for a runtime build, swap included. Compiling every core and contrib component needed 5 GiB in testing; a manifest with fewer components needs less. |

For example, on a 2-CPU, 8 GB machine:

```sh
# .env, beside the five values
LINEA_REPLICAS=1
LINEA_SERVER_CPUS=1
LINEA_POOL_CAPACITY=2
LINEA_POOL_CPUS=2
LINEA_POOL_MEMORY=2g
LINEA_BUILDER_CPUS=2
LINEA_BUILDER_MEMORY=3g
```

PostgreSQL, SeaweedFS and HAProxy keep fixed limits of 1 CPU each.

Scaled pools (`--scale pool=N`) share the `pool-cache` volume, which is safe only
while they run nothing but the built-in runtime. For more pools that run built runtimes, add services with a volume each.

## Identity

The stack has one user.

- HAProxy drops any `X-Linea-User` and `X-Linea-Groups` header a client sent and
  sets `local` in the group `local-admins`, which the server config binds to
  `admin`.
- The server config trusts these headers only from HAProxy's fixed address
  `10.207.70.10` on the internal `edge` network, and no published port reaches an
  instance directly.
- Anyone who can reach `127.0.0.1:7070` on the machine is that admin. Keep the
  port on loopback, or replace `trusted_headers` with `oidc` ([the server
  config](team-server.md#the-server-config)) before exposing it.
- HAProxy refuses with 421 a request whose `Host` is not `127.0.0.1`,
  `localhost` or `proxy` (the workers' name for it), with any port. A web page
  whose own hostname was rebound to 127.0.0.1 therefore cannot reach the API as
  that admin. To reach the stack under another name, add that name to the
  `allowed_host` rule in the `haproxy_config` of `compose.yaml`.

## Networks

| Network | Members | Internal |
| --- | --- | --- |
| `database` | postgres, linea | yes |
| `storage` | seaweedfs, bucket, linea, pool, builder | yes |
| `edge` | proxy, linea, pool, builder; fixed subnet `10.207.70.0/24` | yes |
| `ingress` | proxy | no (publishes the port) |
| `egress` | builder | no (the only way out of the stack: the module proxy) |

Workers dial the load balancer (`http://proxy:7070`), and read and write
payloads over 1 MiB directly in SeaweedFS through presigned URLs, which name
`http://seaweedfs:8333`.

On a host whose Docker default address pools are used up ("all predefined
address pools have been fully subnetted"), give the other networks subnets in an
override file and pass `-f compose.subnets.yaml` after the stack's file:

```yaml
# compose.subnets.yaml
networks:
  database: {ipam: {config: [{subnet: 10.207.71.0/24}]}}
  storage: {ipam: {config: [{subnet: 10.207.72.0/24}]}}
  ingress: {ipam: {config: [{subnet: 10.207.73.0/24}]}}
```

If `10.207.70.0/24` itself is taken, change the `edge` subnet, HAProxy's address
and the server config's `proxies` together.

## Instances, stops and upgrades

Any instance serves any request: runs, events, suites, sessions, sign-ins and MCP
go through PostgreSQL, and one instance at a time does the housekeeping.

| Event | Behavior |
| --- | --- |
| `docker compose stop` or `restart` | The instance drains: it stops claiming work, lets what runs on its workers finish for up to `--drain-timeout` (25 s here), then hands the rest back to the queue without spending an attempt. `stop_grace_period` (45 s) leaves room for that. |
| Instance killed | It loses its runs' leases; another instance requeues them after 30 s, once, as a new attempt. |
| CLI waits | A wait the load balancer answers with 502, 503 or 504, or a lost connection, is sent again and reaches the other instance. HAProxy's timeouts are 10 minutes for these long polls; a longer wait times out and is sent again too. |

## Stopping and removing it

From the directory with `compose.yaml` (in a checkout, add
`-f deploy/compose/linea/compose.yaml`):

```sh
docker compose down        # keep the data
docker compose down -v     # remove the volumes too
```

Keep `.env` as long as you keep the volumes: the database password is set
when its volume is created.

## The builder

`builder` runs `linea-builder`, which compiles the runtimes that server builds
queue, natively with the Go toolchain of its image
(`deploy/docker/linea-builder.Dockerfile`, with the toolchain of the exact Go
image the CLI builds in; the builder refuses to start with another release). It dials the
load balancer with its own token, `builder_token` (the server config's
`builds.token_file`, which must differ from the workers' token), reads the
manifest and the uploaded sources and writes the binary through presigned
SeaweedFS URLs. It holds no database or bucket credentials.

| Topic | Detail |
| --- | --- |
| Network | `edge` and `storage` like a worker, and alone on `egress`, the stack's only way to the Internet besides the published port: the module proxy. `GOPROXY` and the other Go module variables come from its environment, as do `NETRC` and `SSL_CERT_FILE` for a proxy that needs credentials or a private CA ([runtime builds](runtime-builds.md#building-on-a-team-server)). Server builds take uploaded sources, not git. Compose cannot limit `egress` to chosen hosts; use a host firewall for that. |
| Budget | `LINEA_BUILDER_MEMORY` (swap included, 5 GiB by default) and `LINEA_BUILDER_CPUS` (4), one build at a time ([resources](#resources)). |
| Disk | The named volume `builder-work` holds the Go module and build caches, which make rebuilds fast, and each build's directory, removed when the build ends. It is mounted executable, unlike the hardened `/tmp`, because builds start the generator and the worker they compile. Removing the volume only costs the caches. |
| Storage | A runtime binary (about 85 MiB for the default manifest, more with more components) counts toward the server config's `storage_limit` (4 GiB here, 256 MiB by default). |

### Image export (optional)

With `builds.export.repository` in the server config, the builder also pushes
each runtime it builds as an image, `FROM scratch` like `linea runtime build`
makes, tagged with the build's name, to that repository over `egress`. The
build records the image by digest; a failed push is recorded as the build's
export error and leaves the build done.

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

## The pool worker

`pool` runs `linea-worker --connect --pool` ([pool
workers](remote-workers.md#pool-workers)). It dials the load balancer with the
worker token and runs one child process per runtime in use, up to `LINEA_POOL_CAPACITY` requests at
once across them:

- its own executable for the built-in runtime, started with the first request
  that needs it and stopped when idle;
- for each runtime the builder makes, the binary it downloads through a
  presigned SeaweedFS URL.

| Topic | Detail |
| --- | --- |
| Network | `edge` and `storage`, no egress. |
| Budget | `LINEA_POOL_MEMORY` and `LINEA_POOL_CPUS` (4 GiB and 4 CPUs by default, [resources](#resources)) for the pool and its children together. It keeps up to 4 idle runtimes warm (`--warm`), stops one idle for 10 minutes (`--idle`) and keeps its cache within 5 GiB (`--cache`); edit the service's command to change them. It sets no `GOMAXPROCS` or `GOMEMLIMIT`, which its children would inherit. |
| Disk | The named volume `pool-cache`, mounted at `/cache`, holds the downloaded binaries. It is mounted executable, unlike the hardened `/tmp`, because the children run from it, and it survives restarts so they start without a download. Removing the volume only costs the downloads. The image creates `/cache` owned by its user (65534), which a new volume copies. |

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
stack's `edge` and `storage` networks with the worker token, for example as a
service of a second Compose file passed after the stack's (`-f
compose.runtime.yaml`). [Running workers](remote-workers.md) says how such an
image joins the catalog.

```yaml
# compose.runtime.yaml
services:
  worker-mine:
    image: registry.example.com/team/linea-runtime:mine
    command: [--connect, "http://proxy:7070", --name, mine, --capacity, "4", --token-file, /run/secrets/worker_token]
    volumes:
      # The pool's folder of the setup volume holds the worker token.
      - type: volume
        source: setup
        target: /run/secrets
        read_only: true
        volume: {subpath: pool}
    networks: [edge, storage]
    depends_on: [proxy]
    restart: unless-stopped
```
