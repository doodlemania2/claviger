# Retention and legal holds

Retention is how a congregation keeps its promise to delete what it no longer needs (GDPR Article 5(1)(e): storage limitation). A **policy** names a collection, a date, a period and an action. A **daily job** applies the enabled policies. A **legal hold** freezes a person or one record against deletion while a legal matter is open.

Nothing here is legal advice. **How long to keep each kind of record is a decision for your lawyer** (spec section 12, item 7: retention periods per record type, and the limitation period for consent evidence). Claviger ships no policy, enabled or not. The table at the end only lists the periods the guide suggests as a starting point.

## Policies

`_retentionPolicies` (system collection, superusers only; the records API refuses writes, use the routes).

| Field | Meaning |
|---|---|
| `collection` | The collection the policy applies to. |
| `basis` | The date the period counts from: `created`, `updated`, the name of a date field of the collection (for example `expiresAt`), or `ageOut` (below). |
| `after`, `unit` | The period: a number and `days`, `months` or `years`. 0 is allowed only for a named date field (an expiry date). |
| `action` | `delete`, `anonymize` or `archive`. |
| `filter` | Optional filter text on the collection's own fields, for example `kind = "prayer"`. `@request` and `@collection` are refused. Encrypted fields cannot be filtered. |
| `enabled`, `dryRun` | See below. |

Actions:

- `delete` removes the row. Cascading relations behave as for any delete, and a guard that refuses it (a last guardian, a legal hold) counts as `failed`.
- `anonymize` blanks the **personal fields of the collection's data map entry** (docs/data-map.md) and keeps the row. A collection with no entry, or an entry without personal fields, cannot have an anonymize policy enabled: the route answers 400 and says to fill in the data map first.
- `archive` sets `archived = true`. The collection needs a bool field with that name. Hiding archived rows is the collection's rules' job (the pastoral template does it).

### New policies are safe

A policy is **always created disabled and as a dry run**, whatever the request says. Going live is a second step:

1. `POST /api/gdpr/retention/policies` creates it.
2. `POST /api/gdpr/retention/run` with `{"preview": true, "policy": "<id>"}` shows what it would do and changes nothing.
3. `PATCH .../policies/{id}` with `{"enabled": true}` makes the daily job count it (still a dry run, so still nothing changes).
4. `PATCH .../policies/{id}` with `{"dryRun": false}` makes it real. **A policy that ends up enabled and live needs a fresh passkey step-up** (the same check as placing a hold). Changing a live policy needs one too.

A policy cannot be edited or deleted while a run that includes it is unfinished.

### What a policy never touches

- Rows under an active **legal hold** (below).
- Pastoral notes with an **open `_pastoralHolds` row** (a note held back from a person's export and not yet decided, docs/pastoral-notes.md).
- **Permanent registers** (#87) and their annotation collections: a policy on one is refused when it is saved, and again when it runs.
- Accounts (auth collections): erasure handles them. And `_auditLog` (it has its own `audit.retentionDays`), the policy and hold tables, `_dataMap`, `_fieldKeys`, `_permanentCollections`, `_dataRequests`.

### Age out

`basis: "ageOut"` is for youth data. The period counts from the day a person's `isMinor` turns false: `birthDate` plus the org's graduation age (default 18, `graduationAge` in the org settings, as in `docs/households.md`). The collection needs a `birthDate` field. The job reads each row's (encrypted) `birthDate` and computes the date in Go. **There is no derived date column**, because one would reveal the birth year. A row with no usable birth date never qualifies. The org is read from the row's `org` field, or through its `household`. Use a `filter` on plain fields to narrow it (for example `role = "child"`).

## The job

A daily run (02:27 server time) applies every enabled policy in id order, in batches of 200. Per policy it counts:

| Count | Meaning |
|---|---|
| `matched` | Past the period and matching the filter. |
| `held` | Skipped because of a legal or pastoral hold. |
| `unchanged` | Already in the state the action leaves (anonymized, archived). |
| `failed` | Could not be changed (a guard refused). The reason goes to the server log, not the audit entry. |
| `affected` | Deleted, anonymized or archived. For a dry run: would be. |

A dry run (the policy's `dryRun`, or a manual preview) computes the same counts and changes nothing.

`POST /api/gdpr/retention/run` runs the job now. With `{"preview": true}` every enabled policy is previewed; add `"policy": "<id>"` to preview one policy even if it is not enabled. A preview keeps no run state.

### Resumable

The replica restarts on every rollout, so a run must survive that. A run marks its policies `running` and stores the cursor (the last row id scanned) and the counts after every batch. At start-up, and at the next daily run, an unfinished run **continues from its cursor**: each row is handled once, and the counts add up across the restart. Only one run at a time (several replicas: one, through the replica lock).

### Audit

One entry per run, `gdpr_retention_run` (collection `_retentionPolicies`). `meta`: `runId`, `resumed`, `preview`, `startedAt`, `finishedAt`, `policies` (per policy: id, collection, basis, period, action, `dryRun`, the five counts, `error` if any) and `totals`. **Ids and counts only**: no policy name, filter, or row content. The rows the job changes are **not** audited one by one: a delete entry would keep the deleted data in the audit log.

The entry is a webhook event, `gdpr.retention` (filter `_retentionPolicies:gdpr_retention_run`), with the same `meta`. Changes to a policy are ordinary record changes in the audit log.

## Legal holds

`_legalHolds` (system, superusers only). A hold has a `scope`:

- `subject`: a person (`subject` id and `subjectCollection`, default `users`). It covers every row the **data map** ties to that person, through the subject paths of each collection's entry (the same paths the data export uses). A collection with no entry, or a `none` entry, is not reached by a subject hold.
- `record`: one record (`collection`, `recordId`).

and `reason` (required), `reference` (a case number), `placedBy`, `placedAt`, `liftedAt`, `liftedBy`.

While a hold is active:

- The retention job skips the rows (`held`).
- **Nothing can delete a held record.** A guard on the model delete refuses it with 409: the records API, a cascade, a hook, the job. Erasure (88-4) must also call `core.LegalHeldAmong` before it anonymizes or erases, because an update is not guarded.

```
GET  /api/gdpr/holds[?active=1]
GET  /api/gdpr/holds/{id}
POST /api/gdpr/holds              {scope, subject|collection+recordId, reason, reference?}   fresh passkey
POST /api/gdpr/holds/{id}/lift                                                              fresh passkey
```

Placing and lifting need a **fresh passkey step-up** and write one audit entry each: `gdpr_hold` and `gdpr_hold_lift` (webhooks `gdpr.hold`, `gdpr.hold_lift`), with the hold id, the scope and the ids. **Never the reason or the reference**: a hold can be part of a legal matter. A lifted hold stays in the table as evidence; there is no delete.

The person's own data export lists that a hold existed and when, never its reason. Whether to tell a person about a hold at all is a legal question (spec section 12): ask your lawyer before you change the export.

## Suggested starting points (not shipped, not legal advice)

| Data | Suggested | Policy sketch |
|---|---|---|
| Check-in sessions and logs | 12 months after the event, unless an incident is flagged (place a hold) | `delete`, basis `created`, 12 months |
| Expired prayer requests | archive at expiry, delete 30 days after (below) | two policies |
| Recovery codes | 7 days after use | `delete`, basis `usedAt`, 7 days |
| Invitations | 90 days (already enforced by the invitation job) | none needed |
| Consent events | the local limitation period of your country | `delete`, basis `created`, N years; a lawyer decides N |
| Youth records | N years after the person becomes an adult | `anonymize`, basis `ageOut` |

The job may delete `_consentEvents` although that table is append-only for everything else.

## Prayer requests

See `docs/pastoral-notes.md`. A prayer request gets an `expiresAt` of 90 days (the org setting `prayerExpiryDays`, 7 to 730). Two ordinary policies run it, and an owner creates them like any others:

```json
{"collection": "pastoral_notes", "basis": "expiresAt", "after": 0, "unit": "days", "action": "archive"}
{"collection": "pastoral_notes", "basis": "archivedAt", "after": 30, "unit": "days", "action": "delete", "filter": "archived = true"}
```
