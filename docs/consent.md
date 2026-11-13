# Consent records

Consent records say what a person agreed to, when, on which wording, and who gave it. They cover GDPR Article 7 (proof of consent, easy withdrawal) and children's data (a guardian consents for a minor). This is milestone 88-2 of the data tools (#88). Section 10.4 of `docs/specs/80-88-congregations.md` is the design.

This is not legal advice. Which purposes need consent, and for how long evidence must be kept, depends on the country and the congregation. Have a lawyer review the purposes and texts.

## The three collections

All are hidden system collections with null write rules. Rows are written by the routes below, never by the records API.

| Collection | What it holds |
|---|---|
| `_consentPurposes` | What a person can consent to: `key`, `name`, `text`, `version`, `lawfulBasis`, `minAge`, `requiresGuardian`, `marketing`, `org`, `status` |
| `_consents` | The state: one row per person and purpose (unique on `subject, purpose`). `policyVersion`, `grantedAt`, `withdrawnAt`, `givenBy`, `method`, `evidenceRef`, `org` |
| `_consentEvents` | The evidence: one row per grant or withdrawal, with the version and the full text that was shown. Append-only |

The **subject** is a text id: a household member (a person without a login, such as a child) or a user. `subjectType` says which. The **purpose** is stored as its key (for example `newsletter`), not as an id, so that a rule can compare it with a literal. Keys are unique, lowercase, and cannot change.

`method` is `web` (the person or a guardian, signed in), `paper` or `staff` (recorded by staff). `givenBy` is the person who gave it: the subject, or the guardian's household member id.

### Purposes

- A purpose with no `org` is offered to everyone. A purpose with an `org` is offered to the people of that org only (its active members and the people of its households).
- `lawfulBasis` is one of `consent`, `legitimate_interest`, `legal_obligation`, `contract`, `vital_interest`, `public_task`. Only `consent` needs a grant; the others can be listed so people can see them.
- `minAge` is the age below which a guardian must give the consent. `requiresGuardian` makes a guardian necessary for every minor.
- `marketing` marks direct marketing. It gets `minAge` 18 when none is set, and it can never be granted for a minor, by anyone.
- **Changing the text raises the version.** Existing consents keep the version they were given on and show as `outdated` until the person consents again. Changing the name, basis or flags does not raise it. Rules that must only accept the current wording can check `@collection._consents:c.policyVersion` against a literal.
- A purpose that has consent records cannot be deleted (409). Archive it instead (`status: archived`): it is no longer offered, and the evidence stays.

## Who can consent for whom

| Subject | Who can grant or withdraw |
|---|---|
| A user (adult) | The user, signed in |
| A minor (a household member flagged `isMinor`) | An active adult guardian of the same household. The minor has no login |
| A person below the purpose's `minAge` | A guardian, as for a minor. A user who has a member row below the age cannot consent alone |
| Anyone, on paper or in person | Org staff (below), with an `evidenceRef` |

An adult cannot be consented for by someone else through the self-service route: only staff can record it, from paper. Withdrawal is never harder than granting: a guardian, the person, or staff can withdraw.

**Staff** are the owners and admins of the org and the members tagged `registrar`. This is the same set that manages households (`docs/households.md`). No new tag was added.

For a minor, staff must also name the guardian who gave the consent (`givenBy`, an active guardian of the person's household).

## Routes

API keys are refused on all of them.

| Route | Who | |
|---|---|---|
| `GET /api/me/consents` | signed-in user | The purposes offered to the user and to each minor they are a guardian of, with `status` (`granted`, `withdrawn`, `none`), `outdated`, `canGrant` and a `reason` when it cannot be granted (`marketing_minor`, `needs_guardian`) |
| `POST /api/me/consents/{purpose}/grant` | signed-in user | Body `{"member": "<id>"}` for a minor of the household. Without `member`, for the caller |
| `POST /api/me/consents/{purpose}/withdraw` | signed-in user | Same body |
| `POST /api/orgs/{id}/consents` | org staff | `{purpose, member or user, action: grant or withdraw, method: paper or staff, evidenceRef, givenBy}`. A grant needs `evidenceRef` |
| `GET /api/orgs/{id}/consents?member=ID` or `?user=ID` | org staff | The person's consents and history (rows of this org and of no org) |
| `GET /api/consent-purposes?org=ID` | members of the org | Active purposes offered to the org. A superuser without `org` gets all |
| `POST /api/consent-purposes`, `PATCH /api/consent-purposes/{id}`, `DELETE /api/consent-purposes/{id}` | superuser | `id` can be the key |
| `GET /api/consents/history?subject=ID` | superuser | Consents and history of one member id or user id |

Granting what is already granted on the current version changes nothing and writes no event. A grant after a withdrawal clears `withdrawnAt` and records a new event.

## Gating a rule on consent

The rule syntax from the spec works as written. Add the sign-in check you want:

```
@request.auth.id != "" &&
@collection._consents:c.subject ?= member &&
@collection._consents:c.purpose ?= "newsletter" &&
@collection._consents:c.withdrawnAt ?= ""
```

`member` is a relation to `_householdMembers` on your own collection. For a user, compare with `@request.auth.id` instead of `member`. The three conditions use the same alias, so they apply to one row. A consent that was never given has no row, so the rule denies. A withdrawn one has a date, so it denies. Core tests run this rule (`TestConsentRuleGating`).

## The history is append-only

`_consentEvents` accepts new rows only. A model hook refuses every update and delete, so this holds for `app.Save`, Go hooks and JS hooks (not raw SQL), the same way it does for the audit log. Each row keeps the action, the purpose, the version and the text shown, the time, who gave it, who did it, the method, and the evidence reference.

Every consent write also makes the usual audit entry. When the purpose or the person belongs to an org, the entry has `meta.orgId`.

### Erasure

Consent records are evidence, so the data map treats `_consents` and `_consentEvents` as `retain`. When a person is erased (88-4, not built yet) the rows stay, but the person is replaced by a pseudonym. `core.PseudonymizeConsents(app, ctx, ids...)` does that for a household member id or a user id: it replaces the id in `subject`, `givenBy` and `actor` with `erased-<hash>`, clears `evidenceRef`, and sets `pseudonymizedAt`. The action, purpose, version, text and times stay. It is the only code path allowed to change an event. Nothing calls it yet.

## Export

The person's own consents and history are in their data export (`docs/data-export.md`): rows whose subject is their user id, and rows filed under their own household member row. A guardian's export does not include the consents given for a child, because the child's row is not reached through a login. There is no export for a child yet.

## Admin page

The admin UI has a **Consents** page (`#/consents`) to create and edit purposes and to look up the history of one person by member id or user id.

## Not built yet

- A hosted page where people manage their own consents (the routes exist).
- Purposes managed by org admins (only superusers write purposes).
- Webhook events for consent changes (the audit entries are delivered like other entries).
- Automatic expiry of old events (retention, 88-3).
