---
name: pocketbase-fork
description: "Context for this PocketBase fork with WebAuthn/Passkey support and Azure Container Apps deployment. Provides branch strategy, WebAuthn API reference, Docker/Litestream patterns, Azure infrastructure details, and upstream sync workflow. WHEN: WebAuthn, passkey, fork, upstream sync, rebase, deploy, docker, litestream, pocketbase fork, container apps, azure files, branch strategy."
---

# PocketBase Fork — WebAuthn + Azure Deployment

## Fork Context

This is a maintained fork of [pocketbase/pocketbase](https://github.com/pocketbase/pocketbase) that adds WebAuthn/Passkey authentication. The fork is designed to stay in sync with upstream via a layered branch strategy.

**CRITICAL RULES for working in this repo:**

1. **Never modify upstream files on `deploy/azure`** — Only add new files on this branch. All upstream modifications belong on `feat/webauthn-passkey-support`.
2. **WebAuthn changes are additive** — New fields appended to existing structs, new hook methods appended to interfaces, new routes appended to route lists. Do not restructure existing code.
3. **Keep `deploy/azure` conflict-free** — Every file on this branch must be a new file that doesn't exist upstream.

## Branch Strategy

```
upstream/master (pocketbase/pocketbase)
    |
    |  git rebase upstream/master
    v
feat/webauthn-passkey-support    <-- WebAuthn only (4 new + 15 modified files)
    |
    |  git rebase feat/webauthn-passkey-support
    v
deploy/azure                     <-- Docker + infra + CI/CD + docs (ALL new files)
```

**Which branch to target for changes:**

| Change type | Target branch |
|------------|---------------|
| WebAuthn Go code (core/, apis/) | `feat/webauthn-passkey-support` |
| Docker, Bicep, CI/CD, docs | `deploy/azure` |
| Upstream bug fixes | `feat/webauthn-passkey-support` (then rebase deploy) |

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
docker build -t pocketbase-fork .
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

See `FORK_SYNC.md` for full instructions. Summary:

1. `git fetch upstream`
2. Rebase `feat/webauthn-passkey-support` onto `upstream/master` (conflicts here)
3. `make test`
4. Rebase `deploy/azure` onto updated webauthn branch (conflict-free)
5. `make test && docker build`

### Conflict-Prone Files

| File | Risk |
|------|------|
| `go.mod` / `go.sum` | MEDIUM |
| `core/collection_model_auth_options.go` | MEDIUM |
| `core/app.go`, `core/base.go` | LOW |
| `apis/record_auth.go` | LOW |
| Test files (count assertions) | LOW |

## Companion Package

`pocketbase-webauthn` (planned, separate repo) — TypeScript package wrapping WebAuthn endpoints using the upstream JS SDK's `pb.send()`. No JS SDK fork needed.

### Usage

```typescript
import { registerPasskey, loginWithPasskey } from 'pocketbase-webauthn';
await registerPasskey(pb, 'users', { name: 'My MacBook' });
const auth = await loginWithPasskey(pb, 'users', 'user@example.com');
```

Dependencies: `pocketbase` (peer), `@simplewebauthn/browser`.
