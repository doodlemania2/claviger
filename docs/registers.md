# Permanent registers

Sacramental registers (baptism, confirmation, marriage) and membership rolls are never edited. A mistake is corrected in the margin. Claviger gives a collection a **permanent mode**: once an entry is final, nobody can change or delete it, and corrections are **annotations**.

This guide is for operators and developers. The design is in `docs/specs/80-88-congregations.md`, section 9. It builds on organizations (`docs/orgs.md`) and the data map (`docs/data-map.md`).

**This is a working register, not a legal archive.** Claviger does not decide whether an electronic register replaces the paper book, whether a printed page is a valid certificate, or how long entries must be kept. Those are questions for the church and for civil law in your country. A deployment needs a lawyer for each of them.

## What the guarantee covers, and what it does not

A final entry cannot be updated or deleted through the records API, the admin UI, Go code (`app.Save`, `app.Delete`) or JS hooks (`$app.save`, `$app.delete`, a model hook that edits the record). The check sits at the model layer, like the audit log's.

It does **not** cover:

- raw SQL, the superuser SQL console, `$dbx` in a script, or anyone with the database file;
- Go code that runs on `app.UnsafeWithoutHooks()`;
- deleting the database or restoring an older backup.

The optional [hash chain](#hash-chain) makes the first three detectable afterwards. It does not prevent them.

## Set up a register

A register is an ordinary base collection that is listed in the `_permanentCollections` system collection (superusers only).

**From the template.** Creates a sacramental register with the chain on:

```
POST /api/registers   {"template": "sacramental", "name": "baptisms", "signers": 1, "retainReason": "..."}
```

The collection has `org`, `sacrament` (baptism, confirmation, marriage, other), `surname` (plain and searchable), `givenNames`, `eventDate`, `eventPlace`, `officiant`, `bookRef`, `details` (JSON for parents, godparents and so on), and an optional `subject` link to a user. Org owners, admins and registrars read and write drafts through the records API. `retainReason` defaults to a placeholder that says the controller must confirm the legal basis. **Replace it with the reason that applies to you.**

**From an existing collection.** Create a `_permanentCollections` row:

| Field | Meaning |
|---|---|
| `collection` | Name of a base collection. It must have a single-value `org` relation to `_orgs`. |
| `finalizeField` | Date field that marks an entry final. Default `finalizedAt`. |
| `mutableFields` | JSON list of fields that may still change on a final entry. Default `["restricted"]`. |
| `annotationCollection` | Default `<collection>_annotations`. Created when it does not exist. |
| `chain` | Turn the hash chain on. |
| `signers` | `1` (default) or `2`. See [Finalizing](#finalizing). |
| `subjectPaths`, `personalFields`, `retainReason` | For the data map. `subjectPaths` and `retainReason` are required. |

Setting up a register adds the fields it needs to the collection (`finalizedAt`, `finalizedBy`, `secondSigner`, `pendingBy`, `pendingAt`, `pendingHash`, `restricted`, and `seq` and `hash` with the chain), creates the annotation collection, and registers both collections in the data map with treatment `retain`. After that, `collection`, `finalizeField`, `annotationCollection`, `mutableFields` and `chain` cannot change. `signers` can.

The finalize fields are written only by the finalize routes. Writing them directly, on create or update, is refused.

With final entries, the register collection cannot be deleted, renamed, or lose or retype a field. Without final entries you can delete the `_permanentCollections` row to leave permanent mode. A register with final entries cannot be taken out of it. Deleting an org that has final entries is refused too, because the entries would go with it.

## Finalizing

```
POST   /api/registers/{collection}/{id}/finalize    take the next step
DELETE /api/registers/{collection}/{id}/finalize    withdraw a pending request
```

Finalizing is irreversible. It needs a **registrar** (a member of the entry's org with the `registrar` tag) and a **fresh passkey assertion** (the same step-up as other sensitive routes). Only registrars sign: an owner or admin without the tag cannot, and a **superuser can never be a signer** (the routes answer 403 with a clear message). Superusers keep their administrative abilities, such as creating registers through `POST /api/registers` and editing `_permanentCollections`. API keys are refused.

- **One signer** (default): the registrar finalizes alone. `finalizedAt` and `finalizedBy` are set.
- **Two signers:** a registrar requests. The entry is **pending**: it is not final and stays editable. A second, **different** registrar of the same org confirms (owners and admins without the tag cannot, and neither can a superuser) and the entry becomes final, with `finalizedBy` (first) and `secondSigner`. If the entry was edited after the request, the confirmation is refused and the request must be made again, so the second person signs what they saw. The requester, an owner or admin can withdraw a request.

The response carries `state` (`draft`, `pending`, `final`). Edits to drafts and pending entries are free.

Every step writes an audit entry with `meta.orgId`: `register_finalize_request`, `register_finalize_withdraw`, `register_finalize`.

## Annotations

```
POST /api/registers/{collection}/{id}/annotations   {"kind", "text", "reason", "supersedes"?}
GET  /api/registers/{collection}/{id}               the entry with its annotations, oldest first
```

An annotation has `entry`, `kind` (`correction`, `later_event`, `legal_note`, `retraction`, `data_subject_note`), `text`, `author`, `reason` (required), `supersedes` and `created`. Annotating needs a registrar (never a superuser) and a fresh passkey. It is allowed only on a **final** entry.

Annotations can never be changed or deleted, by anyone, including superusers (a model hook, as for the audit log). A mistaken annotation is not removed: a `retraction` points to it with `supersedes`. A retraction cannot itself be superseded. The records API cannot create annotations (the create rule is null); org staff can read them.

The annotation audit entry is `register_annotate`.

## Restriction of processing

A person can ask for erasure (GDPR Article 17) or for restriction (Article 18) of an entry about them. A final entry is never deleted, so the answer in both cases is **restriction**: the entry stays exactly as it is, in the same place in the hash chain, and its content is hidden. This is the default for every register (owner decision 2026-11-12). Whether keeping the entry is lawful for your church and country is for the controller and a lawyer to decide (`docs/erasure.md`).

`restricted` is a mutable field and is not part of the chain hash, so restricting and lifting never break verification. It cannot be changed by a plain write (the records API, `app.Save`, a hook): it changes only through the functions and routes below, so a restriction always comes with a marginal note, an audit entry and a reason.

**How an entry becomes restricted**

- **An erasure request** (`docs/erasure.md`). For each final entry about the person, the erasure job calls `core.AnnotateErasureRequest(app, ctx, entry, reason)`: it adds a `data_subject_note` annotation, sets `restricted`, and writes a `register_erasure_note` audit entry. The erasure result lists the entry as retained, with the legal reason and the statement that it is restricted.
- **A direct request.** A registrar restricts an entry (for example when a person disputes its accuracy):

  ```
  POST /api/registers/{collection}/{id}/restrict     {"reason": "..."}
  POST /api/registers/{collection}/{id}/unrestrict   {"reason": "..."}
  ```

  Both need a **registrar** of the entry's org and a fresh passkey; a superuser can never do them, like annotating (owner decision 2026-11-12). They apply to **final** entries of registers whose `mutableFields` include `restricted` (the default). Each adds a marginal note (restrict: `data_subject_note`; lift: `legal_note`, both with the reason) and writes `register_restrict` or `register_unrestrict`. Restricting a restricted entry, or lifting an unrestricted one, answers 409. Lifting also works on a restriction that the erasure job set; it is the registrar's decision, with a reason.

**What is hidden**

| Read | Restricted entry |
|---|---|
| Records API (`/api/collections/<register>/records`, realtime) | Everyone except a superuser sees only `id`, `org`, `restricted`, the finalize time and `seq`. A registrar who needs the content uses the register routes. |
| `GET /api/registers/{collection}?org=` (lists) | `redacted: true`, the same few fields. No names, no annotations. |
| `GET /api/registers/{collection}/{id}` | Redacted without a reason. A **registrar** of the org, or a **superuser**, adds `?reason=...` and gets the entry and its annotations. Owners and admins without the tag cannot reveal it, with or without a reason. `canReveal` says whether the caller could. |
| `GET /api/registers/{collection}/{id}/certificate` | 403 unless revealed the same way (`?reason=`). |

Every reveal writes a `register_restricted_read` audit entry with the reason and the reader. A superuser who works in the admin UI sees the records API unredacted (the admin UI is a superuser's own tool; raw access is outside the guarantee, as for the audit log), so use the register routes with a reason when the read should be on record. A person's own data export (`docs/erasure.md`, Article 15) still contains their entries: restriction limits processing, it does not take away their right of access.

## Anonymization

**Off by default, per register.** For churches whose regulator or court requires it (spec 10.7, open question 15), a register can replace the personal values of entries by **keyed pseudonyms** after a retention period. Turn it on with the `anonymize` and `anonymizeAfterDays` (1 to 36500) fields of the register's `_permanentCollections` row, or when creating one from the template (`"anonymize": true, "anonymizeAfterDays": 36500`). Turning it on adds two fields to the register, `anonymizedAt` and `anonDigest`; the finalize routes manage them, nobody writes them directly.

```
POST /api/registers/{collection}/{id}/anonymize   {"reason": "..."}
POST /api/registers/{collection}/anonymize        {"org": "<id>", "reason": "..."}   every due entry of the org
```

A **registrar** of the entry's org with a fresh passkey (never a superuser; no two-approver rule, because the owner decision of 2026-11-12 limits signing to registrars). An entry qualifies when it is final, `anonymizeAfterDays` have passed since `finalizedAt`, it is not under a legal hold (`docs/retention.md`) and it is not anonymized yet. Otherwise 409. The batch route skips entries that do not qualify and returns the ids it anonymized.

What happens, in one transaction:

1. Every field in the register's `personalFields` and every `subjectPaths` link is replaced: text-like fields (text, email, url, editor, encrypted text) by `anon-` and 20 hex characters, JSON fields by `{"anonymized": "anon-..."}`, dates, relations and selects cleared, numbers set to 0. The pseudonym is an HMAC-SHA-256, under the install's erasure secret, of the register, the entry id, the field and the old value. Two entries never share a pseudonym.
2. `anonymizedAt` and `anonDigest` are set.
3. A `legal_note` annotation records it: author, reason, the fields, the kept chain link and the digest. The note is the signed marginal note: it is append-only and names the registrar.
4. A `register_anonymize` audit entry is written.

This is **pseudonymization** in the sense of Article 4(5), not irreversible anonymization: the old values are gone from the database, but whoever holds the secret and a candidate value can test it. Backups and replicas taken earlier still contain the old values until they rotate (`docs/erasure.md`). Destroying the secret as well makes the pseudonyms unlinkable, and also affects the keyed pseudonyms of the audit log, so treat that as a deliberate decision.

**How verification treats an anonymized entry.** The chain hashes the entry's content, so after the personal fields are gone the entry's own hash cannot be recomputed. The stored `hash` is **kept unchanged**: it is still the link the next entry's hash was computed from, so every other entry verifies as before. For the anonymized entry itself, `claviger registers verify` checks instead

```
anonDigest = SHA-256( prevHash + "\n" + hash + "\n" + canonical(anonymized entry) + "\n" + anonymizedAt )
```

where `prevHash` is the verified hash of the previous entry of the chain, `canonical` is the form described under [Hash chain](#hash-chain), read from the anonymized fields, and `anonymizedAt` has millisecond precision in UTC. A change to the tombstone, to its kept hash or number, to its predecessor, or to the date of anonymization is a break. What verification can no longer prove is that the removed values were the ones originally signed; that is the effect of anonymizing. A register without a chain gets the digest check only. An anonymized entry stays immutable like any final entry, and it can still be restricted and annotated.

## Hash chain

With `chain` on, finalizing gives the entry the next number `seq` (from 1) and

```
hash(n) = SHA-256( hash(n-1) + "\n" + canonical(entry n) )      hash(0) = ""
```

`canonical` is the JSON of the entry's non-empty fields (keys sorted), without the autodate fields, the mutable fields, the pending-request fields, the anonymization fields and the hash. It includes the id, the finalize time, the signers and `seq`. Empty values are left out, so adding a field to the collection does not change old hashes.

```
claviger registers verify
```

recomputes every chain and prints the breaks: an entry changed, removed from the middle, inserted or renumbered outside the application. It exits with an error when it finds a break. It prints the head (number and hash) of each chain. **Keep the head somewhere else** if you also want to notice entries removed from the end. A chain does not say who wrote an entry.

To check a backup, restore it into a scratch data directory and run `claviger registers verify --dir <that directory>`. The backup verification of the server (#78) works on database rows and has no extension point for this check; `core.VerifyRegisters(app)` is the function to call from your own tooling. After restoring an older backup, run the command and compare the head with the one you kept: entries finalized after the backup was made are missing from the end, and re-entering them is a decision for the registrar.

## Certificate

```
GET /api/registers/{collection}/{id}/certificate
```

Returns an HTML page for a final entry: the entry fields, the marginal notes (a retracted one is struck through), the register number and chain hash, the issue date and the issuer. It has print CSS; save it as PDF from the browser. There is no PDF engine in Claviger. The page has no script and no inline style, and its policy allows only its own stylesheet.

It needs a bearer token, so a client fetches it and shows or prints the result (the registers page does that, see [Screens](#screens)); a plain link in the address bar does not carry the token. Registrars, owners and admins of the entry's org (and superusers, who only read) can issue it. Every issue writes a `register_certificate` audit entry. A diocese's seal and wording are not built; whether a certificate is valid is the church's call.

## Reading routes

```
GET /api/registers?org={id}                   the registers, and what the caller may do in that org
GET /api/registers/{collection}?org=&state=&page=&perPage=   entries, newest first (state: draft, pending, final)
GET /api/registers/_verify                    superuser: recompute every hash chain, report the breaks
```

The first two need an org (a superuser may leave it out and read across orgs) and the caller must be staff of it: registrars, owners and admins. A member who is not staff gets 403, a stranger 404. The list answers `registrar` (may finalize and annotate) and `manager` (owner, admin or superuser: may withdraw a request of someone else). Entries and annotations carry the names of the people (`finalizedByName`, `secondSignerName`, `pendingByName`, `authorName`) next to the ids. `_verify` is the same check as `claviger registers verify`, run on the live data. API keys are refused on the first two.

## Screens

**Registrars: `/orgs/registers/<org slug>`.** A hosted page in the style of the other org pages (strict Content-Security-Policy, strings from the message catalog, accessible mode, a sign-in of its own, DOM nodes only). The org page links to it for owners, admins and registrars. It lists the registers of the server and their entries by state, and opens an entry with its **marginal notes** (oldest first, a retracted note struck through, who wrote it and why). A registrar can:

- **finalize** a draft. With one signer it is final at once. With two signers the button reads "Put forward for a second signature"; a different registrar then sees "Confirm and finalize", and the person who put it forward sees that the entry is waiting and can withdraw the request. Both ask for confirmation first, because finalizing is irreversible;
- **add a note** to a final entry (kind, text, reason; a retraction chooses the note it takes back);
- open the **certificate**. The page fetches it with the signed-in token and shows it in a sandboxed frame (no script can run in it) with a Print button; save it as a PDF from the print dialog.

Finalizing and noting need a fresh passkey. The page does not ask in advance: it sends the request, and when the server answers `passkey_step_up_required` it runs the passkey ceremony and sends the request again. If the ceremony does not finish, nothing was changed. Owners and admins without the tag see the same entries read-only, with a message that says why. The server decides everything; the page only shows it.

**Superusers: Registers in the admin UI (`#/registers`).** A superuser cannot sign or annotate, so the page reads: the registers (and "New register" from the sacramental template), the entries across all orgs by state, an entry with its marginal notes, and **Verify the chain**, which shows each chain's head and any breaks. A superuser can also **withdraw** a pending finalization.

## Data map and export

Both collections are registered with `retain` and the stated reason, so an export includes them and an erasure request ends as partial, with the reason (see `docs/data-map.md`). The annotation collection has a copy of the entry's `org` and a `subject` link, filled by the model hook.

## Not built yet

Webhook events, per-org certificate wording, and a two-approver rule for anonymization.
