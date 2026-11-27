# MinistryPlatform (ACS Technologies)

> **Not yet verified end to end.** Written from the vendor's public documentation (desk research, 2026-11-27). No account has been used to test it. Try it with a test user first, and report what differs.

Preset: **MinistryPlatform (ACS Technologies)** (`oidc/templates/ministry-platform.json`).

## What the product supports

External login through an **Identity Provider** record; the type list includes OAuth 2.0 and OpenID Connect.
Sources: <https://help.acst.com/en/ministryplatform/help-topics/platform/customizations/extending-the-platform/external-login-authentication/enable-identity-provider>
(callback URL, activation) and, for the OIDC fields, the vendor's Microsoft Entra ID walkthrough
(<https://help.acst.com/ministryplatform/help-topics/platform/customizations/extending-the-platform/external-login-authentication/microsoft-entra-id-azure-ad>;
read only through search results, the page itself returned 401). SAML and SCIM: unknown.

## Steps

1. In MinistryPlatform (System Administrator): **Administration, Identity Providers, New Identity Provider**.
   Enter a display name, choose the provider type **OAuth 2.0 / OpenID Connect**, enter a temporary Client ID
   and save.
2. Copy the record's **Identity Provider Unique ID** and **uppercase** it.
3. In Claviger, **New client, Start from a template, MinistryPlatform**. Fill in your domain and the uppercase ID.
   The redirect URI becomes
   `https://<domain>/ministryplatformapi/oauth/callback/<ID>`. Confirm the claims, apply, create, and copy the
   secret.
4. Back in the record enter the Claviger **well-known endpoint** (`<application URL>/oidc/.well-known/openid-configuration`),
   **issuer** (`<application URL>/oidc`), **client ID**, **client secret**, and scope `openid profile email`. The
   Entra walkthrough also asks for a client authentication method; use client secret (POST). Exact field
   labels for the generic type were not available: unknown.
5. Recycle the app pool, or use **Restart Auth App** on a Setup Admin's user profile. A login button appears.

## Claims needed

`openid` is mandatory (vendor). How the product matches an external identity to a user is not documented:
expect the email address, and unknown beyond that.

## Test

Sign in through the new button with a user who already exists in MinistryPlatform.

## Limits

- **Existing users only.** External login cannot create new MinistryPlatform accounts (vendor).
- Callback URL case matters: the Unique ID must be uppercase.
- "Require PKCE" is off in the preset; the product's behavior is unknown.
