# Fork Sync Workflow

How to keep this fork (Claviger) in sync with upstream PocketBase releases.

The model is simple: one trunk (`master`), and upstream comes in by **merge**. No rebase, no force-push, no patch replay.

## Sync Status

**Current:** synced to upstream **v0.41.1** (`f70b24e1`) on 2026-10-30.

> **The `**Current:**` line is machine-read — keep its shape.**
> `.github/workflows/upstream-check.yml` parses the synced version from the first
> line matching `**Current:** synced to upstream **vX.Y.Z** ...`. That is how the
> weekly watcher knows whether to file a sync issue and when to auto-close it. Reword
> the rest freely, but keep that prefix and the bold `**vX.Y.Z**` tag. If the parse
> fails, the workflow errors out loudly. Do not bring back `synced-*` git tags: they
> were never pushed and only caused duplicate issues.

`go-webauthn/webauthn` is at **v0.18.2** (`x` v0.3.1).

## Branches

| Branch | Role |
| --- | --- |
| `master` | The one trunk. All work lands here by PR, including WebAuthn, packaging and every upstream sync. |
| `deploy/azure` | **Release pointer only.** The private ops repo `doodlemania2/stfoa-auth` deploys this branch by default. It is fast-forwarded to a `master` commit that has been tested and approved for prod. Never commit to it directly. |
| `feat/webauthn-passkey-support` | Retired. Left in place, frozen, not deleted. Do not push to it. |

## Upstream facts

- **Fetch URL:** `https://ganigeorgiev.com/git/pocketbase/pocketbase`. No `.git` suffix (`.../pocketbase.git` 404s).
- The browsable page `https://ganigeorgiev.com/pocketbase/pocketbase` is a single-page app that answers HTTP 200 to any path, so a 200 proves nothing. Test with `git ls-remote`.
- **Upstream left GitHub after v0.40.6** (2026-10-26). `github.com/pocketbase/pocketbase` is dead as a sync source; it fetches nothing newer.
- SSH is `ssh://ganigeorgiev.com:8022/...`, but anonymous access is refused.
- Release downloads: `https://ganigeorgiev.com/meta/pocketbase/pocketbase/download/{tag}/{filename}`.
- Upstream's own `CHANGELOG.md` is mangled by their search-and-replace. Trust `go.mod`, not the prose.
- **Module paths.** Upstream's module is `ganigeorgiev.com/pocketbase/pocketbase` (since v0.41.0). **This fork's module is `github.com/doodlemania2/claviger`.** The helper libs keep upstream's paths: `ganigeorgiev.com/pocketbase/{dbx,ozzo-validation/v4,tygoja}` and `ganigeorgiev.com/fexpr`.

## Prerequisites

| Remote | URL | Role |
| --- | --- | --- |
| `origin` | `https://github.com/doodlemania2/claviger.git` | Our fork. Push target. |
| `upstream` | `https://ganigeorgiev.com/git/pocketbase/pocketbase` | Fetch only. Push URL is `DISABLED`. |

```bash
# One-time setup for a fresh clone:
git clone https://github.com/doodlemania2/claviger.git
cd claviger
git remote add upstream https://ganigeorgiev.com/git/pocketbase/pocketbase
git remote set-url --push upstream DISABLED
git fetch upstream --tags
```

A clone whose `upstream` still points at `github.com/pocketbase/pocketbase` fetches nothing newer than v0.40.6. Repoint it with `git remote set-url upstream https://ganigeorgiev.com/git/pocketbase/pocketbase && git remote set-url --push upstream DISABLED`.

Tools: Go 1.27+, Node 24 with `npm`, Docker, golangci-lint v2 (the version pinned in `.github/workflows/test.yml`).

## Sync Procedure

### 1. Fetch and review the delta

```bash
git fetch upstream --tags
git fetch origin
git log --oneline <last-synced-tag>..<new-tag>
git diff --stat <last-synced-tag> <new-tag> -- . ':(exclude)ui/dist'
```

**The owner (Derek) approves each upstream change before it is adopted.** List the substantive changes (not import-path churn) and ask per change. Do not merge before that.

### 2. Merge on a sync branch

```bash
git checkout -b sync/<new-tag> origin/master
git merge --no-ff <new-tag>
# resolve conflicts (see the table below)
scripts/rewrite-module-path.sh      # rewrites ganigeorgiev.com/pocketbase/pocketbase -> github.com/doodlemania2/claviger; idempotent
go mod tidy
gofmt -l .                          # import blocks regroup after the rewrite; gofmt -w the files it lists
go build ./... && go test ./...
```

The rewrite script must run after every merge: upstream's files arrive with upstream's module path. If you resolve a conflict by hand, run the script again afterwards. It is safe to repeat.

Commit the rewrite as its own commit on the sync branch so the merge commit stays a clean upstream merge.

Files the fork modifies that upstream also owns (expect conflicts here):

| File | Risk | Notes |
| --- | --- | --- |
| `go.mod` / `go.sum` | MEDIUM | Take upstream's, then re-add `go-webauthn/webauthn` and the OTLP/grpc pins. Run `go mod tidy`. |
| `core/collection_model.go` | **HIGH** | `MarshalJSON` redaction. See the checklist. |
| `core/collection_model_auth_options.go` | MEDIUM | Our addition is the `WebAuthn` field (`WebAuthnConfig.Discoverable`) and one `o.validateSignInMethods(cv)` call in `validate` (logic lives in the fork-only `core/collection_webauthn_validate.go`). |
| `core/app.go`, `core/base.go` | LOW/MEDIUM | Our hook methods and fields (incl. `OnAuditEntry`, #74: interface method, `onAuditEntry` field/init/accessor, and `app.registerAuditHooks()` at the end of `registerBaseHooks`). `core/base.go` also has the OTLP `slog` tee and the logger sink-failure tally inside `runLogsWrite`. |
| `core/base.go` (traces/metrics) | LOW | One call, `app.initTelemetry()`, in `Bootstrap` just before `initDataDB` (#76). Must stay before the DB connections. |
| `core/db_connect.go` | LOW | Four lines at the top of `DefaultDBConnect`: `openTelemetryDB` returns the instrumented driver only while telemetry is active (#76). |
| `apis/base.go` | LOW | Two lines in `NewRouter`: `telemetryMiddleware()` bound first, and `bindMetricsApi` after `bindSQLApi` (#76). |
| `core/events.go`, `core/record_tokens.go`, `pocketbase.go` | LOW | Mostly import-order conflicts. `core/record_tokens.go` also carries `TokenClaimPasskeyAt` and `NewPasskeyAuthToken` (passkey step-up, #73). |
| `tests/app.go`, `tests/api.go` | LOW | Race fix (#49): `EventCallsSnapshot()` in `tests/app.go`, and `tests/api.go` reads events through it instead of `EventCalls`. Kept in the fork, not sent upstream. If upstream rewrites those reads, keep the snapshot call. |
| `apis/record_auth_otp_request_test.go` | LOW | The "existing auth record (with < 9 non-expired)" scenario polls up to 2s for `sentTo`: the mail send and the `sentTo` update run in a `FireAndForget` routine, which can finish after the mail count on Postgres (#106). Keep the poll if upstream rewrites that check. |
| `apis/record_auth.go` | LOW | Our route bindings, including the discoverable login and step-up routes and `RequireFreshPasskey` on credential delete (#73). |
| `pocketbase.go` | LOW | Two lines (#71): `oidc.Register(pb.App)` in `New` (before the help command is hidden) and `pb.RootCmd.AddCommand(oidc.NewCommand(pb))` in `Start`, plus the `oidc` import. Nothing else of the OIDC provider touches upstream-owned code. |
| `go.mod` / `go.sum` (OIDC) | MEDIUM | `github.com/zitadel/oidc/v3 v3.51.17` and `github.com/go-jose/go-jose/v4 v4.1.6` are direct deps (#71); the rest (`doublestar`, `chi`, `securecookie`, `gu`, `httpforwarded`, `rs/cors`, `zitadel/schema`) are indirect. `otel`, `otel/metric` and `otel/trace` moved to v1.46.0 because of them. Keep the pins on a sync, then `go mod tidy`. |
| `apis/collection_test.go`, `apis/collection_import_test.go`, `core/collection_query_test.go` | LOW | System collection count is +6 again (#71): `_oauthClients`, `_oauthRequests`, `_oauthTokens`, `_oauthRefreshTokens`, `_oauthConsents`, `_signingKeys` (24 to 30). The paging scenario in `collection_test.go` now expects `_oauthRefreshTokens`/`_oauthTokens` on page 2. |
| `Makefile` | LOW | `oidc-conformance` target (#114): runs `scripts/oidc-conformance-instance.sh`. |
| `apis/collection_test.go`, `apis/collection_import_test.go`, `core/collection_query_test.go` (again) | LOW | One more system collection (#114): `_oauthSso`, the single sign-on sessions of the hosted sign-in page (31 to 32, and the paging scenario). |
| `ui/src/router.js`, `ui/src/store.js`, `ui/src/main.js`, `ui/src/css/_main.css` | LOW | `#/oidc` route and nav entry, the client modal import and the `oidc.css` import (#71). Rebuild `ui/dist` (not committed on the #71 branch). |
| `THIRD_PARTY_NOTICES.md` | LOW | Added the OIDC provider's dependencies (#71). |
| `core/record_model.go` | LOW | Audit log (#74): `cascadeRecordDelete`/`deleteRefRecords` take a leading `ctx` and use `DeleteWithContext`/`SaveNoValidateWithContext`, so cascade deletes keep the audit actor. Re-apply if upstream touches those two functions. |
| `apis/record_crud.go` | LOW | Audit log (#74): `form.SetContext(auditContext(e))` after both `NewRecordUpsert` calls and `DeleteWithContext(auditContext(...))` in `recordDelete`. Without them every API change is attributed to `system`. |
| `apis/sql.go` | LOW | Audit log (#74): write queries mentioning `_auditLog` are rejected in `executeQuery`. |
| `tests/api.go` | LOW | Audit log (#74): `OnAuditEntry` is dropped from the "no other events" check unless the scenario lists it. |
| `apis/collection_test.go`, `apis/collection_import_test.go`, `core/collection_query_test.go` | LOW | System collection count is one higher again (`_auditLog`); the paging scenario in `collection_test.go` expects `_authOrigins` on page 2. |
| `core/collection_model_test.go`, `plugins/migratecmd/migratecmd_test.go` | LOW | JSON snapshots: keep the `webauthn` field (incl. `discoverable`) and any new upstream `*ResetToken`/`*Template` fixture keys. |
| `apis/record_auth_methods.go` | LOW | `webauthn` response gained `discoverable` and `passkeyOnly` (#73). |
| `core/collection_query_test.go` | LOW | System collection count is upstream's plus one. |
| `plugins/jsvm/binds_test.go` | LOW | Hook count. See the checklist. |
| `.gitignore` | LOW | Keep both sides. |
| `ui/dist/*`, `ui/package-lock.json` | LOW | Take upstream's, then rebuild `ui/dist` (see the checklist). |
| `README.md`, `.github/` | LOW | Upstream may delete or change these. Keep the fork's copy. |

### 3. Pitfall checklist (re-verify after every merge)

- [ ] **Token redaction.** `core/collection_model.go` `MarshalJSON` must blank `Secret` on every `*ResetToken` config, including `PasskeyResetToken` (upstream has none). If upstream adds a new token type, add the matching `alias.X.Secret = ""` line. Check with `grep -n 'ResetToken.Secret' core/collection_model.go`, and keep the `notExpected` list in `TestCollectionSerialize` in step. A missing line leaks the secret through the API.
- [ ] **Passkey reset token type.** `core/record_query.go` `FindAuthRecordByToken` must keep the fork's `case TokenTypePasskeyReset` (secret `PasskeyResetToken.Secret`); upstream has no such type and a merge can drop the case. Check with `grep -n TokenTypePasskeyReset core/record_query.go`, and run `go test -run 'PasskeyReset' ./core/... ./apis/...`. Without it every emailed reset link is rejected (#39).
- [ ] **modernc SQLite.** `apis/TestSQLRun/single_write_query` expects `affectedRows: 1` for DDL. modernc reports 1, upstream's mattn reports 0. Keep the fork's value.
- [ ] **`modernc_versions_check.go`** pins the expected sqlite/libc versions. Keep it in step with `go.mod` when they change.
- [ ] **Audit log guards.** `core/audit_model.go` must stay intact after a merge: the `__pbAuditGuard__` model hooks (create only with the unexported writer context, never update/delete), the `_auditLog` collection update/delete guards, and the `OnRecord{Create,Update,Delete}Execute` capture hooks at priority 100 (they must stay *inside* upstream's priority-99 execute handlers). The `_auditLog` definition guard has one deliberate hole, `core.WithAuditSchemaChange(ctx)` (`auditSchemaCtxKey`), for fork migrations that evolve the audit schema; it must not unlock deletes or entry writes (`TestAuditSchemaChangeBypass`). Run `go test -run 'Audit' ./core/... ./apis/...`. If upstream adds another write path for records that bypasses `Save`/`Delete`, it bypasses the audit too.
- [ ] **API keys.** `loadAuthToken` in `apis/middlewares.go` must keep the `core.IsAPIKeyToken` branch ahead of `FindAuthRecordByToken`, and `enforceAPIKeyScopes` (`apis/apikey_auth.go`) denies unknown routes unless the key has `*`: if upstream adds an auth flow or account-changing route, add it to the deny list there. Run `go test -run 'APIKey' ./apis/...`.
- [ ] **Sessions and bans (#93).** Four small hunks in upstream-owned code call into `apis/sessions_auth.go`: `loadAuthToken` (`checkSessionClaim`), `recordAuthResponse` in `apis/record_helpers.go` (`checkNotBanned` before the MFA step, `attachSession` after it), `recordAuthRefresh` (`carrySession`, `skipSessionAttach`) and the password reset request (`silentlyRefuseBanned`). Keep `attachSession` *after* `checkMFA`: a session must exist only once the sign-in is complete. `core/base.go` calls `registerSessionHooks`. Run `go test -run 'Session|Ban|Legacy|Impersonate' ./apis/...`. Tokens without a `sid` are deliberately not checked (they are the pre-deploy tokens); do not "fix" that.
- [ ] **OIDC provider (#71).** `pocketbase.go` must keep `oidc.Register(pb.App)` and the `oidc` command. The provider refuses to run outside dev mode without the settings encryption key (it would have to store signing keys in clear text); `go test -run 'TestDisabledWithoutEncryption' ./oidc` pins it. `hiddenFields` in `oidc/hooks.go` re-hides `secretHash`, `privateKey`, `tokenHash` and the request secrets in `OnRecordEnrich` (the records API unhides every hidden field for superusers): if upstream changes enrich ordering, run `go test -run 'TestNoSecretsInAPIResponsesOrAudit' ./oidc`. Zitadel quirks we work around and that an upgrade may fix or break: it advertises the implicit grant and device endpoint unconditionally (`serveDiscovery` strips them), it loses `acr` on refresh (re-added in `SetUserinfoFromRequest`), `SigningKey(ctx)` is not told the client (`withClientAlg`), and `ValidateAuthReqScopes` lets the standard scopes through (`CreateAuthRequest` re-filters). After bumping `zitadel/oidc`, run the whole `./oidc` suite.
- [ ] **API key lifetime.** Changing the owner's password does not revoke their API keys, by design (like personal access tokens); revoking is always explicit (`DELETE /api/apikeys/{id}`).
- [ ] **`plugins/jsvm/binds_test.go`** asserts exact hook counts. If the merge adds or removes a hook on `core.App`, update the count (`TestHooksBindsCount`).
- [ ] **`ui/dist` rebuild.** Use Node 24. `rm -rf ui/dist && cd ui && npm ci && ./node_modules/.bin/vite build`. Always `npm ci`, never `npm install` (npm 10 strips the `libc` fields from the lockfile and breaks the Linux CI install). Run vite directly, not `npm run build`. Then `git diff --quiet ui/package-lock.json`. Commit the regenerated `ui/dist`.
- [ ] **Module path ldflag.** `Dockerfile` and `.goreleaser.yaml` use `-X github.com/doodlemania2/claviger.Version=`. A stale `-X` package is silently ignored by the linker, so the binary would report the default version. Verify with a built binary: `claviger --version`.
- [ ] **No stale imports.** `grep -rn 'github.com/pocketbase/\|ganigeorgiev.com/pocketbase/pocketbase' --include='*.go' .` must show nothing for the main module path (helper libs under `ganigeorgiev.com/pocketbase/{dbx,ozzo-validation,tygoja}` are expected).
- [ ] **Go version.** If upstream bumped the `go` directive in `go.mod`, bump the builder image in `Dockerfile` (`FROM golang:X.Y-alpine`), the `Requires Go X.Y+` line in `CLAUDE.md`, and the Go version in `.github/workflows/release.yaml`. Only the CI Docker build catches a stale builder image.
- [ ] **Lint.** `make lint` with golangci-lint v2 at the version pinned in `test.yml` (at least v2.12.0, since `golangci.yml` names `gomodguard_v2`). It must be built with at least the Go that `go.mod` resolves to, or it fails with `export data version ... greater than maximum supported`.
- [ ] **WebAuthn.** Run `go list -m -u github.com/go-webauthn/webauthn github.com/go-webauthn/x`. If newer, read the release notes for breaking changes (`webauthn.Config` fields, `Credential` shape, removed constants), bump, `go mod tidy`, then run the ceremony suite: `go test -count=1 -run 'TestWebAuthnCeremony|TestInitWebAuthn' ./apis/`. It is the only suite that exercises a successful registration and login with real signatures.
- [ ] **Docker smoke.** `docker build -t claviger:sync-test .`, run it, and `curl -sf localhost:8090/api/health`. CI repeats this plus the handover scenarios.
- [ ] **Known flaky tests** (re-run the single test before investigating; ignore unless other tests fail):

  | Test | Cause |
  | --- | --- |
  | `core/TestNotifyWatcher_CollectionsUpdate` | `fsnotify` duplicate WRITE events on macOS. |
  | `core/TestNotifyWatcher_SettingsUpdate` | Same macOS `fsnotify` flake. |
  | `apis/TestDefaultRateLimitMiddleware` | Timing-sensitive windows; flakes under CI load on Linux. |
  | `tools/cron/TestCronStartStop` | Timing-sensitive scheduler assertions under CI load. |
  | `core/TestImportCollections` | Load-sensitive test-app setup (the `tests.NewTestApp()` error is discarded). |

### 4. Pull request and merge

Open a PR from `sync/<new-tag>` to `master`. Wait for CI (`Test` workflow: tests, lint, Docker build, handover scenarios) to go green. Merge with a **merge commit**, not rebase and not squash. The upstream merge must stay a merge so the next sync has a common ancestor.

Then update **Sync Status** above (the `**Current:**` line and the History section) on `master`, in the PR or right after it.

### 5. Release pointer

After the merged `master` commit has been tested and approved for prod:

```bash
git fetch origin
git push origin <tested-master-sha>:deploy/azure     # fast-forward only; no --force
```

Git refuses a non-fast-forward push. If it refuses, stop and find out why; do not force it.

Pushing `deploy/azure` does not deploy. Production deploys are a separate manual step in the private repo `doodlemania2/stfoa-auth` (start a dry run, then the deploy, then watch `/api/health`).

## Fork-modified upstream files

Upstream-owned files the fork edits. Re-check these on every sync merge (conflicts, and tests that assert exact output).

| File | Fork change (issue) |
| --- | --- |
| `core/backup.go` | Auto backup starts a background restore verification after the heartbeat (#78). |
| `core/backup_create.go` | Writes `.claviger_backup_manifest.json` (per-table row counts) into every archive; `zipWriter.copyBytesToZip` (#78). |
| `core/backup_restore.go` | Drops the manifest from the extracted dir so it never lands in `pb_data` (#78). |
| `core/settings_model.go` | `BackupsConfig.DisableVerify` (#78). Exact-JSON assertions in `core/settings_model_test.go` carry the field. |
| `core/backup_test.go` | `verifyBackupContent` expects the manifest entry (#78). |
| `apis/backup.go` | `POST /api/backups/{key}/verify`; list adds `verification`/`verifying`; delete clears the stored result (#78). |
| `ui/src/settings/backups/backupsList.js`, `backupsForm.js` | Verification badge and button; auto-verify switch (#78). |
| `core/base.go` | One call, `app.registerWebhookHooks()`, right after `app.registerAuditHooks()` in `registerBaseHooks` (#77). |
| `core/audit_model.go` | `_webhookDeliveries` added to `auditExcludedCollections` (#77). |
| `apis/base.go` | `bindWebhooksApi(app, apiGroup)` after `bindMetricsApi` (#77). |
| `tools/telemetry/telemetry.go` | `webhookDeliveries` counter field and instrument (`claviger.webhook.deliveries`) (#77). |
| `apis/collection_test.go`, `apis/collection_import_test.go`, `core/collection_query_test.go` | System collection count is two higher (`_webhooks`, `_webhookDeliveries`: 18 to 20); the paging scenario's page 2 now starts at `_auditLog` (#77). |
| `ui/src/main.js`, `ui/src/router.js`, `ui/src/store.js`, `ui/src/css/_main.css` | Webhooks page import, route, header link, stylesheet (#77). |
| `plugins/jsvm/internal/types/generated/types.d.ts` | `BackupsConfig.disableVerify` (regenerates with `make jstypes`) (#78). |
| `apis/middlewares.go` | `loadAuthToken` hands tokens starting with `clv_` to `loadAPIKeyAuth`; the JWT path below is untouched (#95). |
| `core/settings_model.go` | `settings.secretFields()` is now the single list of masked secrets, used by `Settings.MarshalJSON` and the settings audit diff (#110). |
| `core/collection_import.go` | `ImportCollections` delegates to the new `ImportCollectionsWithContext` (ctx reaches the saves/deletes, so imports are attributed) (#110). |
| `apis/settings.go`, `apis/collection.go`, `apis/collection_import.go` | `SaveWithContext/DeleteWithContext(auditContext(e), ...)` and `core.ImportCollectionsWithContext` so settings and schema changes carry the superuser and IP (#110). |
| `core/audit_model.go` | One call, `app.registerAdminAuditHooks()` (new file `core/audit_admin.go`) in `registerAuditHooks` (#110). |
| `apis/record_auth_verification_confirm.go`, `record_auth_email_change_confirm.go`, `record_auth_password_reset_confirm.go`, `record_auth_with_otp.go`, `record_auth_with_oauth2.go`, `record_auth_passkey_reset_confirm.go`, `record_auth_totp.go`, `record_auth_with_webauthn.go` | `SaveWithContext/DeleteWithContext(auditContext(As)(e, ...), ...)` instead of `Save/Delete` so auth-flow writes carry the actor and IP; unauthenticated flows use `auditContextAs` (self actor). `verifySecondFactor`/`consumeBackupCode` take a leading `ctx`. In `record_auth_with_oauth2.go` the unverified-link cleanup deletes external auths one by one (`DeleteWithContext`) instead of `DeleteAllExternalAuthsByRecord` (#110). |
| `apis/record_crud.go` | The create path uses `auditContextForCreate(e, collection)` (anonymous sign-up of an auth record is attributed to the new record, `meta.self`) (#110). |
| `core/external_auth_model.go` | The verified-upgrade cleanup deletes external auths with the event context (`DeleteWithContext(e.Context, ...)`) instead of `DeleteAllExternalAuthsByRecord`, so the unlinks keep the actor (#110). |
| `core/audit_model.go` | `AuditActor.Self` and one `resolveSelfAuditActor(entry, e.Record)` call in `auditNextAndWrite` (new file `core/audit_actor.go`) (#110). |
| `core/backup.go` | `registerAutobackupHooks` calls `registerBackupEncryptionHooks()` and the cron name goes through `BackupStoredName` (".enc" when encrypting) (#78). |
| `core/backup_create.go` | `CreateBackup` maps the name with `BackupStoredName`, loads the key source up front (a missing key fails before any file is written) and passes it to `createZip`/`newZipWriter`, which stream the zip through `backupenc.NewWriter` (the plaintext archive never touches the disk) (#78). |
| `core/backup_restore.go` | Both restore paths decrypt an encrypted backup into the temp dir with `copyBackupAsZip` / `decryptLocalBackupToTemp` before anything in `pb_data` moves (#78). |
| `core/settings_model.go`, `core/settings_model_test.go` | `BackupsConfig.Encrypt`. The exact-JSON assertions carry `"encrypt":false` (#78). |
| `apis/backup_create.go` | `backupNameRegex` accepts `.zip.enc`; the unique-name check uses the stored name; key errors (`core.ErrBackupEncryptionKey`) are shown in the 400 message (#78). |
| `apis/backup.go` | The list adds `encrypted` (name ends with `.enc`) (#78). |
| `apis/backup_upload.go` | `uploadedBackupType` accepts a `*.enc` file that starts with the backup magic, besides application/zip (#78). |
| `pocketbase.go` | `Start()` registers `cmd.NewBackupsCommand(pb)`; `skipBootstrap` returns true for the `backups` subcommands (offline file operation) (#78). |
| `ui/src/settings/backups/backupsForm.js`, `backupsList.js`, `backupCreateModal.js`, `backupUploadButton.js` | Encrypt switch with the key warning, lock icon and download note, `.zip.enc` name pattern, `.enc` accepted by the upload picker (#78). |
| `plugins/jsvm/internal/types/generated/types.d.ts` | `BackupsConfig.encrypt` (hand-added) (#78). |
| `apis/audit_test.go` | The anonymous create scenario expects the self actor instead of `system`; `seedAuditEntry` clears the entries written by the fixture's migrations (#110). |
| `core/settings_model.go`, `core/settings_model_test.go` | `Settings.Audit` (`AuditConfig.RetentionDays`, `0` = forever), validated in `PostValidate`. The exact-JSON snapshots in `TestSettings_DBExport` (both subtests) and `TestSettingsMarshalJSON` end with `,"audit":{"retentionDays":0}` (#110). |
| `core/audit_model.go` | `auditRetentionDays(app)`: `CLAVIGER_AUDIT_RETENTION_DAYS` wins when it is a valid non-negative number, otherwise `Settings.Audit.RetentionDays` (#110). |
| `ui/src/settings/application/pageApplicationSettings.js` | Imports and renders `auditAccordion` (new file), keeps `audit` in the form settings (#110). |
| `plugins/jsvm/internal/types/generated/types.d.ts` | `Settings.audit` and `AuditConfig` (hand-added, a full `make jstypes` regeneration churns every random type id) (#110). |
| `apis/base.go` | `bindAPIKeysApi(app, apiGroup)` (#95). |
| `apis/collection_test.go`, `apis/collection_import_test.go`, `core/collection_query_test.go` | System collection count 18 -> 19 for `_apiKeys` (#95). The conductor reconciles the count across agents. |
| `ui/src/router.js`, `ui/src/store.js` | `#/settings/api-keys` route and nav entry (#95). |
| `core/base.go` | `app.registerSessionHooks()` (#93). |
| `apis/middlewares.go` | `loadAuthToken`: `checkSessionClaim` after `FindAuthRecordByToken` (#93). |
| `apis/record_helpers.go` | `recordAuthResponse`: `checkNotBanned` before the MFA step, `attachSession` after it (#93). |
| `apis/record_auth_refresh.go` | `carrySession` keeps the sid and extends the session; `skipSessionAttach` for non-refreshable tokens (#93). |
| `apis/record_auth_password_reset_request.go` | `silentlyRefuseBanned` (#93). |
| `apis/record_auth_impersonate.go` | Required `reason`, superusers cannot be impersonated, `impersonatedBy` claim, session method `impersonate`, audit entry (#93). |
| `apis/record_auth_impersonate_test.go` | Success scenarios send a `reason` (#93). |
| `apis/collection_test.go`, `apis/collection_import_test.go`, `core/collection_query_test.go` | System collection count 19 -> 21 for `_sessions` and `_bans` (#93); the paging/sorting scenario now expects `_apiKeys` on page 2. The conductor reconciles the count across agents. |
| `ui/src/main.js`, `ui/src/records/recordUpsertModal.js`, `ui/src/records/recordImpersonateModal.js` | "Sessions and ban" menu entry and modal import; impersonation asks for a reason (#93). |
| `core/settings_model.go` | `Settings.Captcha` field, validated in `PostValidate`, `Captcha.SecretKey` masked in `MarshalJSON` (#91). Exact-JSON assertions in `core/settings_model_test.go` carry the `captcha` object. |
| `apis/record_auth.go` | `requireCaptcha(...)` bound after the rate limit on auth-with-password, request-otp, login-begin, request-(password\|passkey)-reset and request-verification (#91). |
| `apis/record_crud.go` | `recordCreate`: `checkCaptcha(e, CaptchaActionSignUp)` for guests creating auth records (#91). |
| `apis/record_auth_methods.go` | `authMethodsResponse.Captcha` (omitted when no action requires a captcha) (#91). |
| `apis/middlewares_rate_limit.go` | `newPersistentRateLimiter`, `rateLimiter.name/persist`, `rateClient.attach` and the `record` call in `consume`; `destroyRateLimitersStore` calls `forgetRateLimitStoreEntries` (#91). |
| `apis/serve.go`, `apis/extensions.go` | The admin UI CSP comes from `adminUICSP(e.App)` (equals `defaultCSP` unless the login requires a captcha) (#91). |
| `tools/telemetry/telemetry.go` | `captchaVerifications` counter (`claviger.captcha.verifications`) (#91). |
| `ui/src/router.js`, `ui/src/store.js`, `ui/src/auth/pageSuperuserLogin.js` | `#/settings/captcha` route and nav entry; captcha token on the superuser password and OTP request (#91). |
| `plugins/jsvm/internal/types/generated/types.d.ts` | Needs `make jstypes` for `Settings.captcha` (not regenerated on the #91 branch to avoid alias-name churn). |
| `core/base.go` | One call, `app.registerFieldEncryptionHooks()`, after `app.registerWebhookHooks()` (#75). |
| `core/record_field_resolver_runner.go` | One hunk in `finalizeActivePropsProcessing`, right after the hidden-field check: `checkEncryptedFieldFilterable(field)` rejects encrypted fields (`encryptedText`, `encryptedJSON`) in every filter, sort and API rule (#75). The only way to cover rules and relation traversal; client filters alone could be pre-validated in the handler, rules could not. |
| `core/base.go` | One call, `app.registerOrgHooks()` (file `core/org.go`), after `app.registerFieldEncryptionHooks()` (#79a). |
| `core/audit_model.go` | One call, `auditOrgMeta(entry, e.Record)` in `auditNextAndWrite` (`core/org.go`): `meta.orgId` / `meta.orgIds` (#79a). |
| `core/apikey.go` | `_orgs`, `_orgMembers`, `_orgRoles` added to `APIKeyForbiddenCollections` (#79a). |
| `apis/base.go` | `bindOrgsApi(app, apiGroup)` after `bindWebhooksApi` (#79a). |
| `apis/collection_test.go`, `apis/collection_import_test.go`, `core/collection_query_test.go` | System collection count +3 (`_orgs`, `_orgMembers`, `_orgRoles`, #79a); the paging scenario's page 2 now starts at `_orgs`, then `_fieldKeys`. The conductor reconciles the count across agents. |
| `ui/src/router.js`, `ui/src/store.js`, `ui/src/main.js`, `ui/src/css/_main.css` | `#/orgs` route and nav entry, form modal import, `orgs.css` import (#79a). |
| `core/base.go` | One call, `app.registerInvitationHooks()` (file `core/invitation.go`: validate hook and the daily expiry/cleanup cron `__pbInvitationCleanup__` at 05:13), after `app.registerOrgHooks()` (#79b). |
| `core/apikey.go` | `_invitations` added to `APIKeyForbiddenCollections` (#79b). |
| `apis/base.go` | `bindOrgsPages(app)` after `bindPasskeysPage(app)` (#79b). The invitation and tag routes are bound from `bindOrgsApi` (`apis/orgs.go`, fork-owned). |
| `apis/collection_test.go`, `apis/collection_import_test.go` | System collection count +1 (`_invitations`, #79b): total 36; the paging scenario now reads page 3 (page 1 is `_invitations` and one org collection, page 2 the other two, page 3 starts at `_fieldKeys`). The conductor reconciles the count across agents. |
| `core/audit_model.go`, `core/audit_admin.go` | `AuditActor.OrgId` (written to `meta.orgId`) and the `scim` actor type, in the two places that copy the actor onto an entry (#79c). |
| `core/apikey.go` | `_scimTokens`, `_scimGroups` added to `APIKeyForbiddenCollections` (#79c). |
| `apis/base.go` | `bindSCIMApi(app, pbRouter)` after `bindOrgsApi` (#79c): the `/scim/v2` routes, bound on the router so the default middlewares apply. |
| `apis/collection_test.go`, `apis/collection_import_test.go`, `core/collection_query_test.go` | System collection count +2 (`_scimTokens`, `_scimGroups`, #79c): total 38; the paging scenario now reads page 4 (page 1 is the two SCIM collections, page 2 `_invitations` and an org collection, page 3 the other two, page 4 starts at `_fieldKeys`). The conductor reconciles the count across agents. |
| `ui/src/orgs/pageOrgs.js` (fork-owned) | Invitations table, invite/resend/revoke and a link to the hosted org page (#79b). `ui/dist` is not rebuilt on the #79b branch. |
| `oidc/` (fork-owned) | `oidc/orgs.go` (new) plus small hunks in `claims.go`, `client.go`, `login.go`, `oidc.go`, `store.go`, `admin.go`: scope `orgs`, org-bound clients (#79a). Migration `1780001000_add_orgs.go` also adds `_oauthClients.org`. |
| `core/record_field_resolver_runner.go` | One hunk in `finalizeActivePropsProcessing`, right after the hidden-field check: `checkEncryptedFieldFilterable(field)` rejects `encryptedText` fields in every filter, sort and API rule (#75). The only way to cover rules and relation traversal; client filters alone could be pre-validated in the handler, rules could not. |
| `apis/record_crud.go` | `recordList`: `encryptedFieldFilterError(err, collection)` added to the `firstApiError(...)` call so a filter or sort on an encrypted field is a clear 400 (#75). The search package flattens resolver errors into strings. |
| `pocketbase.go` | `Start()` registers `cmd.NewKeysCommand(pb)`; `skipBootstrap` returns true for every `keys` subcommand: `keys check` never opens or migrates the database, and the maintenance commands (`rotate`, `rewrap`, `convert`, `strict`, `prune`, `status`) first look for a live server and then call `Bootstrap()` themselves (#75). |
| `apis/base.go` | One line in `NewRouter`: `bindFieldKeysApi(app, apiGroup)` (#75 M3). |
| `entrypoint.sh` | `preflight_field_keys`, called from `run_serve` before `acquire_single_writer` (#75, fork-owned file, prod-critical). Tests: `scripts/test_entrypoint_keys_preflight.sh`. |
| `core/audit_model.go` | `buildRecordAuditEntry` redacts every encrypted field (`encryptedText`, `encryptedJSON`) by type, via `IsEncryptedFieldType` (#75). |
| `ui/src/main.js`, `ui/src/records/recordsList.js` | `encryptedText` and `encryptedJSON` init imports; the table header ignores clicks on a field type with `sortable: false` (#75). |
| `apis/collection_test.go`, `apis/collection_import_test.go`, `core/collection_query_test.go` | System collection count 24 -> 25 for `_fieldKeys` (#75); the paging/sorting scenario expects `_sessions` and `_webhookDeliveries` on page 2. The conductor reconciles the count across agents. |
| `plugins/jsvm/internal/types/generated/types.d.ts` | Needs `make jstypes` for `EncryptedTextField`, `EncryptedJSONField` and the key source loader (not regenerated on the #75 branch: the generator rewrote about 16k unrelated lines). |

Fork-only files for #79b (invitations): `core/invitation.go`, `core/org_tags.go`, `migrations/1780001100_add_invitations.go`, `mails/invitation.go`, `apis/invitations.go`, `apis/orgs_tags.go`, `apis/orgs_pages.go`, `apis/orgs_pages/` (embedded hosted page assets), their tests (`core/invitation_test.go`, `apis/invitations*_test.go`, `apis/orgs_pages_test.go`) and the invitations section of `docs/orgs.md`. Sync checks: `grep -n registerInvitationHooks core/base.go`, `go test -run 'Invitation|OrgTags|OrgAdminPage|OrgPages|HostedPages' ./core/ ./apis/`.

Fork-only files for #75 (field encryption): `tools/keys/`, `core/field_keys.go`, `core/field_keys_ops.go`, `core/field_sealed.go`, `core/field_encrypted_text.go`, `core/field_encrypted_json.go`, `migrations/1780000800_add_field_keys.go`, `migrations/1780000900_field_keys_strict.go`, `apis/field_encrypted.go`, `apis/field_keys.go`, `apis/apikey_auth.go` (denies `/api/field-keys` to API keys), `cmd/keys.go`, `cmd/keys_rotate.go`, `cmd/keys_lock_unix.go`, `cmd/keys_lock_other.go`, `scripts/test_entrypoint_keys_preflight.sh`, `ui/src/fields/encryptedText/`, `ui/src/fields/encryptedJSON/`, `ui/src/settings/fieldkeys/`, `docs/field-encryption.md`. UI files outside those directories: `ui/src/main.js` (field type imports), `ui/src/router.js` and `ui/src/store.js` (the Encryption keys settings page, #75 M3). Sync checks: `grep -n registerFieldEncryptionHooks core/base.go`, `grep -n checkEncryptedFieldFilterable core/record_field_resolver_runner.go`, and `go test -run 'EncryptedText|EncryptedJSON|FieldKeys|KeysCheck|EncryptedField|RotateFieldKeys|Rewrap|Convert|Keys' ./core/ ./apis/ ./cmd/ ./tools/keys/`. The field relies on `Record.originalData` (read via `GetRaw` fallback, copied by `Original()`), `newRecordFromNullStringMap` calling `PrepareValue` after the `id` field, and `Record.dbExport` calling `DriverValuer`; the tests above catch a change in any of them.

Fork-only files for #78 (backup encryption): `tools/backupenc/` (the chunked AES-256-GCM container), `core/backup_encrypt.go` (+ `_test.go`), `cmd/backups.go` (+ `_test.go`), `apis/backup_encrypt_test.go`, `docs/backup-encryption.md`. The format is specified in the doc and pinned by `go test ./tools/backupenc`; the doc's Python decryptor was run against a Go-written file when it was written.

Fork-only files for #91: `core/settings_captcha.go`, `tools/captcha/`, `tools/telemetry/captcha.go`, `apis/captcha.go`, `apis/rate_limit_store.go`, `ui/src/auth/captcha.js`, `ui/src/settings/captcha/`. Fork-owned files touched: `apis/record_auth_with_webauthn_login_begin_limit.go` (its two limiters are persistent, rule names prefixed `webauthn:` so a Settings reload does not reset them), `apis/passkeys_page.go` (CSP builder, `getCaptchaToken` client option and widget loader), `apis/apikey_auth.go` and `apis/record_auth_totp.go` (unchanged; they reach the persistent store through `checkRateLimit`).

Fork-only files for #95: `core/apikey.go`, `migrations/1780000400_add_api_keys.go`, `apis/apikey_auth.go`, `apis/apikeys.go`, `ui/src/settings/apikeys/`. `core/audit_model.go` (fork-owned) gained `AuditActor.APIKeyId` and the `_apiKeys` lastUsed noise rule.

Fork-only files for #93: `core/session.go`, `core/ban.go`, `migrations/1780000600_add_sessions_bans.go`, `apis/sessions_auth.go`, `apis/sessions.go`, `ui/src/records/recordSessionsModal.js`. Fork-owned files touched: `core/apikey.go` (a banned owner's keys fail), `apis/apikey_auth.go` (`sessions` and `bans` routes are denied to keys), `apis/record_auth_with_webauthn_stepup.go` (the step-up keeps the sid) and `apis/record_auth_with_webauthn_discoverable.go` (session label).
| `apis/record_helpers.go` | `checkMFA` rewritten for per-account TOTP enforcement (`core.HasActiveTOTP`, passkey exemption, TOTP required as the second factor for enrolled accounts, zero-duration guard `mfaSessionDuration`); the 401 MFA body is built by `mfaRequiredBody` (adds `"totp": true`) (#90). |
| `apis/record_auth.go` | One line: `bindTOTPApi(sub)` (#90). |
| `core/base.go` | One line: `app.registerTOTPHooks()` (#90). |
| `cmd/superuser.go` | One line: `superuserTOTPDisableCommand` registered (#90). |
| `core/collection_query_test.go`, `apis/collection_import_test.go`, `apis/collection_test.go` | System collection counts 18 to 19 and `_totps` in the list (#90). Other batch-3 collections add to these counts too. |

Fork-only files for #78: `core/backup_verify*.go`, `core/diskfree_*.go`.

Fork-only files for #90 (TOTP): `tools/security/totp.go`, `core/totp_model.go`, `migrations/1780000300_add_totps.go`, `apis/record_auth_totp.go`, `cmd/superuser_totp.go`, `ui/src/settings/security/`. `core/audit_model.go` (fork file) skips the `lastStep` replay counter as noise.

### TOTP (authenticator app, #90)

Opt-in per account; deploying changes nobody's sign-in. Data lives in the `_totps` system collection (one row per auth record, every field hidden, no API rules). An enrolled account needs a TOTP code or an unused backup code after its first factor (password, OAuth2, email OTP), even when the collection's MFA is off; passkey sign-in is exempt. TOTP completes the upstream MFA session through `POST /api/collections/{c}/auth-with-totp {mfaId, code}`.

- **Secret storage.** If the settings encryption key env (`PB_ENCRYPTION_KEY`, i.e. `App.EncryptionEnv()`) is set when the secret is stored, it is AES-256-GCM encrypted (`security.Encrypt`) and stored as `enc:<base64>`; otherwise it is stored plain. Plain secrets stay readable after a key is configured. **An `enc:` secret cannot be read without that key**: if the key is removed or rotated, TOTP codes stop working for those accounts (backup codes still work, they are bcrypt hashes) until `superuser totp-disable` is run and the account re-enrolls. Do not rotate the key without planning that.
- **Hidden from the API.** The records API unhides every field for superusers, so `registerTOTPHooks` re-hides all `_totps` fields in an `OnRecordEnrich` handler. Keep that handler on sync: without it any superuser session can read every TOTP secret.
- **Recovery (lost authenticator and backup codes).** Anyone with shell access to the data directory runs `claviger superuser totp-disable <email>` (removes the enrollment and any pending MFA sessions), then signs in with the password and enrolls again. A superuser can also delete the row via the admin UI/API (`_totps`), audited. On Azure Container Apps, run it in the single replica with `/pb_data` mounted.
- **Pitfalls to re-check on sync:** the `checkMFA` hunk above, `grep -n registerTOTPHooks core/base.go`, `go test -run 'TOTP' ./apis/ ./cmd/ ./tools/security/`.
Fork-only files for #71 (OpenID Connect provider): `oidc/` (provider wiring `oidc.go`, `store.go`, `client.go`, `keys.go`, `claims.go`, `login.go`, `assets.go`, `admin.go`, `hooks.go`, `command.go` and tests), `core/oauth_collections.go`, `migrations/1780000700_add_oidc.go`, `docs/oidc.md`, `ui/src/oidc/`, `ui/src/css/oidc.css`. #114 adds `oidc/ratelimit.go`, `oidc/sso.go`, `migrations/1780000710_add_oidc_sso.go`, `apis/captcha_export.go`, `apis/rate_limit_fixed.go` (`apis.FixedWindowLimiter`, the exported face of the persistent limiter; `rate_limit_store.go` treats its `ext:` rules like `webauthn:` ones, which a Settings reload does not reset), `docs/oidc-conformance.md`, `scripts/oidc-conformance-instance.sh` and `.github/workflows/oidc-conformance.yml` (manual only). The provider routes must use explicit methods, never `Router.Any`: a method-less pattern conflicts with the `GET /{path...}` catch-all of `examples/base` and panics the mux at start (`newEnv` in `oidc/helpers_test.go` registers that catch-all so a regression fails the tests). The provider imports `apis` (`SessionIdFromRequest`, `HasFreshPasskey`, `RequireSuperuserAuth`), so it is registered from `pocketbase.go`, not from `apis/base.go` (that would be an import cycle).

Fork-only files for #77 (webhooks): `core/webhooks*.go`, `migrations/1780000500_add_webhooks.go`, `apis/webhooks*.go`, `tools/telemetry/webhooks.go`, `ui/src/webhooks/`, `ui/src/css/webhooks.css`, `docs/webhooks.md`. Sync checks: `grep -n registerWebhookHooks core/base.go` and `go test -run Webhook ./core/... ./apis/...`. The enqueue handler is on `OnAuditEntry`, so it depends on the audit guards above staying intact; `OnRecordEnrich` on `_webhooks` re-hides the secrets after the records API un-hides every field for superusers.

### Postgres dialect seam (#106, experimental)

Fork-only files: `tools/dialect/` (the `Dialect` interface and the SQLite and Postgres implementations, `postgres_search.go` for the M2 filter/JSON/strftime SQL), `tools/search/dialect.go` and `tools/search/token_functions_dialect.go` (the filter layer's dialect hooks and the Postgres `strftime()`/`geoDistance()`), `core/record_field_resolver_dialect.go`, `core/view_dialect.go`, `core/validators/db_postgres.go` (+ `_stub.go`), `apis/record_crud_dialect.go`, `core/dialect.go`, `core/db_connect_postgres.go` (+ `_stub.go` for the `no_default_driver` tag), `core/db_connect_aux.go`, `tests/pgtests/`, `scripts/pgembedded/` (separate Go module), `docs/datastores.md`, the `postgres` job in `test.yml`. Design: `docs/spikes/106-postgres.md`.

The rule: SQLite behavior and SQL text are unchanged, and every engine-specific piece asks the dialect (`core.DialectOf(app)`, `dialect.Of(db)`). The hunks in upstream-owned files are one-line dialect calls except where noted. After a sync, re-check each file below, and `grep -rn 'sqlite_master\|sqlite_schema\|PRAGMA\|randomblob\|strftime\|rowid' --include='*.go' core apis migrations tools | grep -v _test` for raw SQL upstream added (it needs a dialect call or a Postgres equivalent). Then run `go test -tags postgres ./tests/pgtests/...` (see `docs/datastores.md`).

| File | Fork change | Re-check on sync |
| --- | --- | --- |
| `core/base.go` | `BaseAppConfig.AuxDBConnect` (appended field); `NewBaseApp` calls `applyEnvDBConfig` before defaulting `DBConnect`; `initDataDB` opens a second (write) pool only when `dialect.SeparateWritePool()`; `initAuxDB` uses `app.auxDBConnect()`; the `__pbDBOptimize__` cron calls `Checkpoint`/`Optimize` of the dialect instead of `PRAGMA`. About 25 lines. | New fields in `BaseAppConfig`, other `PRAGMA` calls. |
| `core/db_table.go` | `TableColumns`, `TableInfo`, `TableIndexes`, `hasTable` and `vacuum` delegate to the dialect (the SQLite bodies moved to `tools/dialect/sqlite.go` unchanged). `TableInfoRow` is now an alias of `dialect.TableInfoRow` (declared in `core/dialect.go`). | The moved SQLite queries still match upstream's. |
| `core/field_*.go` (13 files) | `ColumnType()` returns a dialect column definition (`dialectOf(app).TextColumn()` etc.). One line each, two in the fields with a single/multiple variant. | A new field type or a changed column type: update `tools/dialect` for both engines. |
| `core/collection_record_table_sync.go` | Hidden columns on create; `Optimize` instead of `PRAGMA optimize`; `ConvertMultiplicity` hook ahead of the SQLite view-drop strategy; `createCollectionIndexes` renders the index with `BuildIndex` inside `Try`. About 20 lines. | The single/multiple conversion and index sync logic. |
| `core/collection_validate.go` | The "index name used by another collection" probe calls `IndexOwner`. | |
| `core/collection_query.go` | `FindAllCollections` orders by `RowOrder()` (`rowid ASC` on SQLite). | |
| `core/migrations_runner.go` | The `_migrations.applied` type comes from `Int64Type()` (Postgres INTEGER is 32 bit); `Up` takes the migrations advisory lock first. | |
| `migrations/1640988000_init.go` | `_collections` and `_params` DDL assembled from dialect fragments (byte for byte the old SQL on SQLite); `hiddenColumnsDDL`. | Column list of `_collections` / `_params`. |
| `migrations/1778828400_normalize_indexes.go` | Returns early unless SQLite. The v0.23 migrations already return early on a fresh database. | |
| `go.mod`, `go.sum` | `github.com/jackc/pgx/v5` (pinned) and its dependencies. | |

**M2 (records, filters, sorting, views).** `tools/dbutils/json.go` and `tools/search/token_functions.go` are untouched: the SQLite dialect calls the former and the SQLite filter functions stay the reference. The Postgres filter SQL comes from the dialect; the resolver's app selects it (`RecordFieldResolver.Dialect()`, `search.DialectProvider`). Upstream-owned hunks:

| File | Fork change | Re-check on sync |
| --- | --- | --- |
| `tools/search/filter.go` (+39 -21) | `buildResolversExpr` and `resolveEqualExpr` take the dialect (first argument); `IS`/`IS NOT`, `LIKE`, the text cast of the operands and `COALESCE` go through it (byte for byte the old SQL on SQLite, pinned by `dialect_sqlite_golden_test.go`); `adaptJSONOperands` runs first; the `true`/`false` identifiers and filter functions are looked up through the dialect; the multi-match structs carry the dialect and the `JSONB` flag. | **A new operator, modifier or `TokenFunctions` entry upstream needs a Postgres decision** (a function that is not in `builderTokenFunctions` runs its SQLite SQL on Postgres). Any new raw SQL in `resolveEqualExpr`/`buildResolversExpr`. |
| `tools/search/sort.go` (+6 -1), `provider.go` (+9), `multi_match_subquery.go` (+3), `simple_field_resolver.go` (+4) | `OrderTerm` / `JSONOrderTerm` for the sort terms; the default order hook (`DefaultOrderProvider`); `ON TRUE` for joins without a condition; `ResolverResult.JSONB`. | New fields on `ResolverResult`, new sort keywords, changes to how `Exec` builds the count query (it must still reset `Distinct`, `GroupBy` and `OrderBy`, which the Postgres `GROUP BY` rewrite relies on). |
| `core/record_field_resolver_runner.go` (+36 -16) | Every `dbutils.JSONEach/JSONExtract/JSONArrayLength` and `json_each(...)` goes through `r.resolver.Dialect()`; the `JSONB` flag on JSON results; multiple values compared as text (`AsText`); the `:lower` modifier. | **Any new `dbutils.*` or `json_*` call in the runner.** New modifiers. |
| `core/record_field_resolver.go` (+3 -8) | The join loops call `r.leftJoin`; `updateQueryWithDeduplicateConstraint` calls `r.groupJoinedOrderBy` (Postgres: `DISTINCT` plus a joined `ORDER BY` becomes `GROUP BY id` with `ARRAY_AGG` sort terms). | The dedupe logic, `UpdateQuery`. |
| `core/record_query.go` (+6 -1) | `NoCaseEqual` for the email lookup; the default order in `FindRecordsByFilter`. | |
| `core/view.go` (+8 -5), `core/collection_query.go` (+1 -1) | `SubqueryAlias()` after the derived tables of `SaveView`, `DryRunView` and `normalizeViewQueryId`; field type inference for unknown view columns (`defaultViewFieldForColumn`). | The view query wrapping, `CreateViewFields`. |
| `core/record_model.go`, `core/record_query_expand.go`, `core/field_text.go` | `dialectOf(app).JSONEach(...)`, `NoCaseEqual(...)`. One line each. | |
| `core/validators/db.go` (+5) | `NormalizeUniqueIndexError` first asks `normalizePostgresUniqueError` (the pgx unique violation detail names the columns). | |
| `apis/record_crud.go` (+1 -1), `apis/record_auth_with_password.go`, `apis/record_auth_with_oauth2.go` | `dummyRecordSelect` (typed dummy record of the create rule); `NoCaseEqual`. | New `COLLATE NOCASE` or `SELECT {:p} AS col` constructs. |
| `tools/dialect/postgres.go` | The `C` collation of text columns. | |

After a sync, also run `go test ./tools/search -run 'Golden|PostgresFilterSQL'` (SQL text of both dialects), `go test -tags postgres ./tests/pgtests/...` (the differential suite) and grep for the constructs above. `core/log_query.go` is the auxiliary db and stays SQLite.

**M3 (migrations, import/export, backups) and M4 (fork features, test harness).** Fork-only files: `apis/sql_dialect.go`, `tests/app_postgres.go`, `tests/postgres_harness_test.go`, `tests/postgres_skip.txt`, `scripts/test_postgres_suites.sh`, `tests/pgtests/{sql_api,backup,import_export}_test.go`. Upstream-owned hunks:

| File | Fork change | Re-check on sync |
| --- | --- | --- |
| `apis/sql.go` (+22 -3) | `isWriteQuery` / `sqlTouchesAuditLog` (apis/sql_dialect.go) replace the prefix guess and the audit substring check; the write path is a closure (`runWrite`) so a Postgres read that the server refuses as a write (SQLSTATE 25006) can take it; reads go through `runReadQuery` (a READ ONLY transaction on Postgres). The SQLite statements are unchanged. | New write prefixes in `knownWriteQueryPrefixes` (add to `postgresWriteKeywords` too); the audit guard text. |
| `apis/backup.go` (+7) | `backupRestore` answers 400 when the database is not SQLite. | |
| `apis/health.go` (+2 -1) | `data.database` for superusers (the admin UI backups notice). | |
| `apis/collection.go` (+1 -1) | `collectionsList` gives its `SimpleFieldResolver` the dialect (`WithDialect`): without it `system=false` is sent as a number to a boolean column. | New `SimpleFieldResolver` users on tables other than the aux db. |
| `tools/search/simple_field_resolver.go` (+29) | `WithDialect`, `Dialect()`, `DefaultOrder()` on the resolver. | |
| `core/backup_create.go` (+41 -22) | The `VACUUM INTO` copy of `data.db` (and its manifest/exclude lines) is the `else` branch of a dialect check, so the block is **re-indented**: use `git diff -w` when merging. On Postgres the data database is skipped and the manifest gets `dataDatabase`; the `PRAGMA wal_checkpoint` of the data db is `Checkpoint()`. | Changes inside the data.db copy block; a new database file added to the archive. |
| `core/backup_restore.go` (+7) | Refuses when the database is not SQLite. | |
| `core/backup_verify.go` (+31 -1, fork-only file) | `BackupManifest.DataDatabase`/`Note`; a Postgres archive without `data.db` is `skipped`. | |
| `core/view.go` (+10 -3), `core/db_table.go` (+2 -1) | `DropSuffix()` on `DROP VIEW` / `DROP TABLE`; `SaveView` detaches and recreates the dependent views (`DetachDependentViews`). | Any other `DROP` of a table or view. |
| `core/field_encrypted_*.go`, `core/telemetry.go`, `core/db_connect_postgres*.go`, `cmd/keys.go`, `tools/telemetry/db.go` | Fork-only files: the plain text column from the dialect; the instrumented Postgres connection; `keys check` reads Postgres (`CLAVIGER_DB_URL`) read only; `db.system.name` follows the driver. | |
| `migrations/1640988000_init.go` (+8 -1) | `_params.value` is `TEXT` on Postgres (an encrypted settings value is not JSON). | |
| `tools/dialect/postgres.go` | `PortableIndex` (`TableIndexes` returns the backtick form); `ConvertMultiplicity` leaves a column that already has the target type alone. | |
| `tests/app.go` (+14) | `pgCleanup` on `TestApp`, and `preparePostgres(&config, srcDataDir, tempDir)` ahead of `NewBaseApp`. The tests dir is upstream's helper package. | A rewritten `NewTestAppWithConfig`. |
| `.github/workflows/test.yml` | The `postgres` job runs `scripts/test_postgres_suites.sh` after `tests/pgtests`. | |
| `ui/src/settings/backups/pageBackupsSettings.js` (+41 -1), `ui/dist` | The Postgres notice (`health.data.database`). `ui/dist` rebuilt. | |

Upstream test files were touched only for fork-owned behaviour: none of the upstream tests changed (the SQLite specific ones are in `tests/postgres_skip.txt`). Fork test files that wrote SQLite SQL now use `{{table}}` / `[[column]]` placeholders, `core.DialectOf(app).Checkpoint` and `tests.CompactJSON` (JSONB renders `{"a": 1}`). After a sync run `scripts/test_postgres_suites.sh`: a new upstream test that asserts SQLite SQL goes into `tests/postgres_skip.txt` with its reason (or is fixed), never into a silent skip.

**M5 (`claviger migrate-db`) and M6 (CI matrix, skip list).** Fork-only files: `cmd/migrate_db.go`, `cmd/migrate_db_verify.go`, `cmd/migrate_db_test.go`, `scripts/test_migrate_db_smoke.sh`, `scripts/pgdb/` (create or drop a database without psql). Upstream-owned hunks:

| File | Fork change | Re-check on sync |
| --- | --- | --- |
| `pocketbase.go` (+9) | `Start()` registers `cmd.NewMigrateDBCommand`; `skipBootstrap()` returns true for `migrate-db` (it opens the SQLite source read only and bootstraps the target itself, so the eager bootstrap must not migrate either). | A rewritten `Start()` or `skipBootstrap()`. |
| `core/field_{autodate,date,editor,email,file,geo_point,json,number,password,relation,select,text,url}_test.go` (13 files, one line each) | `ColumnType` tests compare against `tests.ColumnType(expected)`, which translates the SQLite definition for Postgres. | An upstream change of a field's column type: update the expected string, and `tools/dialect` for both engines. |
| `core/collection_query_test.go`, `core/db_test.go`, `plugins/jsvm/binds_test.go` (one line each) | The statement text is compared through `tests.NormalizeQuotes` (Postgres quotes with `"`). | |
| `core/view_test.go` | The temp view check asks `information_schema.views` on Postgres. | |
| `tests/app_postgres.go` | `ColumnType` and `NormalizeQuotes` helpers. `adaptFixtureValue` mirrors `adaptSQLiteValue` in `cmd/migrate_db.go` (the `tests` package cannot import `cmd`: its tests import `tests`). | Change both together. |
| `tests/postgres_skip.txt` | 35 entries down to 16. | A new upstream test that asserts SQLite SQL goes here with its reason. |
| `.github/workflows/test.yml` | The `postgres` job is a matrix (`pg`: 16.15, plus 17.11 off pull requests) and has the migrate-db smoke step. The service image tags are bumped by hand. | The job name changed to `postgres <version>`: update required status checks if any name `postgres`. |
| `scripts/pgembedded/main.go` | `EPG_VERSION` (16 or 17). | |

`migrate-db` reads the stored `_collections` rows and the system migration list (`core.SystemMigrations`): after a sync that adds a system migration or changes how `ImportCollections` treats system collections, run `go test ./cmd -run MigrateDB` against a Postgres (`CLAVIGER_TEST_PG_URL`). The `_migrations`, `_params` and `_collections` handling in `copyTable` / `copyMigrations` follows the upstream table layout.

## CI

| Workflow | File | Runs on |
| --- | --- | --- |
| `Test` | `.github/workflows/test.yml` | PRs to `master`, pushes to `master`, manual. `go test`, golangci-lint, Docker build, `/api/health` smoke test, handover scenarios. PR runs cancel when superseded; master runs never do. |
| `basebuild` | `.github/workflows/release.yaml` | Version tags `v*` and manual only. Builds the UI, runs tests and GoReleaser (a snapshot unless the ref is a tag). |
| `Check Upstream Releases` | `.github/workflows/upstream-check.yml` | Mondays 09:00 UTC and manual. Files an `upstream-sync` issue when upstream has a newer tag than Sync Status. |

A workflow only runs if its file exists on the branch being pushed. Keep that in mind if you ever move a workflow file.

## History

Condensed. The detailed per-sync notes from the rebase era were dropped when the model changed to merge-based sync.

- **v0.41.1** (2026-10-30): first sync from the new host. v0.41.0 renamed upstream's module to `ganigeorgiev.com/pocketbase/pocketbase` (and moved `dbx`, `ozzo-validation/v4`, `tygoja`, `fexpr`), so all Go imports, the Dockerfile and `.goreleaser.yaml` ldflag were rewritten. Adopted: `Event.Extra`, `ServeEvent.StartBannerFunc`, thumbs `PB_THUMBS_MAX_SOURCE_PIXELS` default 100<<20, mailer fixes (upstream #7871), removal of the goja `nullObject` workaround. `plugins/update` now fetches from the new host.
- **v0.40.6** (2026-10-23): patch. Raised the minimum Go patch version to 1.27.3 (`release.yaml`).
- **v0.40.5** (2026-10-14): patch. The resolved Go became 1.27.2, so the lint pin moved to golangci-lint v2.14.0 (v2.13.2 was built with Go 1.27.0 and could not read export data).
- **v0.40.4** (2026-09-28): bumped `go-webauthn/webauthn` v0.18.0 to v0.18.2. Upstream's logger-deadlock fix (#7836) added `OnBootstrapClear`/`ClearBootstrap()`; the fork's OTLP sink-failure tally moved inside `runLogsWrite` in `core/base.go`, and the fork's `OnTerminate` flush was replaced by upstream's hook.
- **v0.40.1 and earlier**: WebAuthn layered on a rebased `feat/webauthn-passkey-support` branch, with `deploy/azure` rebuilt on top. The lint action moved to `golangci-lint-action@v9` because upstream renamed `gomodguard` to `gomodguard_v2`.
- **Branch model change** (#100): the two-branch rebase/rebuild flow was replaced by one trunk and merge-based sync. The module and binary were renamed to `github.com/doodlemania2/claviger` and `claviger` (the image keeps a compat symlink `/usr/local/bin/pocketbase`).

**M7 (several replicas on Postgres, `CLAVIGER_MULTI_REPLICA`).** Fork-only files: `tools/replica/` (the LISTEN/NOTIFY bus, the shared tables, advisory-lock helpers; `listen_pgx.go` is the only file that imports pgx and is excluded by `no_default_driver`), `core/multireplica.go`, `core/multireplica_pg_test.go`, `apis/multireplica_state.go`, `apis/realtime_replica.go`, `tests/pgtests/multireplica_test.go`. The mode is off unless the env var is set and refuses to start on SQLite; with it off every hook is inert. Nothing was added to `core.App`: the bus is read through `core.ReplicaBusOf(app)` (a type assertion on `*BaseApp`). Upstream-owned hunks:

| File | Fork change | Re-check on sync |
| --- | --- | --- |
| `tools/cron/cron.go` (+30), `tools/cron/job.go` (+15) | `Cron.SetRunGuard(RunGuard)`; `runDue` and `Job.Run` call the guard instead of the job when one is set. Jobs keep a `*Cron` back reference. With no guard the code path is the upstream one. | A rewritten `runDue`, `Add` or `Job.Run`. |
| `core/base.go` (+18) | `BaseAppConfig.DBURL` and `.MultiReplica`, `BaseApp.replicaBus`, `applyEnvMultiReplica` in `NewBaseApp`, `initReplicaBus()` in `Bootstrap` after `initLogger`, `registerMultiReplicaHooks()` next to `registerNotifyWatcherHooks()`. | `Bootstrap` order, new config fields. |
| `core/migrations_runner.go` (+16) | `initMigrationsTable` runs inside a transaction holding the migrations advisory lock on Postgres (`CREATE TABLE IF NOT EXISTS` is not atomic). The SQLite statement is unchanged. | A rewritten `initMigrationsTable`. |
| `core/settings_query.go` (+10) | `ReloadSettings`: if the insert of the default settings loses a race with another replica (multi replica only), it loads the winner's row. | A rewritten `ReloadSettings`. |
| `apis/middlewares_rate_limit.go` (+14) | `rateLimiter.shared` and the first lines of `isAllowed`: the shared counter in Postgres, then fall back to the local one. `apis/rate_limit_fixed.go` (fork-owned) mirrors it in `Blocked`. | A rewritten `rateLimiter` or `isAllowed`. |
| `apis/realtime.go` (+1) | `bindRealtimeReplica(app)` after `bindRealtimeEvents(app)`. | |
| `apis/record_auth_with_oauth2_redirect.go` (+8) | When the client is not on this replica, forward the redirect over the bus. | A rewritten redirect handler. |
| `core/session.go`, `core/webhooks.go`, `core/webhooks_deliver.go`, `core/field_keys_ops.go`, `apis/record_auth_with_webauthn.go` (fork-owned) | Session cache publishes invalidations; webhook endpoint change event and `claimDue` (`FOR UPDATE SKIP LOCKED`); `lockFieldKeyOp(app)` takes a database lock and publishes a reload; WebAuthn ceremony sessions are shared. | |
| `entrypoint.sh` (fork-owned, prod-critical) | `multi_replica_on_postgres()` makes `acquire_single_writer` and `start_handover_watcher` return early. The SQLite path is untouched. Tests: `scripts/test_entrypoint_handover.sh` T11 and T12. | |
| `.github/workflows/test.yml` | "Run the multi-replica suite" step in the `postgres` job. | |

After a sync: any new `app.Cron().Add` that cleans **replica local** state (the local `auxiliary.db`, in-memory maps) must be added to `localCronJobs` in `core/multireplica.go`, or it will run on one replica only. Any new in-process cache that another replica can invalidate needs an event (see `registerReplicaHandlers`). `grep -rn 'Store()\.\(Set\|GetOrSet\)' --include='*.go' core apis oidc | grep -v _test` lists the process-local state to review.
