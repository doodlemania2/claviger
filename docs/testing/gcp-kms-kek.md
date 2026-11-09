# Runbook: Google Cloud KMS as the key-encryption key

Status: **not yet run against a real project.** The Cloud KMS source is tested against a fake server in CI
(`go test -tags kms_aws,kms_gcp ./tools/keys/...`). This runbook is the end-to-end drill: a real key, a real service
account, a running Claviger, and the failure modes. Use a **throw-away** project, key ring and Claviger instance.
Placeholders: `<project>`, `<location>`, `<ring>`, `<key>`, `<sa>`.

What you prove: a Claviger built with `kms_gcp` starts with the KEK in Cloud KMS, encrypted fields and encrypted backups
work, a rewrap from a local KEK works, a revoked permission is a definite `FAILED`, and a network block is indeterminate.

## 1. Build with the tag

The release image has no Cloud KMS support. Build your own:

```sh
docker build --build-arg GO_TAGS=kms_gcp -t claviger:kms-gcp .
# or: go build -tags kms_gcp -o claviger ./examples/base
```

Check the fail-closed behaviour of an untagged build first: with `CLAVIGER_KEK_GCP_KEY_ID` set, the stock image must
refuse to start and print an error that mentions `-tags kms_gcp`.

## 2. Create the key and the service account

```sh
gcloud kms keyrings create <ring> --location <location> --project <project>
gcloud kms keys create <key> --keyring <ring> --location <location> --project <project> \
  --purpose encryption
gcloud iam service-accounts create <sa> --project <project>
gcloud kms keys add-iam-policy-binding <key> --keyring <ring> --location <location> --project <project> \
  --member "serviceAccount:<sa>@<project>.iam.gserviceaccount.com" \
  --role roles/cloudkms.cryptoKeyEncrypterDecrypter
```

Attach the service account to the workload (Compute Engine, GKE Workload Identity, Cloud Run). For a local run only,
create a key file (`gcloud iam service-accounts keys create sa.json --iam-account ...`), keep it `chmod 600`, and point
`GOOGLE_APPLICATION_CREDENTIALS` at it. Delete it afterwards. `gcloud auth application-default login` credentials are
**not** supported.

## 3. Configure and start

```
CLAVIGER_KEK_GCP_KEY_ID=projects/<project>/locations/<location>/keyRings/<ring>/cryptoKeys/<key>
PB_ENCRYPTION_KEY=<32 characters>        # still needed for settings encryption; not the KEK when Cloud KMS is set
# GOOGLE_APPLICATION_CREDENTIALS=/path/to/sa.json   (omit on Google Cloud: the metadata server is used)
```

```sh
claviger keys check --dir=/pb_data --encryptionEnv=PB_ENCRYPTION_KEY
```

| State | Expected `keys check` |
|---|---|
| No encrypted field yet | exit 0 (nothing to open) |
| Encrypted field exists, permission right | exit 0, no `FAILED` |

Add an `encryptedText` field, create a record, read it back, confirm the stored value starts `enc:v1:`. `keys status`
shows the KEK id `gcpkms:projects/.../cryptoKeys/<key>`. Turn on **Encrypt new backups**, create a backup, confirm
`*.zip.enc`. Cloud Audit Logs (enable Data Access logs for Cloud KMS) should show `Encrypt`/`Decrypt` only at start, on
new fields and on rotation.

## 4. Rewrap from a local KEK and to a second KMS key

1. Start with `CLAVIGER_KEK=$(openssl rand -base64 32)` only. Add the field and a few records.
2. Stop. Set `CLAVIGER_KEK_GCP_KEY_ID=...` and `CLAVIGER_KEK_OLD=<the same base64 key>`; remove `CLAVIGER_KEK`
   (both together is an error, by design). Start: it starts and reads. `keys status` shows data keys on the old KEK.
3. `claviger keys rewrap`: completes in seconds; `keys status` shows every data key on the Cloud KMS KEK. Remove
   `CLAVIGER_KEK_OLD`, restart.
4. Create a second key. Set `CLAVIGER_KEK_GCP_KEY_ID=<key-2>` and `CLAVIGER_KEK_GCP_KEY_ID_OLD=<key>`, start,
   `keys rewrap`, remove the OLD variable, restart. Expected: reads still work.
5. Create a new **version** of the first key (`gcloud kms keys versions create --primary`): the key id does not change and
   nothing needs rewrapping; existing data keys still unwrap.

## 5. Failure drills

| # | Drill | How | Expected |
|---|---|---|---|
| F1 | Revoked permission | `remove-iam-policy-binding` for the service account; wait for propagation | `keys check: FAILED ...` (PERMISSION_DENIED), exit 1; the entrypoint stops the rollout before the handover. **Definite** |
| F2 | Restore the role | add the binding again | `keys check` passes again |
| F3 | Key not found | point at a key name that does not exist | `FAILED` (NOT_FOUND) |
| F4 | Bad credentials | corrupt or delete the service account key (`gcloud iam service-accounts keys delete`) | `FAILED` (invalid_grant); no key material in the output |
| F5 | Network block | block egress to `cloudkms.googleapis.com` | `skipped (could not reach the key-encryption key service ...)`, exit 0: **indeterminate** |
| F6 | Start during F5 | restart while blocked | the app does not start (fail closed). Unblock, it starts |
| F7 | Outage while running | block after a successful start | reads and writes keep working (data keys are cached); a **new** encrypted field cannot get its data key until unblocked |
| F8 | Disable the primary version | `gcloud kms keys versions disable` | `FAILED` at start (primary not ENABLED). Re-enable to recover. Do this last |

Never destroy key versions during these drills unless the whole instance is disposable.

## 6. Clean up

```sh
gcloud kms keys versions destroy <n> --key <key> --keyring <ring> --location <location> --project <project>
gcloud iam service-accounts delete <sa>@<project>.iam.gserviceaccount.com
```

Key rings cannot be deleted; use a throw-away project and delete the project. Delete the test instance, its volume and
backups made under the key.

## Results template

```
Google Cloud KMS KEK drill
Date:                      Tester:
Claviger version/commit:   Built with -tags kms_gcp: yes
Credentials: attached service account [ ]  key file [ ]
Untagged build refuses to start with a message naming the tag:   pass [ ] fail [ ]
3. Startup and use                       pass [ ] fail [ ]   keys check (healthy) exit:
   encrypted field / backup (.zip.enc):  pass [ ] fail [ ]
4. Rewrap local -> KMS:  pass [ ] fail [ ]   key 1 -> key 2: pass [ ] fail [ ]   new version: pass [ ] fail [ ]
5. Failure drills (exact output line, exit code)
   F1:   F2:   F3:   F4:   F5:   F6:   F7:   F8:
Notes / deviations:
```
