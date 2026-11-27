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
[PocketBase](https://pocketbase.io): a single Go binary with an embedded SQLite database,
realtime subscriptions, file storage, an admin dashboard and REST APIs. Claviger adds what
PocketBase leaves out: passkeys, an OpenID Connect provider, organizations, an audit log,
field encryption, and tools for congregations and for GDPR. Production container packaging
keeps SQLite safe during rolling deploys.

It is built for two kinds of users:

- **Developers and self-hosters** who like PocketBase and need real identity: passkeys, an
  OpenID Connect provider, organizations with SCIM, scoped API keys, and an audit log.
- **Congregations, parishes and dioceses**, which organize people as households, guard
  children's data, and keep records that must never be edited. Claviger is the first backend
  designed around those needs. It is for any denomination.

> **Status:** early. Everything PocketBase does works, plus the features listed below as
> shipped. The roadmap at the end lists what is not built. Where a guide says a topic needs
> local legal review, take that seriously: Claviger is software, not legal advice.

## What ships today

Everything in PocketBase still works: collections, API rules, realtime, files, email and
password, OTP, OAuth2 with about 30 providers, MFA, the admin UI, backups, and Go and
JavaScript hooks. Every feature below is off, or has no effect, until you configure it.

### Identity

| Feature | Notes |
|---|---|
| Passkeys (WebAuthn) | Register, sign in, manage and revoke per auth collection; counts as an MFA factor. See [Passkey API](#passkey-api). |
| Passkey-only collections, step-up, passkey reset | Collections that accept no password, a fresh passkey check for sensitive actions, email recovery that clears passkeys ([#73](../../issues/73)) |
| OpenID Connect provider | Authorization code, refresh and device flows under `/oidc`, plus the account chooser: [docs/oidc.md](docs/oidc.md) |
| Church software SSO presets | Client templates and setup guides for Church Online Platform, MinistryPlatform, Blackbaud ID and a generic app, with a vendor survey. The guides are written from public documentation and are **not yet verified end to end**: [docs/sso](docs/sso/README.md) ([#85](../../issues/85)) |
| TOTP and backup codes | Authenticator-app second factor for users and superusers, enforced once enrolled ([#90](../../issues/90)) |
| Captcha and persistent rate limits | Turnstile, hCaptcha or reCAPTCHA; counters survive restarts. See [Captcha and rate limits](#captcha-and-rate-limits) |
| Bans, session revocation, impersonation | Superuser tools, audited ([#93](../../issues/93)) |
| Scoped API keys | Per-key scopes; read-only on organization data ([#95](../../issues/95)) |
| Localized pages and emails | Nine languages, right-to-left support: [docs/localization.md](docs/localization.md) |
| Accessible hosted pages | Larger text, high contrast, keyboard and screen reader use: [docs/accessibility.md](docs/accessibility.md) |
| Organizations and roles | Members, roles, tags, invitations, nested orgs and transfers: [docs/orgs.md](docs/orgs.md) |
| SCIM 2.0 | Provision members from Entra ID or Okta: [docs/scim.md](docs/scim.md) |

### Trust and operations

| Feature | Notes |
|---|---|
| Audit log | Append-only, with optional read audit for flagged collections: [docs/audit.md](docs/audit.md) |
| Field encryption | Encrypted text and JSON fields, KEK sources, rotation: [docs/field-encryption.md](docs/field-encryption.md) |
| Signed webhooks | Standard Webhooks signatures, retries: [docs/webhooks.md](docs/webhooks.md) |
| Encrypted backups | AES-256-GCM, restore verification: [docs/backup-encryption.md](docs/backup-encryption.md) |
| OpenTelemetry and metrics | OTLP/HTTP logs, traces and metrics; Prometheus text at `/api/metrics`. See [Telemetry](#telemetry) |
| SQLite-safe rollouts | `entrypoint.sh` hands the database from the outgoing to the incoming container |
| Container packaging | Multi-stage `Dockerfile`, `docker-compose.yml`, optional Litestream replication |
| PostgreSQL | Supported, opt-in data store (16 and 17); several replicas only on PostgreSQL; no zip backups: [docs/datastores.md](docs/datastores.md) |

### Congregations

| Feature | Notes |
|---|---|
| Households | Shared contact data and guardian-managed children: [docs/households.md](docs/households.md) |
| Safeguarding credentials | Access to children's data ends when a credential expires: [docs/credentials.md](docs/credentials.md) |
| Staff-assisted recovery | Short codes vouched for by staff: [docs/recovery.md](docs/recovery.md) |
| Diocese hierarchy and transfers | Nested orgs, opt-in oversight rules, consented transfers, org-scoped role tags: [docs/orgs.md](docs/orgs.md) |
| Pastoral notes | Locked rules, two-person break-glass access, expiring prayer requests: [docs/pastoral-notes.md](docs/pastoral-notes.md) |
| Permanent registers | Final entries cannot be edited; corrections are annotations: [docs/registers.md](docs/registers.md) |
| SMS | Twilio or a generic webhook: [docs/sms.md](docs/sms.md) |

### Data protection (GDPR)

| Feature | Notes |
|---|---|
| Data map | Every collection and what it holds about people: [docs/data-map.md](docs/data-map.md) |
| Personal data export | One ZIP per person, driven by the data map: [docs/data-export.md](docs/data-export.md) |
| Consent records | What was agreed, when, on which wording, by whom: [docs/consent.md](docs/consent.md) |
| Retention and legal holds | Daily retention job, holds that block deletion: [docs/retention.md](docs/retention.md) |
| Erasure | Confirmed requests, a grace period, holds and registers respected, audit scrub, re-erasure after a restore: [docs/erasure.md](docs/erasure.md) |
| Admin screens | Data map, requests, holds, retention and consent in the admin UI under Data protection |
| Administrator guide and templates | Day-to-day guide for the parish data-protection lead, a DPIA template and an Article 30 record template (templates, not legal advice): [docs/gdpr/admin-guide.md](docs/gdpr/admin-guide.md) |

Retention periods, pastoral disclosure and the interaction of permanent registers with
erasure all need local legal review. The guides say so where it applies.

## Roadmap

Not built yet. Labels and details are in the [issues](../../issues).

- Register restriction flow ([#88](../../issues/88)).
- Children's check-in ([#82](../../issues/82)).
- Enterprise inbound SSO ([#92](../../issues/92)).
- Built-in handover between instances is in, opt-in (`serve --handover`, see [docs/handover.md](docs/handover.md)); deleting the entrypoint's copy of it is what remains of [#72](../../issues/72).
- Owner-held key vault ([#86](../../issues/86)). Design spike: [docs/spikes/86-6-owner-key-vault.md](docs/spikes/86-6-owner-key-vault.md).
- Other data stores beyond SQLite and PostgreSQL ([#106](../../issues/106)).

## Quick start

```sh
docker compose up --build
```

Then open the dashboard at `http://127.0.0.1:8090/_/` and check
`http://127.0.0.1:8090/api/health`.

### Container image

Each version tag publishes a multi-arch image (linux/amd64, linux/arm64) to
`ghcr.io/doodlemania2/claviger`:

| Tag | Points at |
|---|---|
| `1.2.3` | that release |
| `1.2` | the newest patch of 1.2 |
| `latest` | the newest stable release (pre-releases such as `1.2.3-rc.1` get only their exact tag) |

Pin by digest in production (`ghcr.io/doodlemania2/claviger@sha256:...`). The image runs
`/usr/local/bin/claviger` through `entrypoint.sh`, with data in `/pb_data` and the handover
described below. It serves at the root path of its host name: it cannot run under a URL
path prefix such as `https://example.org/auth/`, so give it its own host name. Until the
first release is tagged, build it yourself with `docker compose up --build` above.

Or build and run the binary directly (Go 1.27+):

```sh
cd examples/base && go run main.go serve
```

### Running in production

Claviger is SQLite: **run exactly one instance** against a data directory on durable
storage. On a container platform that starts the new container before stopping the old one,
the entrypoint's handover keeps the two from writing at once. On network storage, use NFS,
not SMB; SMB lacks the byte-range locks SQLite's WAL mode needs.

### Deploy

Copy-paste templates live in [`deploy/`](deploy/). Each folder has its own README.

| Platform | Folder |
|---|---|
| Docker Compose, optional Caddy HTTPS and Litestream | [`deploy/docker-compose`](deploy/docker-compose) |
| Fly.io | [`deploy/fly`](deploy/fly) |
| Kubernetes | [`deploy/kubernetes`](deploy/kubernetes) |
| Azure Container Apps (Bicep, NFS) | [`deploy/azure-container-apps`](deploy/azure-container-apps) |

All of them run a single instance. Passkeys need HTTPS and a host name you do not
change later.

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

## Captcha and rate limits

Off until you use them. In Settings > Captcha, choose Turnstile, hCaptcha or reCAPTCHA, enter
the keys, and switch on the actions that need it (sign-up, sign-in, OTP and reset requests,
passkey login). Clients send `X-Captcha-Token` or a `captchaToken` body field; a rejected
token is a `400` with `data.captchaToken.code` of `captcha_failed`. If the provider is
unreachable, requests pass by default so an outage cannot lock people out; set `failClosed` to
refuse them. Rate limit counters are kept in `auxiliary.db`, so a restart does not reset them.

**Account lockout** protects short secrets such as numeric PINs, which a per-IP limit does not:
set `accountLockout.maxFailures` (0 = off, up to 100) and `accountLockout.windowSeconds` (default
900, up to 86400) through the settings API. After that many failed `auth-with-password` calls for
one identity inside the window, the identity gets `429` with `Retry-After` until the window ends,
even for the right password; a success clears the count. The count is kept per collection and
submitted identity whether or not the account exists, so the answer reveals nothing about who is
registered, and it survives restarts. Someone who knows an identity can lock it for one window.

## Provisioning keys

An API key can never write an auth collection, except with the scope `provision:<auth collection>`
(the API key scope list otherwise holds `*` and `collection:<name>:read|write`). It lets a backend
create accounts and choose their passwords without holding a superuser token, and nothing more: only
`POST /api/collections/<c>/records` (not `verified`, `tokenKey` or `id`) and
`PATCH /api/collections/<c>/records/<id>` with a JSON body of just `password` and
`passwordConfirm`. It cannot change an email, delete, or touch another collection, and `*` does not
grant it. The companion scope `manage:<auth collection>` allows `PATCH` with a JSON body of just `email`
and `DELETE` of a record, nothing else. A key-made email change clears `verified`, rotates the token key
and removes the record's OAuth2 links, so the new address must be proven again. Collection rules still run as the key's owner afterwards, so a user owner needs rules that
allow those two writes. A collection whose auth rule requires `verified` will refuse accounts made
this way, because a key cannot set it.

## Telemetry

Set `OTEL_EXPORTER_OTLP_ENDPOINT` to export logs, traces and metrics over OTLP/HTTP. Without
it, nothing is exported. `OTEL_TRACES_EXPORTER=none`, `OTEL_METRICS_EXPORTER=none` and
`OTEL_SDK_DISABLED=true` switch signals off. Set `CLAVIGER_METRICS_TOKEN` to serve
Prometheus text at `GET /api/metrics` with that bearer token; without it the route returns
404.

## SDKs and compatibility

Claviger keeps PocketBase's HTTP API, so the official
[JavaScript](https://github.com/pocketbase/js-sdk) and [Dart](https://github.com/pocketbase/dart-sdk)
SDKs work unchanged. Passkey endpoints are plain REST calls on top.

Claviger can also be used as a Go framework, the same way as PocketBase
([Extend with Go](https://pocketbase.io/docs/go-overview/)). The Go module path is
`github.com/doodlemania2/claviger`:

```sh
go get github.com/doodlemania2/claviger
```

## Upstream

Claviger follows PocketBase releases. Upstream now lives at
[ganigeorgiev.com](https://ganigeorgiev.com/pocketbase/pocketbase). `master` is the one trunk,
and each upstream release is merged in by pull request. The procedure and sync status are in
[FORK_SYNC.md](FORK_SYNC.md).

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

Claviger is built on [PocketBase](https://pocketbase.io) by Gani Georgiev ([source](https://ganigeorgiev.com/pocketbase/pocketbase)). Its architecture,
developer experience and steady maintenance make this project possible. Changes that would
help every PocketBase user are offered upstream first. Claviger is an independent project and
is not affiliated with or endorsed by PocketBase.
