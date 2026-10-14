# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this repo is

A fork of PocketBase (Go backend with embedded SQLite, realtime subscriptions, file/user management, and an embedded Svelte admin UI) carrying two custom layers on top of upstream:

1. **WebAuthn/Passkey auth** (`feat/webauthn-passkey-support` branch)
2. **Docker + Azure Container Apps deployment** (`deploy/azure` branch — Dockerfile, `entrypoint.sh`, `litestream.yml`, `infra/` Bicep, `azure.yaml`, `DEPLOY.md`)

`master` is the fork default branch, produced by merging `deploy/azure` after each upstream sync. See `FORK_SYNC.md` for the full sync runbook and current sync status.

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

- **Remotes**: `origin` = doodlemania2/pocketbase (push), `upstream` = pocketbase/pocketbase (fetch only).
- **Branch stack**: `upstream/master` → rebase `feat/webauthn-passkey-support` → rebuild `deploy/azure` on top → merge into `master`. `deploy/azure` is **not cleanly rebasable** — it is rebuilt by replaying its delta as a patch onto the fresh WebAuthn tip (FORK_SYNC.md Step 4). Linear history, force-push with `--force-with-lease`. Full procedure in `FORK_SYNC.md`; update its Sync Status section after each sync.
- **Token redaction pitfall**: `core/collection_model.go` `MarshalJSON` must blank the `Secret` of every `*ResetToken`-style auth token config. When upstream adds a new token type (e.g. `PasskeyResetToken` in v0.39.0), the rebase silently drops the redaction — re-add `alias.X.Secret = ""` or the secret leaks via the API.
- **modernc vs mattn divergence**: `apis/TestSQLRun/single_write_query` expects affected-rows = 1 for DDL under modernc (upstream's mattn reports 0). Keep the fork's value of 1 when the test conflicts on rebase.
- **Known flaky tests on macOS**: `core/TestNotifyWatcher_CollectionsUpdate` and `core/TestNotifyWatcher_SettingsUpdate` (fsnotify duplicate WRITE events) — not fork regressions.
- **Deployment constraint**: Azure Container Apps runs a **single replica** (SQLite single-writer) with an **NFS** Azure Files share at `/pb_data`. NFS, not SMB — SMB lacks the POSIX byte-range locks SQLite WAL needs and crashlooped with `SQLITE_BUSY`. Don't introduce changes that assume horizontal scaling. `maxReplicas: 1` does **not** cover a rollout: ACA starts the incoming replica and waits for it to be *ready* before draining the outgoing one, and that ~40s overlap corrupted `auxiliary.db` five times (#35). `entrypoint.sh` fixes it by inverting the dependency — the incoming replica writes `/pb_data/.pb_handover`, the outgoing replica's watcher sees it, releases `/pb_data`, records itself in `.pb_released`, **acks** in `.pb_handover_ack`, and parks. **Never replace that with a blocking startup lock**: readiness gates the handover in both directions, so an incoming replica that blocks deadlocks the rollout (tried and reverted 2026-09-06, `b7642f75` / `2f46f48b`). The wait must stay **fail-open** for the same reason. **The lock alone is not proof** (#54, 2026-09-28): `flock -w` on NFS backs off and misses releases, a replica that fail-opened leaves the lock free, and a parked replica fails *liveness* and gets restarted by ACA — so the incoming replica needs the ack, and a restarted replica listed in `.pb_released` parks instead of serving. Console logs are **not retained** (no Log Analytics since #51/#52); handover events go to `/pb_data/.pb_handover.log` and WARNs to SigNoz via OTLP. Test changes with `scripts/test_entrypoint_handover.sh`.
- **OTLP telemetry export**: `core/logger_otel.go` (fork-local) tees `app.Logger()` records to the parish SigNoz collector alongside the SQLite `_logs` sink, wired at the single `slog.New(...)` call in `core/base.go`. Gated on `OTEL_EXPORTER_OTLP_ENDPOINT` — **never** on `APPLICATIONINSIGHTS_CONNECTION_STRING`, since removing that setting must not silence OTLP. No endpoint = exact upstream behavior. Contract lives in `docs/observability/otlp-onboarding.md` in the STFoA-Church repo; setup in `DEPLOY.md`. **SigNoz is the only log destination.** Azure Log Analytics and App Insights are retired (#51/#52): the Container Apps env ships `appLogsConfiguration.destination: null`, `infra/` holds no cross-RG `existing` reference, and the `SHARED_OBS_RG`/`SHARED_LAW_NAME`/`SHARED_AI_NAME` repo variables were deleted 2026-10-02. Reintroducing either service re-creates the hard cross-RG dependency that took the deploy pipeline down for two days (#50) — don't.
- **Litestream is disabled** (since `f9cac5e0`, 2026-05-25) even though the binary and `litestream.yml` still ship in the image. No `LITESTREAM_*` env var is set, so `entrypoint.sh` skips both restore and replicate, and **there is no blob replica to restore from** — deleting `data.db` expecting an auto-restore destroys the database. Durability = persistent NFS volume + PocketBase's native backup cron (daily, S3). Full rationale and the unfinished re-enable path are in `DEPLOY.md`.

<!-- BEGIN derek-task-inbox — shared block, identical in every CLAUDE.md under /Volumes/Data/repos. Edit all copies together. Canonical source: the Outline "Protocol" page linked below. -->

## Handing work back to Derek — the Outline Task Inbox

Derek runs many agents at once, so a task mentioned only in a chat reply is lost. **Anything
that requires Derek personally must be filed as an item in the Outline Task Inbox in the same
turn you discover it**, and then also mentioned in your reply.

- Inbox: [Derek's Task Inbox](https://outline.thedoodleproject.net/doc/dereks-task-inbox-qJpbaMEN3j) — `documentId: ae39277c-130c-4e95-90e0-80d7ed4138e4`
- Full spec: [Protocol — how agents file tasks](https://outline.thedoodleproject.net/doc/protocol-how-agents-file-tasks-lLeYcUvbS4) — canonical if it disagrees with this block

**Which direction is which.** The inbox is work *for Derek*: approvals, decisions, merging to
`main`, entering a credential, vendor-UI clicks (CCB, Stripe, Power Platform, Azure Portal,
Exchange), phone calls, hand-verifying prod. Work *for agents* — bugs, features, refactors,
investigations — is a **GitHub issue** in the owning repo, never an inbox item. If an agent
could do it, do it. Status and findings belong in your reply, not the inbox.

**Filing an item**

1. `mcp__outline__fetch` (`resource: "document"`, `id: "ae39277c-130c-4e95-90e0-80d7ed4138e4"`)
   first. If the task is already filed, patch that item instead of adding a duplicate.
2. `mcp__outline__update_document` with `editMode: "patch"`, `findText` = the target section
   heading, `text` = that heading + a blank line + your new item, so it lands at the top of the
   section. Sections: `## 🔴 Blocking` (an agent or a parishioner is stuck), `## 🟡 Normal`
   (needed soon, nothing stuck), `## 🔵 Whenever`. Delete the `*Nothing pending.*` placeholder
   when a section gets its first real item.

```markdown
- [ ] **<Imperative one-line title>** · `<repo>` · [#123](https://github.com/doodlemania2/<repo>/issues/123) · _YYYY-MM-DD_
  - **Why:** what is broken or blocked until this happens
  - **Do:** the exact steps or command to run
  - **Done when:** the observable condition that means it worked
  - **Context:** IDs, URLs, env/org, file paths — everything needed to act without asking
  - **Filed by:** agent · <branch / worktree / session hint>
```

Every field is required, and the item must stand alone: Derek opens it cold days later with no
memory of the session and finishes it without asking a question. That means real GUIDs and
record IDs, the Dataverse org name (`stfoafrisco-prod` vs `stfoafrisco-staging`), the PR/issue
link, and copy-pasteable commands — not "the affected family" or "the usual script". Never put
a secret value in an item; name the Key Vault secret instead. One task per item.

**Sweep the ticked items when you file.** Ticking is Derek's alone — never tick, untick, or
reorder an item. Moving a ticked item is your job. Every time you add or edit an inbox item,
first move every already-ticked item to the child page **Archive**
(`43e67dfb-7055-40a9-a17c-7c54186c024f`), under its `## 2026` heading. Keep the title line — it
carries the repo, the issue link and the date. Drop the Why / Do / Done-when / Context
sub-bullets, because the linked GitHub issue holds that detail. Write the Archive first and the
inbox second, so a failed second write loses nothing. Never sweep an item Derek has not ticked,
however finished it looks; add a dated note to it instead. Never put literal checkbox syntax
inside an item, even in backticks — the Outline parser reads it as list markup and silently
destroys the sub-bullets of the items that follow.

If a task you filed became unnecessary, patch it to `~~struck through~~` with the reason and
date. If the Outline MCP is unavailable in your session (headless and cron runs sometimes lack
it), say so explicitly and put the fully-formatted item inline in your reply rather than
dropping it.

<!-- END derek-task-inbox -->
