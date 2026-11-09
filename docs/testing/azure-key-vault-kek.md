# Runbook: Azure Key Vault as the key-encryption key

Status: **not yet run against a real vault.** The Key Vault client is tested against a fake server in CI, and
`go test -tags azurekv_live` can run a wrap/unwrap round trip (`docs/field-encryption.md`, "Live test"). This runbook is
the end-to-end drill: a real vault, a real identity, a running Claviger, and the failure modes. Use a **throw-away**
resource group, vault and Claviger instance. Placeholders: `<rg>`, `<vault>`, `<key>`, `<app>`, `<env>`, `<sub>`.

What you prove: the server starts with the KEK in the vault, encrypted fields and encrypted backups work, a rewrap from
an environment KEK works, and a revoked permission is reported as a definite FAILED while a network block is reported as
indeterminate (the start is not blocked by the pre-flight).

## 1. Create the vault and the key

```sh
az group create -n <rg> -l <region>
az keyvault create -n <vault> -g <rg> --enable-rbac-authorization true \
  --enable-purge-protection true        # soft delete is on by default
az keyvault key create --vault-name <vault> -n <key> --kty RSA --size 3072 \
  --ops wrapKey unwrapKey
```

Your own user needs **Key Vault Crypto Officer** on the vault to create keys (`az role assignment create`), and
**Key Vault Crypto User** to run the live test. For an **access-policy** vault instead of RBAC, create it with
`--enable-rbac-authorization false` and grant the identity `get`, `wrapKey`, `unwrapKey` on keys.

Key id for Claviger: `https://<vault>.vault.azure.net/keys/<key>` (add `/<version>` to pin one).

## 2. Give Claviger an identity

### Azure Container Apps (the reference deployment shape)

```sh
az containerapp identity assign -n <app> -g <rg> --system-assigned
PRINCIPAL=$(az containerapp show -n <app> -g <rg> --query identity.principalId -o tsv)
VAULT_ID=$(az keyvault show -n <vault> --query id -o tsv)
az role assignment create --assignee-object-id "$PRINCIPAL" --assignee-principal-type ServicePrincipal \
  --role "Key Vault Crypto User" --scope "$VAULT_ID/keys/<key>"
```

Scope the role to the key, not the vault (least privilege). Role assignments take up to a few minutes to apply.
For a user-assigned identity set `AZURE_CLIENT_ID` to its client id on the app; leave it unset for system-assigned.

### Local (no Azure host)

Use a service principal with the same role and `CLAVIGER_KEK_AZURE_AUTH=client-secret` with `AZURE_TENANT_ID`,
`AZURE_CLIENT_ID`, `AZURE_CLIENT_SECRET` in the environment. A signed-in `az login` user is **not** used by Claviger.

## 3. Configure and start

Environment of the Claviger container (`docs/field-encryption.md`, "Azure Key Vault"):

```
CLAVIGER_KEK_AZURE_KEY_ID=https://<vault>.vault.azure.net/keys/<key>
# CLAVIGER_KEK_AZURE_AUTH=auto   (managed identity is picked by IDENTITY_ENDPOINT on Container Apps)
PB_ENCRYPTION_KEY=<32 characters>   # still needed for settings encryption; not the KEK when the vault is set
```

Check before and after starting:

```sh
claviger keys check --dir=/pb_data --encryptionEnv=PB_ENCRYPTION_KEY
```

| State | Expected `keys check` |
|---|---|
| No encrypted field yet | exit 0 (nothing to open) |
| Encrypted field exists, identity and permission right | exit 0, no `FAILED` |

Then: add an `encryptedText` field to a collection (dashboard or API), create a record, read it back (plaintext through
the API), and confirm the stored value starts `enc:v1:`. `claviger keys status` lists the field, its key id and the KEK
id (`azkv:<vault host>/<key>/<version>`).

Switch on Settings, Backups, **Encrypt new backups**. Saving proves the key can wrap and unwrap (permissions included);
create a backup and confirm `*.zip.enc`.

## 4. Rewrap from an environment KEK

Start the instance once with a local KEK to create the encrypted data, then move it to the vault.

1. Start with `CLAVIGER_KEK=$(openssl rand -base64 32)` (no `CLAVIGER_KEK_AZURE_KEY_ID`). Add the encrypted field and a
   few records; make a backup (encrypted, if you want to prove the old key still opens it).
2. Stop. Set `CLAVIGER_KEK_AZURE_KEY_ID=...` and move the local key to `CLAVIGER_KEK_OLD=<the same base64 key>`;
   remove `CLAVIGER_KEK`. (Setting both `CLAVIGER_KEK` and the vault id is an error, by design.)
3. Start. Expected: starts, the data is still readable, `keys status` shows data keys whose KEK differs from the
   current one ("old KEK").
4. `claviger keys rewrap` (or **Re-wrap** on Settings, Encryption keys). Expected: it completes in seconds; `keys status`
   shows every data key on the vault KEK; row data was not rewritten.
5. Remove `CLAVIGER_KEK_OLD`, restart. Expected: still starts and reads. The old plaintext-key backup is no longer
   openable without `CLAVIGER_KEK_OLD`, as documented (`docs/backup-encryption.md`).
6. Optionally create a **new key version** in the vault (`az keyvault key rotate`), restart, `keys rewrap`, and confirm
   the KEK id's version part changed.

## 5. Failure drills

Run each drill with the encrypted field present. Record the exact log and exit lines.

| # | Drill | How | Expected |
|---|---|---|---|
| F1 | Revoked permission | `az role assignment delete` for the app's identity on the key; wait for propagation (poll `keys check` until it changes) | `claviger keys check` prints `keys check: FAILED ...` and exits 1; with `entrypoint.sh` the rollout is stopped **before** the handover request is written. A **definite** failure |
| F2 | Restore the role | `az role assignment create` again | `keys check` passes again, no restart needed for the check |
| F3 | Key not found | point `CLAVIGER_KEK_AZURE_KEY_ID` at a name that does not exist | `FAILED` (404 is definite) |
| F4 | Wrong tenant / bad client secret (client-secret auth) | change `AZURE_CLIENT_SECRET` | `FAILED` (credentials refused) |
| F5 | Network block | vault firewall to deny the app, or block egress to `<vault>.vault.azure.net` (container: `iptables`/NSG rule) | `keys check` prints `skipped (could not reach the key-encryption key service ...)`, exit 0: **indeterminate**, never reported as FAILED |
| F6 | Start during F5 | restart the app while blocked | the app does not start (fail closed); logs say the vault is unreachable. Unblock, it starts |
| F7 | Outage while running | block after a successful start | reads and writes of encrypted fields keep working (data keys are cached); a **new** encrypted field cannot get its data key until unblocked |
| F8 | Disable the key version in use | `az keyvault key set-attributes --enabled false` | `FAILED` or refusal on startup; re-enable and recover. Do this last |

Never delete the key during these drills with purge protection off. The drill resource group is deleted at the end.

## 6. Clean up

```sh
az group delete -n <rg> --yes
az keyvault purge --name <vault>      # only if purge protection was left off; with it on the name stays reserved for the retention period
```

Delete the test Claviger instance, its volume and the backups made with this vault key (they are unreadable once the key
is gone). Remove the role assignments if the identity outlives the drill.

## Results template

```
Azure Key Vault KEK drill
Date:                      Tester:
Claviger version/commit:
Vault type: Key Vault [ ]  Managed HSM [ ]     Permission model: RBAC [ ]  access policy [ ]
Key: RSA ____ bits, version pinned: yes/no       Auth: managed identity [ ] workload identity [ ] client secret [ ]

3. Startup and use
   start with vault KEK:                 pass [ ] fail [ ]
   encrypted field create/read/stored enc:v1:   pass [ ] fail [ ]
   keys check (healthy) exit code:
   encrypted backup (.zip.enc) + restore verification:   pass [ ] fail [ ]
4. Rewrap from env KEK
   start with CLAVIGER_KEK_OLD:          pass [ ] fail [ ]
   keys rewrap duration:                 data readable after: yes/no
   after removing the old variable:      pass [ ] fail [ ]
   new key version + rewrap:             pass [ ] fail [ ] n.a. [ ]
5. Failure drills (exact output line, exit code)
   F1 revoked role:
   F2 restored:
   F3 key not found:
   F4 bad credentials:
   F5 network block (must say skipped, exit 0):
   F6 start while blocked:
   F7 outage while running:
   F8 disabled version:
Bugs found (issue links):
Clean-up done: resource group deleted [ ]  purge [ ]  instance and backups removed [ ]
```
