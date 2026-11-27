# Generic OpenID Connect application

> **Not yet verified end to end.** Written from the vendor's public documentation (desk research, 2026-11-27). No account has been used to test it. Try it with a test user first, and report what differs.

Preset: **Generic OpenID Connect application** (`oidc/templates/generic.json`). Use it for any product that
accepts an external OpenID Connect provider and has no preset of its own.

## Steps

1. In the product, find its OpenID Connect or "custom provider" settings and note its **callback (redirect) URL**.
2. In Claviger, **New client, Start from a template, Generic**, paste the callback URL, confirm the claims,
   apply, create, and copy the secret.
3. In the product enter: issuer `<application URL>/oidc`, or the discovery URL
   `<application URL>/oidc/.well-known/openid-configuration` if it asks for that; the client ID and secret;
   scopes `openid profile email`.
4. Sign in with a test user whose email matches an account in the product.

## Claims sent

`sub`, `collection`, `email`, `email_verified`, `name`, `preferred_username`, `updated_at`. Drop `profile` from
the client's scopes if the product only needs the email address. The `orgs` scope and household claims are not
in the template; add them only if the product needs them.

## Limits

- Authorization code with PKCE is required by this preset. If the product cannot send PKCE, untick
  "Require PKCE" on the client.
- Redirect URIs are matched exactly; `https` only.
- Tokens are signed RS256. Choose ES256 on the client only if the product supports it.
