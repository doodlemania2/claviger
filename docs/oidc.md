# Claviger as an OpenID Connect provider

Claviger can sign users in to other applications (Grafana, oauth2-proxy, Outline, your own apps) with
OpenID Connect. Users sign in on a Claviger-hosted page with the same methods they use everywhere else:
password, passkey, and an authenticator-app code where the account has one.

It is built on [zitadel/oidc](https://github.com/zitadel/oidc) (`pkg/op`), with Claviger-owned storage,
sign-in pages and key management. Design notes: `docs/spikes/71-oidc-provider.md`.

**Nothing changes until you register a client.** The routes exist, but `/oidc/authorize` rejects unknown
clients, and there is no background work: no cron, no key generation, no writes on idle traffic.

## Requirements

- **The settings encryption key.** Signing keys are stored in `_signingKeys`, encrypted with it
  (`PB_ENCRYPTION_KEY`, 32 characters, passed as `--encryptionEnv`; see the README). Without it the
  provider **does not start** outside dev mode: no `/oidc` routes, one log line
  ("OpenID Connect provider is disabled: ..."), and key creation is refused. In dev mode (`--dev`) keys may be
  stored unencrypted, with a warning. Losing or changing the key makes the stored signing keys unreadable;
  create new ones with `claviger oidc rotate-keys`.
- **The application URL** (Settings, Application) must be the public `https://` URL. The issuer is
  `<application URL>/oidc`. `http://` is accepted only for localhost and in dev mode.

## Register a client

Admin UI: **OpenID Connect**, **New client**. Or the API (superuser token):

```sh
curl -X POST https://auth.example.com/api/oidc/clients \
  -H "Authorization: $SUPERUSER_TOKEN" -H "Content-Type: application/json" \
  -d '{
    "name": "Grafana",
    "authCollection": "users",
    "redirectUris": ["https://grafana.example.com/login/generic_oauth"],
    "postLogoutUris": ["https://grafana.example.com/login"],
    "firstParty": true
  }'
```

The response carries `clientId` and, for confidential clients, `clientSecret`. **The secret is shown once**;
only a hash is stored. `POST /api/oidc/clients/{id}/secret` replaces it.

| Field | Meaning |
| --- | --- |
| `authCollection` | The one auth collection whose users can sign in to this client (not `_superusers`). |
| `public` | No secret (single-page and native apps). PKCE is always required for public clients. |
| `redirectUris` | **Exact match**, no wildcards. `https` only; `http` only for `localhost`/`127.0.0.1`. Native apps may use a loopback URI (any port, RFC 8252) or a reverse-domain scheme. |
| `postLogoutUris` | Allowed `post_logout_redirect_uri` values for RP-initiated logout. |
| `scopes` | Allowed scopes. Empty means `openid profile email offline_access`. |
| `firstParty` | Skips the consent screen. Leave it off for anything you do not run yourself. |
| `requirePkce` | Defaults to on; confidential clients are asked for PKCE too (OAuth 2.1). |
| `accessTokenType` | `jwt` (default, 5 minutes) or `opaque` (use introspection). |
| `signingAlg` | `RS256` (default) or `ES256`: the algorithm of the ID and access tokens issued to the client. |
| `disabled` | Refuses the client everywhere without deleting it. |

Clients are the `_oauthClients` system collection (superusers only). Changes are audited; the secret hash is
redacted from the audit log and never returned by the records API.

## Endpoints

All under the issuer `https://<host>/oidc`:

| Endpoint | Path |
| --- | --- |
| Discovery | `/oidc/.well-known/openid-configuration` (also `/.well-known/openid-configuration`, which redirects) |
| JWKS | `/oidc/keys` |
| Authorization | `/oidc/authorize` (code flow, PKCE `S256`) |
| Token | `/oidc/oauth/token` (`authorization_code`, `refresh_token`) |
| UserInfo | `/oidc/userinfo` |
| Introspection | `/oidc/oauth/introspect` (RFC 7662; confidential clients) |
| Revocation | `/oidc/revoke` (RFC 7009) |
| End session | `/oidc/end_session` (RP-initiated logout) |
| Sign-in, consent | `/oidc/login`, `/oidc/consent` (hosted pages) |

Client authentication at the token endpoint: `client_secret_basic` and `client_secret_post` for confidential
clients, `none` for public ones. The implicit and hybrid flows are **not offered** (discovery says so, and a
client asking for `id_token` or `code id_token` is refused). The `password`, `client_credentials` and device
grants are not offered either.

Management API for the admin UI (superuser only): `GET /api/oidc/status`, `POST /api/oidc/clients`,
`POST /api/oidc/clients/{id}/secret`, `GET /api/oidc/keys`, `POST /api/oidc/keys/rotate`.

## How sign-in works

1. The application sends the browser to `/oidc/authorize` (with PKCE). Claviger validates it and redirects to
   `/oidc/login?authRequestID=...`.
2. The hosted page signs the user in against the client's auth collection through the ordinary endpoints
   (`auth-with-password`, passkey login, `auth-with-totp` for the second factor). If this browser already has a
   [single sign-on session](#single-sign-on) the page skips the credentials (see below).
3. The page sends the resulting Claviger token to `POST /oidc/login/finish`. Claviger accepts it only when it is
   a **fresh sign-in with a session** (not an API key, not a token that predates sessions, not older than
   15 minutes) of the client's collection, and the account is not banned.
4. Non-first-party clients get a consent screen; the grant is remembered per user, client and scopes, and
   audited. Declining redirects to the client with `error=access_denied`.
5. The browser resumes at `/oidc/authorize/callback`, which issues the code and redirects to the client.

The authorize request id is a bearer value, so the first load of the sign-in page binds the request to that
browser with a per-request `HttpOnly` cookie. Finish, consent and callback refuse any other browser.

The hosted pages use external scripts and styles only; the CSP is `default-src 'none'; script-src 'self'; ...`
with no `unsafe-inline`.

**CAPTCHA.** When Settings, CAPTCHA requires a token for password sign-in (`login`) or passkey sign-in
(`passkey_login`), the sign-in page reads the widget settings from the collection's `auth-methods` response,
renders the provider widget, and sends the token in `X-Captcha-Token` with `auth-with-password` and passkey
`login-begin`, the same way `/passkeys` does. The provider script is loaded only in that case, and the page's
CSP then gains the provider's domains (script, frame, XHR and, for hCaptcha, style) and nothing else: the same
narrow table `/passkeys` and the admin UI use, with still no `unsafe-inline`. A deployment without a CAPTCHA
gets exactly the strict policy above. Other pages (consent, errors) never get the extension. A provider outage
follows `failClosed` as everywhere else (open by default, so a church is not locked out of sign-in).

## Single sign-on

After a sign-in on `/oidc/login` the browser gets one cookie, `claviger_oidc_sso`: `HttpOnly`, `SameSite=Lax`,
`Secure` on https, `Path=/oidc`, valid 12 hours. Its value is 32 random bytes and **nothing else**: no token, no
id, no claim. The server stores only its SHA-256 in `_oauthSso`, next to the id of the Claviger session (`sid`)
the sign-in created. A later `/oidc/authorize` from the same browser, for another client (or the same one), is
completed by the sign-in page without asking for credentials: it re-checks that the Claviger session behind the
cookie is alive and the account is not banned, then issues the code with the original `auth_time`, `acr`, `amr`
and the same `sid` (so revoking that session ends every client's refresh chain at once).

- **Consent** is still asked for any client that is not first party, once per user, client and scopes.
- **`prompt=none`** succeeds silently when the session is valid. Otherwise the client receives `login_required`
  (no session, expired, revoked, other account type or collection) or `consent_required`, as a redirect, with no
  page. `none` combined with another value is `invalid_request`.
- **`prompt=login`**, **`prompt=select_account`** (there is no account chooser) and a **`max_age`** older than the
  original sign-in (`max_age=0` always) ignore the session and ask for credentials. That sign-in replaces the
  session: new cookie, old row deleted. A sign-in that happened while the request waited always satisfies
  `max_age`/`prompt=login`.
- **`prompt=consent`** shows the consent page again for a client that is not first party.
- The session **ends** when the Claviger session is revoked or expires (the user, an administrator, a password
  change, a ban), when the account is banned or deleted (it is re-checked on every use, so this is immediate),
  after 12 hours, and on `end_session`: the row and the Claviger session go and the cookie is cleared, with or
  without an `id_token_hint` (a bare logout request from the browser ends the browser's own session; a hostile
  page can therefore sign a user out, which is the usual logout-CSRF trade-off).
- A session belongs to one auth collection. Signing in to a client of another collection replaces it.
- The 12 hours are deliberately shorter than a Claviger session: the cookie lives in a browser that may be
  shared, while the long-lived credential belongs to the relying party (and is bounded by the refresh rules
  below).

A deployment with no registered clients never sets the cookie: only a sign-in through `/oidc/login` does.

## Rate limits

The protocol endpoints have their own limits. They are **always on**, independent of Settings, Rate limits (like
the passkey login-begin throttle), and use the persistent counters, so a restart or a rollout does not refill a
spent budget. They do nothing, and write nothing, until the first client is registered.

| Endpoint | Per IP / minute | Per client / minute |
| --- | --- | --- |
| `/oidc/authorize` | 120 | 1200 |
| `/oidc/oauth/token` | 120 | 600 |
| `/oidc/oauth/introspect` | 600 | 1200 |
| `/oidc/revoke` | 60 | 300 |
| `/oidc/userinfo` | 300 | 60 per access token |

Why these numbers: a congregation has a few hundred people behind one NAT address and a few relying parties
whose servers each share one address, so the per-IP budgets leave room for a Sunday morning but stop a loop or
a sweep within a minute. The introspection budget is high because a resource server holding opaque tokens may
call it per request. The per-client budget is a ceiling against a runaway client, not an abuse control: a client
id is public, so anyone could spend it, and it is therefore generous and only counted for registered clients.
UserInfo carries no client id, so its "client" budget is per access token.

**Failed client authentication** (a `401` from the token, introspection or revocation endpoint: wrong or missing
secret) is counted separately and is far stricter: **10 failures per 5 minutes per (IP, client)** and **30 per 5
minutes per IP**. When either is spent the three endpoints answer `429` before looking at the credentials, until
the window ends. The key is the pair, not the client alone, on purpose: a stranger who knows a client id cannot lock
the real client out by failing on purpose. (The token endpoint checks the authorization code before the client's
secret, so guessing a secret there needs a valid code; introspection and revocation check the client first.)

A refused request is `HTTP 429` with `Retry-After` (seconds) and `Cache-Control: no-store`. RFC 6749 has no 429
and the `slow_down` error of RFC 8628 is for device-flow polling, so the body uses the closest registered OAuth
error code, `temporarily_unavailable` (RFC 6749, 4.1.2.1):

```json
{"error":"temporarily_unavailable","error_description":"Too many requests. Retry after 31 seconds."}
```

`/oidc/authorize` is a browser navigation, so it answers with the hosted error page and the same status and
header. Budget refusals are logged by the application logger (`Info`; a client whose authentication keeps
failing is a `Warn`).

## Tokens and claims

- **Authorization code**: single use, valid for 1 minute, stored hashed. Using a code twice fails and revokes
  the tokens it produced.
- **Access token**: JWT, 5 minutes (or opaque). JWT claims: `iss`, `sub`, `aud`, `exp`, `iat`, `nbf`, `jti`,
  `client_id`, `scope`, `sid`, `collection`.
- **ID token**: 1 hour.
- **Refresh token**: opaque, 30 days, stored hashed, **rotated on every use**. Presenting a token that was
  already rotated is treated as theft: the whole chain is revoked and an audit entry is written. A refresh
  also fails when the Claviger session behind it is revoked or expired, or the account is banned or deleted.
  **A refresh extends the session.** Each accepted refresh sets the Claviger session to end the collection's auth
  token duration from now, never later than **30 days after the sign-in** (an absolute cap, `MaxSessionLifetime`)
  and never earlier than it already ended. So an app in daily use keeps its user signed in for up to 30 days, then
  the user signs in again; an app that is not used for longer than the auth token duration loses the session. The
  cap is a constant, not a per-client setting (one knob fewer for a congregation; say so if a client needs its own).
  A revoked, expired or banned session is never revived by a refresh: the check comes first. The single sign-on
  cookie is not extended (12 hours from sign-in).

| Claim | Value |
| --- | --- |
| `sub` | The record id. |
| `collection` | The name of the auth collection (the pair is the identity). |
| `email`, `email_verified` | With the `email` scope. |
| `name`, `preferred_username`, `updated_at` | With the `profile` scope, when the collection has `name` / `username` fields. |
| `sid` | The Claviger session id (#93). Revoking the session ends the refresh chain. |
| `auth_time` | When the user signed in. |
| `acr` | `urn:claviger:acr:passkey` (fresh passkey assertion), `urn:claviger:acr:mfa` (TOTP or MFA completed), else `urn:claviger:acr:password`. Kept across refreshes. |
| `amr` | `["hwk","user"]` for a passkey; `["pwd"]`; `["pwd","otp","mfa"]` for password plus authenticator app; `["pwd","mfa"]` for collection MFA; `["otp"]` for email OTP. |

Access tokens are not revocable offline: a revoked session or ban stops refresh, userinfo and introspection at
once, but a JWT already issued stays valid until its 5 minutes are up. Use an opaque token plus introspection
for instant cut-off.

## Notes for client authors

- **Native apps and loopback redirects (RFC 8252, 7.3).** For a *public* client (the library treats it as a
  native app), a registered `http://127.0.0.1/callback` (or `localhost`, `[::1]`) also accepts the same path and
  query on any port: the app picks a free port at run time. Only loopback hosts qualify, the path and query must
  match exactly, and a registered `https` or custom-scheme URI is matched exactly as before. A *confidential*
  client is always an exact match, port included.
- **A failed PKCE check burns the code.** The authorization code is consumed by the first token request that
  presents it, whatever happens next: a wrong `code_verifier` or a wrong client secret leaves the code spent
  (`invalid_grant` afterwards), so a client that guesses cannot retry the same code and must start a new
  authorization. This is deliberate (single use, RFC 6749 4.1.2): it gives an attacker holding a stolen code one
  shot, not many. Presenting an already-used code a second time also revokes the tokens it produced.

## Signing keys

Keys live in `_signingKeys`, one active key per algorithm (`RS256`, `ES256`), created on first use. The JWKS
publishes the active keys and the retiring keys that are still inside their grace period.

```sh
claviger oidc rotate-keys                  # new key for every algorithm in use; old ones stay 7 days
claviger oidc rotate-keys --alg ES256 --grace 48h
```

or **Rotate keys** in the admin UI. The old key keeps verifying tokens it signed until its retirement time,
after which it leaves the JWKS (a later rotation deletes keys retired more than 30 days ago). Rotations are audited; the private key is
redacted from audit entries and never returned by an API.

## Logout

`/oidc/end_session?id_token_hint=...&post_logout_redirect_uri=...&state=...` revokes the Claviger session named
by the hint's `sid` (so every client holding that session loses its refresh chain), then redirects to the
registered `post_logout_redirect_uri`. Without an `id_token_hint` nothing can be identified and only the
redirect happens.

## Audit

- Client and key changes (create, update, delete, rotation) are audited like any record change.
- A granted consent is audited (actor: the user).
- Refresh-token reuse, authorization-code reuse and RP-initiated logout write `oidc_revoke_family` /
  `oidc_logout` entries.
- Token issuance is **not** audited (too noisy); the token tables are written with plain SQL like `_sessions`.

## Example: oauth2-proxy

```
--provider=oidc
--oidc-issuer-url=https://auth.example.com/oidc
--client-id=<clientId> --client-secret=<clientSecret>
--redirect-url=https://app.example.com/oauth2/callback
--code-challenge-method=S256
--scope="openid profile email"
```

## Limitations (milestone 1)

- No DPoP, PAR, device flow (#96), dynamic client registration, `client_credentials`, back-channel logout or
  token exchange.
- No account chooser (#94): a browser holds one single sign-on session, and `prompt=select_account` asks for
  credentials.
- The hosted pages are English only (#97). Email OTP and the OAuth2 "sign in with" providers are not offered
  on the hosted page.
- The library logs through the process-global `slog` default (it ignores its `WithLogger` option), and the only
  way to route that into the application logger is to replace the global default, which would also capture every
  other package's `slog` output. Claviger does not do that: the library's lines (protocol errors such as a bad
  `redirect_uri`) go to the process's standard slog destination, while Claviger's own OIDC log lines (rate limits,
  failing client authentication, key and single sign-on problems) use the application logger.
- Formal OpenID conformance (the Basic OP profile) has not been run yet; the plan is to run the suite in CI.
