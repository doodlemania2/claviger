# Single sign-on presets for church software (#85)

> **Desk research, not tested.** Everything here was read from each vendor's public documentation or search
> results on 2026-11-27. No trial accounts exist yet, so **no guide has been verified end to end**. Where a
> fact could not be confirmed it says "unknown". Vendor products change; check the cited page before relying
> on a row. The first two platforms to verify will be chosen later (spec question 29).

Claviger is an OpenID Connect provider ([docs/oidc.md](../oidc.md)). It is not a SAML identity provider
(spec question 28: OIDC only), so a product that accepts only SAML cannot be connected today.

Direction matters. "Inbound OIDC" below means the product lets its users sign in through an external OIDC
provider such as Claviger. A product that is itself an OIDC *provider* (Planning Center, Rock) does not
help here.

## Vendor survey

| Product | Inbound OIDC | SAML | SCIM | Preset and guide | Source |
| --- | --- | --- | --- | --- | --- |
| Church Online Platform (Subsplash) | **Yes**: Issuer URI, Client ID, Client Secret | unknown | unknown | [guide](church-online-platform.md) | [support.online.church](https://support.online.church/article/dtjpnrky6f-sso-open-id-connect) |
| MinistryPlatform (ACS Technologies) | **Yes**: Identity Provider record of type OAuth 2.0 / OpenID Connect (existing users only) | unknown | unknown | [guide](ministry-platform.md) | [help.acst.com](https://help.acst.com/en/ministryplatform/help-topics/platform/customizations/extending-the-platform/external-login-authentication/enable-identity-provider) |
| Blackbaud ID (Blackbaud K-12 and church products) | **Yes**: OIDC connection, metadata URL, ID Token or Code | Yes (SAML 2.0) | not confirmed by Blackbaud (Okta lists a SCIM connector) | [guide](blackbaud-id.md) | [Blackbaud OIDC setup](https://webfiles-sc1.blackbaud.com/files/support/helpfiles/admin/content/sso-oidc-setup.html), [Okta listing](https://www.okta.com/integrations/blackbaud) |
| Subsplash (apps and web tools) | Only with Rock as the provider, per Subsplash's article; other providers unknown | unknown | unknown | none (use [generic](generic.md)) | [Subsplash article 9834775](https://support.subsplash.com/en/articles/9834775-rock-oidc-single-sign-on-sso) (search result only; page not read) |
| Rock RMS | Not found. Rock is an OIDC *server* for other systems; its External Authentication Services list Auth0, and a third-party plugin offers Office 365/Okta. Generic OIDC login: unknown | unknown | unknown | none | [Rock: Intro to OpenID Connect](https://community.rockrms.com/documentation/core-concepts/security/rock-authentication/intro-to-openid-connect), [Auth0 setup](https://community.rockrms.com/documentation/core-concepts/security/external-authentication-services/set-up-auth0-authentication) |
| ChurchSuite | Not found | **Yes** (SAML2 with Entra ID and Google Workspace) | unknown | none (needs a SAML IdP) | [ChurchSuite SAML2 article](https://support.churchsuite.com/article/708-integrating-with-saml2-sso) (seen in search results; fetch returned 403) |
| Planning Center | No: it is an OIDC *provider* for third-party apps | unknown | unknown | none | [Planning Center changelog](https://www.planningcenter.com/changelog/integrations-api/new-openid-connect-for-privacy-conscious-authentication) |
| Church Community Builder / Pushpay | Not found. Pushpay announced a proprietary "Single Sign-On Lite" between Pushpay and CCB (2020) | unknown | unknown | none | [Pushpay blog](https://pushpay.com/blog/pushpay-church-community-builder-april-2020/) |
| Elvanto / Tithe.ly | Not found. Proprietary sign-in from Tithe.ly into Elvanto | unknown | unknown | none | [Elvanto help](https://help.elvanto.com/hc/en-us/articles/7478725915159-Tithely-and-Elvanto-Single-Sign-On) |
| Breeze (Breeze ChMS) | unknown (no SSO page found; the "Breeze" SSO hits were unrelated products) | unknown | unknown | none | search only; nothing citable |
| Realm (ACS Technologies) | unknown (nothing found) | unknown | unknown | none | search only; nothing citable |
| Faithlife (Proclaim, Logos) | unknown (nothing found) | unknown | unknown | none | search only; nothing citable |
| Brightwheel and similar school portals | unknown (nothing found) | unknown | unknown | none | search only; nothing citable |

## What the survey means

- **Presets and guides written (4):** Church Online Platform, MinistryPlatform, Blackbaud ID, and a generic
  OIDC application. Each is a template in `oidc/templates/` and is offered in the admin UI under
  **OpenID Connect, New client, Start from a template**.
- **SAML-only products** (ChurchSuite) need a SAML identity provider. That stays out of scope unless Derek
  decides otherwise.
- **Products with no known SSO** (CCB, Elvanto, Breeze, Realm, Brightwheel) are the case for the one-way sync
  connector in the spec (85-4), not for presets.
- **Unknown rows** need a vendor contact or a trial account. Do not read "unknown" as "unsupported".

## Before you connect a third party

A template sends member identity to another company. For a church that can amount to disclosing religious
affiliation. The template picker lists the claims that leave and asks you to confirm. Keep the `orgs` scope
and household claims off unless the product needs them (spec section 4.6), and tell your members which
products receive their data.

## Shared limits

- **Not verified end to end.** Every guide carries that banner until someone runs the flow against a real
  account and records the result here.
- Redirect URIs are matched exactly and must be `https`.
- Claviger accepts PKCE but not every product sends it, so the product templates leave "Require PKCE" off.
  Turn it on if the product shows a code challenge setting.
- Users sign in from one auth collection per client. Each product matches people by **email address**, so
  addresses in Claviger must equal the ones in the product.
- Claviger has no SCIM client. Accounts must already exist in the product (or be created by the product's own
  import) before sign-in works.
