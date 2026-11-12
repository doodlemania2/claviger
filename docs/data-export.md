# Personal data export

A person can ask for a copy of the data the system holds about them (GDPR Articles 15 and 20). A superuser can also make the request for them. The result is one ZIP file.

The export is driven by the [data map](data-map.md). It is not a fixed list: every collection on the map that holds personal data is exported, so a new collection only needs a data map entry to be included.

## What is in the ZIP

| File | Content |
|---|---|
| `manifest.json` | When it was made, who it is about, the row count of each collection, the data map version (a hash), collections that were skipped and why, errors, and files that were left out |
| `README.txt` | A short explanation for the person |
| `data/<collection>.json` | The rows about the person, one file per collection |
| `files/<collection>/<record>/<file>` | Files the person uploaded (file fields) |

What counts as "about the person" comes from the `subjectPaths` of the data map entry. All treatments except `none` are exported: `export`, `erase`, `anonymize` and `retain` rows are all the person's data. Collections marked `none` are listed in the manifest as skipped.

## What each built-in collection exports

Built-in collections use a fixed allowlist (`core/data_export_fields.go`). A column that is not on the list is not exported. A test fails if a built-in entry has no allowlist.

| Collection | Exported | Never exported |
|---|---|---|
| auth record (`users`, `_superusers`, any auth collection the person is in) | the whole profile | password hash, token key |
| `_externalAuths` | provider, provider id | |
| `_webauthnCredentials` (passkeys) | name, created, last used | credential id, public key, transport, flags |
| `_totps`, `_mfas`, `_otps` | existence and dates | TOTP secret, backup codes, one-time password hash, address |
| `_sessions` | device label, IP, auth method, times, revoked | |
| `_authOrigins` | existence and dates | device fingerprint |
| `_apiKeys` | name, prefix, scopes, expiry, last used | secret hash |
| `_bans` | reason, until, created | who created it |
| `_orgMembers` | organization, role, status, tags, source, external id | |
| `_invitations` | the person's own (by address or accepted by them) | token hash. Invitations the person sent to others are listed without the invited address |
| `_oauthConsents`, `_oauthTokens`, `_oauthRefreshTokens` | client, scopes, expiry, state | token hashes, token ids |
| `_oauthRequests`, `_oauthSso`, `_oauthDeviceCodes` | client, state, expiry | code, binding and session hashes, request data |
| `_auditLog` | entries where the person is the actor or the record. Entries about somebody else's record keep action, time, target and IP, but not the before and after values | |
| `_webhookDeliveries` | event type, status, attempts, and the payload only when it describes a record about the person | error text, attempt log |
| `_dataRequests` | the person's requests, state and dates | the export file |
| collections with treatment `none` (`_orgs`, `_scimTokens`, `_fieldKeys`, `_signingKeys`, ...) | nothing | everything |

Request logs (`_logs`) are not part of the export. They are removed by the log retention setting (`logs.maxDays`). The manifest says so.

### Collections an administrator registers

For these the export has no allowlist. It writes every field except password fields, the auth `tokenKey`, and fields whose name looks like a secret (`password`, `secret`, `token`, `api key`, `private key`, `credential`). Only the person's own rows are exported. If a row mixes in other people's data, split the collection or leave it off the map and handle requests for it by hand.

### Encrypted fields

`encryptedText` and `encryptedJSON` fields are exported **decrypted**. It is the person's own data, and the spec (10.10) says exports decrypt in the app. The key never leaves the server. The ZIP is only readable through the download route below.

### Other people's data

The export covers the person's own rows only. Nothing is added for households yet: when household collections exist they get a data map entry and the export includes the person's own row.

### Files

Files of file fields are included, up to 100 MB for one file and 1 GB for all files. A file over the limit is listed in `manifest.json` under `files.skipped` and the request ends as `partial`. Set the limits in the admin UI under **Settings > Data export** (`Settings.dataExport.maxFileMB` and `maxTotalMB`). See [Limits](#limits).

## Requesting an export

| Who | Route |
|---|---|
| A signed-in user, for themselves | `POST /api/me/export` |
| A superuser, for anyone | `POST /api/gdpr/subjects/{id}/export` (optional body `{"collection": "users"}` when the id could be in several auth collections) |

Self-service needs proof that the person is present. If the account has a passkey, a fresh passkey assertion is needed (the same step-up as for API keys; the response is a 403 with code `passkey_step_up_required`). Otherwise the body must carry the account password: `{"password": "..."}`. An account with neither a passkey nor a usable password (for example sign-in by an OAuth provider only) must ask an administrator. Self-service is limited to 3 requests per hour. API keys cannot use any of these routes.

A person has one export at a time. A second request while one is queued or running gets a 409.

The response is `202` with the request:

```json
{"id": "...", "state": "queued", "dueAt": "...", "selfService": true, ...}
```

`dueAt` is 30 days after the request (the GDPR period is one month and can be extended; the CCPA allows 45 days).

## Status and download

| Route | |
|---|---|
| `GET /api/gdpr/exports/{id}` | State, counts, whether it can be downloaded. Allowed for the requester and for superusers |
| `GET /api/me/exports` | The person's own requests |
| `GET /api/gdpr/exports` | Superuser: all requests (`?subject=<id>`) |
| `GET /api/gdpr/exports/{id}/download` | The ZIP |
| `DELETE /api/gdpr/exports/{id}` | Cancel a queued request, or throw away a finished file |

States: `queued`, `running`, `done`, `partial` (something was left out, see the manifest), `failed`, `cancelled`, `expired`.

The download works with the session (`Authorization` header) or with a file token (`?token=`, from `pb.files.getToken()`), so a plain link works in a browser. The link works **once**: after the ZIP was sent in full, the file is deleted. A download that breaks half way does not use it up. A finished export expires after 7 days by default (`dataExport.retentionDays`, see [Limits](#limits)); an hourly job deletes the file and marks the request `expired`.

For an export a superuser made for somebody else, the superuser downloads it and passes it on. The subject cannot fetch it through the API.

## Where the file lives

The ZIP is the hidden, protected `file` field of the request row in `_dataRequests`, so it uses the app file storage (local `pb_data/storage` or S3). Local backups skip that folder: a restored backup never brings back an expired export.

## Audit and webhook

Entries on the `_dataRequests` target, with ids and counts only:

| Action | When |
|---|---|
| `gdpr_export_request` | A request was made (actor: the requester, IP) |
| `gdpr_export` | The job ended. `meta` has the status (`ok`, `partial`, `failed`), the subject, the row count per collection, the file count and size |
| `gdpr_export_download` | The file was downloaded |

Changes to `_dataRequests` rows are not audited a second time.

A webhook endpoint with the filter `*`, `_dataRequests:*` or `_dataRequests:gdpr_export` receives `gdpr.export` when an export finishes `ok` or `partial`. The payload carries the same `meta` as `data.record`: the request id, the subject ids and the counts. Never content.

## Not built

- A notification email when the export is ready (the status route and the audit entry tell the requester; an email needs translated templates).
- Pastoral notes (#86) and households (#80): they get a data map entry when their collections exist.
- Erasure (88-4). The `erase` request type and the erasure states exist in `_dataRequests` so the schema does not change later.
- Several replicas: the job is one at a time per process. With SQLite there is only one process. With several replicas on Postgres the claim of a queued request is a compare-and-swap, so two replicas do not run the same request.

## For developers

Adding a system collection: add its data map entry (see [data-map.md](data-map.md)) **and** a line to `builtinDataExport` in `core/data_export_fields.go` that names the columns that may leave the system. `TestDataExportBuiltinAllowlistIsComplete` fails until you do. `TestDataExportEveryBuiltinCollectionAppearsAndNoSecretsLeak` seeds a row in every built-in collection and scans the ZIP for secret columns that are named in the test itself, not read from the allowlist.

## Limits

The limits live in the app settings (`dataExport`, admin UI **Settings > Data export**, or `PATCH /api/settings`). A value of `0` means the default.

| Setting | Default | Allowed | Meaning |
|---|---|---|---|
| `maxFileMB` | 100 | 0 - 1024 (1 GB) | largest single file put in the ZIP |
| `maxTotalMB` | 1024 | 0 - 10240 (10 GB) | total size of all files in the ZIP |
| `retentionDays` | 7 | 0 - 30 | how long a finished export can be downloaded |
| `selfRequestsPerHour` | 3 | 0 - 1000 | exports one person can request for themselves per hour |
| `adminRequestsPerHour` | 60 | 0 - 10000 | exports one superuser can request per hour |

The upper limits of the size and retention settings (1 GB, 10 GB, 30 days) apply to the setting only; a deprecated environment variable below is not capped, so an upgrade keeps its value.

A changed rate limit takes effect at once (the counters start over). The other limits apply to the next job.

### Deprecated environment variables

`CLAVIGER_EXPORT_MAX_FILE_MB`, `CLAVIGER_EXPORT_MAX_TOTAL_MB` and `CLAVIGER_EXPORT_RETENTION_DAYS` still work so an upgrade does not change a running deployment. **Precedence: a valid (positive whole number) environment variable wins over the setting; otherwise the setting; otherwise the default.** The first time one is used, a WARN is logged (once per variable and process) that it is deprecated in favour of the setting. Unset the variable to manage the limit from the admin UI.
