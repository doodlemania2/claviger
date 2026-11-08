# Field encryption

Claviger adds the field type **Encrypted text** (`encryptedText`). Its values are stored encrypted in `data.db` (and so in backups and Litestream replicas) and are plaintext everywhere above the database: the records API, hooks, realtime and the admin UI.

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
   | the app encryption key (`PB_ENCRYPTION_KEY`, or whatever `--encryptionEnv` names) | Fallback. The key is derived with HKDF-SHA256 and the fixed info string `claviger/kek/v1`, so the same string is never used as two different keys. One secret then guards the settings and the fields, and rotating it means rotating both. |

3. Add the field in the collection editor (type **Encrypted text**) or through the API with `"type": "encryptedText"`. Saving a collection that has such a field without a key configured fails with an error naming the variables.

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
- Converting an existing `text` field to Encrypted text is not automated yet. Values already in the column are read as plaintext and are encrypted the next time their record is saved. A bulk conversion command and key rotation are planned.

## Audit log and webhooks

The audit log redacts every `encryptedText` field **by type**, whatever its name (other fields are still redacted by the existing name and `hidden` rules). Webhook payloads are built from the audit entry, so they inherit this. Request logs do not store bodies.

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

## Losing the key loses the data

The KEK is the only way to read the encrypted values. Backups contain `data.db` with the ciphertext **and** the wrapped DEKs, which is useless without the KEK.

- Store the KEK in at least one place that is not the server and not the backup storage (a password manager, a sealed envelope held by two people).
- Test a restore together with the key before you rely on it.
- Changing `CLAVIGER_KEK`, or `PB_ENCRYPTION_KEY` when it is the source, makes the existing data unreadable and the app will not start. There is no rotation command yet; do not change the key of a database that holds encrypted fields. To move to a dedicated `CLAVIGER_KEK` from the derived key, wait for the rotation command.

## Deployment notes

- Single replica / handover: nothing to change besides providing the variable to the container and the pre-flight step above. Provide the same variable to every command that opens the database (`superuser`, `migrate`, ...), since they bootstrap the app and fail closed too.
- `ui/dist` contains the field type; there is nothing to configure in the dashboard.
- Metadata is not hidden: the length of values, whether a value is empty, and when a row was written.

## Roadmap

`encryptedJSON`, key rotation (KEK rewrap and DEK re-encryption), bulk conversion of an existing column, and cloud key sources (Azure Key Vault first) are tracked in #75. The `keyId` in every ciphertext and the `retired` column of `_fieldKeys` exist so that rotation can be added without rewriting existing data first.
