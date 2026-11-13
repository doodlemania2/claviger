package core

import (
	"context"
	"strings"
)

// Fork-local (Claviger #87, 87-2): annotations of final entries.
//
// An annotation is the margin note of a paper register: who, when, why. It is
// only allowed on a final entry, and it can never be changed or deleted (the
// model hook refuses it for every caller, like the audit log). A mistaken
// annotation is not removed; a "retraction" points to it with supersedes.

// RegisterAnnotationInput is the request to annotate an entry.
type RegisterAnnotationInput struct {
	Kind       string
	Text       string
	Reason     string
	Supersedes string
}

// AddRegisterAnnotation adds an annotation to a final entry. Only a registrar
// of the entry's org (or a superuser) may annotate.
func AddRegisterAnnotation(app App, ctx context.Context, auth *Record, collection, id string, in RegisterAnnotationInput) (*Record, error) {
	var out *Record

	err := app.RunInTransaction(func(txApp App) error {
		cfg, entry, _, st, err := registerEntry(txApp, collection, id, auth)
		if err != nil {
			return err
		}

		if !st.registrar {
			return orgErr(403, "Only registrars can annotate entries.")
		}

		if !cfg.IsFinalized(entry) {
			return orgErr(409, "Only a final entry can be annotated. Edit the draft instead.")
		}

		rec, err := newAnnotation(txApp, cfg, entry, in, auth.Id)
		if err != nil {
			return err
		}

		if err := txApp.SaveWithContext(ctx, rec); err != nil {
			return wrapOrgSaveErr(err)
		}

		out = rec

		extra := map[string]any{"annotation": rec.Id, "kind": rec.GetString("kind")}
		if s := rec.GetString("supersedes"); s != "" {
			extra["supersedes"] = s
		}

		return writeRegisterAudit(txApp, ctx, AuditActionRegisterAnnotate, cfg, entry, extra)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func newAnnotation(app App, cfg *PermanentConfig, entry *Record, in RegisterAnnotationInput, author string) (*Record, error) {
	col, err := app.FindCollectionByNameOrId(cfg.AnnotationCollection)
	if err != nil {
		return nil, orgErr(404, "The annotation collection is missing.")
	}

	rec := NewRecord(col)
	rec.Set("entry", entry.Id)
	rec.Set("kind", in.Kind)
	rec.Set("text", strings.TrimSpace(in.Text))
	rec.Set("reason", strings.TrimSpace(in.Reason))
	rec.Set("supersedes", in.Supersedes)
	rec.Set("author", author)

	if _, err := validateAnnotationRecord(app, cfg, rec); err != nil {
		return nil, orgErr(400, err.Error())
	}

	return rec, nil
}

// RegisterAnnotations returns the annotations of an entry, oldest first.
func RegisterAnnotations(app App, cfg *PermanentConfig, entry *Record) ([]*Record, error) {
	return app.FindRecordsByFilter(cfg.AnnotationCollection, "entry={:entry}", "created,@rowid", 0, 0, map[string]any{"entry": entry.Id})
}

// AnnotateErasureRequest records that the entry's subject asked for erasure of
// a final entry that is kept: a data_subject_note annotation with the reason,
// and the entry is restricted (the one mutable field) when the register has
// that field. It is meant to be called by the erasure feature (#88) for a
// collection whose data map treatment is "retain". It does not anonymize
// anything. The entry must be final; a draft can simply be deleted.
//
// ctx should carry the audit context of whoever handles the request; without
// one the annotation is attributed to the system.
func AnnotateErasureRequest(app App, ctx context.Context, entry *Record, reason string) (*Record, error) {
	cfg := PermanentConfigFor(app, entry.Collection().Name)
	if cfg == nil {
		return nil, orgErr(400, "This entry does not belong to a permanent register.")
	}

	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, orgErr(400, "A reason is required.")
	}

	var out *Record

	err := app.RunInTransaction(func(txApp App) error {
		stored, err := txApp.FindRecordById(cfg.Collection, entry.Id)
		if err != nil {
			return orgErr(404, "Entry not found.")
		}

		if !cfg.IsFinalized(stored) {
			return orgErr(409, "Only a final entry is kept. Delete the draft instead.")
		}

		text := "The data subject asked for erasure. The entry is kept: " + cfg.RetainReason
		if len(text) > registerAnnotationTextMax {
			text = text[:registerAnnotationTextMax]
		}

		rec, err := newAnnotation(txApp, cfg, stored, RegisterAnnotationInput{
			Kind: RegisterKindDataSubject, Text: text, Reason: reason,
		}, "system")
		if err != nil {
			return err
		}

		if err := txApp.SaveWithContext(ctx, rec); err != nil {
			return wrapOrgSaveErr(err)
		}

		out = rec

		if cfg.isMutable("restricted") && stored.Collection().Fields.GetByName("restricted") != nil {
			stored.Set("restricted", true)

			if err := txApp.SaveWithContext(ctx, stored); err != nil {
				return wrapOrgSaveErr(err)
			}
		}

		return writeRegisterAudit(txApp, ctx, AuditActionRegisterErasureNote, cfg, stored, map[string]any{"annotation": rec.Id})
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
