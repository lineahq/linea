# Linea team server with Docker Compose

`compose.yaml` starts the debugger's team server (web client, HTTP API and
MCP endpoint) from the published images, on `http://127.0.0.1:7070` by
default. It is the whole stack; a `.env` beside it configures it: how people
sign in, the address, port and TLS, limits and retention. It needs Docker
Engine 26 or later with Compose v2.30 or later.

1. In an empty directory, fetch `compose.yaml` from a release's tag, which
   pins that release's images:

   ```sh
   curl -fsSLO https://raw.githubusercontent.com/lineahq/linea/vX.Y.Z/deploy/compose/linea/compose.yaml
   ```

2. Put five random values and the sign-in mode in a `.env` beside it, and
   keep it as long as the data: PostgreSQL takes its password when its
   volume is created. `LINEA_AUTH=local` is one admin on `127.0.0.1` with no
   sign-in; for a team choose `oidc` or `proxy` ([sign-in](https://github.com/lineahq/linea/blob/main/docs/debugger/deploy-compose.md#sign-in)).

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

3. Start it:

   ```sh
   docker compose up -d
   ```

4. Open `http://127.0.0.1:7070` and choose **Try an example**, or send a run
   from the CLI with `--server`, which keeps it in the server's history:

   ```sh
   linea samples get logs-cleanup
   linea test --server http://127.0.0.1:7070 \
     --config logs-cleanup/config.yaml --scenario logs-cleanup/scenario.yaml
   ```

Idle, the stack uses about 260 MiB of memory. Its CPU and memory figures are
limits, not reservations, but Docker refuses a container whose CPU limit is
above the machine's CPU count. With fewer than 4 CPUs, add lower limits to
`.env`, for example:

```sh
LINEA_SERVER_CPUS=1
LINEA_POOL_CAPACITY=2
LINEA_POOL_CPUS=2
LINEA_BUILDER_CPUS=2
```

Stop it with `docker compose down` (add `-v` to remove the data too). In a
checkout, put `.env` here and run the commands from the root with
`-f deploy/compose/linea/compose.yaml`; `LINEA_VERSION=dev` and `--build`
build the images from the checkout. After editing `.env`, run
`docker compose up -d --force-recreate`. Every setting, from sign-in and TLS
to networks, upgrades and building runtimes, is in
[Running a team server with Docker Compose](https://github.com/lineahq/linea/blob/main/docs/debugger/deploy-compose.md).
