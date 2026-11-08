# Fly.io

One machine, one volume, HTTPS from Fly.

## Run one machine only

Claviger stores data in SQLite on a Fly volume. A volume attaches to one machine,
and the app must run exactly one machine. Never run `fly scale count 2`.
`auto_stop_machines` is off so the machine is not stopped when idle.

Fly gives you `https://<app>.fly.dev` with a valid certificate. Passkeys need
HTTPS and a stable host name. If you will use your own domain, add it first
(`fly certs add auth.example.com`) and enrol users on that name, not on `fly.dev`.

## Prerequisites

- The `flyctl` CLI and a Fly account (`fly auth login`).
- A release tag of the image. Edit `build.image` in `fly.toml`.

## Deploy

```sh
cd deploy/fly
# 1. Edit fly.toml: app, primary_region, image tag.
fly apps create your-claviger-app
fly volumes create claviger_data --size 1 --region iad --count 1

# 2. Secrets. PB_ENCRYPTION_KEY must be exactly 32 characters.
fly secrets set --stage \
  PB_ENCRYPTION_KEY="$(openssl rand -hex 16)" \
  PB_ADMIN_EMAIL=admin@example.com \
  PB_ADMIN_PASSWORD='a-long-random-password'

# 3. Deploy a single machine.
fly deploy --ha=false
fly status
fly scale count 1
```

Save the encryption key before you stage it, because `fly secrets` will not show
it again. Without it, encrypted settings cannot be read.

After the first start, remove the bootstrap password:

```sh
fly secrets unset PB_ADMIN_PASSWORD
```

Optional secrets: `CLAVIGER_METRICS_TOKEN`, `OTEL_EXPORTER_OTLP_ENDPOINT`,
`OTEL_EXPORTER_OTLP_HEADERS`, `LITESTREAM_*`.

## Real client IPs

Fly's proxy adds `Fly-Client-IP`. In the dashboard, open Settings, Application,
"User IP proxy headers" and add `Fly-Client-IP`. Otherwise rate limits see Fly's
internal addresses.

## Back up

Fly takes daily volume snapshots (`fly volumes snapshots list`). To restore, create
a volume from a snapshot (`fly volumes create ... --snapshot-id ...`).

For point-in-time recovery, set `LITESTREAM_*` secrets. The image's built-in
Litestream config targets Azure Blob Storage. For S3-compatible storage, use the
config in `../docker-compose/litestream.s3.yml`; you would need a derived image that
copies it to `/etc/litestream.yml`.

You can also use the Backups page in the dashboard, or:

```sh
fly ssh console -C "ls -la /pb_data"
fly sftp get /pb_data/data.db
```

(Copying a live SQLite file can be inconsistent; prefer snapshots or Litestream.)

## Upgrade

```sh
# Edit build.image in fly.toml to the new tag, then:
fly deploy --ha=false
fly logs
```

`strategy = "immediate"` replaces the machine without a second one running beside it.
There is a short outage during the swap. Take a snapshot first
(`fly volumes snapshots create <volume-id>`).

## Not verified

`flyctl` was not available when this template was written, so
`fly config validate` has not been run on it. Run it before you deploy.
