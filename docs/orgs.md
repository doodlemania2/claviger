# Organizations, roles and org-owned collections

Organizations (#79a) let one Claviger instance serve several groups (parishes, ministries, tenants), each with its own members and roles. With no organization created nothing changes: no extra checks run, and the new collections stay empty.

Design notes and the evidence behind the rules are in `docs/specs/79-orgs-roles-scim.md`. Invitations (79b) and SCIM (79c) are not part of this release.

## Model

Three hidden system collections:

| Collection | Holds |
|---|---|
| `_orgs` | `name`, `slug` (unique, `^[a-z0-9-]{2,64}$`), `parent` (optional, one level), `archived`, `settings` |
| `_orgMembers` | `org`, `user`, `role`, `status`, `tags`, `source`; unique on `(org, user)` |
| `_orgRoles` | free-form labels (`org`, `name`, `description`), the targets of `_orgMembers.tags` |

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
| `GET /api/orgs/rule-templates` | superuser | the presets below, for a relation field named `?field=` (default `org`) |

Guards, enforced for every caller and backed by model hooks for any app-level write:

- The last active owner cannot be removed, demoted or suspended, not even by a superuser. Make someone else an owner first, or archive and delete the org.
- Nobody grants a role above their own, and nobody edits or removes a member ranked above them. An admin cannot touch an owner.
- Nobody changes their own status. Members can lower their own role and can leave.
- Unknown or hidden orgs answer 404, so a non-member cannot tell an org exists.
- Superusers bypass the hierarchy; their changes are audited with the superuser as actor.

Note that the `user` lookup by email answers 404 for every miss, but an org admin can still probe which emails have an account. Treat org admins as trusted for that.

Writes through the records API (`/api/collections/_orgMembers/records`) are superuser-only (null rules) and hit the same model-hook guards, but skip the hierarchy checks. Prefer the routes.

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
