# Spike: owner-held key vault for sealed pastoral notes (#86-6)

Status: design only. No code is written for this. Nothing here is built.
Related: #86 (pastoral notes, break-glass), #75 (field encryption), #88 (GDPR tools). Read first: `docs/pastoral-notes.md`, `docs/field-encryption.md`, and section 8 of `docs/specs/80-88-congregations.md`, especially 8.5.

## The question

Today a sealed pastoral note is protected by rules and by field encryption. Field encryption protects the database file and backups. It does not protect against the server operator, because the server holds the key and decrypts for any request the rules allow, and a superuser bypasses the rules (spec 8.5).

Can a parish opt in to a mode where the **parish holds the key and the server cannot read sealed notes**, and what would that cost?

## Short answer

Yes, it is possible. It is a large change, and it takes away things the server does today. I recommend a staged, opt-in vault that starts with **author-held keys** and adds **threshold escrow for break-glass** only in a second stage. Do not start before the owner answers the open questions at the end. The biggest limit is not the cryptography. It is that **the same server that you distrust delivers the JavaScript that does the decrypting**. See the threat model.

## What exists today

| Piece | Fact | Source |
|---|---|---|
| Note body | `encryptedText` field, AES-256-GCM, one data key (DEK) per field, wrapped by one key-encryption key (KEK) | `docs/field-encryption.md` |
| Who can read | Author; tagged members by visibility; break-glass grantee; **superuser** | `docs/pastoral-notes.md` |
| Break-glass | Two people, fresh passkey each, owner told first, at most 24 hours, every read logged | `docs/pastoral-notes.md` |
| Export | A note about a person is held for owner review, never in the automatic export | `docs/pastoral-notes.md` |
| Passkeys | WebAuthn registration and login exist. **There is no use of the PRF or largeBlob extension anywhere in the repo** (searched) | repo search |
| Recovery | Staff-vouched recovery **removes the person's old passkeys** and creates a new one | `docs/recovery.md` |

One DEK per field, not per note, is a property of the existing design. A vault needs a key per note or per author, so it cannot reuse `encryptedText` as it is.

## Threat model

What the vault is meant to stop, and what it is not.

| # | Adversary | Today | With a vault |
|---|---|---|---|
| T1 | Someone with a database file or backup, no key | Blocked | Blocked |
| T2 | Someone with a backup and the KEK (stolen env, cloud key admin) | Reads everything | Blocked for vault notes. The KEK is not enough |
| T3 | A curious or compromised **operator or superuser** reading live data | Reads everything (reads are logged) | Blocked for stored data. Metadata still visible |
| T4 | A **malicious operator who changes the code** the browser downloads (hosted pages, admin UI) | Not relevant, already reads everything | **Not blocked.** They can ship a page that sends the key or the text to themselves |
| T5 | A court order or request to the host for the data | Host can produce plaintext | Host can produce ciphertext only. The parish gets the request instead |
| T6 | An org admin or owner | Blocked by rules | Blocked by rules and by keys |
| T7 | A trusted key holder who abuses access | Break-glass limits and logs it | Same, but the server can no longer prove what was decrypted (see break-glass) |
| T8 | A thief with the author's device | Needs the author's login | Needs the author's authenticator and its user verification (PIN, biometric) |

Out of scope: a compromised author device, a note author who writes something they should not, and sacramental confession, which must not be stored at all (`docs/pastoral-notes.md`).

Two attacks need an explicit answer in any design:

- **Code delivery (T4).** End-to-end encryption in a browser is only as honest as the code the server sends. Options: a native or desktop client with signed releases, a browser extension, or published and pinned hashes of the hosted pages that holders check. None is free. Be plain with the owner: a vault in the browser **defends against T1 to T3, T5 and T6, not against T4**, unless a signed client is built.
- **Key substitution.** The server stores each holder's public key. A hostile server could hand out its own key in place of a holder's. Holders need to compare short fingerprints of each other's keys once, out of band (in person, or read over a phone call), and the client should warn when a key changes.

## What the server can still see

Even with a vault, the server needs these in the clear to apply rules, run retention and audit: `org`, `author`, `subject`, `visibility`, `sealed`, `kind`, `expiresAt`, `archived`, timestamps, and the **size** of the ciphertext. An operator learns that a note exists, who wrote it, who it is about, and roughly how long it is. If the owner treats the existence of a note about a named person as sensitive, the vault does not hide that. Padding hides length. Hiding `subject` would break the data export and erasure flows, so it is not proposed.

## Candidate designs

All three assume the same base: the note text is encrypted in the browser with a fresh random 256-bit **content key (CEK)** (AES-256-GCM, with the record id and collection id bound as authenticated data, as `encryptedText` does). The server stores `ciphertext` and one or more **envelopes**: the CEK encrypted to somebody. They differ in where the unwrapping keys come from and who holds them.

### Design A. Author key from the passkey (WebAuthn PRF), per-person vault

Each person who writes sealed notes has a **vault key pair** (for example X25519). The private half is generated in the browser and stored on the server **only in wrapped form**. The wrapping key comes from the WebAuthn **PRF extension**: the authenticator computes a secret from a salt the page supplies, and the result never leaves the person's authenticator except into the page. The note's CEK is encrypted to the author's public key.

How it works:

1. Enrollment. The person registers or uses a passkey with PRF. The page asks the authenticator for PRF output with a fixed salt (for example the string `claviger/vault/v1` plus the org id). It generates the key pair, wraps the private key with a key derived (HKDF) from the PRF output, and uploads public key plus wrapped private key.
2. Writing. The page encrypts the note with a new CEK, encrypts the CEK to the author's public key, uploads both.
3. Reading. The author signs in, the page asks for PRF output again (one more prompt), unwraps the private key, decrypts the CEK, decrypts the note.

Pros:
- No secret to remember. Nothing new to carry for people who already use passkeys.
- A passkey with user verification protects the key. The server holds only wrapped keys.
- No coordination among people. Works for the large majority of sealed notes (author-only).

Cons:
- Depends on PRF support in the person's browser and authenticator. See the support section. Some will not be able to enroll.
- The PRF secret belongs to **one credential**. If that credential is lost, the key is lost. This is a real conflict with Claviger's staff-vouched recovery, which **removes old passkeys** by design. A recovered person would lose their vault, so enrollment must also create another unlock path (below).
- A synced passkey may or may not carry its PRF secret to the person's other devices, depending on the passkey provider. Not confirmed here.
- Does not help break-glass by itself.

### Design B. Parish vault key held by key-holders, wrapped per holder

The parish (org) has one **vault key pair**. The private half is **wrapped separately to each key holder's public key** (each holder has a personal key pair as in A, or a passphrase-derived one). Notes are encrypted to the org vault public key, and optionally also to the author. Anyone with a wrapped copy can read all notes at that level.

Pros:
- Familiar model (a shared vault, as in password managers). Easy to explain.
- Adding a holder is one new wrapped copy, made by an existing holder in their browser.
- Fits notes that clergy are meant to read (visibility `clergy`, `care`) so they can be read by several people without the author.

Cons:
- Any single holder can read **every** note in the vault. That is a bigger power than break-glass gives today, which is two people, one note, 24 hours, logged. It does not model sealed (author-only) notes.
- Removing a holder does not take back what they have: you must **rotate** the vault key and re-encrypt every note's CEK, which needs a holder online and touches every note.
- It makes break-glass meaningless for the notes it covers, because holders can read them anyway.

### Design C. Threshold split (Shamir) among N holders

The vault key (or an escrow key, below) is **split into N shares so that any K of them rebuild it** (for example 2 of 3 clergy). No single person holds the key. Shares are encrypted to each holder's personal public key and stored on the server, or held offline.

Two ways to use the split:

- **C1. Rebuild in one browser.** K holders each decrypt their share and send it (encrypted) to the person who rebuilds the key. The rebuilt key can decrypt **everything** under that key, for as long as the page holds it. Time-boxing is by honor, not by mathematics.
- **C2. Threshold decryption.** Holders each compute a partial result for **one specific note** and the pieces are combined. The key is never rebuilt, so one grant opens one note. This is the property break-glass wants. It needs threshold public-key cryptography (for example threshold ElGamal over a curve) and a careful key ceremony. It is the strongest and the hardest to build correctly. It should use a reviewed library, and a cryptographer should review the protocol.

Pros:
- Matches two-person break-glass: K = 2.
- No single person can open anything.

Cons:
- If fewer than K holders remain reachable, the escrow is lost.
- Holder changes mean re-splitting, which needs K current holders.
- Share handling is a new class of support problem for a parish office.
- C2 is novel to Claviger and not a small task.

## Recommended shape

Combine A for ownership with a **per-org escrow key** for break-glass, and build C2 only if the owner accepts the cost; otherwise accept C1's honor-based time limit.

- Every sealed note's CEK is encrypted to **the author** (Design A).
- It is also encrypted to the org's **escrow public key**. The escrow private key never exists in one place. It is split K-of-N among the current holders of the `breakglass` tag (Design C).
- Notes at lower visibility levels (`clergy`, `care`, `congregation`) are **not** put in the vault in the first stage. They keep today's rules and field encryption. The vault is for `private` and `sealed` notes. This keeps the scope to the notes that most need it.

## Break-glass when the server cannot decrypt

Today the server grants access and the rules let the grantee read. With a vault, the server can grant and log, but **granting no longer decrypts anything**. The grantee needs cryptographic help from other holders.

A workable flow, in the C2 form:

1. A `breakglass` holder asks, with reason and basis, as now. Fresh passkey, as now.
2. A second holder approves, as now. The owner and org owners are told before the grant opens, as now. The server still refuses (503) when nobody can be told.
3. The server marks the grant active and serves the ciphertext and the escrow envelope to the grantee.
4. The **two holders' browsers** each compute a partial decryption for that one note, and the grantee's browser combines them and decrypts. The approver's click in step 2 now has to be followed by their browser doing the partial step (a second prompt for their authenticator).
5. The server logs the fetch, as now.

What changes in meaning:

- **Two-person becomes real.** Today it is a rule the server enforces. With a vault it is also a cryptographic fact: one person cannot decrypt alone, even a malicious operator with one holder's cooperation.
- **Reads are no longer provable.** The server sees that the ciphertext was fetched, not that it was decrypted. The audit entry says "fetched". `readCount` counts fetches.
- **Revocation is by the clock only.** After decryption the plaintext is on the grantee's screen, as it is today. Revoking cannot take it back. What revocation stops is a second fetch and a second partial decryption.
- **A superuser still cannot ask or approve** (today's rule stays).
- With C1 instead of C2, the grantee holds the rebuilt escrow key in the page and could decrypt every vault note for as long as that page lives. The 24-hour limit is then a promise, not a mechanism. State this to the owner.

## Recovery and people changes

| Event | What happens | What is lost |
|---|---|---|
| Author loses a device but has another enrolled authenticator | Unlock with the other one | Nothing |
| Author loses **all** authenticators | Staff-vouched recovery issues a new passkey (it removes old ones). The new passkey has no access to the old vault | The author's own sealed notes, unless there is a recovery key or an escrow open (below) |
| Author leaves the parish | Their notes stay encrypted to them and to the escrow. The org can break-glass them under the rules | Nothing, if escrow is intact |
| Author dies | Same as leaves. Break-glass with the basis `owner_unavailable` | Nothing, if escrow is intact |
| A break-glass holder leaves | Remove the tag. Re-split the escrow key among the new holder list. This needs K current holders to cooperate and the server cannot do it | Holders who keep an old share could still contribute to a rebuild with other old shares. Rotation of the escrow key and re-wrapping of every note's CEK is the full fix. It is heavy |
| A holder dies | Same as leaves | If K holders do not remain, the escrow is lost forever |
| Fewer than K holders remain | Nobody can break-glass | The escrow notes, until authors read them themselves |

To make "loses all authenticators" survivable the design should **require at enrollment**: a second PRF-capable authenticator, **or** a printed recovery key (for example 24 words) that unwraps the same vault key. Make the recovery key the default fallback because not everyone owns two authenticators. Store it like the KEK: not on the server, not in the backup, ideally with two people.

The staff-vouched recovery page must **warn** that recovering an account that has a vault will not restore the vault, and must not offer to "reset" what it cannot reset.

## What stops working on the server

| Feature | With the vault |
|---|---|
| Search in the body | Already absent (spec 8.2). No change |
| Any server code that reads the body (webhooks, mail, reports) | Cannot. Webhook payloads already redact the body |
| Rules and visibility | Unchanged. They use metadata, not the body |
| Read audit | Records fetches, not decryptions |
| Data export to the subject (`docs/data-export.md`) | The server cannot put the text in a ZIP. A note about a person stays "held for review" (already the default). To release it, the **author or an escrow quorum** must decrypt it and the export needs a path to receive the plaintext. This is new work |
| Data export of the author's own notes | Same. The author's browser must supply the text |
| Erasure (`docs/erasure.md`) | Works. Deleting the row deletes the ciphertext. Unlinking the subject (`core.UnlinkPastoralSubject`) is metadata. Erasing the author's account loses access to their notes, which erasure already says it does not delete |
| Retention and expiry (`docs/retention.md`) | Works. It uses `expiresAt`, `archived`, `archivedAt`, all metadata |
| Legal hold, backups, restore | Work. Backups hold ciphertext. Restore needs no vault key |
| Field-key rotation (`keys rotate`) | Does not touch vault notes. The KEK no longer matters for them |
| Prayer-request renewal | Works (metadata) |
| Moving a note to a lower visibility | The author's browser has to re-encrypt the note, since it leaves the vault |
| Admin recovery of a lost note | Impossible. This is the point and the cost |

New server work, for the record: a per-note ciphertext field type (no server-held key), an envelope collection or JSON field, a key directory (public keys, fingerprints, change warnings), and routes to serve envelopes only to permitted callers. Rules do not need to change.

## WebAuthn PRF: browser and authenticator support

**I could not check live support while writing this.** What I believe, from memory, and must be re-checked before any plan depends on it:

- PRF is an extension in WebAuthn Level 3. At the authenticator level it is built on the CTAP2 `hmac-secret` extension. The page passes a salt, the authenticator returns a secret that is stable for that credential and salt.
- Chromium-based desktop browsers have supported it for a while (around version 116 for security keys; I believe platform support came later). Safari added support in its 18 release family, for iCloud Keychain passkeys (I am not sure of the exact versions and conditions). Firefox: I am not sure, I believe it arrived in desktop versions in 2025 and may depend on the operating system.
- Security keys that support `hmac-secret` (for example recent YubiKeys) work in browsers that implement PRF. Not every key supports it.
- Windows Hello as a platform authenticator: I believe PRF was not supported at the time I last knew. Treat as unknown.
- Whether a **synced** passkey keeps the **same** PRF output on every device the person syncs to depends on the passkey provider and its vendor. I am not sure for each. A test matrix is needed.
- Registration may report only whether PRF is **enabled**, and produce the secret on a later assertion. The person may need an extra prompt at enrollment.
- The `largeBlob` extension is an alternative to PRF that stores a small blob on the authenticator. Support is narrower than PRF as far as I know. I do not recommend it as the base.
- Each use of the key is a new user-verification prompt. Reading a sealed note costs a prompt.

Action for the implementation task: write a one-page browser and device matrix from real tests (Chrome, Edge, Safari, Firefox on Windows, macOS, iOS, Android; iCloud Keychain, Google Password Manager, a password manager, a hardware key). Decide the minimum supported set from that. Until then do not promise "works with any passkey".

Fallback if PRF is missing: a passphrase-derived key (Argon2id in the browser) as the unlock method. That brings back human memory and weak-passphrase risk, so it should be a deliberate second choice.

## Options that cost less (not a vault)

If the real fear is T2 or T5, a vault may be more than needed:

- **Parish-owned KEK.** Put the KEK in a key vault the parish controls (Azure Key Vault, AWS KMS, Google KMS are supported key sources, `docs/field-encryption.md`) and let the parish revoke the host's access. The server still sees plaintext while running. Covers T2 and, partly, T5. Exists today.
- **Read audit and alerts** on superuser reads of sealed notes. Exists today for the records API.
- **A separate instance for pastoral data**, run by the parish, with its own operator. Moves the trust, does not remove it.

State these to the owner so the vault is chosen for what only it gives: protection from the live operator (T3) and a no-plaintext answer to a legal request (T5).

## Risks of building it

- Cryptographic protocol mistakes. Mitigation: use platform primitives (Web Crypto) and a reviewed threshold library; independent review before release.
- Lost keys and angry users. Mitigation: required recovery key at enrollment, clear text at opt-in, a test that proves the recovery key works.
- False comfort. A parish may believe the operator can never read anything. The docs must state T4 in plain words.
- Large client code in the hosted pages, which today are small and have strict CSP.
- Interaction with staff-vouched recovery and org transfers (a person moving between orgs takes no vault with them).

## Staging if the owner says yes

1. **Prototype**, no merge: PRF enrollment and read/write of one author-only note in a test page; the browser matrix; measure prompts.
2. **Author vault** (A) for `private` and `sealed` notes, with required recovery key. Break-glass is disabled for vault notes, and the page says so. Export for vault notes comes through the author's browser.
3. **Escrow and break-glass** (C), starting with C1 and the honest limits, then C2 only after a cryptography review.
4. **Key rotation and holder changes**, with a clear runbook.

The work item stays XL (spec 8.10). Stage 1 alone is about an M.

## Open questions for the owner

1. Who is the threat? If it is "the host operator" or "a court order to the host", the vault is the right tool. If it is only "a stolen backup", field encryption already covers it and a parish-owned KEK is enough.
2. Is a lost note acceptable? Sealed notes are unreadable to everyone if the author and the escrow are lost. Is that a feature or a failure for parishes?
3. Is the code-delivery limit (T4) acceptable in a browser, or is a signed native or desktop client required for the vault to be worth building?
4. Break-glass: is C1 (honor-based 24 hours) acceptable, or must one grant open one note only (C2, a bigger build with a crypto review)?
5. Which unlock methods are required: PRF only, PRF plus a printed recovery key, or a passphrase as well?
6. What should happen to a vault when the person is recovered by staff? Today recovery removes their old passkeys.
7. Should vault notes be exportable to the subject at all? With a vault, "held for review" means a human with the key has to open the note.
8. Who holds the `breakglass` tag, and is K of N (for example 2 of 3) realistic for a small parish? A parish with one priest cannot form a quorum.
9. Does the owner accept that metadata (who wrote a note about whom, and when) is still visible to the operator?
10. Does a lawyer need to review the case where a reporting duty applies and the host cannot produce plaintext (spec section 12, item 5)?
