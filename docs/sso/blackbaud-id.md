# Blackbaud ID (school and church portals)

> **Not yet verified end to end.** Written from the vendor's public documentation (desk research, 2026-11-27). No account has been used to test it. Try it with a test user first, and report what differs.

Preset: **Blackbaud ID** (`oidc/templates/blackbaud-id.json`).

## What the product supports

Organization single sign-on through Microsoft Entra ID, Google Workspace, **OIDC** or **SAML 2.0**. Source:
<https://webfiles-sc1.blackbaud.com/files/support/helpfiles/admin/content/sso-oidc-setup.html>. SCIM: not
confirmed by Blackbaud (Okta's listing shows a SCIM connector: <https://www.okta.com/integrations/blackbaud>).
Which of your Blackbaud products honor Blackbaud ID SSO: unknown, check with your account manager.

## Steps

1. In Blackbaud ID, an organization admin first **claims at least one email domain**. Create a fallback
   Blackbaud ID outside the claimed domains that can reach the Authentication settings, in case of lockout.
2. Open the **Configure OIDC connection** screen. It shows the redirect URI
   `https://id.blackbaud.com/bbid.onmicrosoft.com/oauth2/authresp`; the preset already holds it.
3. In Claviger, **New client, Start from a template, Blackbaud ID**, confirm the claims, apply, create, copy the
   secret.
4. In Blackbaud enter: connection name; **metadata URL** `<application URL>/oidc/.well-known/openid-configuration`;
   response type **Code**; the Claviger **client ID** and **secret** (with Code you must renew the secret
   before it expires; the expiration date field applies only to Microsoft metadata URLs); and the
   **user identity mapping** for email, first name and last name. A mapping is only possible for claims in the
   metadata's `claims_supported`; Claviger advertises `email` and `name` but has no separate first and last
   name claims, so the name mapping may be limited: unknown.
5. The connection starts in **test mode**. At least one user must sign in successfully before you can turn SSO on.

## Claims needed

`openid email profile`. Email addresses must be unique, and the vendor advises against using email as the NameID.

## Limits

- New connections can take up to 24 hours, edits up to 30 minutes; domain verification a few minutes, rarely
  two days.
- IdP-initiated sign-in is not supported by Blackbaud.
- Users with pending invitations need them resent after SSO is on.
- Scopes are not described on the page: unknown. "Require PKCE" is off in the preset for that reason.
