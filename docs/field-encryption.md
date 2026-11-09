# Field encryption

Claviger adds two field types, **Encrypted text** (`encryptedText`) and **Encrypted JSON** (`encryptedJSON`). Their values are stored encrypted in `data.db` (and so in backups and Litestream replicas) and are plaintext everywhere above the database: the records API, hooks, realtime and the admin UI.

Use it for values that should not be readable from a database file, a leaked backup or a SQL console: phone numbers, national ids, pastoral notes. It does not hide a value from the API. Anyone allowed to read the field through the API (a rule, or a superuser) gets the plaintext. Hide it with the field's **Hidden** option or a collection rule if that is what you need.

With no encrypted fields defined, nothing changes: no key is needed and no extra work runs.

## Set up

1. Generate a key and keep it somewhere other than the database and its backups:

   ```sh
   openssl rand -base64 32
   ```

2. Give it to the server, in one of these ways (first match wins):

   | Setting | Meaning |
   |---|---|
   | `CLAVIGER_KEK` | The key itself: base64 of exactly 32 bytes. Preferred. |
   | `CLAVIGER_KEK_FILE` | Path of a file holding the same base64 text. The file must not be readable by group or others (`chmod 600`). Setting both `CLAVIGER_KEK` and `CLAVIGER_KEK_FILE` is an error. |
   | `CLAVIGER_KEK_AZURE_KEY_ID` | Use a key in Azure Key Vault or Managed HSM instead of a local key: see [Azure Key Vault](#azure-key-vault). Excludes `CLAVIGER_KEK` and `CLAVIGER_KEK_FILE` (setting both is an error). |
   | `CLAVIGER_KEK_AWS_KEY_ID` | Use a key in AWS KMS: see [AWS KMS](#aws-kms). Needs a build with `-tags kms_aws`. Excludes every other key source. |
   | `CLAVIGER_KEK_GCP_KEY_ID` | Use a key in Google Cloud KMS: see [Google Cloud KMS](#google-cloud-kms). Needs a build with `-tags kms_gcp`. Excludes every other key source. |
   | the app encryption key (`PB_ENCRYPTION_KEY`, or whatever `--encryptionEnv` names) | Fallback. The key is derived with HKDF-SHA256 and the fixed info string `claviger/kek/v1`, so the same string is never used as two different keys. One secret then guards the settings and the fields, and rotating it means rotating both. |

3. Add the field in the collection editor (type **Encrypted text** or **Encrypted JSON**) or through the API with `"type": "encryptedText"` / `"type": "encryptedJSON"`. Saving a collection that has such a field without a key configured fails with an error naming the variables.

## What is stored

```
enc:v1:<keyId>:<base64url(nonce | ciphertext)>
```

AES-256-GCM, a random 96-bit nonce per value. The authenticated data binds each ciphertext to

```
claviger/field/v1 | collection id | field id | record id | key id
```

so a ciphertext copied into another record or another field does not decrypt, and the API cannot be used to decrypt a ciphertext someone found in a backup. The field id (not its name) is used, so renaming the field is safe. Empty values are stored as an empty string, which means which rows are empty is not hidden.

Saving a record without changing the field stores the same ciphertext again instead of encrypting anew.

## Encrypted JSON

`encryptedJSON` uses the same scheme, the same stored format, the same AAD and the same key hierarchy; each field gets its own data key. The JSON text is sealed as a whole, and above the database the value is an ordinary JSON value: the records API accepts and returns real JSON (an object, an array, a number, a string), hooks see a `json` value.

- The column is plain `TEXT`, not a JSON column. `null` and the empty value are stored as an empty string.
- Options: **Max size** (bytes of the plaintext JSON, default about 1 MB), **Required** (not `null`, `""`, `[]`, `{}`), **Hidden**, help text. Presentable is not offered.
- Input is parsed like a `json` field (the same string normalizations apply). A string that merely looks like ciphertext (`"enc:v1:..."`) is stored as a JSON string; ciphertext cannot be pasted in.
- Not filterable, sortable, indexable or usable in API rules, with the same errors as `encryptedText`. A JSON path into the field (`profile.a = 1`) is rejected as an invalid path.
- The admin UI uses the same code editor as the `json` field.

## Key hierarchy

```
KEK  (CLAVIGER_KEK, a file, or HKDF of the app key)
 |  wraps (AES-256-GCM)
DEK  one random 256-bit key per encrypted field, created when the field is added
 |  seals
field values
```

The wrapped DEKs live in the hidden system collection `_fieldKeys` (`collectionRef`, `fieldRef`, `keyId`, `kekId`, `wrapped`, `retired`, `created`). They are not in the field options on purpose: those are part of the collection JSON, which is exported, snapshotted and shown in the admin UI. `_fieldKeys` is superuser-only through the API, and the server refuses every write to it through the app (records API, Go and JS hooks). The server creates its rows with raw SQL when a collection with an encrypted field is saved.

`kekId` is a fingerprint of the KEK (HMAC-SHA256 of a fixed string), `keyId` is a random id. Both are stored in the clear and are safe to print. Wrong keys are reported as "different key-encryption key" with both ids, not as a generic decryption error. The ids are what a later rotation uses to tell old data from new.

## Limits

Encrypted fields cannot be filtered on, sorted by, indexed or used in API rules. The column holds random ciphertext, so any of these would silently return nothing or the wrong thing. Instead they are rejected:

| Where | What happens |
|---|---|
| A unique or any other index that mentions the field | The collection save fails ("Encrypted fields cannot be indexed"). |
| An API rule (own collection, or another collection through a relation or `@collection`) that references the field | The rule is rejected when the collection is saved. `@request.body.<field>` is allowed: it is the submitted plaintext. |
| `filter=` or `sort=` on the field in the records API | HTTP 400, "Encrypted fields cannot be used in filters, sorts or API rules". |
| A value that starts with `enc:v1:` | Validation error (reserved prefix). |
| An encrypted field placed before the `id` field | The collection save fails. |

There is no searchable or exact-match encryption. To find a record by phone number, store a separate, non-secret lookup value in a plain field. Other options:

- **Presentable** is not offered: the value would be shown as the record's label.
- **Hidden** works as for any field (hidden from the API response and from filters; superusers still see hidden fields, as in upstream).
- Min, max (default 5000) and Required apply to the plaintext length in characters.
- Hooks (`$app` in JS) see plaintext. A raw `$app.db()` query sees ciphertext.
- Realtime events carry the same plaintext the API would return to that subscriber.
- Changing an existing `text` (or `json`) field to the encrypted type keeps the values readable: a stored value without the `enc:v1:` prefix is read as plaintext and is encrypted the next time its record is saved. `claviger keys convert` encrypts them all at once, and strict mode (below) then forbids unencrypted values.

## Audit log and webhooks

The audit log redacts every `encryptedText` and `encryptedJSON` field **by type**, whatever its name (other fields are still redacted by the existing name and `hidden` rules). Webhook payloads are built from the audit entry, so they inherit this. Request logs do not store bodies.

## Fail closed

At startup, if any collection has an encrypted field and the key is missing or cannot unwrap the stored DEKs, the app refuses to start and says which field and which key ids:

```
cannot start: encrypted field people.phone: cannot unwrap data key 1a40b6... with the env key source
(key id kdeafb..., data wrapped under kb0d64...): the data was wrapped with a different key-encryption key
```

Nothing is written unencrypted and nothing becomes unreadable by accident. A removed field's leftover key that can no longer be unwrapped only logs a warning.

### Pre-flight check

```sh
claviger keys check --dir=/pb_data [--encryptionEnv=PB_ENCRYPTION_KEY]
```

Exit 0: the keys open, or there are no encrypted fields, or the database could not be inspected. Exit 1 with a line starting `keys check: FAILED`: an encrypted field exists and the key is missing, malformed or wrong. It does not start the app, run migrations or write anything: it opens `data.db` read-only and immutable, so it is safe to run while another process owns the directory. The trade-off is that it does not see changes still sitting in the WAL (a field added in the last few minutes), in which case it passes and the startup check catches the problem.

`entrypoint.sh` runs it before the single-writer handover request is written. Without that, a bad key would be discovered only after the outgoing replica had already released `/pb_data`, with nothing left serving. Only the explicit `keys check: FAILED` verdict blocks the start; a timeout, a crash or an old binary without the command logs a warning and the start goes on, like the rest of the handover. It is skipped when there is no `data.db` yet. Set `PB_KEYS_PREFLIGHT=0` to disable it.

## Key rotation

There are two independent rotations.

| | What changes | Cost | Command |
|---|---|---|---|
| **Data key (DEK)** | A new random key for the field; every row is re-encrypted under it. | Proportional to the data. | `keys rotate` |
| **Key-encryption key (KEK)** | Every data key is re-wrapped under the new KEK. Row data is not touched. | Seconds. | `keys rewrap` (or `keys rotate --new-kek`) |

Both are safe on a live system and both can be started in the **Settings > Encryption keys** page of the dashboard (superusers).

### Where it runs, and why

The deployment is single-writer: one process owns `/pb_data` (SQLite, NFS, Litestream, the handover in `entrypoint.sh`). A second process that opens the same database to rotate keys would be a second writer. So the primary way to rotate is **inside the serving process**:

```
GET  /api/field-keys           the keys and pending work of every encrypted field (+ the current job)
GET  /api/field-keys/job       the current or last job
POST /api/field-keys/rotate    {collection?, field?, batchSize?}
POST /api/field-keys/rewrap    {}
POST /api/field-keys/convert   {collection, field?}
POST /api/field-keys/prune     {collection?, field?}
POST /api/field-keys/strict    {collection, field, strict}      (synchronous)
```

Superusers only; an API key is refused even if it is scoped `*`. The `POST`s other than `strict` start a background job and answer `202`; one job runs at a time (`409` otherwise). A job stops between two batches when the server shuts down. Every run writes **one** audit entry (action `field_key_rotate`, `field_key_rewrap`, `field_key_convert`, `field_key_prune`, `field_key_strict`, target `_fieldKeys`; counts and key ids, no plaintext, no key material), never one per row.

The CLI does the same operations offline:

```sh
claviger keys status
claviger keys rotate  [--collection X] [--field Y] [--batch-size 500]
claviger keys rotate  --new-kek          # = keys rewrap
claviger keys rewrap
claviger keys convert --collection X [--field Y]
claviger keys strict  --collection X --field Y [--off]
claviger keys prune   [--collection X] [--field Y]
```

Because the CLI opens the data directory, it **refuses to run** when it finds a live server: the single-writer lock `.pb_singlewriter.lock` is held (the whole process tree of a replica holds it) or `.pb_handover` exists (a handover is in progress). The message says what it found. Pass `--offline-ok` if you are certain no server is running; it also overrides a false positive. A server started without `entrypoint.sh` takes no lock and cannot be detected, so `--offline-ok` is then simply your statement. While a CLI command runs it holds the lock, so a replica that starts meanwhile waits for it. `docker exec` into the running replica and running the CLI there is refused for this reason; use the dashboard or the API instead.

### Rotating a data key

```sh
claviger keys rotate --collection people --field phone
```

1. A new data key (new `keyId`) is created for the field and the old one is marked **retired**. The retired key stays available for reads. From this moment the app seals every write with the new key.
2. The rows are re-encrypted in batches of 500 rows per transaction, with a progress log. Each row update is a compare-and-swap on the stored value, so a row the app saved in the meantime is never overwritten with stale data: it was written under the new key already, or a final pass catches it.
3. When no row uses the old key, `keys prune` deletes it. Pruning a key that a row still uses is refused (the command reports how many rows use it).

It is **resumable and idempotent** without a stored cursor. The work set is "rows whose value is not on the current key". If a run stops half way (a crash, a restart, an error), run the same command again: it finds the field with a retired key and rows still on it, continues with the same new key and skips the rows already moved. `keys status` shows a half-finished rotation (rows on old keys, a retired key that is still in use). Rows that cannot be decrypted (corrupt, or a ciphertext moved to another record) are left alone and make the run end with an error naming how many; they keep their old key from being pruned.

Rotation uses raw SQL, so it fires no record hooks, no realtime events and no per-row audit entries, and it does not change `updated`. Rows holding unencrypted legacy values are not touched by it (see `keys convert`).

Rotate on a schedule that suits you; there is no hard limit, but every DEK should stay well under 2^32 values sealed (random 96-bit nonces), which in practice means a rotation every few years for most deployments.

### Rotating the key-encryption key

The app must be able to read the data keys while you switch, so both KEKs are made available for a while:

1. Generate the new KEK (`openssl rand -base64 32`) and store it.
2. Deploy with the **new** KEK as the primary one and the **previous** one declared:

   | Setting | Meaning |
   |---|---|
   | `CLAVIGER_KEK_OLD` | The previous KEK (base64). |
   | `CLAVIGER_KEK_FILE_OLD` | A file holding it (mode 600). |
   | `CLAVIGER_KEK_OLD_FROM_APP_KEY=1` | The previous KEK is the one derived from `PB_ENCRYPTION_KEY`: use this to move from the fallback to a dedicated `CLAVIGER_KEK`. |
   | `CLAVIGER_KEK_AWS_KEY_ID_OLD` / `CLAVIGER_KEK_GCP_KEY_ID_OLD` | The previous KEK is an AWS KMS or Google Cloud KMS key (same format as the primary variable). Needs the same build tag. |
   | `CLAVIGER_KEK_AZURE_KEY_ID_OLD` | The previous KEK is a Key Vault key (URL as for `CLAVIGER_KEK_AZURE_KEY_ID`): use this to move from Key Vault back to a local key, or to another Key Vault key. |

   Set only one of them. The app starts, reads every data key through the previous KEK, and writes new keys under the primary. `keys check` (the entrypoint pre-flight) accepts the same variables. A previous KEK that is set but malformed or unreadable is an error: it fails closed.
3. Run **Re-wrap** in the dashboard, `POST /api/field-keys/rewrap`, or `claviger keys rewrap`. All data keys, retired ones included, are re-wrapped in one transaction; each is unwrapped again and compared before the commit. If a data key is wrapped under a KEK that is not available, nothing is changed and the error names the missing KEK id and the variables to set.
4. Remove the `*_OLD` variable and redeploy. From now on the old KEK opens nothing.

Rewrap needs both KEKs, so you cannot rewrap a database with only the new key.

### Converting an existing column, and strict mode

Change the field type of an existing `text` field to `encryptedText` (or `json` to `encryptedJSON`), keeping the field id. The old values stay readable as plaintext. To encrypt them all:

```sh
claviger keys convert --collection people --field phone     # or "Encrypt legacy values" in the dashboard
claviger keys strict  --collection people --field phone     # then forbid unencrypted values
```

`convert` is idempotent and resumable (the work set is "non-empty values without the prefix"); legacy JSON that is not valid JSON becomes a JSON string. **Strict mode** makes a stored value without the `enc:v1:` prefix a read error ("holds an unencrypted value and the field is in strict mode"), so a value written around the app (a SQL console, a bad restore) cannot silently pass as data. Turning it on is refused while unencrypted values exist, so run `convert` first. The flag is stored with the field's data key (`strictMode` in `_fieldKeys`), not in the field options, so it is not exported with the collection; it carries over to a rotated key and survives restarts. `strict --off` turns it off. Empty values are always fine.

## Losing the key loses the data

The KEK is the only way to read the encrypted values. Backups contain `data.db` with the ciphertext **and** the wrapped DEKs, which is useless without the KEK.

- Store the KEK in at least one place that is not the server and not the backup storage (a password manager, a sealed envelope held by two people).
- Test a restore together with the key before you rely on it.
- Changing `CLAVIGER_KEK`, or `PB_ENCRYPTION_KEY` when it is the source, without the rotation steps above makes the existing data unreadable and the app will not start. Rotate with `CLAVIGER_KEK_OLD` and `keys rewrap`.
- Pruning a retired data key is final. A backup taken before the prune still holds the key, but also the rows of its time: restore the two together. Keep a backup from before you prune until you are satisfied with the rotation.
- During a DEK rotation, a backup holds a mix of old and new keys; that is fine, all of them are in `_fieldKeys`.

## Encrypting the backups too

Field encryption protects individual values inside `data.db`. To encrypt the whole backup archive (database, uploaded files, settings) with the same key, switch on **Encrypt new backups**: see [Backup encryption](backup-encryption.md). Rotating the KEK affects old encrypted backups too: keep the previous key (`CLAVIGER_KEK_OLD`) for as long as you keep backups made under it.

## Deployment notes

- Single replica / handover: nothing to change besides providing the variable to the container and the pre-flight step above. Provide the same variable to every command that opens the database (`superuser`, `migrate`, ...), since they bootstrap the app and fail closed too.
- Rotate through the dashboard or the API, not by `docker exec` of the CLI (it refuses, see above).
- `ui/dist` contains the field types and the Encryption keys page; there is nothing to configure in the dashboard.
- Metadata is not hidden: the length of values, whether a value is empty, and when a row was written.

## Roadmap

Cloud key sources are tracked in #75: Azure Key Vault, AWS KMS and Google Cloud KMS are implemented (the last two behind build tags). The `keyId` in every ciphertext and the `retired` column of `_fieldKeys` are what rotation is built on; a KMS source only has to implement wrap and unwrap.

## Azure Key Vault

The key-encryption key can live in Azure Key Vault (or a Managed HSM) so that it never reaches the server. Claviger only asks the vault to wrap and unwrap the field data keys (envelope encryption); record data never goes to Azure. It talks to the Key Vault REST API directly (API version `7.5`), without the Azure SDK.

### Configure

| Variable | Meaning |
|---|---|
| `CLAVIGER_KEK_AZURE_KEY_ID` | `https://<vault>.vault.azure.net/keys/<name>[/<version>]` (or `https://<hsm>.managedhsm.azure.net/keys/...`). Without a version, the current version is resolved at startup and pinned for that process. |
| `CLAVIGER_KEK_AZURE_KEY_ID_OLD` | The previous key during a rotation (see below). |
| `CLAVIGER_KEK_AZURE_AUTH` | `auto` (default), `managed-identity`, `workload-identity` or `client-secret`. |

Authentication, in `auto` order:

1. **Workload identity federation** when `AZURE_FEDERATED_TOKEN_FILE` is set (with `AZURE_CLIENT_ID` and `AZURE_TENANT_ID`). The token file is re-read on every refresh. `AZURE_AUTHORITY_HOST` overrides `login.microsoftonline.com`.
2. **Client secret** when `AZURE_CLIENT_SECRET` is set (with `AZURE_CLIENT_ID` and `AZURE_TENANT_ID`). For development; prefer an identity without a secret in production.
3. **Managed identity** otherwise: the App Service / Container Apps / Functions identity endpoint (`IDENTITY_ENDPOINT` + `IDENTITY_HEADER`) when present, else the instance metadata service (`169.254.169.254`). Set `AZURE_CLIENT_ID` to pick a user-assigned identity; leave it unset for the system-assigned one.

Client certificates are not supported. Tokens are cached and refreshed shortly before they expire. The token scope is derived from the vault host (`https://vault.azure.net/.default`, or `https://managedhsm.azure.net/.default` for a Managed HSM, and the matching sovereign-cloud hosts).

### What to create in Azure

- **Key**: an **RSA** key, 3072 or 4096 bits recommended (2048 works). Claviger uses `RSA-OAEP-256`. The key needs the `wrapKey` and `unwrapKey` operations. A Managed HSM **AES** key is also accepted (`A256KW`). EC keys cannot wrap and are refused.
- **Permission** for the identity Claviger runs as, on the key (least privilege) or the vault:
  - Azure RBAC vault: **Key Vault Crypto User** (it includes reading the key's metadata, which Claviger does once at startup, and wrap/unwrap). On a Managed HSM: **Managed HSM Crypto User**.
  - Access-policy vault: the key permissions `get`, `wrapKey` and `unwrapKey`.
- **Network**: the server must reach `<vault>.vault.azure.net` (HTTPS, 443) and, for the identity, `login.microsoftonline.com` or the metadata endpoint. With the vault firewall on, allow the app's network (a service endpoint or private endpoint) or the "trusted Microsoft services" exception does not apply to your own app. A blocked path looks like a timeout: Claviger treats it as an outage, not as a wrong key.

### Cost and availability

Calls happen only at startup (one read of the key, one unwrap per data key), when a new encrypted field gets its data key, and when you rotate or rewrap. Data keys are unwrapped once and held in memory, so reads and writes do not call Key Vault: an outage while the app is running changes nothing. Plan for a handful of operations per start, far below any Key Vault throttling limit, and negligible cost.

### Failure behaviour

- **Fail closed.** If encrypted fields exist and Key Vault cannot be reached or refuses the request at startup, the app does not start. If no encrypted field exists, no call is made and a Key Vault outage does not matter.
- **`keys check` and the entrypoint pre-flight.** A definite refusal is reported as `keys check: FAILED` and stops the rollout before the handover: bad credentials, missing permission (403), key not found (404), a key that cannot unwrap the stored data keys. An *inconclusive* result (timeout, network error, throttling, 5xx) is reported as `skipped (could not reach the key-encryption key service ...)` with exit 0, so the pre-flight fails open like the rest of the handover and the app itself decides when it starts. Calls are retried a few times with exponential backoff and jitter first.
- Tokens, client secrets, federated assertions and key material are never logged or put in error messages.

### Key ids and rotation in Key Vault

The stored key id is `azkv:<host>/<name>/<version>`. Creating a new version of the key in Key Vault (manual rotation or a rotation policy) is a KEK rotation:

1. Existing data keys keep working: Claviger unwraps each one with the version recorded with it. A running server keeps wrapping with the version it pinned at start.
2. Restart (or deploy). With no version in `CLAVIGER_KEK_AZURE_KEY_ID`, the new version becomes the primary one.
3. Run `claviger keys rewrap` (or **Re-wrap** in the dashboard). Data keys wrapped under older versions are re-wrapped under the new one.
4. Once nothing uses the old version, you may disable it in Key Vault. Do not disable or delete a version while data keys still use it. If you pin a version in the URL, nothing changes until you edit it.

Keep the Key Vault key backed up and enable soft delete and purge protection: deleting the key makes the data unreadable.

### Moving between a local key and Key Vault

Both directions use the same rewrap flow as any KEK rotation:

- Local to Key Vault: set `CLAVIGER_KEK_AZURE_KEY_ID` and move the local key to `CLAVIGER_KEK_OLD` (or `CLAVIGER_KEK_OLD_FROM_APP_KEY=1`), start, `keys rewrap`, then remove the old variable.
- Key Vault to local: set `CLAVIGER_KEK` and put the vault key in `CLAVIGER_KEK_AZURE_KEY_ID_OLD`, start, `keys rewrap`, then remove the old variable.

### Live test

`CLAVIGER_LIVE_KEY_URL=https://<vault>.vault.azure.net/keys/<name> go test -tags azurekv_live -run Live ./tools/keys/azurekv` runs a wrap/unwrap round trip against a real vault with the credentials from the environment. It is not part of CI.

## AWS KMS and Google Cloud KMS

AWS KMS and Google Cloud KMS work like Azure Key Vault: the key-encryption key stays in the service, Claviger only asks it to wrap and unwrap the field data keys, and record data never leaves the server. Both talk to the service's REST API directly with the standard library (SigV4 signing for AWS, OAuth2 for Google), with no cloud SDK, so they add **no Go module dependencies**.

**Build tags.** The sources are compiled in only with `-tags kms_aws` and/or `-tags kms_gcp`. The release binary and image are built **without** them. To use one, build your own:

```sh
go build -tags kms_aws,kms_gcp -o claviger ./examples/base
docker build --build-arg GO_TAGS=kms_aws,kms_gcp -t claviger:kms .
```

If the variable is set in a binary built without the tag, the app **does not start** (and `keys check` reports `FAILED`): the error names the variable and the tag. It never falls back to another key. Exactly one key source may be configured; setting two (for example `CLAVIGER_KEK_AWS_KEY_ID` and `CLAVIGER_KEK_GCP_KEY_ID`, or one of them with `CLAVIGER_KEK`) is an error. Everything under [Failure behaviour](#failure-behaviour) in the Azure section applies unchanged: fail closed at startup when the service refuses, inconclusive results (timeout, network, throttling, 5xx) skip the pre-flight, calls are retried with backoff and jitter, and credentials, tokens and key material are never logged. Cost and availability are the same too: a few calls per start, none while serving.

### AWS KMS

| Variable | Meaning |
|---|---|
| `CLAVIGER_KEK_AWS_KEY_ID` | A symmetric key ARN (preferred), key id, or alias (`alias/name`). It is resolved with `DescribeKey` at startup and the key's **ARN** is what is stored (`awskms:<arn>`), so repointing an alias later cannot silently change the KEK. A key id or alias needs `AWS_REGION` (or `AWS_DEFAULT_REGION`); an ARN carries its region. |
| `CLAVIGER_KEK_AWS_KEY_ID_OLD` | The previous key during a rotation. It is read with the same credentials as the primary. |
| `AWS_ENDPOINT_URL_KMS` | Optional endpoint override (VPC endpoint, emulator). |

Credentials, in order: `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` (/ `AWS_SESSION_TOKEN`); web identity (`AWS_WEB_IDENTITY_TOKEN_FILE` + `AWS_ROLE_ARN`, as set by EKS IRSA; `AWS_ENDPOINT_URL_STS` overrides STS); the shared credentials file (`AWS_SHARED_CREDENTIALS_FILE`, `AWS_PROFILE`; static keys only, role-assumption and SSO profiles are not supported); the ECS task role (`AWS_CONTAINER_CREDENTIALS_*`); the EC2 instance profile (IMDSv2). Temporary credentials are refreshed before they expire.

What to create: a **symmetric** `ENCRYPT_DECRYPT` key (`SYMMETRIC_DEFAULT`; asymmetric and HMAC keys are refused). The identity needs `kms:DescribeKey`, `kms:Encrypt` and `kms:Decrypt` on the key. Claviger binds each wrapped data key to the encryption context `purpose=claviger-dek-wrap`; do not write key policies or grants that require a different context.

Rotation: turning on **automatic key rotation** in KMS keeps the ARN and old ciphertexts stay readable, so nothing needs re-wrapping. To move to a different KMS key (or from or to a local or other cloud key), set the new key as primary and the old one in `CLAVIGER_KEK_AWS_KEY_ID_OLD` (or `CLAVIGER_KEK_OLD`, `CLAVIGER_KEK_AZURE_KEY_ID_OLD`, ...), start, run `claviger keys rewrap`, then remove the old variable. Do not schedule deletion of the old key before the rewrap has finished; KMS key deletion is irreversible and makes the data unreadable.

Runbook: [docs/testing/aws-kms-kek.md](testing/aws-kms-kek.md).

### Google Cloud KMS

| Variable | Meaning |
|---|---|
| `CLAVIGER_KEK_GCP_KEY_ID` | `projects/<project>/locations/<location>/keyRings/<ring>/cryptoKeys/<key>`, a symmetric `ENCRYPT_DECRYPT` key, **without** a version. Cloud KMS decrypts with whichever version produced the ciphertext, so adding or rotating versions needs no re-wrap; the stored key id is `gcpkms:<resource name>`. |
| `CLAVIGER_KEK_GCP_KEY_ID_OLD` | The previous key during a rotation. |

Credentials: the service account key file named by `GOOGLE_APPLICATION_CREDENTIALS` (a signed JWT is exchanged for an access token) when set, else the metadata server (Compute Engine, GKE Workload Identity, Cloud Run, ...; `GCE_METADATA_HOST` overrides the host). Authorized-user files (`gcloud auth application-default login`) and external-account (workload identity federation) files are **not** supported; use an attached service account or a key file. Prefer an attached service account to a key file.

What to create: a symmetric key whose primary version is enabled, and grant the service account `roles/cloudkms.cryptoKeyEncrypterDecrypter` on the key (it includes `cloudkms.cryptoKeys.get`, which Claviger uses once at startup). Wrapped data keys are bound to the key id as additional authenticated data.

Rotation: new key versions are transparent. To move to a different key, set it as primary, the old one in `CLAVIGER_KEK_GCP_KEY_ID_OLD`, start, `claviger keys rewrap`, then remove the old variable. Do not destroy a key version that still protects data keys.

Runbook: [docs/testing/gcp-kms-kek.md](testing/gcp-kms-kek.md).

### Tests

`go test -tags kms_aws,kms_gcp ./tools/keys/...` runs the cloud KMS suites against in-process fakes (CI runs it in the `test` job). The fakes perform real AES-GCM wrapping and the AWS fake verifies SigV4 signatures independently of the client. There is no live-cloud test in CI; use the runbooks.
