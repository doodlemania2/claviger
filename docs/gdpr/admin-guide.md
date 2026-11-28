# Administrator guide: data protection with Claviger

This guide is for the person in a parish who looks after data protection: the data-protection lead, the parish administrator, or the volunteer who got the job. It tells you which Claviger tool does what, in the order you will need them.

> **This is not legal advice.** Claviger gives you tools. It does not decide what the law requires of your parish. The parts that depend on your country, your denomination and your regulator are listed in [Items for a lawyer](#items-for-a-lawyer). Have a lawyer read them before you rely on this guide.

Companion documents:

- [DPIA template](dpia-template.md): a Data Protection Impact Assessment to fill in.
- [Article 30 template](article-30-template.md): a Record of Processing Activities to fill in.

Each tool has its own technical page. This guide links to them and does not repeat all their detail.

## Who does what

Claviger's roles map to the work like this.

| Role | Who | What they can do in this guide |
|---|---|---|
| Superuser | The person who runs the server, or the parish's technical contact | Everything in the admin UI under **Data protection**: data map, requests, holds, retention. Starts exports and erasures for others. Cannot sign registers, ask for break-glass or approve it |
| Org owner or admin | Parish leadership | Manage members and tags. Own the decision on pastoral notes held back from an export. Receive break-glass notices |
| Registrar (org tag `registrar`) | The parish office | Households, consent records on paper, register entries |
| Safeguarding coordinator (tag `safeguarding`, or owner or admin) | The safeguarding lead | Credentials of volunteers |
| Greeter (tag `greeter`) | Trusted office staff | Issue staff-vouched recovery codes |
| Break-glass holder (tag `breakglass`) | Very few people | Ask for, or approve, emergency access to a sealed note |

Create a tag in the org page (`/orgs/<slug>`) and add it to people. Review who holds `breakglass`, `greeter` and `safeguarding` at least once a year.

The **controller** is normally the parish. The company or person who runs the server for the parish is normally a **processor** and the parish needs a written agreement with them (a data processing agreement). See [Items for a lawyer](#items-for-a-lawyer).

## A first-month checklist

1. Set the encryption key before you store children's or pastoral data ([Encryption](#encryption-and-backups)).
2. Open **Data protection > Data map** and read it. Register every collection your own apps add ([The data map](#1-the-data-map)).
3. Write down your consent purposes and their wording ([Consent](#2-consent)).
4. Decide your retention periods with your lawyer, then create the policies as dry runs ([Retention](#5-retention)).
5. Set the grace period for erasure ([Erasure](#4-erasure)).
6. Set log and audit retention ([Logs and the audit log](#logs-and-the-audit-log)).
7. Complete the DPIA and the Article 30 record from the templates.
8. Run one practice export and one practice erasure on a test person.
9. Write down who answers a request, and the one-month deadline.

## Where things are in the admin UI

Superusers find **Data protection** in the admin menu.

| Screen | Address | Use it for |
|---|---|---|
| Data map | `#/gdpr/data-map` | See and register what holds personal data |
| Data requests | `#/gdpr/requests` | Exports and erasures: list, start, cancel, read results |
| Legal holds | `#/gdpr/holds` | Freeze a person or record against deletion |
| Retention | `#/gdpr/retention` | Policies, previews, runs |
| Consents | `#/consents` | Purposes and the history of one person |
| Households | `#/households` | Households and children |
| Credentials | `#/credentials` | Safeguarding credentials of volunteers |
| Registers | `#/registers` | Permanent registers and chain check |

Org staff who are not superusers use hosted pages instead: `/orgs/<slug>` (members), `/orgs/consents/<slug>` (consent purposes), `/orgs/registers/<slug>` (registers), `/orgs/recovery/<slug>` (recovery codes). People use `/households/mine` and `/credentials/mine` for themselves.

## 1. The data map

**What it is.** A list of every collection and what it holds about people. Export and erasure only reach what is on the list. A collection that holds personal data and is not on the list is missed.

**What to do.**

1. Open **Data protection > Data map**. The built-in entries ship with Claviger and cannot be changed.
2. Open the list of **unmapped collections** (the same as `GET /api/data-map/unmapped`, superuser). Keep it empty.
3. For every collection your own apps add, create an entry: the collection, a treatment, the fields that link a row to a person (subject paths), the personal fields, and, for `retain`, the reason.

| Treatment | On erasure |
|---|---|
| `erase` | Rows are deleted |
| `anonymize` | The personal fields are blanked, the row stays |
| `retain` | Rows are kept, with the stated reason |
| `export` | Left alone, but exported on request |
| `none` | Reviewed, holds no personal data |

Details and field names: [data-map.md](../data-map.md).

**Check:** the data map is also the base for your Article 30 record. The [template](article-30-template.md) has a column for each collection.

## 2. Consent

Consent is only one lawful ground. A parish often relies on others for core records (for example legitimate interests of a religious not-for-profit body, or a legal obligation). Do not ask for consent where another ground applies. Choose the ground with your lawyer, then record it.

**Purposes.** A purpose is something a person can agree to, with its wording, a version and a lawful basis. Create them in the admin UI (**Consents**) or, for org staff, on `/orgs/consents/<slug>`.

- Changing the **text** raises the version. People who agreed to the old text show as outdated until they agree again.
- A purpose with consent records cannot be deleted. Retire it (`archived`).
- Keys are unique across the whole server. Use a prefix such as `<parish>-newsletter`.
- `marketing` purposes can never be granted for a minor. They get a minimum age of 18.

**Who can agree.** An adult agrees for themselves. A guardian agrees for a minor in their household. For people who agree on paper or in person, staff record it with a reference to the paper (`evidenceRef`). For a minor, staff also name the guardian who agreed.

**Withdrawal** is as easy as agreeing. Staff can record a withdrawal for a person who asks in person.

**Proof.** Each grant or withdrawal writes an event that keeps the text shown. The history cannot be edited. Look up a person's history in **Consents** by member id or user id (superuser), or `GET /api/orgs/{id}/consents?member=ID` (org staff).

Details: [consent.md](../consent.md).

**Not built:** a page where people manage their own consents. The routes exist (`GET /api/me/consents`, grant and withdraw).

## 3. Export (access and portability)

A person can ask for a copy of what the system holds about them (GDPR Articles 15 and 20).

| Who | How |
|---|---|
| The person, for themselves | `POST /api/me/export`. A fresh passkey is needed (or the password, when there is no passkey). Three per hour |
| A superuser, for anyone | **Data requests**, or `POST /api/gdpr/subjects/{id}/export` |

What to know:

- The result is one ZIP: `manifest.json`, a `README.txt`, one JSON file per collection, and uploaded files.
- The due date is set 30 days after the request. GDPR allows one month, which can be extended in some cases. Your lawyer confirms the rules for your country.
- The link works **once**, and the file expires after seven days by default (`dataExport.retentionDays`, 0 to 30 days, **Settings > Data export**).
- For an export you made for someone else, you download it and pass it on. The person cannot fetch it through the API. Send it by a safe route.
- Other people's data is left out. A household export shows the person's own row and the shared contact data. A **child** has no login, so a child's row is not in anyone's export through this path. A guardian asking for a child's data needs a manual process. Ask your lawyer how you will answer that request.
- Request logs (`_logs`) are not exported.
- Pastoral notes about a person are **not** exported automatically. See [Pastoral notes](#pastoral-notes-and-break-glass).

Check the **manifest** before you pass the file on. It lists collections that were skipped, errors, and files that were too large to include (the request then ends `partial`).

Details: [data-export.md](../data-export.md).

## 4. Erasure

A person can ask to be erased (GDPR Article 17). Erasure is destructive and cannot be undone. Claviger puts several steps in front of it.

**Who can ask.**

| Who | How |
|---|---|
| The person | `POST /api/me/erasure`. Fresh passkey, then an emailed confirmation |
| A guardian, for a child without a login | `POST /api/me/erasure` with the child's member id |
| A superuser, on the person's behalf | **Data requests > Start erasure**, or `POST /api/gdpr/subjects/{id}/erase`. Needs a fresh passkey |

**The staff path** is for a person with no email. Check who they are first, and write how you checked ("passport seen at the office, 2026-11-14"). That text is kept on the request. Choose the emailed confirmation instead when the person has an address.

**What happens.**

1. Confirmation by email. Only the button on the page confirms, so a mail scanner changes nothing.
2. A **grace period** (14 days by default; set `erasure.graceDays`, 0 to 30; 0 starts the job at once). The account is locked.
3. The person can cancel during the grace period with a passkey sign-in, the cancel link, or you can cancel it.
4. The job works through the data map. It deletes, anonymizes or retains each collection, skipping anything under a legal hold.
5. The person is emailed the result, with what was kept and why, and that they can complain to the authority. The email goes out before the address is anonymized.

**Two refusals** (HTTP 409, nothing is changed):

- `last_guardian`: the person is the only active adult guardian of a household with a minor. Add another guardian first.
- `last_owner`: the person is the only owner of an org. Make someone else an owner first.

**The result.** Read it with `GET /api/gdpr/erasures/{id}` or in **Data requests**. A request ends `done`, `partial` (something was kept) or `failed`.

**What is kept.** Permanent register entries are retained by default (the request ends `partial`). Consent evidence and break-glass records are kept with the person replaced by a pseudonym. Pastoral notes the person wrote are not erased with them. A person under a legal hold is not erased. Read [erasure.md](../erasure.md), section "What each built-in collection gets", before you talk to a person about what will remain.

**Backups.** Erased data stays in backups and replicas until they rotate. Claviger keeps an erasure log and **erases people again after a restore**. After a restore, check `GET /api/gdpr/erasures?state=queued` and the server log. Set backup retention to match your policy and tell people erased data is "beyond use" until backups rotate. Do not delete the stored erasure secret (it lives in the database, so backups carry it).

**The audit log.** One audited function scrubs the person's entries (before and after values, IP, ids). Keeping some entries for a security reason is a lawyer's decision ([Items for a lawyer](#items-for-a-lawyer)). Today nothing is kept.

**Tell others.** Relying parties (apps that sign in through Claviger) keep their own copies. Tell them. For SCIM clients, the member is deactivated first, and you tell the identity provider.

## 5. Retention

Retention is how you keep the promise to delete what you no longer need. A policy names a collection, a date, a period and an action. A daily job (02:27 server time) applies enabled policies.

**Claviger ships no policy.** Choose periods with your lawyer. The table in [retention.md](../retention.md) lists suggested starting points only.

**Create a policy safely.**

1. Create it (Retention screen, or `POST /api/gdpr/retention/policies`). It is always created disabled and as a dry run.
2. Preview it: **Run preview**, or `POST /api/gdpr/retention/run` with `{"preview": true, "policy": "<id>"}`. Read the counts. Nothing changes.
3. Enable it. It is still a dry run.
4. Turn off the dry run. This needs a fresh passkey.

Actions: `delete`, `anonymize` (blanks the personal fields of the data map entry), `archive` (sets `archived = true`).

**What a policy never touches.** Rows under a legal hold, pastoral notes with an open export-hold, permanent registers, accounts (erasure handles those) and the audit log (it has its own setting).

**Youth data.** A policy with basis `ageOut` counts from the day a person stops being a minor (birth date plus the org's graduation age, default 18, set `graduationAge` 14 to 21 in org settings).

**Audit.** Each run writes one `gdpr_retention_run` entry with counts only.

### Legal holds

A hold freezes a person (it reaches every row the data map ties to them) or one record while a legal matter is open.

- Place or lift a hold on **Data protection > Legal holds** or `POST /api/gdpr/holds` and `/lift`. Each needs a fresh passkey.
- While a hold is active, retention skips the rows and **nothing can delete a held record**. Erasure skips them and lists them as `legal_hold`.
- To freeze a child without a login, hold the child's household member row. A subject hold can only be placed on an account.
- The reason and reference are **not** in the audit log or the person's erasure result. The person's own export **does** include their holds with reason and reference (owner decision, 2026-11-14), without the names of staff. If a matter must not be disclosed, take advice before an export runs.

Details: [retention.md](../retention.md).

## Special-category data

Membership of a parish reveals religious belief, which is special-category data under GDPR Article 9. Health data (allergies, medical notes) is special-category data too. Most parishes rely on a specific ground for a religious body's own members (Article 9(2)(d)). That ground limits **disclosure** outside the parish. It matters when you:

- share data with a diocese (the hierarchy features, `docs/orgs.md`),
- connect third-party apps (OIDC, SCIM, webhooks), or
- send data out through webhooks.

Check each connection against your lawyer's advice. Claviger's household collections never produce webhooks and are not in OIDC claims.

**Encrypted fields.** Birth date, address, home phone, allergies and medical notes in households are encrypted fields. Pastoral note text is encrypted. The server must have a key configured.

## Children's data

Claviger's defaults:

- A minor has **no login**. A guardian manages the child's household record. Age rules differ by country, so ask your lawyer before changing this.
- No direct marketing to minors. A marketing purpose cannot be granted for a minor.
- A guardian consents for a child.
- Access to children's data can be tied to a **current safeguarding credential**. When the credential expires, access closes by itself.
- A minor becomes independent at the org's graduation age (default 18). A daily job (06:41) flips `isMinor`, writes `household_graduated`, and guardians lose write access.
- A guardian flag is a church record, not a legal finding. Claviger does not decide custody. Staff can remove a guardian at once.

**What to do.**

- Set the key (below) before children's data goes in. Until the fields are encrypted, the routes refuse to store health data (HTTP 409).
- Choose a graduation age with your lawyer (`graduationAge`, 14 to 21).
- Create credential types for adults who work with children, and a retention period for credential records (credentials are erased after the period your parish sets).
- For `ageOut` retention, test on a copy first.
- For a request by a guardian on behalf of a child, see Export and Erasure above.

Safeguarding credentials store an attestation (type, dates, issuer, who verified it) and **not** the background-check result. Documents are off unless you enable `credentialDocuments` in the org settings. Ask a lawyer what you may keep (FCRA, DBS and local equivalents).

Details: [households.md](../households.md), [credentials.md](../credentials.md).

## Pastoral notes and break-glass

Prayer requests and pastoral care notes are the most sensitive data you hold.

**Sacramental confession must not be stored in Claviger.** Train authors accordingly.

**Set up.** A superuser runs `POST /api/pastoral` with the org. It creates the locked collection, registers it in the data map and creates the tags `clergy`, `care-team`, `breakglass` and `moderator`. It refuses until the encryption key is set.

**Who reads.** The author. Members by visibility level and tag. **An org owner or admin has no automatic right.** A superuser can read through the API, so the **read audit** matters: reads of sealed notes by anyone but the author are logged, including superuser reads. Turn on read audit for other sensitive collections in **Settings > Audit log** (`audit.readCollections`).

**Break-glass.** For emergencies only, two people, never silent.

1. A holder of the `breakglass` tag asks (`POST /api/breakglass`) with a reason of at least 20 characters and a basis (`safety`, `legal`, `owner_unavailable`, `other`). Fresh passkey.
2. A different holder approves. Fresh passkey. A superuser can neither ask nor approve.
3. Before the grant opens, the author and the org owners are emailed with the reason. This cannot be turned off. If nobody can be told, no grant is given.
4. The grant lasts at most 24 hours (`breakglassHours` in the org settings can shorten it). Reads are logged.
5. The author, an org owner or the grantee can revoke it at once.

Reasons are sensitive. Only org owners see them.

**Export and the data subject.** A note **about** a person is not put in their export. It is "held for review". An owner of the org decides: `GET /api/gdpr/pastoral-holds?org=<id>`, then `release` or `withhold` (a reason is required). Whether you must give a person their pastoral notes is a legal question.

**Prayer requests expire** after 90 days by default (`prayerExpiryDays`, 7 to 730). You must create the two retention policies that archive and then delete them. No policy ships enabled. The author can renew.

**Honest limit.** Field encryption protects the database file and backups. It does **not** hide notes from the instance operator, who holds the key. Controls for that are read audit, a key held outside the server (a cloud key vault), and policy. See [the owner-held vault design spike](../spikes/86-6-owner-key-vault.md) for what is not built.

Details: [pastoral-notes.md](../pastoral-notes.md).

## Registers and erasure requests

Sacramental registers are permanent: a final entry cannot be edited, only annotated. They are marked `retain` in the data map with a stated reason, so an erasure request ends `partial`.

For an erasure request on a final entry, the job adds a `data_subject_note` annotation and sets `restricted = true`, so the entry is not shown outside the registrar role. Anonymizing a final entry (with two approvers) is **not built**.

Whether a church register may be kept against an erasure request is a legal question that depends on your country and denomination. Ask your lawyer before you tell anyone the register will be kept. Record the answer in your DPIA.

Registrars finalize and annotate entries on `/orgs/registers/<slug>`. A superuser can verify the hash chain (**Registers > Verify the chain**, `GET /api/registers/_verify`).

Details: [registers.md](../registers.md).

## Recovery and identity checks

Staff-vouched recovery lets a person who cannot use email set up a new passkey. It is a security control **and** an identity check, so treat it carefully.

- Staff check the person in person, then issue a code on `/orgs/recovery/<slug>`. The code is valid for 15 minutes and works once.
- Staff never see or set a password. The code is never sent by message.
- For an owner or admin of any org, two staff are needed.
- Never recover someone because of a phone call or message.
- Each issue writes an audit entry. The export has who vouched, how and when.

The same identity-check discipline applies to erasure and export requests made through staff. Record how you checked.

Details: [recovery.md](../recovery.md).

## Logs and the audit log

| What | Setting | Default | Notes |
|---|---|---|---|
| Request logs (`_logs`) | `logs.maxDays` | 5 days | Hold user id and IP. Not exported. Goes away on this setting |
| Audit log | `audit.retentionDays` (or `CLAVIGER_AUDIT_RETENTION_DAYS`) | 0 (keep forever) | Append-only. Erasure scrubs a person's entries. Choose a period with your lawyer |
| Read audit | `audit.readCollections` | none | Add pastoral and other sensitive collections |

Setting `logs.logIP` to false stops the IP address being logged.

## Encryption and backups

- Set `CLAVIGER_KEK` (or a key file, or a cloud key source) before you store encrypted fields. Keep a copy somewhere that is not the server and not the backups, with two people who know it. **Losing the key loses the data.** See [field-encryption.md](../field-encryption.md).
- Turn on encrypted backups when backups leave the server ([backup-encryption.md](../backup-encryption.md)).
- Test a restore **with** the key before you rely on it.

## Requests: a working routine

1. **Log** the request on the day it arrives. Note the date. The due date is one month later (the export tool sets 30 days).
2. **Check identity.** Ask only for what you need. Do not ask for more data than you hold.
3. **Run the tool.** Export, or erasure with the grace period.
4. **Check the result.** For an export, read the manifest. For an erasure, read the result and the retained list.
5. **Answer the person** in plain words. Say what you gave or erased, what you kept and why, and how to complain to the authority.
6. **Tell others** where needed (relying parties, SCIM identity provider, other processors).
7. **Keep a note** of the request and your decisions, without the personal data itself.

## Breaches

Claviger does not have a breach tool. If personal data is lost or exposed, tell your data-protection lead and your lawyer **at once**. In many countries a notification to the authority is due within 72 hours of becoming aware, and affected people may need to be told. Use the audit log and read audit to find who accessed what. Write down the incident, the effects and what you did. Add the procedure to your DPIA.

## What is not built yet

- Anonymizing a final register entry with two approvers (88-7).
- The register restriction flow (a registrar-driven way to restrict an entry; today only erasure requests set `restricted`).
- A page where people manage their own consents.
- An email when an export is ready.
- A breach tool.
- An owner-held key vault ([design only](../spikes/86-6-owner-key-vault.md)).
- Automatic discovery of personal data in free text.

## Items for a lawyer

These depend on law that differs by country, state and denomination. Claviger's authors did not decide them for you.

1. Which lawful ground covers each kind of data, and whether Article 9(2)(d) fits, including limits on sharing with a diocese or third-party apps.
2. The age of digital consent and parental consent rules for children in your country (GDPR Article 8, COPPA, UK rules).
3. Whether a church register may be kept against an erasure request, and what your regulator says.
4. Retention periods for each kind of record, including consent evidence (limitation period), check-in records, recovery records and the audit log.
5. Background-check data: what you may store and for how long (FCRA, DBS, local rules).
6. Mandatory reporting duties, clergy and counselor privilege, and the break-glass bases and notice.
7. Whether a data subject access response includes pastoral notes, and when.
8. Whether to keep some audit entries after erasure for a security reason, and on what basis.
9. The data processing agreement and the controller and processor roles when someone else runs your server, and where the data is stored.
10. International transfers, if the host or a connected service is outside your country.
11. Whether a data-protection officer is required, and whether a DPIA is mandatory for you.
12. Breach notification duties and the text of notices.
13. The wording of consent texts, privacy notices, the erasure result email and the notice to people about pastoral break-glass.
14. SMS consent and carrier rules, if you send recovery links by text.
15. Court orders about contact or custody that affect how a child's record may be used.
16. Whether you must disclose legal holds in an export.

## Related pages

[Data map](../data-map.md) · [Consent](../consent.md) · [Export](../data-export.md) · [Erasure](../erasure.md) · [Retention and holds](../retention.md) · [Households](../households.md) · [Credentials](../credentials.md) · [Pastoral notes](../pastoral-notes.md) · [Registers](../registers.md) · [Recovery](../recovery.md) · [Audit log](../audit.md) · [Field encryption](../field-encryption.md)
