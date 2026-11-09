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
| `allowDeviceFlow` | Off by default. Lets the client use the [device authorization grant](#device-authorization-grant) (RFC 8628). |
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
| Token | `/oidc/oauth/token` (`authorization_code`, `refresh_token`, and `urn:ietf:params:oauth:grant-type:device_code` for clients with `allowDeviceFlow`) |
| Device authorization | `/oidc/device_authorization` (RFC 8628) |
| UserInfo | `/oidc/userinfo` |
| Introspection | `/oidc/oauth/introspect` (RFC 7662; confidential clients) |
| Revocation | `/oidc/revoke` (RFC 7009) |
| End session | `/oidc/end_session` (RP-initiated logout) |
| Sign-in, consent, account chooser | `/oidc/login`, `/oidc/consent` (hosted pages; the chooser is the sign-in page when there is a choice to make) |
| Device verification | `/oidc/device` (hosted page where a person enters the code) |

Client authentication at the token endpoint: `client_secret_basic` and `client_secret_post` for confidential
clients, `none` for public ones. The implicit and hybrid flows are **not offered** (discovery says so, and a
client asking for `id_token` or `code id_token` is refused). The `password` and `client_credentials` grants are
not offered either. The device grant is advertised in discovery (`device_authorization_endpoint` and the grant
type) whenever the provider runs, because discovery is global; a client without `allowDeviceFlow` is refused at the
device endpoint with `unauthorized_client`.

Management API for the admin UI (superuser only): `GET /api/oidc/status`, `POST /api/oidc/clients`,
`POST /api/oidc/clients/{id}/secret`, `GET /api/oidc/keys`, `POST /api/oidc/keys/rotate`.

## How sign-in works

1. The application sends the browser to `/oidc/authorize` (with PKCE). Claviger validates it and redirects to
   `/oidc/login?authRequestID=...`.
2. The hosted page signs the user in against the client's auth collection through the ordinary endpoints
   (`auth-with-password`, passkey login, `auth-with-totp` for the second factor). If this browser already has a
   [single sign-on session](#single-sign-on-and-the-account-chooser) the page skips the credentials, or offers the account chooser (see below).
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

## Single sign-on and the account chooser

After a sign-in on `/oidc/login` the browser gets one cookie, `claviger_oidc_sso`: `HttpOnly`, `SameSite=Lax`,
`Secure` on https, `Path=/oidc`, valid 12 hours. A browser can be signed in to **several accounts at once** (a
person's work and personal account, a shared family device, a member account and an admin account), up to
**5**. Each account has its own server-side session: 32 random bytes and **nothing else** (no token, no id, no
claim), of which the server stores only the SHA-256 in `_oauthSso`, next to the id of the Claviger session (`sid`)
the sign-in created. The cookie holds the (up to five) values joined with `~`, which is not a base64url
character. A later `/oidc/authorize` from the same browser is completed by the sign-in page without asking for
credentials: it re-checks that the Claviger session behind the chosen account is alive and the account is not
banned, then issues the code with the original `auth_time`, `acr`, `amr` and the same `sid` (so revoking that
session ends every client's refresh chain at once).

Which account carries a request:

- **One account can** (the others belong to another auth collection, are not members of the client's
  organization, or are too old for `max_age`): it is used silently, as before. A browser with only one account
  behaves exactly like the old single session.
- **More than one can, and there is no `login_hint`**: the sign-in page is the **account chooser**: one button per
  account (initial, name, email, collection), "Use another account", a "Sign out" link per account and "Sign out of
  all accounts". Accounts that cannot be used for this client are listed greyed out with the reason.
- **`login_hint`** (an email, a username or a record id) picks the matching signed-in account without a page. With
  `prompt=select_account` the chooser opens with that account highlighted. A hint that names nobody signed in sends
  the user to the sign-in page, with the identity prefilled.
- **`prompt=select_account`** always shows the chooser when the browser has any account (the sign-in page when it
  has none).
- **`prompt=none`** succeeds silently when exactly one account can carry the request (or `login_hint` names one).
  With several and no resolving hint the client receives `account_selection_required`; with none,
  `login_required`; if consent is missing, `consent_required`. All are redirects, with no page, and say nothing
  about the accounts.
- **Consent** is still asked for any client that is not first party, once per user, client and scopes.
- **`prompt=login`**, "Use another account" and a **`max_age`** older than a sign-in (`max_age=0` always) ask for
  credentials. A sign-in replaces the browser's session for **that account** only (new cookie value, old row
  deleted) and adds to the others; a sixth account pushes out the oldest one from the browser (its Claviger session
  stays alive for the relying parties that hold it).
- **`prompt=consent`** shows the consent page again for a client that is not first party.

An account leaves the chooser when its Claviger session is revoked or expires (the user, an administrator, a
password change, a ban), when it is banned or deleted (re-checked on every use, so this is immediate), after 12
hours, when the chooser signs it out, and on `end_session`:

- with an `id_token_hint`, only the account named by the hint's `sid` signs out (its Claviger session and every
  OIDC token issued under it are revoked); the browser's other accounts stay, and the cookie is rewritten without it;
- without a hint, nothing says who is leaving, so every account of the browser signs out and the cookie is cleared
  (a hostile page can therefore sign a user out of all accounts, which is the usual logout-CSRF trade-off);
- the chooser's "Sign out" and "Sign out of all accounts" do the same for one or all accounts, and need the
  request's browser binding.

**Nothing about the other accounts reaches a relying party.** The accounts are listed only on the chooser page,
which is same-origin and sits behind the `HttpOnly` cookie and the per-request browser binding (the chooser's
select and sign-out endpoints refuse any other browser). Accounts are named there by the opaque id of their
single sign-on row, never by record id; codes, tokens, errors and redirects carry only the account that was chosen.

The 12 hours are deliberately shorter than a Claviger session: the cookie lives in a browser that may be shared,
while the long-lived credential belongs to the relying party (and is bounded by the refresh rules below).

The page text of the chooser is in `chooserText` (`oidc/chooser.go`) and `STRINGS` at the top of its script, so it
can be translated in one place.

## Device authorization grant

For CLIs, TVs, kiosks and anything else without a usable browser (RFC 8628). Off by default: set
`allowDeviceFlow` on the client (admin UI: "Allow the device flow"). A public client needs no secret; a
confidential client authenticates with HTTP Basic at both endpoints.

1. The device calls `POST /oidc/device_authorization` (`client_id`, optional `scope`) and gets `device_code`,
   `user_code` (`XXXX-XXXX`), `verification_uri` (`https://<host>/oidc/device`), `verification_uri_complete`
   (the same with `?user_code=...`, for a QR code), `expires_in` (600) and `interval` (5).
2. The person opens the page (on a phone, say) and types the code, or follows the QR link. The code is
   normalised, so `bcdf ghjk`, `BCDFGHJK` and `BCDF-GHJK` are the same.
3. They sign in with the hosted sign-in page (password, passkey, authenticator app, captcha: exactly the same
   page and checks as `/oidc/login`), or, if the browser already has signed-in accounts, pick one on the
   [account chooser](#single-sign-on-and-the-account-chooser) (the device flow always shows it, even for a single
   account). The sign-in is added to the browser's single sign-on accounts.
4. The page shows the client name, the scopes and the account, with **Allow** and **Cancel**. **Approval is always
   explicit**, even for a first-party client and a signed-in browser, because a device code can be phished; the
   page warns about it. (Consent is not stored: every device authorization asks again.)
5. Meanwhile the device polls `POST /oidc/oauth/token` with
   `grant_type=urn:ietf:params:oauth:grant-type:device_code`, `device_code` and the client credentials, and gets
   `authorization_pending`, `slow_down`, `access_denied`, `expired_token` or tokens: access token, ID token and,
   with `offline_access`, a refresh token.

Scopes are limited to what the client may use and always include `openid`. The tokens are ordinary ones: they
carry the `sid`, `acr`, `amr` and `auth_time` of the person's real sign-in, so revoking that Claviger session (or
signing out) ends them, and the refresh rules above apply.

**Storage.** `_oauthDeviceCodes` (system collection, superusers only, hashes hidden): one row per authorization
with the SHA-256 of the `device_code` and of the normalised `user_code` (never the codes), the client, scopes,
status (`pending`, `authenticated`, `approved`, `denied`, `consumed`), the poll state and, after sign-in, the
subject, `sid`, `acr`, `amr` and `auth_time`. It is separate from `_oauthRequests` because it is looked up by two
different secrets and carries poll state with a different lifecycle. Expired rows are purged with the other OIDC
tables when a new request comes in; an installation with no device traffic does no work.

**Limits and safety.**

- Codes: the `user_code` is 8 consonants (`BCDFGHJKLMNPQRSTVWXZ`: no vowels, no 0/O or 1/I/l; about 2.6e10
  possibilities) and lives **10 minutes**; the `device_code` is 128 random bits.
- Code entry on the verification page: **30 submissions per minute per IP**, and after **10 wrong or malformed
  codes in 15 minutes** an IP is refused (`429`, `Retry-After`) even for a right code. The counters are the
  persistent ones, so a restart does not refill them. Nothing is counted until a client exists.
- `/oidc/device_authorization`: 30 per minute per IP, 300 per client.
- Polling: faster than the interval gets `slow_down`, and the interval for that code grows by 5 seconds each time
  (up to 60). The token endpoint's own per-IP and per-client budgets apply on top.
- The first poll that sees an approval takes it: **one device_code, one set of tokens** (a later poll reads
  `expired_token`). An unauthenticated poll of a confidential client is refused (`401 invalid_client`) before it
  can use the approval up.
- The browser that enters the code is bound to the authorization by a cookie; a second browser cannot take it over,
  open the page for it, or finish its sign-in.
- Approve and Deny are audited (`oidc_device_approve`, `oidc_device_deny`, actor: the person, with the client,
  scopes and `sid`).
- If the person's session ends, the account is banned or leaves the client's organization between approval and
  the poll, the poll reads `access_denied`.

The page text is in `deviceText` (`oidc/device.go`) and `STRINGS` at the top of its script.

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
| `/oidc/device_authorization` | 30 | 300 |

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
registered `post_logout_redirect_uri`. Only that account signs out of the browser; its other accounts stay (see
[the chooser](#single-sign-on-and-the-account-chooser)). Without an `id_token_hint` nothing identifies the
account, so every account of the browser signs out, and the redirect happens.

## Audit

- Client and key changes (create, update, delete, rotation) are audited like any record change.
- A granted consent is audited (actor: the user).
- Refresh-token reuse, authorization-code reuse and RP-initiated logout write `oidc_revoke_family` /
  `oidc_logout` entries (one per account that signs out); the device flow writes `oidc_device_approve` /
  `oidc_device_deny`.
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

- No DPoP, PAR, dynamic client registration, `client_credentials`, back-channel logout or
  token exchange.
- A browser keeps at most 5 accounts; the JS SDK has no multi-account store yet (the chooser covers the hosted
  pages only).
- The device flow has no notification of the device when the person cancels (it polls and reads `access_denied`),
  and a confidential client must authenticate with HTTP Basic when it polls.
- The hosted pages are English only (#97). Email OTP and the OAuth2 "sign in with" providers are not offered
  on the hosted page.
- The library logs through the process-global `slog` default (it ignores its `WithLogger` option), and the only
  way to route that into the application logger is to replace the global default, which would also capture every
  other package's `slog` output. Claviger does not do that: the library's lines (protocol errors such as a bad
  `redirect_uri`) go to the process's standard slog destination, while Claviger's own OIDC log lines (rate limits,
  failing client authentication, key and single sign-on problems) use the application logger.
- Formal OpenID conformance (the Basic OP profile) has not been run yet. The runbook, a test-instance bootstrap
  script and a manual CI job are in `docs/oidc-conformance.md`; running the suite needs a public https hostname
  per run and a browser driver for the sign-in page, neither of which exists yet.
