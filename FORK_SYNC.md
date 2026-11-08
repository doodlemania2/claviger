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
| `apis/record_auth.go` | LOW | Our route bindings, including the discoverable login and step-up routes and `RequireFreshPasskey` on credential delete (#73). |
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
- [ ] **Audit log guards.** `core/audit_model.go` must stay intact after a merge: the `__pbAuditGuard__` model hooks (create only with the unexported writer context, never update/delete), the `_auditLog` collection update/delete guards, and the `OnRecord{Create,Update,Delete}Execute` capture hooks at priority 100 (they must stay *inside* upstream's priority-99 execute handlers). Run `go test -run 'Audit' ./core/... ./apis/...`. If upstream adds another write path for records that bypasses `Save`/`Delete`, it bypasses the audit too.
- [ ] **API keys.** `loadAuthToken` in `apis/middlewares.go` must keep the `core.IsAPIKeyToken` branch ahead of `FindAuthRecordByToken`, and `enforceAPIKeyScopes` (`apis/apikey_auth.go`) denies unknown routes unless the key has `*`: if upstream adds an auth flow or account-changing route, add it to the deny list there. Run `go test -run 'APIKey' ./apis/...`.
- [ ] **Sessions and bans (#93).** Four small hunks in upstream-owned code call into `apis/sessions_auth.go`: `loadAuthToken` (`checkSessionClaim`), `recordAuthResponse` in `apis/record_helpers.go` (`checkNotBanned` before the MFA step, `attachSession` after it), `recordAuthRefresh` (`carrySession`, `skipSessionAttach`) and the password reset request (`silentlyRefuseBanned`). Keep `attachSession` *after* `checkMFA`: a session must exist only once the sign-in is complete. `core/base.go` calls `registerSessionHooks`. Run `go test -run 'Session|Ban|Legacy|Impersonate' ./apis/...`. Tokens without a `sid` are deliberately not checked (they are the pre-deploy tokens); do not "fix" that.
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
Fork-only files for #77 (webhooks): `core/webhooks*.go`, `migrations/1780000500_add_webhooks.go`, `apis/webhooks*.go`, `tools/telemetry/webhooks.go`, `ui/src/webhooks/`, `ui/src/css/webhooks.css`, `docs/webhooks.md`. Sync checks: `grep -n registerWebhookHooks core/base.go` and `go test -run Webhook ./core/... ./apis/...`. The enqueue handler is on `OnAuditEntry`, so it depends on the audit guards above staying intact; `OnRecordEnrich` on `_webhooks` re-hides the secrets after the records API un-hides every field for superusers.

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
