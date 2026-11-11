# Staff-vouched recovery codes

Some people cannot use an email reset. They may have no email, or find it hard. The church office often knows them by sight. This feature lets staff vouch for a person. Staff give the person a short code. The person uses it to set up a new passkey.

The design is in `docs/specs/80-88-congregations.md`, section 6. It builds on organizations (`docs/orgs.md`) and passkeys.

## How it works

1. A staff member checks who the person is, in person.
2. Staff open the recovery page of the org: `/orgs/recovery/<org slug>`. They choose the person and say how they checked.
3. The page shows a code, for example `K7M2Q 9XWTD`. It is valid for 15 minutes and works once.
4. The person opens `/recovery` on their own phone or computer and types the code.
5. The person creates a passkey. This is the normal passkey reset: the old passkeys are removed and old sessions stop working.

Staff never see or set a password. The code is never sent by message.

## Who can issue a code

- Owners and admins of the org.
- Members who carry the tag `greeter` in that org. Create the tag in the org page, then add it to the people you trust. A viewer cannot be staff.
- Superusers.

The person must be an active member of the org. Nobody can issue a code for themselves.

Staff need a passkey of their own, and they must have used it in the last 5 minutes. The page asks for it when needed.

## Two staff for owners and admins

If the person is an owner or admin of any org, one staff member is not enough.

- The first staff member creates a request. There is no code yet.
- A different staff member opens the same page and confirms the request. They also need a fresh passkey.
- The code is created at that moment and shown to the person who confirmed. It is not stored anywhere else, so it cannot be shown again.

The same rule applies to a person whose email is the email of a superuser. The person asking and the person confirming must be two different people, and neither can be the person being recovered.

## What the person receives

If the person has an email address, they get a notice: "staff issued a recovery code at 10:14". The notice has a button, "This was not me". It stops the code at once and writes an audit entry. The notice never contains the code.

Text message notices are not built yet. The code `sendRecoveryNoticeSMS` in `apis/recovery.go` is the place to add them. The `sms` channel works like the display channel for now: it shows the code and sends nothing.

## Staff checklist

- [ ] The person is in front of me, or on a video call I can trust.
- [ ] I know them by sight, or I checked an ID.
- [ ] I chose the right person. Two people can have the same name.
- [ ] I read the code aloud or showed the screen. I did not send it in a message.
- [ ] I did not write anything sensitive in the note.
- [ ] The person has used the code and made a passkey before they leave. If not, the code will expire. Issue a new one.
- [ ] If someone asks you by phone or message to recover another person, do not. Ask them to come in. This is the most common trick.

## Limits and safety

- The code has 10 characters from a set of 32. The first 5 are a public reference. Three wrong guesses with a valid reference lock the code.
- Only a SHA-256 hash is stored. The code and the hash are never written to the audit log or to the application log.
- A new code for a person closes any older code for the same person.
- Each staff member can issue 5 codes per hour.
- Redeeming is limited per IP address. Every failure gives the same answer: invalid or expired code.
- A banned person gets no code, and the refusal looks the same as for a person who is not a member.
- If the staff member who vouched loses the right (they are removed, suspended or lose the `greeter` tag), their open codes stop working.
- Open codes of a deleted person are deleted with the account. Rows are deleted 30 days after they expire.

The board should review who holds the `greeter` tag from time to time.

## Settings

| Environment variable | Default | Meaning |
|---|---|---|
| `CLAVIGER_RECOVERY_CODE_MINUTES` | 15 | Lifetime of a code, 5 to 60. |
| `CLAVIGER_RECOVERY_ISSUE_PER_HOUR` | 5 | Codes one staff member may issue per hour, 1 to 100. |

## Routes

| Route | Who | |
|---|---|---|
| `GET /api/orgs/{id}/recovery` | staff | Open requests of the org. Never a code, a hash or a reference. |
| `POST /api/orgs/{id}/recovery` | staff, fresh passkey | `{user, verification, channel?, note?}`. `verification`: `known_by_sight`, `id_checked`, `other`. `channel`: `display` (default), `qr`, `sms`. `note`: up to 200 characters. Returns the code once, or `pending: true` for an owner or admin. |
| `POST /api/orgs/{id}/recovery/{rid}/confirm` | a second staff member, fresh passkey | Returns the code once. |
| `POST /api/recovery/redeem` | anyone | `{code}`. Returns `{token, collection}`. The token is the passkey reset token for `/api/collections/{collection}/confirm-passkey-reset/begin` and `/finish`. |
| `POST /api/recovery/report` | anyone with the link | `{token}`. Closes the code. |

API keys are refused on the staff routes. The `_recoveryCodes` collection has no API rules, so only superusers can list it, and no API key can reach it.

## QR codes

The `qr` channel creates a longer token and a link, `/recovery#code=<token>`. The token is in the part after `#`, which browsers do not send to the server. The staff page shows the link as text. **It does not draw a QR code**, because the project has no QR library and does not add dependencies for this. Make the QR code with a generator that runs on your own device. Do not paste the link into a website.

## Audit

Every step writes an entry with the staff member as the actor, `meta.orgId`, `meta.verification` and `meta.channel`:

| Action | When |
|---|---|
| `recovery_issue` | A code or a pending request was created. |
| `recovery_confirm` | A second staff member confirmed. |
| `recovery_redeem` | The person used the code. The actor is the staff member who vouched; `meta.user` is the person. |
| `recovery_redeem_failed` | A guess that matched a live code's reference, or a right code that could not be used. `meta.reason` says why. |
| `recovery_code_locked` | The third wrong guess. |
| `recovery_reported` | The person pressed "This was not me". |

A guess that matches no code is written to the application log (the reason and the IP address only), not to the audit log. That keeps a guessing attack from filling the audit log.

## Data map

`_recoveryCodes` is on the data map and in the personal data export. The export has who vouched, how and when. It never has the reference or the hash.
