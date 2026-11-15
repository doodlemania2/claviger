# Erasure

A person can ask to be erased (GDPR Article 17). This is the guide for operators and developers. The design is in `docs/specs/80-88-congregations.md`, section 10.6, and it builds on the data map (`docs/data-map.md`), legal holds (`docs/retention.md`), permanent registers (`docs/registers.md`), consents (`docs/consent.md`) and households (`docs/households.md`).

**Erasure is destructive and cannot be undone.** That is why it has a confirmation by email, a grace period, legal holds, a dry look at what the data map covers, and an erasure log. Nothing here is legal advice: whether a record may be kept against a request is a question for your lawyer (see [For a lawyer](#for-a-lawyer)).

## Who can ask

| Who | How | Needs |
|---|---|---|
| A signed-in person, for their own account | `POST /api/me/erasure` | A fresh passkey (or the password when the account has no passkey), then the emailed confirmation |
| A guardian, for a child of their household | `POST /api/me/erasure` with `{"member": "<household member id>"}` | The same, and the guardian is the one emailed. The child has no login and cannot ask for themselves |
| A superuser | `POST /api/gdpr/subjects/{id}/erase` | A fresh passkey. Either the identity check (below) or the emailed confirmation of the person |

A guardian is an **active adult guardian of the child's household** (the same rule as for consent, `docs/consent.md`). They can ask for a child who is a **minor without an account**. A child who has graduated or has a login is a person who asks for themselves. A guardian cannot ask for another adult.

**The staff path (no email).** A person without a usable email address cannot confirm by link. A superuser starts the erasure after checking who the person is, and records how in the request: `{"identityCheck": "Passport seen at the office, 2026-11-14"}`. It is kept in `meta.identityCheck` of the request and in nothing else. With `{"requireConfirmation": true}` the person is emailed the link instead, and the identity check is optional. API keys are refused on every erasure route. A superuser account cannot be erased with this tool.

## What happens

1. **Request.** The request is a row of `_dataRequests` with `type` `erase` (`docs/data-export.md` has the export rows of the same table). Its state is `awaiting_confirmation`. The person is emailed a link valid for 7 days. Opening the link shows a page with a button; **only the button (a POST) confirms**, so a mail scanner that opens the link changes nothing. An unused link expires and the request becomes `expired`.
2. **Confirmation and grace.** After the confirmation the state is `grace` for the **grace period**: 14 days by default, set in the settings as `erasure.graceDays` (0 to 30; 0 starts the job at once). The account is **locked**: a ban with the reason `pending_erasure` (sessions are revoked, API keys stop, the token key is rotated). An account that already has a ban of another kind keeps it, and a cancel never lifts it. A child without a login is not locked, and the guardian is not locked out. The person is emailed the date and a link to cancel.
3. **Cancel.** Until the job starts, the request can be cancelled by the cancel link, by a **passkey sign-in** of the locked account (the sign-in is the proof, and it cancels the erasure), by the person from `POST /api/me/erasure/{id}/cancel` (a fresh passkey or the password), or by a superuser (`POST /api/gdpr/erasures/{id}/cancel`). Cancelling lifts the lock and erases nothing.
4. **The job.** When the grace period ends the request is `queued` and the job runs (see [The job](#the-job)). The state ends `done`, or `partial` when something was kept, or `failed`.
5. **The result.** The person is told by email (Article 12(4)): counts, what was kept and why, and that they can complain to the data protection authority. The email is sent **before the address is anonymized**. A person without a usable address is not emailed (`result.notice` says `no_email`), and the administrator reads the result in `GET /api/gdpr/erasures/{id}`.

A person has one open erasure at a time. Starting one with the data map or the households in a state that makes it unsafe is refused at once, and again when the job starts: see [Refusals](#refusals).

## What each built-in collection gets

This is what the job does for the data map entries that ship with Claviger (spec 10.3).

| Collection | Treatment |
|---|---|
| The account (`users`, or the app's auth collection) | **Anonymized.** Email `erased-<id>@invalid`, the personal fields of its data map entry blanked (`name`, `avatar`), `username` replaced, email hidden, unverified, a random password, tokens rotated. The row stays (other rows point at it) and a permanent ban `erased` stops sign-in |
| `_externalAuths`, `_webauthnCredentials`, `_totps`, `_mfas`, `_otps`, `_sessions`, `_authOrigins`, `_apiKeys` | **Erased** |
| `_oauthConsents`, `_oauthTokens`, `_oauthRefreshTokens`, `_oauthRequests`, `_oauthSso`, `_oauthDeviceCodes` | **Erased.** Relying parties cannot refresh. They keep their own copies: tell them (a SCIM delete helps for SCIM clients) |
| `_bans` | The `pending_erasure` lock is turned into the permanent `erased` ban |
| `_orgMembers`, `_invitations` (by invited address, by inviter and by acceptor) | **Erased.** Memberships that came from SCIM are suspended first |
| `_orgTransfers` | **Erased** (a transfer of the account, or of the person's household row) |
| `_credentials`, `_recoveryCodes` | **Erased** |
| `_householdMembers` | The person's row is **anonymized**: name, birth date, photo, allergies, medical notes and login blanked, `guardian` cleared, status `left`. It is never deleted: other records point at it |
| `_households` | **Left alone**: the shared contact data belongs to the other members too |
| `_consents`, `_consentEvents` | **Pseudonymized** (`core.PseudonymizeConsents`): kept as evidence of what was agreed, with the person replaced by `erased-<hash>`. See [Pseudonyms](#pseudonyms) |
| `_breakGlass` | **Kept, the person pseudonymized** (grantee, approver, revoker). The reason, time and note stay: it is the record of who opened the most sensitive notes |
| `_legalHolds` | An active hold is kept as it is. A lifted hold is kept with the person pseudonymized |
| `_pastoralHolds` | **Erased** (the holds about the person) |
| Pastoral notes about the person | **Kept for their author, unlinked** from the person (`core.UnlinkPastoralSubject`). Notes the person wrote are never erased with them |
| `_auditLog` | **Scrubbed**: see [The audit log](#the-audit-log) |
| `_webhookDeliveries` | **Erased** when the payload names the person |
| `_dataRequests` | The person's export requests are erased with their files. Earlier erasure requests keep their record and lose the person (pseudonym) |
| Permanent registers (#87) | **Retained.** See [Registers](#registers) |
| App collections registered in the data map | By the entry: `erase` deletes the rows, `anonymize` blanks the entry's personal fields (no personal fields: the rows are kept and listed), `retain` keeps them with the stated reason, `export` leaves them |
| `_logs` | Not a collection. It goes with `logs.maxDays` |

The job reads the **data map**, not a list in code. A collection that holds personal data and is not in the map is **not** reached: keep `GET /api/data-map/unmapped` empty.

### Registers

A permanent register entry (a baptism, say) is **retained** by default and the erasure ends `partial` with the reason (spec 10.7). For each **final** entry about the person the job adds a `data_subject_note` annotation to the entry (`core.AnnotateErasureRequest`) and sets `restricted = true`, so it is no longer shown outside the registrar role. Nothing else of the entry changes. A **draft** entry is not final: it is kept and listed, without an annotation, for a registrar to decide. Anonymizing a final entry (with two approvers) is the later milestone 88-7 and is not built.

### Legal holds

Before it deletes or anonymizes any row the job calls `core.LegalHeldAmong`. A row under a hold on that record, or on the person (a subject hold reaches the rows the data map ties to them), is **skipped** and listed in the result as `legal_hold`. The hold's reason and reference are never copied into the result or the email: a hold can be part of a legal matter. A held account is not anonymized and is unlocked again. A subject hold can only be placed on an account; to freeze a child without a login, place a hold on their `_householdMembers` row.

## The job

One erasure runs at a time per server process, in the serving process (single writer). It works through steps, and each step is idempotent. After every step it stores its progress on the request row: the steps done, the ids of the person's household rows (the job clears their login itself), the rows touched and the partial result. A run that is cut short (a crash, a restart, an error) goes back to `queued` and **continues where it stopped**; a step is never done twice, and the person is never told twice. After 5 failed attempts the request is `failed` with the error. A failed attempt is retried at the next pass (the job runs every 10 minutes and at start).

The steps, in order: `init`, `log`, `lock`, `scim`, one `map:<collection>` per data map entry (the household rows last), `pastoral`, `notice`, `subject`, `ban`, `audit`, then the final entry and the state.

The job's own writes do not create record-change audit entries (their before and after values would hold exactly what is being erased). Instead the log has the request entries and one `gdpr_erase` entry.

### Refusals

Two things are refused, before anything is changed, with a code in the response `data.code`. The state may change during the grace period, so the job checks again.

- **`last_guardian`** (409): the person is the only active adult guardian of a household that has an active minor. Erasing them would leave a child with no guardian. Add another adult guardian to the household first (`docs/households.md`), or move the child. `data.ids` names the households.
- **`last_owner`** (409): the person is the only active owner of an organization. Make someone else an owner first, or archive the organization. `data.ids` names the orgs.

At run time a refusal ends the request `failed` with `result.refused`, unlocks the account and emails the person. Nothing is erased.

## The audit log

The audit log is append-only. Erasure needs to reach the personal values in it, so there is **one documented exception**: `core.ScrubAuditForErasure`. For the entries about the person it:

- empties `before`, `after` and `ip`;
- replaces the person's id (as actor and as target) with the pseudonym;
- reduces `meta` to the org ids (so an org still finds its entries) and a scrubbed marker;

and **keeps** the action, the time, the collection names and the names of the changed fields. An entry is "about" the person when the actor or the target is one of their ids (the account and its household rows), when the target is a row the erasure itself deleted or anonymized, or when `before`, `after` or `meta` mention one of their ids or an email address they had. The scrub is idempotent.

How the append-only guard lets it through: the guard on `_auditLog` refuses every update and delete, except an update that carries a context key only this function can create (an unexported type) **and** changes no column but `before`, `after`, `ip`, `meta`, `actorId` and `targetId`. A route, a hook or a script cannot build the key, and even code in the package cannot use it to rewrite an action or a time. Direct SQL is outside the guarantee, as for the rest of the log. See `docs/audit.md`.

The job then writes the person's own **`gdpr_erase`** entry (a normal, append-only entry): the request id, the pseudonym, the status and the counts. It is delivered as the webhook event `gdpr.erase` with the same meta: ids and counts only, never content.

## Pseudonyms

`core.ErasurePseudonym(app, id)` (the same as `core.ConsentPseudonym`) is `erased-` and 24 hex characters of an **HMAC-SHA-256 of the id under a per-install secret**. The same person gets the same pseudonym everywhere in one installation (audit entries, consents, break-glass grants), so the evidence can still be read as "one person". Without the secret the pseudonym cannot be computed from an id, so a webhook receiver or a relying party that holds an old id cannot link it to the evidence that stays. It is still pseudonymization, not anonymization: the installation itself can recompute it. The result says what was kept.

**The secret.** 32 random bytes, made the first time an erasure needs it and kept in the `_params` table of `data.db` under the key `erasureSecret`. It is not a collection, so no records route, collection export, data export, settings response or audit entry returns it, and it is never written to the log. It is **not** derived from `PB_ENCRYPTION_KEY` or the KEK (both are optional). Because it lives in the database it travels with every backup and restore, which the replay needs. Do not delete it: pseudonyms and log hashes made under a lost secret cannot be matched again.

## The erasure log and restores

Backups and replicas are not rewritten: erased data stays in them until they rotate. Set the backup retention against your policy and tell people that erased data is "beyond use" until then.

`_erasureLog` has one row per erased person: a keyed hash (`HMAC-SHA-256` under the secret above of `"claviger-erasure-log:" + collection + ":" + id`), the date and the names of the collections touched. Nothing in it can be turned back into a person. When the server starts it looks through the ids of the accounts and household rows, finds any whose hash is in the log and that are no longer anonymized (a restore of an older backup brought them back), and **erases them again** with a request of mode `replay` (no grace period, no email, the same holds and refusals). A person who is already erased is not found, so the replay is idempotent. **If the secret is missing** (a database restored without its `_params` row) the replay is skipped and a WARN is logged: a different key would match nobody, or the wrong people. A replay never creates the secret; a new erasure does. The log counts replays. The log is written only by the job: the records API refuses to create, change or delete a row, and refuses to create or change an erasure request (a superuser could otherwise skip the grace period by editing a state).

After restoring a backup, check `GET /api/gdpr/erasures?state=queued` and the server log for "found people who are back after a restore".

## Settings and routes

Setting: `erasure.graceDays` (null means 14; 0 to 30).

```
POST /api/me/erasure                 {member?, password?}  the person or a guardian; step-up; emails the link
GET  /api/me/erasure                 my requests, and those I made as a guardian
POST /api/me/erasure/{id}/cancel     step-up
POST /api/gdpr/subjects/{id}/erase   superuser, fresh passkey: {collection?, identityCheck?, requireConfirmation?, graceDays?}
GET  /api/gdpr/erasures[?subject=&state=]   superuser
GET  /api/gdpr/erasures/{id}         superuser
POST /api/gdpr/erasures/{id}/cancel  superuser, fresh passkey
POST /api/gdpr/erasure/confirm       {token}   the emailed link, no sign-in
POST /api/gdpr/erasure/cancel        {token}
GET/POST /erasure/confirm, /erasure/cancel     the pages the emailed links open
```

A person's view of a request hides the ids of kept rows and the hold ids; a superuser sees them.

Audit actions: `gdpr_erase_request`, `gdpr_erase_confirm`, `gdpr_erase_cancel`, `gdpr_erase`. Emails: `email.erasureConfirm.*`, `email.erasureGrace.*`, `email.erasureResult.*` in the catalog (English is the source; the other languages are drafts until a native speaker reviews them).

## In the admin UI

The admin UI has a **Data protection** section (superusers). **Data requests** (`#/gdpr/requests`) lists the exports and erasures; for an erasure a superuser can view it with its result, **cancel** one that has not started (it needs a fresh passkey, like the API route), and **start** an erasure for a person, with the identity check text or the emailed confirmation. The **grace period in days** (`erasure.graceDays`, 0 to 30) can be given for one erasure on that form, and the default grace period is set on the same page. The other pages are the data map (`#/gdpr/data-map`), legal holds (`#/gdpr/holds`) and retention policies (`#/gdpr/retention`). Every screen uses the routes described here.

## For a lawyer

- **Which audit entries may be kept** for a security reason (spec open question 18). The scrub keeps nothing: every entry about the person loses `before`, `after` and the IP. Keeping some entries (failed sign-ins, ban decisions) under a legitimate-interest basis is a decision for a lawyer, who decides the list and the basis. The function would then take an exclusion list; it does not today.
- Whether a church register may be kept against an erasure request, in your country and denomination (spec 10.7, item 3 of section 12).
- How long consent evidence and the break-glass record may be kept after erasure (item 7).
- The text of the email that tells the person about the result and their right to complain.

## Not built

- Anonymizing a final register entry with two approvers (88-7).
- Per-collection hold reasons in the result (they are left out on purpose).
- An outbound SCIM client. Claviger is the SCIM *target*: the memberships that came from SCIM are suspended before they are removed, and an administrator tells the identity provider.
