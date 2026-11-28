# Data Protection Impact Assessment (DPIA) template

> **This is a template, not legal advice.** Claviger's authors are not your lawyer. Whether you must do a DPIA, what it must contain and who must review it depend on your country and regulator. Have a lawyer review the finished document. See [Items for a lawyer](admin-guide.md#items-for-a-lawyer).

A DPIA is a written assessment of risks to people from a way you use their data. Many regulators expect one when you process special-category data (such as religious belief) or children's data at scale. Check with your lawyer whether it is required for you.

How to use this page:

- Text in plain type is pre-filled from what Claviger stores and does. Check it against your own setup. It describes Claviger as shipped, not your deployment.
- `[ ... ]` is a blank for you to fill in.
- Delete what does not apply. Add what you do beyond Claviger.
- Date it, sign it, and review it at least once a year and when something changes.

Related: [Administrator guide](admin-guide.md), [Article 30 record template](article-30-template.md).

## 1. Summary

| | |
|---|---|
| Parish or organization (the controller) | [ ... ] |
| Data-protection lead or officer | [ ... ] |
| Who runs the server (the processor) | [ ... ] |
| Data processing agreement signed | [ yes / no, date ] |
| Date of this assessment | [ ... ] |
| Version | [ ... ] |
| Next review date | [ ... ] |
| Decision | [ proceed / proceed with measures / do not proceed ] |

Short description of what you use Claviger for:

[ for example: sign-in for parish members, household records, volunteer safeguarding credentials, pastoral care notes, sacramental registers ]

## 2. Why a DPIA is needed

Tick what applies.

- [ ] Special-category data (religious belief, health) is processed. Membership of a parish reveals religious belief.
- [ ] Children's data is processed.
- [ ] Pastoral notes about people's lives and circumstances.
- [ ] Safeguarding and background-check information about volunteers.
- [ ] Systematic records of identity and sign-in (passkeys, sessions, IP addresses).
- [ ] Data is shared with a diocese or other bodies.
- [ ] Other: [ ... ]

Regulator's list of cases that need a DPIA, and the rule that applies to you: [ ... ]

## 3. Description of the processing

### 3.1 Nature

Claviger is a server that stores the records below, lets people sign in with passkeys, and runs the data-protection tools (export, erasure, retention, consent). Data goes in when people register, when staff enter it, through imports and SCIM, and through apps you connect.

How data is collected: [ web forms / paper entered by staff / import from [ ... ] / SCIM from [ ... ] / other ]

Where it is stored: [ server location, country, host ]. Backups: [ where, how long, encrypted yes/no ]

### 3.2 Scope: what data

| Category | Examples in Claviger | Special category? | Where encrypted |
|---|---|---|---|
| Account and sign-in | Email, name, avatar, passkey names and times, sessions with device label and IP, API keys, OAuth tokens | No | Passkey public keys and secrets are never exported. Not field-encrypted. Hashes for secrets |
| Membership | Organization, role, tags, status, invitations | **Yes** (membership of a religious body reveals belief) | Database. Not field-encrypted |
| Household | Household name, address, home phone | No | Address and phone are encrypted fields |
| People in a household | Name, role, birth date, photo, guardian flag, minor flag | Children's data | Birth date, allergies, medical notes encrypted. Photo is a stored file |
| Health data | Allergies, medical notes | **Yes** (health) | Encrypted fields |
| Safeguarding credentials | Credential type, dates, issuer, who recorded and verified it. Optional document | Not a check result, but sensitive | Database. Documents are off by default |
| Consent | Purpose, version, text shown, time, who gave it, method, evidence reference | Depends on purpose | Database. Append-only events |
| Pastoral notes and prayer requests | Body, visibility, author, subject, expiry | **Yes** (belief, health, personal life) | Body is an encrypted field |
| Break-glass grants | Reason, basis, grantee, approver, time | Sensitive | Database. Reasons visible to org owners only |
| Sacramental registers | Entries and annotations (baptism, confirmation, marriage) | **Yes** | Database. Permanent by design |
| Recovery codes | Who vouched, how, when. Hash of the code | No | Hash only |
| Audit log | Actor, IP, action, before and after values of records (encrypted fields redacted) | May contain any of the above | Database |
| Request logs | User id, IP, request | No | Database, kept `logs.maxDays` (5 days default) |
| Webhook deliveries | Event type and payload | May contain any | Database |
| Your own collections | [ ... ] | [ ... ] | [ ... ] |

Source of truth: the **data map** (`Data protection > Data map`). Attach an export of it: [ date ].

Number of people (estimate): adults [ ... ], children [ ... ], volunteers [ ... ], staff [ ... ].

How long data is kept: see 3.5.

### 3.3 Context

- Relationship with people: [ members, families, volunteers, visitors ]
- Would people expect this use? [ ... ]
- Are children or vulnerable adults involved? [ yes ] Details: [ ... ]
- Is the processing new or unusual? [ ... ]
- Technology: passkeys, field encryption (AES-256-GCM), optional encrypted backups, an audit log, a read audit for flagged collections.

### 3.4 Purposes

| Purpose | Lawful ground (Art. 6) | Special-category condition (Art. 9) | Notes |
|---|---|---|---|
| Run the membership | [ ... ] | [ for example 9(2)(d), by your lawyer ] | |
| Care for children in parish activities | [ ... ] | [ ... ] | |
| Safeguarding volunteers | [ ... ] | [ ... ] | |
| Pastoral care | [ ... ] | [ ... ] | |
| Sacramental records | [ ... ] | [ ... ] | |
| Security and audit | [ ... ] | n/a | |
| [ ... ] | [ ... ] | [ ... ] | |

### 3.5 Retention

Claviger ships **no retention policy**. Fill in what you will configure.

| Data | Period | How it is enforced |
|---|---|---|
| Request logs | [ `logs.maxDays`; default 5 ] | Setting |
| Audit log | [ `audit.retentionDays`; default 0 = forever ] | Setting |
| Pastoral prayer requests | Expire after [ 90 default / `prayerExpiryDays` 7 to 730 ]; archived, then deleted after [ ... ] days | Two retention policies you create |
| Pastoral care notes | [ ... ] | [ policy / author deletes ] |
| Recovery codes | Expire in 15 minutes by default. Rows deleted 30 days after expiry | Built in |
| Invitations | 90 days | Built in |
| Export files | Expire after [ 7 default ] days | Built in (`dataExport.retentionDays`) |
| Credential records | [ ... ] | [ ... ] |
| Consent events | [ the local limitation period ] | [ policy ] |
| Check-in records (if used) | [ ... ] | [ ... ] |
| Youth data after adulthood | [ N years after graduation age ] | `ageOut` policy |
| Registers | Permanent, with the reason: [ ... ] | Built in |
| Backups | [ keep N, rotate every N days ] | Backup settings |

### 3.6 Who can access

| Who | Access | Control |
|---|---|---|
| The person | Own data, their household | Passkey, rules |
| Guardians | Children of their household | Rules; flag removable by staff |
| Org owners and admins | Members and settings. **Not** sealed or private pastoral notes | Roles. Locked pastoral rules |
| Registrars, greeters, coordinators | Their areas (households and consents, recovery, credentials) | Org tags |
| Care team and clergy | Notes at their visibility level | Org tags |
| Break-glass holders | One note, after two people agree, up to 24 hours | Two-person approval, owner notice, read audit |
| Superusers (server operator) | Everything through the admin API, **including pastoral notes** (the server holds the key) | Read audit logs their reads of flagged and sealed collections. Policy and the agreement with the host |
| Diocese or parent org | Only what you enable (opt-in oversight rules). Never pastoral notes | Hierarchy settings |
| Connected apps | What their scopes allow (OIDC, SCIM, webhooks) | Scopes, consent, per-client review |
| The host or other processors | [ ... ] | Data processing agreement |

Named people and numbers: [ ... ]

### 3.7 Recipients and transfers

| Recipient | What they get | Country | Safeguard |
|---|---|---|---|
| [ host ] | [ ... ] | [ ... ] | [ ... ] |
| [ email or SMS provider ] | Address or number, message | [ ... ] | [ ... ] |
| [ diocese ] | [ ... ] | [ ... ] | [ ... ] |
| [ connected apps ] | [ ... ] | [ ... ] | [ ... ] |

## 4. Consultation

| Who | When | Their view |
|---|---|---|
| Data-protection lead | [ ... ] | [ ... ] |
| Pastor / priest | [ ... ] | [ ... ] |
| Safeguarding lead | [ ... ] | [ ... ] |
| People affected or their representatives | [ ... ] | [ ... ] |
| The host / processor | [ ... ] | [ ... ] |
| Regulator (prior consultation, if a high risk stays) | [ ... ] | [ ... ] |
| Lawyer | [ ... ] | [ ... ] |

## 5. Necessity and proportionality

For each purpose answer: do we need this data, and could we do it with less?

| Question | Answer |
|---|---|
| Is each field needed? Which can be dropped? | [ ... ] |
| Credentials: attestation only, no documents? | [ Claviger default: attestation only; documents off ] |
| Children: no minor logins, no marketing? | [ Claviger default: minors have no login; no marketing for minors ] |
| Pastoral notes: how are authors told to write less? Is confession excluded? | [ ... ] |
| How do people see and correct their data? | [ the "My household" page, passkey sign-in, staff ] |
| How do people export and erase? | [ `POST /api/me/export`, `POST /api/me/erasure`, staff path ] |
| How do you keep data accurate? | [ ... ] |
| Information given to people (privacy notice) | [ link and date ] |
| Processor agreement and instructions | [ ... ] |
| International transfers | [ ... ] |

## 6. Risks to people

Rate each risk: likelihood (low / medium / high) and severity (low / medium / high). Starting list below. Edit it.

| # | Risk to people | Likelihood | Severity | Notes |
|---|---|---|---|---|
| R1 | Database or backup is stolen | [ ... ] | [ ... ] | Pastoral text, health data, addresses are encrypted fields. Other fields are not |
| R2 | The encryption key is leaked, or lost (data unreadable) | [ ... ] | [ ... ] | Key kept off the server and off the backups. Losing it loses the data |
| R3 | Server operator or an administrator reads pastoral notes | [ ... ] | [ ... ] | Superusers can read. Reads of sealed notes are logged. Owner-held keys are not built |
| R4 | Misuse of break-glass by a trusted person | [ ... ] | [ ... ] | Two-person, notice, 24-hour limit, read audit |
| R5 | Membership list reveals religious belief to others | [ ... ] | [ ... ] | Roles, tags, connected apps, diocese sharing |
| R6 | A child's data is seen by an unsuitable adult | [ ... ] | [ ... ] | Credential gate, guardian flags, staff review |
| R7 | A guardian flag is wrong (custody, court order) | [ ... ] | [ ... ] | Flag is a church record. Staff can remove at once |
| R8 | Wrong person recovers an account (impersonation) | [ ... ] | [ ... ] | Staff-vouched recovery, two staff for owners and admins |
| R9 | A request for erasure or export is missed or late | [ ... ] | [ ... ] | Request table with due dates |
| R10 | Erased data returns from a backup | [ ... ] | [ ... ] | Erasure log and re-erasure after restore |
| R11 | Data kept too long | [ ... ] | [ ... ] | No policy by default. You must set them |
| R12 | Export contains another person's data | [ ... ] | [ ... ] | Own rows only, other members left out. Review your own collections |
| R13 | A connected app or webhook receives more than needed | [ ... ] | [ ... ] | Scopes, redacted payloads |
| R14 | Background-check information is kept too long or shared | [ ... ] | [ ... ] | Attestation only |
| R15 | Data subject cannot exercise rights (a child, no email) | [ ... ] | [ ... ] | Staff path, guardian path |
| R16 | Breach not noticed or not reported in time | [ ... ] | [ ... ] | No breach tool. Procedure is yours |
| R17 | Register entry kept against a lawful erasure request | [ ... ] | [ ... ] | Legal view needed |
| R18 | Pastoral notes about third parties | [ ... ] | [ ... ] | Held for review in exports; train authors |
| R19 | [ ... ] | [ ... ] | [ ... ] | |

## 7. Measures

For each risk, say what reduces it, what remains, and who owns it.

| Risk | Measure | Built into Claviger? | Residual risk | Owner | Done |
|---|---|---|---|---|---|
| R1 | Key set; encrypted backups on; backups kept off-site | Yes (you must configure) | [ ... ] | [ ... ] | [ ... ] |
| R2 | Key copies kept by two people in separate places; restore tested | No (procedure) | [ ... ] | [ ... ] | [ ... ] |
| R3 | Read audit on; key in a cloud key vault; contract with host; limit who is a superuser | Partly | [ ... ] | [ ... ] | [ ... ] |
| R4 | Few holders; yearly review; reasons reviewed | Yes (two-person, notice, audit) | [ ... ] | [ ... ] | [ ... ] |
| R5 | Roles and tags reviewed; no unnecessary connections | Yes | [ ... ] | [ ... ] | [ ... ] |
| R6 | Credential-gated access; yearly review of guardians | Yes | [ ... ] | [ ... ] | [ ... ] |
| R7 | Process for court orders; staff can remove a guardian | Partly | [ ... ] | [ ... ] | [ ... ] |
| R8 | Recovery checklist; yearly review of `greeter` tag | Yes | [ ... ] | [ ... ] | [ ... ] |
| R9 | Named person; log of requests | Partly | [ ... ] | [ ... ] | [ ... ] |
| R10 | Backup retention set; check queued erasures after any restore | Yes | [ ... ] | [ ... ] | [ ... ] |
| R11 | Retention policies created and tested with dry runs | Tool yes, policies no | [ ... ] | [ ... ] | [ ... ] |
| R12 | Review each collection you add; unmapped list kept empty | Partly | [ ... ] | [ ... ] | [ ... ] |
| R13 | Review each connection before enabling | Partly | [ ... ] | [ ... ] | [ ... ] |
| R14 | No documents; retention period set | Yes (default) | [ ... ] | [ ... ] | [ ... ] |
| R15 | Staff path and guardian path written down | Yes | [ ... ] | [ ... ] | [ ... ] |
| R16 | Written breach procedure; contact for the regulator | No | [ ... ] | [ ... ] | [ ... ] |
| R17 | Lawyer's view recorded | No | [ ... ] | [ ... ] | [ ... ] |
| R18 | Author training; owner review of held notes | Partly | [ ... ] | [ ... ] | [ ... ] |
| R19 | [ ... ] | [ ... ] | [ ... ] | [ ... ] | [ ... ] |

## 8. Outcome

| | |
|---|---|
| High residual risks that remain | [ ... ] |
| Prior consultation with the regulator needed? | [ yes / no, reason ] |
| Measures approved by | [ name, date ] |
| Residual risks accepted by | [ name, date ] |
| Data-protection lead's advice | [ ... ] |
| Was the advice followed? If not, why | [ ... ] |
| Lawyer's review | [ name, date ] |

## 9. Review log

| Date | Reason (annual, new feature, incident, change of host) | Changes | By |
|---|---|---|---|
| [ ... ] | [ ... ] | [ ... ] | [ ... ] |

## Items for a lawyer

Ask your lawyer to review at least: the lawful grounds and the Article 9 condition for each purpose, retention periods, the age of consent for children, background-check data, pastoral notes and break-glass, the register and erasure conflict, the processor agreement and any transfers, whether a DPIA and a data-protection officer are required, and breach duties. The full list is in the [administrator guide](admin-guide.md#items-for-a-lawyer).
