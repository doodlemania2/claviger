# Spec: organizations, roles, invitations and SCIM (#79)

Date: 2026-11-08. Status: spec only. No Go code in this repo changed.

Issue #79 is split into three deliverables, in this order:

- **79a** organizations, memberships and roles.
- **79b** email invitations and member management.
- **79c** SCIM 2.0 provisioning.

Owner decision: org-scoped access rules must not change upstream's rule resolver (`core/record_field_resolver.go`). Everything below uses existing filter syntax, system collections and helper collections. Section 3 shows the rules and the result of running each one.

## Recommendation

Build 79a on two system collections, `_orgs` and `_orgMembers`. A membership row holds one built-in `role` (`owner`, `admin`, `member`, `viewer`), a `status` (`active`, `suspended`) and optional free-form `tags` for app-defined roles such as "youth leader". Do not build a custom permission-string role engine in v1. A record becomes org-owned by having a relation field named `org` that points at `_orgs`. Collection rules test membership with back-relations through that field. The rules in section 3 were run against a real test app and all pass.

Membership writes do not go through collection rules. They go through small custom routes (`/api/orgs/{id}/members`) that enforce what rules cannot express (last owner, no escalation above your own role). System collection rules cannot be edited after creation, so the rules are fixed in the migration, and read-only for members.

79b adds `_invitations` with hashed single-use tokens. 79c adds a separate `/scim/v2` surface with its own per-org bearer tokens, targeting Entra ID and Okta first. Google Workspace is not a general SCIM client for custom apps, so it is last.

Upstream files touched are one line in `core/base.go` (hook registration) and the route-binding call in `apis/base.go`. The resolver, the filter grammar and the collection validator stay untouched.

## 1. Goals and non-goals

### 79a: organizations, memberships, roles

Goals:

- A user can belong to many organizations, each with its own role.
- Any collection can opt in to org ownership, and its API rules can restrict list, view, create, update and delete to members, and to chosen roles.
- Org admins (not only superusers) manage members.
- The model leaves room for hierarchy (#84), credential gates (#81) and households (#80) without a schema rewrite.
- Org context is available in OIDC tokens (#71), the audit log (#74) and webhooks.

Non-goals:

- No `@request.auth.hasRole(org, "admin")` helper. That needs a resolver change. The issue text should be edited to say "rule snippets" instead.
- No custom permission strings in v1 (see section 2.3).
- No per-org billing, quotas, branding or custom domains.
- No cross-org record sharing beyond the multi-org relation shown in 3.6.
- No automatic tenant isolation of the whole database. Isolation is per collection that opts in.

### 79b: invitations

Goals:

- Invite by email with a role. Single-use, expiring, revocable links.
- Pending-invite list with resend and revoke.
- Accepting links an existing account or creates a new one.
- Org admins can run all of it from a member-management view.

Non-goals:

- No bulk CSV import in v1 (SCIM covers bulk).
- No invitation to a user who is not reachable by email (minors, see #80).
- No SMS invitations.

### 79c: SCIM 2.0

Goals:

- `/scim/v2/Users` and `/Groups` so Entra ID and Okta can create, update and deactivate users and group memberships.
- Bearer tokens per org, managed in the admin UI and by org admins.
- Entra's SCIM validator passes (the issue's definition of done).

Non-goals:

- No SCIM client (outbound provisioning to other apps). #85 may add a one-way sync later.
- No `/Bulk`, no `/Me`, no ETag concurrency in v1.
- No complex filters (full RFC 7644 grammar). A subset is enough for the clients.
- No SCIM for superusers.

## 2. Data model

All collections are system collections created by a fork migration (next timestamp after `1780000800`). Names start with `_` like the other fork collections.

### 2.1 Collections

`_orgs`

| Field | Type | Notes |
|---|---|---|
| `name` | text, required | display name |
| `slug` | text, required, unique | `^[a-z0-9-]{2,64}$`, used in URLs and the `org_slug` claim |
| `parent` | relation to `_orgs`, single, optional | unused in 79a, reserved for #84. Cascade: no (deleting a parent with children is refused) |
| `status` | select `active`, `archived` | archived orgs deny everything through rules |
| `settings` | json | invitation expiry, allowed email domains, default role. Small |
| `created`, `updated` | autodate | |

Index: unique on `slug`; non-unique on `parent`.

`_orgMembers`

| Field | Type | Notes |
|---|---|---|
| `org` | relation to `_orgs`, required, cascade delete | |
| `user` | relation to the auth collection, required, cascade delete | see 2.5 for which collection |
| `role` | select `owner`, `admin`, `member`, `viewer`, required | |
| `status` | select `active`, `suspended` | SCIM deactivate and safeguarding lapse use `suspended` |
| `tags` | relation to `_orgRoles`, multiple, optional | app-defined labels |
| `source` | select `manual`, `invitation`, `scim`, `sso` | who created the row. Lets SCIM manage only what it made |
| `created`, `updated` | autodate | |

Indexes (all three are needed, section 3.8 has the evidence):

- unique on `(org, user)`: one membership per person per org, and it is the index the rule join uses.
- on `user`: for "my orgs" and the auth-side rule form.
- on `(org, role)`: for member-management lists.

`_orgRoles` (labels, optional in v1): `org`, `name`, `description`. Unique on `(org, name)`. Only `_orgMembers.tags` points here. It is the cheap step toward custom roles (2.3).

`_invitations` (79b): `org`, `email`, `role`, `tags`, `tokenHash`, `invitedBy`, `status` (`pending`, `accepted`, `revoked`, `expired`), `expires`, `acceptedBy`, `acceptedAt`, `lastSentAt`, `sendCount`. Indexes: unique on `tokenHash`; on `(org, email)` filtered to `status='pending'` (unique, so one live invite per email per org); on `expires`.

`_scimTokens` and `_scimIdentities` (79c) are in section 4.8.

### 2.2 Cascade behavior

- Delete a user: their memberships are removed (cascade). Their invitations stay (`invitedBy` is cleared, not required).
- Delete an org: members, roles, invitations and SCIM rows cascade. Org-owned collections keep their `org` field as a **required** relation **without** cascade, so deleting an org that still owns records is refused by the database. That is the safe default. The admin UI offers "archive" first and a separate "delete org and its data" action that the owner must confirm. Do not default to cascade: one click would delete a parish's records.
- Last owner: a model hook refuses to delete, demote or suspend the last active `owner` of an org. It is a guard in Go, since rules cannot express it.

### 2.3 Role models compared

| | A. Fixed roles | B. Custom roles with permission strings | C. Fixed roles plus tags (recommended) |
|---|---|---|---|
| Shape | `role` select with four values | `_roles` rows holding `permissions: ["docs.write", ...]` | Fixed `role` plus optional `tags` |
| Rules | One short snippet per role level | Rules must test a permission, which is a JSON array on a related row | Role snippets plus `tags.name ?= "youth"` |
| Tested in rules? | Yes (3.1 to 3.5) | **No.** Matching inside a JSON array through a relation is not a form I verified. A safe version needs a `_rolePermissions` row per permission, which multiplies joins | Yes (3.7) |
| Admin UI | Dropdown | A permission editor and a catalog of permission names that each app must define | Dropdown plus tag picker |
| SCIM group mapping | A group maps to one role | A group maps to a role | A group maps to a role or a tag |
| Church fit | Owner/admin/member/viewer covers "pastor, coordinator, volunteer, viewer" | Flexible but each parish would invent its own set | Tags cover "youth leader", "treasurer", "safeguarding coordinator" |
| Cost | Smallest | Large: catalog, UI, rule templates, migration of permissions, cache | Small |
| Risk | Cannot express "can edit but not delete" | Permission sprawl; hard to audit | Tags are not ranked, so authors must write the rule |

Recommendation: C. Ship A's four ranked roles and the tag mechanism. A rule that needs "can edit but not delete" can use `tags`. Revisit B only if real users ask for permission strings. It can be added later without breaking C: a `_roles` row would just be a named bundle of a built-in role and tags.

Rank order for snippets: `owner` > `admin` > `member` > `viewer`. `viewer` means read-only. It has no special meaning to the engine: it is a convention the rule templates follow (viewers appear in list and view rules, and not in write rules).

### 2.4 Org-owned collections (opt-in)

A collection is org-owned if it has a relation field named `org` that points at `_orgs`. That is the whole contract. There is no new collection flag, so the upstream `Collection` model and its validator do not change.

- The admin UI gets an "Make org-owned" action that adds the field, marks it required, adds an index on `org`, and offers rule templates (3.1 to 3.5).
- Multi-org records use a multiple relation named `orgs` (3.6). The convention for audit and webhooks (section 4) looks for `org` first, then `orgs`.
- Default org on create: if the caller does not send `org` and has exactly one active membership, a fork hook fills it. If they have several, the field is required and the client must say which. This is a convenience, not a security control: the create rule decides.

### 2.5 Which auth collection

`_orgMembers.user` must point at one collection. Claviger apps normally have one user collection, but `users` is not a system collection and can be renamed. Options:

1. A setting `orgs.userCollection` (default `users`), checked when the migration creates the relation. Changing it later is a migration.
2. One membership collection per auth collection.
3. Store `userCollection` + `userId` text pair (no relation, no joins).

Recommendation: 1. Rules need a real relation to join. Option 3 loses `_via_` entirely.

## 3. Rules without resolver changes

### 3.1 Method

I built a throwaway Go module in the session scratchpad that imports this repo through a `replace` directive. It uses `tests.NewTestApp()`, creates the collections of section 2 (the `_orgMembers` one with the indexes shown), seeds users and memberships, mounts the real router, and sends HTTP requests with each user's auth token. It is not committed. Test users:

- `alice`: owner of P1. `bob`: admin of P1. `carol`: member of P1 and admin of P2. `dave`: viewer of P1. `erin`: member of P2. `frank`: no membership. `gina`: admin of the diocese D (parent of P1), no parish membership.
- Orgs: D (diocese), P1 (parent D), P2 (no parent). Each org-owned collection has one record in P1 and one in P2.

Grammar facts verified in `core/record_field_resolver_runner.go` and by the runs:

- A back-relation is written `<collection>_via_<field>`. The regex is `^(\w+)_via_(\w+)$`, so a collection name that starts with `_` works: `_orgMembers_via_org`.
- Back-relations and `@collection` joins are multi-valued. Use the `?` operators (`?=`, `?>`, `?~`). A plain `>` on a multi-valued path requires **every** matched row to satisfy it, which silently denies (I hit this with `expires > @now`: it returned nothing until it became `?>`).
- The join alias is derived from the path. Two conditions that use the same `_via_` path share one join, so they are evaluated on the **same membership row**. That is what makes `user ?= me && role ?= "admin"` mean "I am an admin", not "I am a member and somebody is an admin". For `@collection` joins the same holds when you reuse the alias (`@collection._orgMembers:m`).
- Create rules can reference the new record's own `org` field (3.2) and `@request.body.org`.
- A denied create returns 400 ("Failed to create record."), not 403. A denied update, view or delete returns 404. A denied list returns 200 with no items. Client code and tests should expect that.

### 3.2 Rules, with results

Notation: `M` is the member snippet and `A` is the admin snippet.

```
M = org._orgMembers_via_org.user ?= @request.auth.id && org._orgMembers_via_org.status ?= "active"
A = M && (org._orgMembers_via_org.role ?= "admin" || org._orgMembers_via_org.role ?= "owner")
```

The membership-only form (no `status`) was run as form A below. The `status` clause was run separately in 3.2.7. The snippets are the same shape, so the same-row property holds.

| # | Rule (collection `docs`, field `org`) | Result in the prototype |
|---|---|---|
| 3.2.1 list/view/create: `org._orgMembers_via_org.user ?= @request.auth.id` | alice sees only P1 doc; carol sees P1 and P2 docs; erin sees only P2; frank and guests see nothing. Create: carol in P1 OK, bob in P1 OK, bob in P2 denied (400), frank in P1 denied (400). **Works.** |
| 3.2.2 update/delete admin form: `org._orgMembers_via_org.user ?= @request.auth.id && (org._orgMembers_via_org.role ?= "admin" \|\| org._orgMembers_via_org.role ?= "owner")` | bob updates P1 doc: 200. carol updates the P1 doc: **404**, although P1 has an admin (bob) and carol is an admin of P2. carol updates the P2 doc: 200. dave (viewer) and erin (member, other org): 404. alice deletes P1 doc: 204; carol deletes it: 404. **Works, and the same-row property is proven by the carol case.** |
| 3.2.3 same intent with `@collection` and an alias: `@collection._orgMembers:m.org ?= org && @collection._orgMembers:m.user ?= @request.auth.id && @collection._orgMembers:m.role ?= "admin"` | Same outcomes as 3.2.1 and 3.2.2 for list, update and create. **Works.** It is longer and its join is not tied to the record's path, so prefer 3.2.1 for new work. |
| 3.2.4 auth-side back-relation: `@request.auth._orgMembers_via_user.org ?= org` | alice sees P1 only; carol sees both; erin P2; frank nothing. **Works.** It reads the same data from the user's side. It cannot also test the role of that same row against the record's org with a second `?=` shortcut, so use it for member-only rules. |
| 3.2.5 create by body org, admins only: `@request.auth.id != "" && @collection._orgMembers:m.org ?= @request.body.org && @collection._orgMembers:m.user ?= @request.auth.id && @collection._orgMembers:m.role ?= "admin"` | bob in P1: 200. bob in P2: 400. carol in P2 (admin there): 200. carol in P1 (member only): 400. **Works.** Use this form when the create must depend on the submitted org, not the stored one. |
| 3.2.6 update that stops moving a record to an org you do not belong to: `<member rule> && (@request.body.org:isset = false \|\| @collection._orgMembers:n.org ?= @request.body.org && @collection._orgMembers:n.user ?= @request.auth.id)` | carol (member of P1 and P2) moves a P1 record to P2: 200. erin (P2 only) moves a P2 record to P1: **404**. erin editing a field without `org` in the body: 200. **Works.** Use a distinct alias (`n`) from the one in the create rule. |
| 3.2.7 suspended members: `org._orgMembers_via_org.user ?= @request.auth.id && org._orgMembers_via_org.status ?= "active"` | erin's P2 row is `suspended` while carol's is `active` in the same org. erin sees nothing, carol sees both docs. **Works** (same-row again). |

### 3.3 Membership collection rules

Test of rules on `_orgMembers` itself, to see if raw rules could manage members:

- list/view `org._orgMembers_via_org.user ?= @request.auth.id`: members see co-members of their orgs only. carol (two orgs) sees the rows of both. erin sees only P2's rows. frank sees none. **Works.**
- create with an admin check on `@request.body.org` and `@request.body.role != "owner"`: bob adds frank as `member`: 200. bob adds as `owner`: 400. carol (member of P1) adds: 400. **Works for those cases.**
- update/delete admin rule: not exercised beyond create (same shape as 3.2.2).

Rules cannot say: "an admin cannot edit an owner", "do not remove the last owner", "an admin cannot promote to a role above their own", "SCIM-owned rows are read-only to humans". So the recommendation is: list and view rules as above, **no create/update/delete rules** (superuser only), and all writes through `/api/orgs/{id}/members` routes that enforce those invariants and write the rows with the app. That also means the rule text never has to change after the system collection exists (system collection rules cannot be edited later: "System collection API rule cannot be changed." I hit this in the prototype). Set the list/view rules once, in the migration.

### 3.4 Tags and credentials (for #81)

- Tag gate: `org._orgMembers_via_org.user ?= @request.auth.id && org._orgMembers_via_org.tags.name ?= "youth"`. carol (tagged `youth` in P1) sees the P1 doc. bob (no tag) sees nothing. **Works.**
- Credential gate (a sketch of #81): a `_credentials` collection with `user`, `type`, `expires`. Rule: `<member rule> && @collection._credentials:c.user ?= @request.auth.id && @collection._credentials:c.type ?= "safe_environment" && @collection._credentials:c.expires ?> @now`. carol (valid until 2099) sees both of her orgs' docs. bob (credential expired 2020) and dave (none) see nothing. **Works.** Access lapses at the moment of expiry with no cron: the date is compared on every request.
  Caveat: the credential is matched on the user, not on the org. If a credential must be per org, add an `org` relation to `_credentials` and a second condition `@collection._credentials:c.org ?= org`.

### 3.5 Multi-org records and hierarchy

- Multi-org: a record with `orgs` (multiple relation) holding [P1, P2]. Rule `orgs._orgMembers_via_org.user ?= @request.auth.id` shows it to dave (P1), erin (P2), and hides it from gina and frank. **Works.** Semantics are "any of the orgs".
- One level up: `org._orgMembers_via_org.user ?= @request.auth.id || org.parent._orgMembers_via_org.user ?= @request.auth.id`. gina (diocese admin, no parish membership) sees the P1 doc. dave sees P1. erin sees P2. **Works.**
- One level up, admin only: `org.parent._orgMembers_via_org.user ?= @request.auth.id && org.parent._orgMembers_via_org.role ?= "admin"`. gina sees the P1 doc. dave and alice (parish roles) see nothing. **Works.**

The limit: each hop is written into the rule, so depth is fixed per rule. Section 5 covers how to remove that limit without touching the resolver.

### 3.6 Performance

I loaded 500 orgs, about 20,000 memberships and 200,000 records in one org-owned collection (scratch data, not committed), with the indexes of 2.1.

- The generated join plan is `SCAN docs`, then `SEARCH _orgs` by primary key, then `SEARCH _orgMembers` using the unique `(org, user)` index. So the membership lookup is an index search per record. There is no scan on `_orgMembers`.
- The scan on the org-owned table is the cost. A list with a total count on 200k rows took roughly 90 to 290 ms, for members and non-members alike. Adding an index on `docs(org)` did **not** change the unfiltered list time.
- With a client filter on `org` (`filter=org="X"`) the same list took about 0.4 to 1 ms with an index on `docs(org)` (about 12 to 25 ms without it).
- `skipTotal=1` avoids the count for the member case: about 2 to 3 ms for a member of one org (the query stops at the page size). A non-member still scans (about 195 ms), since no row ever matches.

What this means:

- Always index `(org, user)` on `_orgMembers` and `org` on every org-owned collection. The "Make org-owned" action does both.
- Clients should send `filter=org="<id>"` and `skipTotal=1` for big collections. The admin UI helper should do so by default.
- Past roughly 100k rows per org-owned collection, an unfiltered list with total count is the thing to avoid. This is a property of the existing rule engine (it filters after join), not of the org design.
- These are single-run timings on a laptop with a warm cache. Treat them as order of magnitude.

## 4. Integration

### 4.1 OIDC claims (#71)

Today `oidc/claims.go` emits `sub`, `collection`, `email`, `name`, etc. Add:

- `orgs` scope. When granted, the ID token and userinfo carry `orgs`: a list of `{id, slug, role, tags}` for the user's **active** memberships. Cap the list (for example 50) and set `orgs_truncated` when cut.
- If the OIDC client is bound to one org (new optional `org` on `_oidcClients`), the token carries `org_id`, `org_slug`, `org_role`, `org_tags` for that org only, and sign-in is refused for non-members. That is the issue's `org_id` claim, and it is the right default for apps such as Grafana per parish.
- Add the claim names to `SupportedClaims`. The claims are read at token time, so a role change shows at the next refresh/userinfo call.
- Files: `oidc/claims.go`, `oidc/oidc.go` (fork-owned).

### 4.2 API key scopes (#95)

Keys act as their owner, and rules run after the scope check, so a key can only restrict (see `apis/apikey_auth.go`). A key owned by a user already sees only the orgs that user belongs to. Nothing is needed for correctness.

For a key limited to one org, add a scope `org:<id>`. `enforceAPIKeyScopes` already sits in front of every `/api/collections/{c}/records` request:

- reads: inject `org="<id>"` into the `filter` query parameter for org-owned collections.
- writes: reject a body whose `org` differs; for update/delete, look up the record's `org` first.

Both are in fork-owned code. The collections `_orgs`, `_orgMembers`, `_orgRoles` and `_invitations` go on `APIKeyForbiddenCollections` (membership changes must not be possible with a stolen key). `_scimTokens` too.

### 4.3 Audit log (#74)

- All membership, org and invitation writes are audited by the existing record hooks. Add to the capture: if the collection has `org` (or `orgs`), set `meta.orgId` (or `meta.orgIds`).
- `ChangedFields` for `_orgMembers` includes `role`, so a role change is visible with before/after.
- For SCIM calls the actor is the token, not a user. Add an actor of type `scim` (or use `system` with `meta.scimTokenId`); use the same shape as `APIKeyId`. Recommendation: `meta.scimTokenId`, and actor type `system`, so no enum change.
- `_invitations.tokenHash` and `_scimTokens.tokenHash` must go on the audit redaction list (same as the webhook secret treatment).
- An org-scoped audit view for org admins is not possible through rules, since `_auditLog` is a system collection with superuser rules. Add `GET /api/orgs/{id}/audit` (read-only, filtered by `meta.orgId`) in a later milestone.

### 4.4 Webhooks (#76)

Webhook events are built from audit entries, filtered by `collection:action`. So `_orgMembers:create`, `_orgMembers:update`, `_orgMembers:delete` and `_invitations:*` work without code, and the payload includes `record.org` and `record.role`. The issue text names events like `org.member.added`. Recommendation: ship the collection-based names in 79a, document them, and add friendly aliases (`org.member.added`, `.removed`, `.role_changed`) later by extending `webhookEventTypes` in the fork-owned `core/webhooks.go`. A per-org endpoint filter (`_webhooks.org`) is a later add.

### 4.5 Sessions (#93)

Sessions are per user, not per org. Rules read membership live, so removing a member takes effect on the next request with no session change. Two cases need `core.RevokeSessions`:

- SCIM deactivation of a **managed** user (the user's only memberships are SCIM-sourced, 4.8): revoke sessions and ban or disable sign-in.
- "SSO required" for a domain (#92): out of scope here.

Removing someone from one org must never revoke their sessions if they belong to other orgs.

### 4.6 Admin UI

Svelte files under `ui/src/orgs/` (new, fork-only): org list, org detail with members, pending invitations, SCIM tokens, and a rule-snippet picker for org-owned collections. Existing pages that change: the collection editor gets the "Make org-owned" action (a small hook into the field list, in a new file if possible). `ui/dist` is rebuilt as always.

Org admins who are not superusers cannot use the superuser admin UI (`/_/`). So the member-management view for org admins is a **separate page in the app's own client** built on `/api/orgs/...`. Recommendation: ship the API routes and the admin UI view first. Add a minimal hosted page `/orgs/{slug}` (like the OIDC login page) in 79b so a parish coordinator needs no custom app. See open question 3.

### 4.7 Invitations flow (79b)

Routes (all JSON, under `/api/orgs/...`):

| Route | Who | What |
|---|---|---|
| `POST /api/orgs/{id}/invitations` | org admin | `{email, role, tags}`. Creates the row, sends the email |
| `GET /api/orgs/{id}/invitations` | org admin | pending/accepted list |
| `POST /api/orgs/{id}/invitations/{inv}/resend` | org admin | rotates the token, resets expiry, rate-limited |
| `DELETE /api/orgs/{id}/invitations/{inv}` | org admin | status `revoked`; the token stops working |
| `POST /api/orgs/invitations/accept` | token holder | `{token}` while signed in |
| `POST /api/orgs/invitations/register` | token holder | `{token, password, name...}`; creates the account |

Token: 32 random bytes, base64url, sent in the link, stored only as a SHA-256 hash. The issue says "signed". A random hashed token needs no signing key, can be revoked, and is single-use by status. It meets the intent. Default expiry is 7 days (`_orgs.settings`).

Accept logic:

1. Look up by hash. Reject unknown, revoked, accepted or expired the same way (one generic error, to avoid probing).
2. Signed in as a user whose verified email equals the invite email: create the membership, mark accepted.
3. Signed in with a different email: refuse by default. An org setting can allow "link anyway" after the user confirms; off by default because it lets a forwarded link attach the wrong account.
4. Not signed in: if an account exists for that email, ask them to sign in and then accept. If not, `register` creates it with `verified=true` (the link proved the mailbox) and the membership.
5. Accepting writes the membership with `source=invitation` and one audit entry with the inviter as actor-of-record in `meta`.

Email: use the existing mailer through `app.NewMailClient()`, with a fork template (HTML and text). Resend rate limit: 1 per 60 seconds per invitation, 20 invitations per hour per org (setting). Registration and accept are rate-limited by IP like other auth routes.

Expired rows are marked `expired` lazily on read and by a cron job that also deletes old rows after a retention period (90 days).

### 4.8 SCIM 2.0 (79c)

Standards: RFC 7643 (schema) and RFC 7644 (protocol). Base path `/scim/v2`.

**Endpoints** (v1):

- `GET /ServiceProviderConfig`, `GET /ResourceTypes`, `GET /Schemas`.
- `GET|POST /Users`, `GET|PUT|PATCH|DELETE /Users/{id}`.
- `GET|POST /Groups`, `GET|PUT|PATCH|DELETE /Groups/{id}`.
- `ServiceProviderConfig` advertises `patch.supported=true`, `bulk=false`, `filter.supported=true` (`maxResults` 200), `changePassword=false`, `sort=false`, `etag=false`, and `authenticationSchemes` with the OAuth bearer token scheme.
- Media type `application/scim+json` on responses. Accept `application/json` too (both Entra and Okta send it).
- Error body: `{"schemas":["urn:ietf:params:scim:api:messages:2.0:Error"],"status":"409","scimType":"uniqueness","detail":"..."}`.

**Auth.** One bearer token per org, from `_scimTokens`:

- Fields: `org`, `name`, `prefix`, `tokenHash`, `expires` (optional), `lastUsed`, `revoked`, `createdBy`. Format `clv_scim_<prefix>_<secret>`. Show the secret once.
- Why not reuse API keys: an API key acts as a user and runs rules as that user. A SCIM token must not be a user. It can only call `/scim/v2` for its own org. Reusing the hash and prefix helpers from `core/apikey.go` is fine; sharing the collection is not. `enforceAPIKeyScopes` already denies non-`/api` routes to non-`*` keys, so a SCIM token and an API key cannot be confused.
- Rate limit failures per client the way `loadAPIKeyAuth` does. Org admins create and revoke tokens (`POST /api/orgs/{id}/scim-tokens`). Rotating gives overlap: two live tokens per org.

**Users to records.** The SCIM Users resource maps to a record in the org's configured auth collection (setting from 2.5):

| SCIM | Claviger |
|---|---|
| `id` | the auth record id |
| `userName` | `email` (we use email as the unique login handle; `userName` must be an email or it is rejected with `invalidValue`) |
| `emails[type eq "work"].value` / `emails[primary]` | `email` (if `userName` differs, `userName` wins) |
| `name.givenName`, `name.familyName`, `displayName` | the `name` field if present (combined), else ignored |
| `active` | the membership `status` (`active`/`suspended`) |
| `externalId` | `_scimIdentities.externalId` |
| enterprise extension (department, manager...) | ignored in v1 |
| `meta.created`, `meta.lastModified`, `meta.location`, `meta.resourceType` | from the record |
| `password` | **never accepted**; response omits it. Provisioned users sign in by SSO, passkey or a reset email |

`_scimIdentities`: `org`, `user`, `externalId`, `scimToken` (which token created it). Unique on `(org, externalId)` and on `(org, user)`. Needed since `externalId` is the IdP's key and must not pollute the user collection.

A SCIM Users listing is scoped to **members of the token's org** (via `_orgMembers`). Create:

1. If an auth record with that email exists, link it (create membership and identity). Do not modify its password or profile fields, apart from empty ones. This avoids a provider overwriting a user's own data across orgs.
2. If not, create it with `verified=false` and a random password, send no welcome mail (setting), create membership with `source=scim` and default role.
3. If a membership exists, return 409 with `scimType: uniqueness` (Entra relies on 409 to switch to a lookup).

`DELETE /Users/{id}` removes the **membership** (and identity), not the account. If the user has no other memberships and the record was created by SCIM, the org setting `deleteOrphanedAccounts` (default off) decides. `active=false` suspends the membership (and revokes sessions if managed, 4.5). Recommendation: soft by default.

**Groups.** A SCIM group does not create an org. Mapping options:

1. Group = org role (or tag). `_scimGroups`: `org`, `externalId`, `displayName`, `role`, `tag`. Members of the group get that role/tag.
2. Group = child org (needs #84).
3. Group = org membership list (one group per org).

Recommendation: 1, with the mapping configured in the admin UI per group (an unmapped group is accepted and stored but grants nothing). The highest mapped role wins when a user is in several groups. Removing a user from a group removes only that role/tag, never the membership. This is also how #92 attribute/group mapping can reuse the same table later.

**PATCH.** Support RFC 7644 section 3.5.2 `add`, `replace`, `remove` with:

- a path (`active`, `userName`, `name.givenName`, `emails[type eq "work"].value`, `members`, `members[value eq "<id>"]`);
- no path, where `value` is an object of attributes (Entra and Okta both send this for `replace`);
- operation names in any case (`Replace`, `replace`, `REPLACE`);
- `active` as a boolean or the strings `"True"`/`"False"`.

On `members` for Groups: `add` appends, `replace` swaps the set, `remove` with a filter path removes one. `remove` of `members` without a filter removes all.

**Filtering (subset).** Operators `eq`, `ne`, `co`, `sw`, `pr`, `and` on `userName`, `externalId`, `emails.value`, `displayName`, `id`, `meta.lastModified` (`gt`, `ge`). Parse into a small AST in the SCIM package and translate to a query on the membership join. No `or`, no `not`, no nested attribute filters beyond `emails[...]`. Everything else returns 400 `invalidFilter`. Pagination: `startIndex` is 1-based, `count` capped at 200, `totalResults` returned.

**Identity provider quirks** (from the vendors' public SCIM guides and the RFCs; verify against current docs and a test tenant before locking the design):

- **Entra ID** (first target; its validator is the definition of done):
  - Uses the Entra gallery "non-gallery app" provisioning with a tenant URL and a secret token.
  - Probes with `GET /Users?filter=userName eq "x"` and `GET /Groups?filter=displayName eq "x"` before create, and expects an empty `ListResponse` (not 404).
  - Sends PATCH with `"op": "Replace"` (capitalized) and may send `active` as the string `"False"`. Sends the `urn:ietf:params:scim:schemas:extension:enterprise:2.0:User` extension.
  - Group member operations use `path: "members"` with `value: [{"value": "<id>"}]`, and removal with `members[value eq "<id>"]`.
  - It deactivates by PATCH `active=false`, and deletes only for removed-from-scope users if configured. It expects the created resource id in the 201 body and a `Location` header.
  - The `/Users/{id}` lookup after a 409 must return the same resource.
- **Okta**:
  - Uses Bearer (or Basic) and probes `GET /Users?filter=userName eq "x"&startIndex=1&count=1`.
  - Pushes `active` changes by PUT or PATCH; PATCH may carry the full user in a path-less `replace`.
  - Group push uses `PATCH /Groups/{id}` with `add` and `remove` members. Supports "import users" via paged `GET /Users`.
  - Expects 201 on create, 200 on update, 204 or 200 on delete. It is strict about `totalResults`, `itemsPerPage` and `startIndex` in lists.
- **Google Workspace**: Google does not provide a general SCIM client for custom apps (its auto-provisioning is limited to a catalog of pre-integrated apps). For Google, use inbound OIDC/SAML (#92) and a directory sync connector (#85), not SCIM. This is lower confidence; verify against Google's current Cloud Identity documentation before documenting it.

**Where it lives.** A new package `scim/` (fork-only, like `oidc/`) with its own handlers, mounted by `apis/base.go` under `/scim/v2`, outside the `/api` prefix so API-key scope enforcement and the records middleware do not apply. Writes go through the same Go functions as the member routes, so audit, webhooks and the last-owner guard behave the same.

## 5. Church fit

How later work builds on 79a with no rework.

### #84 hierarchy (diocese, parish, ministry)

- `_orgs.parent` is in the 79a schema already (unused). Names are free text, so denominations pick their own words; add an optional `kind` text field in #84 for display.
- Rules with one hop (`org.parent._orgMembers_via_org...`) work today (3.5). Depth is fixed per rule, and the deepest chain needed is small (3 levels), so rules for a three-level church can be written out.
- To get "any depth" without touching the resolver, #84 adds a derived table `_orgClosure(ancestor, descendant)` kept in sync by org hooks. A rule then reads `@collection._orgClosure:c.descendant ?= org && @collection._orgMembers:m.org ?= @collection._orgClosure:c.ancestor && ...`. It is a helper collection plus a hook, no resolver change. I did not prototype this rule, so treat it as a design to prove in #84.
- "Scoped oversight" for higher levels is a membership at the higher org with a role, plus rules that include the ancestor hop. Admins at the diocese do not need parish accounts (proven for one hop in 3.5).
- "Transfer with consent" is an invitation (79b) from the target org plus the removal at the source, done in a single route. The invitation row already has `source`.
- Cycle prevention (an org cannot be its own ancestor) is a hook in #84. Not needed in 79a since `parent` is unused.

### #81 safeguarding credentials

- The tag mechanism and `_credentials` rule in 3.4 are the shape of the gate. #81 adds the `_credentials` collection, reminders and the dashboard; its rules are written the way 3.4 shows. Expiry is enforced on every request by `?> @now`, so "lapses automatically" holds with no cron.
- Roles and tags from 79a are what the coordinator uses. A "safeguarding coordinator" is a tag.
- The audit of "every grant and lapse" needs one thing 79a does not give: lapses are not writes, so nothing is audited when a credential expires. #81 needs a daily job that records `credential.lapsed` entries. That is #81's work, with no change to 79a.
- `@request.auth.hasCredential("...")` in #81's text is the same unavailable helper as `hasRole`. Say "rule snippet" there too.

### #85 church SSO presets

- Presets are OIDC clients (#71) bound to an org (the `org` field in 4.1), so each parish gets its own client and the `org_id` claim.
- SCIM for products that support it uses 79c as is, per org token. The per-group role mapping covers "volunteers in this group get the member role".
- Nothing in 79a blocks the one-way sync connector. It would use `_scimIdentities`-like rows with a different `source`.

### #80 households

Households overlap with orgs in one place: both are groups of users with roles. Keep them separate:

- A household is a small group with its own guardian semantics (minors without login, graduation). An org is an administrative unit with roles.
- Do not model a household as an org. It would put every family in the org list and the member-management UI, and `parent` would suggest a hierarchy that does not exist.
- They do share the rule technique: `@request.auth._householdMembers_via_user...` is the same pattern as 3.2.4. #80 can reuse the "Make X-owned" UI action with a `household` field.
- Overlap to decide once: where a minor with no email lives. Minors are users with no login, and invitations (79b) require an email. So a guardian-created minor is a household member and, if they must be in an org (youth group), is added by an org admin through the member route with `source=manual`. 79a's membership row does not require an email, only a user record, so no change is needed.

## 6. Milestones, files, risks

### Milestones

| | Scope | Size |
|---|---|---|
| 79a-1 | Migration for `_orgs`, `_orgMembers`, `_orgRoles` with fixed list/view rules and indexes; the last-owner and role-escalation guards; `/api/orgs` routes for orgs and members; audit `meta.orgId`; tests including the rule scenarios of section 3 | 6-8 days |
| 79a-2 | Admin UI: org list/detail, member editor, "Make org-owned" action and rule snippets, `APIKeyForbiddenCollections` additions, docs | 4-5 days |
| 79a-3 | OIDC `orgs` scope and org-bound clients; `org:<id>` API key scope; webhook docs | 3-4 days |
| 79b-1 | `_invitations`, routes, token handling, email templates, accept/register logic, rate limits, expiry cron, tests | 5-6 days |
| 79b-2 | Hosted member-management page for org admins and invitation list UI | 3-4 days |
| 79c-1 | `scim/` package: discovery endpoints, `_scimTokens`, `_scimIdentities`, Users CRUD, filter subset, PATCH, errors | 8-10 days |
| 79c-2 | Groups with role/tag mapping, pagination, deactivation and session revoke, token UI | 4-5 days |
| 79c-3 | Entra validator pass against a test tenant, Okta test app, quirks fixes, docs | 4-6 days |

79a alone meets "an API rule can gate a collection on org role". 79c-3 meets "Entra ID's SCIM validator passes".

### Upstream files likely touched

| File | Change | Merge risk |
|---|---|---|
| `core/base.go` | register the org guard hooks (one line) | low |
| `apis/base.go` | bind the `/api/orgs` and `/scim/v2` routes (a few lines) | low |
| `core/record_field_resolver*.go`, `tools/search/` | **none** (owner decision) | n/a |
| `core/collection_validate.go` | none | n/a |

Fork-owned files touched: `core/apikey.go` (forbidden list), `apis/apikey_auth.go` (org scope), `core/audit_model.go` (`meta.orgId`, redaction list), `core/webhooks.go` (aliases, later), `oidc/claims.go`, `oidc/oidc.go`. New fork-only: `core/org*.go`, `apis/org*.go`, `scim/`, `migrations/<ts>_add_orgs.go`, `ui/src/orgs/`, `docs/orgs.md`, `docs/scim.md`. Add them to the FORK_SYNC checklist with sync checks: `go test -run 'Org|Scim|Invitation' ./core/... ./apis/... ./scim/...`.

### Risks

- **Rule engine behavior changes upstream.** The whole design rests on same-row `_via_` joins (3.1) and on the `?` multi-match operators. Upstream is frozen at v0.41-ish and we sync by hand, so the risk is a sync, not surprise. Mitigation: commit the section 3 scenarios as `ApiScenario` tests so a sync that changes join semantics fails loudly.
- **Silent denials.** A wrong rule returns an empty list, not an error (and a plain `>` instead of `?>` denies everything). Mitigation: rule templates generated by the UI, not hand-typed, and the tests above.
- **Scan cost on big collections** (3.6). Mitigation: indexes, client filter on `org`, `skipTotal`, documented limits.
- **System collection rules are immutable.** Wrong list/view rules on `_orgMembers` need a new migration. Mitigation: keep the rule minimal (3.3) and test it before release.
- **Rule-less escalation paths.** Anything that writes `_orgMembers` outside the routes (a superuser script, an import) skips the guards. A model hook backs up the routes so the guards hold for any app-level write. Raw SQL (as the session code uses) would bypass them; do not use it here.
- **SCIM scope creep.** Entra and Okta differ in PATCH shape. Mitigation: build the validator and a recorded-request test suite first (79c-3 includes fixing the quirks).
- **Mixed identity authority.** SCIM could overwrite data users own. Mitigation: link-not-overwrite on create (4.8), org-scoped membership edits only.
- **Account takeover by email.** Linking SCIM users and invitation accepts by email trusts that the IdP/mailbox owns the address. Mitigation: invitation links prove the mailbox; SCIM links only existing accounts whose email is verified, otherwise it creates a membership that waits for the user to verify. See open question 6.
- **Prototype scope.** The prototype exercised HTTP rule evaluation on SQLite only. It did not run the same rules on Postgres (#106). The `_via_` and `@collection` joins are generated by the same resolver, but the Postgres dialect layer is new, so run section 3 on it once that layer lands.

## 7. Open questions for the owner

1. **Role model.** Options: (a) four fixed roles; (b) custom roles with permission strings; (c) fixed roles plus tags. Recommendation: (c). It covers church needs ("youth leader", "treasurer") and is the only richer model I could rule-test.
2. **Where do org members' writes happen?** Options: (a) collection create/update/delete rules on `_orgMembers`; (b) custom routes only. Recommendation: (b). Rules cannot protect the last owner or stop escalation, and system collection rules cannot be edited later.
3. **How do non-superuser org admins reach the member-management view?** Options: (a) API only, apps build their own UI; (b) a hosted page `/orgs/{slug}` served by Claviger; (c) extend the superuser admin UI with org-scoped logins. Recommendation: (b) in 79b-2, since congregations will not build a client. (c) is the most invasive.
4. **One user collection or several?** Options: (a) one configured collection (`orgs.userCollection`); (b) a membership collection per auth collection. Recommendation: (a). A deployment with two user types is rare; (b) doubles every rule.
5. **Do SCIM groups map to roles/tags or to orgs?** Options: (a) roles/tags, with an admin mapping; (b) child orgs (needs #84); (c) one group per org. Recommendation: (a) now, (b) when #84 lands.
6. **Account linking policy.** When invitation email or SCIM `userName` matches an existing account: (a) link only if that account's email is verified; (b) always link; (c) never link, always create a new membership request for the user to accept. Recommendation: (a). (b) risks taking over an account through an unverified address. (c) is safer but breaks SCIM's expectation that a create becomes active.
7. **Deactivation behavior.** On SCIM `active=false` or `DELETE`: (a) suspend/remove the membership only; (b) also ban the account when it has no other memberships. Recommendation: (a) by default, with a per-org setting for (b). Banning is account-wide and surprising for a person in two parishes.
8. **Org-bound API keys and OIDC clients now or later?** Options: ship in 79a-3; defer. Recommendation: ship OIDC org-bound clients with 79a (the issue asks for org claims); defer `org:<id>` API key scopes until someone needs them.
9. **Webhook event names.** Options: collection-based names only (`_orgMembers:create`); also the friendly aliases from the issue (`org.member.added`). Recommendation: collection names first, aliases in a later patch.
10. **Archive vs delete for orgs.** Options: archive by default with explicit "delete and its data"; hard delete only. Recommendation: archive by default (2.2).
11. **Should `hasRole` and `hasCredential` helpers be dropped from #79 and #81?** They need a resolver change, which you ruled out. Recommendation: yes. Replace with UI-generated rule snippets, and edit the issue text so nobody expects the function syntax.
12. **Default invitation expiry and rate limits.** Proposal: 7 days, 1 resend per minute, 20 invites per hour per org. Confirm or change.
13. **Google Workspace.** Confirm that Google is handled through inbound SSO (#92) and a directory sync connector, not SCIM, once the vendor documentation is re-checked.
