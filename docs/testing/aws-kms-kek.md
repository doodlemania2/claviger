# Runbook: AWS KMS as the key-encryption key

Status: **not yet run against a real AWS account.** The AWS KMS source is tested against a fake KMS in CI
(`go test -tags kms_aws,kms_gcp ./tools/keys/...`). This runbook is the end-to-end drill: a real key, a real identity, a
running Claviger, and the failure modes. Use a **throw-away** account or sandbox, key and Claviger instance.
Placeholders: `<region>`, `<key-arn>`, `<role>`, `<policy>`.

What you prove: a Claviger built with `kms_aws` starts with the KEK in KMS, encrypted fields and encrypted backups work,
a rewrap from a local KEK works, a revoked permission is a definite `FAILED`, and a network block is indeterminate.

## 1. Build with the tag

The release image has no AWS KMS support. Build your own:

```sh
docker build --build-arg GO_TAGS=kms_aws -t claviger:kms-aws .
# or: go build -tags kms_aws -o claviger ./examples/base
```

Check the fail-closed behaviour of an untagged build first: with `CLAVIGER_KEK_AWS_KEY_ID` set, the stock image must
refuse to start and print an error that mentions `-tags kms_aws`.

## 2. Create the key and the permission

```sh
aws kms create-key --region <region> --description "claviger kek drill" \
  --key-usage ENCRYPT_DECRYPT --key-spec SYMMETRIC_DEFAULT
aws kms create-alias --alias-name alias/claviger-drill --target-key-id <key-arn>
```

Least-privilege policy for the identity Claviger runs as:

```json
{ "Version": "2012-10-17",
  "Statement": [{ "Effect": "Allow",
    "Action": ["kms:DescribeKey", "kms:Encrypt", "kms:Decrypt"],
    "Resource": "<key-arn>" }] }
```

Attach it to the task role / instance profile / IRSA role. For a local run use a throw-away IAM user's keys in
`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`.

## 3. Configure and start

```
CLAVIGER_KEK_AWS_KEY_ID=<key-arn>        # or alias/claviger-drill together with AWS_REGION
AWS_REGION=<region>
PB_ENCRYPTION_KEY=<32 characters>        # still needed for settings encryption; not the KEK when KMS is set
```

```sh
claviger keys check --dir=/pb_data --encryptionEnv=PB_ENCRYPTION_KEY
```

| State | Expected `keys check` |
|---|---|
| No encrypted field yet | exit 0 (nothing to open) |
| Encrypted field exists, permission right | exit 0, no `FAILED` |

Add an `encryptedText` field, create a record, read it back, confirm the stored value starts `enc:v1:`. `keys status`
shows the KEK id `awskms:<key-arn>` (the **ARN**, also when you configured an alias). Turn on **Encrypt new backups**,
create a backup, confirm `*.zip.enc`. In CloudTrail you should see `Encrypt`/`Decrypt` events only at start, on new
fields, and on rotation, none while serving requests.

## 4. Rewrap from a local KEK and to a second KMS key

1. Start with `CLAVIGER_KEK=$(openssl rand -base64 32)` only. Add the field and a few records.
2. Stop. Set `CLAVIGER_KEK_AWS_KEY_ID=<key-arn>` and `CLAVIGER_KEK_OLD=<the same base64 key>`; remove `CLAVIGER_KEK`
   (both together is an error, by design). Start: it starts and reads. `keys status` shows data keys on the old KEK.
3. `claviger keys rewrap`: completes in seconds; `keys status` shows every data key on the KMS KEK. Remove
   `CLAVIGER_KEK_OLD`, restart.
4. Create a second key. Set `CLAVIGER_KEK_AWS_KEY_ID=<key-2-arn>` and `CLAVIGER_KEK_AWS_KEY_ID_OLD=<key-arn>`, start,
   `keys rewrap`, remove the OLD variable, restart. Expected: reads still work.
5. Optionally enable automatic rotation (`aws kms enable-key-rotation`): the ARN does not change; nothing needs rewrapping.

## 5. Failure drills

| # | Drill | How | Expected |
|---|---|---|---|
| F1 | Revoked permission | detach the policy; wait for propagation | `keys check: FAILED ...` (AccessDeniedException), exit 1; the entrypoint stops the rollout before the handover. **Definite** |
| F2 | Restore the policy | re-attach | `keys check` passes again |
| F3 | Key not found | point at an ARN that does not exist | `FAILED` (NotFoundException) |
| F4 | Bad credentials | change `AWS_SECRET_ACCESS_KEY` | `FAILED` (signature / token rejected); the secret is not in the output |
| F5 | Network block | block egress to `kms.<region>.amazonaws.com` | `skipped (could not reach the key-encryption key service ...)`, exit 0: **indeterminate** |
| F6 | Start during F5 | restart while blocked | the app does not start (fail closed). Unblock, it starts |
| F7 | Outage while running | block after a successful start | reads and writes keep working (data keys are cached); a **new** encrypted field cannot get its data key until unblocked |
| F8 | Disable the key | `aws kms disable-key` | `FAILED` / refusal at start (key not Enabled). `enable-key` to recover. Do this last |

Never schedule deletion of the key during these drills unless the whole instance is disposable.

## 6. Clean up

```sh
aws kms schedule-key-deletion --key-id <key-arn> --pending-window-in-days 7
aws kms delete-alias --alias-name alias/claviger-drill
```

Delete the test instance, its volume and backups made under the key (unreadable once the key is gone), and the IAM
resources.

## Results template

```
AWS KMS KEK drill
Date:                      Tester:
Claviger version/commit:   Built with -tags kms_aws: yes
Key: ARN used / alias used     Credentials: env [ ] web identity [ ] shared file [ ] ECS [ ] EC2 [ ]
Untagged build refuses to start with a message naming the tag:   pass [ ] fail [ ]
3. Startup and use                       pass [ ] fail [ ]   keys check (healthy) exit:
   encrypted field / backup (.zip.enc):  pass [ ] fail [ ]
4. Rewrap local -> KMS:  pass [ ] fail [ ]   KMS key 1 -> key 2: pass [ ] fail [ ]
5. Failure drills (exact output line, exit code)
   F1:   F2:   F3:   F4:   F5:   F6:   F7:   F8:
Notes / deviations:
```
