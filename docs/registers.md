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

Finalizing is irreversible. It needs a **registrar** (a member of the entry's org with the `registrar` tag) and a **fresh passkey assertion** (the same step-up as other sensitive routes). Superusers count as registrars, and the audit log names them. API keys are refused.

- **One signer** (default): the registrar finalizes alone. `finalizedAt` and `finalizedBy` are set.
- **Two signers:** a registrar requests. The entry is **pending**: it is not final and stays editable. A second, **different** person (a registrar, or an owner or admin of the org) confirms and the entry becomes final, with `finalizedBy` (first) and `secondSigner`. If the entry was edited after the request, the confirmation is refused and the request must be made again, so the second person signs what they saw. The requester, an owner or admin can withdraw a request.

The response carries `state` (`draft`, `pending`, `final`). Edits to drafts and pending entries are free.

Every step writes an audit entry with `meta.orgId`: `register_finalize_request`, `register_finalize_withdraw`, `register_finalize`.

## Annotations

```
POST /api/registers/{collection}/{id}/annotations   {"kind", "text", "reason", "supersedes"?}
GET  /api/registers/{collection}/{id}               the entry with its annotations, oldest first
```

An annotation has `entry`, `kind` (`correction`, `later_event`, `legal_note`, `retraction`, `data_subject_note`), `text`, `author`, `reason` (required), `supersedes` and `created`. Annotating needs a registrar and a fresh passkey. It is allowed only on a **final** entry.

Annotations can never be changed or deleted, by anyone, including superusers (a model hook, as for the audit log). A mistaken annotation is not removed: a `retraction` points to it with `supersedes`. A retraction cannot itself be superseded. The records API cannot create annotations (the create rule is null); org staff can read them.

The annotation audit entry is `register_annotate`.

## Restriction and erasure requests

`restricted` is the one default mutable field. It lets a registrar stop an entry from being shown outside the registrar role while the entry stays.

A final entry is **retained** when a person asks for erasure, for the reason recorded in the data map. A later erasure feature calls

```go
core.AnnotateErasureRequest(app, ctx, entry, reason)
```

which adds a `data_subject_note` annotation with the reason, sets `restricted` when the register has that mutable field, and writes a `register_erasure_note` audit entry. It does not change or anonymize anything else. Anonymizing the personal fields of a final entry, with two approvers, is not built; build it only when a regulator or court makes a church need it.

## Hash chain

With `chain` on, finalizing gives the entry the next number `seq` (from 1) and

```
hash(n) = SHA-256( hash(n-1) + "\n" + canonical(entry n) )      hash(0) = ""
```

`canonical` is the JSON of the entry's non-empty fields (keys sorted), without the autodate fields, the mutable fields, the pending-request fields and the hash. It includes the id, the finalize time, the signers and `seq`. Empty values are left out, so adding a field to the collection does not change old hashes.

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

It needs a bearer token, so a client fetches it and shows or prints the result (the admin UI of 87-5 will do that); a plain link in the address bar does not carry the token. Registrars, owners, admins and superusers of the entry's org can issue it. Every issue writes a `register_certificate` audit entry. A diocese's seal and wording are not built; whether a certificate is valid is the church's call.

## Data map and export

Both collections are registered with `retain` and the stated reason, so an export includes them and an erasure request ends as partial, with the reason (see `docs/data-map.md`). The annotation collection has a copy of the entry's `org` and a `subject` link, filled by the model hook.

## Not built yet

The admin UI for finalizing, annotating and the entry-with-margin view (87-5), webhook events, per-org certificate wording, and anonymization.
