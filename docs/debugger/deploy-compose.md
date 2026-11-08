# Running a team server with Docker Compose

`deploy/compose/linea` runs a [team server](team-server.md) on one machine:
for one person on their own computer, or for a team behind its identity
provider. Where it runs does not decide what it is; `.env` does: how people
sign in, the address and port it listens on, TLS, retention and limits
([settings](#settings)). It is the same server as on
[Kubernetes](deploy-kubernetes.md), and the stack the instance behavior was
checked on. The CLI's direct commands (`run`, `test`, `diff`, `inspect`
without `--server`) need no server.

| Service | What it is |
| --- | --- |
| `postgres` | PostgreSQL 17: runs, queue, change feed, sessions, sign-ins, catalog |
| `seaweedfs` | S3-compatible object storage for bundles, reports and evidence, bucket `linea` |
| `setup` | One-shot: checks `.env` and writes the server config, HAProxy's and each service's secrets ([secrets and configuration](#secrets-and-configuration)) |
| `bucket` | One-shot: creates the bucket, keeps an existing one |
| `linea` | `linea-server --config`, one instance by default (`LINEA_REPLICAS`) |
| `proxy` | HAProxy on `LINEA_BIND:LINEA_PORT` (`127.0.0.1:7070`), the only published port |
| `pool` | `linea-worker --connect --pool`: runs the built-in runtime and the runtimes the builder makes, capacity `LINEA_POOL_CAPACITY` (4) ([pool worker](#the-pool-worker)) |
| `builder` | `linea-builder --connect`: compiles the runtimes that server builds ask for ([builder](#the-builder)) |

## Starting it

The stack is one file, `compose.yaml`, configured by a `.env` file beside
it. It needs Docker Engine 26 or later with Compose v2.30 or later, for the
volume subpaths the services mount their secrets from.

Put five random values in `.env`, and choose how people sign in
(`LINEA_AUTH`, [sign-in](#sign-in)). Each value needs at least 16 letters,
digits or `. _ ~ -`, and the two tokens must differ:

```sh
for name in POSTGRES_PASSWORD S3_ACCESS_KEY S3_SECRET_KEY WORKER_TOKEN BUILDER_TOKEN; do
  echo "LINEA_$name=$(openssl rand -hex 32)"
done > .env
echo LINEA_AUTH=local >> .env
```

```powershell
$rng = [Security.Cryptography.RandomNumberGenerator]::Create()
$values = 'POSTGRES_PASSWORD','S3_ACCESS_KEY','S3_SECRET_KEY','WORKER_TOKEN','BUILDER_TOKEN' | ForEach-Object {
  $b = [byte[]]::new(32); $rng.GetBytes($b)
  "LINEA_$_=" + -join ($b | ForEach-Object { $_.ToString('x2') })
}
$values + 'LINEA_AUTH=local' | Set-Content -Encoding ascii .env
```

| Value | Used for |
| --- | --- |
| `LINEA_POSTGRES_PASSWORD` | The database user `linea`; set when the `postgres-data` volume is created |
| `LINEA_S3_ACCESS_KEY`, `LINEA_S3_SECRET_KEY` | The SeaweedFS identity the server uses |
| `LINEA_WORKER_TOKEN` | The pool worker's token |
| `LINEA_BUILDER_TOKEN` | The builder's token, which must differ from the worker's |
| `LINEA_AUTH` | `local`, `oidc` or `proxy`; the stack does not choose one for you |

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
`http://127.0.0.1:7070/mcp` (with your own address, port and `https` when you
set them). With `LINEA_AUTH=local` nobody signs in: `linea login` says so and
every command takes `--server` as it is.

```sh
linea samples get logs-cleanup
linea test --server http://127.0.0.1:7070 \
  --config logs-cleanup/config.yaml --scenario logs-cleanup/scenario.yaml
```

## Settings

Everything else is optional, in `.env` or the shell. After changing `.env`,
run `docker compose up -d --force-recreate`, so `setup` writes the configs
again and the services read them.

| Variable | Default | Effect |
| --- | --- | --- |
| `LINEA_VERSION` | the file's release | The image tag of `linea-server`, `linea-worker` and `linea-builder` |
| `LINEA_BIND` | `127.0.0.1` | The host address the port is published on, such as `0.0.0.0`. `LINEA_AUTH=local` refuses any but `127.0.0.1` and `[::1]`. |
| `LINEA_PORT` | `7070` | The host port |
| `LINEA_TLS_CERT_FILE`, `LINEA_TLS_KEY_FILE` | none | PEM certificate (chain) and key; set both and the port serves HTTPS only ([TLS](#tls)) |
| `LINEA_ADMIN_GROUP`, `LINEA_ENGINEER_GROUP`, `LINEA_VIEWER_GROUP` | none | The groups bound to the `admin`, `engineer` and `viewer` roles with `oidc` or `proxy` ([roles](team-server.md#roles)); admin is required |
| `LINEA_STORAGE_LIMIT` | `4GiB` | The server config's `storage_limit` |
| `LINEA_RUN_LIMIT` | `32MiB` | The server config's `run_limit` |
| `LINEA_RETENTION_RUNS`, `LINEA_RETENTION_AGE` | none | `retention.runs` and `retention.age`, such as `500` and `30d` |
| `LINEA_QUEUE_LIMIT` | the server's | `limits.queue` |
| `LINEA_BUILD_EXPORT` | none | `builds.export.repository` ([image export](#image-export-optional)) |

Values go into YAML and HAProxy's configuration, so `setup` checks their
characters and stops with the variable's name when one does not fit. The
[server config](team-server.md#the-server-config) has more settings than
these; to set one, edit the script of the `setup` service in `compose.yaml`
that writes it.

### Secrets and configuration

The one-shot `setup` service runs before the others on every `up`. It checks
`.env`, then writes the server config, HAProxy's configuration and each
service's secrets into that service's own folder of the `setup` volume. Each
service mounts only its folder, read-only, at `/run/secrets` (HAProxy at
`/usr/local/etc/haproxy`), so the hardened, read-only services never see the
other services' secrets. `setup` itself has no network. The OIDC client
secret and the TLS key reach it as Compose configs, not as environment
variables, so `docker inspect` does not show them.

## Sign-in

`LINEA_AUTH` is required; an empty or unknown value stops `setup` with the
choices.

| `LINEA_AUTH` | Who signs in | Required with it |
| --- | --- | --- |
| `local` | Nobody: HAProxy signs every request in as the admin `local` | `LINEA_BIND` of `127.0.0.1` (the default) or `[::1]` |
| `oidc` | People, through your identity provider; the CLI through `linea login` | `LINEA_PUBLIC_URL`, `LINEA_OIDC_ISSUER`, `LINEA_OIDC_CLIENT_ID`, `LINEA_OIDC_CLIENT_SECRET`, `LINEA_ADMIN_GROUP` |
| `proxy` | An authenticating proxy in front of the stack names the user | `LINEA_TRUSTED_PROXIES`, `LINEA_USER_HEADER`, `LINEA_GROUPS_HEADER`, `LINEA_ADMIN_GROUP` |

In every mode workers and the builder reach the server through HAProxy's
internal `workers` frontend (`http://proxy:7070`), which has no published
port and removes every identity header; they present their tokens. API
tokens ([`linea token`](team-server.md#api-tokens)) work in every mode.

### local

For one person on their own machine.

- HAProxy drops any `X-Linea-User` and `X-Linea-Groups` header a client sent and
  sets `local` in the group `local-admins`, which the server config binds to
  `admin`.
- The server config trusts these headers only from HAProxy's fixed address
  `10.207.70.10` on the internal `edge` network, and no published port reaches an
  instance directly.
- Anyone who can reach the port on the machine is that admin, so `setup`
  refuses any `LINEA_BIND` but loopback.
- HAProxy refuses with 421 a request whose `Host` is not `127.0.0.1`,
  `localhost` or `[::1]`, with any port. A web page whose own hostname was
  rebound to 127.0.0.1 therefore cannot reach the API as that admin.

### oidc

For a team: the server's own OIDC sign-in ([the server
config](team-server.md#the-server-config)).

| Variable | Effect |
| --- | --- |
| `LINEA_PUBLIC_URL` | The URL people open, such as `https://linea.example.com`. The callback is `LINEA_PUBLIC_URL/auth/callback`; register it with the provider. Use `https`, or `http` on loopback only. |
| `LINEA_OIDC_ISSUER` | The provider's issuer URL |
| `LINEA_OIDC_CLIENT_ID`, `LINEA_OIDC_CLIENT_SECRET` | The web client's confidential client |
| `LINEA_OIDC_CLI_CLIENT_ID` | Optional: a public client for `linea login` |
| `LINEA_OIDC_GROUPS_CLAIM` | Optional: the claim that lists groups, `groups` by default |
| `LINEA_OIDC_SCOPES` | Optional: extra scopes, comma-separated, such as `groups,offline_access` |

The server's session cookies are `Secure`, so browsers send them only over
HTTPS (or to `localhost`): set `LINEA_TLS_CERT_FILE` and `LINEA_TLS_KEY_FILE`,
or put a TLS-terminating proxy in front and point `LINEA_PUBLIC_URL` at it.

### proxy

For a team behind a proxy that authenticates people already, such as
oauth2-proxy or a company gateway. HAProxy passes the proxy's identity headers
to the server only from the addresses in `LINEA_TRUSTED_PROXIES` and removes
them from every other request.

| Variable | Effect |
| --- | --- |
| `LINEA_TRUSTED_PROXIES` | Comma-separated addresses or CIDR prefixes of your proxy, as HAProxy sees them |
| `LINEA_USER_HEADER`, `LINEA_GROUPS_HEADER` | The headers that carry the user and their comma-separated groups, such as `X-Forwarded-User` and `X-Forwarded-Groups` |
| `LINEA_NAME_HEADER` | Optional: a header with a display name |
| `LINEA_PROXY_ISSUER` | Optional: the issuer principals are recorded under, `proxy` by default; changing it gives everyone new principal IDs |

HAProxy sees the source address as Docker delivers it. On Linux with the
default bridge that is the client's own address; Docker Desktop and some
userland-proxy setups show a gateway address instead, which every client on
the host shares. Check what arrives before trusting a prefix, and keep
`LINEA_BIND` on an address only your proxy reaches.

## TLS

With `LINEA_TLS_CERT_FILE` and `LINEA_TLS_KEY_FILE` (paths relative to
`compose.yaml`, or absolute), HAProxy serves HTTPS on the published port and
plain HTTP not at all. The certificate file holds the certificate and any
intermediates, PEM-encoded; the key is a PEM key without a passphrase.
Replacing the files takes `up -d --force-recreate`. The `workers` frontend
stays plain HTTP inside the internal `edge` network.

### Resources

The CPU and memory figures are limits, not reservations. Idle, the whole stack
uses about 260 MiB; the pool and the builder use their budgets only while they
run Collectors or compile a runtime. Docker refuses a container whose CPU
limit is above the host's CPU count, so the defaults need at least 4 CPUs; on
a smaller machine lower them. Set these in `.env` or the shell:

| Variable | Default | Effect |
| --- | --- | --- |
| `LINEA_REPLICAS` | `1` | Server instances. One is enough on a single machine; two keep the stack up while one restarts ([instances](#instances-stops-and-upgrades)). |
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

## Networks

| Network | Members | Internal |
| --- | --- | --- |
| `database` | postgres, linea | yes |
| `storage` | seaweedfs, bucket, linea, pool, builder | yes |
| `edge` | proxy, linea, pool, builder; fixed subnet `10.207.70.0/24` | yes |
| `ingress` | proxy | no (publishes the port) |
| `egress` | builder | no (the only way out of the stack: the module proxy) |

Workers dial the load balancer's `workers` frontend (`http://proxy:7070`),
which has no published port, and read and write
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

If `10.207.70.0/24` itself is taken, change the `edge` subnet, HAProxy's
address and the trusted proxy `10.207.70.10` in the `setup` script together.

## Fleet and live capture

`compose.fleet.yaml` adds the [fleet server](../fleet/README.md) and its
[live-capture plugin](../fleet/livecapture.md) to the stack and connects
`linea-server` to fleet, so [live capture](livecapture.md) works in the web
client, CLI and MCP. All three use the stack's one PostgreSQL server with a
database each (`linea`, `fleet`, `livecapture`). Fleet and the plugin get their
own migration and runtime roles, which can reach only their own database.

Start it, from the repository root, the first time and after every upgrade:

```sh
scripts/debugger/compose-fleet.sh            # add --build with LINEA_VERSION=dev
```

The script is safe to repeat. It:

1. creates the databases and roles, and on every run resets the role
   passwords from the setup volume;
2. stops fleet and the plugin and migrates both schemas (the plugin refuses
   to migrate while it runs);
3. installs `linea-server`'s fleet token as the first fleet admin. The setup
   volume generates the token once and nothing prints it; the admin does not
   expire, and a repeated run changes nothing while it is active;
4. starts the stack with both files.

After that, pass both files to every `docker compose` command:
`-f compose.yaml -f compose.fleet.yaml`.

| Setting (in `.env`) | Default |
| --- | --- |
| `LINEA_FLEET_SERVER_ID` | `linea-fleet`. Bootstrap records it; changing it later stops fleet from opening its database |
| `LINEA_FLEET_PORT` | `8080`, the fleet management API (`/api/v1`) |
| `LINEA_FLEET_AGENT_PORT` | `4320`, OpAMP for Collectors (`ws://<host>:4320/v1/opamp`) |
| `LINEA_CAPTURE_CONTROL_PORT`, `LINEA_CAPTURE_DELIVERY_PORT` | `4340`, `4341`, the live-capture extension's control and delivery endpoints (TLS with the plugin's self-signed certificate) |
| `LINEA_FLEET_BIND` | `127.0.0.1` for all four |
| `LINEA_FLEET_CONFIG_MANAGEMENT` | `false`, fleet only reports. `true` starts it with `--config-management`: config versions, assignments and rollouts for supervised Collectors ([config management](../fleet/config-management.md)) |

Fleet serves plain HTTP and OpAMP over `ws://`, and `linea-server` reaches it
with `--fleet-allow-http` on the internal `fleet` network. Put TLS in front
before binding these ports beyond loopback. The plugin's certificate is in the
setup volume at `livecapture/tls_cert.pem`; Collectors need it as their CA.
Collectors on the same host can join the external networks
`linea_fleet-agents` and `linea_capture-ingress` and use the service names
`fleet` and `livecapture-plugin`.

Fleet only reports unless `LINEA_FLEET_CONFIG_MANAGEMENT=true`; recreate fleet
after changing it (`compose-fleet.sh`). **Fleet → Connection** in the web
client shows whether it is on. To keep
fleet on a PostgreSQL of its own, use `deploy/compose/fleet` instead and start
`linea-server` with `--fleet-url` ([Connecting to
fleet](livecapture.md#connecting-to-fleet)).

## Instances, stops and upgrades

The stack runs one instance; set `LINEA_REPLICAS=2` or more to keep it
serving while one restarts. Any instance serves any request: runs, events,
suites, sessions, sign-ins and MCP go through PostgreSQL, and one instance at a
time does the housekeeping. With one instance, a stop or restart makes the API
unavailable until it is back; queued runs wait and the CLI's waits retry.

| Event | Behavior |
| --- | --- |
| `docker compose stop` or `restart` | The instance drains: it stops claiming work, lets what runs on its workers finish for up to `--drain-timeout` (25 s here), then hands the rest back to the queue without spending an attempt. `stop_grace_period` (45 s) leaves room for that. |
| Instance killed | It loses its runs' leases; another instance requeues them after 30 s, once, as a new attempt. |
| CLI waits | A wait the load balancer answers with 502, 503 or 504, or a lost connection, is sent again and reaches another instance, or the same one once it is back. HAProxy's timeouts are 10 minutes for these long polls; a longer wait times out and is sent again too. |

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

With `LINEA_BUILD_EXPORT` (`builds.export.repository` in the server config), the builder also pushes
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
