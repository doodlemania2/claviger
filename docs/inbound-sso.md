# Enterprise single sign-on (inbound identity providers)

An organization (#79) can connect its own identity provider, prove that it owns its email domains, and have its people sign in to Claviger through that provider. This is the reverse of [docs/oidc.md](oidc.md): there Claviger is the provider, here it is the relying party.

With no connection created nothing changes: the new collections stay empty, the sign-in page asks nothing extra, and no password sign-in is refused.

This wave ships OpenID Connect. SAML 2.0 plugs into the same engine later (see [Adding a connection type](#adding-a-connection-type)).

## How it fits together

| Piece | What it does |
|---|---|
| Connection (`_ssoConnections`) | One upstream provider of one organization: type, issuer, client id and secret, scopes, role mapping, `enabled`, `ssoRequired`, `jit` |
| Domain claim (`_ssoDomains`) | A domain an organization claims for a connection, proven by a DNS TXT record. Only a **verified** claim routes anyone |
| Flow (`_ssoFlows`) | Short-lived state of a sign-in in progress (minutes, hashes only) |
| Home-realm discovery | `name@example.org` on the sign-in page goes to the verified domain's connection; every other address signs in as before |
| Provisioning | The account is found, linked or created, and the person becomes a member of the organization with a role from their groups |

All three collections are hidden system collections with null rules (superusers only through the records API), are on the API-key forbidden list, and are registered in the data map. Managing them is done with the routes below.

## Setting it up

1. **Create a connection** (org owner or admin, or a superuser): `POST /api/orgs/{id}/sso/connections`. Start from a [preset](#presets). The connection is created disabled.
2. **Register Claviger at the provider.** The redirect URI is `<application URL>/api/sso/callback` (the same for every connection; `GET /api/orgs/{id}/sso/presets` returns it). Set **Settings > Application URL**: the callback URL is built from it.
3. **Check it**: `POST .../connections/{cid}/test` fetches and validates the provider's discovery document.
4. **Claim the domain**: `POST /api/orgs/{id}/sso/domains` with `{"domain": "example.org", "connection": "<id>"}`. The answer carries the record to publish:

   ```
   example.org.   TXT   "claviger-verification=<token>"
   ```

   If the DNS host cannot hold a second TXT record at the apex, publish the same value at `_claviger.example.org` instead. Both names are checked.
5. **Verify**: `POST .../domains/{did}/verify`. It answers `verified: true` once the record is visible; until then it answers `verified: false` and you can call it again as DNS propagates. Verification is not repeated for a claim that is already verified.
6. **Enable** the connection: `PATCH .../connections/{cid}` with `{"enabled": true}`.

An unverified claim, a disabled connection and an archived organization are never routed. A domain can be **verified for one organization only**; a second organization that publishes a valid record for the same domain gets 409 `domain_taken`.

## Routes

Management needs an owner or admin of the organization (or a superuser); a member gets 403, a stranger 404. API keys can read but never change. Every change is audited with the actor and `meta.orgId`; the client secret is redacted in the log and never returned by any route (`hasSecret` says whether one is set).

| Route | |
|---|---|
| `GET /api/orgs/{id}/sso/presets` | provider presets, the connection types, the callback URL |
| `GET`, `POST /api/orgs/{id}/sso/connections` | list, create |
| `GET`, `PATCH`, `DELETE /api/orgs/{id}/sso/connections/{cid}` | view, change, delete (its domain claims go with it) |
| `POST /api/orgs/{id}/sso/connections/{cid}/test` | check the provider's discovery document |
| `GET`, `POST /api/orgs/{id}/sso/domains` | list, claim |
| `POST /api/orgs/{id}/sso/domains/{did}/verify` | look for the TXT record |
| `DELETE /api/orgs/{id}/sso/domains/{did}` | release a claim |

Connection fields: `name`, `type` (`oidc`), `enabled`, `ssoRequired`, `jit` (default true), `issuer` (a pasted `.../.well-known/openid-configuration` is trimmed to the issuer), `clientId`, `clientSecret` (write-only), `scopes` (array, must include `openid`; default `openid email profile`), `groupsClaim` (default `groups`; a dotted path such as `realm_access.roles` reads a nested claim), `roleMapping` (`{"<group>": "<role>"}`), `defaultRole`, `config` (type-specific settings, unused by OIDC).

The client secret is stored encrypted (AES-256-GCM, the key of the encrypted settings) when `--encryptionEnv`/`PB_ENCRYPTION_KEY` is configured, and as given otherwise, like webhook secrets. Set the key in production.

### The sign-in routes (public)

| Route | |
|---|---|
| `POST /api/sso/discover` | `{email}`: `{"sso": false}`, or `{"sso": true, "connection", "name", "required", "collection", "url"}` |
| `GET /api/sso/start` | `?connection=&returnTo=&login_hint=`: sends the browser to the provider |
| `GET`, `POST /api/sso/callback` | the provider's redirect URI |
| `POST /api/sso/exchange` | `{code}`: the one-time code for the ordinary auth response (`token` and `record`) |

`returnTo` must be a path on this server (or an absolute URL on the application's origin); anything else is refused. After the provider answers, the browser is sent to `returnTo` with `sso_code=<code>` (success) or `sso_error=<code>` (failure), and the page POSTs the code to `/api/sso/exchange`. The hosted sign-in page (`/oidc/login`) does all this by itself. A single-page app on the same origin can do the same.

## What happens at sign-in

1. **Discovery.** On the hosted sign-in page, leaving the identity field with an address at a verified domain shows a "Continue with *connection name*" button (and, when SSO is required, hides the password field). The page never auto-redirects while the person is typing.
2. **Start.** A `_ssoFlows` row holds the state (hashed), nonce and PKCE verifier. The browser gets an `HttpOnly`, `SameSite=Lax` binding cookie scoped to `/api/sso`; only its hash is stored. The authorization request is the code flow with PKCE (S256), `state` and `nonce`.
3. **Callback.** The state is looked up and deleted (it works once), must match the binding cookie, and expires after 10 minutes. The code is exchanged at the token endpoint with the client secret, and the **ID token is verified** before any claim is believed: signature against the provider's JWKS (asymmetric algorithms only: RS/PS/ES/EdDSA, never `none` or HMAC), `iss`, `aud`, `azp`, `exp`, `nbf`, `iat` (2 minutes of skew), `nonce`, `sub`. The userinfo endpoint is not used.
4. **Account.** See [Accounts](#accounts-and-linking). A one-time code (2 minutes, hash stored) bound to the same browser is handed back to the page.
5. **Exchange.** The page POSTs the code; the response is the ordinary Claviger auth response, so sessions, bans, login alerts and the collection's MFA rules apply exactly as for an OAuth2 sign-in (`authMethod` is `oauth2`).

Failures send the browser back with an `sso_error` code (`email_unverified`, `no_account`, `domain_not_verified`, `invalid_token`, `other_browser`, `expired`, ...) and write an `sso_login_failed` audit entry; the hosted page shows a translated message for the ones a person can act on.

## Accounts and linking

The accounts live in the organization user collection (`CLAVIGER_ORGS_USER_COLLECTION`, default `users`). Rules, in order:

1. **The email must be at a verified domain of this connection.** Otherwise the provider could assert any address, including that of an administrator of another organization (`domain_not_verified`).
2. **An account already linked to this connection and subject** is the person. The email is not consulted, so a changed address does not lock anyone out. The link is an ordinary `_externalAuths` row (provider `sso:<connection id>`, provider id = the subject), so it shows in the person's linked sign-ins and is covered by data export and erasure.
3. **An existing account with that email** is linked only when the provider **vouches for the address**: `email_verified: true`, or Entra's `xms_edov`. Otherwise the sign-in is refused (`email_unverified`) and nothing is linked. An account whose own email was never verified (someone may have registered the address first) is taken over for its owner: marked verified, password replaced with a random one, tokens revoked.
4. **Otherwise an account is created** (just-in-time) when `jit` is on (`no_account` when off). It gets a random password (the person can set one through a reset, unless `ssoRequired`), the provider's name when the collection has a `name` field, and `verified` as the provider reported it. A collection with other required fields makes creation fail (`provision_failed`): use a collection whose only required fields are the standard ones.
5. **Membership and role.** The person becomes a member with `source: "sso"`. The role is the highest-ranked role that any of their groups maps to in `roleMapping`; with no match, `defaultRole`; with none, `member`. Single sign-on **never assigns `owner`** (validated at save and again at sign-in). An `sso` member follows the mapping at every sign-in (a demotion is an `sso_role_sync` audit entry); a member added by hand, invitation or SCIM keeps their role. A suspended member stays suspended. The usual guards apply (the last owner cannot be demoted).

Mapping matches group values exactly (case-sensitive).

## SSO required

`ssoRequired` on a connection makes **password and email-OTP sign-in refuse (403, `data.sso.code = "sso_required"`) the accounts whose email is at a verified domain of that connection**, in the organization user collection. The refusal is audited (`sso_password_blocked`) and counts neither as a failed attempt for the lockout nor as a sign-in.

Deliberately **not** affected, so a lost identity provider does not lock a whole domain out and people keep their strongest local factor:

- **Passkeys** (WebAuthn) still work.
- OAuth2 providers configured on the collection still work. Turn them off if the policy forbids them.
- Account recovery (password reset by email, staff-vouched recovery codes). A reset that is completed does not bring password sign-in back: the sign-in still refuses.
- Superusers are never affected (they are not in the user collection).

A connection that is **disabled** (or whose organization is archived) does not require anything: turning the connection off is the break-glass for a provider outage. Before turning `ssoRequired` on, check that you can sign in through the provider yourself, in a second session, and that the claim is verified for the right domain.

## Presets

`GET /api/orgs/{id}/sso/presets` returns these with their notes. Redirect URI everywhere: `<application URL>/api/sso/callback`.

| | Okta | Microsoft Entra ID | Google Workspace |
|---|---|---|---|
| Issuer | `https://{your-okta-domain}/oauth2/default` (the authorization server that issues the tokens) | `https://login.microsoftonline.com/{tenant-id}/v2.0` (the tenant id; `common` and `organizations` have no fixed issuer and do not work) | `https://accounts.google.com` |
| Scopes | `openid email profile groups` | `openid email profile` | `openid email profile` |
| Groups claim | `groups`: add a groups claim to the ID token on the authorization server | `groups`: add a groups claim under Token configuration. It carries group **object ids**, so the mapping keys are object ids. Users in more than 200 groups get no claim (overage): emit only the groups assigned to the application | none; `hd` (the Workspace domain) is the only useful claim |
| Verified email | sends `email_verified` | does **not** send `email_verified`: add the optional ID-token claims `email` and `xms_edov` | sends `email_verified` |
| Notes | Create an OIDC web app | Register a web app, create a client secret | OAuth client "Web application"; set the consent screen to Internal |

Any other provider with a discovery document, the authorization code flow, PKCE and asymmetric ID-token signatures works with the "Other OpenID Connect provider" preset (Keycloak, Authentik, Auth0, ...).

## Provider URLs and the network

The server fetches the provider's discovery document, keys and token endpoint, and an organization administrator supplies the URL. So the fetches must not become a way into the internal network: the HTTP client **refuses to connect to loopback, private, link-local (including the cloud metadata address), CGNAT and other reserved addresses**, checked on the address actually dialed (so a name that resolves or rebinds to one is refused), follows at most 3 redirects, and the issuer and every endpoint in the discovery document must be `https`. For an on-premises provider on a private address, or a development setup over plain `http` on loopback, set `CLAVIGER_SSO_ALLOW_PRIVATE_NETWORKS=true` (an operator decision, not an organization one).

## Audit

Besides the ordinary record-change entries of `_ssoConnections` and `_ssoDomains` (the secret is redacted):

| Action | When |
|---|---|
| `sso_domain_verify` | a claim became verified |
| `sso_login` | a sign-in completed (`meta`: org, connection, created, linked) |
| `sso_login_failed` | a refused or failed sign-in (`meta.reason` is the code) |
| `sso_jit_create` | an account was created at sign-in |
| `sso_link` | an existing account was linked by verified email |
| `sso_role_sync` | an SSO member's role changed (`meta.from`, `meta.to`) |
| `sso_password_blocked` | a password or OTP sign-in was refused because SSO is required |

`_ssoFlows` is not audited (ephemeral rows).

## Adding a connection type

The engine is in package `sso`; a connection type implements `sso.Provider` (`sso/sso.go`) and registers itself:

```go
type Provider interface {
    Type() string                                                    // the value of _ssoConnections.type
    Validate(c *Connection) error                                    // no network
    Begin(ctx context.Context, c *Connection, in BeginInput) (*BeginOutput, error)
    Complete(ctx context.Context, c *Connection, in CompleteInput) (*Identity, error)
}
```

- `Begin` returns the URL to send the browser to. `BeginInput` carries the callback URL, the `State` (use it as the SAML `RelayState`), a nonce, a PKCE verifier and a login hint.
- `Complete` verifies what came back (`CompleteInput.Query` for a redirect, `CompleteInput.Form` for a POST such as a SAML assertion consumer; the callback route accepts `GET` and `POST`, and reads the state from `state` or `RelayState`) and returns an `Identity` (subject, email, whether the provider vouches for it, name, groups, raw claims).
- Everything else is shared: the state and the browser binding, domain routing, `ssoRequired`, account linking, JIT, role mapping, the one-time exchange code, audit.
- Type-specific settings that need no column (IdP metadata, entity id, certificates) go in `Connection.Config` (the `config` JSON field); nothing about the collections changes. The `type` select already accepts `saml`.
- A SAML ACS is a **cross-site POST**, and the binding cookie is `SameSite=Lax`, which the browser withholds from it. The SAML type must either set the cookie `SameSite=None; Secure` (change `ssoSetBind` in `apis/sso.go`) or bind through the `RelayState` round trip. Decide this in the SAML wave; it is the one place the seam is not purely protocol.

## Limits (this wave)

- OpenID Connect only; SAML is the next wave.
- No admin UI yet; the routes above are the interface.
- Domain claims are verified once. A TXT record that is later removed does not revoke the claim (release it with `DELETE`).
- The provider's groups are read from the ID token only (no userinfo or Graph lookup), so Entra's group overage and Google's missing groups are not bridged.
- One connection per domain; an organization can hold several connections for different domains.
- Sign-out at the provider (single logout) is not propagated.
