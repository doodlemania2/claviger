# Church Online Platform (Subsplash)

> **Not yet verified end to end.** Written from the vendor's public documentation (desk research, 2026-11-27). No account has been used to test it. Try it with a test user first, and report what differs.

Preset: **Church Online Platform (Subsplash)** (`oidc/templates/church-online-platform.json`).

## What the product supports

Inbound OpenID Connect (OAuth 2.0 and OIDC 1.0) under **Admin, Integrations, OpenID Connect**. Source:
<https://support.online.church/article/dtjpnrky6f-sso-open-id-connect>. The vendor lists Rock RMS and Auth0 as
verified, and Planning Center, Okta, Microsoft Entra ID and Supabase as untested. Claviger is not on either list.
SAML and SCIM: unknown.

## Steps

1. In Claviger, **OpenID Connect, New client, Start from a template, Church Online Platform**.
2. In the product: **Admin, Integrations, OpenID Connect, Set Up**. It shows its callback URL(s). Paste the
   first into the template's callback field, read the claims list, confirm and apply. Add any further
   callback URLs to the client's redirect URIs.
3. Create the client and copy the **client ID** and **secret** (shown once).
4. Back in the product enter:
   - **Issuer URI:** the Claviger issuer, `<application URL>/oidc` (shown at the top of the OpenID Connect page).
   - **Client ID** and **Client Secret** from step 3.
5. Click **Test Configuration**, then **Save and Activate**.

## Claims needed

`openid email profile`. The product page does not say which claims it reads; `email` and `name` are the likely
ones. Unknown: confirm with a test sign-in.

## Test

Keep a second admin session open. Sign in to the product's admin through Claviger with a test user.

## Limits and warnings

- **Once active, every user, including existing ones, must sign in through the provider** (vendor page). If
  Claviger fails, admins are locked out; the vendor says to contact its support to switch OpenID Connect off.
- Callback URL values, required scopes and token lifetimes are not documented on the page: unknown.
- "Require PKCE" is off in the preset because it is unknown whether the product sends a code challenge.
