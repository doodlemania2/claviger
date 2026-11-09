# Spec: congregation features (#80 to #88)

Date: 2026-11-09. Status: spec only. No Go code in this repo changed.

This spec covers the nine issues labelled `religious-orgs`:

| Issue | Feature |
|---|---|
| #80 | Households and guardian-managed minors |
| #81 | Access gated on expiring safeguarding credentials |
| #82 | Children's check-in and pickup verification |
| #83 | Staff-vouched recovery and accessible login |
| #84 | Hierarchical organizations (diocese, parish, ministry) |
| #85 | Single sign-on presets for church software |
| #86 | Pastoral confidentiality levels and break-glass |
| #87 | Permanent records with annotation-only corrections |
| #88 | GDPR and children's data tools |

They build on the shipped org model (`docs/orgs.md`, #79), field encryption (`docs/field-encryption.md`, #75), the audit log (#74), webhooks (#77), passkeys and step-up, sessions and bans (#93), and the OIDC provider (`docs/oidc.md`, #71).

Owner decision carried over from #79: org-scoped access rules must not change upstream's rule resolver (`core/record_field_resolver.go`). Everything below uses existing filter syntax. Section 2 lists the rules I ran against a real test app.

Claviger is for congregations of any denomination. Names such as "diocese" and "parish" are examples. The model uses neutral words (organization, kind, level) and each deployment picks its own labels.

## Recommendation

1. Treat these as one program with one shared base: households (#80) and a data map (the #88 registry). Almost every other issue touches a child, a guardian, or a person's data. The base is small. Doing it late means reworking every collection.
2. Prefer **documented user-collection templates plus rule presets** for data that apps own (notes, registers, check-in logs). Use **system collections** only where integrity or security needs it: households, credentials, break-glass grants, recovery codes, consents, the closure table. This is the same split #79 used.
3. Rules can do more than expected. Section 2 shows that a closure table gives unlimited-depth hierarchy, expiring credentials lapse with no job, break-glass grants open and close by time, and household guardian scoping all work **without resolver changes**. Rules cannot do four things, and each needs a small Go piece: immutability (#87), last-guardian and merge guards (#80), reading audit (#86), and erasure of the audit log (#88).
4. Build order: data map registry and closure table first, then credentials (#81), then households (#80) with recovery (#83), then permanent records (#87) and confidentiality (#86), then GDPR tools (#88) in stages, and check-in (#82) last. #85 is mostly documentation and can run any time. Section 13 has the reasons.
5. Several parts are **not** the job of this repo. Label printers, classroom rosters, attendance reports and a check-in UI belong to an app built on Claviger. Claviger supplies the identity, rules, codes and audit that make those apps safe.

None of this is legal advice. Each section has a "Legal and safeguarding" part that lists what a lawyer must review before a congregation relies on it. Section 12 gathers the main ones.

## 1. Shared foundations

### 1.1 People are not users

A child may have no email and no login. A parishioner may be in the register and never sign in. So these issues need a notion of a **person** that is not an auth record.

Decision (open question 1): the household member row **is** the person record. It has an optional `user` relation. Adults who sign in have it set. Children and non-users do not. Other collections point at the member row (for example `child`) when the subject is a person, and at the user when the subject is an account holder.

### 1.2 Which things are system collections

| Thing | System or template | Why |
|---|---|---|
| `_households`, `_householdMembers` (#80) | System | Guards for merge, split, last guardian and graduation. Many features join through them. |
| `_credentialTypes`, `_credentials` (#81) | System | Security gate. Writes only by routes. |
| `_recoveryCodes` (#83) | System | Secrets with hashes, single use. |
| `_orgClosure` (#84) | System | Maintained by a hook. Must never be hand-edited. |
| `_breakGlass` (#86) | System | Grants access. Written by a route only. |
| `_consents`, `_consentEvents`, `_dataMap`, `_legalHolds`, `_retentionPolicies`, `_erasureLog`, `_dataRequests` (#88) | System | Compliance evidence and destructive jobs. |
| `_kiosks` (#82) | System | Device credentials. |
| Notes, prayer requests, registers, annotations, check-in sessions, pickup authorizations | Documented templates | App-shaped. The rules come from presets. |

System collection rules cannot be edited after creation (see `docs/orgs.md`). So each system collection gets minimal list and view rules, null write rules, and all writes go through routes plus model hooks. The routes enforce what rules cannot say.

### 1.3 Rule facts I relied on

From `docs/specs/79-orgs-roles-scim.md` and re-run here:

- Back-relations and `@collection` joins are multi-valued. Use `?=`, `?>`, `?<=`. A plain operator silently denies.
- Conditions that share a path prefix, or reuse an `@collection` alias, run on the **same row**. That is what makes "admin at this ancestor within two levels" mean one row.
- A denied create answers 400, a denied update or view or delete 404, and a denied list 200 with no items.
- `@now` is evaluated at query time. A date that passes closes access with no job.
- Encrypted fields cannot appear in rules. Rules gate on plain flags next to them.

Facts found while writing this spec (see section 2):

- A multi-valued path on the **right side** of a comparison does not work. `@collection.creds:c.org ?= org.orgClosure_via_descendant.ancestor` returned nothing. Write the same idea with a second `@collection` alias. That form works.
- Rules alone do not make a record immutable. `app.Save`, Go and JS hooks and superusers pass through them.

### 1.4 What "audited" means today

The audit log (`core/audit_model.go`) records **writes** to records (create, update, delete) plus a few admin actions with their own action names (for example `field_key_rotate`). It does **not** record reads. It is append-only for the records API, Go hooks and JS hooks (not raw SQL). Before and after values are stored with secrets redacted, and encrypted fields redacted by type.

Two consequences matter here. Break-glass (#86) needs read audit, which does not exist. Erasure (#88) must reach into audit entries, which are designed to be immutable. Both are covered in their sections.

## 2. Prototype results

Method: a throwaway test file in the session scratchpad (not committed, and removed from the worktree after the run). It used `tests.NewTestApp()`, the real `_orgs`, `_orgMembers` and `_orgRoles` collections, extra collections created in the test, and `app.CanAccessRecord(record, requestInfo, &rule)` to run each rule for each seeded user. 35 checks ran. 34 passed as written. One check showed a syntax that does not work, and the working form passed.

Limits of the prototype: it ran on SQLite, it checked single-record access (the same rule engine builds list queries, but I did not time lists), and create rules were run against a pre-inserted dummy record with the body supplied through the request info. Postgres (#106) was not run. Timings were not measured for the closure join; run the #79 benchmark on it before release (M1 in section 3.8).

### 2.1 Hierarchy by closure table (#84)

Orgs D (diocese), N (deanery), P (parish), Q (parish), Y (ministry under P). Closure rows for every ancestor and descendant pair, including self at depth 0. Users: ann (admin of D), ned (member of N), pat (admin of P), yan (member of Y), qed (admin of Q), gwen (member of D with tag `safeguarding`), mix (member of P, admin of D), zed (nobody). The depth of three levels is deeper than the `parent` field allows today, so the closure rows were inserted directly.

Let `cp = org.orgClosure_via_descendant` and `mp = cp.ancestor._orgMembers_via_org`. Base rule: `mp.user ?= @request.auth.id && mp.status ?= "active"`.

| Check | Result |
|---|---|
| Doc in Y, base rule | ann, ned, pat, yan, mix, gwen. Not qed (sibling branch), not zed. |
| Doc in Q, base rule | ann, ned, qed, mix, gwen. Not pat or yan. |
| Doc in D, base rule | Only ann, mix, gwen. Lower levels do **not** see upward. |
| Base `&& mp.role ?= "admin" && cp.depth ?<= 2`, doc in Y | **pat only.** ann is admin at depth 3 and is denied. mix is member at depth 1 and admin at depth 3, and is denied, so role and depth are read from the same row. |
| Base `&& mp.role ?= "admin"`, doc in Y | ann, pat, mix. |
| Base `&& mp.tags.name ?= "safeguarding"`, doc in Y | gwen only. |
| Existing one-hop `OrgParentRule` form on a doc in Y | Only yan. A fixed hop count cannot reach D. |

Conclusion: the closure table gives any depth, role-limited, depth-limited and tag-limited oversight, with rules only.

### 2.2 Credential gate (#81)

A `creds` collection with `user`, `org`, `type`, `expires`, `revoked`.

| Check | Result |
|---|---|
| Member rule plus a valid `safe_environment` credential | yan only. pat has an expired one, ned a revoked one, qed a different type. |
| Same, with a credential issued by the same org as the record | No one (none was issued by Y). |
| Credential issued by an ancestor counts, using the **right-side path** form | **Fails.** Returns no one. |
| Same intent using a second alias: `@collection.orgClosure:k.descendant ?= org && @collection.orgClosure:k.ancestor ?= @collection.creds:c.org` | **Works.** yan only. |
| Same, with `@collection.orgClosure:k.depth ?<= 1` | No one (the diocese is three levels up). |
| Move yan's expiry into the past, then evaluate again | Access gone with no job run. |

### 2.3 Households and guardians (#80)

Collections: `households`, `people` (household, optional user, `graduated`), `hmembers` (household, user, role, `guardian`).

| Check | Result |
|---|---|
| View: `user = me \|\| household.hmembers_via_household.user ?= me` | Household members (pat, ned) see the child. The graduated person's own login (yan) sees themself. Other households see nothing. |
| Update: guardian flag set and `graduated = false` | pat for kid1. Nobody for the graduated person. qed for the child of the other household. |
| Delete the head's membership row | pat loses write access on the next evaluation. |
| Check-in create, body names a household where the caller is a guardian | pat for household 1, qed for household 2. |

### 2.4 Visibility levels and break-glass (#86)

A `notes` collection (org, author, visibility, sealed) and a `breakGlass` collection (note, grantee, expires, revoked). Members: lay (author), fr (tag `clergy`), cathy (tag `care-team`), boss (**owner** of the org), others.

| Check | Result |
|---|---|
| Private note | Author only. The org owner and clergy cannot read it. |
| Clergy note | Author and fr. |
| Care note | Author and cathy. |
| Congregation note | All members of the org. |
| Sealed clergy note (`sealed = true`) | Author only. The clergy tag does not help. |
| Break-glass grant for boss on the sealed note, expiry in the future | boss can read it. |
| Same grant, other note | boss gets nothing. Grants are per note. |
| Grant expiry set in the past | Access closes by itself. |
| Grant `revoked = true` | Access closes. |

### 2.5 Permanent records (#87)

A `register` with `finalizedAt`, and `annotations`.

| Check | Result |
|---|---|
| Update rule `finalizedAt = "" && member rule` | Open entry: all members. Finalized entry: nobody through the API. |
| `app.Save` on the finalized entry | **Succeeded.** Rules do not bind hooks, Go code or superusers. A core model hook is required. |
| Annotation create rule: caller is a member of the entry's org and the entry has `finalizedAt != ""` (via `@collection.register`) | Allowed on the finalized entry, denied on the open one. |
| Annotation update and delete with a null rule | Denied for every non-superuser. |

### 2.6 What was not prototyped

Everything that is a route or a job: recovery codes, kiosk sessions, label rendering, export, erasure, retention, SSO vendor behavior. These are designed from the code I read (`apis/record_auth_with_webauthn_stepup.go`, `core/record_tokens.go`, `core/ban.go`, `apis/sessions.go`, `core/audit_model.go`) and are marked "not run" where it matters.

## 3. #84 Hierarchical organizations

### 3.1 Problem and who it serves

People serve in several parishes and ministries. A diocese, synod or conference needs oversight (for example "which parishes have volunteers with lapsed safeguarding training") without holding an account in each parish. Today `_orgs.parent` allows one level, and the rule cookbook can reach exactly one hop.

Serves: diocesan administrators and safeguarding officers (read oversight), parish staff (their own org only), volunteers who belong to several orgs.

### 3.2 Goals and non-goals

Goals:

- Any depth up to a configured limit (default 4, hard cap 8).
- One person, one login, memberships in many orgs (already true).
- Oversight rules by role, tag and depth, written with existing syntax.
- Consent-based transfer of a membership between orgs.
- Org kind labels per deployment ("diocese", "parish", "ministry", or anything).

Non-goals:

- No automatic inheritance of membership. Being admin of a diocese does not make you a member of its parishes.
- No upward visibility. A parish member never sees the diocese's records (verified).
- No moving of records between orgs on transfer. Pastoral records stay where they were written.
- No cross-denomination federation or trust between separate deployments.

### 3.3 Options compared

| | A. Fixed hops in rules | B. Denormalized `ancestors` on every record | C. `_orgClosure` table (recommended) |
|---|---|---|---|
| Rule shape | `org.parent.parent._orgMembers_via_org.user` per hop | `ancestors._orgMembers_via_org.user ?= me` | `org.orgClosure_via_descendant.ancestor._orgMembers_via_org.user ?= me` |
| Depth | Fixed per rule, rules grow | Any | Any |
| Re-parent cost | None | Rewrite every record in the subtree | Rewrite closure rows of the subtree only |
| Writes on each record | None | A hook on every org-owned collection | None |
| Verified | One hop exists in the shipped cookbook. More hops not run. | Not run | **Run** (section 2.1) |
| Risk | Rule explosion, silent denial | Drift between record and org tree | Drift between table and tree (guarded by a hook and a repair command) |

Recommendation: C. B is rejected because it puts a hook on every user collection and a re-parent becomes a bulk rewrite of business data.

### 3.4 Data model

System collection `_orgClosure`:

| Field | Type | Notes |
|---|---|---|
| `ancestor` | relation to `_orgs`, required, cascade delete | |
| `descendant` | relation to `_orgs`, required, cascade delete | |
| `depth` | number, required | 0 for the self row |

Indexes: unique on `(ancestor, descendant)`; on `descendant`; on `(ancestor, depth)`.

`_orgs` gains `kind` (free text, optional, for display) and the existing `parent` stays the source of truth. The closure table is derived. Changes to `_orgs`:

- Remove the "a parent cannot have a parent" check in `validateOrg`. Replace it with: no cycles, depth at most `orgs.maxDepth`.
- A model hook on `_orgs` create, update and delete rebuilds the closure rows for the affected subtree in the same transaction. Create adds the self row and one row per ancestor. A re-parent deletes the rows from the old ancestors to every node in the subtree and inserts the new ones.
- `claviger orgs closure-repair` rebuilds the whole table from `parent` and reports differences. Run it after a restore.
- The table is on the API-key forbidden list. Its list and view rules: members of the descendant org may read the rows where they are a member of `descendant` or of `ancestor`. Write rules null.

Existing deployments: the migration fills the table from the current one-level `parent`.

### 3.5 Rules (existing syntax only)

With `cp = org.orgClosure_via_descendant` and `mp = cp.ancestor._orgMembers_via_org`:

| Goal | Rule | Run |
|---|---|---|
| Members of the org or any ancestor read | `org.archived = false && mp.user ?= @request.auth.id && mp.status ?= "active"` | yes |
| Admins of an ancestor within N levels | add `&& mp.role ?= "admin" && cp.depth ?<= N` | yes |
| Tag at any ancestor (for example `safeguarding`) | add `&& mp.tags.name ?= "safeguarding"` | yes |
| Credential issued by an ancestor | use the `orgClosure:k` alias form from section 2.2 | yes |
| Org-only (no oversight) | the existing `OrgMemberRule` | yes (shipped) |

Add Go helpers `core.OrgAncestorRule(field, opts)` that return these strings, as `OrgParentRule` does today. The admin UI generates snippets the same way.

Oversight must be opt-in per collection. The default presets stay org-only. Pastoral data (#86) never gets an ancestor preset.

### 3.6 Transfer with consent

Route family `POST /api/orgs/{id}/transfers`:

1. An admin of the source org (or the person) starts a transfer to a target org. A row in `_orgTransfers` (system, small) records person, from, to, state, and `expiresAt`.
2. The person must accept while signed in. An admin cannot complete it alone. A minor's transfer is accepted by a guardian.
3. On accept, the route creates the target membership, suspends or removes the source membership (choice recorded), and writes one audit entry with both orgs in `meta`.
4. Records the person wrote stay in the source org. The route lists them for the admins.

### 3.7 Integration

- **Orgs and SCIM:** SCIM groups can map to child orgs once this exists (resolves #79 open question 5b). Not in the first milestone.
- **OIDC:** an optional `org_path` claim for a bound client (ids from root to the bound org) is useful for apps that show breadcrumbs. Low priority.
- **Audit and webhooks:** closure rebuilds write one audit entry (action `org_reparent`, counts) and not one per row. `_orgClosure` itself is excluded from audit like other derived tables.
- **Safeguarding view for dioceses:** a route returning counts per child org (people with a current credential, expiring, lapsed) without any person detail. Dioceses see status, not content.

### 3.8 Risks and legal notes

- A diocese seeing parish data may need consent or an agreement. Under GDPR a church body disclosing member data to a higher body is a disclosure to another controller in many setups. Flag for a lawyer (section 12).
- Congregational traditions (independent churches) do not want a parent at all. The feature stays off until a parent is set.
- Drift between tree and table. Mitigation: the hook is in the same transaction, and `closure-repair` plus a startup check that logs a difference.
- Performance of a three-table join. **Not measured.** Re-run the #79 benchmark (500 orgs, 20k memberships, 200k records) with the closure join before release. If slow, an index on `(descendant)` and a client `filter=org="..."` are the first tools.

### 3.9 Milestones

| # | Work | Size |
|---|---|---|
| 84-1 | `_orgClosure`, hook, migration, lifted depth check, repair command, rule helpers and tests | M |
| 84-2 | Benchmark on the closure join; Postgres run of the rule tests | S |
| 84-3 | Admin UI: tree view, kind labels, depth setting, snippets | M |
| 84-4 | Transfers with consent | M |
| 84-5 | Diocese status route (counts only) | S |

Sizes: S up to 3 days, M up to 2 weeks, L up to 4 weeks, XL more than 4 weeks, for one developer.

## 4. #80 Households

### 4.1 Problem and who it serves

Congregations think in families. Every church app rebuilds households and guardians, usually badly. Claviger only has individual auth records.

Serves: families (parents managing children), parish staff (merging duplicates, splitting households), children's ministry (who may pick up whom), and every later feature in this spec.

### 4.2 Goals and non-goals

Goals:

- A household is a group of people with roles (head, spouse, adult, child, other) and shared contact data.
- Guardians manage minors' records. Minors may have no email and no login.
- Graduation: at a configured age a minor becomes independent and may leave with their history.
- Rule presets that scope data to a household and to guardians.
- Admin tools to merge duplicates and split households.

Non-goals:

- No `@request.auth.household` or `isGuardianOf()` helper. They need a resolver change. Rule snippets replace them (same decision as #79's `hasRole`).
- No family-tree or genealogy model.
- No legal determination of custody. A guardian flag is a church record, not a court ruling.
- No automatic detection of duplicates beyond a candidate list.

### 4.3 Data model

`_households`: `name`, `org` (required, the home org), `address` (encryptedJSON), `homePhone` (encryptedText), `status` (`active`, `archived`), `created`, `updated`.

`_householdMembers`:

| Field | Type | Notes |
|---|---|---|
| `household` | relation, required, cascade delete | |
| `user` | relation to the auth collection, optional | set for people who sign in |
| `name` | text, required | |
| `role` | select `head`, `spouse`, `adult`, `child`, `other` | |
| `guardian` | bool | may manage minors in this household |
| `isMinor` | bool | maintained by a daily job from `birthDate` and the org's age setting |
| `birthDate` | encryptedText | rules cannot read it, so the job derives `isMinor` |
| `photo` | file, optional | protected file |
| `allergies`, `medical` | encryptedJSON | used by #82 |
| `status` | select `active`, `left`, `deceased` | `left` keeps history after a split |
| `source` | select `manual`, `import`, `scim` | |

Indexes: on `household`; unique on `(household, user)` where `user != ""`; on `user`.

Rule `M` for "I am an active guardian in this person's household":
`household._householdMembers_via_household.user ?= @request.auth.id && household._householdMembers_via_household.guardian ?= true && household._householdMembers_via_household.status ?= "active"`.

Other collections that describe a person carry a relation `member` to `_householdMembers` and a **copy** of `household` (indexed). The copy keeps rules to one hop, which is the shape I ran. A hook keeps the copy in step. The `child.household._householdMembers_via_household` two-hop form should also work but was not run; use the copy.

Templates: `people_notes`, `enrolments` and similar app collections use `member` + `household`, and the guardian preset.

### 4.4 Rules

| Case | Rule | Run |
|---|---|---|
| Household members read a person | `household._householdMembers_via_household.user ?= @request.auth.id` | yes |
| A person with a login reads themself | `user = @request.auth.id \|\| <above>` | yes |
| Guardians write, not after graduation | `M && isMinor = true` | yes (with a `graduated` flag) |
| Staff read members of their org | `OrgMemberRule("household.org")` style, with a tag such as `registrar` | pattern shipped |
| Access ends when a guardian is removed | follows from the rule | yes |

Writes to `_households` and `_householdMembers` go through routes: `/api/households/...`. Rules there are list and view only (members of the household and org staff with the `registrar` tag).

### 4.5 Guards (Go, in routes and model hooks)

- A household must keep at least one active adult with `guardian = true` while it has an active minor. Refuse the removal or demotion that breaks this.
- A minor cannot be a guardian. A guardian must have `isMinor = false`.
- **Merge:** staff select two households. The route moves members, rewrites the `household` copy on dependent records in one transaction, marks the source `archived`, and writes one audit entry listing the ids moved. It is reversible by the audit entry (a `split` using the recorded ids).
- **Split:** staff or a head moves selected members to a new household. Copies update the same way.
- **Graduation:** the daily job sets `isMinor = false` at the configured age (default 18; setting per org). The person can then be given a `user` (invitation, #79b) and can leave the household with their history. Their own records keep their `member` id, so history follows them.
- Minors with `user` set are allowed only when the minor is over the provider's age of digital consent (see 4.7). Default is no login for minors.

### 4.6 Integration

- **Orgs/tags:** a household belongs to one home org. Staff access comes from `_orgMembers` with a tag, not from the household.
- **Field encryption:** `birthDate`, `address`, `homePhone`, `allergies`, `medical`. Rules use `isMinor`, `status` and `guardian`.
- **Audit:** membership changes are normal record writes, so they audit with `meta.orgId` and `changedFields`. Merge and split add `meta.mergedFrom`.
- **Invitations (#79b):** guardian invites adults to the household. Invites to minors are refused (79b non-goal).
- **OIDC:** optional claim `household` (id) and `guardian_of` (member ids) for apps that need them. Off by default; minors' ids in a token to a third party need care (see section 12).
- **Sessions:** removing a guardian does not need session revocation. Rules read live.

### 4.7 Safeguarding and legal considerations

- Children's data: names, birthdates, photos, allergies, medical notes and household addresses. Encrypt what rules do not need. Never put these in OIDC claims or webhook payloads by default (webhooks are built from audit entries, which redact encrypted fields by type; keep it that way).
- Age rules differ. US COPPA: under 13 and online services. GDPR Article 8: consent age between 13 and 16 by member state. UK: 13. Make the minimum age a deployment setting with a safe default (no minor logins). A lawyer decides the default per country.
- Shared household data holds other people's data (the spouse's phone). Each member row is its own data subject. #88 export must show a person only their own row and the shared contact data, not the other members' details.
- Divorce and custody: a guardian flag can be wrong or contested. Staff need a way to remove a guardian quickly, and #82 needs a "do not release" flag (see 7.5). Courts decide custody, not the app.

### 4.8 Risks

- Duplicate people from imports. Mitigation: candidate list by name plus household and birth year; merge is manual.
- The `household` copy drifts. Mitigation: the hook that sets it, plus a repair command.
- Over-sharing in rules. Mitigation: the UI generates rules, and the templates are tested.

### 4.9 Milestones

| # | Work | Size |
|---|---|---|
| 80-1 | `_households`, `_householdMembers`, migration, model hooks and guards, routes | L |
| 80-2 | Rule presets and Go helpers; tests including removal and graduation | M |
| 80-3 | Graduation job and age settings | S |
| 80-4 | Merge and split routes with audit | M |
| 80-5 | Admin UI (household list, member editor, merge/split) | L |
| 80-6 | Hosted guardian page: see and edit my household | M |

## 5. #81 Safeguarding credentials gate

### 5.1 Problem and who it serves

Most denominations require volunteers who work with children to hold a current background check and safeguarding training (VIRTUS, Safe Environment, Ministry Safe, and similar). Today it is a spreadsheet, and access is not tied to it.

Serves: safeguarding coordinators and parish staff, volunteers, diocesan safeguarding officers.

### 5.2 Goals and non-goals

Goals:

- Credentials with a type, issue date, expiry, issuer, verified-by and optional document.
- Rules that require a current credential. Access lapses at expiry with no job (verified).
- Warnings before expiry to the person and the coordinator.
- A coordinator dashboard: who expires soon, who lapsed, and the reason.
- Grants and lapses are audited.

Non-goals:

- No `@request.auth.hasCredential()` helper (resolver). Use rule snippets.
- Claviger does not run background checks and does not store their results. It stores the **attestation**: type, dates, who verified it.
- No integration with a specific vendor in v1.

### 5.3 Data model

`_credentialTypes`: `org` (the org that defines it; ancestors' types apply to descendants), `key` (for example `safe_environment`), `name`, `validMonths`, `requiresDocument` (bool), `requiresVerifier` (bool), `status`.

`_credentials`:

| Field | Type | Notes |
|---|---|---|
| `user` | relation to the auth collection, required, cascade delete | the holder |
| `org` | relation to `_orgs`, required | the org that issued or recognizes it |
| `type` | text, required | the type `key` |
| `issuedAt`, `expires` | date | `expires` drives the rules |
| `issuer` | text | training provider or agency |
| `verifiedBy` | relation to the auth collection | the coordinator who checked it |
| `document` | file, optional, protected | for example a certificate |
| `revoked` | bool | immediate withdrawal |
| `revokedReason` | encryptedText | |
| `notifiedAt` | date | last reminder |

Indexes: `(user, type)`; on `expires`; on `org`.

Writes go through `/api/orgs/{id}/credentials` routes, which require the `safeguarding` tag (or admin) and refuse a coordinator verifying their own credential (a second person must). List and view rules: the holder sees their own; coordinators see their org and, through the closure table, the descendants.

### 5.4 Rules

For a collection that holds children's data, add to the member rule:

```
<member rule> && @collection._credentials:c.user ?= @request.auth.id
  && @collection._credentials:c.type ?= "safe_environment"
  && @collection._credentials:c.expires ?> @now
  && @collection._credentials:c.revoked ?= false
```

Credential recognized from an ancestor org (diocese-issued):
`... && @collection._orgClosure:k.descendant ?= org && @collection._orgClosure:k.ancestor ?= @collection._credentials:c.org`.

Both forms were run (section 2.2). Do **not** write the ancestor test as a path on the right side of `?=`. It returns nothing.

Combine with a tag (`tags.name ?= "youth"`) and #84 depth limits as needed. The UI generates the strings.

Lapse is immediate at query time. Realtime subscribers stop receiving matching events, but data already fetched stays in the client. Apps should refetch after a 404 or empty list.

### 5.5 Reminders, dashboard, audit

- A daily job finds credentials expiring in 60, 30 and 7 days (setting), emails the holder and the org's coordinators, and sets `notifiedAt`. Use the app mailer (`app.NewMailClient()`).
- A lapse is a passage of time, not a record change, so the audit log would not see it. The same job writes one audit entry per newly lapsed credential (action `credential_lapsed`, with `meta.orgId`, type and holder). It also sets a plain `status` for the dashboard. The rules still read `expires`, so a late job never leaves access open.
- `GET /api/orgs/{id}/credentials/summary`: counts and lists for valid, expiring and lapsed, and which collections each lapsed person lost.
- `GET /api/me/access-status`: tells a volunteer why a list is empty ("your Safe Environment credential expired on ..."). Silent empty lists confuse people.
- Webhook events follow the collection names (`_credentials:create`, `:update`) plus the `credential_lapsed` audit entry.

### 5.6 Legal and safeguarding

- Background-check results are regulated (for example FCRA in the US, DBS in the UK). Storing only a yes/no attestation and a date reduces the burden. A lawyer decides what else may be kept and for how long.
- The holder has data rights (#88). Credentials are personal data; retention follows the policy table.
- A lapse that removes access must not become a public shaming. The dashboard is for coordinators only.
- Some regions require a check on the person **and** a barred-list check. That is the congregation's process, not Claviger's.

### 5.7 Risks

- Empty lists look like bugs. Mitigation: the access-status route and UI text.
- A coordinator can approve a friend. Mitigation: a second verifier rule, audit, and the diocesan dashboard.
- Rules grow long. Mitigation: generated snippets and tests.

### 5.8 Milestones

| # | Work | Size |
|---|---|---|
| 81-1 | Collections, routes, rule helpers, tests (valid, expired, revoked, wrong type, ancestor-issued) | M |
| 81-2 | Reminder and lapse job, audit entries | S |
| 81-3 | Coordinator dashboard and access-status route | M |
| 81-4 | Admin UI and hosted page for volunteers | M |

## 6. #83 Staff-vouched recovery and accessible login

### 6.1 Problem and who it serves

Older members are where password resets fail. The parish office knows them by sight. Let staff vouch.

Serves: older and less technical members, people with no email, parish office staff.

### 6.2 Goals and non-goals

Goals:

- Staff verify a person in person and issue a short-lived, single-use code (or QR) that enrolls a passkey.
- Passkey enrollment link by SMS for people without email, only with a staff member present in the flow.
- Accessible login pages: large text, high contrast, plain language, keyboard and screen reader use.
- Localized login and email templates, starting with Spanish.

Non-goals:

- No self-service recovery by SMS alone. SIM swaps and phishing make that unsafe.
- Staff never see or set a password. They issue a code and the person enrolls a passkey.
- No knowledge-based questions.

### 6.3 Design

**Code.** `_recoveryCodes` (system): `user`, `org`, `issuedBy`, `channel` (`display`, `qr`, `sms`), `codeHash`, `expires`, `usedAt`, `verification` (select: `known_by_sight`, `id_checked`, `other`), `note`.

- Code: 10 characters from a 32-letter alphabet (50 bits) in groups of five, or a 128-bit token inside a QR. Hash with SHA-256. Show once.
- Lifetime: 15 minutes (setting 5 to 60). Single use. At most 3 wrong guesses per code, then the code dies. Redemption is rate limited per IP.
- Issue route `POST /api/orgs/{id}/recovery`: caller needs the `greeter` tag (or admin), **a fresh passkey step-up** (`passkey_step_up_required`, 5 minutes), and a target that is a member of the org. The route refuses: self-issue; targets with `owner`, `admin` or `superuser` standing unless a second staff member confirms; more than N issues per staff member per hour.
- Redeem route: `POST /api/recovery/redeem {code}` returns the same short-lived passkey reset token that `NewPasskeyResetToken()` produces today (`core/record_tokens.go`). The existing passkey reset confirm route then enrolls the passkey and rotates the token key, which revokes old sessions. No new enrollment path is needed. (Read in code, not run.)
- The person gets an email or SMS notice on any other channel they have: "staff issued a recovery code at 10:14". If it was not them, they can report it.
- Banned accounts get no code and no hint why (same rule as `silentlyRefuseBanned`).
- Every issue and redeem is an audit entry with the staff member as actor, `meta.orgId`, `verification`, and the channel.

**SMS.** Claviger has no SMS sender. Add a small provider interface (Twilio and a generic webhook first) as a separate piece. The SMS carries a link to the hosted enrollment page. The link alone is not enough: it needs the code the staff member read out or displayed (so a stolen phone or forwarded text fails). SMS is for delivery of the link to the person's own phone while they are at the office.

**Accessible login.** The hosted pages (`apis/passkeys_page.go`, `apis/orgs_pages`) get:

- A "Larger text and high contrast" toggle, saved in `localStorage` (wrapped in try and catch) and respecting `prefers-contrast` and `prefers-reduced-motion`.
- WCAG 2.2 AA as the target: focus visible, 24px minimum targets (44px in large mode), no timeouts shorter than needed (warn and allow extension), labels tied to inputs, error text next to the field, and no CAPTCHA that blocks assistive tech. Passkeys satisfy the "accessible authentication" success criterion.
- Plain-language copy at about a grade 6 reading level, reviewed by an older user.
- No inline script or style stays as is (strict CSP).

**Localization.** Add a `locale` field on the member collection (optional) and per-org default locale. Mail templates and hosted-page strings come from a message table (`en`, `es`). Fallback to `en`. Template overrides per org already exist for invitations; reuse that shape. Not run.

### 6.4 Safeguarding and legal

- A staff member who can issue a code can take over an account. The controls are: tag-gated, step-up, no self-issue, second approval for elevated roles, rate limit, notification, audit. The board should still review who holds the `greeter` tag.
- SMS consent and carrier rules (opt-in, STOP handling, 10DLC in the US) apply. Use transactional messages only. A lawyer reviews wording and consent capture.
- The recovery note must not hold sensitive observations about the person.

### 6.5 Risks

- Social engineering of staff ("I am the pastor, please issue a code to my phone"). Mitigation: in-person only, notification to the person, second approval for elevated roles.
- Older users and passkeys: device confusion. Mitigation: a printed one-page guide and a staff checklist in the docs.

### 6.6 Milestones

| # | Work | Size |
|---|---|---|
| 83-1 | `_recoveryCodes`, issue and redeem routes using the existing reset token, audit, limits | M |
| 83-2 | Hosted staff page and enrollment page | M |
| 83-3 | Accessible mode, WCAG review, copy review | M |
| 83-4 | Locale field, message table, Spanish | M |
| 83-5 | SMS provider interface and link delivery | M |

## 7. #82 Children's check-in and pickup

### 7.1 Problem and who it serves

Check-in and pickup is the most anxious identity moment in church life. The question is "may this person take this child?"

Serves: children's ministry staff, parents and guardians, safeguarding coordinators.

### 7.2 What belongs where

| Piece | In Claviger | In an app on top | Why |
|---|---|---|---|
| Who may check a child in, and who may pick up | Yes (rules and household data) | | Identity and authorization. |
| Check-in session record and log | Template collections plus routes | The UI | The log must be tamper-evident. |
| Pickup code generation and verification | Yes (routes) | | Security. Brute force limits live here. |
| Guardian passkey verification | Yes (existing step-up) | | Already built. |
| Authorized-pickup list and photo | Yes (template) | UI to edit | Data and rules. |
| Do-not-release flags | Yes | | Safety. |
| Alerts on mismatch | Webhook event and email | Display on staff screen | Claviger raises the signal. |
| Kiosk device identity and short sessions | Yes (`_kiosks`) | | Credentials. |
| Kiosk UI, large buttons, idle screens | | Yes | Product design. |
| Label printing (Zebra, Brother, paper) | | Yes | Hardware and templates. Claviger returns a label payload. |
| Rooms, age groups, ratios, attendance reports | | Yes | Ministry policy. |
| Allergy display on a nursery screen | Data and rule | Layout | |

### 7.3 Goals and non-goals

Goals:

- Check-in session per event. A guardian checks a child in. The result is a pickup code (printed label and QR).
- Authorized-pickup list per child: household guardians, plus approved others with a photo.
- Pickup by code match, QR scan or guardian passkey.
- Alerts on mismatch and a complete log.
- Kiosk mode: device-bound, short sessions on shared tablets, automatic sign-out.

Non-goals:

- No label printer drivers, no classroom roster or ratio engine, no attendance dashboards, no billing.
- No facial recognition.
- Claviger does not decide custody disputes.

### 7.4 Data model

Templates (user collections, created by `claviger templates install checkin`):

- `checkin_events`: `org`, `name`, `starts`, `ends`, `status`. Admin write, org members read.
- `checkin_sessions`: `event`, `org`, `household`, `child` (relation to `_householdMembers`), `checkedInBy` (member), `checkedInAt`, `codeHash`, `state` (`in`, `out`, `void`), `releasedTo` (member or free text), `releasedBy` (staff user), `releasedAt`, `method` (`code`, `qr`, `passkey`, `override`), `flags` (json, plain, such as `["allergy"]`).
- `pickup_authorizations`: `household`, `child`, `name`, `relationship`, `photo`, `validUntil`, `approvedBy` (guardian), `status`.
- `checkin_log`: append-only rows (see 7.6).

System:

- `_kiosks`: `org`, `name`, `deviceTokenHash`, `registeredBy`, `lastSeen`, `status`, `allowedEvents`.

Allergy and medical data stay on `_householdMembers` as encryptedJSON. A session holds a plain `flags` array (only "has an alert"), so rules and labels never show details. Details appear on a staff screen through the records API only for users allowed by a tag such as `nursery`.

### 7.5 Verification flow

1. **Check in.** A guardian (signed in on their phone, or identified at a kiosk) selects children in their household. Create rule for `checkin_sessions` (run, section 2.3): the caller is an active guardian of the named household.
2. The route `POST /api/checkin/{event}/in` creates the sessions, generates for each child a pickup secret (see below), returns label data, and writes a `checkin_log` row.
3. **Pickup.** Staff scan the QR or enter the code. `POST /api/checkin/sessions/{id}/release` takes `{code}` or `{guardianAssertion}` and compares in constant time. A matching code releases the child. A mismatch increments a counter (5 tries, then lock and alert) and returns a generic failure.
4. **Guardian passkey.** An authorized guardian on their phone does the step-up ceremony (`.../step-up-begin`, `.../step-up-finish`, 5 minute freshness) and calls release with the new token. The route checks the signed-in user is an active guardian of the child's household or an authorized pickup person with an account.
5. **Override.** A staff member with the `checkin-supervisor` tag releases against a mismatch with a typed reason. The log entry is flagged, the guardians get a message, and the coordinator sees it.
6. **Do-not-release.** A flag on the child (set by a coordinator, reason in an encryptedText) makes release fail for the named people and alerts staff. Rule-free: this is a route check.

Codes:

- Printed code: 4 to 6 characters, unambiguous alphabet, plus a long random token in the QR. Both map to one session.
- Stored as HMAC-SHA256 with a server key (not a plain hash), so a short code cannot be brute forced offline from a database leak.
- Valid only for that session and event. Dead after release or event end.
- A parent code is per family per event, the child's label carries the same code, so matching is visible to a person (the label and the parent slip show the same short code). That is the usual practice in children's ministry and it does not replace the checks above.

### 7.6 Log

`checkin_log` is append-only: create allowed to the route only, update and delete null (run: null rules deny every non-superuser, section 2.5). A model hook refuses update and delete for any caller, like `_auditLog`. Each check-in and check-out is also a normal audit entry.

### 7.7 Kiosk mode

- A `_kiosks` row represents a device. Registration is by an org admin with step-up, producing a device token shown once.
- A kiosk token is a restricted principal, not a user: it may call only `/api/checkin/...` routes for its org and events. It cannot read the records API. This follows the reasoning in #79c for SCIM tokens (an API key acts as a user, so it is the wrong tool).
- Kiosk sessions for a family are short: a guardian identifies themselves at the kiosk (passkey on the phone through a QR handoff, or code), the kiosk session lasts 5 minutes (setting), and the kiosk clears its screen after each family and after 60 seconds of inactivity. Idle sign-out is enforced both in the client and by the server TTL.
- Revoking a kiosk (lost tablet) is one click. It works at once because the token is checked on each request.

### 7.8 Safeguarding and legal

- Check-in data is children's data. Keep the retention short (for example delete sessions 12 months after the event unless an incident is flagged). Policy table in #88.
- Photos of children and of authorized others are personal data; consent needed (consent records in #88).
- Medical and allergy information is special-category (health). Stay encrypted. Labels show an alert marker, not details.
- Incident records (a child released wrongly) may be legal evidence. Provide a legal hold on the session and its log (#88).
- A congregation's insurer may require specific procedures. This feature supports a procedure; it does not replace one.

### 7.9 Risks

- Brute-forcing short codes: scoped to a session, HMAC, five tries, alert.
- Shoulder surfing of labels. Mitigation: the code alone is not enough when staff also check that the person matches the pickup list.
- Kiosk theft: revoke the token; the token cannot read data.
- Overreach: building a whole children's-ministry product. Mitigation: the table in 7.2 is the boundary.

### 7.10 Milestones

| # | Work | Size |
|---|---|---|
| 82-1 | Template collections and rule presets; log guard hook | M |
| 82-2 | Check-in and release routes, codes, limits, alerts | L |
| 82-3 | Guardian passkey verification path | M |
| 82-4 | `_kiosks`, kiosk principal, short sessions | L |
| 82-5 | Reference kiosk and staff UI as an example app (not core) | L |
| 82-6 | Do-not-release flags and incident hold | M |

## 8. #86 Pastoral confidentiality and break-glass

### 8.1 Problem and who it serves

Prayer requests and pastoral care notes are the most sensitive data a congregation holds. They need finer visibility than a collection rule, and access to a sealed note must leave a trace.

Serves: clergy and pastoral care teams, people who share prayer requests, and the congregation's trust.

### 8.2 Goals and non-goals

Goals:

- Visibility levels: private to author, clergy only, care team, whole congregation.
- Sealed notes: readable only by the named owner.
- Break-glass: time-boxed emergency access with a mandatory reason, owner notification and audit.
- Prayer requests that expire or archive automatically.

Non-goals:

- **Sacramental confession (the seal) must not be stored in Claviger.** The docs and the template must say so. It is outside the product.
- No search inside sealed notes.
- No end-to-end encryption in the first milestone (see 8.5).
- No replacement for mandatory-reporting duties. Break-glass supports them; it does not decide them.

### 8.3 Visibility levels (rules, run)

Template `pastoral_notes`: `org`, `author`, `subject` (optional member), `visibility` (`private`, `clergy`, `care`, `congregation`), `sealed` (bool), `body` (encryptedText), `expiresAt` (date, optional), `archived` (bool).

The rule is built from pieces and was run in full (section 2.4):

```
author = @request.auth.id
|| (sealed = false && archived = false && (
     (visibility = "congregation" && M)
  || (visibility = "care"   && M && org._orgMembers_via_org.tags.name ?= "care-team")
  || (visibility = "clergy" && M && org._orgMembers_via_org.tags.name ?= "clergy")))
|| <break-glass clause>
```

where `M` is the shipped org member rule. Results: private and sealed notes are author-only, clergy and care levels follow the tags, congregation level is any active member, and **an org owner or admin has no extra right**. The default org presets must not be applied to this collection. The UI template creates the rule above and refuses to apply a looser one without a warning.

Write rules: create by any member as author; update by the author only (nobody else changes the text or widens the visibility); delete by the author. A moderator tag may archive.

The body is `encryptedText`, so a leaked backup shows ciphertext only. This protects the database file. It does not stop an authorized reader through the API, and it does not stop a superuser (section 8.5).

### 8.4 Break-glass

System collection `_breakGlass`:

| Field | Notes |
|---|---|
| `collection`, `recordId` (text) | what is opened. The rule joins on the record id of the note collection. |
| `note` | relation to the template collection is used in the rule; the route sets it |
| `grantee` | relation to the auth collection |
| `org` | relation to `_orgs` |
| `reason` | text, required, at least 20 characters |
| `basis` | select: `safety`, `legal`, `owner_unavailable`, `other` |
| `requestedAt`, `activeFrom`, `expires` | `expires` at most 24 hours later (setting) |
| `approvedBy` | relation, optional (two-person mode) |
| `ownerNotifiedAt` | date |
| `revoked` | bool |
| `readCount` | number |

Rule clause (run for grantee, per note, expiry and revoke):
`(@collection._breakGlass:g.note ?= id && @collection._breakGlass:g.grantee ?= @request.auth.id && @collection._breakGlass:g.activeFrom ?<= @now && @collection._breakGlass:g.expires ?> @now && @collection._breakGlass:g.revoked ?= false)`.
The prototype ran everything except `activeFrom`, which has the same shape as `expires`.

Route `POST /api/breakglass`:

1. The caller must hold the `breakglass` tag in the note's org. Fewer people hold it than hold `clergy`.
2. A **fresh passkey step-up** is required.
3. `reason` and `basis` are required. Free text is stored in the grant and in the audit entry `meta`.
4. **Modes** (org setting): instant (grant active now, owner told at once), or two-person (a second `breakglass` holder must approve; self-approval refused; `activeFrom` is set at approval).
5. The route emails the note's author and the org owners with the reason, the caller, and the expiry. The notice cannot be turned off. If the author has no usable email, the other breakglass holders and the org owners are told.
6. It writes an audit entry (`action: breakglass_open`, actor, `meta.orgId`, `meta.noteId`, `reason`, `basis`, `expires`).
7. A webhook event follows from the audit entry.

Revocation by the owner or an org owner is one call. Expiry is by the clock.

**Reads must be audited.** The audit log has no read entries. Add a flagged-collection setting `audit.readCollections` (list of collection names). A record view and list request hook (`OnRecordViewRequest`, `OnRecordsListRequest`) writes one audit entry per request (action `read`, collection, record ids up to a cap, actor, IP) for those collections only. For sealed notes, every read by anyone but the author is logged with the grant id, and `readCount` is incremented. This also covers superuser reads through the admin UI, which would otherwise be invisible.

**Deferred notice.** In an active abuse investigation, telling the author may tip off a suspect. Allow `notifyOwner: deferred` only in two-person mode, with a recorded legal basis and a hard date after which the notice is sent. Needs a lawyer's review (section 12).

### 8.5 How encryption (#75) fits and who can read sealed notes

| Reader | Sees plaintext? | Why |
|---|---|---|
| The author | Yes | Rule `author = me`. |
| Org owner or admin | No, until break-glass | No ancestor or admin clause. Verified. |
| A break-glass grantee | Yes, while the grant is live | Grant clause. Reads audited. |
| Superuser (instance operator) | **Yes** | Superusers bypass rules. The server also holds the key. |
| Someone with only a database file or backup | No | Field encryption (ciphertext only). |
| Someone with a backup and the KEK | Yes | Key control. |

So field encryption gives **protection at rest**, not protection from the operator. The issue's "unreadable to admins" means org admins, and the rules deliver that. For the operator, the controls are read audit, the key held outside the server (Azure Key Vault source), and policy.

Stronger option, not in the first milestone: **owner-held keys.** The note is encrypted in the browser with a key derived from the author's passkey (WebAuthn PRF extension) or a passphrase. The server stores ciphertext it cannot read. Break-glass would need escrowed key shares (for example 2 of 3 holders). Trade-offs: no server-side features on the body, a lost key loses the note, support is harder, and PRF support varies by device. Offer it as an opt-in "vault" mode later. See open question 11.

### 8.6 Expiring prayer requests

`expiresAt` plus a daily job from the #88 retention engine: archive (`archived = true`, hides from rules) at expiry, delete after the archive period. A prayer request template defaults to 90 days. The author can renew.

### 8.7 Integration

- **Orgs/tags:** `clergy`, `care-team`, `breakglass`, `moderator` are tags (no resolver change).
- **Hierarchy (#84):** pastoral notes never get an ancestor preset. A diocese does not read parish pastoral care.
- **Audit and webhooks:** breakglass entries and read entries. The webhook payload for sealed note events has `body` redacted by type.
- **Step-up:** break-glass and approval require it.
- **GDPR (#88):** notes are in the data map. A note about a person is that person's data. Exporting it to the person is a decision (see section 12): pastoral notes can include third-party information.
- **Sessions/bans:** a banned or suspended member loses grants at once (the member rule is part of the clause; the grant clause itself is not tied to membership, so the route also revokes grants when a member is suspended).

### 8.8 Legal and safeguarding

- Clergy-penitent and counselor privilege differ by country and state, and so do reporting duties. A lawyer reviews the defaults and the deferred-notice mode.
- A pastoral note about a living person is special-category data. The author, not the church, is often the one who should decide what is written. Train authors to write less.
- Break-glass reasons are themselves sensitive. They sit in the audit log and `_breakGlass`, readable by org owners only.

### 8.9 Risks

- Rules too loose by a preset slip. Mitigation: a locked template and tests.
- Break-glass abuse by a trusted insider. Mitigation: two-person mode, read audit, owner notice, quarterly review report.
- Read audit volume. Mitigation: flag only the sensitive collections, one entry per request.
- Reliance on field encryption as if it hid data from the operator. Mitigation: the table in 8.5 in the docs.

### 8.10 Milestones

| # | Work | Size |
|---|---|---|
| 86-1 | Template `pastoral_notes`, locked rule presets, tests (from section 2.4) | M |
| 86-2 | `_breakGlass`, route, step-up, notification, audit | L |
| 86-3 | Read audit for flagged collections | M |
| 86-4 | Two-person mode, deferred notice, revoke | M |
| 86-5 | Expiry and archive job (shares the #88 engine) | S |
| 86-6 | Owner-held key vault mode (spike first) | XL |

## 9. #87 Permanent records and annotations

### 9.1 Problem and who it serves

Sacramental registers (baptism, confirmation, marriage) are never edited, only annotated in the margin. Many traditions keep membership rolls the same way.

Serves: parish registrars and pastors, diocesan archivists, people who need a certificate.

### 9.2 Goals and non-goals

Goals:

- A collection mode where a finalized record cannot be updated or deleted by anyone.
- Corrections and later events are annotations linked to the record: who, when, why.
- Export of a record with annotations as a certificate.
- Covered by backup verification (#78) and the audit log.

Non-goals:

- No legal validity of an electronic register. Whether it replaces the paper book is a church and civil-law question.
- No digital signature scheme in v1 (a hash chain is provided for tamper evidence).
- No PDF engine in core (see 9.6).

### 9.3 Why rules are not enough

Rules stop the records API (verified: a finalized entry rejects every member update, and annotation update and delete are denied). They do not stop `app.Save`, Go hooks, JS hooks, or superusers (verified: `app.Save` succeeded). The issue asks for rejection "through the API, admin UI, and hooks". That needs a core model hook.

### 9.4 Design

Do **not** add a flag to upstream's `Collection` struct. Use a system config collection `_permanentCollections`: `collection` (name), `finalizeField` (a date field, default `finalizedAt`), `mutableFields` (json list, default `["restricted"]`), `annotationCollection`, `chain` (bool).

A fork hook (`OnModelUpdate`, `OnModelDelete`, and the collection hooks):

- Refuses to delete any record whose `finalizeField` is set.
- Refuses to update any field outside `mutableFields` on such a record, and refuses to clear `finalizeField`.
- Refuses to delete the collection, remove the finalize field, or change its type.
- Applies to Go and JS hooks and the admin UI because it sits at the model layer. Raw SQL and the SQL console are outside the guarantee, as for the audit log. Stated in the docs.

Finalize is irreversible. Route `POST /api/registers/{collection}/{id}/finalize` requires step-up, a registrar tag, and optionally a second signer ("registrar and pastor"). It sets `finalizedAt`, `finalizedBy`, and when `chain` is on, `seq` and `hash` = SHA-256 over the canonical record JSON and the previous record's hash. Edits to a draft entry are free until then.

`claviger registers verify` recomputes the chain and reports breaks. #78 backup verification calls it on restore and reports the result. A break means a record was changed outside the app (raw SQL, a bad restore).

Mutable fields: `restricted` (bool), used by #88 for restriction of processing (Art 18) while the entry stays. Rules for viewers include `restricted = false`.

### 9.5 Annotations

Template `register_annotations`: `entry` (relation to the register, required), `kind` (select: `correction`, `later_event`, `legal_note`, `retraction`, `data_subject_note`), `text`, `author`, `reason` (required), `supersedes` (optional relation to another annotation), `created` (autodate).

Rules (run, section 2.5): create allowed to org members with the `registrar` tag when the entry is finalized (via `@collection.register:r.finalizedAt ?!= ""`), update and delete null. A model hook adds the same ban for every caller, as for `_auditLog`. A mistaken annotation is not deleted. A `retraction` annotation points to it with `supersedes`. The view shows the entry and its annotations together, newest last.

Annotation kinds map to practice: a later marriage noted in the baptism margin is `later_event`; a person's objection or apostasy note is `data_subject_note` (see 10.7).

### 9.6 Certificate

`GET /api/registers/{collection}/{id}/certificate` returns an HTML page with print CSS: the entry fields, the annotations, the chain hash, the issue date and issuer. Browsers save it as PDF. A server-side PDF library adds size and fonts; defer it. A diocese's seal and wording are theme templates per org in `_orgs.settings`. Each certificate issue is an audit entry. Whether a certificate is legally valid is the church's call.

### 9.7 Integration

- **Audit:** finalize, annotate and certificate issue are audit entries with `meta.orgId`.
- **Field encryption:** sensitive columns (parents' names, godparents) can be `encryptedText`. They are not filterable. Keep a plain searchable surname field for lookup.
- **Hierarchy (#84):** a diocesan archivist tag may read registers of descendants through the closure presets.
- **GDPR (#88):** the register is registered in `_dataMap` as `retain` with the legal reason, and is the main erasure conflict (10.7).
- **Webhooks:** `_annotations` and `register:finalize` events.

### 9.8 Risks

- A hook bug that blocks legitimate edits to drafts. Mitigation: tests per field and per state.
- Chain break after a restore from an older backup. Mitigation: the verify command and a documented restore step.
- Treating this as a legal archive. Mitigation: docs that say it is a working register.

### 9.9 Milestones

| # | Work | Size |
|---|---|---|
| 87-1 | `_permanentCollections`, model hook, finalize route, tests that cover `app.Save`, JS hooks and API | L |
| 87-2 | Annotation template, rules, retraction, display | M |
| 87-3 | Hash chain and `registers verify`, backup verification hook | M |
| 87-4 | Certificate page | M |
| 87-5 | Admin UI (finalize, annotate, entry plus margin view) | M |

## 10. #88 GDPR and children's data tools

### 10.1 Problem and who it serves

Membership in a congregation reveals religious belief. That is special-category data under GDPR Article 9. Children's data adds COPPA and similar rules. Built-in tools make the project credible for European churches and reassure US ones.

Serves: church administrators (access and erasure requests), data subjects (self-service export and deletion), safeguarding and legal contacts, auditors.

### 10.2 Goals and non-goals

Goals:

- Consent records: what, when, by whom (including a guardian), and withdrawal.
- Data subject tools: export everything about a person, erase or anonymize with legal holds.
- Self-service export and deletion (the extra scope in the issue comment), with emailed confirmation and a grace period.
- Minors: no direct marketing by default, guardian consent, purge after ageing out.
- Retention policies per collection.
- A data-protection guide for administrators.

Non-goals:

- No legal advice. Defaults are conservative, and the guide says where a lawyer is needed.
- No automatic discovery of personal data in free-text fields.
- Backups are not rewritten (see 10.6).
- No cookie banner or marketing tool.

### 10.3 The data map (build first)

A system collection `_dataMap` lists, for each collection that holds personal data: the **subject path** (which field links a row to a person: `user`, `member`, `author`, `owner`), the **treatment** (`export`, `erase`, `anonymize`, `retain`), `retainReason`, `fields` (which fields are personal), and `ageOutBasis`. System collections register themselves in the migration. Template collections register when installed. Custom collections are registered in the admin UI.

Why first: erasure and export are only correct if every collection that holds personal data is listed. If #80 to #87 land without registering, their data is missed. The map also produces the Article 30 record of processing (a generated report).

Subject paths for the built-in collections:

| Collection | Subject | Export | Erasure |
|---|---|---|---|
| auth record (users) | itself | profile, minus password hash and token key | anonymize: email to `erased-<id>@invalid`, name blank, tokens rotated |
| `_externalAuths` | `recordRef` | provider, provider id | erase |
| `_webauthnCredentials` | owner | name, created, last used (no key material) | erase |
| `_totps`, `_mfas`, `_otps` | owner | existence only | erase |
| `_sessions`, `_authOrigins` | owner | device label, IP, times | erase |
| `_apiKeys` | owner | name, prefix, scopes, last used | erase |
| `_bans` | target | reason, dates | retain for the ban period (legal or security interest), then erase |
| `_orgMembers`, `_invitations` (by email) | user, email | roles, tags, source | erase |
| `_oauthConsents`, `_oauthTokens`, `_oauthRefreshTokens`, `_oauthRequests` | user | client, scopes, dates | erase |
| `_scimTokens`, `_scimGroups` | not personal | none | none |
| `_householdMembers`, `_households` | member | the person's own row and shared contact data | anonymize the member row |
| `_credentials` | user | all | erase after the credential retention |
| `_consents`, `_consentEvents` | subject | all | retain as evidence for the limitation period, then erase |
| `_breakGlass` | grantee, note author | rows about them | retain (audit value), anonymize the person |
| `_auditLog` | actor, record | entries about the subject and by the subject | scrub (10.6) |
| `_webhookDeliveries` | via payload | deliveries whose payload names the subject | delete |
| `_logs` (auxiliary db) | by user id, IP | not exported by default | removed by the existing log retention (set `logs.maxDays`) |
| app collections | per `_dataMap` | per map | per map |

### 10.4 Consent

`_consentPurposes` (system): `key`, `name`, `text`, `version`, `lawfulBasis` (`consent`, `legitimate_interest`, `legal_obligation`, ...), `minAge`, `requiresGuardian` (bool).

`_consents` (state row, unique on `(subject, purpose)`): `subject` (member or user), `purpose`, `policyVersion`, `grantedAt`, `withdrawnAt`, `givenBy` (the subject or a guardian member), `method` (`web`, `paper`, `staff`), `evidenceRef`.

`_consentEvents`: append-only (same hook as `_auditLog`): one row per grant or withdrawal with the text version shown. This is the evidence, kept for the limitation period even after erasure of the person (as a pseudonymized row).

Rules can gate on consent:
`@collection._consents:c.subject ?= member && @collection._consents:c.purpose ?= "newsletter" && @collection._consents:c.withdrawnAt ?= ""`.
This follows the same alias pattern as the credential gate; the date-empty comparison was not run.

Minors: a purpose with `requiresGuardian` accepts a grant only from an active guardian of the subject's household (rule and route). Marketing purposes default to `minAge` 18, so no direct marketing to minors.

### 10.5 Export

Admin route `POST /api/gdpr/subjects/{id}/export` and self-service `POST /api/me/export`. Both start a background job (one at a time, as with field-key jobs) and produce a ZIP: `manifest.json`, one JSON file per collection, attachments in `files/`, and a short human-readable `README.txt`. Format: structured, common, machine-readable (Article 20).

Contents come from the data map. In addition:

- **Audit entries:** where the subject is the actor or the record. For records that are not the subject's own, only the subject's fields are shown. IP addresses of the subject are included.
- **Webhook deliveries and OAuth records:** the subject's rows (see the table).
- **Other people's data:** removed. The household export shows the household's contact data and the subject's row; other members appear as a count.
- **Pastoral notes** (#86): not included by default, because they contain the author's and third parties' information. An admin policy decides (open question 14).
- **Secrets:** never included.

Delivery: a one-time link (session required, fresh step-up for self-service), expiring after 7 days, file deleted after download or expiry. The audit entry (`gdpr_export`) lists categories and counts, not content. A webhook event is emitted from that entry. Time limits: GDPR one month (extendable); CCPA 45 days. The request table tracks the due date.

`_dataRequests` (system): `type` (`export`, `erase`), `subject`, `requestedBy`, `state` (`awaiting_confirmation`, `grace`, `running`, `done`, `partial`, `cancelled`), `dueAt`, `confirmedAt`, `graceUntil`, `result` (json: categories, counts, retained list with reasons).

### 10.6 Erasure

Self-service flow:

1. Signed-in user asks to delete the account. A confirmation link goes to their email. Without email, staff handle it through the admin tool with identity checks.
2. After confirmation, an optional grace period (default 14 days, setting 0 to 30) starts. The account is locked during it (a ban with reason `pending_erasure`). The user can cancel by signing in with a passkey step-up or by the link.
3. When the grace ends, the job runs the data map: delete, anonymize or retain, per row, subject to legal holds.
4. The result lists what was erased and what was retained with the reason. The data subject is told (Article 12(4)).

Legal holds: `_legalHolds` (system): `scope` (subject, or collection and record), `reason`, `reference`, `placedBy`, `placedAt`, `liftedAt`. A hold blocks erase and anonymize for the rows in scope. Placing and lifting need step-up and are audited. Retention policies skip held rows.

**Audit log vs erasure.** The audit log is append-only by design, and its before and after values contain personal data. Erasure must reach it. Options:

| | A. Scrub (recommended) | B. Per-subject keys (crypto-shredding) | C. Keep as is |
|---|---|---|---|
| How | A dedicated function nulls `before`, `after` and IP in entries about the subject, keeps action, time, collection, a pseudonymous record id, and writes its own `gdpr_erase` entry | Audit personal fields are encrypted under a key per subject, and erasure destroys the key | Retain the log under a legitimate-interest claim |
| Append-only guarantee | One documented exception, itself audited | Kept | Kept |
| Cost | Small | Large, changes the audit format | None |
| Risk | The function is a powerful tool | Key management at scale | Likely fails an erasure request for non-security data |

Recommendation: A. Entries needed for a security reason may be kept under a documented basis, which the lawyer decides (open question 18).

Backups and replicas: erased data persists in backups and Litestream replicas until they rotate. Keep a `_erasureLog` of (subject id hash, date, collections). After a restore, the app replays the log and re-erases. The guide says this and sets backup retention against the policy ("beyond use" until it rotates). Encrypted fields help: once a field's data key is pruned, old ciphertext stays unreadable. Per-field keys do not isolate individuals, so this is not a substitute.

### 10.7 Erasure vs permanent records (#87) and legal holds

The conflict is real. A baptism entry is permanent by church rule, and a person (or the family) can ask for erasure. Under GDPR, Article 17(3) lists exceptions (legal obligation, public interest, archiving, legal claims), and Article 91 respects churches' own data rules where they existed before 2016. Whether a church register falls under these depends on the country, the denomination and the regulator, and decisions differ. This needs a lawyer for each deployment. The design gives the congregation the tools for either outcome:

- Default: registers are `retain` in the data map with a stated reason. An erasure request ends as `partial`, with the retained list and the reason, and the person is told how to complain.
- The request is recorded as a `data_subject_note` annotation on the entry (the practice of many registers). The entry gets `restricted = true`, so it is no longer shown outside the registrar role or used for processing. `restricted` is the one mutable field.
- If the church decides erasure applies, a registrar-level operation with two approvers can anonymize the **personal fields** of an entry, leave its place in the chain through a tombstone hash, and write an annotation. This is not built in the first milestone (open question 15).
- Civil registers copied into the church's own (for example marriage) may be subject to the state's retention rules, not the church's.

### 10.8 Retention

`_retentionPolicies`: `collection`, `basis` (a date field: `created`, `updated`, or a named field), `after` (days or months), `action` (`delete`, `anonymize`, `archive`), `filter` (plain filter text), `enabled`, `dryRun`.

A daily job applies enabled policies in batches, skips held rows, and writes one audit entry per run (counts per policy). A dry run reports counts without changing anything. Defaults suggested in the guide: check-in sessions 12 months, expired prayer requests 90 days, recovery codes 7 days after use, invitations (already 90 days), consent events per local limitation period.

Youth data: a policy basis of "age out" uses `isMinor` turning false plus N years. The job runs in Go and can read `birthDate` (encrypted) per row. This is fine for congregation scale (thousands of rows). A plain derived date column would reveal birth year, so avoid it.

### 10.9 Special-category and children's defaults

- Religion is Article 9 data. The lawful ground for a church's own members is usually Article 9(2)(d) (not-for-profit bodies with a religious aim, about members with regular contact, not disclosed outside without consent). That ground limits **disclosure**, which matters for #84 (a diocese), #85 (third-party apps) and webhooks. Lawyer.
- Children: no minor logins under the deployment's age setting, no marketing, guardian consent, short retention for check-in, health data encrypted.
- Data protection impact assessment: the guide includes a template, since children's data and special-category data at scale trigger it in many places.
- Roles: the congregation is the controller. The host operating the server is a processor. A data processing agreement is needed (template for a lawyer to adapt).

### 10.10 Integration

- **Audit and webhooks:** `gdpr_export`, `gdpr_erase`, `gdpr_hold`, `consent_*` are audit entries with their own action names (the precedent is the `field_key_*` entries). Webhooks get them like other entries, so the receivers see ids and counts only.
- **Sessions/bans:** the grace period is a ban with a distinct reason. Erasure revokes sessions, API keys, OAuth tokens and passkeys.
- **Field encryption:** exports decrypt in the app. The backup of an export file is not kept. `convert` and `strict` help when adding personal fields.
- **OIDC:** on erasure the subject's refresh tokens and consents are removed, so relying parties cannot refresh. Relying parties keep their own copies; the guide tells admins to notify them (SCIM delete helps for SCIM clients).
- **SCIM:** an erased member is deprovisioned (membership removed, `active=false` sent) before anonymization.

### 10.11 Risks

- Incomplete data map. Mitigation: register-on-create for all fork collections, a CI test that lists personal-data collections without a map entry, and a "unmapped collections" warning in the UI.
- Erasure is destructive and hard to test in production. Mitigation: grace period, dry run, holds, `_erasureLog`, restore replay.
- Scrubbing the audit log weakens its integrity story. Mitigation: one function, one audited path, documented.
- Export leaks other people's data. Mitigation: redaction rules and review of every new mapped collection.

### 10.12 Milestones

| # | Work | Size |
|---|---|---|
| 88-0 | `_dataMap` registry, built-in entries, CI check for unmapped collections | M |
| 88-1 | Export (admin and self-service), job, manifest, audit and webhook | L |
| 88-2 | Consent purposes, state rows, append-only events, guardian consent | L |
| 88-3 | Retention engine with dry run and holds | L |
| 88-4 | Erasure: requests, confirmation, grace, anonymize/erase, audit scrub, erasure log and restore replay | XL |
| 88-5 | Admin UI (requests, holds, consent history, data map) | L |
| 88-6 | Administrator guide, DPIA and Article 30 templates | M |
| 88-7 | Permanent-record restriction flow and optional register anonymization (after #87) | M |

## 11. #85 Single sign-on presets for church software

This section is short because the work is mostly integration testing, and the vendor facts must be checked against current documentation and test accounts first. I did not verify any vendor's behavior while writing this spec.

### 11.1 Problem and who it serves

Members use separate logins for giving, the church management system, livestream and the school portal. With the OIDC provider and SCIM shipped, one login can cover them. Serves members, parish staff and administrators.

### 11.2 Goals and non-goals

Goals:

- Documented, tested setup guides and client templates for common church software.
- SCIM provisioning to the products that accept it.
- At least two platforms verified end to end (the issue's definition of done).

Non-goals:

- No SAML identity provider in this spec. The issue lists "generic SAML". Claviger has no SAML IdP, and it would be a large separate piece (open question 28).
- No outbound SCIM client in core (a #79 non-goal). A one-way sync connector is a separate tool.
- No bundled vendor SDKs.

### 11.3 Design

- **Presets** are JSON templates for `_oidcClients` (name, redirect URIs, scopes, claim mappings, token settings), served by `GET /api/oidc/client-templates` and applied in the admin UI. A template holds placeholders for the product's tenant URL.
- **Guides** in `docs/sso/<product>.md`, each listing: what the product supports (inbound OIDC, SAML, SCIM, or none), the exact settings, the claims it needs, how to test, and known limits.
- **Claims:** the `orgs` scope and org-bound clients (`docs/orgs.md`) already give per-parish tokens. Households or guardian claims stay off for third parties by default (section 4.6).
- **Disclosure:** sending member identity to a third-party product is a disclosure of religious-affiliation data in effect. The template UI shows what claims leave and asks the admin to confirm (section 12, item 2).

### 11.4 Milestones

| # | Work | Size |
|---|---|---|
| 85-0 | Vendor survey: which products accept inbound OIDC, SAML or SCIM (trial accounts, written results) | M |
| 85-1 | Client template store and admin UI | S |
| 85-2 | Two verified end-to-end guides (pick from 85-0) | M |
| 85-3 | Further guides, one at a time | S each |
| 85-4 | One-way sync connector as an example tool outside core, for products with no SSO | M |

## 12. Items for a lawyer

Not legal advice. These are the places where the design depends on law that varies by country, state or denomination.

1. Age of digital consent and parental consent (COPPA, GDPR Art. 8, UK) for any child account or child data.
2. Lawful ground for religious data (Art. 9(2)(d)) and the limits it puts on sharing with a diocese (#84) or third-party apps (#85).
3. Whether a church register can be kept against an erasure request, and the regulator's view in the country (#87, #88).
4. Background-check data: what may be stored and for how long (FCRA, DBS and local equivalents) (#81).
5. Mandatory reporting duties and clergy privilege, and the deferred-notice mode of break-glass (#86).
6. SMS consent and carrier rules for recovery links (#83).
7. Retention periods per record type, and the limitation period for consent evidence.
8. Data processing agreement and controller/processor roles when a host runs the server for a congregation.
9. Any court-ordered no-contact or custody restrictions that check-in must honor (#82).
10. Whether pastoral notes are included in a data subject access response (#86, #88).
11. Insurance conditions that dictate check-in procedure (#82).

## 13. Dependencies and roadmap

### 13.1 Dependency graph

```
shipped: #71 OIDC  #73 passkeys/step-up  #74 audit  #75 field encryption  #77 webhooks
         #78 backup verify  #79 orgs/invitations/SCIM  #93 sessions/bans  #39 passkey reset

88-0 data map ─────────────────────────────────────────────┐ (every new collection registers)
                                                           │
#84 closure ──► #81 credentials ─────────────┐             │
      │                                      ▼             ▼
      └──────► oversight presets      #82 check-in ◄── #80 households ──► #88 consent/export/erasure
                                           ▲   ▲            │
#83 recovery (uses #39) ───────────────────┘   │            │
                                               │            ▼
#87 permanent records ──────────────────────► #88 erasure conflict (hold, restricted flag)
#86 confidentiality ───────────────────────► #88 export policy, read audit
#85 SSO presets ◄── #71, #79c   (independent)
```

Notes: #82 needs #80 and #81 and benefits from #83. #88 export and erasure need #80 (minors, guardians), and #87 and #86 shape their exceptions. #84 is independent but changes the org validator that #81 presets use, so it goes first. #85 depends on nothing in this set.

### 13.2 Recommended order and why

1. **88-0 data map and #84 closure table** (M each, parallel). Both are small foundations. Doing them late forces rework of every collection or every rule.
2. **#81 credentials** (M to L). The central safeguarding gate and the most requested. It needs only shipped parts plus the closure table.
3. **#80 households** (L to XL) with **#83 recovery and accessible login** (M to L) in parallel. Households unlock check-in and the minors parts of #88. Recovery makes passkeys usable for older members and uses mostly existing code.
4. **#87 permanent records** (M to L) and **#86 confidentiality** (L). Both are self-contained. #87 is smaller and defines the immutability hook that #82's log reuses. #86 needs the read-audit hook, which also helps #88.
5. **#88 GDPR tools** (XL, staged 88-1 to 88-7). Export first (value and low risk), then consent, then retention, then erasure last (highest risk, needs everything mapped).
6. **#82 check-in** (XL). It needs households, credentials and recovery, and carries the most safety risk. Ship the reference app separately.
7. **#85 SSO presets** (M), any time after a vendor survey. It is mostly documentation and tested client templates. Schedule it where marketing needs it.

### 13.3 Rough sizes

| Issue | Total |
|---|---|
| #84 | about 5 to 7 weeks |
| #81 | about 4 to 6 weeks |
| #80 | about 8 to 10 weeks |
| #83 | about 6 to 8 weeks |
| #87 | about 4 to 6 weeks |
| #86 | about 8 to 10 weeks (without the vault mode) |
| #88 | about 14 to 18 weeks |
| #82 | about 10 to 14 weeks (core only) |
| #85 | about 3 to 5 weeks |

One developer, calendar estimates with review and docs. They are order of magnitude.

## 14. Cross-cutting risks

- **Rule length and silent denial.** Many gates stack in one rule. Mitigation: generated snippets, tests for each preset, and the access-status route (5.5).
- **Scope.** The nine issues together are about a year of work. The order above delivers value in slices, and the foundations are first.
- **Single-writer deployment.** All jobs (credential lapse, retention, graduation, closure repair, erasure) run in the one serving process, like the field-key jobs. One job at a time per type. They must be resumable, because the replica restarts on every rollout.
- **Postgres (#106).** The rule shapes use only back-relations and `@collection` aliases. Run section 2's checks on Postgres once the dialect layer is the default.
- **Trust in the superuser.** Superusers bypass rules. Pastoral and child data protections are strong against org admins and weak against the operator. The docs say so, and read audit plus key custody narrow the gap.
- **Public repo.** Nothing here is deployment-specific. Sample tags, kinds and labels are neutral. No tenant, domain or runbook content belongs in this spec.

## 15. Open questions for the owner

Each has options and a recommendation.

### Foundations

1. **Is the household member row the person record?**
   (a) Yes: `_householdMembers` is the person, with an optional `user`. (b) A separate `_persons` collection linked to both users and households. (c) Minors as auth records with no email and no password.
   Recommendation: (a). It is the fewest tables and matches the rules I ran. (c) puts non-login people in the auth collection, which weakens its meaning.
2. **System collections or templates for households?**
   (a) System. (b) Templates only.
   Recommendation: (a), because merge, split, last-guardian and graduation guards need Go, and system rules are fixed once.
3. **Build the data map (88-0) before any other feature?**
   (a) Yes. (b) Later, as part of #88.
   Recommendation: (a). It is small and prevents missed collections.

### #84 Hierarchy

4. **Closure table, fixed hops, or denormalized ancestors?**
   Recommendation: closure table (section 3.3).
5. **Depth limit.**
   (a) 4 by default, hard cap 8. (b) Unlimited.
   Recommendation: (a). A limit bounds the closure table and the join.
6. **Should oversight be on by default?**
   (a) Off; each collection opts in. (b) On for descendants.
   Recommendation: (a). A diocese reading parish data may need consent.

### #80 Households

7. **Default graduation age and minor login.**
   (a) 18, no minor logins. (b) 16 with guardian consent. (c) Per-org setting, default (a).
   Recommendation: (c) with default (a). A lawyer sets regional defaults.
8. **Who may remove a guardian quickly (custody disputes)?**
   (a) A staff member with `registrar` tag, audited. (b) Org owners only.
   Recommendation: (a), with notice to the coordinator.

### #81 Credentials

9. **Store document uploads (certificates)?**
   (a) Optional protected file. (b) Attestation only.
   Recommendation: (b) by default and (a) as an org setting. Less data, less risk.
10. **Who verifies a credential?**
    (a) Any coordinator. (b) A second person than the holder's supervisor. (c) Coordinator plus diocesan confirmation.
    Recommendation: (b). Simple and stops self-approval.

### #86 Confidentiality

11. **Sealed notes: rules and at-rest encryption, or owner-held keys?**
    (a) Rules plus field encryption plus read audit now. (b) Owner-held keys (passkey PRF) now. (c) (a) now, (b) as an opt-in vault later.
    Recommendation: (c). It fits "readable only by the owner" against org admins now, and avoids lost-key support in v1.
12. **Break-glass mode.**
    (a) Instant with notice. (b) Two-person approval. (c) Org setting, default (b) for sealed notes.
    Recommendation: (c).
13. **Allow deferred owner notification for investigations?**
    (a) No. (b) Yes, two-person only with a legal basis and a date.
    Recommendation: (a) in v1, (b) after a lawyer reviews. Tipping off a suspect is a real concern, but so is a silent override.
14. **Does a data subject access response include pastoral notes?**
    (a) Never. (b) Only the person's own authored notes. (c) Notes about the person, redacted of third parties, when the author or pastor releases them.
    Recommendation: (b) by default, with (c) as a manual, lawyer-reviewed step.

### #87 Permanent records

15. **Which erasure outcome is the default for registers?**
    (a) Retain with reason and annotate. (b) Allow anonymization of personal fields with two approvers. (c) Per org.
    Recommendation: (a) now. Build (b) only when a regulator or court makes a church need it (88-7).
16. **Chain hash on finalized entries?**
    (a) On by default. (b) Optional per register.
    Recommendation: (b), default on for sacramental templates.
17. **PDF certificates.**
    (a) HTML with print CSS now. (b) Server PDF.
    Recommendation: (a). Revisit if dioceses require signed PDFs.

### #88 GDPR

18. **How to treat the audit log on erasure.**
    (a) Scrub personal values, keep structure. (b) Per-subject keys. (c) Keep.
    Recommendation: (a), with a documented list of entries kept for security reasons.
19. **Self-service deletion grace period.**
    (a) None. (b) 14 days (setting 0 to 30). (c) 30 days.
    Recommendation: (b).
20. **Export contents for other people in the household.**
    (a) Own row and shared contact data only. (b) The whole household.
    Recommendation: (a). The other members are separate data subjects.
21. **Backups after erasure.**
    (a) Replay the erasure log on restore. (b) Rewrite backups.
    Recommendation: (a). (b) breaks backup verification and is slow.

### #83 Recovery and accessibility

22. **Who holds the `greeter` tag, and does an elevated account need two staff?**
    (a) Admins only. (b) Any staff with the tag, two staff for owner/admin targets.
    Recommendation: (b).
23. **SMS provider.**
    (a) Interface plus Twilio. (b) Generic webhook only. (c) No SMS in v1.
    Recommendation: (a). People without email are the point of the feature.
24. **First extra language.**
    Spanish is named in the issue. Recommendation: Spanish, with the message table ready for more.

### #82 Check-in

25. **How much of check-in is core?**
    (a) Primitives only (templates, routes, codes, kiosk principal). (b) Also a reference kiosk UI in this repo. (c) A full product.
    Recommendation: (a), with the reference UI as a separate example app. The table in 7.2 is the boundary.
26. **Per-family or per-child codes?**
    (a) One code per family per event. (b) One per child.
    Recommendation: (a) for usability, with the child's session id in the QR.
27. **Retention for check-in logs.**
    (a) 12 months. (b) Until age of majority. (c) Per org.
    Recommendation: (c), default (a), with incident holds.

### #85 SSO presets

28. **Is a SAML identity provider in scope?**
    (a) No, OIDC only. (b) A separate spike for SAML. 
    Recommendation: (a) now and (b) only if the vendor survey (85-0) shows a product that needs it.
29. **Which two platforms first?**
    Decide from the survey (85-0), not from this spec.
