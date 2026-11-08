# Deploying a team server on Kubernetes

The Helm chart in `deploy/charts/linea` installs a [team server](team-server.md),
a worker pool that runs the runtimes the server builds, and a builder that
compiles them. For the same server on one machine, see
[Docker Compose](deploy-compose.md).

- The server runs as identical instances (`server.replicas`, default 2).
- Records live in PostgreSQL and payloads in S3-compatible object storage.
  Bring your own, or let the chart run single-pod ones ([Stores](#stores)).
- Collectors run only in the workers.
- Every key is documented in
  [`values.yaml`](../../deploy/charts/linea/values.yaml); this guide covers
  what an operator decides and sets up.

Implementation notes (probes, volumes, shutdown, store pods) are in
[the chart's internals](../internals/debugger-kubernetes-chart.md).

## Images

Three images run by default. Each release publishes them to `ghcr.io/lineahq/`
tagged with the release version. The released chart and the chart in a
checkout both default their image tags to their `appVersion` (the release), so
both use the published images. To run your own, build from the repository root
and push to your registry.

| Image | Dockerfile | Runs as | What it is |
| --- | --- | --- | --- |
| `linea-server` | `deploy/docker/linea-server.Dockerfile` | 65532 | The server: web client and `linea-server` binary in a distroless image. No Collector, no worker source, no toolchain. |
| `linea-worker` | `deploy/docker/linea-worker.Dockerfile` | 65534 | The pool worker (`pool.image`). Serves any runtime the server builds, downloading each binary on first use. |
| `linea-builder` | `deploy/docker/linea-builder.Dockerfile` | 65532 | The builder (`builder.image`): pinned Go toolchain and worker source. Compiles natively, no Docker daemon. |

```sh
docker build -f deploy/docker/linea-server.Dockerfile -t registry.example.com/team/linea-server:1.0 .
docker build -f deploy/docker/linea-worker.Dockerfile -t registry.example.com/team/linea-worker:1.0 .
docker build -f deploy/docker/linea-builder.Dockerfile -t registry.example.com/team/linea-builder:1.0 .
docker push registry.example.com/team/linea-server:1.0   # and the other two
```

Then set `image`, `pool.image` and `builder.image` to what you pushed.

- The Go builds compile 2 packages in parallel. On a constrained host:
  `docker build --build-arg GO_BUILD_PARALLELISM=1 --build-arg GOMAXPROCS=2 ...`.
- A dedicated runtime image is optional, for a runtime that must run in its
  own container, such as a pinned production Collector. `linea runtime build`
  makes it on a workstation with Docker, and `runtime push` pushes it. Pin the
  manifest to your Collector release: without `--version`, `runtime manifest`
  uses the newest release in the catalog.

```sh
linea runtime manifest --version 0.162.0 \
  --component receivers/otlp --component exporters/debug --out ocb.yaml
linea runtime build --name collector-0.162 --manifest ocb.yaml
linea runtime push collector-0.162 registry.example.com/team/runtime:collector-0.162
```

Add every component your configurations use (`--component`, or `--config
collector.yaml` to take them from a configuration) and `--source` for your own.
See [building debug Collectors](runtime-builds.md). Run the image with
`worker.enabled: true` (see [Workers, the pool and
builders](#workers-the-pool-and-builders)).

## Install

Each release attaches the packaged chart, `linea-<version>.tgz`, to its
[GitHub release](https://github.com/lineahq/linea/releases). It needs no image
settings:

```sh
helm install linea ./linea-0.1.1.tgz -n linea --create-namespace -f values.yaml
```

From a checkout, install the chart directory, which uses the same images:

```sh
helm install linea deploy/charts/linea -n linea --create-namespace -f values.yaml
```

The values below name images you built; with the published images, leave out
`image`, `pool.image` and `builder.image`.

### Stores

The server needs a PostgreSQL database (it creates and migrates its own
tables) and a bucket in an S3-compatible store.

| Option | Values | Use for |
| --- | --- | --- |
| Your own (default) | `store.databaseURL` or `store.existingSecret`, `store.objects.*`, `networkPolicy.objectStorage` | Production: HA PostgreSQL, a durable bucket |
| Bundled PostgreSQL 17 | `postgresql.enabled: true` | Evaluation, small teams |
| Bundled SeaweedFS (S3 gateway) | `seaweedfs.enabled: true` | Evaluation, small teams |

The bundled stores are one StatefulSet pod each with a PersistentVolumeClaim
(`persistence.size`, `persistence.storageClassName`). They have no
replication, no backups and no TLS inside the cluster.

- The chart generates their passwords and S3 keys once and keeps them across
  upgrades. Set `postgresql.password` or `seaweedfs.accessKey` and
  `seaweedfs.secretKey` to choose them yourself.
- A Job per release creates the bucket (`seaweedfs.bucket`, default `linea`).
- Each can be enabled on its own. An enabled store replaces its part of
  `store`, so leave that part empty. Neither can be combined with
  `store.existingSecret`.
- With `seaweedfs.enabled`, the pool and builders reach it without
  `networkPolicy.objectStorage`. With `networkPolicy.enabled`, only the server
  reaches PostgreSQL, and only chart pods reach SeaweedFS.
- Kept credentials are read from the cluster at render time. Under
  `helm template` or a renderer without cluster access they are generated
  anew on each render, so set them yourself there.

```yaml
postgresql: {enabled: true}
seaweedfs: {enabled: true}
```

### Example values

Your own PostgreSQL and object storage, OIDC sign-in, fleet, and an ingress
that ends TLS. `networkPolicy.objectStorage` is required with your own store
(see [Egress](#egress)); here it reaches an external store:

```yaml
image:
  repository: registry.example.com/team/linea-server
  tag: "1.0"
store:
  existingSecret: linea-store      # keys database-url, access-key, secret-key
  objects:
    endpoint: https://s3.example.internal
    bucket: linea
config:
  authentication:
    oidc:
      issuer: https://dex.example.com
      client_id: linea
      redirect_url: https://debugger.example.com/auth/callback
      cli_client_id: linea-cli
      scopes: [openid, profile, email, groups]
  bindings:
    - {group: platform, role: engineer, scope: "*"}
    - {group: platform-admins, role: admin, scope: "*"}
oidc:
  existingSecret: linea-oidc       # key client-secret
fleet:
  url: https://fleet.example.com
  existingSecret: linea-fleet      # key token
ingress:
  enabled: true
  className: nginx
  host: debugger.example.com
  tls:
    secretName: debugger-example-com
pool:
  image: {repository: registry.example.com/team/linea-worker, tag: "1.0"}
  replicas: 2
builder:
  image: {repository: registry.example.com/team/linea-builder, tag: "1.0"}
networkPolicy:
  objectStorage:
    - to:
        - ipBlock: {cidr: 10.20.0.0/16}   # where s3.example.internal resolves
      ports: [{port: 443}]
```

### After the install

Nothing needs registering. The pool's image is the server's release, so the
pool registers the built-in runtime and runs it as its own build. Requests
that name no runtime run on `built-in` until the team chooses another default.

For another Collector release or components, build a runtime and, if you like,
make it the default (the notes the chart prints show the commands). The
server queues the build, a builder compiles it and registers the runtime, and
the pool runs it. No image is pushed and nobody deploys a worker.

```sh
linea runtime build --server https://debugger.example.com \
  --name mine --manifest mine.yaml
linea runtime default mine --server https://debugger.example.com
```

## What the chart creates

| Resource | Contents |
| --- | --- |
| Server ConfigMap | The server config, composed from `config`. The chart sets `store` (from `store`, `config.storageLimit`, `config.runLimit`), `worker_token_file` when the server accepts workers, `client_secret_file` with an OIDC secret, and `builds.token_file` when it accepts builders. `config.limits` and `config.retention` pass through as the server config's keys. No default runtime is set: use `linea runtime default`. A change restarts the server. |
| Server Deployment | `server.replicas` identical pods (default 2), `RollingUpdate` with `maxSurge` 1 and `maxUnavailable` 0: a new pod starts before an old one stops. Runs `linea-server --config` on port 7070 with `--mcp`. Queue, events, sessions and sign-ins are in PostgreSQL, so any instance serves any request. TCP probes. |
| PodDisruptionBudget | `maxUnavailable: 1`, when `server.replicas` is above 1: a node drain keeps one instance serving. |
| Ingress | For `ingress.host`, TLS from `ingress.tls.secretName`. The server then serves plain HTTP in the cluster (`--allow-http`). With `server.tls.secretName` the server ends TLS itself instead. |
| Pool Deployment (`pool.enabled`, default on) | `pool.replicas` pods running `linea-worker --connect --pool`, `pool.capacity` requests at once across all runtimes. Downloads each runtime binary through a presigned URL into `/cache` (emptyDir bounded by `pool.cache.size`) and runs it as a child process: idle ones beyond `pool.warm` stop, least recently used first, and an idle one after `pool.idle`. No port, no Service. |
| Builder Deployment (`builder.enabled`, default on) | `builder.replicas` pods running `linea-builder --connect`: `builder.capacity` builds at once, `builder.cpus` CPUs per build. Compiles with the pinned Go toolchain from a manifest and sources it downloads through presigned URLs, and uploads the binary the same way. Holds no database or bucket credentials. |
| Dedicated workers (`worker.enabled`, default off) | `worker.replicas` pods running `linea-worker --connect` from a runtime image you built, `worker.capacity` requests at once. |
| Bundled PostgreSQL (`postgresql.enabled`) | StatefulSet of one pod (PVC from `postgresql.persistence`), a Service on 5432, and a Secret with the generated password (key `password`, kept across upgrades). |
| Bundled SeaweedFS (`seaweedfs.enabled`) | StatefulSet of one pod (PVC from `seaweedfs.persistence`) serving the S3 gateway on 8333, a Service, a Secret with the generated keys (`access-key`, `secret-key`, kept across upgrades), and a bucket Job per release revision that creates `seaweedfs.bucket` and keeps an existing one. |
| Fleet and live capture (`fleet.deploy.enabled`) | A Deployment and Service each for fleet and the plugin, a database Job with the bundled PostgreSQL, their Secrets and a ConfigMap (see [Fleet and live capture](#fleet-and-live-capture)). |
| NetworkPolicies | For the pool, the builder, the dedicated worker, each bundled store, and, with the build metrics, the server's metrics port (see [Egress](#egress)). |

Secrets, each from a value or your own Secret:

| Secret | From | Your own | Keys |
| --- | --- | --- | --- |
| Store | `store.databaseURL`, `store.objects.accessKey`, `store.objects.secretKey` (or the bundled stores) | `store.existingSecret` | `database-url`, `access-key`, `secret-key` |
| Fleet token | `fleet.token`, or generated with `fleet.deploy.enabled` | `fleet.existingSecret` | `token` |
| OIDC client secret | `oidc.clientSecret` | `oidc.existingSecret` | `client-secret` |
| Worker token | `worker.token`, or generated | `worker.existingSecret` | `token` |
| Builder token | `builder.token`, or generated | `builder.existingSecret` | `token` |

The server reads the Secrets as files. Generated worker and builder tokens are
kept across upgrades. The chart keeps no volume for the server: everything it
stores is in PostgreSQL and the bucket.

Install fails, as the server would, without:

- `config.bindings`,
- `config.authentication` with `oidc` or `trusted_headers`,
- one of `pool.enabled`, `worker.enabled` or a worker token.

Server shutdown and builder sizing:

| Topic | Behavior |
| --- | --- |
| Server drain | On stop, an instance stops claiming work for `server.drainSeconds` (default 25) and lets its workers' runs finish, then hands the rest back to the queue without spending an attempt. The grace period is `drainSeconds` plus 20. |
| Server killed | Its runs lose their leases, and another instance requeues them after 30 s, once. |
| Builder resources | Limits 5 GiB and 4 CPUs (the build budget), requests 256 MiB and 100m (what an idle builder uses). A build bursts to the limits; on a node without that memory free it can be killed and is retried once. Raise `builder.resources.requests` to the limits to reserve the budget. |
| Builder stop | Finishes the builds it runs first: grace period `builder.terminationGracePeriodSeconds` (default 2100: the 30-minute build deadline plus a margin). |
| `builder.replicas: 0` | Builds off: the Deployment scales to zero, the server still takes builders, queued builds wait. |
| `builder.autoscaling` | An autoscaler sets the replicas (see [Scaling builders](#scaling-builders)). |

Every pod has a read-only root filesystem, no capabilities and no service
account token.

## Workers, the pool and builders

Workers, the pool and builders dial the server; the server never dials them.

| | Workers and the pool | Builders |
| --- | --- | --- |
| Credential | Registration token, as `LINEA_WORKER_TOKEN` | Build token, read as a file (`--token-file`) |
| Chart Secret (key `token`) | From `worker.token`, a generated one kept across upgrades, or `worker.existingSecret` | From `builder.token`, a generated one kept across upgrades, or `builder.existingSecret` |
| Server reads it as | `worker_token_file` (at least 32 characters) | `builds.token_file` |
| Server accepts them without the chart deploying them | `worker.enabled: false`: give `worker.token` or `worker.existingSecret` | `builder.enabled: false`: give `builder.token` or `builder.existingSecret` |
| Without a token | | The server answers builders 404 |

The two tokens must differ: the chart refuses equal `builder.token` and
`worker.token`, and one Secret named for both. See [Connecting
workers](team-server.md#connecting-workers), the [worker operator
guide](remote-workers.md) and its [pool workers](remote-workers.md#pool-workers).

| Setting | Meaning |
| --- | --- |
| `worker.serverURL` | The URL workers, the pool and builders dial. Defaults to the server's Service in the namespace: `https` with `server.tls.secretName`, otherwise plain `http`, which suits a protected cluster. Workers in another cluster dial `https://<ingress.host>`. |
| `pool.caSecret`, `builder.caSecret`, `worker.caSecret` | A Secret with a private CA's PEM certificate (key `ca.crt`, or the matching `caKey`). They use it to verify the server, in place of the system roots. |
| `pool.capacity`, `pool.resources`, `pool.warm` | `pool.capacity` requests share `pool.resources` (default 4 CPUs, 4 GiB), and so do the warm children. Scale with `pool.replicas`; a rolling update starts the new pod before it stops the old one. |
| `builder.modules` | Where builds download modules: the operator's choice, never a request's ([Module source](#module-source)). |

- The pool serves the built-in runtime of its image's release (keep
  `pool.image.tag` at the chart's `appVersion` to match the server) and any
  runtime the server built for its platform.
- A runtime registered from an image (`linea runtime register`) needs a
  dedicated worker. Set `worker.enabled: true`, `worker.name` and
  `worker.image`, or run it through your own Deployments (see
  [`examples/kubernetes/`](../../examples/kubernetes/)). Both need the same
  token. Once registered, make it the default with `linea runtime default NAME
  --server URL`.
- A runtime binary (about 85 MiB for the default manifest) counts toward
  `config.storageLimit`.

## Module source

By default builds download modules from `proxy.golang.org` and check them
against `sum.golang.org`. Where those are blocked, point the builder at an
internal proxy (Artifactory, Nexus, Athens) with `builder.modules`:

```yaml
builder:
  modules:
    proxy: https://artifactory.example.com/api/go/go-remote
    # Modules of your own, fetched from git and kept out of the public
    # checksum database.
    private: git.example.com/*
    # A Secret with key netrc:
    #   machine artifactory.example.com login linea password <token>
    netrcSecret: builder-netrc
    # A Secret with key ca.crt: the CA that signed the proxy's certificate.
    caSecret: corporate-ca
networkPolicy:
  builderEgress:
    - to:
        - ipBlock: {cidr: 10.30.0.0/16}
      ports: [{port: 443}]
```

| Key | Sets |
| --- | --- |
| `proxy` | `GOPROXY` |
| `private` | `GOPRIVATE` |
| `sumDB` | `GOSUMDB`. Go checks public modules against `sum.golang.org` through the proxy when it serves `/sumdb/` (Artifactory, Nexus and Athens can). Only where it does not, set `"off"`, which trusts the proxy for every module. |
| `netrcSecret` (`netrcKey`) | `NETRC`. The `machine` is the proxy's host, with `:port` when the URL names one. Also applies to the git hosts of private modules. |
| `caSecret` (`caKey`) | Adds the CA to the system roots (`SSL_CERT_DIR`). |

- `builder.extraEnv` can set the other module variables (`GONOPROXY`,
  `GONOSUMDB`, `GOINSECURE`, `GOAUTH`).
- Only these variables and the certificate settings reach a build, so the
  netrc file is the way to give credentials.
- Narrow `networkPolicy.builderEgress` to the proxy once nothing else is
  needed.

## Egress

With `networkPolicy.enabled` (default), the pool, the builder and the
dedicated worker each get a NetworkPolicy that accepts no connection. What
they may reach:

| Pods | Egress |
| --- | --- |
| Pool and dedicated workers | The server (port `http`), DNS, and the rules in `networkPolicy.objectStorage`. A run's Collector exports nowhere outside the pod, and the pool has no route to the module proxy. |
| Builders | The same, and `networkPolicy.builderEgress`: the module proxy and the git hosts a manifest names. The default is every address (`0.0.0.0/0`). |
| Server | No egress policy. With the build metrics on (`server.metrics`), an ingress policy admits the metrics port only from `server.metrics.allowFrom` and leaves port `http` open to every client. |
| Bundled stores | Ingress only: the server reaches PostgreSQL; chart pods reach SeaweedFS ([Stores](#stores)). |

`networkPolicy.objectStorage` is a list of egress rules (`to` and `ports`)
that reach `store.objects.endpoint`.

- The chart cannot know where your store runs, and presigned URLs for
  bundles, results and runtime binaries point there. So with the pool or a
  builder the install fails until you set it, unless `seaweedfs.enabled`.
- It also lets dedicated workers fetch bundles over the inline limit.

An in-cluster store, and an external one:

```yaml
networkPolicy:
  objectStorage:
    - to:
        - podSelector: {matchLabels: {app: seaweedfs}}
      ports: [{port: 8333}]
    - to:
        - ipBlock: {cidr: 10.20.0.0/16}
      ports: [{port: 443}]
```

- Narrow `networkPolicy.builderEgress` to the CIDRs your module proxy and git
  hosts resolve to: a NetworkPolicy matches addresses, not names.
- The default also matches pod and node addresses; add `except` ranges to its
  `ipBlock` to exclude the cluster's.
- With `worker.serverURL` pointing outside the release, allow that egress
  yourself.
- Your cluster's network plugin must enforce NetworkPolicy; k3s, and so k3d,
  does.

`scripts/debugger/k8s-acceptance.sh` checks the model on a disposable k3d
cluster. It builds a runtime named `default` on the builder and runs a sample
on the pool. It shows that a pod with the pool's labels reaches the server and
the object store but not the module proxy or an external address, and that a
pod with the builder's labels reaches the module proxy.

## Exporting runtime images

The builder can also push each runtime it builds as an image, `FROM scratch`
like `linea runtime build` makes, tagged with the build's name. Workers never
pull it: the pool serves the binary from the object store. The image is for CI
and dedicated workers.

```yaml
builder:
  export:
    repository: registry.example.com/team/linea-runtimes
    authSecret: regcred              # a kubernetes.io/dockerconfigjson Secret
    authKey: .dockerconfigjson
    plainHTTP: []                    # registry hosts reached without TLS
```

| Topic | Behavior |
| --- | --- |
| Where settings go | `repository` goes to the server config (`builds.export.repository`). Credentials stay with the builder (`--registry-auth`). |
| Credentials | The Docker config is read as Docker does: `credHelpers`, then `credsStore`, then `auths`. A helper must be on the builder's PATH as `docker-credential-<name>`. The `linea-builder` image ships none, so a helper such as `ecr-login` needs an image built from it. They need push, not delete. |
| Egress | `networkPolicy.builderEgress` must reach the registry. |
| Result | A build records the image by digest. A failed push is the build's export error; the build stays done. |
| Cleanup | Linea never deletes an exported image. Each export moves the runtime's tag and the previous image stays untagged; the registry's retention of untagged images (ECR lifecycle policy, Harbor retention and garbage collection, GHCR or GitLab cleanup) removes it. |

## Scaling builders

A builder is idle between builds. Where the cluster runs an autoscaler, the
chart can scale builders with the build queue instead of running
`builder.replicas` around the clock. It is off by default
(`builder.autoscaling.kind: none`), and the chart installs no autoscaler.

Both kinds target one builder per `builder.capacity` unfinished builds, up to
`builder.autoscaling.maxReplicas` (default 2). Running builds count, so the
target never drops below what builds in progress need. A builder that is
scaled in finishes its builds within the grace period and builds nothing new;
a build queued meanwhile goes to another builder.

Either kind turns on the server's build metrics (`server.metrics`, also
available alone with `enabled: true`): `linea-server --metrics-listen` on port
`server.metrics.port` (9090), behind a Service named after the server's with
`-metrics` appended, never behind the ingress. `GET /metrics` serves counts
only, in Prometheus text format, without credentials:

| Metric | Meaning |
| --- | --- |
| `linea_runtime_builds{state="queued"}`, `{state="running"}` | Builds of the installation, the same on every instance (read every 10 s) |
| `linea_runtime_builds_unfinished` | Queued plus running: the builder slots the builds need |
| `linea_builders_connected`, `linea_builder_slots` | The builders connected to the instance that answered, and their capacity |

With `networkPolicy.enabled`, only `server.metrics.allowFrom` reaches the
port. The default admits the namespace `keda`; add your Prometheus's
namespace to scrape it.

| | KEDA (`kind: keda`) | HPA (`kind: hpa`) |
| --- | --- | --- |
| Renders | A `ScaledObject` with the `metrics-api` trigger | An `autoscaling/v2` HorizontalPodAutoscaler |
| Reads | `linea_runtime_builds_unfinished` from the metrics Service directly; no Prometheus needed | The external metric `builder.autoscaling.hpa.metricName` (`linea_runtime_builds_unfinished`), which your external metrics adapter must serve |
| Scale to zero | `minReplicas` 0 (default): no builder while nothing is built, so the first build after an idle period waits for a pod. `cooldownPeriod` (600 s) keeps the last builder for a follow-up. | No: at least one builder runs |
| Install fails unless | The cluster serves `keda.sh/v1alpha1` (with `helm template`, pass `--api-versions keda.sh/v1alpha1`) | |
| Also | | `hpa.selector` when the adapter needs labels to pick the series |

```yaml
builder:
  autoscaling:
    kind: keda
    maxReplicas: 3
```

With prometheus-adapter for HPA, scrape the metrics Service and expose the
series as an external metric:

```yaml
externalRules:
  - seriesQuery: 'linea_runtime_builds_unfinished'
    metricsQuery: 'max(<<.Series>>)'
    resources: {overrides: {namespace: {resource: namespace}}}
```

Every instance reports the same queue, so `max` across instances is the
count; a sum would double it.

### A persistent cache

A new builder pod starts with an empty `/work` and downloads its modules and
compiles cold, several minutes longer. To keep the caches across pods, mount a
claim at `/work` with `builder.cache.persistentVolumeClaim`: `existingClaim`,
or `create: true` with `size`, `storageClassName` and `accessMode`.

- The chart keeps a claim it created when the release is uninstalled.
- `ReadWriteOnce` allows one builder (`replicas` or `maxReplicas` at most 1,
  else the install fails) and replaces the builder on an upgrade instead of
  rolling it.
- With more builders use `ReadWriteMany`, or keep the emptyDir and point
  `builder.modules.proxy` at a module proxy.

```yaml
builder:
  autoscaling: {kind: keda, maxReplicas: 1}
  cache:
    persistentVolumeClaim: {create: true, size: 20Gi}
```

## Fleet and live capture

With `fleet.deploy.enabled` the release also runs the [fleet
server](../fleet/README.md) and its [live-capture
plugin](../fleet/livecapture.md), and connects the server to fleet, so [live
capture](livecapture.md) works in the web client, CLI and MCP as soon as the
pods are ready. Nothing is copied by hand, and an upgrade keeps the
connection. It works like the Compose stack's
[overlay](deploy-compose.md#fleet-and-live-capture):

```yaml
postgresql: {enabled: true}
fleet:
  deploy:
    enabled: true
```

| Step | What the chart does |
| --- | --- |
| Databases | With `postgresql.enabled` and no `fleet.deploy.database.existingSecret`, a Job per release revision creates the databases `fleet` and `livecapture` in the bundled PostgreSQL, each with its own migration and runtime role, and resets their generated passwords. With your own PostgreSQL, create them yourself and name a Secret with the keys `fleet-migrate-url`, `fleet-runtime-url`, `capture-migrate-url` and `capture-runtime-url` ([roles](../fleet/deployment.md)). |
| Migrations | An init container in each pod migrates its schema before the pod starts. Each runs one pod, replaced rather than rolled, so a migration never overlaps a running instance. |
| Server token | Generated once, kept in a Secret across upgrades and never printed. Fleet's `bootstrap` init container installs it as the first admin, without expiry; on later starts it changes nothing. |
| Config management | Off: fleet only reports. `fleet.deploy.configManagement: true` starts it with `--config-management` for config versions, assignments and rollouts ([config management](../fleet/config-management.md)). |
| Raw capture | Off: captures select only points a Collector marks redacted. `fleet.deploy.livecapture.rawCapture: true` puts the server's fleet credential into the plugin's `raw_principals`; who may capture raw is still the debugger's `capture.raw` permission ([raw capture](livecapture.md#raw-capture-and-permissions)). |
| Plugin | Registered with fleet with generated host and service tokens. Its control and delivery endpoints serve TLS with `fleet.deploy.livecapture.tls.secretName`, or a self-signed certificate the chart generates once for the Service names and `tls.extraHosts`. |

Install fails with `fleet.url`, `fleet.token` or `fleet.existingSecret` set
beside `fleet.deploy.enabled`, or without a database for fleet.

Collectors connect to:

| Endpoint | Service and port | Protocol |
| --- | --- | --- |
| OpAMP | `<release>-linea-fleet:4320` (`fleet.deploy.service.agentPort`) | `ws://`, an agent credential per Collector |
| Capture control | `<release>-linea-livecapture:4340` (`controlPort`) | gRPC over TLS |
| Capture delivery | `<release>-linea-livecapture:4341` (`deliveryPort`) | gRPC over TLS |

Both Services are `ClusterIP` by default; for Collectors outside the cluster
set their `type` and add the address they use to `tls.extraHosts`. Collectors
trust the plugin's certificate from its Secret:

```sh
kubectl -n <namespace> get secret <release>-linea-livecapture-tls -o jsonpath='{.data.tls\.crt}' | base64 -d > livecapture-ca.crt
```

The release's NetworkPolicies admit the management API only from the server,
fleet's plugin host only from the plugin, the plugin's own port only from
fleet, and OpAMP, control and delivery from anywhere.

## Private certificate authorities

The server verifies the identity provider and fleet with the system roots.
For a private CA, name a Secret holding its PEM certificate:

```yaml
server:
  caSecret: company-ca   # key ca.crt; set server.caKey for another key
```

- The chart mounts the certificate and sets `SSL_CERT_DIR` to its directory,
  so the server trusts it beside the system roots. (`SSL_CERT_FILE` would
  replace the system roots, and a public identity provider would then fail
  verification.)
- The CLI trusts the CA with `linea login --server URL --ca ca.crt`. The
  sign-in keeps the CA, and every later `--server` command to that server uses
  it, also with `$LINEA_TOKEN`. The CA also verifies the identity provider
  during the sign-in.
- `server.hostAliases` maps names the cluster's DNS does not resolve, such as
  an identity provider reached through the same ingress.

## Signing in

People open `https://<ingress.host>` and sign in at the identity provider. The
CLI signs in with `linea login --server https://<ingress.host>`; see [the team
server](team-server.md#signing-in). Register `redirect_url` at the provider.

## Trying it locally on k3d

A disposable install with no registry and no company identity provider.
`debugger.localhost` and `dex.localhost` are placeholders. Browsers resolve
`*.localhost` to the loopback address; if your CLI does not, add both names to
your hosts file as `127.0.0.1`. Needs Docker, k3d, kubectl, helm, openssl and
free host port 443.

1. Create the cluster and build the three images of [Images](#images). The
   pool serves the runtime the builder makes, so no runtime image is needed.
   k3d's Traefik is the ingress, and k3s enforces NetworkPolicy.

   ```sh
   k3d cluster create debugger -p "443:443@loadbalancer" --wait
   docker build -f deploy/docker/linea-server.Dockerfile -t linea-server:local .
   docker build -f deploy/docker/linea-worker.Dockerfile -t linea-worker:local .
   docker build -f deploy/docker/linea-builder.Dockerfile -t linea-builder:local .
   k3d image import -c debugger linea-server:local linea-worker:local linea-builder:local
   kubectl create namespace linea
   ```

2. Make a private CA certificate that names both hosts. It is the ingress
   certificate and the server's trust root. In Git Bash on Windows, prefix
   `openssl` with `MSYS_NO_PATHCONV=1`, or `-subj` is turned into a file path.

   ```sh
   openssl req -x509 -newkey rsa:2048 -nodes -days 30 -subj "/CN=local debugger CA" \
     -addext "basicConstraints=critical,CA:TRUE" \
     -addext "subjectAltName=DNS:dex.localhost,DNS:debugger.localhost" \
     -keyout tls.key -out tls.crt
   kubectl -n linea create secret tls local-tls --cert=tls.crt --key=tls.key
   ```

3. Run Dex with a confidential web client, a public CLI client for the device
   flow and users in groups. Choose the client secret, and make each `hash` a
   bcrypt hash (`htpasswd -bnBC 10 "" '<password>' | tr -d ':\n'`; without
   `htpasswd`, run it as `docker run --rm httpd:2.4-alpine htpasswd ...`).
   Static passwords carry groups from Dex v2.45; with an older Dex, bind the
   users by principal ID instead. Save as `dex.yaml`:

   ```yaml
   apiVersion: v1
   kind: Secret
   metadata: {name: dex-config, namespace: linea}
   stringData:
     config.yaml: |
       issuer: https://dex.localhost
       storage: {type: memory}
       web: {http: 0.0.0.0:5556}
       oauth2: {skipApprovalScreen: true}
       enablePasswordDB: true
       staticClients:
         - {id: linea, name: debugger, secret: "<client-secret>",
            redirectURIs: ["https://debugger.localhost/auth/callback"]}
         - {id: linea-cli, name: cli, public: true,
            redirectURIs: ["/device/callback"]}
       staticPasswords:
         - {email: ada@example.com, username: ada, userID: ada, hash: "<bcrypt>", groups: [platform]}
         - {email: root@example.com, username: root, userID: root, hash: "<bcrypt>", groups: [platform-admins]}
   ---
   apiVersion: apps/v1
   kind: Deployment
   metadata: {name: dex, namespace: linea}
   spec:
     selector: {matchLabels: {app: dex}}
     template:
       metadata: {labels: {app: dex}}
       spec:
         containers:
           - name: dex
             image: ghcr.io/dexidp/dex:v2.45.1
             args: [dex, serve, /etc/dex/config.yaml]
             ports: [{containerPort: 5556}]
             volumeMounts: [{name: config, mountPath: /etc/dex, readOnly: true}]
         volumes:
           - {name: config, secret: {secretName: dex-config}}
   ---
   apiVersion: v1
   kind: Service
   metadata: {name: dex, namespace: linea}
   spec:
     selector: {app: dex}
     ports: [{port: 5556}]
   ---
   apiVersion: networking.k8s.io/v1
   kind: Ingress
   metadata: {name: dex, namespace: linea}
   spec:
     ingressClassName: traefik
     tls: [{hosts: [dex.localhost], secretName: local-tls}]
     rules:
       - host: dex.localhost
         http:
           paths:
             - {path: /, pathType: Prefix, backend: {service: {name: dex, port: {number: 5556}}}}
   ```

   ```sh
   kubectl apply -f dex.yaml
   kubectl -n linea create secret generic local-oidc --from-literal=client-secret='<client-secret>'
   kubectl -n linea rollout status deploy/dex
   curl -4 --cacert tls.crt https://dex.localhost/.well-known/openid-configuration
   ```

   `-4` matters on Windows: `*.localhost` resolves to `::1` first, where the
   k3d port does not answer TLS.

4. The server pod must reach Dex by the name the browser uses, which is not
   in cluster DNS. Map the name to Traefik's cluster IP:

   ```sh
   kubectl -n kube-system get svc traefik -o jsonpath='{.spec.clusterIP}'
   ```

   Save as `values.yaml`, with that address as `<traefik-ip>`. The chart runs
   PostgreSQL and SeaweedFS ([Stores](#stores)):

   ```yaml
   image: {repository: linea-server, tag: local}
   postgresql: {enabled: true}
   seaweedfs: {enabled: true}
   config:
     authentication:
       oidc:
         issuer: https://dex.localhost
         client_id: linea
         redirect_url: https://debugger.localhost/auth/callback
         cli_client_id: linea-cli
         scopes: [openid, profile, email, groups]
     bindings:
       - {group: platform, role: engineer, scope: "*"}
       - {group: platform-admins, role: admin, scope: "*"}
   oidc: {existingSecret: local-oidc}
   ingress:
     enabled: true
     className: traefik
     host: debugger.localhost
     tls: {secretName: local-tls}
   server:
     hostAliases:
       - {ip: "<traefik-ip>", hostnames: [dex.localhost]}
     # The self-signed certificate is its own CA.
     caSecret: local-tls
     caKey: tls.crt
   pool:
     image: {repository: linea-worker, tag: local}
   builder:
     image: {repository: linea-builder, tag: local}
   ```

5. Install and wait for the stores, the server, the pool and the builder:

   ```sh
   helm install linea deploy/charts/linea -n linea -f values.yaml
   kubectl -n linea rollout status sts/linea-postgresql sts/linea-seaweedfs \
     deploy/linea deploy/linea-pool deploy/linea-builder
   ```

6. Sign in. Open `https://debugger.localhost` and accept the certificate
   warning, or trust `tls.crt`; sign in as `ada`. The CLI trusts `tls.crt`
   through `--ca` and keeps it for later commands:

   ```sh
   linea login --server https://debugger.localhost --ca tls.crt
   linea runs --server https://debugger.localhost
   ```

   Run a sample. Nothing was registered: the pool runs the built-in runtime as
   its own build:

   ```sh
   linea run --server https://debugger.localhost \
     --config samples/logs-cleanup/config.yaml --scenario samples/logs-cleanup/scenario.yaml
   ```

   To try a server build, build the runtime `default`. The builder compiles
   the Collector with cold caches (several minutes) and registers the runtime;
   the pool then downloads the binary and starts it:

   ```sh
   linea runtime build --server https://debugger.localhost \
     --name default --manifest build/linea-runtime.yaml
   linea run --server https://debugger.localhost --runtime default \
     --config samples/logs-cleanup/config.yaml --scenario samples/logs-cleanup/scenario.yaml
   ```

   To give other people or groups a role, add entries under `config.bindings`
   (`group:` for an identity-provider group, or `principal:`) and run `helm
   upgrade` with the same values file; the server restarts on a server config
   change.

7. Tear down. This removes the PostgreSQL and SeaweedFS volumes too:

   ```sh
   k3d cluster delete debugger
   ```

## Limits of this chart

The bundled PostgreSQL and SeaweedFS are single pods without replication or
backups. For production, use your own HA PostgreSQL and a durable bucket.
