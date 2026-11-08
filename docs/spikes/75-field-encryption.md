# Spike: field-level encryption (#75)

Status: research only. No Go code is committed with this doc.
Branch: `spike/75-field-encryption`. Related: #78 (backup encryption half), #86 (confidentiality levels), #88 (GDPR tools).

## Recommendation

Add two new fork-only field types, `encryptedText` and `encryptedJSON`, instead of an `encrypted: true` option on upstream's `text` and `json` fields. Back them with an envelope keyring: one random data key (DEK) per field, wrapped by a key-encryption key (KEK) from a pluggable key source. Ship env and file key sources first. Add Azure Key Vault over plain REST (no SDK). Put AWS and GCP behind a build tag or an exec helper.

Encrypted fields are not filterable, sortable or indexable. The records API returns plaintext. `data.db` and backups hold ciphertext only.

## What exists today

| Piece | Where | Notes |
|---|---|---|
| App secret | `--encryptionEnv`, entrypoint passes `PB_ENCRYPTION_KEY` | A 32-character string used directly as the AES key. No KDF, no key id, no AAD. |
| Helper | `tools/security/encrypt.go` | `Encrypt` / `Decrypt`, AES-256-GCM, base64 of `nonce||ciphertext`. |
| Settings | `core/settings_model.go`, `core/settings_query.go` | The whole settings JSON is encrypted. Upstream's own comment says it "doesn't really accomplish much". |
| TOTP secret | `core/totp_model.go` | `enc:<base64>` prefix. Plaintext is still accepted when no key is set. |
| Webhook secret | `core/webhooks.go` | Same `enc:` pattern. Also hidden in `OnRecordEnrich`. |
| Audit redaction | `core/audit_model.go` line 441 | Redacts by `Hidden`, `PasswordField`, or a name regex (`password`, `secret`, `token`, `api key` and so on). |
| Field plumbing | `core/field.go`, `core/record_model.go` | `PrepareValue` runs on DB scan and on user input. `DriverValuer.DriverValue` runs on save. |

The existing `enc:` uses have no key id, so they cannot be rotated without a flag day. The new keyring should leave them alone at first and take them over later (see M6).

## Prototype

Scratch only, outside the repo (`.../scratchpad/proto`, a copy of this tree). About 130 lines in one new file, `core/field_encrypted_text.go`, plus a test harness. It embeds upstream's `TextField`:

```go
type EncryptedTextField struct{ TextField }

func (f *EncryptedTextField) Type() string { return "encryptedText" }

// AAD binds the ciphertext to collection, field and record id.
func (f *EncryptedTextField) aad(r *Record) []byte {
    return []byte(r.Collection().Id + "|" + f.Id + "|" + r.Id)
}

func (f *EncryptedTextField) PrepareValue(r *Record, raw any) (any, error) {
    s, _ := f.TextField.PrepareValue(r, raw)
    if !strings.HasPrefix(s.(string), "enc:v1:") { return s, nil } // plaintext / legacy
    plain, err := keyring.open(s.(string), f.aad(r))
    return string(plain), err
}

func (f *EncryptedTextField) DriverValue(r *Record) (driver.Value, error) {
    v, _ := r.GetRaw(f.Name).(string)
    if v == "" { return "", nil }
    return keyring.seal([]byte(v), f.aad(r))      // enc:v1:<keyId>:<b64(nonce||ct)>
}
```

Because the type embeds `TextField`, validation, the column type (`TEXT DEFAULT '' NOT NULL`), min/max/pattern and the setters are inherited. Everything above the DB layer (API, hooks, realtime, jsvm) sees plaintext. Nothing upstream-owned was edited. Both hooks are existing extension points: `DriverValuer` and `PrepareValue`.

### Round trip through the records API

```
POST   {"name":"Ann","phone":"+1 555 0100","plainPhone":"+1 555 0100"}
  -> 200 {"name":"Ann","phone":"+1 555 0100", ...}
data.db row:
  phone      = enc:v1:k1:WvLNKuUQO779bWyOVBiv9FwUicyrSDqEfTlfpsgxc/cBLelfHlW0
  plainPhone = +1 555 0100
GET    -> 200 phone "+1 555 0100"
```

### Filter, sort, index, search

The query layer compares the raw column, so ciphertext is what gets compared.

| Operation | Result on the encrypted field |
|---|---|
| `filter=phone="+1 555 0100"` | 0 rows. The control column returns 1. |
| `filter=phone~"555"` | 0 rows. The control column returns 2. |
| `sort=phone` / `sort=-phone` | Works, but orders by random ciphertext. It looks plausible and is meaningless. |
| Unique index on `phone` | Creates without error and never fires. A duplicate phone saved with HTTP 200. |
| API rules that mention the field | Compile, but never match. |
| Full text / `search` | Same as `~`: no match. |

Silent wrong answers are the danger. Rule: the field's `ValidateSettings(ctx, app, collection)` receives the collection, so it can reject indexes and API rules that mention the field. That needs no upstream edit. Filters sent by clients cannot be rejected without touching the resolver. Document that they return nothing.

### Overhead

Apple M4 Max, in-process, SQLite, 64-byte value, `go test -bench`, 3 runs each. The numbers exclude HTTP and JSON.

| Benchmark | Plain `text` | `encryptedText` | Delta |
|---|---|---|---|
| Save one record | 120-122 µs | 122-132 µs | about +6 µs (+5%, partly noise) |
| Read 1000 records | 737-759 µs | 983-1001 µs | about +0.25 µs per record |

Cost is small next to the SQLite work and the JSON encoding. AES-GCM uses hardware instructions on arm64 and amd64. The key must be a `cipher.AEAD` built once, not per call.

### Findings from the prototype

1. **AAD with the record id is mandatory.** `PrepareValue` also runs on user input. A client can paste another record's ciphertext into its own field. With the record id in the AAD, the paste fails to decrypt and is stored as a literal string. Without it, the API would be a decryption oracle for any ciphertext the client can see (backups, SQL console, logs). Needs a regression test.
2. **Reserved prefix.** A user value starting with `enc:v1:` is ambiguous. `Set` swallows the `PrepareValue` error. Reject the prefix in `ValidateValue`.
3. **Every update re-seals.** Random nonces mean the exported value always differs, so the "skip unchanged columns" logic in `dbExport` rewrites the column on every save. It also burns the nonce budget (random 96-bit nonces, keep each DEK under about 2^32 seals). Fix: stash the loaded ciphertext in `record.Store()` and reuse it when the plaintext is unchanged.
4. **A wrong or missing key fails the whole list request.** With the wrong key, `GET /records` returned HTTP 400 with a generic message. The error should name the field and the key id in the server log.
5. **Empty stays empty.** Storing `""` as `""` leaks which rows are empty. Accepted, and documented.

## Options compared

| | A. New field types (recommended) | B. `encrypted: true` option on `text`/`json` | C. Hook-based (OnRecord* encrypt/decrypt) |
|---|---|---|---|
| Matches the wording of #75 | No, but equivalent for users | Yes | No |
| Upstream files changed | None | `field_text.go`, `field_json.go`, UI settings | None |
| Rebase risk | Very low | Medium. Upstream edits these files and the UI settings. | Low |
| Column type control | Own (`TEXT`, even for JSON) | JSON column holds a non-JSON string | n/a |
| Rules/index validation | Local to the field | Needs edits in shared code | Hard, no field hook |
| Converting an existing field | Type change plus `keys encrypt` command | Flip a flag plus the same command | Same |
| Query layer treats it as text/JSON | Not as JSON, so no `json_extract` on ciphertext | Yes, which is wrong for ciphertext | Yes |
| Hidden bugs | Few | Several code paths branch on `field.Type() == FieldTypeJSON` (see `record_field_resolver_runner.go` line 836) | Records seen by other hooks in mixed state |

Option A wins. Option B can still be offered later as UI sugar over A.

## Proposed architecture

### Envelope keys

- Each encrypted field gets a random 256-bit DEK, created lazily on first save.
- The DEK is wrapped by the KEK and stored in a plain table `_encKeys` in `data.db`. It is not a collection, so no API exposes it. Columns: `id`, `collectionId`, `fieldId`, `kekId`, `wrapped`, `canary`, `createdAt`, `retiredAt`.
- `canary` is a known string sealed with the DEK. On startup it proves that the KEK is correct before any user data is touched.
- Ciphertext format: `enc:v1:<dekId>:<b64url(nonce||ct)>`. AAD is `collectionId|fieldId|recordId`.
- DEKs are unwrapped once at startup and held as ready `cipher.AEAD` values. After that, a KMS outage does not affect running reads or writes.
- Why stored DEKs and not `HKDF(KEK, field)`: cloud KMS never releases the KEK, only wrap and unwrap. Stored wrapped DEKs work with every source. The HKDF shortcut (used in the prototype) works only for env and file.

### Two rotations

| Rotation | What changes | Cost | Command |
|---|---|---|---|
| KEK | Rewrap each DEK with the new KEK | Seconds, no data rewrite | `keys rotate --kek` |
| DEK | New DEK per field, re-encrypt rows | Proportional to data | `keys rotate --field people.phone` |

Re-encryption is resumable without a cursor. The work set is "rows whose value does not start with `enc:v1:<currentDekId>:`". Process in batches of 500 inside one transaction, ordered by rowid, then repeat until the set is empty. The same query finds legacy plaintext rows, so the command also does the initial encryption of an existing column. The command uses raw SQL, so it fires no hooks, writes no audit entries and does not bump `updated`. It must read the record id to build the AAD. It runs while the server is stopped, or takes the same single-writer lock the handover uses.

### Key sources

```go
type KeySource interface {
    Name() string
    Wrap(ctx context.Context, dek []byte) (kekId string, wrapped []byte, err error)
    Unwrap(ctx context.Context, kekId string, wrapped []byte) ([]byte, error)
}
```

`env` and `file` hold a raw KEK and wrap locally with AES-GCM. Cloud sources call the service.

### Relation to `PB_ENCRYPTION_KEY`

Keep the existing uses (settings, TOTP, webhooks) exactly as they are. The new `env` source defaults to `PB_ENCRYPTION_KEY` when no dedicated variable is set, with a domain-separated derivation: `KEK = HKDF-SHA256(PB_ENCRYPTION_KEY, info="claviger/kek/v1")`. That way the same string is never used directly as two different keys, and existing deployments need one fewer secret. Downside: one secret guards everything and rotating it means rotating settings too. Offer `CLAVIGER_KEK` (base64, 32 bytes) as the recommended dedicated variable.

### Unavailable KEK at startup

| Mode | Behaviour | Pros | Cons |
|---|---|---|---|
| Fail closed (recommended default) | `serve` exits with a clear error naming the source and key id | Nothing silently written in the clear or unreadable | Outage if the key is missing |
| Degraded | Start; reads of encrypted fields return an error, writes to them are rejected | Rest of the app stays up | Partial outage is hard to notice; list endpoints on mixed collections break |

Recommendation: fail closed, with retry and backoff for cloud sources for a bounded time (default 60 s) before exiting. Once started, DEKs live in memory, so a later KMS outage does not hurt.

**Rollout risk to handle.** `entrypoint.sh` implements the single-writer handover: the incoming replica writes `.pb_handover` and the outgoing one releases `/pb_data`. If the incoming replica then fails closed on a bad KEK, the outgoing one has already released the data. Add a `claviger keys check` pre-flight that runs before the handover file is written. The check must be fail-open on infrastructure errors, in line with the existing rule that the wait stays fail-open, and fail only on "KEK definitely wrong".

### Cloud KMS without heavy SDKs

Measured in a scratch module (Go 1.27, `go list -deps`, `go build`, blank imports only, so real use links more):

| SDK | Modules | Packages | Binary growth |
|---|---|---|---|
| Azure (`azidentity` + `azkeys`) | 15 | 281 | about 7.6 MB |
| AWS (`aws-sdk-go-v2/config` + `service/kms`) | 16 | 304 | about 6.3 MB |
| GCP (`cloud.google.com/go/kms/apiv1`) | 34 | 486 | about 22.6 MB (gRPC, protobuf) |
| Claviger today | 156 modules in the build list | 667 | 58 MB (with embedded UI) |

All three would be a large share of the default build. Options:

| Option | Default build | Effort | Notes |
|---|---|---|---|
| Direct REST + managed identity / env credentials, no SDK | Unchanged | Azure small (Key Vault wrap/unwrap is two POSTs; token from IMDS or client secret). AWS medium (SigV4 signing, about 150 lines). GCP small (metadata-server token + REST). | Recommended for Azure, because the reference deployment is Azure Container Apps. |
| Build tags (`-tags kms_aws`) in the main module | Unchanged | Small | Fork-only files, so low rebase risk. Needs separate image variants. |
| Separate Go modules (`claviger-kms-aws`) registered via `core.RegisterKeySource` | Unchanged | Medium | Users build their own binary. Fits a public MIT project. |
| Exec helper (`--keySourceCmd`, like git credential helpers) | Unchanged | Small | Works for any vault. Weakest isolation. |

Recommendation: Azure via REST in the main module; AWS and GCP as build-tagged files using the SDKs, shipped in a second image tag, until demand justifies more. The exec helper is a cheap escape hatch for everything else.

## Interactions

| Area | Result | Action |
|---|---|---|
| Audit log | `audit_model.go` redacts by `Hidden`, password type, or name regex. An encrypted field named `phone` or `note` is **not** redacted today, so its plaintext would land in `_auditLog` `before/after`. | Add `|| isEncryptedField(f)` to the `redact` expression. One line in a fork-owned file. Add a test. |
| Webhooks | Payloads are built from the audit entry, so they inherit the redaction. | Covered by the same fix. Test it. |
| Request logs | The logger does not store request bodies. OTLP export tees the same records. | No change. Confirm in a test. |
| Backups | The zip holds `data.db` with ciphertext and the wrapped DEKs. Without the KEK, a restore is unreadable. | **Key escrow:** document an offline KEK copy; `keys export-kek` prints a recovery string once. Restore verification (#109) should also decrypt the canary when the keyring is available. |
| SQL console / raw DB access | Shows ciphertext. The AAD stops row swapping. | No change. |
| Realtime | Delivers the in-memory record, so subscribers allowed by the API rule get plaintext. Correct. | No change. |
| jsvm hooks | Hooks see plaintext. `$app.db()` raw queries see ciphertext. | Document. |
| Exports | There is no CSV export. The schema export has no values. | Nothing yet. A future "export person" (#88) goes through records and gets plaintext. |
| Admin UI | New field type needs a settings panel, input, view and an icon. A text-like input is enough. The filter helper should not offer the field. | New dir `ui/src/fields/encryptedText/` (about 330 lines copied from `text/`). Rebuild `ui/dist`. |
| Superusers | Superusers read plaintext through the API like any reader. | See #86 below. |

### How the dependent issues use it

- **#78 backup encryption.** Separate concern: encrypt the whole archive stream. Use the same key source, but a distinct purpose key (`HKDF(KEK, "claviger/backup/v1")`) and a chunked AES-GCM stream format (so large archives need no buffering). `age` can come later for recipient keys. Restore must decrypt before extraction (`core/backup_restore.go`), and the backup name check (`backupNameRegex` in `apis/backup_create.go` only accepts `*.zip`) and the backup list must accept the encrypted extension. Verify both before sizing M5 firmly.
- **#86 sealed notes.** At-rest encryption does not make a note "unreadable to admins". The server decrypts for any permitted reader. That needs an access policy: hide the field from everyone but the owner unless a break-glass grant exists, like the webhook secret's `Hide` in `OnRecordEnrich`. The crypto in this spike is necessary, not sufficient. Owner-only keys that the server cannot use (passkey-derived) would be a different design.
- **#88 GDPR.** Subject access export goes through the API, so it gets plaintext. Encrypted fields cannot be searched, so "find everything about a person" must key on an unencrypted id or relation. Crypto-shredding (per-subject keys) does not give real erasure while old backups hold the wrapped keys, so erasure should be a normal delete plus a backup retention rule.

## Files and merge risk

| File | New / touched | Owner | Merge risk |
|---|---|---|---|
| `core/keyring.go`, `core/keysource.go`, `core/keysource_env.go`, `core/keysource_file.go` | new | fork | none |
| `core/field_encrypted_text.go`, `core/field_encrypted_json.go` | new | fork | none |
| `migrations/<ts>_add_enc_keys.go` | new | fork | none |
| `cmd/keys.go` (`keys check`, `keys rotate`, `keys encrypt`, `keys export-kek`) | new | fork | none |
| `ui/src/fields/encryptedText/*`, `encryptedJSON/*` | new | fork | low (registered by the same `init.js` pattern) |
| `ui/dist` | rebuilt | fork | known cost on every UI change |
| `core/audit_model.go` | one line | fork (#74) | none |
| `core/base.go` (init keyring in bootstrap) | small edit | **upstream** | low |
| `pocketbase.go` (new global flags: key source, failure mode) | small edit | **upstream** | low |
| `core/backup_create.go`, `core/backup_restore.go`, `apis/backup_create.go` (M5) | edit | **upstream** | medium |
| `core/settings_model.go`, `core/settings_query.go` (M6, settings refs) | edit | **upstream** | medium |
| `tools/search/filter.go`, `core/record_field_resolver*.go` (only if deterministic mode ships) | edit | **upstream** | high |

## Milestones

| | Scope | Size |
|---|---|---|
| M1 | Keyring, `_encKeys`, env + file KEK, `encryptedText`, validation of rules/indexes, audit redaction, reuse of ciphertext on unchanged saves, `keys check`, admin UI type, docs | 6-8 days |
| M2 | `encryptedJSON` (TEXT column, size limits, UI editor) | 2-3 days |
| M3 | `keys rotate --kek`, `keys rotate --field`, `keys encrypt` (convert existing column), resumable batches, tests with a killed run | 4-5 days |
| M4 | Azure Key Vault via REST + managed identity (3 days); AWS and GCP behind build tags (3 days each) | 3-9 days |
| M5 | Backup stream encryption, restore + list support, canary check in restore verification (#78) | 5-6 days |
| M6 (optional) | Settings secrets as references (`env:`, `file:`, `kms:`) for SMTP, S3, OAuth2 secrets; move TOTP and webhook secrets onto the keyring | 4-5 days |

M1 alone meets "round-trips through the API and is unreadable in `data.db` and backups". M3 meets "survives a key rotation".

## Risks

- Silent non-matching filters and sorts. Mitigation: reject indexes and rules at schema validation, document filters, hide the field from UI filter pickers.
- Losing the KEK loses the data. Mitigation: escrow command, docs, backup canary.
- Fail-closed startup interacting with the Azure rollout handover (see above).
- Nonce reuse bound under heavy writes. Mitigation: reuse ciphertext on unchanged saves, DEK rotation threshold, and a counter warning.
- Upstream changes to `newRecordFromNullStringMap` or `dbExport` would break the hook points. Both are stable today, and the tests catch it.
- `PrepareValue` doing crypto on user input: ambiguity of the `enc:v1:` prefix. Mitigation: reserved prefix plus AAD.
- Metadata still leaks: length of values, emptiness, and write timing.
- The raw-SQL rotation skips hooks and audit by design, so it must log its own audit event.

## Open questions for the owner

1. **New field types or an option on `text`/`json`?** Options: (a) new types; (b) option. Recommendation: (a), explained above. Users pick "Encrypted text" in the type list. A field cannot be flipped in place; `keys encrypt` converts existing data.
2. **Fail closed or degraded when the KEK is missing at startup?** Options: closed; degraded. Recommendation: closed, with a pre-flight in `entrypoint.sh`.
3. **Ship deterministic / exact-match mode?** Options: no; a separate blind-index column (HMAC) with a filter rewrite; deterministic AES-SIV in the column. Recommendation: not in v1. It needs edits in upstream search code (high merge risk) and weakens confidentiality. Revisit after M3 if a real case needs lookup by phone or national id.
4. **Reuse `PB_ENCRYPTION_KEY` as the default KEK source?** Options: yes, via HKDF; no, require `CLAVIGER_KEK`. Recommendation: yes as a fallback, with `CLAVIGER_KEK` documented as the proper setting.
5. **Which cloud KMS first?** Options: Azure only; all three. Recommendation: Azure via REST first (matches the reference deployment); AWS and GCP as build-tagged variants on demand.
6. **Per-field DEK or finer?** Options: per field (as the issue says); per record or per subject. Recommendation: per field. Per-subject keys complicate everything and do not deliver erasure while backups exist.
7. **Is "sealed notes readable only by the owner" (#86) meant as access control or true server-blind encryption?** Options: access policy plus break-glass on top of this work; client-side keys. Recommendation: access policy, built in the #86 issue itself.
8. **Backup encryption format (#78).** Options: chunked AES-GCM with the keyring; `age`. Recommendation: chunked AES-GCM first, `age` recipients later if wanted.
9. **Are settings secret references (M6) wanted in the first release?** Recommendation: defer. The issue lists them, but they touch upstream-owned settings code and add little over M1.
