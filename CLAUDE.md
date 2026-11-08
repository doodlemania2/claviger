# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this repo is

**Claviger** (Latin, "key-bearer"): a public, MIT-licensed fork of PocketBase (Go backend with embedded SQLite, realtime subscriptions, file/user management, and an embedded Svelte admin UI) that leads with identity, aimed at developers and at congregations. Roadmap: GitHub issues #71–#98. Brand assets and guidelines: `brand/`. On top of upstream it carries two custom features, both part of `master`:

1. **WebAuthn/Passkey auth** (`apis/record_auth_with_webauthn.go`, `core/webauthn_*`, the `1712966400_add_webauthn.go` migration)
2. **Container packaging** (Dockerfile, `entrypoint.sh` with the single-writer handover, `litestream.yml`, docker-compose, OTLP log export, CI)

**This repo is public. Nothing deployment-specific belongs here**: no tenant IDs, domains, resource names, alert rules or runbooks for a particular installation. The production deployment at auth.stfoafrisco.org lives in the private repo `doodlemania2/stfoa-auth` (`/Volumes/Data/repos/stfoa-auth`), which builds a ref of this repo; its CLAUDE.md holds the operational rules (NFS, alerting, Litestream state, the Outline Task Inbox protocol). The split mirrors TDPdf / tdpdf-fleet.

`master` is the one trunk. See `FORK_SYNC.md` for the sync runbook and current sync status.

## Commands

```bash
make test          # go test ./... -v --cover
make lint          # golangci-lint run -c ./golangci.yml ./...
make test-report   # tests with HTML coverage report
make jstypes       # regenerate jsvm TS type declarations
```

- Single test: `go test -run TestRecordAuthWithWebAuthnRegisterBegin ./apis/...`
- WebAuthn suite: `go test -run 'WebAuthn|Webauthn' ./apis/... ./core/...`
- Run the app locally: `cd examples/base && go run main.go serve` (serves on http://localhost:8090 with the embedded admin UI)
- The release binary and image binary are named `claviger` (`/usr/local/bin/claviger`, with a compat symlink `/usr/local/bin/pocketbase`).
- Requires Go 1.27+.

### Admin UI (`ui/`)

Svelte + Vite SPA, embedded into the Go binary via `//go:embed all:dist` in `ui/embed.go` (excludable with `-tags no_ui`). Requires Node 24+.

```bash
cd ui
npm install
npm run dev      # dev server on :5173, expects backend on :8090 (override via ui/.env.development.local)
npm run build    # regenerates ui/dist
```

`ui/dist` is committed. After any UI change: `npm run build`, then commit the regenerated `ui/dist`.

## Architecture

### Package layout

- `core/` — the engine: the `App` interface (`core/app.go`) and `BaseApp` implementation, `Collection`/`Record`/`Settings` models, DB layer, event definitions, system migrations runner.
- `apis/` — HTTP route handlers, bound in `apis/base.go` (records CRUD, auth flows including WebAuthn, collections, settings, files, backups, batch, realtime).
- `forms/` — validation/submit structs for record and collection upserts and auth.
- `tools/` — standalone utilities: `hook/` (the event system), `router/`, `auth/` (OAuth2 providers), `filesystem/`, `mailer/`, `cron/`, `subscriptions/` (realtime broker), `search/` (filter/sort parsing), `security/`.
- `plugins/` — `jsvm/` (Goja-based JS hooks runtime; hooks loaded from a `pb_hooks` dir), `migratecmd/` (user migration CLI), `ghupdate/`.
- `migrations/` — system schema migrations (the WebAuthn collection lives in `migrations/1712966400_add_webauthn.go`).
- `cmd/` — cobra commands: serve, superuser, version.
- `tests/` — test helpers: test app factory and the `ApiScenario` harness.
- `pocketbase.go` — top-level `PocketBase` struct wiring `BaseApp` + cobra CLI; `examples/base/main.go` is the canonical full setup with all plugins registered.

### Request lifecycle

Request → `tools/router` middleware chain (CORS, rate limit, token auth via `apis.RequireAuth` etc., security headers) → handler in `apis/` receives a `*core.RequestEvent` (carries `App`, `Auth`, request info) → handler typically drives a `forms/` submit which triggers the model hook chain → response.

### Hook/event system

Everything extends through `tools/hook` generic hooks exposed on the `App` interface: `OnModel{Validate,Create,CreateExecute,AfterCreateSuccess,...}`, `OnRecord*`/`OnCollection*` (typed proxies of the model hooks), `OnRecordAuthWith{Password,OAuth2,OTP,WebAuthnRequest}`, `OnBootstrap`, `OnServe`, `OnTerminate`. Handlers form a middleware chain — each must call `e.Next()` to continue. The jsvm plugin mirrors these hooks into JavaScript via reflection (`plugins/jsvm/binds.go`); `plugins/jsvm/binds_test.go` asserts exact bind counts, so adding a hook to `core.App` breaks that test until updated.

### Database layer

- Driver is **`modernc.org/sqlite`** (pure Go), not upstream's default mattn/go-sqlite3. `modernc_versions_check.go` pins expected sqlite/libc versions — keep it in sync when bumping.
- Two databases per instance: `data.db` (app data, via `app.DB()`) and `auxiliary.db` (logs/internal, via `app.AuxDB()`), each with separate concurrent/nonconcurrent `dbx` connections. WAL mode and pragmas are set in `core/db_connect.go`.
- Models implement `core.Model` (`TableName()`, `IsNew()`); `Collection` defines schema + API rules, `Record` is a dynamic row with rule-aware JSON export.

### Test conventions

API tests are table-driven `tests.ApiScenario` slices (method, URL, body, auth header, expected status/events/body substrings) executed against a throwaway test app from `tests/`. Core tests exercise models directly. Run `make test` before pushing — CI runs the full suite plus golangci-lint and a Docker build/health-check smoke test.

## Fork maintenance (critical)

- **Remotes**: `origin` = doodlemania2/claviger (push), `upstream` = `https://ganigeorgiev.com/git/pocketbase/pocketbase` (fetch only, push URL `DISABLED`). **Upstream left GitHub after v0.40.6** (2026-10-26); the GitHub repo is dead and `github.com/pocketbase/pocketbase` fetches nothing newer. **v0.41.0 renamed upstream's Go module** to `ganigeorgiev.com/pocketbase/pocketbase` (with `dbx`/`ozzo-validation`/`tygoja`/`fexpr`, which this fork still imports from there). **This fork's own module path is `github.com/doodlemania2/claviger`**; fork Go files import `github.com/doodlemania2/claviger/...`. After merging an upstream tag, run `scripts/rewrite-module-path.sh` (idempotent) to rewrite upstream's path to ours. The `Dockerfile` and `.goreleaser.yaml` `-X github.com/doodlemania2/claviger.Version=` ldflag must use the same path: a stale `-X` package is **silently ignored**, so the binary would report the default version. Recipe: `FORK_SYNC.md`.
- **Branch model**: one trunk, `master`. All work lands on it by PR. Upstream is synced by `git merge <upstream tag>` on a `sync/<tag>` branch, then a PR to `master` merged with a merge commit (no rebase, no force-push, no patch replay). `feat/webauthn-passkey-support` is retired and frozen (left in place, not deleted). `deploy/azure` is only the **release pointer**: the private repo `doodlemania2/stfoa-auth` deploys it by default, so fast-forward it (`git push origin <tested-master-sha>:deploy/azure`) only to a `master` commit that is tested and approved for prod. Never commit to it directly. Full procedure in `FORK_SYNC.md`; update its Sync Status section after each sync.
- **Token redaction**: `core/collection_model.go` `MarshalJSON` must blank the `Secret` of every `*ResetToken`-style auth token config (`PasskeyResetToken` is ours). A missing `alias.X.Secret = ""` line is not silent. `TokenConfig.Secret` is `json:"secret,omitempty"`, and the auth case of `TestCollectionSerialize` (`core/collection_model_test.go`, `notExpected` list containing `"secret"`) fails with `Didn't expect part "secret"`. That one generic check covers any new token type. It lives in an upstream-owned test file, so the real risk is a merge that weakens or restructures that `notExpected` list. Re-check it after each upstream merge.
- **Passkey reset token verification** (#39): `core/record_query.go` `FindAuthRecordByToken` carries a fork-added `case TokenTypePasskeyReset` (secret `PasskeyResetToken.Secret`). The file is upstream-owned, so an upstream merge can drop it, and every emailed reset link is then rejected as `unknown token type`. `TestFindAuthRecordByTokenPasskeyReset` and `TestPasskeyResetFlow*` (apis) catch it.
- **modernc vs mattn divergence**: `apis/TestSQLRun/single_write_query` expects affected-rows = 1 for DDL under modernc (upstream's mattn reports 0). Keep the fork's value of 1 when the test conflicts on rebase.
- **Known flaky tests on macOS**: `core/TestNotifyWatcher_CollectionsUpdate` and `core/TestNotifyWatcher_SettingsUpdate` (fsnotify duplicate WRITE events) — not fork regressions.
- **Single-writer constraint**: the reference deployment (Azure Container Apps) runs a **single replica** (SQLite single-writer) with an **NFS** Azure Files share at `/pb_data`. NFS, not SMB — SMB lacks the POSIX byte-range locks SQLite WAL needs and crashlooped with `SQLITE_BUSY`. Don't introduce changes that assume horizontal scaling. `maxReplicas: 1` does **not** cover a rollout: ACA starts the incoming replica and waits for it to be *ready* before draining the outgoing one, and that ~40s overlap corrupted `auxiliary.db` five times (#35). `entrypoint.sh` fixes it by inverting the dependency — the incoming replica writes `/pb_data/.pb_handover`, the outgoing replica's watcher sees it, releases `/pb_data`, records itself in `.pb_released`, **acks** in `.pb_handover_ack`, and parks. **Never replace that with a blocking startup lock**: readiness gates the handover in both directions, so an incoming replica that blocks deadlocks the rollout (tried and reverted 2026-09-06, `b7642f75` / `2f46f48b`). The wait must stay **fail-open** for the same reason. **The lock alone is not proof** (#54, 2026-09-28): `flock -w` on NFS backs off and misses releases, a replica that fail-opened leaves the lock free, and a parked replica fails *liveness* and gets restarted by ACA — so the incoming replica needs the ack, and a restarted replica listed in `.pb_released` parks instead of serving. Handover events go to `/pb_data/.pb_handover.log` and, when OTLP is configured, as WARN logs. Test changes with `scripts/test_entrypoint_handover.sh`.
- **OTLP telemetry export**: `core/logger_otel.go` (fork-local) tees `app.Logger()` records to an OTLP/HTTP collector alongside the SQLite `_logs` sink, wired at the single `slog.New(...)` call in `core/base.go`. Gated on `OTEL_EXPORTER_OTLP_ENDPOINT` — **never** on `APPLICATIONINSIGHTS_CONNECTION_STRING`. No endpoint = exact upstream behavior. A success that something must alert on the absence of is logged with `slog.Bool(OTelAlwaysAttr, true)` (see the backup heartbeat in `core/backup.go`).
- **Litestream** ships in the image (`litestream.yml`) but runs only when `LITESTREAM_*` variables are set; `entrypoint.sh` skips restore and replicate otherwise.

<!-- BEGIN derek-task-inbox — shared block, identical in every CLAUDE.md under /Volumes/Data/repos. Edit all copies together. Canonical source: the Outline "Protocol" page linked below. -->

## Settings encryption

Opt-in. `entrypoint.sh` passes PocketBase's global `--encryptionEnv=PB_ENCRYPTION_KEY` (the flag takes the *name* of the env var, whose value is the key) to `serve`, the superuser bootstrap and subcommand passthrough only when `PB_ENCRYPTION_KEY` is set and non-empty; otherwise behaviour is unchanged. The key must be exactly 32 characters or the entrypoint refuses to start (it reports the length, never the key). Existing plaintext settings keep loading and are encrypted on the next settings save; once encrypted, losing the key (or unsetting it) loses the settings and fails startup, so the deployment repo must supply the key from its secret store and back it up. Covered by `scripts/test_entrypoint_encryption.sh`.
