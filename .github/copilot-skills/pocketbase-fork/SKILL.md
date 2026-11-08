---
name: pocketbase-fork
description: "Context for Claviger, this PocketBase fork with WebAuthn/Passkey support and container packaging. Provides branch model, WebAuthn API reference, Docker/Litestream patterns, deployment notes, and the upstream sync pointer. WHEN: WebAuthn, passkey, fork, upstream sync, merge, deploy, docker, litestream, claviger, pocketbase fork, container apps, azure files, branch model."
---

# Claviger (PocketBase fork) — WebAuthn + container packaging

## Fork Context

This is a maintained fork of [pocketbase/pocketbase](https://ganigeorgiev.com/pocketbase/pocketbase) (upstream left GitHub after v0.40.6; the `upstream` remote is `https://ganigeorgiev.com/git/pocketbase/pocketbase`) that adds WebAuthn/Passkey authentication and container packaging. The fork's Go module path is `github.com/doodlemania2/claviger`; the binary is `claviger`.

**Rules for working in this repo:**

1. **One trunk.** All work, WebAuthn and packaging included, lands on `master` by PR.
2. **WebAuthn changes are additive.** Append new fields to existing structs, new hook methods to interfaces, new routes to route lists. Do not restructure upstream code, so upstream merges stay small.
3. **Nothing deployment-specific.** This repo is public. Deployment details live in the private repo `doodlemania2/stfoa-auth`.

## Branches

| Branch | Role |
|--------|------|
| `master` | The trunk. Target every change here. |
| `deploy/azure` | Release pointer only. Fast-forwarded to a tested, approved `master` SHA with `git push origin <sha>:deploy/azure`. Never commit to it. |
| `feat/webauthn-passkey-support` | Retired and frozen. Do not push to it. |

Never rebase or force-push `master`.

## WebAuthn API Reference

### Endpoints

| Operation | Route | Method | Auth |
|-----------|-------|--------|------|
| Register Begin | `/api/collections/{col}/auth-with-webauthn/register-begin` | POST | Authenticated |
| Register Finish | `/api/collections/{col}/auth-with-webauthn/register-finish` | POST | Authenticated |
| Login Begin | `/api/collections/{col}/auth-with-webauthn/login-begin` | POST | Public |
| Login Finish | `/api/collections/{col}/auth-with-webauthn/login-finish` | POST | Public |
| List Credentials | `/api/collections/{col}/auth-with-webauthn/credentials` | GET | Authenticated |
| Delete Credential | `/api/collections/{col}/auth-with-webauthn/credentials/{id}` | DELETE | Authenticated |
| Admin Clear | `/api/collections/{col}/auth-with-webauthn/credentials-by-record/{id}` | DELETE | Superuser |

### Request/Response Shapes

**Register Begin** (empty body) returns:
```json
{ "options": { "publicKey": { "challenge": "...", "rp": {...}, "user": {...} } }, "sessionToken": "string" }
```

**Register Finish** (body: `{ "sessionToken": "...", "name": "..." }`) returns:
```json
{ "id": "string", "name": "string" }
```

**Login Begin** (body: `{ "identity": "user@example.com" }`) returns:
```json
{ "options": { "publicKey": { "challenge": "...", "allowCredentials": [...] } }, "sessionToken": "string" }
```

**Login Finish** (body: `{ "identity": "...", "sessionToken": "..." }`) returns:
```json
{ "token": "JWT", "record": { "id": "...", "email": "...", ... } }
```

**List Credentials** returns:
```json
[{ "id": "...", "name": "...", "created": "...", "signCount": 42 }]
```

### Security

- Session TTL: 2 minutes
- Rate limit on login-finish: 5 requests / 180 seconds per record
- Sign count validation on every login
- Origin/RP derived from `app.Settings().Meta.AppURL`
- Cascading delete: credentials auto-deleted when user or collection is deleted

### Configuration

Enable per collection:
```json
{ "webauthn": { "enabled": true } }
```

MFA: WebAuthn counts as a valid MFA method (`MFAMethodWebAuthn = "webauthn"`).

## Docker Setup

### Build

```bash
docker build -t claviger .
```

Multi-stage: `golang:1.27-alpine` (build) -> `alpine:3` (runtime with Litestream).

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PB_HOST` | `0.0.0.0` | Bind address |
| `PB_PORT` | `8090` | Listen port |
| `PB_ADMIN_EMAIL` | - | Auto-create superuser email |
| `PB_ADMIN_PASSWORD` | - | Auto-create superuser password |
| `LITESTREAM_REPLICA_URL` | - | Azure Blob URL for backup (e.g., `abs://container-name`) |
| `LITESTREAM_ACCESS_KEY_ID` | - | Storage account name |
| `LITESTREAM_SECRET_ACCESS_KEY` | - | Storage account key |
| `ENCRYPTION` | - | 32-char hex key for settings encryption |

### Litestream Backup

Continuous WAL streaming to Azure Blob Storage. On cold start, if `/pb_data/data.db` is missing, the entrypoint auto-restores from the latest Litestream snapshot.

Databases replicated: `data.db` and `auxiliary.db`.

## Azure Infrastructure

- **Azure Container Apps** — Single replica (SQLite is single-writer)
- **Azure Files** — SMB mount at `/pb_data` for persistent storage
- **Azure Blob Storage** — Litestream backup target
- **Azure Container Registry** — Docker image storage
- **Managed Identity** — RBAC: ACR Pull + Storage Blob Data Contributor
- **Health probes** — Startup (90s tolerance), Liveness (30s), Readiness (10s) on `/api/health`

Deploy: `azd up`

## Upstream Sync Workflow

Upstream is brought in by merge, never rebase. See `FORK_SYNC.md` for the full runbook and pitfall checklist. Summary:

1. `git fetch upstream --tags`; list the delta with `git log --oneline <last-synced-tag>..<new-tag>`. The owner approves each upstream change first.
2. Branch `sync/<tag>` off `origin/master`, then `git merge --no-ff <tag>`.
3. Run `scripts/rewrite-module-path.sh` (rewrites upstream's module path to `github.com/doodlemania2/claviger`), then `go mod tidy`, `go build ./...`, `go test ./...`.
4. Re-check the pitfalls in `FORK_SYNC.md` (token secret redaction, modernc test value, jsvm hook count, `ui/dist` rebuild).
5. PR to `master`, CI green, merge with a merge commit.
6. After testing and approval, fast-forward the release pointer: `git push origin <sha>:deploy/azure`.

Conflict-prone files are listed in `FORK_SYNC.md`.

## Companion Package

`pocketbase-webauthn` (planned, separate repo) — TypeScript package wrapping WebAuthn endpoints using the upstream JS SDK's `pb.send()`. No JS SDK fork needed.

### Usage

```typescript
import { registerPasskey, loginWithPasskey } from 'pocketbase-webauthn';
await registerPasskey(pb, 'users', { name: 'My MacBook' });
const auth = await loginWithPasskey(pb, 'users', 'user@example.com');
```

Dependencies: `pocketbase` (peer), `@simplewebauthn/browser`.
