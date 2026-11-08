# Docker Compose

A small production setup: Claviger, a data volume, and optional Caddy for HTTPS.

## Run one instance only

Claviger stores data in SQLite. **Run exactly one container per data volume.**
Do not use `docker compose up --scale claviger=2`. Two writers corrupt the database.

## Why HTTPS and a stable name matter

Passkeys only work over HTTPS (or on `localhost`). A passkey is also bound to the
host name it was created on (the "RP ID"). If you change the domain later,
existing passkeys stop working. Pick your final domain before you enrol anyone.

## Prerequisites

- Docker with the Compose plugin.
- For HTTPS: a DNS name pointing at the host, and ports 80 and 443 open.

## Set up

```sh
cp .env.example .env
openssl rand -hex 16        # paste the output as PB_ENCRYPTION_KEY
$EDITOR .env                # set CLAVIGER_VERSION, CLAVIGER_DOMAIN, PB_ADMIN_*
```

Set `PB_ENCRYPTION_KEY` before the first start. Keep a copy somewhere safe.
Without it you cannot read the encrypted settings again.

Start with HTTPS:

```sh
docker compose --profile caddy up -d
```

Or start without a proxy and test on `http://127.0.0.1:8090` (uncomment `ports` in
`compose.yaml` first):

```sh
docker compose up -d
```

Check it:

```sh
docker compose ps
curl -fsS https://auth.example.com/api/health
```

The dashboard is at `/_/`. After the first start, remove `PB_ADMIN_PASSWORD`
from `.env`. The first superuser already exists.

## Real client IPs

Behind a proxy, Claviger sees the proxy's address unless you tell it which header
to trust. Rate limits then count all users as one.

1. Open the dashboard: Settings, Application, "User IP proxy headers".
2. Add `X-Forwarded-For`.
3. Leave "Use leftmost IP" off. The bundled Caddyfile overwrites the header with
   the real peer address, so there is only one value.

Only do this when every request really passes through your proxy.

## Read-only container

`compose.yaml` runs the container with a read-only root file system. The
entrypoint writes only to `/pb_data` (the volume) and `/tmp` (a tmpfs). If you add
hooks or public files, mount them read-only as shown in the file.

The image runs as root inside the container. Dropping privileges is not supported yet.

## Back up

The simplest safe backup is a short stop:

```sh
docker compose stop claviger
docker run --rm -v claviger_claviger_data:/data -v "$PWD":/backup alpine \
  tar czf /backup/claviger-$(date +%F).tgz -C /data .
docker compose start claviger
```

The volume name is `<project>_claviger_data`, and the project name is `claviger`.
Check with `docker volume ls`.

For continuous backup, enable Litestream. Uncomment the `litestream.s3.yml` volume
in `compose.yaml`, then fill the `LITESTREAM_*` values in `.env`, including a
non-empty `LITESTREAM_REPLICA_URL` (for example `s3://my-bucket/claviger`). This
variable switches replication on. Back up `PB_ENCRYPTION_KEY` separately: Litestream
does not store it.

On a new host with an empty volume and the same `.env`, the container restores
from the replica before it starts.

You can also use the Backups page in the dashboard.

## Upgrade

```sh
$EDITOR .env                 # set the new CLAVIGER_VERSION
docker compose pull
docker compose --profile caddy up -d
docker compose logs -f claviger
```

Take a backup first, and read the release notes. A migration may not be
reversible, so to roll back after a schema change, restore the backup.

## Files

| File | Purpose |
|---|---|
| `compose.yaml` | Services and volumes |
| `.env.example` | Every setting, with comments |
| `Caddyfile` | HTTPS reverse proxy |
| `litestream.s3.yml` | Litestream config for S3-compatible storage |

## Checked

`docker compose config` passes for both the default and `caddy` profiles. The
containers themselves were not started when this was written (no Docker daemon).
