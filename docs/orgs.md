# Organizations, roles and org-owned collections

Organizations (#79a) let one Claviger instance serve several groups (parishes, ministries, tenants), each with its own members and roles. With no organization created nothing changes: no extra checks run, and the new collections stay empty.

Design notes and the evidence behind the rules are in `docs/specs/79-orgs-roles-scim.md`. Invitations (79b, below) and the hosted org page ship with this release. SCIM provisioning (79c) is described in [`docs/scim.md`](scim.md).

## Model

Hidden system collections:

| Collection | Holds |
|---|---|
| `_orgs` | `name`, `slug` (unique, `^[a-z0-9-]{2,64}$`), `parent` (optional, one level), `archived`, `settings` |
| `_orgMembers` | `org`, `user`, `role`, `status`, `tags`, `source`; unique on `(org, user)` |
| `_orgRoles` | free-form labels (`org`, `name`, `description`), the targets of `_orgMembers.tags` |
| `_invitations` | email invitations (`org`, `email`, `role`, `tags`, `invitedBy`, `tokenHash`, `expires`, `status`, ...), see [Invitations](#invitations) |
| `_scimTokens`, `_scimGroups` | SCIM tokens and groups (79c), see [`docs/scim.md`](scim.md). `_orgMembers` also carries `externalId` |

- **Roles** are fixed and ranked: `owner` > `admin` > `member` > `viewer`. `viewer` has no special meaning to the engine; the rule presets simply leave viewers out of write rules.
- **Status** is `active` or `suspended`. A suspended member loses all access that goes through the rules, immediately.
- **Tags** are labels such as "youth leader" or "treasurer", scoped to one org. Use them in rules for anything the four roles cannot say.
- **Archiving** hides an org: every rule below checks `org.archived = false`, and the member routes refuse changes. Deleting is a separate, explicit action (see below).
- **Hierarchy** is one level (`parent`). Setting a parent is a superuser action. A parent cannot have a parent, and a parent with children cannot be deleted.

### Which users are members

`_orgMembers.user` points at one auth collection, `users` by default. To use another, set `CLAVIGER_ORGS_USER_COLLECTION=<name>` **before the first start with this version** (the migration reads it when it creates the collections). Afterwards the relation field of `_orgMembers` is the source of truth. If no suitable auth collection exists when the migration runs, it does nothing and the feature stays off.

### Who can create organizations

Superusers. Set `CLAVIGER_ORGS_USERS_CAN_CREATE=true` to let any signed-in user of the member collection create one; they become its owner. Only superusers set a `parent` or name another owner.

## API

All routes need authentication. API keys can read but never change organizations, and the three collections are on the API-key forbidden list.

| Route | Who | |
|---|---|---|
| `GET /api/orgs` | any user | the caller's live orgs with their role (`?archived=true` lists archived orgs they own). Superusers list all (`q`, `archived=true\|all`) |
| `POST /api/orgs` | superuser, or any user when enabled | `{name, slug?, parent?, owner?}`; `owner` is an email or user id |
| `GET /api/orgs/{id}` | members | id or slug. Non-members get 404 |
| `PATCH /api/orgs/{id}` | owner/admin (name, slug), owner (archive/restore), superuser (parent) | |
| `DELETE /api/orgs/{id}` | owner or superuser | archived orgs only, body `{"confirm": "<slug>"}`. Refused while child orgs or org-owned records exist |
| `GET /api/orgs/{id}/members` | members | owners/admins also see suspended members and emails |
| `POST /api/orgs/{id}/members` | owner/admin | `{user, role?, status?, tags?}`; `user` is an email or id of an existing account |
| `PATCH /api/orgs/{id}/members/{member}` | owner/admin | `{role?, status?, tags?}` |
| `DELETE /api/orgs/{id}/members/{member}` | owner/admin, or the member themself (leave) | |
| `GET /api/orgs/{id}/tags` | members | the tags of the org |
| `POST /api/orgs/{id}/tags` | owner/admin | `{name, description?}` |
| `DELETE /api/orgs/{id}/tags/{tag}` | owner/admin | the tag leaves every member that carried it |
| invitation routes | see [Invitations](#invitations) | |
| `GET /api/orgs/rule-templates` | superuser | the presets below, for a relation field named `?field=` (default `org`) |

Guards, enforced for every caller and backed by model hooks for any app-level write:

- The last active owner cannot be removed, demoted or suspended, not even by a superuser. Make someone else an owner first, or archive and delete the org.
- Nobody grants a role above their own, and nobody edits or removes a member ranked above them. An admin cannot touch an owner.
- Nobody changes their own status. Members can lower their own role and can leave.
- Unknown or hidden orgs answer 404, so a non-member cannot tell an org exists.
- Superusers bypass the hierarchy; their changes are audited with the superuser as actor.

Note that the `user` lookup by email answers 404 for every miss, but an org admin can still probe which emails have an account. Treat org admins as trusted for that.

Writes through the records API (`/api/collections/_orgMembers/records`) are superuser-only (null rules) and hit the same model-hook guards, but skip the hierarchy checks. Prefer the routes.

## Invitations

Org admins invite people by email. The invitee follows a link to a hosted page, signs in (or creates an account), and is added with the invited role. With no invitation created nothing changes.

### Routes

| Route | Who | |
|---|---|---|
| `POST /api/orgs/{id}/invitations` | owner/admin | `{email, role?, tags?}`. Creates the invitation and sends the email. Response carries `emailSent`; the token is never returned |
| `GET /api/orgs/{id}/invitations` | owner/admin | `?status=pending\|accepted\|revoked\|expired`, paged. A pending invitation past its expiry reads as `expired` |
| `POST /api/orgs/{id}/invitations/{inv}/resend` | owner/admin | new token, new expiry, email again. Also renews an expired invitation |
| `DELETE /api/orgs/{id}/invitations/{inv}` | owner/admin | revoke; the link stops working |
| `POST /api/invitations/accept` | the signed-in invitee | `{token}` |
| `POST /api/invitations/register` | the invitee without an account | `{token, password, passwordConfirm, name?}` (+ captcha token when sign-up captcha is on) |

API keys can read but never change invitations; `_invitations` is on the API-key forbidden list and has no API rules (superusers only), so the routes above are the way in.

### Rules

- **Roles.** Nobody invites above their own role (an admin can invite admins, members and viewers, not owners). The same holds for resend and revoke.
- **Token.** 32 random bytes, base64url, in the emailed link only. The database holds the SHA-256, so a database read does not give a working link. Single use: the status moves `pending` to `accepted`, `revoked` or `expired` and a used, revoked or expired token fails with one generic error ("This invitation is invalid or has expired.").
- **Expiry.** 7 days; `settings.invitationExpiryDays` (1 to 30) on the org changes it. A resend restarts the clock.
- **Limits.** One resend per minute per invitation, 20 new invitations per hour per org (`settings.invitationsPerHour`, 1 to 200). Both answer 429. The limits count rows, so a restart does not reset them. Token guesses on the hosted page and the accept/register routes are also limited per client IP.
- **One live invitation per email and org.** A second create answers 409 (resend instead). Inviting someone who is already a member (any status) answers 409.
- **Account linking.** An existing account is linked only when its email equals the invited address (case-insensitive) **and is verified**. Otherwise accept answers 403 with `data.invitation.code` of `email_mismatch` or `email_not_verified`, and the invitation stays usable. The hosted page offers to send the verification email.
- **Accept** adds the membership with `source: "invitation"` and the invited role and tags and marks the invitation accepted, in one transaction. An invitee who is already a member gets 409 and nothing changes.
- **Register** is for an invitee with no account: it creates the account in the member collection with the invited email, `verified = true` (the link proved the mailbox) and the given password, and accepts the invitation, all in one transaction. A weak password rolls everything back. It answers 409 `account_exists` when the email already has an account, so a leaked link cannot overwrite one. It does not sign the browser in; the page signs in right after through the ordinary endpoint, so every sign-in rule of the collection still applies. It needs password auth on the member collection (a passkey-only collection cannot use it) and honors the sign-up captcha.
- **Archived orgs** refuse create, resend and accept.

### The email

Sent through the app mailer (`Settings > Mail`, `meta.senderName` / `senderAddress`), to the invited address, with a link to `{meta.appURL}/orgs/invite/{token}`. Set `meta.appURL` to the public URL of the server or the links point at the default.

The default subject is "You are invited to join {ORG_NAME} on {APP_NAME}". An org overrides either part in its `settings` JSON (superuser edit of `_orgs`):

```json
{ "invitationEmail": { "subject": "Welcome to {ORG_NAME}", "body": "<p>{INVITER} invited you as {ROLE}.</p><p><a href=\"{INVITE_URL}\">Join</a></p>" } }
```

Placeholders: `{ORG_NAME}`, `{ORG_SLUG}`, `{ROLE}`, `{INVITER}`, `{INVITE_URL}`, `{EXPIRES}`, `{APP_NAME}`, `{APP_URL}`. Values are HTML-escaped in the body. The body goes inside the standard mail layout; the text part is derived from the HTML.

### Hosted pages

Both pages are served by Claviger, have no inline script or style, and use a strict Content-Security-Policy (`default-src 'none'`, same-origin scripts and styles; only when a captcha is required are that provider's domains opened). They sign in through the ordinary auth endpoints of the member collection: password, TOTP second factor, passkey, and the email code when OTP is enabled.

- `/orgs/invite/{token}` shows the org, the role and the invited address. With an account it offers sign-in then "Accept"; without one it offers account creation. Unknown, expired, revoked and used links show the generic message.
- `/orgs/{slug}` is the page for the people who run an org, so a parish coordinator needs no custom client and no access to `/_/`. After sign-in, members see who is in the org; owners and admins can change roles, status and tags of members, remove them, send, resend and revoke invitations, and add or delete tags. It is a shell until sign-in: it does not look the slug up, so slugs cannot be probed. Everything it does is a call to the routes above, so every guard applies.

Superusers manage the same things on the Organizations page of the admin UI (members, invitations, a link to the hosted page).

### Housekeeping

A daily job (05:13) marks pending invitations past their expiry as `expired` and deletes accepted, revoked and expired invitations that have not changed for 90 days. Every create, resend, revoke and accept is an audit entry (`_invitations` create or update with `meta.orgId`, the actor being the admin, or the invitee for accept and register); `tokenHash` is redacted. Webhooks fire from the collection events (`_invitations:create`, `_invitations:update`).

## Audit, webhooks, OIDC

- **Audit.** Every change of `_orgs`, `_orgMembers` and `_orgRoles` is an audit entry with the actor and `meta.orgId`. Any collection with an `org` (or `orgs`) field gets `meta.orgId` (`meta.orgIds`) on its entries too. A role change records `role` in `changedFields` with before and after.
- **Webhooks.** Event names are collection based: `_orgMembers:create`, `_orgMembers:update`, `_orgMembers:delete`, `_orgs:*`, and so on (see `docs/webhooks.md`). The payload record carries `org`, `user` and `role`.
- **OIDC** (`docs/oidc.md`). Add the scope `orgs` to a client's scopes and request it. The ID token, userinfo and JWT access tokens then carry `orgs` (a list of `{id, slug, role, tags}` of the active memberships in live orgs, at most 50, `orgs_truncated` when cut) and `org_roles` (`{orgId: role}`). A client can be **bound to one org** (`org` field of the client, `"org": "<id>"` on `POST /api/oidc/clients`): only active members can sign in, the claims are `org_id`, `org_slug`, `org_role`, `org_tags` for that org only, and a member who is suspended or removed loses userinfo and refresh at once.
- **Sessions** are per user, not per org; removing someone from one org never signs them out.
- **API keys** keep their scopes. Org-scoped keys come later.

## Org-owned collections

A collection is org-owned when it has a **required relation field named `org`** that points at `_orgs` (no cascade, so an org that still owns records cannot be deleted by accident), **with an index on `org`**. That is the whole contract. Records with several orgs use a multiple relation named `orgs` instead.

The rules read membership through the back relation `org._orgMembers_via_org`. All conditions on that path run against **the same membership row**, so `user ?= me && role ?= "admin"` means "I am an admin of this record's org", never "I am a member and somebody is an admin". Being admin of org B does not elevate you in org A.

Rules for the field `org`; `M` stands for the shared prefix:

```
M = org.archived = false && org._orgMembers_via_org.user ?= @request.auth.id && org._orgMembers_via_org.status ?= "active"
```

### Preset 1: members read and write, admins delete

```
list / view:  M
create:       M && (org._orgMembers_via_org.role ?= "owner" || org._orgMembers_via_org.role ?= "admin" || org._orgMembers_via_org.role ?= "member")
update:       <create rule> && (@request.body.org:isset = false || (@collection._orgMembers:n.org ?= @request.body.org && @collection._orgMembers:n.user ?= @request.auth.id && @collection._orgMembers:n.status ?= "active" && (@collection._orgMembers:n.role ?= "owner" || @collection._orgMembers:n.role ?= "admin" || @collection._orgMembers:n.role ?= "member")))
delete:       M && (org._orgMembers_via_org.role ?= "owner" || org._orgMembers_via_org.role ?= "admin")
```

The tail of the update rule stops a writer from moving a record into an org where they have no write access. Without it, a member could re-point `org` at any org. Keep the alias `n` distinct from other `@collection._orgMembers` aliases in the same rule.

Other presets, generated by `GET /api/orgs/rule-templates` and shown on the Organizations page of the admin UI (copy buttons): *members read and write, members delete*; *members read, admins write*; *members read, superusers write* (null write rules).

### Cookbook

| Goal | Rule |
|---|---|
| Any active member reads | `M` |
| Owners and admins only | `M && (org._orgMembers_via_org.role ?= "owner" \|\| org._orgMembers_via_org.role ?= "admin")` |
| Members with a tag | `M && org._orgMembers_via_org.tags.name ?= "youth"` |
| Members of the org **or of its parent** (one level) | `org.archived = false && ((org._orgMembers_via_org.user ?= @request.auth.id && org._orgMembers_via_org.status ?= "active") \|\| (org.parent._orgMembers_via_org.user ?= @request.auth.id && org.parent._orgMembers_via_org.status ?= "active"))` |
| Parent admins reach child records | add `&& (org.parent._orgMembers_via_org.role ?= "owner" \|\| ...)` to the parent branch, or use `OrgParentRule` |
| Record shared by several orgs (`orgs`) | `orgs.archived ?= false && orgs._orgMembers_via_org.user ?= @request.auth.id && orgs._orgMembers_via_org.status ?= "active"` (any of the orgs) |
| Create into the org named in the body | `@request.auth.id != "" && @collection._orgMembers:m.org ?= @request.body.org && @collection._orgMembers:m.user ?= @request.auth.id && @collection._orgMembers:m.status ?= "active" && @collection._orgMembers:m.role ?= "admin"` |

Go helpers with the same strings: `core.OrgMemberRule`, `core.OrgTagRule`, `core.OrgParentRule`, `core.OrgNoMoveRule`, `core.OrgRulePresets`. They are what the tests (`apis/orgs_rules_test.go`) run against real collections.

### Things to know

- **Multi-valued paths need the `?` operators.** Back-relations are multi-valued. Use `?=`, `?>`, `?~`. A plain `>` or `=` on such a path requires every matched row to satisfy it and silently denies. (`org.archived = false` is fine: `org` is a single relation.)
- **Silent denials.** A wrong rule returns an empty list, not an error. A denied create answers 400, a denied update/view/delete 404, a denied list 200 with no items.
- **System collection rules are fixed.** The list and view rules of the three org collections are set once by the migration and cannot be edited.
- **Postgres.** The rules use only the back-relation and `@collection` syntax that the dialect layer preserves; run the cookbook tests on it when you enable it.

### Performance

Measured on 500 orgs, about 20,000 memberships and 200,000 records in one org-owned collection (single runs, warm cache, order of magnitude):

- The membership lookup is an index search per record (unique `(org, user)`), never a scan of `_orgMembers`. The cost is the scan of the org-owned table.
- An unfiltered list with a total count took roughly 90 to 290 ms. With `filter=org="<id>"` and an index on `org` it took about 1 ms.
- `skipTotal=1` avoids the count: about 2 to 3 ms for a member of one org. A non-member still scans.

So: index `org` on every org-owned collection, have clients send `filter=org="<id>"` and `skipTotal=1` on big collections, and avoid unfiltered lists with totals past roughly 100k rows per collection.
