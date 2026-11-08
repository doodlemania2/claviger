# Spike: Claviger as an OpenID Connect provider (#71)

Date: 2026-11-07. Status: research only. No Go code in this repo changed.

## Recommendation

Use `github.com/zitadel/oidc/v3` (`pkg/op`).

It needs a little more glue code to read, but it ships the whole HTTP surface (discovery, JWKS, authorize, token, userinfo, introspection, revocation, end-session, device flow, back-channel logout). It adds 9 modules to the build, against 25 for fosite. It is the only one of the two that has the device flow (#96) in the box. It is released weekly. Fosite has had no release for 11 months and has no device flow, no DPoP and no HTTP layer.

The cost: zitadel/oidc has no DPoP and no PAR, and the `op` package is not itself OpenID-certified as an OP. We must run the conformance suite ourselves and fix what fails. We would own DPoP.

## What was built

Two throwaway prototypes, outside the repo, each a Go module that imports `github.com/doodlemania2/claviger` through a `replace` directive. Each wires the library into a real Claviger app (bootstrapped `BaseApp`, real migrations, real router) and stores everything in PocketBase collections. Scratch location (not committed): the session scratchpad, dirs `zit/` and `fos/`.

Each has two Go tests. Both libraries pass both tests.

1. `TestAuthCodePKCEEndToEnd` drives the flow with `httptest` and a client that follows redirects by hand:
   - discovery document, JWKS;
   - `GET /oidc/authorize` with S256 PKCE, then 302 to the Claviger-hosted `/oidc/login`;
   - sign-in through the existing `POST /api/collections/users/auth-with-password`, then `POST /oidc/login/finish` with the resulting Claviger token;
   - code issued, wrong `code_verifier` rejected, correct exchange returns `id_token`, `access_token` and `refresh_token`;
   - ID token signature checked against the JWKS, claims checked (`iss`, `sub`, `nonce`, `email`, `name`, `acr`, `amr`, `sid`);
   - userinfo, refresh with rotation (old refresh token dies), code replay rejected;
   - `core.RevokeSessions` (#93) makes the next refresh fail;
   - key rotation puts two keys in the JWKS.
2. `TestPasskeyStepUpMapsToAMR` signs in with a token from `NewPasskeyAuthToken` (it carries `passkeyAt`). The ID token gets `acr=urn:claviger:acr:passkey` and `amr=["hwk","user"]`.

Not covered: a real WebAuthn ceremony in the browser (the login page is a stub that calls the existing endpoints), consent screen, RP-initiated logout, confidential clients, the OpenID conformance suite.

## Comparison

| | zitadel/oidc v3 | ory/fosite |
|---|---|---|
| Prototype result | Auth code + PKCE passes end to end | Passes end to end |
| What it is | Complete OP: provides `http.Handler` | OAuth2 toolkit: we write every HTTP handler |
| Glue code (non-blank, non-comment Go lines, storage + mount) | 627 (of which 89 is the shared collection helper) | 550 plus the same helper, but the login/resume handlers, discovery and JWKS are ours |
| Test code | 299 lines | 309 lines |
| Discovery doc | Built by the library | Hand-written JSON |
| JWKS | Library serves `Storage.KeySet` | Hand-written |
| Login delegation | Library redirects to `Client.LoginURL(id)`, then `op.AuthCallbackURL` | We park the query ourselves and re-run `NewAuthorizeRequest` on resume |
| PKCE | S256; enforced for public clients by library, we reject a missing challenge in `CreateAuthRequest` | `EnforcePKCE: true` |
| Refresh tokens | Rotation is our storage code | `RotateRefreshToken` hook; reuse detection partly built in |
| Code replay revokes issued tokens | No (we would add it) | Yes, built in (verified in the prototype) |
| `acr` after refresh | Lost: `RefreshTokenRequest` has no ACR | Kept (stored session) |
| ID token private claims | Only through `UserInfo.AppendClaims` (found by trial) | `Session.Claims.Extra` |
| Weak `state` | Accepted | Rejected below 8 chars (tunable) |
| Spec coverage (source) | Code, PKCE, refresh, JWT profile (RFC 7523), token exchange (8693), client credentials, introspection (7662), revocation (7009), device flow (8628), back-channel logout, end session | Code, PKCE, refresh, implicit/hybrid, client credentials, ROPC, introspection, revocation, JWT assertion grant (7523), PAR (9126) |
| Device flow (#96) | Yes, `DeviceAuthorizationStorage` | No (grep finds nothing) |
| DPoP | No (no issue found either) | No (issue #829, closed without implementing) |
| PAR | No | Yes |
| Dynamic client registration | No | No |
| Back-channel logout | Yes (OP side) | No |
| Implicit flow | Advertised in `grant_types_supported` and not removable by config; we can strip it in an HTTP interceptor | Off unless the factory is composed (we left it out) |
| OpenID certification | Library README: the RP part is certified (basic, config). OP certification is listed as a to-do. | Library not certified. Ory Hydra, built on it, is reported as certified (not verified here: the certification site did not load usable data). |
| License | Apache-2.0 | Apache-2.0 |
| MIT compatible | Yes. Apache-2.0 deps may be used in an MIT project. Ship their LICENSE/NOTICE texts with binaries. | Same |
| Latest release | v3.51.17, 2026-11-02; weekly cadence, semantic-release | v0.49.0, 2024-12-12; last commit 2025-07 |
| Open issues | 51 | 61 |
| Stars | 1.9k | 2.6k |
| Dependency weight (modules compiled that Claviger does not already use) | 9: doublestar, chi, go-jose v4, securecookie, gu, httpforwarded, rs/cors, zitadel/schema, oidc | 25: includes ory/x, logrus, gobuffalo/pop, gogo/protobuf, ristretto, zipkin/jaeger exporters, go-jose v3 |
| `go.sum` lines in the prototype | 240 | 972 |
| Binary size (CGO off, Claviger core + the library, no UI) | 46.2 MB | 49.7 MB |
| CGO-free | Yes (built with `CGO_ENABLED=0`) | Yes |
| Logging | Global `slog` | logrus via `ory/x` |

Notes on the table:
- Line counts are rough. The two prototypes are not equally complete. Fosite's total grows once discovery, JWKS, consent and end-session are polished. Zitadel's grows with device flow storage.
- Fosite's JWKS needs go-jose v3 and zitadel's needs v4. They never coexist in one build, so this does not matter once we pick.
- The fosite module graph is large because of test-only requirements. The compiled set (25 modules) is what matters, plus `go.sum` growth.

## How Claviger concepts map

### Storage

Both prototypes used one helper: a collection per object kind with `key` (unique), `aux` (secondary lookup), `data` (JSON) and `expires`. Production should use typed fields and the plain-SQL writes that `core/session.go` uses, so token issuance does not fire model hooks or add audit rows.

| Object | Collection | Zitadel interface | Fosite interface |
|---|---|---|---|
| Clients | `_oauthClients` | `GetClientByClientID`, `AuthorizeClientIDSecret` | `GetClient` (`fosite.Client`) |
| Pending authorize request | `_oauthAuthRequests` | `CreateAuthRequest`, `AuthRequestByID`, `SaveAuthCode`, `DeleteAuthRequest` | our own table (the "pending" record) |
| Codes | `_oauthCodes` | `AuthRequestByCode` | `Create/Get/InvalidateAuthorizeCodeSession`, PKCE and OIDC session stores |
| Access tokens | `_oauthTokens` | `CreateAccessToken`, `SetUserinfoFromToken` | `Create/Get/DeleteAccessTokenSession` |
| Refresh tokens | `_oauthRefreshTokens` | `CreateAccessAndRefreshTokens`, `TokenRequestByRefreshToken`, `RevokeToken` | `Create/Get/Delete/RotateRefreshTokenSession`, `RevokeRefreshToken` |
| Signing keys | `_signingKeys` | `SigningKey`, `KeySet`, `SignatureAlgorithms` | our own `keyGetter` and JWKS handler |

Zitadel's interface is bigger (about 25 methods) but each method is small and named for the protocol step. Fosite's is about 25 too, but the methods deal in a generic `Requester` that we must serialize and rebuild ourselves.

### Keys and rotation

Both prototypes keep keys in a table with `active` and `retireAfter`. Rotation marks the old key inactive with a grace period and creates a new one. The JWKS lists every key that is active or inside its grace period. Test confirms two keys after rotation. Zitadel asks for the JWKS from `KeySet` and signs with `SigningKey`, so this is natural. Fosite signs with whatever `keyGetter` returns and needs the `kid` set by hand in the session headers.

ES256/EdDSA: both libraries accept other `jose` algorithms. The prototype used RS256 only. Add a `alg` column.

### Sessions (#93) and `sid`

At login-finish the handler parses the (already verified) Claviger token, reads the `sid` claim, and stores it on the authorize request. It ends up in the ID token as `sid` and on every refresh token. On refresh, the storage calls `core.ValidateSession`. A revoked, expired or banned session kills the refresh token on the next use. Tested in both prototypes. Access tokens stay valid until they expire (5 minutes); introspection can check the session too if we want instant cut-off.

This is the natural base for back-channel logout: revoke a session, find refresh tokens with that `sid`, and POST a logout token to each client.

### Passkey step-up and `acr`/`amr`

`apis.HasFreshPasskey(e, 5*time.Minute)` decides the values at login-finish:

- password: `amr=["pwd"]`, `acr=urn:claviger:acr:password`
- fresh passkey: `amr=["hwk","user"]`, `acr=urn:claviger:acr:passkey`
- TOTP or MFA completion would add `mfa` (not built).

Gap in zitadel: `acr` is lost on refresh because `RefreshTokenRequest` has no ACR. Fix: keep it in a private claim, or patch upstream.

A client can ask for `acr_values` or `max_age`. We should honor `max_age` by forcing a new sign-in, and treat a request for the passkey ACR as "run step-up". Neither is in the prototype.

### Login delegation

Flow, same in both:

1. `GET /oidc/authorize` is validated by the library, which creates and stores the request.
2. 302 to `/oidc/login?authRequestID=…` (Claviger-hosted page; the stub in the prototype uses `/api/passkeys-client.js` and `auth-with-password`).
3. The page signs in with existing endpoints and gets a Claviger token (MFA and OTP flows work unchanged because they end in the same token).
4. `POST /oidc/login/finish` with `Authorization: <token>` and the request id. Handler is guarded by `apis.RequireAuth("users")`.
5. Browser is sent to the library's callback (zitadel) or our resume handler (fosite). Code is issued and the browser goes to the client's `redirect_uri`.

Key excerpt (zitadel prototype, finish handler):

```go
amr, acr := []string{"pwd"}, "urn:claviger:acr:password"
if apis.HasFreshPasskey(e, 5*time.Minute) {
    amr, acr = []string{"hwk", "user"}, "urn:claviger:acr:passkey"
}
claims := jwt.MapClaims{}
_, _, _ = jwt.NewParser().ParseUnverified(e.Request.Header.Get("Authorization"), claims)
sid, _ := claims[core.TokenClaimSessionId].(string)
st.CompleteLogin(body.AuthRequestID, e.Auth.Id, sid, amr, acr, time.Now())
return e.JSON(200, map[string]string{"redirect": "/oidc" + op.AuthCallbackURL(provider)(ctx, id)})
```

Mounting under the PocketBase router (zitadel):

```go
std := http.StripPrefix("/oidc", provider)
se.Router.Any("/oidc/{path...}", func(e *core.RequestEvent) error {
    std.ServeHTTP(e.Response, e.Request); return nil
})
```

Gotchas found while building it:
- `op.AuthCallbackURL` returns a path without the mount prefix when called outside the provider's own handler. We add `/oidc` ourselves.
- `op.WithAllowInsecure()` is needed for an `http://` issuer in tests only.
- A custom `Client` must return `DevMode() == true` to allow `http://127.0.0.1` redirects. Production clients must not.
- Zitadel does not require PKCE for confidential clients. We reject authorize requests without `code_challenge` in `CreateAuthRequest` (OAuth 2.1 behavior).
- The auth request id is a bearer secret. Production needs a cookie bound to the browser so another browser cannot finish someone else's request (login CSRF).
- Fosite: the authorize endpoint must be called twice (before and after login). We park the query string and rebuild the request on resume. Hydra solves this with its login/consent API; we write it.
- Fosite: an opaque HMAC access token needs introspection. JWT access tokens need extra strategy wiring. Zitadel supports JWT access tokens per client (`AccessTokenTypeJWT`), used in the prototype.
- Fosite correctly revoked tokens when a code was replayed. Zitadel does not, so add it.

## Proposed architecture

New fork-only packages (no edits to upstream files beyond route binding and migrations):

- `oidc/` provider wiring: `Config`, mount function, discovery tweaks, middleware (CORS for userinfo/token, rate limits through the existing `collectionPathRateLimit` style).
- `oidc/store/` the zitadel `Storage` over the collections below (typed fields, plain SQL for token writes).
- `oidc/login/` login-finish handler, consent handler, account chooser hooks (#94), claim mapping.
- `oidc/keys/` signing key generation, rotation cron, retire job.
- `oidc/pages/` hosted HTML (login, consent, device code, logged-out) with embedded templates, following `apis/passkeys_page.go`.
- Admin UI: Svelte screens for clients, keys, consent grants (under `ui/`; rebuild `ui/dist`).

System collections (hidden, like `_sessions`):

- `_oauthClients`: id, secret hash, redirect URIs, post-logout URIs, grant types, allowed scopes, auth collection, token lifetimes, access token type, branding fields, `requireConsent`, `firstParty`, `public`.
- `_oauthAuthRequests`: short-lived (10 min), holds the pending request and login result.
- `_oauthCodes`: code hash, request data, used flag.
- `_oauthRefreshTokens`: token hash, client, subject, `sid`, scopes, `amr`, `acr`, `authTime`, rotated-from, expires, revoked.
- `_oauthConsents`: user, client, scopes, granted at (so consent is asked once).
- `_signingKeys`: kid, alg, private key (encrypted with the app encryption env when set), created, active, `retireAfter`.
- Later: `_oauthDeviceCodes` (#96), `_oauthResources` (APIs and scopes).

Only hashes of codes and refresh tokens are stored. Private signing keys should be encrypted at rest; the prototype stored PEM in the clear.

Routes: `/oidc/*` for the protocol (issuer = `https://host/oidc`, or the site root if the owner prefers), `/oidc/login`, `/oidc/consent`, `/oidc/logged-out`, `/.well-known/openid-configuration` redirect or proxy at the site root for clients that insist on it.

Subject: use the record id, and put the collection id in a `collection` claim, since two auth collections can share ids only by chance but a stable pair is safer. Open question below.

## Risks

- Not certified. Passing the Basic OP profile (a hard requirement in #71) may expose library gaps. Mitigation: run the suite in CI against a test instance from milestone 1.
- The zitadel `op` package has a "limited availability for feature reviews" notice. We may carry local patches (ACR on refresh, implicit grant advertisement). Mitigation: keep patches small, upstream them, and pin with `go.mod`.
- zitadel/oidc is maintained mainly for ZITADEL. If their needs diverge, the cadence could change. Mitigation: it is Apache-2.0, small, and forkable.
- Security surface is new and large: login CSRF, open redirects, token hashing, consent bypass, refresh reuse. Needs a dedicated review.
- Dependency growth in a public MIT fork (9 modules). Low.
- Zitadel has no DPoP: we write it. RFC 9449 touches token issuance, the resource side, and nonce handling.
- `go-jose` major versions differ between libraries. Irrelevant if we pick one.
- Passkey UX: a request needing `acr=passkey` must trigger step-up. Easy to get wrong.

## Implementation plan

Sizes are rough: S under 2 days, M 3 to 5 days, L 1 to 2 weeks.

### Milestone 1: core OP (issue #71), size L

1. Collections and migrations: clients, auth requests, codes, refresh tokens, keys. (M)
2. Storage over them, with hashing and session binding. (M)
3. Mount, discovery, JWKS, key rotation job. (S)
4. Hosted login page reusing password, OTP and passkey; login-finish; CSRF cookie binding. (M)
5. Consent screen and `_oauthConsents`; claim mapping from auth collection fields; custom claims hook. (M)
6. RP-initiated logout, session revocation on logout. (S)
7. Introspection, revocation, client credentials. (S)
8. Admin UI for clients and keys. (M)
9. Conformance suite in CI (Basic OP, then Config OP), fix findings. (L, open-ended)
10. Done check: Grafana or oauth2-proxy signs in with a passkey.

### Milestone 2: #94 account chooser, size M

Cookie of signed-in accounts per browser, chooser page, `prompt=select_account`, `prompt=login`, `max_age`. Builds on the session table. Mostly pages and a small cookie format.

### Milestone 3: #96 device flow, size M

Zitadel already has the protocol and `DeviceAuthorizationStorage`. We add the table, the user-code page with passkey sign-in, rate limits, and the discovery entry. This is the main reason fosite loses.

### Milestone 4: #97 localized pages, size M

Message catalogs, `ui_locales`, `Accept-Language`, RTL CSS. Zitadel passes `UILocales` to the login URL step, and `SupportedUILocales` feeds discovery. Independent of the library choice. Needs templates from M1 to be done first.

### Milestone 5: add-ons

- Back-channel logout (library has the OP side): S to M.
- Dynamic client registration (RFC 7591) with mandatory consent for self-registered clients: M.
- DPoP (RFC 9449) for access and refresh tokens: L. Own implementation; check upstream first.
- PAR (RFC 9126): M. Not in zitadel.
- API/resource registry, per-client token lifetimes, per-client branding: M.

## Open questions for the owner

1. Library: zitadel/oidc (recommended) or fosite? Choose fosite only if PAR matters more than device flow, dependency weight and release cadence. Fosite also forces us to write all the HTTP layer.
2. Issuer URL. Options: a) `https://host/oidc` (what the prototype uses; keeps routes clear of PocketBase's `/api`); b) site root, so `/.well-known/openid-configuration` works with no redirect. Recommend a), plus a root `.well-known` redirect.
3. Subject format. Options: a) record id; b) `collectionId:recordId`; c) a separate stable UUID per user per client (pairwise). Recommend a) with a `collection` claim now; leave pairwise for later.
4. Which auth collections can sign in? Options: one collection per client (stored on the client), or any. Recommend one per client.
5. Consent. Options: always ask for third-party clients, skip for `firstParty` clients. Recommend that, and mandatory for dynamically registered clients.
6. Access token format. Options: JWT (resource servers verify offline, cannot be revoked early) or opaque with introspection. Recommend JWT with a 5-minute life, opaque per client as an option.
7. Signing algorithm default. RS256 maximizes compatibility; ES256 is smaller and faster. Recommend RS256 by default, ES256 selectable.
8. Private key storage. Options: plaintext in `_signingKeys`; encrypted with the app encryption env; external KMS later. Recommend encryption when the env is set, and refusing to create keys in plaintext in production mode.
9. Conformance gate. Is "passes Basic OP in CI" enough for the first release, or do you want a formal OpenID certification submission (paid, and needs a public test instance)? Recommend running the suite in CI first and deciding on certification after M1.
10. DPoP: build it ourselves in M5, or wait for upstream? Recommend building it only once a real client needs it.
