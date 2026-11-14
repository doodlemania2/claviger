# Pastoral notes and break-glass access

Prayer requests and pastoral care notes are the most sensitive data a congregation holds. Claviger gives them a **template collection** with locked rules, an **audit entry for every read of a sealed note**, and **break-glass access**: time-boxed, two-person, always announced to the people it concerns.

This guide is for operators and developers. The design is in `docs/specs/80-88-congregations.md`, section 8. It builds on organizations (`docs/orgs.md`), field encryption (`docs/field-encryption.md`) and the audit log.

## Sacramental confession is not stored here

**The seal of confession must not be stored in Claviger, in a note, a prayer request, a comment or anywhere else.** Sacramental confession is outside the product. Do not create a note type for it, do not paste what was said in confession into a note, and do not use break-glass to reach anything of the kind. Train note authors accordingly. The template's `body` field is for pastoral care and prayer requests, which are a different thing.

Claviger does not decide clergy-penitent privilege, counselor privilege or mandatory reporting. Those differ by country and state, and a deployment needs a lawyer for each. Break-glass supports a reporting duty; it does not decide one.

## Set up

```
POST /api/pastoral   {"name": "pastoral_notes", "orgs": ["<org id or slug>"]}      # superuser
```

This creates the collection (the name defaults to `pastoral_notes`) with the locked rules below, registers it in the data map, and creates the four tags in each listed org. A tag is an ordinary org tag (`docs/orgs.md`); an org owner can also create them by hand.

| Tag | Meaning |
|---|---|
| `clergy` | Reads notes of visibility `clergy`. |
| `care-team` | Reads notes of visibility `care`. |
| `breakglass` | May ask for, and approve, emergency access. Give it to few people. |
| `moderator` | May archive a note (and change nothing else). |

Fields: `org`, `author`, `subject` (optional, the person the note is about), `visibility` (`private`, `clergy`, `care`, `congregation`), `sealed`, `body` (an `encryptedText` field), `expiresAt` (optional), `archived`.

The `body` encryption needs a key (`CLAVIGER_KEK` and friends, `docs/field-encryption.md`). The app refuses to start without one when an encrypted field exists. So `POST /api/pastoral` refuses (400) until a key is configured.

### The rules are locked

The route refuses to apply any rule that differs from the locked one, except `null` (superusers only, which is stricter). To apply another rule you must say so on purpose: `{"rules": {"listRule": "..."}, "allowLooseRules": true}`. Never apply the org presets (`/api/orgs/rule-templates`) or the ancestor presets of the hierarchy (#84) to this collection: **a parish's owner or admin gets no automatic read right, and a diocese does not read parish pastoral care.**

| Action | Who |
|---|---|
| Create | Any active member of the org, as author. |
| Update | The author. Nobody else changes the text or widens the visibility. A `moderator` may set `archived = true` and nothing else. |
| Delete | The author. |
| Read | See the table below. |

## Who can read a note

| Reader | Sees the text? | Why |
|---|---|---|
| The author | Yes | Rule `author = me`. |
| A member, note `congregation` | Yes | Any active member of the org (not sealed, not archived). |
| A `care-team` member, note `care` | Yes | The tag, on the same membership row as the member rule. |
| A `clergy` member, note `clergy` | Yes | Same. |
| Anyone, note `private` or `sealed` | No | Author only. The tags do not help. |
| Org owner or admin | **No**, until break-glass | There is no owner or admin clause. Verified by tests. |
| An admin of a parent org | No | Pastoral notes get no ancestor or oversight preset. |
| A break-glass grantee | Yes, while the grant is live | The grant clause. Every read is logged. |
| A superuser (the instance operator) | **Yes** | Superusers bypass rules, and the server holds the key. Their reads of sealed notes are logged too. |
| Someone with only a database file or a backup | No | Field encryption: ciphertext only. |
| Someone with a backup and the key | Yes | Key control. |

Field encryption gives **protection at rest**, not protection from the operator. "Unreadable to admins" means org admins, and the rules deliver that. For the operator the controls are the read audit, a key held outside the server (Azure Key Vault source) and policy. An owner-held key vault (the note is encrypted in the browser) is a later option and is not built.

An archived note leaves everyone but the author. Realtime subscriptions never deliver a sealed note to anyone but its author; fetch it through the records API, where the read is logged.

## Break-glass

Break-glass is for an emergency, such as a person at risk when the author cannot be reached. It is **always two people** and **never silent**.

1. **Ask.** A holder of the `breakglass` tag in the note's org calls `POST /api/breakglass` with `collection`, `recordId`, `reason` (at least 20 characters), `basis` (`safety`, `legal`, `owner_unavailable`, `other`) and optionally `hours`. It needs a fresh passkey. This only records a request; it opens nothing. A request waits 24 hours for a decision.
2. **Approve.** A **different** holder of the tag calls `POST /api/breakglass/{id}/approve`, also with a fresh passkey. Approving your own request is refused. A superuser can neither ask nor approve.
3. **Tell.** Before the grant opens, Claviger emails the note's author and the org owners with the reason, the person who asked, the approver and the expiry. If the author has no usable address, the other holders of the tag are told as well. **This notice cannot be turned off, and there is no deferred or silent mode.** If nobody can be reached, the grant is not given (HTTP 503).
4. **Read.** The grantee reads the note through the normal records API. Every read is logged and counted.
5. **End.** The grant ends by the clock, or earlier when the org owner, the note's author or the grantee calls `POST /api/breakglass/{id}/revoke`. It is also revoked at once when the grantee is suspended or removed from the org.

**Length.** A grant lasts at most **24 hours**. An org can shorten it with the `breakglassHours` key of its settings (1 to 24). A larger value is ignored: an org can never lengthen it.

**Reasons are sensitive.** They sit in the grant and in the audit entry. `GET /api/breakglass?org=<id>` shows reasons to org owners (and each person their own); holders of the tag see the list without them. Webhook payloads never carry the reason.

### Audit

| Action | When | `meta` |
|---|---|---|
| `breakglass_request` | A request is made. | `orgId`, `noteId`, `collection`, `grantId`, `grantee`, `reason`, `basis`, `hours` |
| `breakglass_open` | The second person approves. | the same, plus `expires`, `approvedBy`, `notified` |
| `breakglass_revoke` | A grant is revoked. | the same, plus `revokedBy`, `cause` (`manual` or `membership_ended`) |
| `read` | Someone other than the author reads a **sealed** note (one entry per request). | `collection`, `recordIds`, `orgIds`, `grantId`, `grantIds`; `superuser: true` when it is a superuser |

Each `breakglass_*` entry is also a webhook event (`breakglass.request`, `breakglass.open`, `breakglass.revoke`; filter with `_breakGlass:breakglass_open`), without the reason. The records of `_breakGlass` are not audited as record changes; the explicit entries describe each step once.

The `read` entry is written before the response, so a failure to log stops the read. Reads through the records API are covered (view and list). Reads that go around it (raw SQL, Go code on the app) are not.

## A note about a person and their data export

A note whose `subject` is a person is **not** put in that person's automatic data export, because it can describe other people. The export records it in `_pastoralHolds` as **held for review** and says only how many are held (the manifest key `held`). An **owner of the note's org** reviews and decides:

```
GET  /api/gdpr/pastoral-holds?org=<id>[&state=held]    # owner of the org
GET  /api/gdpr/pastoral-holds/{id}                     # the hold with the note; a logged read
POST /api/gdpr/pastoral-holds/{id}/release   {"note": "..."}   # included in the person's NEXT export
POST /api/gdpr/pastoral-holds/{id}/withhold  {"note": "reason"}  # the reason is required
```

Each step is audited (`pastoral_hold`, `read`, `pastoral_release`, `pastoral_withhold`). A superuser does not decide this and neither does an org admin; it is the owner's call. Whether, and when, a church must give a person their pastoral notes is a legal question: see the items for a lawyer in the spec, section 12.

## For a lawyer

Review before you rely on this: the default retention of pastoral notes, the break-glass bases, the contents of the notice, and what the export may hold back, in your jurisdiction.
