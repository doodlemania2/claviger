# Record of processing activities (Article 30) template

> **This is a template, not legal advice.** Whether you must keep a record, and in what form, depends on your country and your size. Many regulators expect one even from small organizations that process special-category data. Have a lawyer review the finished record. See [Items for a lawyer](admin-guide.md#items-for-a-lawyer).

The record is a written list of what personal data you process, why, who sees it, where it goes and how long you keep it. You give it to the regulator on request.

How to use this page:

- Plain text is pre-filled from what Claviger stores. Check it against your own setup.
- `[ ... ]` is a blank for you to fill in.
- Add a row for every collection or app you add.
- The **data map** (`Data protection > Data map`) lists every collection. Use it to check you have not missed one. The spec for the data map says a generated record is planned, but **no generator is built**, so fill this by hand.
- Keep it up to date, and date every change.

Related: [Administrator guide](admin-guide.md), [DPIA template](dpia-template.md).

## Part 1. Controller details

| | |
|---|---|
| Name of the controller (the parish or organization) | [ ... ] |
| Address and contact | [ ... ] |
| Representative, if outside the EU/UK | [ ... ] |
| Data-protection lead or officer, with contact | [ ... ] |
| Joint controllers (diocese, network, other parish) | [ ... ] |
| Processors (who runs the server, email or SMS provider, backup storage) | [ ... ] |
| Record last updated | [ ... ] |
| Updated by | [ ... ] |

## Part 2. Processing activities

One row per activity. The pre-filled rows are what Claviger holds. The data categories use the names of the built-in collections.

| # | Activity | Purpose | Data subjects | Categories of data | Special category? | Recipients | Transfers outside the country | Retention | Security measures |
|---|---|---|---|---|---|---|---|---|---|
| 1 | Accounts and sign-in | Let people sign in and keep the accounts secure | Members, volunteers, staff | Email, name, avatar. Passkey name and times, sessions (device label, IP), API keys, one-time codes, linked sign-in providers (`_webauthnCredentials`, `_sessions`, `_apiKeys`, `_externalAuths`, `_totps`, `_mfas`, `_otps`) | No | [ host ] | [ ... ] | Until the account is erased. Sessions and codes expire. [ ... ] | Passkeys. Secrets stored as hashes. Not exported. [ ... ] |
| 2 | Membership and roles | Run the organization, give the right access | Members | Organization, role, tags, status, source, invitations (`_orgMembers`, `_invitations`) | **Yes** (membership reveals belief) | [ staff, diocese if enabled ] | [ ... ] | [ ... ] Invitations: 90 days | Roles, tags, audit log. [ ... ] |
| 3 | Households and children | Know families, let guardians manage children | Households, children, guardians | Household name, address, home phone. For each person: name, role, birth date, photo, guardian flag, minor flag, status (`_households`, `_householdMembers`) | Children's data | [ guardians, staff ] | [ ... ] | [ ... ] Youth data: [ N years after graduation age ] | Address, phone and birth date are encrypted fields. Rules limit who sees what |
| 4 | Health information about children and members | Safe care in activities | Children, members | Allergies, medical notes (`_householdMembers`) | **Yes** (health) | [ guardians, staff who need it ] | [ ... ] | [ ... ] | Encrypted fields |
| 5 | Safeguarding credentials | Make sure volunteers who work with children are cleared | Volunteers, staff | Credential type, issue and expiry dates, issuer, who recorded and verified it, status. Optional document (off by default) (`_credentials`) | Not a check result. Sensitive | [ safeguarding coordinators, diocese if enabled ] | [ ... ] | [ the period your parish sets ] | Coordinator-only access. Not available to API keys. Attestation only |
| 6 | Consent records | Prove what people agreed to and when | Anyone who consents, guardians | Subject id, purpose, version, text shown, time, who gave it, method, evidence reference (`_consents`, `_consentEvents`) | Depends on the purpose | [ staff ] | [ ... ] | [ the limitation period ] Kept as evidence after erasure with a pseudonym | Append-only events |
| 7 | Pastoral care notes and prayer requests | Pastoral care, prayer | Members, people the notes are about | Note text, author, subject, visibility, expiry (collection `pastoral_notes`, or the name you chose) | **Yes** | [ care team or clergy by visibility ] | [ ... ] | Prayer requests: [ 90 days default, then archive and delete ]. Care notes: [ ... ] | Body is an encrypted field. Locked access rules. Read audit for sealed notes |
| 8 | Break-glass access | Emergency access to a sealed note with two people and notice | Authors of notes, grantees | Grantee, approver, reason, basis, times, read count (`_breakGlass`) | Sensitive | [ org owners (reason visible) ] | [ ... ] | [ ... ] Kept after erasure with a pseudonym | Two-person approval, owner notice, audit |
| 9 | Pastoral export holds | Decide whether a note about a person is given in their export | People notes are about | Hold state, decision note (`_pastoralHolds`) | Sensitive | [ org owners ] | [ ... ] | [ ... ] | Owners only |
| 10 | Sacramental registers | Permanent record of baptisms, confirmations, marriages | People named in entries | Entry fields as you define them, annotations, hash chain | **Yes** | [ registrars ] | [ ... ] | Permanent, reason: [ ... ] | Final entries cannot be changed. Annotations only. Hash chain |
| 11 | Staff-vouched recovery | Let people who cannot use email recover an account | People recovered, staff | Who vouched, how, when. Hash of the code (`_recoveryCodes`) | No | [ staff ] | [ ... ] | Rows deleted 30 days after the code expires | Hash only. Audit |
| 12 | Organization transfers | Move a person to another organization with consent | People moved | Transfer record (`_orgTransfers`) | [ ... ] | [ both orgs ] | [ ... ] | [ ... ] | [ ... ] |
| 13 | Data-subject requests | Answer access, export and erasure requests | Requesters | Subject, type, state, dates, result counts, identity check note (`_dataRequests`, `_erasureLog`) | No | [ data-protection lead ] | [ ... ] | [ ... ] Erasure log holds keyed hashes only | Superuser only |
| 14 | Legal holds | Preserve data for a legal matter | People involved | Scope, reason, reference, placed and lifted by (`_legalHolds`) | Sensitive | [ ... ] | [ ... ] | Until lifted, then [ ... ] | Superuser only, passkey step-up |
| 15 | Audit log | Security, accountability | Everyone who acts or is acted on | Actor, IP, action, time, before and after values (secrets and encrypted fields redacted), `meta` (`_auditLog`) | May contain | [ superusers ] | [ ... ] | [ `audit.retentionDays`; default keeps forever ] | Append-only. Scrubbed for erased people |
| 16 | Request logs | Operation, abuse handling | Anyone who makes a request | User id, IP, request details (`_logs`) | No | [ superusers ] | [ ... ] | [ `logs.maxDays`; default 5 days ] | [ ... ] |
| 17 | Connected apps and sign-in provider | Single sign-on for parish apps | Members | OIDC consents, tokens, requests (`_oauth*`) | [ ... ] | [ each app: ... ] | [ ... ] | Until revoked or erased | Scopes. Hashed tokens |
| 18 | Provisioning from an identity provider (SCIM) | Keep members in step with an identity provider | Members | Name, email, group and role mapping (`_scimTokens`, `_scimGroups`, `_orgMembers`) | [ ... ] | [ identity provider ] | [ ... ] | [ ... ] | Token scope |
| 19 | Webhooks | Tell other systems about events | Subjects of the events | Event type and payload (`_webhookDeliveries`) | May contain | [ each receiver ] | [ ... ] | [ ... ] | Signed payloads. Household and pastoral bodies redacted |
| 20 | Email and SMS | Invitations, notices, recovery links, results | Recipients | Address or number, message text | No | [ mail or SMS provider ] | [ ... ] | [ ... ] | [ ... ] |
| 21 | Backups | Recovery after failure | All of the above | Copies of the database and files | **Yes** (includes everything) | [ backup storage ] | [ ... ] | [ keep N, rotate every N days ] | Optional encryption (AES-256-GCM). Key off the server |
| 22 | [ your own collection ] | [ ... ] | [ ... ] | [ ... ] | [ ... ] | [ ... ] | [ ... ] | [ ... ] | [ ... ] |

Lawful basis and special-category condition (Article 6 and Article 9) for each row: you decide, with your lawyer. Fill them in this table.

| # | Article 6 ground | Article 9 condition | Notes |
|---|---|---|---|
| 1 | [ ... ] | n/a | |
| 2 | [ ... ] | [ for example 9(2)(d) ] | |
| 3 | [ ... ] | [ ... ] | |
| 4 | [ ... ] | [ ... ] | |
| 5 | [ ... ] | [ ... ] | |
| 6 | [ ... ] | [ ... ] | |
| 7 | [ ... ] | [ ... ] | |
| 8 | [ ... ] | [ ... ] | |
| 9 | [ ... ] | [ ... ] | |
| 10 | [ ... ] | [ ... ] | |
| 11 | [ ... ] | n/a | |
| 12 | [ ... ] | [ ... ] | |
| 13 | [ ... ] | n/a | |
| 14 | [ ... ] | [ ... ] | |
| 15 | [ ... ] | n/a | |
| 16 | [ ... ] | n/a | |
| 17 | [ ... ] | [ ... ] | |
| 18 | [ ... ] | [ ... ] | |
| 19 | [ ... ] | [ ... ] | |
| 20 | [ ... ] | n/a | |
| 21 | [ ... ] | [ ... ] | |

## Part 3. Transfers outside your country

| Recipient | Country | Safeguard (adequacy decision, standard clauses, other) | Date |
|---|---|---|---|
| [ ... ] | [ ... ] | [ ... ] | [ ... ] |

## Part 4. General description of security measures (Article 32)

Pre-filled with what Claviger provides. Add yours.

- Sign-in with passkeys. Fresh passkey step-up for sensitive actions (erasure, holds, break-glass, finalizing register entries).
- Field encryption (AES-256-GCM, a key per field, wrapped by a key held outside the database). Key sources: environment, a file, or a cloud key vault.
- Optional encrypted backups.
- Append-only audit log. Read audit for flagged collections. Webhooks carry ids and counts, not content, for data-protection events.
- Access by org role and tag. Locked rules for pastoral notes. Break-glass needs two people and always tells the author and the org owners.
- Staff-vouched recovery with short-lived codes stored as hashes.
- Retention tools with dry runs and legal holds.
- Your own measures: [ physical security, device policy, staff training, access reviews, incident procedure, host's certifications ]

## Part 5. Processor record (if you act as a processor for someone else)

Complete only if you process data for another controller. A parish usually is the controller, and the host is the processor.

| | |
|---|---|
| Name and contact of the processor | [ ... ] |
| Controllers you act for | [ ... ] |
| Categories of processing | [ ... ] |
| Transfers | [ ... ] |
| Security measures | [ ... ] |

## Review log

| Date | Change | By |
|---|---|---|
| [ ... ] | [ ... ] | [ ... ] |

## Items for a lawyer

Ask your lawyer to confirm: whether you must keep a record, the lawful ground and the Article 9 condition per activity, retention periods, controller and processor roles (and the written agreement), and transfers. The full list is in the [administrator guide](admin-guide.md#items-for-a-lawyer).
