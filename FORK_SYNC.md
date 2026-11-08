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
| `core/app.go`, `core/base.go` | LOW/MEDIUM | Our hook methods and fields. `core/base.go` also has the OTLP `slog` tee and the logger sink-failure tally inside `runLogsWrite`. |
| `core/events.go`, `core/record_tokens.go`, `pocketbase.go` | LOW | Mostly import-order conflicts. `core/record_tokens.go` also carries `TokenClaimPasskeyAt` and `NewPasskeyAuthToken` (passkey step-up, #73). |
| `tests/app.go`, `tests/api.go` | LOW | Race fix (#49): `EventCallsSnapshot()` in `tests/app.go`, and `tests/api.go` reads events through it instead of `EventCalls`. Kept in the fork, not sent upstream. If upstream rewrites those reads, keep the snapshot call. |
| `apis/record_auth.go` | LOW | Our route bindings, including the discoverable login and step-up routes and `RequireFreshPasskey` on credential delete (#73). |
| `apis/record_auth_methods.go` | LOW | `webauthn` response gained `discoverable` and `passkeyOnly` (#73). |
| `core/collection_model_test.go`, `plugins/migratecmd/migratecmd_test.go` | LOW | JSON snapshots: keep the `webauthn` field (incl. `discoverable`) and any new upstream `*ResetToken`/`*Template` fixture keys. |
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
