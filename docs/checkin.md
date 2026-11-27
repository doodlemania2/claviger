# Children's check-in and pickup

Claviger holds the primitives of check-in: who may check a child in and who may take them home, the pickup codes, the kiosk device identity, the do-not-release flags and a log nobody can edit. The kiosk screen, label printing, rooms, ratios and attendance reports are an app on top ([#82](../../issues/82), spec section 7 of `docs/specs/80-88-congregations.md`).

Check-in data is children's data. Nothing in this feature replaces your insurer's procedure; it supports one.

## Set up

Needs households ([households.md](households.md)), field encryption ([field-encryption.md](field-encryption.md)) and a server key for the codes.

1. Set `CLAVIGER_CHECKIN_KEY` (at least 16 characters) in the environment. Without it the key is derived from the settings encryption key (`PB_ENCRYPTION_KEY`); with neither, check-in refuses to run. The key must stay outside the database: the codes are stored as HMAC-SHA256 with it, so a database leak alone does not let anyone brute force a short code. Changing it makes every open code unusable.
2. As a superuser: `POST /api/checkin/install` with `{"orgs": ["<org id or slug>"]}`. It creates the collections below, registers each one in the data map ([data-map.md](data-map.md)), creates two retention policies (disabled and a dry run, like every policy) and the org tags `checkin` and `checkin-supervisor`.
3. Give the tags to the staff who need them. Owners and admins need none.

| Collection | Holds | Written by |
|---|---|---|
| `checkin_events` | org, name, starts, ends, status (`open`, `closed`) | org owners and admins (records API) |
| `checkin_sessions` | one row per child per event: household, child, who checked in, `state` (`in`, `out`, `void`), how it was released, plain `flags` (`allergy`, `medical`), the code and QR as HMACs (`codeHash`, `qrHash`, never returned), attempts and lock, `incident`, `purgeAfter` | the routes only |
| `pickup_authorizations` | people a guardian approved to pick a child up: name, relationship, photo, valid until, optional account | guardians of the household (records API) |
| `checkin_log` | append-only log of every check-in, release, refusal, lock and alert | the routes only |
| `checkin_flags` | do-not-release flags; the reason is an `encryptedText` field | coordinators, through the routes |
| `_kiosks` (system) | kiosk devices: org, name, `deviceTokenHash`, registeredBy, lastSeen, status, allowedEvents | org admins, through the routes |

Allergy and medical details stay on the household member as encrypted JSON. A session carries only the marker, so rules, labels and webhooks never show details. Staff read the details through the households API.

Roles: staff are owners, admins, and members with the `checkin` tag; coordinators (supervisors) are owners, admins, and members with `checkin-supervisor`. A viewer is never staff.

## Rules

Reading `checkin_sessions` and `pickup_authorizations`: members of the household, and staff. Reading `checkin_log`: staff. Reading `checkin_flags`: coordinators. Writing sessions, log and flags: nobody through the records API (null rules; superusers excepted); the routes write them. A guardian creates and edits the pickup list of their own household; a hook sets the org from the household and refuses a child of another household.

## Check in

`POST /api/checkin/{event}/in` with `{"household": "<id>", "children": ["<member id>", ...]}`.

The caller must be an active guardian of the household (or a kiosk with a kiosk session, below). The event must be open, not past its `ends`, and belong to the household's org. Each child must be an active minor of the household and not already in.

The answer holds one **family code** and the label of each child:

```json
{"event": "...", "household": "...", "code": "K7M2Q",
 "labels": [{"session": "...", "childName": "Kid A", "code": "K7M2Q", "qr": "<sessionId>.<token>", "flags": ["allergy"], "checkedInAt": "..."}]}
```

- One code per family per event (the child's label and the parent slip show the same code, so a person can compare them). A second child of the same family checked in later gets the same code.
- The code is 4 to 6 characters (org setting `checkinCodeLength`, default 5) from an alphabet without look-alikes (no 0, O, 1, I, L). It is derived from the server key, so it can be shown again without being stored; only its HMAC is stored.
- The QR token is long and random, one per child, and maps to that session only.
- Both are shown once, in this answer. Printing is the app's job.

## Release

`POST /api/checkin/sessions/{id}/release`. Three ways:

- **Code or QR**: `{"code": "K7M2Q", "releasedTo": "Erin E"}` or `{"qr": "<sessionId>.<token>"}`, by staff or a kiosk. Compared in constant time against the HMAC. The code is dead after the release, once the event is closed or past its `ends`, and for any other session.
- **Guardian passkey**: `{}` with a token from the step-up ceremony (`.../auth-with-webauthn/step-up-begin` and `step-up-finish`, valid 5 minutes). The signed-in user must be an active guardian of the child's household, or a person with an account on the child's pickup list (not expired). Without a fresh passkey the route answers 403 `passkey_step_up_required`.
- **Override**: `{"override": true, "reason": "...", "releasedTo": "..."}` by a coordinator, against a mismatch. The reason (at least 10 characters) is logged, the coordinators are alerted and the household's guardians are emailed. An override cannot beat a do-not-release flag.

**Five wrong codes lock the session.** Each try is reserved with an atomic update before the compare, so parallel guesses cannot get more than five. The fifth wrong try locks the session, writes an alert, and emails the coordinators. Every failure of the code check, whether the session is unknown, locked, dead or the code wrong, gives the same answer (403, "The code could not be verified."). A locked session can still be released by the guardian's passkey or by an override.

## Do-not-release

`POST /api/checkin/flags` (coordinators) with `{"child": "<member id>", "people": ["Grandpa Joe"], "reason": "..."}`. `people` names who may not take the child: a name, a household member id or a user id (case-insensitive). With no names, nobody may take the child until the flag is lifted (`POST /api/checkin/flags/{id}/lift`; the row stays as evidence). A flag with names also blocks a release that does not say who takes the child, because the check cannot be answered.

The check is in the release route, for every method, an override included. A blocked release answers 403 and raises a `do_not_release` alert. The reason is encrypted at rest and returned by no check-in route.

## Kiosks

A kiosk is a device, not a user. Its token is a restricted principal: the `/api/checkin` routes of its own org (and only the events in `allowedEvents`, when set) and nothing else. It is sent in the `X-Kiosk-Token` header, which nothing else in the API recognises, so the records API, realtime and the rest see a guest.

- `POST /api/checkin/kiosks` (org owner or admin, fresh passkey) with `{"org", "name", "allowedEvents"?}` returns the token (`clvk_...`) **once**. Only its hash is stored.
- `GET /api/checkin/kiosks?org=<id>` lists them (no hashes). `POST /api/checkin/kiosks/{id}/revoke` ends one. The token is checked against the database on every request, so a revoked kiosk stops working at once. Failed token checks are rate limited.
- A kiosk may release by code or QR. It cannot override and cannot read data.
- **Short sessions.** A guardian identifies at the kiosk by a handoff: on their phone, with a fresh passkey, `POST /api/checkin/handoffs` with `{"household", "kiosk"}` returns a handoff token (valid 2 minutes, one use, one kiosk, shown as a QR). The kiosk sends it to `POST /api/checkin/kiosk/sessions` and receives a signed kiosk session, which it sends as `X-Kiosk-Session` on `/in`. The session names the kiosk, the household and the guardian, and the server enforces its lifetime: 5 minutes, or the org setting `kioskSessionMinutes` (1 to 15). Clearing the screen after each family and after 60 seconds of inactivity is the client's job. (The single-use bookkeeping of handoffs is in process memory: with several replicas a second use is not caught, but the 2 minute lifetime still holds.)

## Alerts

An alert is written when a session is locked, when a do-not-release flag blocks a release, and on an override. Each one is:

- an audit entry `checkin_alert` and a webhook event `checkin.alert` (ids and the kind only: no name, code or reason; see [webhooks.md](webhooks.md));
- a `checkin_log` row;
- an email to the coordinators of the org (owners, admins, `checkin-supervisor`), in the recipient's language. The email carries no names and no reasons.

The check-in collections never send record webhooks (children's data); only `checkin.alert` leaves.

## Log, retention and incidents

`checkin_log` is append-only: the records API, Go hooks and JS hooks cannot change or delete a row (a model hook refuses it, like `_auditLog`; raw SQL is not covered). Only the retention job may delete, after `purgeAfter`. Each check-in and release is also an ordinary audit entry of the session.

**Retention.** Every session and log row gets `purgeAfter` when it is written: 12 months after (org setting `checkinRetentionMonths`, 1 to 240, applies to rows written afterwards). The two policies created at install delete on that date. They start disabled and as a dry run: review with the preview in the retention screen, then enable and switch off the dry run ([retention.md](retention.md)).

**Incident hold.** `POST /api/checkin/sessions/{id}/incident` with `{"reason"}` (a coordinator, fresh passkey) flags the session and places a legal hold ([retention.md](retention.md)) on it and on every log row of it, and on log rows written afterwards. Retention, cascades and the records API then cannot delete them. Lift the holds with `/api/gdpr/holds` (superusers).

## Data protection

Every collection is in the data map: sessions and log are `retain` (a safeguarding record, with the 12 month policy above), pickup authorizations are `erase`, flags are `retain`, kiosks are `anonymize` (only the admin's id). The export of a guardian includes the sessions they made and their own log rows, never the code and QR hashes. Photos of children and of authorized people need consent ([consent.md](consent.md)).

## Not in core

The kiosk and staff screens, large buttons and idle screens, label printing (Zebra, Brother, paper), rooms, age groups and ratios, attendance reports, and the display of allergies on a nursery screen are the app's. Claviger returns the label data.
