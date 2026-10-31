<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="brand/logo/lockup-horizontal-dark.svg">
    <img src="brand/logo/lockup-horizontal.svg" alt="Claviger" width="360">
  </picture>
</p>

<p align="center"><b>Open the right doors.</b><br>
A self-hosted backend in one file, with passkeys and identity built in.<br>
<a href="https://claviger.app">claviger.app</a></p>

---

**Claviger** (*KLAV-ih-jer*, Latin for "key-bearer") is an open-source backend built on
[PocketBase](https://ganigeorgiev.com/pocketbase/pocketbase): a single Go binary with an
embedded SQLite database, realtime subscriptions, file storage, an admin dashboard and REST
APIs. Claviger adds what PocketBase leaves out: first-class **passkeys**, production
container packaging that is safe for SQLite, telemetry export, and a roadmap that turns it
into a full **identity server**.

It is built for two kinds of users:

- **Developers and self-hosters** who like PocketBase and need real identity: passkeys today,
  an OpenID Connect provider, organizations, SCIM and audit logs on the roadmap.
- **Congregations, parishes and dioceses**, which organize people as households, guard
  children's data, and keep records that must never be edited. Claviger is the first backend
  designed around those needs. It is for any denomination.

> **Status:** early. Everything PocketBase does works today, plus the features marked
> *shipped* below. Items marked *roadmap* are open issues, not promises.

## What ships today

| | |
|---|---|
| **Everything in PocketBase** | Collections, API rules, realtime, files, email/password, OTP, OAuth2 with ~30 providers, MFA, admin UI, backups, Go and JavaScript hooks |
| **Passkeys (WebAuthn)** | Register, sign in, manage and revoke passkeys per auth collection; passkeys count as an MFA factor; rate-limited ceremonies with origin and sign-counter checks |
| **Passkey reset** | Email-based recovery that clears a user's passkeys so they can enroll again. Known bug: tokens are currently rejected ([#39](../../issues/39)) |
| **OpenTelemetry log export** | Tee app logs to any OTLP/HTTP collector beside the built-in log store; off unless configured |
| **SQLite-safe container rollouts** | `entrypoint.sh` hands the database over between the outgoing and incoming container, so a rolling deploy never runs two writers on one database |
| **Container packaging** | Multi-stage `Dockerfile`, `docker-compose.yml`, optional Litestream replication |

## Roadmap

**Identity:** OpenID Connect provider ([#71](../../issues/71)), passkey-only collections and
device management ([#73](../../issues/73)), TOTP and backup codes ([#90](../../issues/90)),
enterprise SSO ([#92](../../issues/92)), organizations, roles and SCIM ([#79](../../issues/79)),
device sign-in ([#96](../../issues/96)), scoped API keys ([#95](../../issues/95)).

**Trust:** audit log ([#74](../../issues/74)), field-level encryption ([#75](../../issues/75)),
signed webhooks ([#77](../../issues/77)), verified backups ([#78](../../issues/78)), traces
and metrics ([#76](../../issues/76)).

**Congregations** ([label](../../issues?q=label%3Areligious-orgs)): households
([#80](../../issues/80)), access tied to safeguarding training ([#81](../../issues/81)),
children's check-in ([#82](../../issues/82)), staff-assisted recovery for older members
([#83](../../issues/83)), one identity across a diocese ([#84](../../issues/84)), pastoral
confidentiality ([#86](../../issues/86)), permanent sacramental records
([#87](../../issues/87)), GDPR special-category compliance ([#88](../../issues/88)).

## Quick start

```sh
docker compose up --build
```

Then open the dashboard at `http://127.0.0.1:8090/_/` and check
`http://127.0.0.1:8090/api/health`.

Or build and run the binary directly (Go 1.27+):

```sh
cd examples/base && go run main.go serve
```

### Running in production

Claviger is SQLite: **run exactly one instance** against a data directory on durable
storage. On a container platform that starts the new container before stopping the old one,
the entrypoint's handover keeps the two from writing at once. On network storage, use NFS,
not SMB; SMB lacks the byte-range locks SQLite's WAL mode needs.

## Passkey API

Enable passkeys on an auth collection (`webauthn.enabled`), then:

```
POST   /api/collections/{collection}/auth-with-webauthn/register-begin
POST   /api/collections/{collection}/auth-with-webauthn/register-finish
POST   /api/collections/{collection}/auth-with-webauthn/login-begin
POST   /api/collections/{collection}/auth-with-webauthn/login-finish
GET    /api/collections/{collection}/auth-with-webauthn/credentials
DELETE /api/collections/{collection}/auth-with-webauthn/credentials/{credentialId}
DELETE /api/collections/{collection}/auth-with-webauthn/credentials-by-record/{recordId}
```

Set `WEBAUTHN_RP_ID` and `WEBAUTHN_RP_ORIGINS` to scope passkeys to a parent domain while
the server runs on a subdomain; otherwise they follow the app URL.

## SDKs and compatibility

Claviger keeps PocketBase's HTTP API, so the official
[JavaScript](https://github.com/pocketbase/js-sdk) and [Dart](https://github.com/pocketbase/dart-sdk)
SDKs work unchanged. Passkey endpoints are plain REST calls on top.

Claviger can also be used as a Go framework, the same way as PocketBase
([Extend with Go](https://pocketbase.io/docs/go-overview/)). For now the Go module path is
still upstream's (`ganigeorgiev.com/pocketbase/pocketbase`); see [#89](../../issues/89).

## How this repo tracks upstream

Claviger follows PocketBase releases and carries its changes as layers on top:

- `feat/webauthn-passkey-support`: the passkey feature
- `deploy/azure`: container packaging, telemetry and CI, rebuilt onto each new passkey tip
- `master`: the default branch, produced by merging `deploy/azure` after each sync

The procedure and the status of every sync are in [FORK_SYNC.md](FORK_SYNC.md).

## Development

```sh
make test     # go test ./...
make lint     # golangci-lint
cd ui && npm install && npm run build   # admin UI; commit the regenerated ui/dist
```

## Brand

Logo, colors, typography and voice guidelines are in [`brand/`](brand/).

## License and credit

MIT, like PocketBase. See [LICENSE.md](LICENSE.md) and
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

Claviger is built on [PocketBase](https://pocketbase.io) by Gani Georgiev. Its architecture,
developer experience and steady maintenance make this project possible. Changes that would
help every PocketBase user are offered upstream first. Claviger is an independent project and
is not affiliated with or endorsed by PocketBase.
