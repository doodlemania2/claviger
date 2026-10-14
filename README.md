# PocketBase Fork: Enterprise Security, Minimal Overhead

[![PocketBase](https://i.imgur.com/aCBbjKx.png)](https://pocketbase.io)

[![Upstream build](https://github.com/pocketbase/pocketbase/actions/workflows/release.yaml/badge.svg)](https://github.com/pocketbase/pocketbase/actions/workflows/release.yaml)
[![Upstream latest release](https://img.shields.io/github/release/pocketbase/pocketbase.svg)](https://github.com/pocketbase/pocketbase/releases)
[![Go package documentation](https://godoc.org/github.com/pocketbase/pocketbase?status.svg)](https://pkg.go.dev/github.com/pocketbase/pocketbase)

This repository is a maintained fork of [pocketbase/pocketbase](https://github.com/pocketbase/pocketbase).

It is designed for teams that need stronger authentication and production reliability without introducing unnecessary platform sprawl.

## Who this is for

- teams that want phishing-resistant authentication without a full identity-platform migration
- organizations that need a simpler production footprint and fewer systems to operate
- internal platforms that prefer SQLite durability plus continuous off-node backup replication
- security-conscious teams adopting passkeys while staying close to upstream PocketBase

PocketBase itself is an excellent open source Go backend with:

- embedded SQLite with realtime subscriptions
- built-in auth, files, and admin UI
- straightforward REST-style APIs
- a strong extension model in Go and JavaScript

If you are new to PocketBase, start with the official docs at [pocketbase.io/docs](https://pocketbase.io/docs).

## Why this fork exists

This fork is focused on two things:

1. Adding first-class WebAuthn/Passkey authentication support.
2. Providing a practical Docker + Azure Container Apps deployment path for real-world operation.

The goal is to stay close to upstream while adding security and operations features that reduce risk, overhead, and complexity.

## Security + simplicity at a glance

- phishing-resistant sign-in with passkeys
- fewer moving parts (PocketBase + SQLite + Litestream)
- no separate database cluster to operate
- backup and restore built into the deployment model
- fork changes kept intentionally scoped for easier upstream sync

## What this fork adds

### 1) WebAuthn / Passkey auth method

This fork adds passkeys as a first-class auth method beside password, OAuth2, and OTP.

Included API surface:

- `POST /api/collections/{collection}/auth-with-webauthn/register-begin`
- `POST /api/collections/{collection}/auth-with-webauthn/register-finish`
- `POST /api/collections/{collection}/auth-with-webauthn/login-begin`
- `POST /api/collections/{collection}/auth-with-webauthn/login-finish`
- `GET /api/collections/{collection}/auth-with-webauthn/credentials`
- `DELETE /api/collections/{collection}/auth-with-webauthn/credentials/{credentialId}`
- `DELETE /api/collections/{collection}/auth-with-webauthn/credentials-by-record/{recordId}`

Security highlights:

- collection-level toggle (`webauthn.enabled`)
- challenge-session handling with TTL
- login-finish rate limiting
- sign counter validation
- RP/origin validation
- MFA integration (`webauthn` as a valid MFA method)
- credential cleanup hooks on user deletion

### 2) Docker + Azure deployment stack

This fork includes a deployment stack aimed at running PocketBase safely in production while keeping operations simple:

- multi-stage `Dockerfile`
- `docker-compose.yml` for local dev
- `entrypoint.sh` for bootstrap/restore/startup orchestration
- `litestream.yml` for continuous WAL replication to Azure Blob Storage
- `azure.yaml` + Bicep in `infra/` for Azure Container Apps

This enables a lean operational model: one service, one durable data path, continuous off-node backups, and straightforward disaster recovery.

## Quick start

You can start quickly and keep the same architecture from development to production.

### Local (Docker)

```sh
docker compose up --build
```

Then open:

- `http://127.0.0.1:8090/_/`
- `http://127.0.0.1:8090/api/health`

### Azure (azd)

```sh
azd up
```

Detailed production steps are documented in [DEPLOY.md](DEPLOY.md).

## Upstream compatibility and sync model

This fork is intentionally layered to minimize sync friction with upstream:

- `feat/webauthn-passkey-support`: passkey feature branch (upstream-facing delta)
- `deploy/azure`: container + infra + ops layer

Sync workflow and conflict notes are documented in [FORK_SYNC.md](FORK_SYNC.md).

Current sync workflow and branch maintenance guidance are documented in [FORK_SYNC.md](FORK_SYNC.md).

## Using PocketBase as a Go framework

PocketBase remains a regular Go package. You can build a custom app the same way as upstream.

Minimal example:

```go
package main

import (
    "log"

    "github.com/pocketbase/pocketbase"
    "github.com/pocketbase/pocketbase/core"
)

func main() {
    app := pocketbase.New()

    app.OnServe().BindFunc(func(se *core.ServeEvent) error {
        se.Router.GET("/hello", func(re *core.RequestEvent) error {
            return re.String(200, "Hello from PocketBase")
        })

        return se.Next()
    })

    if err := app.Start(); err != nil {
        log.Fatal(err)
    }
}
```

Run it:

```sh
go mod init myapp && go mod tidy
go run main.go serve
```

For more, see [Extend with Go](https://pocketbase.io/docs/go-overview/).

## API SDK clients

The official SDKs remain the easiest way to call PocketBase APIs:

- JavaScript: [pocketbase/js-sdk](https://github.com/pocketbase/js-sdk)
- Dart: [pocketbase/dart-sdk](https://github.com/pocketbase/dart-sdk)

## Testing

Run the full Go test suite:

```sh
go test ./...
```

Additional WebAuthn implementation details and roadmap notes are tracked in [WEBAUTHN_PLAN.md](WEBAUTHN_PLAN.md).

## License compliance

- Project license: [LICENSE.md](LICENSE.md)
- Third-party dependency notices: [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)
- Regenerate notices: `make third-party-notices`

## Contributing

PocketBase is MIT licensed and this fork follows the same spirit.

- Upstream project: [pocketbase/pocketbase](https://github.com/pocketbase/pocketbase)
- Upstream contribution guidelines: [CONTRIBUTING.md](CONTRIBUTING.md)
- Fork sync docs: [FORK_SYNC.md](FORK_SYNC.md)

If a contribution is generally applicable to PocketBase core, please consider proposing it upstream first.

## Credit

Huge thanks to the upstream PocketBase project for the architecture, developer experience, and continued maintenance. This fork is built to extend that work respectfully and stay aligned with upstream as closely as practical.
