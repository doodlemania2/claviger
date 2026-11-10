# Safeguarding credentials

Many congregations require volunteers who work with children to hold a current background check and safeguarding training. Claviger can tie access to that: a person sees or edits the children's data only while they hold a current credential of the right type. When the credential expires, access closes by itself. No job has to run for that to happen.

This is the guide for operators and coordinators. The design and the evidence behind the rules are in `docs/specs/80-88-congregations.md`, section 5. It builds on organizations (`docs/orgs.md`) and the data map (`docs/data-map.md`).

**This is not legal advice.** Background-check data is regulated in many places (for example FCRA in the US and DBS in the UK). Ask a lawyer what you may store and for how long before relying on this. See [Legal and safeguarding](#legal-and-safeguarding).

## What Claviger stores

Claviger stores an **attestation**, not a background check result: the credential type, the dates, who issued it, and which coordinator recorded and verified it. It does not run checks, call any vendor, or keep a report.

Two hidden system collections hold it:

| Collection | Holds |
|---|---|
| `_credentialTypes` | What an org recognizes: `key` (for example `safe_environment`), `name`, `validMonths`, `requiresDocument`, `requiresVerifier`, `status` (`active` or `retired`). A type defined at an org also applies to every org below it. |
| `_credentials` | One row per person and type: `user`, `org` (the org that issued or recognizes it), `type`, `issuedAt`, `expires`, `issuer`, `recordedBy`, `verifiedBy`, `verified`, `revoked`, `revokedReason`, `status` and reminder bookkeeping. |

Both are on the data map. `_credentials` is exported with the person's data and erased after the credential retention period your congregation sets. `_credentialTypes` holds no personal data. `_credentials` is also on the API-key forbidden list: no API key can read or write it.

## Who is a coordinator

A **coordinator** of an org is an active owner or admin, or an active `member` who carries the tag `safeguarding`. A `viewer` is never a coordinator, whatever tags they carry. Coordinators of an **ancestor** org (for example a diocesan safeguarding officer) can read the credentials of the orgs below them, but they cannot change them.

Rules that keep it honest:

- Nobody records, renews or verifies their own credential. Ask another coordinator (a superuser can also do it).
- A credential is attested by the coordinator who recorded it. If the type has **`requiresVerifier`**, it starts as `pending` and grants nothing until a **second** coordinator, who is neither the recorder nor the holder, verifies it. A second verifier is optional and off by default.
- Documents (a scanned certificate) are optional. They are off unless the org setting `credentialDocuments` is `true` (edit the org's `settings` as a superuser). With it off, a document upload is refused and a type cannot require one. A stored document is a protected file.
- A holder must be a member of the issuing org or of an org below it.

## Credential routes

All routes need a signed-in user. API keys are refused.

| Route | Who | |
|---|---|---|
| `GET /api/orgs/{id}/credential-types` | members, and coordinators of an ancestor | the org's types and its ancestors' types |
| `POST /api/orgs/{id}/credential-types` | owner/admin | `{key, name, validMonths?, requiresDocument?, requiresVerifier?}`. Key: `a-z`, `0-9`, `_`, 2 to 40 characters |
| `PATCH /api/orgs/{id}/credential-types/{type}` | owner/admin | change a type of this org, or retire it (`status`) |
| `DELETE /api/orgs/{id}/credential-types/{type}` | owner/admin | refused (409) while credentials of the type exist; retire it instead |
| `GET /api/orgs/{id}/credentials` | coordinators | `?user=&type=&state=pending\|valid\|lapsed\|revoked&page=&perPage=` |
| `POST /api/orgs/{id}/credentials` | coordinators | `{user, type, issuedAt?, expires?, issuer?}`, JSON or multipart (with a `document` file when allowed) |
| `PATCH /api/orgs/{id}/credentials/{cred}` | coordinators | renew or correct: `{issuedAt?, expires?, issuer?}`. New dates restart the reminders, and a type with a verifier needs verifying again |
| `POST /api/orgs/{id}/credentials/{cred}/verify` | a second coordinator | confirm a pending credential |
| `POST /api/orgs/{id}/credentials/{cred}/revoke` | coordinators | `{reason?}`. Takes effect at once |
| `GET /api/orgs/{id}/access-status` | coordinators | see [Who is cleared](#who-is-cleared) |

`user` is a user id or an email. `expires` is required unless the type has `validMonths`, and must be in the future. A date without a time (`2027-01-31`) means the **end of that day** in UTC.

A holder reads their own credentials through the records API (`/api/collections/_credentials/records`), as do coordinators (the issuing org and its ancestors). The revoke reason is a hidden field and is not served there; coordinators get it from the routes. Writes through the records API are superuser-only. Use the routes.

Every change is an audit entry with the actor and `meta.orgId`, and a webhook event from the collection events (`_credentials:create`, `:update`).

## Gating access on a credential

There is no `hasCredential` function in the rule engine, and there will not be: the resolver stays upstream's. The gate is plain rule syntax. Go helpers generate the exact strings, and the tests run them:

```go
// a full rule set for a collection with an `org` relation
set, err := core.CredentialRuleSet("org", core.CredentialRuleOpts{
    Type:  "safe_environment",
    Scope: core.CredentialScopeAncestors, // or CredentialScopeOrg (default), CredentialScopeAny
})
```

`CredentialRuleSet` is the "members read and write, admins delete" preset with the gate added to list, view, create and update. `core.CredentialGatedMemberRule` returns one member rule plus the gate, and `core.CredentialGateRule` returns the gate alone, to combine with your own rule. Superusers can also fetch a set from the API:

```
GET /api/orgs/rule-templates?field=org&credentialType=safe_environment&credentialScope=ancestors&credentialMaxDepth=2
```

The gate, in the `org` scope:

```
@collection._credentials:c.user ?= @request.auth.id
  && @collection._credentials:c.type ?= "safe_environment"
  && @collection._credentials:c.expires ?> @now
  && @collection._credentials:c.revoked ?= false
  && @collection._credentials:c.verified ?= true
  && @collection._credentials:c.org ?= org
```

| Scope | Counts a credential issued by |
|---|---|
| `org` (default) | the record's own org only |
| `ancestors` | the record's org **or any ancestor**, through `_orgClosure`. A diocese-issued credential counts in a parish. `MaxDepth` limits how far up (1 is the parent) |
| `any` | any org |

Notes:

- Every condition on the alias `c` reads the same credential row, so "a credential that is valid, not revoked, verified and of this type" means one credential.
- **Lapse is immediate at query time**, because the rule reads `expires`. A late or failed job never leaves access open. Realtime subscribers stop receiving matching events, but data already fetched stays in the client. Apps should refetch after a 404 or an empty list.
- The ancestor test uses a second `@collection` alias on purpose. A multi-valued path on the right of a comparison silently matches nothing (`docs/orgs.md`, Hierarchy). The helpers use the aliases `c` and `q`, which do not clash with `OrgNoMoveRule` (`n`) or `OrgAncestorRule` with a tag (`k`, `m`).
- Combine with a tag (`OrgTagRule`) or oversight (`OrgAncestorRule`) by joining strings with `&&`. Keep pastoral data out of ancestor rules.
- A silent empty list looks like a bug to a volunteer. Tell people why: the coordinators' access-status route below shows who is affected, and the holder can read their own credential rows.

## Reminders and lapses

A daily job (06:17) keeps the dashboard status and writes the events that a passage of time would otherwise leave out:

- **`credential_expiring`**: when the days left cross a threshold. Thresholds are 60, 30 and 7 days by default. An org changes them with `settings.credentialReminderDays` (up to six values, each 1 to 365). One entry per threshold crossed, at most.
- **`credential_lapsed`**: once, when a credential's expiry passes. It sets `status` to `lapsed`.

Both are audit entries (collection `_credentials`, with `meta.orgId`, `meta.type` and `meta.user`) and are delivered to webhooks as `credential.expiring` and `credential.lapsed`. Subscribe with the event filters `_credentials:credential_expiring`, `_credentials:credential_lapsed`, `_credentials:*` or `*:credential_lapsed`.

The job is idempotent. Each step is a compare-and-swap update and only the run that changed the row writes the entry, so running it twice, or on two servers, never duplicates. Revoked and still-unverified credentials get no reminder.

It does not send email. Claviger raises the event; deliver the reminder to the person and the coordinators through a webhook to your own tooling. (Sending the mail from Claviger itself is an open question.)

## Who is cleared

`GET /api/orgs/{id}/access-status` answers, for every **active member** of the org and each active credential type that applies to it: cleared, expiring, pending, lapsed, revoked or missing. Credentials issued by the org or any ancestor count.

Query: `type` (one key), `standing` (limit the items, not the counts), `days` (the "expiring" window, default 60, up to 365), `limit` (items per type, default 200, up to 1000).

```json
{
  "org": "…", "asOf": "…", "expiringDays": 60,
  "types": [{
    "key": "safe_environment", "name": "Safe environment",
    "counts": {"cleared": 12, "expiring": 2, "pending": 0, "lapsed": 1, "revoked": 0, "missing": 4},
    "totalItems": 19,
    "items": [{"user": "…", "email": "…", "standing": "lapsed", "expires": "…", "daysLeft": -9, "issuedByOrg": "…"}]
  }]
}
```

The best credential of a person wins. Only coordinators (including ancestors') can call it: this is for coordinators, never a public list. A lapse that removes access must not turn into public shaming.

## Legal and safeguarding

Not legal advice. These points are kept in front of the people who run this:

- **Background-check results are regulated** (for example FCRA in the US, DBS in the UK). Storing only a yes/no attestation and a date reduces the burden. A lawyer decides what else may be kept and for how long. Documents are off by default for that reason.
- **The holder has data rights** (#88). Credentials are personal data; retention follows the retention policy of your jurisdiction and congregation.
- **Some regions require a check on the person and a barred-list check.** That is the congregation's process, not Claviger's.
- **A coordinator can approve a friend.** The second-verifier option, the audit trail and the diocesan read access are the safeguards; Claviger cannot judge a check was real.
- **A lapse must not become public shaming.** Keep the access-status route and the credential rows to coordinators.

## Operating notes

- Deleting an org does not remove its credentials: the `org` relation has no cascade, so an org that still issued credentials cannot be deleted by accident. A superuser removes the rows first (they are records of the `_credentials` collection), then deletes the org.
- Deleting a user removes their credentials (cascade). Their recorder and verifier links are cleared on the credentials they handled.
- Restoring a backup or editing the database by hand is fine: the stored `status` is only for the dashboard, and the job corrects it on the next run. Access never depends on it.
