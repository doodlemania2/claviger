package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #88, 88-7): optional anonymization of old register
// entries. Default off, per register (_permanentCollections.anonymize, with
// anonymizeAfterDays as the retention period).
//
// What it does to a final entry once the retention period after finalizing
// has passed and no legal hold covers it:
//
//   - every personal field (the register's personalFields) and every subject
//     link (subjectPaths) is replaced by a keyed pseudonym (text), a JSON
//     object holding the pseudonym, or cleared (dates, numbers, relations);
//   - anonymizedAt and anonDigest are set;
//   - a legal_note annotation records it (who, why, which fields, the digest);
//   - one audit entry is written.
//
// The hash chain stays valid. The stored hash of the entry is NOT changed: it
// is still the link its successor was computed from. Because the personal
// fields are gone, the entry's own hash can no longer be recomputed, so
// verification treats an anonymized entry as a tombstone and checks, instead,
//
//	anonDigest = SHA-256( prevHash "\n" hash "\n" canonical(anonymized entry) "\n" anonymizedAt )
//
// where prevHash is the stored hash of the previous entry of the chain,
// canonical is the same canonical form as for the chain (register_chain.go)
// read from the anonymized fields, and hash is the retained original link. The
// predecessor and successor are still verified through their own hashes, so a
// change to the tombstone, to its hash, to its place or to a neighbour is a
// break. What can no longer be proven is that the removed personal values were
// the ones that were first signed; that is the point of anonymizing.
//
// Pseudonyms are HMAC-SHA-256 values under the per-install secret
// (ErasurePseudonym's key) over the register, the entry, the field and the old
// value, so two entries never share a pseudonym and nobody without the secret
// can derive one from a guessed name. This is pseudonymization in the sense of
// GDPR Article 4(5): the old values are gone from the database, but whoever
// holds the secret and a candidate value can test it. It becomes anonymous
// only when the secret is destroyed too. docs/registers.md says so.

// registerNow is the clock of the retention period (replaced in tests).
var registerNow = time.Now

// SetRegisterClock replaces the clock of the anonymization retention period and
// returns a function that restores the real one. For tests; it is not safe to
// call while requests are served.
func SetRegisterClock(f func() time.Time) func() {
	prev := registerNow
	registerNow = f

	return func() { registerNow = prev }
}

const (
	registerAnonAtField     = "anonymizedAt"
	registerAnonDigestField = "anonDigest"
)

type registerAnonCtxKey struct{}

func registerAnonContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}

	return context.WithValue(ctx, registerAnonCtxKey{}, true)
}

func registerAnonAllowed(me *ModelEvent) bool {
	return me.Context != nil && me.Context.Value(registerAnonCtxKey{}) != nil
}

// anonymizableField reports whether an anonymization may write the field of a
// final entry.
func (c *PermanentConfig) anonymizableField(name string) bool {
	if name == registerAnonAtField || name == registerAnonDigestField {
		return true
	}

	if c.isManaged(name) || name == "id" || name == "org" || name == c.FinalizeField {
		return false
	}

	return slices.Contains(c.PersonalFields, name) || slices.Contains(c.SubjectPaths, name)
}

func registerIsAnonymized(r *Record) bool {
	return r.Collection().Fields.GetByName(registerAnonAtField) != nil && !r.GetDateTime(registerAnonAtField).IsZero()
}

func registerAnonDigest(prev string, rec *Record, cfg *PermanentConfig) string {
	h := sha256.New()
	h.Write([]byte(prev))
	h.Write([]byte("\n"))
	h.Write([]byte(rec.GetString("hash")))
	h.Write([]byte("\n"))
	h.Write(registerCanonical(rec, cfg, true))
	h.Write([]byte("\n"))
	h.Write([]byte(rec.GetDateTime(registerAnonAtField).Time().UTC().Format("2006-01-02 15:04:05.000Z")))

	return hex.EncodeToString(h.Sum(nil))
}

// verifyAnonymized checks the digest of an anonymized entry; "" when the entry
// is not anonymized or the digest holds.
func verifyAnonymized(r *Record, cfg *PermanentConfig, prev string) string {
	if !registerIsAnonymized(r) {
		return ""
	}

	if registerAnonDigest(prev, r, cfg) != r.GetString(registerAnonDigestField) {
		return "anonymized entry does not match its digest: the entry, its hash or its predecessor was changed outside the application"
	}

	return ""
}

// registerAnonymizeFields returns the fields of the entry's collection that an
// anonymization replaces.
func registerAnonymizeFields(cfg *PermanentConfig, col *Collection) []Field {
	var out []Field

	for _, f := range col.Fields {
		if cfg.anonymizableField(f.GetName()) && f.GetName() != registerAnonAtField && f.GetName() != registerAnonDigestField {
			out = append(out, f)
		}
	}

	return out
}

func registerPseudonym(app App, cfg *PermanentConfig, entry *Record, field string, old any) string {
	return "anon-" + erasureHMAC(app, fmt.Sprintf("claviger-register-pseudonym:%s:%s:%s:%v", cfg.Collection, entry.Id, field, old))[:20]
}

func registerPseudonymize(app App, cfg *PermanentConfig, entry *Record, f Field) bool {
	name := f.GetName()
	old := registerFieldValue(entry, name)

	if registerIsZero(old) {
		return false
	}

	switch f.Type() {
	case FieldTypeText, FieldTypeEmail, FieldTypeURL, FieldTypeEditor, FieldTypeEncryptedText:
		entry.Set(name, registerPseudonym(app, cfg, entry, name, old))
	case FieldTypeJSON, FieldTypeEncryptedJSON:
		entry.Set(name, map[string]any{"anonymized": registerPseudonym(app, cfg, entry, name, old)})
	case FieldTypeNumber:
		entry.Set(name, 0)
	case FieldTypeBool:
		entry.Set(name, false)
	default: // dates, relations, selects, files: no value is a pseudonym, so the value is removed
		entry.Set(name, "")
	}

	return true
}

// AnonymizeDueDate is when an entry finalized at the given time may be anonymized.
func (c *PermanentConfig) AnonymizeDueDate(finalized time.Time) time.Time {
	return finalized.AddDate(0, 0, c.AnonymizeAfterDays)
}

// AnonymizeRegisterEntry anonymizes one final entry. Only a registrar of the
// entry's org may do it (never a superuser), the register must have the
// setting on, the retention period since finalizing must have passed, and no
// legal hold may cover the entry. Irreversible.
func AnonymizeRegisterEntry(app App, ctx context.Context, auth *Record, collection, id, reason string) (*Record, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, orgErr(400, "A reason is required.")
	}

	if len(reason) > registerAnnotationReasonMx {
		return nil, orgErr(400, "The reason is too long.")
	}

	if err := EnsureErasureSecret(app); err != nil {
		return nil, err
	}

	var out *Record

	err := app.RunInTransaction(func(txApp App) error {
		cfg, entry, _, st, err := registerEntry(txApp, collection, id, auth)
		if err != nil {
			return err
		}

		if st.superuser {
			return orgErr(403, errSuperuserSigns)
		}

		if !st.registrar {
			return orgErr(403, "Only registrars can anonymize entries.")
		}

		out, err = anonymizeEntry(txApp, ctx, cfg, entry, auth.Id, reason)

		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// AnonymizeDueRegisterEntries anonymizes every final entry of the org whose
// retention period has passed and that is not held. It returns the ids it
// anonymized; entries that are not due or are held are skipped silently.
func AnonymizeDueRegisterEntries(app App, ctx context.Context, auth *Record, collection, orgId, reason string) ([]string, error) {
	cfg := PermanentConfigFor(app, collection)
	if cfg == nil {
		return nil, orgErr(404, "Register not found.")
	}

	acc, err := RegisterAccessFor(app, orgId, auth)
	if err != nil {
		return nil, err
	}

	if acc.Superuser {
		return nil, orgErr(403, errSuperuserSigns)
	}

	if !acc.Registrar {
		return nil, orgErr(403, "Only registrars can anonymize entries.")
	}

	if !cfg.Anonymize {
		return nil, orgErr(409, "Anonymization is not turned on for this register.")
	}

	if !registerNameRegex.MatchString(cfg.FinalizeField) {
		return nil, orgErr(500, "The register is misconfigured.")
	}

	cutoff := registerNow().AddDate(0, 0, -cfg.AnonymizeAfterDays).UTC().Format("2006-01-02 15:04:05.000Z")

	rows, err := app.FindRecordsByFilter(cfg.Collection,
		cfg.FinalizeField+" != '' && "+cfg.FinalizeField+" <= {:cut} && org = {:org}", "@rowid", 0, 0,
		map[string]any{"cut": cutoff, "org": orgId})
	if err != nil {
		return nil, err
	}

	var done []string

	for _, r := range rows {
		if registerIsAnonymized(r) {
			continue
		}

		_, err := AnonymizeRegisterEntry(app, ctx, auth, collection, r.Id, reason)

		switch statusOf(err) {
		case 0:
			if err != nil {
				return done, err
			}

			done = append(done, r.Id)
		case 409:
			// held, not due or nothing to remove: not an error of the batch
		default:
			return done, err
		}
	}

	return done, nil
}

func statusOf(err error) int {
	var oe *OrgError
	if errors.As(err, &oe) {
		return oe.Status
	}

	return 0
}

func anonymizeEntry(app App, ctx context.Context, cfg *PermanentConfig, entry *Record, author, reason string) (*Record, error) {
	if !cfg.Anonymize {
		return nil, orgErr(409, "Anonymization is not turned on for this register.")
	}

	if !cfg.IsFinalized(entry) {
		return nil, orgErr(409, "Only a final entry can be anonymized.")
	}

	if registerIsAnonymized(entry) {
		return nil, orgErr(409, "This entry is already anonymized.")
	}

	if due := cfg.AnonymizeDueDate(entry.GetDateTime(cfg.FinalizeField).Time()); registerNow().Before(due) {
		return nil, orgErr(409, "The retention period has not passed. The entry can be anonymized after "+due.UTC().Format("2006-01-02")+".")
	}

	col, err := app.FindCollectionByNameOrId(cfg.Collection)
	if err != nil {
		return nil, err
	}

	if held, err := LegalHeldAmong(app, col, []string{entry.Id}); err != nil {
		return nil, err
	} else if held[entry.Id] {
		return nil, orgErr(409, "The entry is under a legal hold and cannot be anonymized.")
	}

	if col.Fields.GetByName(registerAnonAtField) == nil || col.Fields.GetByName(registerAnonDigestField) == nil {
		if err := ensureRegisterFields(app, col, cfg); err != nil {
			return nil, err
		}

		if entry, err = app.FindRecordById(cfg.Collection, entry.Id); err != nil {
			return nil, err
		}

		col = entry.Collection()
	}

	prev := ""

	if cfg.Chain {
		if seq := entry.GetInt("seq"); seq > 1 {
			rows, err := app.FindRecordsByFilter(cfg.Collection, "seq = {:s}", "", 1, 0, map[string]any{"s": seq - 1})
			if err != nil || len(rows) == 0 {
				return nil, orgErr(409, "The chain is broken before this entry; verify the register first.")
			}

			prev = rows[0].GetString("hash")
		}
	}

	var names []string

	for _, f := range registerAnonymizeFields(cfg, col) {
		if registerPseudonymize(app, cfg, entry, f) {
			names = append(names, f.GetName())
		}
	}

	if len(names) == 0 {
		return nil, orgErr(409, "The entry has no personal values to remove.")
	}

	entry.Set(registerAnonAtField, types.NowDateTime())
	entry.Set(registerAnonDigestField, registerAnonDigest(prev, entry, cfg))

	if err := app.SaveWithContext(registerAnonContext(ctx), entry); err != nil {
		return nil, wrapOrgSaveErr(err)
	}

	stored := entry

	text := fmt.Sprintf("Personal values replaced by keyed pseudonyms after the retention period of %d days (fields: %s). Chain link kept: %s. Anonymization digest: %s.",
		cfg.AnonymizeAfterDays, strings.Join(names, ", "), stored.GetString("hash"), stored.GetString(registerAnonDigestField))
	if len(text) > registerAnnotationTextMax {
		text = text[:registerAnnotationTextMax]
	}

	note, err := newAnnotation(app, cfg, stored, RegisterAnnotationInput{Kind: RegisterKindLegalNote, Text: text, Reason: reason}, author)
	if err != nil {
		return nil, err
	}

	if err := app.SaveWithContext(ctx, note); err != nil {
		return nil, wrapOrgSaveErr(err)
	}

	if err := writeRegisterAudit(app, ctx, AuditActionRegisterAnonymize, cfg, stored, map[string]any{
		"annotation": note.Id, "fields": names, "digest": stored.GetString(registerAnonDigestField),
	}); err != nil {
		return nil, err
	}

	return stored, nil
}
