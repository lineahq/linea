# Operating a team server

A runbook for the three jobs that need care once a [team
server](team-server.md) holds real work: backing it up and restoring it,
rotating its secrets, and upgrading it or rolling the upgrade back. It covers
both deployments, [Docker Compose](deploy-compose.md) and the
[Kubernetes chart](deploy-kubernetes.md). The fleet server and the
live-capture plugin have their own pages ([fleet
deployment](../fleet/deployment.md), [live-capture
plugin](../fleet/livecapture.md#rotation-backup-restore-and-upgrade)); this
page says how they fit in when they run beside the debugger.

Conventions:

- Compose commands run in the directory with `compose.yaml`. With the fleet
  overlay, add `-f compose.yaml -f compose.fleet.yaml` to every `docker
  compose` command.
- Compose names below assume the project name `linea`, which `compose.yaml`
  sets: the volumes are `linea_postgres-data` and `linea_seaweedfs-data`, and
  the network that reaches the object store is `linea_storage`. Check with
  `docker volume ls` and `docker network ls` if you changed it.
- Kubernetes commands use `<ns>` for the namespace and `<fullname>` for the
  chart's name for the release: `<release>-linea`, or just the release name
  when it already contains `linea`.

## What holds state

| What | Holds | Where it lives |
| --- | --- | --- |
| PostgreSQL, database `linea` | Saved sources and their revisions, the run journal, evaluations, suites, capture metadata, the audit log, API token and web session records (hashes only), the runtime catalog and builds, role bindings, projects, and the rows instances coordinate through (queue, change feed, workers) | Compose: volume `postgres-data`. Chart: your PostgreSQL, or the bundled one's volume claim |
| Object store, bucket `store.objects.bucket` | Every byte payload, named by its SHA-256 under `<prefix>/sha256/`: source revisions, input bundles, evidence, capture segments, built runtime binaries | Compose: volume `seaweedfs-data`. Chart: your bucket, or the bundled SeaweedFS's volume claim |
| Fleet and live-capture databases (only when deployed) | Fleet: inventory, groups, events, credential hashes. Live capture: capture metadata, no telemetry | Compose overlay: the databases `fleet` and `livecapture` in the same PostgreSQL. Chart with `fleet.deploy`: the same, in the bundled PostgreSQL |
| Configuration and secrets | The files and Secrets that tell the stack how to start | Compose: `secrets/`, `.env`, and with the overlay `livecapture.json` and `fleet-plugins.json`. Chart: your values file and every Secret, including the ones the chart generated |

The layout is in [artifact storage](../internals/artifact-storage.md#storage-layout).
Rows hold a payload's digest and size, never the payload, so a row without its
object is a broken record.

Back up the secrets too. PostgreSQL takes `postgres_password` once, when its
volume is created, and the chart's generated Secrets hold the bundled stores'
passwords and keys. Without them a restored volume has no way in. The dump,
the objects and the secrets are all sensitive: the database holds run
metadata, the audit log and token and session hashes, and the object store
holds the saved configurations, the recorded telemetry and the evidence. Keep
them as private as the server.

### What needs no backup

| What | Why |
| --- | --- |
| `linea` server containers and pods | They keep nothing on disk. The chart keeps no volume for the server |
| `pool` and its cache (`pool-cache`, an emptyDir in the chart) | Downloaded runtime binaries. Removing them only costs the downloads |
| `builder` and its work volume (`builder-work`, an emptyDir or your claim in the chart) | Module and build caches. Removing them only costs time |
| Dedicated workers | They dial the server and hold nothing |
| Objects under `<prefix>/staging/` | Transient worker transfers; no row names them and the server deletes the ones older than one hour |
| Exported runtime images | They live in your registry, which Linea never cleans. Protect them as you do any image |

## Backup

### The order

Objects are addressed by content. A writer uploads an object before it commits
the row that names it, and the server deletes an object only when no row
names it. The server's housekeeping leader runs that sweep every 5 minutes. It
removes an object that no row references and whose last-modified time is more
than 24 hours old (`sweepMargin` in `internal/debugger/store/pg/housekeep.go`).
A deleted run, a run evicted by retention and a failed write all leave
objects for the sweep.

That gives the rule:

> Take the PostgreSQL backup first. Take the object backup after it, and make
> sure it still holds every object the dump references.

Why this order:

- A dump can only name objects that existed when it was taken. A copy of the
  bucket taken later holds them, unless the sweep removed one in between.
- A copy taken before the dump can miss objects written after the copy and
  before the dump. The dump names them, and nothing brings them back.
- An object younger than 24 hours is never swept. An older object whose last
  row goes away after the dump can be swept within 5 minutes, and a copy taken
  after that lacks it. The restored database then names an object that is not
  there, and reading that run fails as corrupt (a missing object is
  `ErrCorrupt`).
- An object in the copy that the dump does not name is harmless. The next
  sweep deletes it once it is older than 24 hours.

Two ways to keep the rule, from safest down:

1. **Stop the writers.** Stop the server and the workers, then take both
   backups. Nothing is deleted or swept meanwhile, so the order cannot be
   violated. Compose below does this.
2. **Copy the bucket without deleting.** For a backup while the server runs,
   copy the bucket right after the dump with a tool that never deletes from
   the copy (`aws s3 sync` without `--delete`, a versioned bucket, or
   replication that keeps old versions). Do not mirror the bucket with
   deletion, which copies the sweep's deletions. Keep the gap between dump and
   copy short.

### Docker Compose backup

This stops the server and its workers, so the API is down for the length of
the copy. Queued runs wait and run after the restart.

```sh
backup=backup/$(date -u +%Y%m%dT%H%M%SZ)
mkdir -p "$backup" && chmod 700 "$backup"

# 1. Stop everything that writes. The builder finishes a running build first
#    (up to 35 minutes); `docker compose kill builder` stops it at once.
docker compose stop linea pool builder

# 2. The database, first.
docker compose exec -T postgres pg_dump -U postgres -d linea -Fc -f /tmp/linea.dump
docker compose cp postgres:/tmp/linea.dump "$backup/linea.dump"
docker compose exec -T postgres pg_restore --list /tmp/linea.dump > /dev/null

# 3. The object store, after it. SeaweedFS keeps its data in /data; stop it so
#    the copy is not taken while it writes.
docker compose stop seaweedfs
docker run --rm -v linea_seaweedfs-data:/data:ro -v "$PWD/$backup":/backup alpine \
  tar czf /backup/seaweedfs-data.tgz -C /data .

# 4. The files that start the stack.
tar czf "$backup/config.tgz" compose.yaml .env secrets

docker compose up -d
```

- `pg_dump` runs inside the `postgres` container as the superuser `postgres`,
  which the container accepts on its local socket only ([database
  roles](deploy-compose.md#database-roles)). The dump is in PostgreSQL's
  custom format.
- `.env` is optional; leave it out of the `tar` command if you have none.
  Keep the exact `compose.yaml` and image tag (`LINEA_VERSION`) the backup
  came from: a restore needs a server of the same or a newer release.
- In Git Bash on Windows, prefix the `docker run` with `MSYS_NO_PATHCONV=1`.
- With the fleet overlay, stop `fleet` and `livecapture-plugin` in step 1 too,
  and dump their databases in step 2, so all dumps describe one moment:

  ```sh
  for db in fleet livecapture; do
    docker compose exec -T postgres pg_dump -U postgres -d $db -Fc -f /tmp/$db.dump
    docker compose cp postgres:/tmp/$db.dump "$backup/$db.dump"
  done
  ```

  Roles are not in a single-database dump. Restore recreates them from the
  secret files, which is why `secrets/` is in the backup ([fleet
  backup](../fleet/deployment.md#backup)).

**A backup while the server runs.** Take the dump (step 2) without stopping
anything, then copy the bucket with a tool that does not delete. The object
store is only on the internal `storage` network, so run the tool there:

```sh
docker run --rm --network linea_storage -v "$PWD/$backup/objects":/backup \
  -e AWS_ACCESS_KEY_ID="$(cat secrets/s3_access_key)" \
  -e AWS_SECRET_ACCESS_KEY="$(cat secrets/s3_secret_key)" \
  amazon/aws-cli --endpoint-url http://seaweedfs:8333 s3 sync s3://linea /backup
```

Repeat it into the same directory: `sync` only adds and updates, so objects the
sweep deleted since the last run stay in the copy.

### Kubernetes backup

The bundled PostgreSQL and SeaweedFS (`postgresql.enabled`,
`seaweedfs.enabled`) are one pod each with a volume claim, with no replication
and no backups. They are for evaluation and small teams. For production use
your own PostgreSQL and bucket and give them their own backups:

- PostgreSQL: your service's snapshots or point-in-time recovery, or
  `pg_dump -Fc` on a schedule.
- Bucket: versioning or replication that keeps deleted objects, or a scheduled
  copy that never deletes.
- The order rule above holds for both: whatever restores the database to time
  T needs a bucket state from T or later that still has the objects deleted
  after T.
- Add your values file and the Secrets you created (`existingSecret`).

If you do run the bundled stores, a stop-the-writers backup looks like this.
Note the replica counts first, to scale back to them:

```sh
kubectl -n <ns> get deploy <fullname> <fullname>-pool <fullname>-builder   # replicas
kubectl -n <ns> scale deploy/<fullname> deploy/<fullname>-pool deploy/<fullname>-builder --replicas=0

# The database, first. The pod's /tmp is writable.
kubectl -n <ns> exec sts/<fullname>-postgresql -- pg_dump -U postgres -d linea -Fc -f /tmp/linea.dump
kubectl -n <ns> cp <fullname>-postgresql-0:/tmp/linea.dump ./linea.dump

# The bucket, after it (the bucket is seaweedfs.bucket, `linea` by default).
kubectl -n <ns> port-forward svc/<fullname>-seaweedfs 8333:8333 &
AWS_ACCESS_KEY_ID=$(kubectl -n <ns> get secret <fullname>-seaweedfs -o jsonpath='{.data.access-key}' | base64 -d) \
AWS_SECRET_ACCESS_KEY=$(kubectl -n <ns> get secret <fullname>-seaweedfs -o jsonpath='{.data.secret-key}' | base64 -d) \
  aws --endpoint-url http://127.0.0.1:8333 s3 sync s3://linea ./objects
kill %1

kubectl -n <ns> scale deploy/<fullname> --replicas=<server.replicas>   # and the pool and builder
```

With `fleet.deploy.enabled`, also dump the databases `fleet` and `livecapture`
the same way, and scale `<fullname>-fleet` and `<fullname>-livecapture` to 0
first. Save the generated Secrets (`kubectl get secret ... -o yaml`) in your
secret manager: the bundled stores' passwords and keys, the admin password,
the tokens and the fleet and plugin credentials are in them.

## Restore

Restore in the reverse order of the backup, with every server stopped:

1. Stop the servers and the workers. Nothing may write while you restore.
2. Restore PostgreSQL.
3. Restore the object store.
4. Start the server, then the workers and the builder.

The server applies its embedded migrations when it starts, forward only, all
pending ones in one transaction under an advisory lock
(`internal/debugger/store/pg/migrate.go`):

| The database is | The server |
| --- | --- |
| At the schema of the server's release | Starts |
| Older (a dump from an earlier release) | Migrates it forward, then starts |
| Newer (a dump from a later release) | Refuses to start: "database has migration ..., newer than this binary" (`ErrUnsupportedSchema`) |
| Changed after it was applied | Refuses to start: "applied migration ... was changed" (`ErrCorrupt`) |

So restore into a server of the dump's release or a later one. To see a
dump's schema version after restoring it:
`SELECT max(version) FROM schema_migrations`.

### Docker Compose restore

Restore into an empty stack: either a new Compose project (`docker compose -p
linea-restore ...`, with `LINEA_PORT` set to a free port in its `.env`), or the
same project after you removed its two volumes. Removing them is permanent:
check the backup first, in a new project.

```sh
backup=backup/20260101T120000Z       # the directory you took

# The backup's files, in the directory you run the stack from: the same
# secrets, so PostgreSQL's role gets the password the server will use.
tar xzf "$backup/config.tgz"

docker compose down
docker volume rm linea_postgres-data linea_seaweedfs-data     # only to replace the same project

# The database. The first start creates the role and the empty database.
docker compose up -d --wait postgres
docker compose cp "$backup/linea.dump" postgres:/tmp/linea.dump
docker compose exec -T postgres pg_restore -U postgres --exit-on-error --dbname linea /tmp/linea.dump

# The objects. `create` makes the volume and no process writes to it.
docker compose create seaweedfs
docker run --rm -v linea_seaweedfs-data:/data -v "$PWD/$backup":/backup:ro alpine \
  tar xzf /backup/seaweedfs-data.tgz -C /data

docker compose up -d
```

- In a new project, leave out the `docker volume rm`. The volumes are then
  `linea-restore_postgres-data` and `linea-restore_seaweedfs-data`.
- Use the `compose.yaml` and `LINEA_VERSION` of the backup, or a later
  release's (then follow [upgrade](#docker-compose-upgrade) from step 2).
- If you backed up the bucket with `s3 sync`, restore it the other way instead
  of the volume copy. Keep `linea` stopped and start the stores with
  `docker compose up -d --wait postgres seaweedfs`. The bucket does not exist
  yet (the server creates it when it starts), so make it, then sync back:

  ```sh
  aws="docker run --rm --network linea_storage -v $PWD/$backup/objects:/backup
    -e AWS_ACCESS_KEY_ID=$(cat secrets/s3_access_key)
    -e AWS_SECRET_ACCESS_KEY=$(cat secrets/s3_secret_key)
    amazon/aws-cli --endpoint-url http://seaweedfs:8333"
  $aws s3 mb s3://linea
  $aws s3 sync /backup s3://linea
  ```

- With the fleet overlay, run `docker compose run --rm -T fleet-db` after the
  first `postgres` start. It creates the `fleet` and `livecapture` databases
  and their roles. Restore the two dumps into them with the same
  `pg_restore`, then run `scripts/debugger/compose-fleet.sh`, which migrates
  both, installs the fleet admin and operator tokens only if they are missing,
  and starts the stack. Restore before migrating ([fleet
  restore](../fleet/deployment.md#restore-into-a-fresh-deployment)).

### Kubernetes restore

With your own stores, restore the database with your service's tools and the
bucket from its copy, then point `store.databaseURL` and `store.objects.*` at
the restored ones if they changed. With the bundled stores:

1. Scale the server, pool and builder to 0 (`kubectl scale`, as in the backup).
2. Recreate the database empty, and restore the dump into it. The pod has
   `psql` and `pg_restore`, which work as the superuser on its local socket:

   ```sh
   kubectl -n <ns> cp ./linea.dump <fullname>-postgresql-0:/tmp/linea.dump
   kubectl -n <ns> exec sts/<fullname>-postgresql -- psql -U postgres -d postgres -c 'DROP DATABASE linea' \
     -c 'CREATE DATABASE linea OWNER linea' -c 'REVOKE ALL ON DATABASE linea FROM PUBLIC'
   kubectl -n <ns> exec sts/<fullname>-postgresql -- pg_restore -U postgres --exit-on-error --dbname linea /tmp/linea.dump
   ```

3. Restore the bucket with `aws s3 sync ./objects s3://linea` through the
   port-forward, as in the backup with the arguments swapped. If you replaced
   the volume claim, make the bucket first (`aws s3 mb s3://linea`).
4. Scale back to the replica counts you noted, and wait:

   ```sh
   kubectl -n <ns> rollout status deploy/<fullname> deploy/<fullname>-pool deploy/<fullname>-builder
   ```

### After a restore

- **Check the data.** The server's `/readyz` answers once it opened its
  database and bucket. Then open the oldest and the newest run: the store
  checks the size and SHA-256 of every object it reads, and a missing one
  fails the read as corrupt. If one run reads and a neighbor does not, the
  object copy was older than the dump or the sweep removed it: restore an
  object backup that is no older than the dump.
- **Queued and running runs.** An unfinished run with its input bundle is
  scheduled again at start; one without is marked interrupted
  ([team server](team-server.md#storage)).
- **Tokens and sessions go back to the backup's moment.** An API token you
  revoked since then works again, and sessions started since are gone, so
  people sign in again. Revoke again what you revoked. After a restore that
  follows an admin password rotation, run `linea-server revoke-admin` again
  ([below](#the-admin-account)).
- **Use the secrets of the backup, or rotate after.** The dump holds the
  server's data, not the passwords the services use to reach it. With the
  bundled PostgreSQL, the `linea` role's password is set from the
  `postgres_password` secret (or the chart's Secret) when the volume is first
  initialized, so restore with the backed-up secrets, as above. If you start
  with new ones, change the role's password to match first ([the database
  password](#the-database-password)).

## Rotate secrets

Every secret is read when its service starts. Changing a file or a Secret
changes nothing until the service restarts. The server has no signing,
session or cookie key: a web session or API token is a random value kept as a
hash in PostgreSQL, so there is nothing to rotate for them and a rotation
below never logs people out (except as noted for the admin account).

Make values with `openssl rand -hex 32` (64 characters). A token needs at
least 32 characters and the admin password at least 16.

### Where each secret lives

| Secret | Compose file in `secrets/` | Chart: value and Secret key | Server config key and variable |
| --- | --- | --- | --- |
| Admin password | `admin_password` | `admin.password` or `admin.existingSecret`; Secret `<fullname>-admin`, key `password` | `authentication.admin.password_file`, `LINEA_AUTHENTICATION_ADMIN_PASSWORD_FILE` |
| Worker token | `worker_token` | `worker.token` or `worker.existingSecret`; Secret `<fullname>-worker-token`, key `token` | `worker_token_file`, `LINEA_WORKER_TOKEN_FILE` |
| Builder token | `builder_token` | `builder.token` or `builder.existingSecret`; Secret `<fullname>-builder-token`, key `token` | `builds.token_file`, `LINEA_BUILDS_TOKEN_FILE` |
| OIDC client secret | `oidc_client_secret` (read when `LINEA_AUTHENTICATION_OIDC_ISSUER` is set) | `oidc.clientSecret` or `oidc.existingSecret`; Secret `<fullname>-oidc`, key `client-secret` | `authentication.oidc.client_secret_file`, `LINEA_AUTHENTICATION_OIDC_CLIENT_SECRET_FILE` |
| Object-store keys | `s3_access_key`, `s3_secret_key` (SeaweedFS reads the same two files) | Your store: `store.objects.accessKey`, `secretKey` or `store.existingSecret`; Secret `<fullname>-store`, keys `access-key`, `secret-key`. Bundled: `seaweedfs.accessKey`, `seaweedfs.secretKey`; Secret `<fullname>-seaweedfs` | `store.objects.access_key_file`, `secret_key_file`; `LINEA_STORE_OBJECTS_ACCESS_KEY_FILE`, `LINEA_STORE_OBJECTS_SECRET_KEY_FILE` |
| Database password | `postgres_password` (completes the URL without a password) | Your database: `store.databaseURL` (the password is in the URL) or `store.existingSecret`, key `database-url`. Bundled: `postgresql.password`; Secret `<fullname>-postgresql`, key `password` | `store.database_url_file`, or `store.database_url` with `store.database_password_file`; `LINEA_STORE_DATABASE_URL_FILE`, `LINEA_STORE_DATABASE_URL`, `LINEA_STORE_DATABASE_PASSWORD_FILE` |
| Session and cookie keys | None exist | None exist | None exist |
| API tokens (users and CI) | None; they live in PostgreSQL | None | Not a server secret; see [API tokens](team-server.md#api-tokens) |
| Fleet operator token (the server's) | `fleet_operator_token` | Chart-deployed fleet: Secret `<fullname>-fleet`, key `operator-token`, no value. A separate fleet: `fleet.token` or `fleet.existingSecret`, key `token` | The flag `--fleet-token-file`; no variable |
| Fleet admin token (yours; the server never gets it) | `fleet_admin_token` | Secret `<fullname>-fleet`, key `token`, no value | None |
| Live-capture tokens | `capture_host_token`, `capture_service_token`, `capture_metrics_token` | Secret `<fullname>-livecapture`, keys `host-token`, `service-token`, `metrics-token`, no value | None (plugin and fleet settings) |
| Fleet and live-capture database URLs | `fleet_database_url`, `fleet_migrate_database_url`, `capture_database_url`, `capture_migrate_database_url` | Secret `<fullname>-fleet-db` (or your `fleet.deploy.database.existingSecret`), keys `*-password` and `*-url`, no value | None |
| Live-capture TLS certificate and key | `capture_tls.crt`, `capture_tls.key` | `fleet.deploy.livecapture.tls.secretName`, or the generated Secret `<fullname>-livecapture-tls` | None |

### What restarts, and what is in flight

| Secret | Compose: restart | Chart: restarts by itself on `helm upgrade` | Chart: restart by hand | In flight |
| --- | --- | --- | --- | --- |
| Admin password | `docker compose restart linea` | Server, when the chart renders the Secret (`admin.password` or generated) | Server, with `admin.existingSecret` | The old password's web sessions and API tokens stay valid until `revoke-admin` |
| Worker token | `docker compose restart linea pool` | Server, pool and dedicated workers, when the chart renders the Secret | The same three, with `worker.existingSecret` | A worker with the old token is refused (401) and stops instead of retrying. Workers outside the stack need the new token from you |
| Builder token | `docker compose restart linea builder` | Server and builder, when the chart renders the Secret | The same two, with `builder.existingSecret` | As for workers. A restarted builder finishes its builds first |
| OIDC client secret | `docker compose restart linea` | Server, when the chart renders the Secret (`oidc.clientSecret`) | Server, with `oidc.existingSecret` | Sessions and API tokens are unaffected. A sign-in in progress (10 minutes at most) finishes with the secret the server then holds |
| Object-store keys | `docker compose restart seaweedfs linea` | Server (and the bundled SeaweedFS pod) when the chart renders the Secret | Server, with `store.existingSecret` | Presigned URLs handed out before the restart no longer work |
| Database password | `docker compose restart linea` after the role change | Server (and the bundled PostgreSQL pod), when the chart renders the Secret | Server, with `store.existingSecret` | Connections already open keep working; new ones need the new password |
| Fleet operator token | `docker compose restart linea` | Server, when the chart renders the Secret | Server, with `fleet.existingSecret` | Live-capture calls fail until the server holds a token fleet accepts |
| Fleet admin token | Nothing | Nothing | Nothing | Nothing reads it but fleet's bootstrap |
| Live-capture tokens | `docker compose restart fleet livecapture-plugin` | Fleet and the plugin | Nothing | Fleet and the plugin are single pods replaced, not rolled; open captures end as interrupted and Collectors reconnect |
| Fleet and live-capture database URLs | Run `scripts/debugger/compose-fleet.sh` | Nothing | Fleet and the plugin, after the database Job finishes | Their connections stop at the restart |
| Live-capture certificate | `docker compose restart livecapture-plugin` | Nothing | The plugin | Collectors need the new certificate as their CA |

How the chart decides: the pod templates carry `checksum/...` annotations for
the Secrets the chart renders itself (`deploy/charts/linea/templates/server.yaml`,
`pool.yaml`, `worker.yaml`, `builder.yaml`, `stores.yaml`, `fleet.yaml`).
Server: config, admin, store, OIDC, worker token, builder token and fleet
token. Pool and dedicated workers: worker token. Builder: builder token.
Bundled PostgreSQL: its password. Bundled SeaweedFS: its keys. Fleet: the
plugin list and the host and service tokens. Plugin: its settings and all
three tokens. A Secret you provide through `existingSecret`, the fleet admin
and operator tokens, the fleet database Secret and every TLS Secret are not
hashed: restart those pods yourself:

```sh
kubectl -n <ns> rollout restart deploy/<fullname>                    # server
kubectl -n <ns> rollout restart deploy/<fullname>-pool deploy/<fullname>-worker deploy/<fullname>-builder
kubectl -n <ns> rollout restart deploy/<fullname>-fleet deploy/<fullname>-livecapture
```

### Changing a chart secret

The chart generates the admin password, the worker and builder tokens, the
bundled stores' credentials, the fleet tokens and the live-capture tokens
once, and keeps them: each render reads the current value back from the
Secret. To change one:

- Where the table names a value (`admin.password`, `worker.token`,
  `builder.token`, `oidc.clientSecret`, `seaweedfs.accessKey`, ...), set it in
  your values file and run `helm upgrade` with your values. Use a values file or
  `existingSecret`, not `--set`, which lands in your shell history.
- Where it names none (the fleet and live-capture secrets), edit the key in the
  Secret, then run `helm upgrade` with the same values. The chart reads your
  edit back and keeps it:

  ```sh
  kubectl -n <ns> patch secret <fullname>-livecapture \
    -p '{"stringData":{"host-token":"'$(openssl rand -hex 32)'"}}'
  ```

- Under `helm template` or any renderer without cluster access, nothing is
  kept: every value is generated again on each render, so set them all
  yourself ([stores](deploy-kubernetes.md#stores)).
- Equal builder and worker tokens are refused at install.

### The admin account

1. Write the new password (at least 16 characters, one line):
   Compose, `openssl rand -hex 32 > secrets/admin_password`; chart,
   `admin.password` or your `admin.existingSecret`.
2. Restart the server (Compose `docker compose restart linea`; chart
   `helm upgrade`, or `kubectl rollout restart deploy/<fullname>` with
   `existingSecret`). It reads the password at startup.
3. Revoke what the old password started. Changing the password does not end the
   admin's web sessions or API tokens: the server checks them against its
   database on every request, so they stay valid until they expire.
   `linea-server revoke-admin` revokes the built-in admin's API tokens and ends
   its web sessions, and writes one audit event with the two counts. It works
   whether or not the account is on, and a second run changes nothing. Run it
   with the server's own configuration:

   ```sh
   docker compose run --rm --no-deps linea revoke-admin
   kubectl -n <ns> exec deploy/<fullname> -- /linea-server revoke-admin --config=/etc/linea/config/config.yaml
   ```

   The first line is Compose (`run` replaces the service's command, and the
   environment carries the configuration); the second is Kubernetes.
4. Tell the people who signed in as `admin` to run `linea login --admin
   --server <url>` again. Their stored token ends.

Without step 3, an admin session or token that someone copied keeps working
after the rotation. To turn the account off instead, see [the admin
account](deploy-compose.md#the-admin-account).

### Workers and builders

The server, the pool, a dedicated worker or builder, and every worker you run
elsewhere must hold the same token. A worker that connects with the old token
is refused and stops, so restart them together:

```sh
openssl rand -hex 32 > secrets/worker_token        # or sudo tee, see below
docker compose restart linea pool
```

- If you restricted the two token files as [the Compose
  page](deploy-compose.md#secrets) describes (owner 65532, mode 440), write
  them with `openssl rand -hex 32 | sudo tee secrets/worker_token >/dev/null`.
  `tee` keeps the owner and mode.
- Restart the builder when no build runs. The restart waits for a running build
  (up to 35 minutes); `docker compose kill builder` ends it, and the server
  retries a build killed that way once.
- The server drains on restart: it stops claiming work, lets runs on workers
  finish for up to 25 seconds (`--drain-timeout`), and hands the rest back to
  the queue ([stops and upgrades](deploy-compose.md#stops-and-upgrades)). The
  pool cancels what its runtimes run, and the server retries those runs.
- Chart: one `helm upgrade` replaces the server, pool, workers and builder
  with the new token. Workers in another cluster, such as those from
  `examples/kubernetes/`, need the new token in their own Secret.

### The object store keys

Create the new key at the store, put it in the secret files or Secret, and
restart. With your own store, add the new key before you delete the old one, so
instances that still hold the old one keep working through the rollout. The
bundled SeaweedFS has one identity: Compose restarts `seaweedfs` and `linea`
together (`docker compose restart seaweedfs linea`), and the chart restarts
both on `helm upgrade`. The server retries its storage until it answers.

The pool and builder hold no object-store keys: they use URLs the server
presigns. Rotate when no run or build is moving payloads, because a URL
presigned with the old key stops working.

### The database password

PostgreSQL takes `postgres_password` only when its volume is created. Changing
the file or the Secret changes nothing in the database, and a server started
with a password the database does not know cannot connect. Change the role
first, then the file.

Compose:

```sh
new=$(openssl rand -hex 32)
docker compose exec -T -e LINEA_NEWPW="$new" postgres \
  psql --quiet --set ON_ERROR_STOP=1 --username postgres --dbname postgres <<'SQL'
\getenv pw LINEA_NEWPW
ALTER ROLE linea PASSWORD :'pw';
ALTER ROLE postgres PASSWORD :'pw';
SQL
printf '%s' "$new" > secrets/postgres_password
docker compose restart linea
```

The two roles share one password ([database
roles](deploy-compose.md#database-roles)), so both change. `linea` is the
server's role; the other is the superuser, which works only on the container's
local socket. With the fleet overlay, nothing else reads the file: the
`fleet-db` job reads it each time it runs.

Chart, your own PostgreSQL: change the role there, put the new URL in
`store.databaseURL` or your `store.existingSecret`, and roll the server. The
instances that still hold the old password cannot open new connections until
the rollout replaces them, so do it in a quiet period.

Chart, bundled PostgreSQL: its init script runs only on an empty volume.
Setting `postgresql.password` alone leaves the role with the old password and
the server unable to connect. Change the role in the pod first:

```sh
kubectl -n <ns> exec -it sts/<fullname>-postgresql -- psql -U postgres -d postgres
# in psql:  \password linea     then     \password postgres
```

Then set `postgresql.password` to the same value and run `helm upgrade`. The
PostgreSQL pod and the server restart on their own.

### The OIDC client secret

Register the new secret at your identity provider, and keep the old one valid
until the server runs with the new one, if the provider allows two. Write
`secrets/oidc_client_secret` (chart: `oidc.clientSecret` or your
`oidc.existingSecret`), then restart the server. The CLI's device sign-in uses
the public client (`cli_client_id`), which has no secret.

### Fleet and live-capture secrets

Fleet and live-capture credentials are fleet credentials of the `admin` and
`operator` roles, installed once in fleet's database. A new file does not
replace the installed one: issue a new credential, give it to its user, then
revoke the old one ([fleet credentials](../fleet/README.md#register-collectors-with-a-deployment-token)).
A credential's ID is the second `_`-separated field of its token. Issue with
the current admin token (Compose: the management port is `127.0.0.1:8080`;
chart: `kubectl port-forward svc/<fullname>-fleet 8080:8080`):

```sh
API=http://127.0.0.1:8080/api/v1
ADMIN="Authorization: Bearer $(cat secrets/fleet_admin_token)"
old=$(cut -d_ -f2 secrets/fleet_operator_token)
new=$(openssl rand -hex 16)
# The file is written only if both steps work, never left empty.
resp=$(curl -fsS -H "$ADMIN" -H 'Content-Type: application/json' -X POST $API/credentials \
  -d '{"id":"'$new'","name":"linea-server","role":"operator"}') &&
  token=$(printf '%s' "$resp" | jq -er .token) &&
  printf '%s\n' "$token" > secrets/fleet_operator_token
docker compose -f compose.yaml -f compose.fleet.yaml restart linea
# Fleet -> Connection in the web client shows the new token works. Then:
curl -fsS -H "$ADMIN" -X DELETE $API/credentials/$old
```

- **Operator token.** As above. Restart the server before you delete the old
  credential: revoking closes every session that uses it. If you set
  `raw_principals` in `livecapture.json` to the operator's credential ID,
  change it to `$new` and restart `livecapture-plugin`. On the chart, write
  the new token into the key `operator-token` of `<fullname>-fleet` and run
  `helm upgrade`: the server restarts, and with `fleet.deploy.livecapture.rawCapture`
  the plugin's settings change and it restarts.
- **Admin token.** Issue a new credential with `"role":"admin"` and the name
  `deployment-admin`, store it in `secrets/fleet_admin_token` (chart: key
  `token` of `<fullname>-fleet`), and revoke the old one with the new admin
  token. Fleet's bootstrap accepts the new token as current, because its
  credential exists and is active. Do not just write a new random token into
  the file: bootstrap refuses an admin token that is not installed while
  another admin is active.
- **Live-capture tokens.** Fleet and the plugin hold the host and service
  tokens as a pair, and the plugin holds the metrics token. Replace the files
  (Compose) or the Secret keys (chart), then restart fleet and the plugin
  together. Compose: `docker compose -f compose.yaml -f compose.fleet.yaml
  restart fleet livecapture-plugin`. Plugin configuration, token and
  certificate changes need a plugin restart ([live-capture
  plugin](../fleet/livecapture.md#rotation-backup-restore-and-upgrade)).
- **Fleet and live-capture database roles.** Compose: write new passwords in
  the four URL files, run `scripts/debugger/compose-fleet.sh` (its first step
  sets the roles' passwords from the files, the last restarts the stack). Chart:
  edit the `*-password` keys of `<fullname>-fleet-db` and run `helm upgrade`;
  its Job sets the passwords. Then `kubectl rollout restart` fleet and the plugin.
- **Live-capture certificate.** The generated one lasts 825 days. Replace
  `capture_tls.crt` and `.key` (chart: your `tls.secretName` Secret, or delete
  the generated `<fullname>-livecapture-tls` and run `helm upgrade`), restart
  the plugin, and give Collectors the new certificate as their CA.

### Users' API tokens

Personal and service API tokens live 1 to 365 days (90 by default). To rotate
one, create the new one, change CI to use it, then `linea token revoke --server
<url> --token <id>` ([API tokens](team-server.md#api-tokens)).

## Upgrade and rollback

[Compatibility](../compatibility.md) is the rule: the server, the pool and
dedicated workers, and the builder are of one release, and the debugger's
schema migrates forward only. A server refuses a database that a later release
migrated. So rolling back after an upgrade that migrated means restoring the
backup you took before it.

Order: back up, upgrade, verify. Read the release's notes in
[`CHANGELOG.md`](../../CHANGELOG.md) first: they say what an upgrade needs.

### Docker Compose upgrade

1. **Back up** ([backup](#docker-compose-backup)), and write down:
   - the release you run (`LINEA_VERSION`, or the tag in `compose.yaml`);
   - the schema version:
     `docker compose exec -T postgres psql -U postgres -d linea -Atc 'SELECT max(version) FROM schema_migrations'`.
   Keep the old `compose.yaml` and `.env` beside the backup (the `config.tgz`
   does).
2. **Fetch the new files** from the new release's tag: `compose.yaml`,
   `.env.example` and, with the overlay, `compose.fleet.yaml`, `livecapture.json`
   and `fleet-plugins.json`. Create any secret file the new `compose.yaml` lists
   that you lack, and add to `.env` any setting you want
   ([stops and upgrades](deploy-compose.md#stops-and-upgrades)).
3. **Upgrade.** Following [the upgrade order](../compatibility.md#upgrade-order),
   stop the workers and the builder, move the server, then start them on the
   same release:

   ```sh
   docker compose stop pool builder      # the builder waits for a running build
   docker compose pull
   docker compose up -d
   ```

   With the overlay, pass both files to `pull` and `stop`, and run
   `scripts/debugger/compose-fleet.sh` instead of `up -d`: it migrates fleet and
   the plugin, which stay explicit, and starts the stack. Upgrade fleet and its
   plugin together when the plugin host protocol changed.
4. **Verify.**
   - `docker compose ps` shows every service running and `linea` healthy, and
     `curl -fsS http://127.0.0.1:7070/readyz` answers 200.
   - `docker compose logs linea` shows no migration or storage error. A server
     that cannot migrate stops and names why.
   - The schema query from step 1 returns the new release's version, or the
     same number if the release added none.
   - The server logs `built-in runtime registered` once the pool connects: the
     pool is of the server's release.
   - Open the runtimes view. A runtime built for another worker protocol is
     listed as stale: rebuild it with `linea runtime build` (see
     [compatibility](../compatibility.md#version-table)).
   - Run a sample: `linea test --server http://127.0.0.1:7070 --config
     logs-cleanup/config.yaml --scenario logs-cleanup/scenario.yaml`, after
     `linea samples get logs-cleanup`.
   - With the overlay, **Fleet → Connection** in the web client shows the
     connection works.

### Kubernetes upgrade

1. **Back up** your PostgreSQL and bucket ([backup](#kubernetes-backup)), and write
   down the chart revision (`helm history <release> -n <ns>`), your values
   (`helm get values <release> -n <ns>`) and the schema version
   (`SELECT max(version) FROM schema_migrations` in the database `linea`).
2. **Upgrade** with the new chart and the same values file:

   ```sh
   helm upgrade <release> ./linea-<new-version>.tgz -n <ns> -f values.yaml
   kubectl -n <ns> rollout status deploy/<fullname> deploy/<fullname>-pool deploy/<fullname>-builder
   ```

   The images default to the chart's release, so server, pool and builder move
   together. If you set `image.tag`, `pool.image.tag` and `builder.image.tag`
   yourself, set them to one release. The rollout replaces server pods one at a
   time: the first new pod migrates the database, so for a short time old and
   new pods serve from the migrated database ([rolling
   upgrades](team-server.md#connecting-workers)). The chart moves the pool and
   builder in the same upgrade. If the release notes say the release does not
   support that overlap, scale the server to 0 before you upgrade.
3. **Verify** as for Compose: the rollout finishes, `/readyz` answers (the pods
   become ready), the pods' logs show no migration error, the runtimes view has
   no stale runtime, and a sample runs.

### Rollback

There is no down-migration. A server refuses a database a later release
migrated, so rolling the images back is not enough once the new release has
migrated.

- **The new release did not start.** A migration that fails rolls back as a
  whole: the database is as it was. Start the old release again
  (Compose: the old `compose.yaml` and `LINEA_VERSION`, then `docker compose up
  -d`; chart: `helm rollback <release> <revision> -n <ns>`).
- **The new release migrated, and you want the old one back.** Restore the
  backup you took in step 1 of the upgrade:
  1. Stop the servers and workers.
  2. Restore PostgreSQL and the object store ([restore](#restore)).
  3. Start the old release: Compose with the old `compose.yaml` and
     `LINEA_VERSION`; chart with `helm rollback <release> <revision> -n <ns>`
     (it restores the old objects, never the database).
  4. Verify as above.

  Runs and changes made after the backup are lost. People who signed in after it
  sign in again. Tokens you revoked after it are valid again: revoke them.

With fleet and live capture, restore their backups from the same moment as
the debugger's. Each part refuses a schema newer than its binary
([databases](../compatibility.md#databases)).
