# SCIM 2.0 provisioning

Claviger (#79c) speaks SCIM 2.0 (RFC 7643 and 7644) so an identity provider such as Microsoft Entra ID or Okta can create, update, deactivate and remove the members of one organization, and keep its groups in step. Google Workspace is not a general SCIM client for custom apps and is not covered.

With no SCIM token created, nothing changes: `/scim/v2` answers `401` to every request and no data is touched.

The design notes are in `docs/specs/79-orgs-roles-scim.md` (section 4.8). This page is what shipped, including where it differs from the spec.

## Tenant URL and tokens

- Tenant (base) URL: `https://<your-host>/scim/v2`
- Secret token: a per-organization token, `scim_<prefix>_<secret>`.

Create tokens as an owner or admin of the org, on the hosted page `/orgs/<slug>` ("SCIM provisioning"), in the admin UI under Organizations, or with the API:

| Route | |
|---|---|
| `GET /api/orgs/{id}/scim-tokens` | list (prefix, created, last used, expiry, status; never the secret), plus the tenant URL |
| `POST /api/orgs/{id}/scim-tokens` | `{name, expires?}`; the response is the only time the token is shown |
| `DELETE /api/orgs/{id}/scim-tokens/{token}` | revoke |
| `GET /api/orgs/{id}/scim-settings`, `PUT` | default role, group to role mapping, ban on deactivation |

Facts about tokens:

- A token belongs to one org and is not a user. It cannot call `/api`, run a collection rule or see another org. Request a new one for a rotation: up to 10 live tokens per org, so the old one can stay until the identity provider has the new one.
- Only the SHA-256 of the secret is stored (`_scimTokens.tokenHash`, redacted in the audit log). It is checked in constant time. `lastUsed` is updated at most once a minute and does not write an audit entry.
- A revoked, expired, unknown or malformed token is a `401`. Failed attempts are rate limited per client IP (30 a minute, then `429`). A valid token on an archived org is a `403`.
- The token is accepted as `Authorization: Bearer <token>` or as the bare value.
- `_scimTokens` and `_scimGroups` have no API rules (superusers only) and are on the API-key forbidden list.

## What is supported

Discovery: `GET /ServiceProviderConfig`, `/ResourceTypes[/{id}]`, `/Schemas[/{id}]`. It advertises `patch` yes, `filter` yes (max 200), and `bulk`, `sort`, `etag`, `changePassword` no.

`Users` and `Groups`: `GET` (list, by id), `POST`, `PUT`, `PATCH`, `DELETE`. Responses use `Content-Type: application/scim+json`; bodies may be `application/json` too. Errors follow `urn:ietf:params:scim:api:messages:2.0:Error` with `status`, `scimType` and `detail`.

Not supported: `/Bulk`, `/Me`, `/.search`, ETags, sorting, the enterprise user extension (accepted and ignored), `password` (never accepted, never returned).

### Filters

A list request takes `filter`, `startIndex` (1-based) and `count` (default 100, max 200, `0` returns only `totalResults`). The subset is one or more `attribute eq value` terms joined with `and`:

| Resource | Attributes |
|---|---|
| Users | `userName`, `externalId`, `id`, `displayName`, `active`, `emails.value`, `emails[type eq "work"].value` |
| Groups | `id`, `displayName`, `externalId` |

Attribute names, `eq` and `and` are case-insensitive; `userName`, `emails` and `displayName` compare case-insensitively, `id` and `externalId` exactly. Anything else (`co`, `sw`, `pr`, `or`, `not`, parentheses, other attributes) is `400` with `scimType: invalidFilter`. A Users listing only contains members of the token's org.

### PATCH

`Operations` with `op` `add`, `replace` or `remove` (any case; Entra sends `Replace`). Users: `active`, `userName`, `externalId`, `displayName`, `name.givenName`, `name.familyName`, `name.formatted`, `name`, `emails`, `emails[type eq "work"].value`, `emails[primary eq true].value`, and the same keys as one object in a path-less operation (Okta's `{"op":"replace","value":{"active":false}}`). Unknown attributes, `urn:` extension paths and other email types are ignored, not rejected. `remove` needs a path (`noTarget` otherwise); removing `externalId` clears it.

Groups: `members` with `add`, `replace` or `remove` (a value list, as Entra sends it, or `members[value eq "<id>"]` as Okta sends it), `displayName`, `externalId`, and the path-less object form.

`active` accepts a boolean or Entra's strings `"True"` and `"False"`.

## How a SCIM user maps to Claviger

A SCIM User is an auth record of the org user collection (`users` by default, see `docs/orgs.md`) plus a membership in the token's org with `source = scim`.

| SCIM | Claviger |
|---|---|
| `id` | the account's record id |
| `userName` | the account email. It must be an email address (`invalidValue` otherwise). If `emails[work]` differs, `userName` wins; with no `userName` the work email is used |
| `displayName`, `name.*` | the `name` field of the account, if the collection has a text field of that name |
| `externalId` | `_orgMembers.externalId` (unique per org), not on the account |
| `active` | the membership `status`: `true` active, `false` suspended |
| `groups` | read-only, the org's SCIM groups the user is in |

Decisions:

1. **Linking.** `POST` with an email that already has an account links it only if that account's email is **verified**. The membership is created and the account's password and profile are left alone (only an empty name is filled). An unverified account is `409`, `scimType: uniqueness`, with a detail that says so. A user who is already a member is `409` too. Entra and Okta both fall back to a lookup on a 409.
2. **New accounts are created verified.** The identity provider asserted the email, which is the same trust an invitation link gives. They get a random password nobody knows; the SCIM API never sets or accepts a password. People sign in with a passkey, OIDC, an emailed code or a password reset. No welcome mail is sent.
3. **Whose accounts SCIM may edit.** A change of email or name is applied only when the account was created by this org's SCIM and belongs to no other org. Otherwise an email change is `400 mutability` and a name change is silently skipped, so one provider cannot rewrite an account that other orgs or the user themself depend on.
4. **Deactivation (`active: false`) suspends the membership.** The user loses all org access immediately. If the org setting `deactivationBans` is on, the account is also banned (`core/ban.go`: sign-in blocked, API keys stop, sessions revoked), but only if the account is not an active member of any other org and has no active ban already. The ban is tagged `createdBy = scim:<orgId>`; `active: true` lifts only a ban with that tag. Superusers cannot be banned by this path, and another admin's ban is never replaced or lifted.
5. **`DELETE /Users/{id}` removes the membership, never the account.** The user also leaves the org's SCIM groups. The last owner cannot be removed or deactivated (409), same as everywhere else.
6. **SCIM never grants ownership.** The default role and the group role mappings cannot be `owner`. SCIM can suspend or remove an owner (the last-owner guard still applies).
7. The default role of new users is `member` (setting `defaultRole`, one of `admin`, `member`, `viewer`).

## Groups

A SCIM Group never creates an organization. It maps to a tag and, optionally, a role:

- **Tag.** Every member of a group gets a tag named like the group (`displayName`, 1 to 64 characters, no control characters, quotes or backslashes). The tag is created on demand and removed from the member when they leave the group, the group is renamed (the tag follows) or deleted. A manual tag with the same name is the same tag.
- **Role (optional).** The setting `groupRoles` maps a group name to `admin`, `member` or `viewer`, for example `"Claviger Admins": "admin"`. The highest mapped role of a user's groups wins, and joining a group only ever raises a role. When a user leaves a mapped group and their role is still exactly that group's role, they fall back to the highest role of their remaining mapped groups, or to the default role. A role someone set by hand to something else, and every owner, is left alone.
- Removing a user from a group never removes the membership. Group members must already be users of the org (create the users first; Entra and Okta do).
- The mapping is configured per org on the SCIM settings (hosted page, admin UI, or `PUT /api/orgs/{id}/scim-settings`). An unmapped group still tags its members.

## Entra ID setup

1. Microsoft Entra admin center > Enterprise applications > New application > Create your own application (non-gallery).
2. Provisioning > Get started > Provisioning Mode: Automatic.
3. Tenant URL: `https://<host>/scim/v2`. Secret Token: the token. Test Connection.
4. Mappings > Provision Microsoft Entra ID Users. Keep these and remove the rest:

   | Entra attribute | customappsso attribute |
   |---|---|
   | `userPrincipalName` (or `mail`) | `userName` (matching attribute, precedence 1) |
   | `Switch([IsSoftDeleted], , "False", "True", "True", "False")` | `active` |
   | `displayName` | `displayName` |
   | `givenName` / `surname` | `name.givenName` / `name.familyName` |
   | `objectId` | `externalId` |
   | `mail` | `emails[type eq "work"].value` (optional, `userName` wins) |

   `userName` must be a real email address: if the UPN is not your users' mailbox, map `mail` to `userName` instead.
5. Mappings > Provision Microsoft Entra ID Groups: `displayName` > `displayName`, `members` > `members`, `objectId` > `externalId`. Assign the users and groups, then switch provisioning on. The cycle runs about every 40 minutes; use "Provision on demand" to test.

Quirks, all handled: `active` arrives as the string `"True"` or `"False"`; operation names are capitalised; email changes arrive as `emails[type eq "work"].value`; extension attributes arrive and are ignored; group reads ask for `excludedAttributes=members`; Entra first looks a user up with `filter=userName eq "..."` or `externalId eq "..."`, and treats a `409` on create as "exists, look it up". A soft delete in Entra is `active: false`; a hard delete is `DELETE`, which only removes the membership.

## Okta setup

1. Applications > Browse App Catalog > "SCIM 2.0 Test App (Header Auth)", or Create App Integration > SCIM 2.0 (header auth).
2. Provisioning > Integration: SCIM connector base URL `https://<host>/scim/v2`; Unique identifier field for users `userName`; supported actions: Import Users is not offered (Claviger is the target, not a source); tick Push New Users, Push Profile Updates, Push Groups.
3. Authentication Mode: HTTP Header. Authorization: the token (`Bearer scim_...` or the bare token, both work). Test Connector Configuration.
4. To App: enable Create Users, Update User Attributes, Deactivate Users. The default attribute mappings (`userName`, `givenName`, `familyName`, `email`, `displayName`) are enough; `userName` must be an email address.
5. Push Groups: add the groups to push. Group names become tags (and roles, if mapped).

Quirks, all handled: Okta deactivates with `PATCH` `{"op":"replace","value":{"active":false}}`; it removes group members with `members[value eq "<id>"]`; it sends `password` (ignored, never stored) and `locale` (ignored); it looks users up with `filter=userName eq "..."` and `count=1`.

## Audit and webhooks

Every SCIM write is a normal audit entry with `actorType: "scim"`, `actorId: "scim:<tokenId>"` and `meta.orgId`. Creating a user is a `create` on the member collection and on `_orgMembers`; deactivation is an `update` of `_orgMembers` (and a `create` on `_bans` when the ban setting applies); groups are `_scimGroups` entries. Token creation and revocation are entries with the human admin as actor; the token hash is redacted. Webhooks fire from the collection events as for any other change (`_orgMembers:create`, `_orgMembers:update`, ...).

## Troubleshooting

| Symptom | Cause |
|---|---|
| `401` on every call | wrong, revoked or expired token, or it was copied with spaces. `403`: the org is archived |
| `429` | too many failed token attempts from the IP; wait a minute |
| `400 invalidValue ... userName must be an email` | the provider's unique identifier is not an email address |
| `409 uniqueness ... not verified` | an account with that email exists but never verified it. Ask the person to verify (or reset) first, then provision again |
| `400 mutability` | an email change on an account that other orgs share, or that SCIM did not create |
| a user can still sign in after deactivation | by default only the org membership is suspended; turn on `deactivationBans` to also block the account (when it belongs to no other org) |
