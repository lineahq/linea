# The Helm chart: implementation notes

How the chart in `deploy/charts/linea` is built and why. The operator guide is
[`docs/debugger/deploy-kubernetes.md`](../debugger/deploy-kubernetes.md);
every key is in [`values.yaml`](../../deploy/charts/linea/values.yaml).

## Pods

| Pod | Runs as | Writable volumes | Why |
| --- | --- | --- | --- |
| Server | 65532 | `/tmp` (scratch) | The server stores nothing locally: everything is in PostgreSQL and the bucket. |
| Pool | 65534 | `/cache` (emptyDir, executable), `/tmp` | The pool starts runtime binaries from `/cache`, so it must allow execution. `pool.cache.volumeSizeLimit` (default 6Gi) is a little above the `pool.cache.size` budget (5GiB). |
| Builder | 65532 | `/work` (emptyDir, executable) | `/work` holds the module cache, the build cache and build directories. It allows execution because a build starts the program it compiles. |
| Bundled PostgreSQL | 999 (the image's `postgres` user) | `/var/run/postgresql`, `/tmp`, the data volume | Data lives in `pgdata`, a subdirectory, because the volume's root may hold `lost+found`. |
| Bundled SeaweedFS | 65532 | `/tmp`, the data volume | |

Every pod has a read-only root filesystem, no capabilities and no service
account token.

## Server

- Probes check the TCP port, not an HTTP route: every route except the web
  client's assets needs a credential.
- `RollingUpdate` with `maxSurge` 1 and `maxUnavailable` 0 starts a new pod
  before an old one stops. Any instance serves any request because the run
  queue, events, sessions and sign-ins are in PostgreSQL.
- A stopping pod drains for `--drain-timeout`, set to `server.drainSeconds`
  (default 25): it stops claiming work and lets its workers' runs finish, then
  hands the rest back to the queue without spending an attempt.
  `terminationGracePeriodSeconds` is `drainSeconds` plus 20.
- A pod that is killed loses its runs' leases. Another instance requeues them
  after 30 s, once.
- The ConfigMap's content is a checksum input of the pod template, so a change
  to the server config restarts the server.
- The server reads Secrets as files (`worker_token_file`,
  `client_secret_file`, `builds.token_file`, the store Secret).

## Pool

- The pool downloads each runtime binary through a presigned URL into `/cache`
  and runs it as a child process. Idle children beyond `pool.warm` stop, least
  recently used first, and an idle one after `pool.idle`.
- Children inherit the pod's environment, so `pool.resources` is the bound,
  shared by every runtime the pool keeps running.
- The pool listens on no port, so it has no Service.

## Builder

- It holds no database or bucket credentials: it downloads the manifest and
  sources, and uploads the binary, through presigned URLs.
- Resources: limits 5 GiB and 4 CPUs are the build budget; requests (256 MiB,
  100m) reserve only what an idle builder uses, so a build bursts to the
  limits. On a node without 5 GiB free, a build can be killed for memory and
  is retried once.
- `terminationGracePeriodSeconds` is 2100 s: the build deadline is 30 minutes,
  and a build still running at the end is retried once.
- Only `GOPROXY`, `GOPRIVATE`, `GONOPROXY`, `GONOSUMDB`, `GOSUMDB`,
  `GOINSECURE`, `GOAUTH`, `NETRC`, `SSL_CERT_FILE` and `SSL_CERT_DIR` reach a
  build. Nothing else of the builder's environment does.
- Go checks public modules against `sum.golang.org` through the proxy when the
  proxy serves `/sumdb/`. `sumDB: "off"` therefore trusts the proxy for every
  module.

## Certificates

The chart mounts a CA Secret and sets `SSL_CERT_DIR` to its directory, which
adds the CA to the system roots. `SSL_CERT_FILE` would replace the system
roots, and a public identity provider would then fail verification.

## Network policies

- Presigned URLs for bundles, results and runtime binaries point at
  `store.objects.endpoint`, and the chart cannot know where that runs, so
  `networkPolicy.objectStorage` is required with the pool or a builder
  (`linea.checkObjectStorage`). With `seaweedfs.enabled` the chart adds its own
  egress rule to the bundled gateway (port 8333).
- A NetworkPolicy matches addresses, not names. The default
  `builderEgress` (`0.0.0.0/0`) also matches pod and node addresses.
- Bundled PostgreSQL admits only the server on its port, and with
  `fleet.deploy.enabled` also fleet, the plugin and the database Job.
  Bundled SeaweedFS admits the server, pool, worker and builder pods on the
  S3 port, the bucket Job on the master and filer ports, and itself (the pod
  reaches its own master and filer through the Service).

## Builder autoscaling

- The target is one builder per `builder.capacity` unfinished builds; running
  builds count, so it never drops below what builds in progress need.
- Each instance reports the same queue, so the HPA metric rule uses `max`
  across instances; a sum would double it.
- The metrics Service is the server's name plus `-metrics`. It is never behind
  the ingress.

## Bundled stores

`templates/stores.yaml` renders both stores; helpers are in
`_helpers.tpl`.

- Images are pinned by tag and digest (`postgresql.image.digest`,
  `seaweedfs.image.digest`).
- **Kept credentials.** `linea.keptSecret` takes the value from `values`, else
  from the existing Secret (Helm `lookup`), else a new 32-character random
  string. A render caches each value, so every template sees the same one.
  `lookup` returns nothing under `helm template`, so credentials are
  regenerated on each render there. The pod templates carry a checksum
  annotation of the credentials, so a changed credential restarts the pod.
- **Store wiring.** With `postgresql.enabled` the database URL is
  `postgres://linea:<password>@<release>-postgresql:5432/linea?sslmode=disable`.
  With `seaweedfs.enabled` the object store is
  `http://<release>-seaweedfs:8333` with path-style requests, an empty prefix
  and region, and the bucket `seaweedfs.bucket`. The chart writes the store
  Secret itself, so `linea.checkBundledStores` refuses `store.existingSecret`
  with either store, `store.databaseURL` with PostgreSQL, and
  `store.objects.*` with SeaweedFS.
- **SeaweedFS.** One `weed server` pod with a filer and the S3 gateway
  (`-s3.config` from the Secret's `s3.json`, one identity with Admin, Read,
  Write, List and Tagging), `-volume.max=0` (as many volumes as the disk
  holds) and `-master.volumeSizeLimitMB` from `seaweedfs.volumeSizeLimitMB`
  (256). The Service exposes the S3 port (8333) and the master (9333 and gRPC
  19333).
- **Bucket Job.** One Job per release revision (`<release>-seaweedfs-bucket-<revision>`),
  with `backoffLimit` 6, removed 600 s after it finishes. It loops up to 120
  times, 2 s apart: it lists buckets with `weed shell` against the master,
  exits when the bucket exists, and otherwise creates it. It fails if the
  bucket cannot be created. The pod advertises its own IP (`-ip`) so the
  shell can reach the filer.

## Fleet

`templates/fleet.yaml` renders fleet and the live-capture plugin for
`fleet.deploy.enabled`; `linea.fleetDeployed` checks the values.

- **Tokens.** `linea.keptSecret` takes a format: `fleet` gives an
  `of1_<32 hex>_<64 hex>` admin token, `hex` 64 lowercase hex characters for
  the plugin's host, service and metrics tokens (`pluginsdk.ValidToken`).
  Fleet's `bootstrap` installs the admin token from
  `LINEA_FLEET_BOOTSTRAP_TOKEN_FILE`, so the chart never reads a token back
  from fleet.
- **One pod each, `Recreate`.** The plugin holds an advisory lock on its
  database while it runs and its migration takes the same lock; fleet's
  migration expects no running instance either. Migrations run as init
  containers.
- **Readiness cycle.** The plugin is ready only once fleet's host session
  answers, and fleet answers only after it has fetched the plugin's manifest.
  A Service routes only to ready pods, so fleet reaches the plugin through
  `<livecapture>-plugin`, which sets `publishNotReadyAddresses`; Collectors use
  the ready-gated `<livecapture>` Service.
- **Database Job.** Runs on the bundled PostgreSQL image as user 999 with the
  same psql script as the Compose overlay, and resets the four role passwords
  from `<fleet>-db` on every revision.
- **Certificate.** `genSelfSignedCert` for 825 days when no
  `tls.secretName` is given, kept through `lookup` like the other generated
  values.
