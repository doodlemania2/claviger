# Backup encryption

Claviger can encrypt every backup it writes. An encrypted backup is the usual zip archive, run through a chunked AES-256-GCM stream, stored as `<name>.zip.enc`. It is **off by default**, and when it is off nothing about backups changes: names, archives, restore and verification are exactly as before.

Use it when backups leave the server (S3, a downloaded copy, a cloud snapshot of the volume) and you do not want the database, the uploaded files and the settings (SMTP and S3 passwords) readable by whoever holds the file. It uses the same key-encryption key (KEK) as [field encryption](field-encryption.md), so one key setup covers both.

## Turn it on

1. Configure a key-encryption key, if you do not have one yet. See [Set up](field-encryption.md#set-up) in the field encryption guide: `CLAVIGER_KEK`, `CLAVIGER_KEK_FILE`, `CLAVIGER_KEK_AZURE_KEY_ID`, or the `PB_ENCRYPTION_KEY` fallback.
2. **Store a copy of the key away from the server and away from the backups.** See [Key escrow](#key-escrow).
3. In the dashboard open **Settings > Backups** and switch on **Encrypt new backups**, or set `backups.encrypt` to `true` with `PATCH /api/settings`.

Saving the setting checks, right then, that the key can wrap and unwrap a key (for Azure Key Vault this also proves the permissions). If it cannot, the save is refused with the reason, instead of the first nightly backup failing.

From then on:

| Backup | Stored as |
|---|---|
| Manual, name left empty | `pb_backup_<app>_<timestamp>.zip.enc` |
| Manual, name `church.zip` | `church.zip.enc` (`.enc` is appended; the name is checked against the stored name, so `church.zip` and `church.zip.enc` cannot both exist) |
| Auto (cron) | `@auto_pb_backup_<app>_<timestamp>.zip.enc` |
| S3 | the same names, uploaded already encrypted |

A name that already ends in `.zip.enc` is always encrypted, whatever the setting says. The API and the dashboard accept both `*.zip` and `*.zip.enc` names.

The archive is encrypted while it is written: the plaintext zip never touches the disk, and nothing is held in memory beyond one 64 KiB chunk. If the key is missing or broken at backup time, the backup fails with a clear error and leaves no file behind (a cron failure alerts the superusers as usual). It never falls back to a plain backup.

### Automatic restore verification

Restore verification (the check that runs after each auto backup) decrypts the archive into a temporary directory, authenticates every chunk, and then verifies it like a plain one. The result is marked `encrypted` and shows in the list. A backup that cannot be opened with the current key fails its verification with the reason and alerts the superusers.

### Existing plain backups

Plain backups stay in place and stay restorable, with the setting on or off and with or without a key. Whether a stored file is encrypted is decided by its first bytes, not by its name or by the current setting.

## Restoring

Restore and verify work the same on an encrypted backup: pick it in the dashboard, or `POST /api/backups/{key}/restore`. The backup is decrypted into the temp directory first and every chunk is authenticated. **`pb_data` is not touched until the whole archive has been decrypted and extracted**, so:

- a missing key, a wrong key, a key that was rotated away, a truncated file or any altered byte stops the restore with an error and leaves `pb_data` exactly as it was;
- the error names the key id the backup needs.

Restoring an encrypted backup needs the key it was made with (see [Rotation](#key-rotation)).

## Downloading and offline decryption

The dashboard's download button returns the **encrypted file**, as stored. Keep it as is, or decrypt it where you hold the key:

```sh
claviger backups decrypt pb_backup_church_20261108000000.zip.enc            # writes ...zip
claviger backups decrypt church.zip.enc --out /safe/place/church.zip
```

The command needs no running server and does not touch `pb_data`. It reads the key from the same environment as the server (`CLAVIGER_KEK`, `CLAVIGER_KEK_FILE`, `CLAVIGER_KEK_AZURE_KEY_ID`, or the app encryption key variable), plus the `*_OLD` variables for a backup made with an earlier key. The archive is written to a temporary file and moved into place only after every chunk was authenticated, so a wrong key or a damaged file leaves nothing behind. `--force` overwrites an existing output file. Without `--out`, the output is the input name without `.enc`.

## Key escrow

**The key is the only way to read an encrypted backup. If you lose it, you lose every encrypted backup, and Claviger cannot recover them.**

- Keep a copy of the KEK in at least one place that is not the server and not the backup storage: a password manager, a sealed envelope held by two people. Backups that sit next to the key protect nothing.
- When you use Azure Key Vault, a backup is only as durable as the Key Vault key: enable soft delete and purge protection, and see its notes in [Azure Key Vault](field-encryption.md#azure-key-vault).
- Test your escrow: once, take an encrypted backup to a clean machine, set only the escrowed key, and run `claviger backups decrypt`.
- If you also use field encryption, the same KEK guards the data keys inside `data.db`. One escrowed key covers both.

## Key rotation

Every encrypted backup records the id of the KEK that wrapped its key, and it is never rewritten. After you rotate the KEK (see [Rotating the key-encryption key](field-encryption.md#rotating-the-key-encryption-key)):

- **new** backups are written under the new KEK;
- **old** backups keep their old key id and stay readable **as long as the old key is provided**: set `CLAVIGER_KEK_OLD`, `CLAVIGER_KEK_FILE_OLD`, `CLAVIGER_KEK_OLD_FROM_APP_KEY=1` or `CLAVIGER_KEK_AZURE_KEY_ID_OLD` next to the new key, for the server and for `claviger backups decrypt`;
- `keys rewrap` re-wraps the field data keys in the database. It does **not** touch backup files.

So step 4 of the rotation ("remove the `*_OLD` variable") makes the old backups unreadable on that server. Before you do that, either keep the old key in escrow for as long as you keep the old backups (the retention you set with `cronMaxKeep` decides that), or take a fresh encrypted backup under the new key and delete the old ones. A restore that needs a missing key says so and names the key id.

## File format

Version 1. All integers are big-endian. A file is a header followed by chunks until the end of the file.

```
header
  0   8   magic            "CLAVBAK\n"  (43 4c 41 56 42 41 4b 0a)
  8   1   version          1
  9   1   cipher           1 = AES-256-GCM
  10  4   chunkSize        plaintext bytes in every chunk but the last (65536; accepted: 1024 to 16777216)
  14  2   kekIdLen        1 to 256
  16  n   kekId            UTF-8 id of the KEK that wrapped the key (for example "k0123456789abcdef")
      2   wrappedLen       1 to 8192
      m   wrappedDEK       the data key, wrapped by the KEK (format below)
      12  fileNonce        random

chunk i = AES-256-GCM-Seal(streamKey, nonce_i, plaintext_i, aad_i)     (ciphertext || 16-byte tag)
```

- **Data key.** A random 32-byte data key (DEK) per backup.
- **Stream key.** `streamKey = HKDF-SHA256(secret = DEK, salt = fileNonce, info = "claviger/backup/v1")`, 32 bytes.
- **Nonce.** `nonce_i` = `fileNonce` with its last 8 bytes XOR-ed with the chunk counter `i` (starting at 0) as a big-endian uint64. A nonce is never reused, because the stream key is unique to the file.
- **AAD.** `aad_i = SHA-256(header bytes, from the magic through the fileNonce) || final`, where `final` is one byte: `0x01` for the last chunk, `0x00` otherwise. The header hash binds each chunk to its file, and the flag stops truncation: a stream cut at a chunk boundary ends on a chunk that was sealed as not final, and fails authentication.
- **Chunks.** Every chunk but the last holds exactly `chunkSize` plaintext bytes, so it is `chunkSize + 16` bytes long. The last chunk holds 0 to `chunkSize` bytes, and is always present (an empty stream is one 16-byte chunk). To find the last chunk without a length field, read `chunkSize + 17` bytes ahead: if fewer than that remain, the chunk is the last one. Bytes after the last chunk, a missing last chunk, reordered, dropped, altered or duplicated chunks, and a header from another file all fail authentication.
- **Wrapped DEK, local keys** (`CLAVIGER_KEK`, `CLAVIGER_KEK_FILE`, the app key fallback): `wrappedDEK = nonce(12) || AES-256-GCM-Seal(KEK, nonce, DEK, aad = "claviger/dek-wrap/v1|" + kekId)`. The key id is `"k" + hex(HMAC-SHA256(KEK, "claviger/kek-id/v1")[:8])`. For the app key fallback the KEK is `HKDF-SHA256(secret = the PB_ENCRYPTION_KEY string, salt = none, info = "claviger/kek/v1")`.
- **Wrapped DEK, Azure Key Vault:** the opaque result of the Key Vault wrap operation of the key named by `kekId`; unwrapping calls the same service. Decrypting offline then needs access to that key.

The decrypted stream is a standard zip archive that any unzip tool opens.

### Reference decryptor

A complete decryptor for a local KEK, to prove the format can be read without Claviger (Python 3 with `cryptography`):

```python
import base64, hashlib, struct, sys
from cryptography.hazmat.primitives import hashes
from cryptography.hazmat.primitives.ciphers.aead import AESGCM
from cryptography.hazmat.primitives.kdf.hkdf import HKDF

def decrypt(src, dst, kek_b64):
    kek = base64.b64decode(kek_b64)
    f = open(src, "rb")
    fixed = f.read(16)
    assert fixed[:8] == b"CLAVBAK\n" and fixed[8] == 1 and fixed[9] == 1
    chunk_size, id_len = struct.unpack(">IH", fixed[10:16])
    kek_id = f.read(id_len)
    (wl,) = struct.unpack(">H", f.read(2))
    wrapped = f.read(wl)
    file_nonce = f.read(12)
    header = fixed + kek_id + struct.pack(">H", wl) + wrapped + file_nonce
    header_hash = hashlib.sha256(header).digest()

    dek = AESGCM(kek).decrypt(wrapped[:12], wrapped[12:], b"claviger/dek-wrap/v1|" + kek_id)
    key = HKDF(hashes.SHA256(), 32, file_nonce, b"claviger/backup/v1").derive(dek)
    aead = AESGCM(key)

    out = open(dst, "wb")
    window = f.read(chunk_size + 17)
    i = 0
    while True:
        final = len(window) <= chunk_size + 16
        chunk = window if final else window[: chunk_size + 16]
        nonce = bytearray(file_nonce)
        for k, b in enumerate(struct.pack(">Q", i)):
            nonce[4 + k] ^= b
        out.write(aead.decrypt(bytes(nonce), chunk, header_hash + (b"\x01" if final else b"\x00")))
        if final:
            break
        window = window[chunk_size + 16 :] + f.read(chunk_size + 16)
        i += 1

decrypt(sys.argv[1], sys.argv[2], sys.argv[3])
```

## Limits and notes

- The file name, size and the number of chunks are visible. The names of the files inside the archive are not.
- Encryption protects the backup file. It does not change what the archive contains: the settings (including SMTP and S3 passwords) are inside `data.db`, and are only as safe as the key.
- Postgres installs: the archive holds the storage files and the local auxiliary database, not the Postgres data, and is encrypted the same way. Encrypt your `pg_dump` separately.
- Only the zip archive is encrypted. `.attrs` files next to local backups are storage metadata (size, content type).
- Recipient keys (`age`) are not part of this version. The format has a version byte for that.
