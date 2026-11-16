# Linea team server with Docker Compose

`compose.yaml` starts the debugger's team server (web client, HTTP API and
MCP endpoint) from the published images, on `http://127.0.0.1:7070` by
default. One stack serves one person and a team: its secrets are files in a
`secrets/` directory beside it, and an optional `.env` says how people sign in,
the address and the limits. It needs Docker Engine 25 or later with Compose
v2.24 or later.

1. In an empty directory, fetch `compose.yaml` and `.env.example` from a
   release's tag, which pins that release's images:

   ```sh
   curl -fsSLO https://raw.githubusercontent.com/lineahq/linea/vX.Y.Z/deploy/compose/linea/compose.yaml
   curl -fsSLO https://raw.githubusercontent.com/lineahq/linea/vX.Y.Z/deploy/compose/linea/.env.example
   ```

   ```powershell
   Invoke-WebRequest https://raw.githubusercontent.com/lineahq/linea/vX.Y.Z/deploy/compose/linea/compose.yaml -OutFile compose.yaml
   Invoke-WebRequest https://raw.githubusercontent.com/lineahq/linea/vX.Y.Z/deploy/compose/linea/.env.example -OutFile .env.example
   ```

2. Create the seven secret files: six random values, and the identity
   provider's client secret, empty without one. Keep `secrets/postgres_password`
   as long as the data: PostgreSQL takes its password when its volume is
   created. `secrets/admin_password` is the password of the built-in `admin`
   account.

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

   `.env` is optional. To change a setting, copy `.env.example` to `.env`.

3. Start it:

   ```sh
   docker compose up -d
   ```

4. Open `http://127.0.0.1:7070`, sign in as `admin` with the contents of
   `secrets/admin_password` and choose **Try an example**, or send a run from
   the CLI, which keeps it in the server's history:

   ```sh
   linea login --server http://127.0.0.1:7070
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
checkout, put `secrets/` (and `.env`) here and run the commands from the root
with `-f deploy/compose/linea/compose.yaml`; `LINEA_VERSION=dev` and `--build`
build the images from the checkout. After editing `.env`, run
`docker compose up -d`; after changing a secret file, run
`docker compose restart`, because services read their secrets when they
start. Every setting, from sign-in and a reverse proxy for other machines to
networks, upgrades and building runtimes, is in
[Running a team server with Docker Compose](https://github.com/lineahq/linea/blob/main/docs/debugger/deploy-compose.md).
