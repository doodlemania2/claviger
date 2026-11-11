# Households and guardian-managed minors

Congregations think in families. A household groups people, holds their shared contact data, and says which adults may manage the children. A child needs no email and no login.

This is the guide for operators and developers. The design is in `docs/specs/80-88-congregations.md`, section 4. It builds on organizations (`docs/orgs.md`), field encryption (`docs/field-encryption.md`) and the data map (`docs/data-map.md`).

**A guardian flag is a church record, not a legal finding.** Claviger does not decide custody. Courts do. Staff must be able to remove a guardian quickly, and they can (see [Staff](#staff)).

## What is stored

Two hidden system collections:

| Collection | Holds |
|---|---|
| `_households` | `org` (the home org), `name`, `address` (JSON), `homePhone`, `status` (`active`, `archived`). |
| `_householdMembers` | One row per **person**: `household`, optional `user`, `name`, `role` (`head`, `spouse`, `adult`, `child`, `other`), `guardian`, `isMinor`, `birthDate`, `photo`, `allergies`, `medical`, `status` (`active`, `left`, `deceased`), `source` (`manual`, `import`, `scim`). |

The member row **is** the person. A login (`user`) is optional. Other collections point at the member row when they are about a person, and at the user when they are about an account holder.

Address, home phone, birth date, allergies and medical notes are **encrypted fields** (see the field encryption guide). Rules cannot read them, so rules use the plain flags `isMinor`, `guardian` and `status` next to them.

The migration creates those five fields as plain fields, so it never depends on a key. At every start of the server, if a key-encryption key is configured (`CLAVIGER_KEK`), Claviger turns them into encrypted fields and encrypts the values already there. Until that has happened the routes refuse to store any value in them (409), and the server logs a warning at start. **Set the key before you put children's data in.**

The photo is a protected file.

## Rules

Both collections have list and view rules only. Create, update and delete are closed. All writes go through the routes below, so the guards always run.

- A person sees the household and its people while they are an **active** member with a login.
- A person with a login always sees their own row.
- Staff of the household's org see all of its households (see [Staff](#staff)).

Staff of an **ancestor** org do not see a household through the tree. Give them a role in the home org if they need it.

No API key can read or write these collections.

## Guards

They hold for every writer: routes, Go code, hooks and the admin API.

- A minor has **no login**. Setting `user` on a minor is refused.
- A minor is **not a guardian**.
- A household with an active minor keeps **at least one active adult guardian**. A change that would break this is refused with a 409: demoting the last guardian, marking them `left` or `deceased`, or removing them.
- The same login can appear once per household.
- When `birthDate` is set, `isMinor` follows it and the org's graduation age. A value set by hand is overwritten. Without a birth date, `isMinor` is whatever staff set.

The guard on removal runs in the routes. Deleting a row directly (for example erasing a person, #88) is not blocked.

## Graduation

A minor becomes independent at the org's **graduation age**. The default is 18. Set it in the org settings (as a superuser): `{"graduationAge": 16}`. Whole numbers from 14 to 21 are accepted. Anything else is refused when the org is saved, and a stored invalid value counts as 18.

A daily job (06:41) looks at every active minor who has a birth date. If the person has reached the age in their org, it sets `isMinor` to false and writes one audit entry, `household_graduated`. The entry holds the member id, the household and the age. It holds no name and no birth date. The job never turns an adult back into a minor, leaves a minor without a birth date alone, and is safe to run twice.

After graduation:

- guardians can no longer write the person's records (the rule reads `isMinor` live);
- household members can still read them;
- staff can give the person a login with `PATCH /api/households/{id}/members/{member}` and `{"user": "..."}`. Their records keep the same member id, so their history follows them.

## Staff

Staff of an org are: owners, admins, and active members tagged **`registrar`**. Create the tag as an `_orgRoles` row named `registrar` in the org and add it to the member's `tags`. A `viewer` is never staff. A superuser is always staff.

Staff can read and change every household of their org, give a person a login, archive a household, and remove a guardian at once (as long as another adult guardian remains while there is a minor).

## Who may do what

| Action | Allowed |
|---|---|
| Create a household in an org | any active member of the org (they become head and first guardian), or staff (empty household, or any first person) |
| Rename, change address and phone | a guardian of the household, staff |
| Archive a household | staff |
| Add a person | a guardian, staff |
| Give a person a login | staff (a guardian may only attach their own) |
| Change a person | staff; a guardian, except another guardian's flag or status and any login; a person with a login may change their own name, birth date, allergies and medical notes, step down as guardian, or leave |
| Set a guardian flag | a guardian or staff |
| Clear another person's guardian flag, remove a guardian | staff, or the person themselves |
| Remove a person who is not a guardian | a guardian, staff, or the person themselves |

## Routes

All need a signed-in user. API keys are refused.

| Route | |
|---|---|
| `GET /api/households/mine` | the households I belong to, with their people |
| `GET /api/households?org=ID` | the households of an org (staff) |
| `POST /api/households` | `{org, name, address?, homePhone?, first?}` |
| `GET /api/households/{id}` | the household and its people |
| `PATCH /api/households/{id}` | `{name?, address?, homePhone?, status?}` |
| `POST /api/households/{id}/members` | `{name, role?, guardian?, isMinor?, user?, birthDate?, allergies?, medical?}` |
| `PATCH /api/households/{id}/members/{member}` | the same keys, and `status` |
| `DELETE /api/households/{id}/members/{member}` | remove the row |
| `POST /api/households/{id}/members/{member}/guardian` | make the person a guardian |
| `DELETE /api/households/{id}/members/{member}/guardian` | clear the flag |

`birthDate` is a date such as `2014-05-17`. Another household, or one you have no part in, answers 404.

Birth date, allergies and medical notes of a person are shown to staff, to guardians and to the person. Other members see name, role and flags.

Every change is a normal record write, so it lands in the audit log with the actor, the changed fields and `meta.orgId` (and `meta.household` for people).

## Rules for your own collections

A collection about a person carries a relation `member` to `_householdMembers` and a **copy** `household` (a relation to `_households`). The copy keeps the rules to one hop. Go helpers build the rules (`core/household_rules.go`):

```go
set := core.HouseholdRuleSet(core.HouseholdRuleOpts{}) // household + member
core.BindHouseholdCopy(app, "child_notes", core.HouseholdRuleOpts{})
```

| Helper | Meaning |
|---|---|
| `HouseholdMemberRule("household")` | I am an active member of this record's household |
| `HouseholdGuardianRule("household")` | I am an active guardian in it (rule M of the spec) |
| `HouseholdPersonReadRule(opts)` | household members read; so does the person, with a login |
| `HouseholdGuardianWriteRule(opts)` | an active guardian writes while the person is a minor, and the person belongs to the record's household |
| `HouseholdStaffRule("org")` | owner, admin, or `registrar` of the org |
| `HouseholdRuleSet(opts)` | list and view = person read; create, update and delete = guardian write |

Back relations are multi-valued, so the rules use `?=`. `BindHouseholdCopy` sets `household` from `member` on every save, so a guardian cannot name their own household with another household's child.

Access follows the data live: remove a guardian and they lose access on the next request. Graduation closes guardian write access the same way. No session is revoked and no job runs for it.

## Privacy

- Household collections never produce webhooks, and are not in OIDC claims.
- The personal data export (#88) shows a person their **own** row and the **shared** household contact data (address, home phone). It never shows another member's details. A minor has no login, so a minor's row is not part of anyone's export through this path.
- Erasing one person removes their row and keeps the household record, because it belongs to the others too.
- Age rules differ by country (for example COPPA, GDPR Article 8). Claviger's default is that minors have no logins at all. Ask a lawyer before you change a deployment that way.
