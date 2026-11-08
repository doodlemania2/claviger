# Azure Container Apps

A generic Bicep template: Container App, Azure Files NFS share, VNet, and logs.
It has no owner-specific names. Everything derives from the `name` parameter.

## Run one replica only

Claviger stores data in SQLite. The template sets `minReplicas` and `maxReplicas`
to 1. Do not change that.

`maxReplicas: 1` does **not** cover a rollout. During an update, Container Apps starts
the new replica and waits until it is ready before it stops the old one. For about
40 seconds two containers have the share mounted. Left alone, that corrupts the database.

The image's `entrypoint.sh` handles this. The incoming replica writes a request file
on the share. The outgoing replica sees it, closes both databases, acknowledges, and
parks. Then the incoming replica opens the databases. This needs no setup beyond the
variables below. Do not replace it with a blocking startup lock: readiness gates the
handover, so a replica that blocks at startup would deadlock the rollout.

| Variable | Template default | Meaning |
|---|---|---|
| `PB_HANDOVER_TIMEOUT` | `60` | Seconds the incoming replica waits for the outgoing one to let go. After that it starts anyway (fail-open). `0` turns the handover off. That is unsafe. |
| `PB_HANDOVER_POLL` | `2` | Seconds between the outgoing replica's checks for a request. |
| `PB_INSTANCE_ID` | container hostname | Name of this replica. The default is right. Do not set it. |

Handover events are written to `/pb_data/.pb_handover.log` on the share.

## Why NFS and not SMB

SQLite in WAL mode needs POSIX byte-range locks. SMB Azure Files shares do not
provide them. On SMB, the app crash-loops with `SQLITE_BUSY`. NFS does.

NFS on Azure Files has some rules, and the template follows them:

- The storage account is `FileStorage` kind, `Premium_LRS`. Minimum share size is 100 GiB.
- NFS has no passwords. Access is by network only, so the storage account denies
  all traffic except from the Container Apps subnet (service endpoint).
- `supportsHttpsTrafficOnly` is false. NFS requires that.
- The Container Apps environment is VNet-integrated (workload profiles type).
- Root squash is off, because the image runs as root.

## Prerequisites

- Azure CLI (`az`) and a subscription.
- A resource group.
- A release tag of the image.

## Deploy

```sh
az group create -n claviger-rg -l westeurope

az deployment group create -g claviger-rg \
  --template-file main.bicep \
  --parameters name=claviger clavigerVersion=0.1.0 \
               encryptionKey="$(openssl rand -hex 16)" \
               adminEmail=admin@example.com \
               adminPassword='a-long-random-password'
```

The deployment prints the app `url`. Check it: `curl -fsS <url>/api/health`.

Save the encryption key from this command (print it first, or generate it
beforehand into a variable). Without it, encrypted settings cannot be read.
Avoid leaving it in shell history: use a parameter file kept out of git.

Parameters you can add: `metricsToken`, `otlpEndpoint`, `auditRetentionDays`,
`shareSizeGiB`, `cpu`, `memory`, `handoverTimeoutSeconds`, `handoverPollSeconds`.

## Your own domain and passkeys

Passkeys need HTTPS and a host name that never changes (the "RP ID"). Add a custom
domain to the Container App (Container Apps manages the certificate), and enrol users
on that name, not on the `azurecontainerapps.io` one:

```sh
az containerapp hostname add -g claviger-rg -n claviger --hostname auth.example.com
az containerapp hostname bind -g claviger-rg -n claviger --hostname auth.example.com --environment claviger-env --validation-method CNAME
```

## Real client IPs

The Container Apps ingress sets `X-Forwarded-For`. In the dashboard open Settings,
Application, "User IP proxy headers" and add `X-Forwarded-For`. Otherwise rate
limits see the ingress address.

## Back up

- Azure Files NFS shares do not support Azure Backup or share snapshots in all
  regions. Check current support in your region before you rely on them.
- Use the Backups page in the dashboard to write archives to the share, and copy
  them off.
- For continuous backup, add the `LITESTREAM_*` variables. The image's built-in
  Litestream config targets Azure Blob Storage (`LITESTREAM_REPLICA_URL`,
  `LITESTREAM_AZURE_ACCOUNT_NAME`, `LITESTREAM_AZURE_ACCOUNT_KEY`). Add them to
  `baseEnv` and `baseSecrets` in `main.bicep`. This was not added to the template by default.
- Back up the encryption key separately.

## Upgrade

```sh
az deployment group create -g claviger-rg \
  --template-file main.bicep \
  --parameters name=claviger clavigerVersion=0.2.0 encryptionKey="<same key>"
az containerapp logs show -g claviger-rg -n claviger --follow
```

Use the **same** `encryptionKey`. A different value makes the stored settings unreadable.
Back up first and read the release notes.

## Checked

`az bicep build --file main.bicep` compiles with no errors or warnings (Bicep 0.44.1).
The template was **not deployed**. Details that only a real deployment proves:
the NFS `shareName` path format, the `NfsAzureFile` volume mount, and the handover
under a real rollout. Report problems as issues.
