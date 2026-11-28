package core

import (
	"context"
	"strings"
)

// Fork-local (Claviger #88, 88-7): restriction of processing (GDPR Article 18)
// for entries of a permanent register.
//
// A restricted entry stays exactly as it is. It keeps its place in the hash
// chain (restricted is a mutable field and is not part of the hash), but its
// content is hidden:
//
//   - the records API redacts it for everyone but a superuser (the enrich hook
//     below), and the register routes redact it in lists and views;
//   - a registrar of the entry's org, or a superuser, can read it through the
//     register routes by stating a reason, which is written to the audit log
//     (register_restricted_read);
//   - every restriction and every lifting is a marginal note, so the register
//     itself says when and why.
//
// An erasure request against a final entry restricts it (AnnotateErasureRequest).
// A registrar can also restrict an entry directly (RestrictRegisterEntry), for
// example when a person objects to the accuracy of an entry. Only a registrar
// lifts a restriction, with a reason; a superuser cannot sign or annotate.

const registerRestrictedKeep = "restricted"

// RegisterReadResult is what a caller may see of an entry.
type RegisterReadResult struct {
	Config *PermanentConfig
	Entry  *Record
	Org    *Record
	// Restricted is the state of the entry.
	Restricted bool
	// Revealed is true when the caller may see the content: the entry is not
	// restricted, or a registrar or superuser gave a reason (which was audited).
	Revealed bool
	// CanReveal is true when the caller could reveal a restricted entry with a reason.
	CanReveal bool
}

// RegisterRestricted reports whether the entry is restricted.
func RegisterRestricted(entry *Record) bool {
	return entry.Collection().Fields.GetByName("restricted") != nil && entry.GetBool("restricted")
}

// RegisterRedactedFields lists the fields of a restricted entry that stay
// visible to a reader who has not revealed it: the id, the org, the
// restriction flag, the finalize time and the chain number.
func RegisterRedactedFields(cfg *PermanentConfig) []string {
	return []string{"id", "collectionId", "collectionName", "org", registerRestrictedKeep, cfg.FinalizeField, "seq"}
}

// ReadRegisterEntry resolves what the caller may see of an entry. A caller who
// is not staff of the org gets the usual 403 or 404. For a restricted entry the
// content is revealed only to a registrar or superuser who gives a reason; the
// reveal is audited.
func ReadRegisterEntry(app App, ctx context.Context, auth *Record, collection, id, reason string) (*RegisterReadResult, error) {
	cfg, entry, org, err := RegisterStaff(app, collection, id, auth)
	if err != nil {
		return nil, err
	}

	res := &RegisterReadResult{Config: cfg, Entry: entry, Org: org, Restricted: RegisterRestricted(entry)}

	if !res.Restricted {
		res.Revealed = true

		return res, nil
	}

	st := registerStandingOf(app, org, auth)
	res.CanReveal = st.registrar || st.superuser

	reason = strings.TrimSpace(reason)

	if !res.CanReveal || reason == "" {
		return res, nil
	}

	if len(reason) > registerAnnotationReasonMx {
		return nil, orgErr(400, "The reason is too long.")
	}

	if err := writeRegisterAudit(app, ctx, AuditActionRegisterRestrictedRead, cfg, entry, map[string]any{"reason": reason, "reader": auth.Id}); err != nil {
		return nil, err
	}

	res.Revealed = true

	return res, nil
}

// RestrictRegisterEntry restricts a final entry at a registrar's request.
func RestrictRegisterEntry(app App, ctx context.Context, auth *Record, collection, id, reason string) (*Record, error) {
	return setRegisterRestriction(app, ctx, auth, collection, id, reason, true)
}

// LiftRegisterRestriction lifts the restriction of an entry.
func LiftRegisterRestriction(app App, ctx context.Context, auth *Record, collection, id, reason string) (*Record, error) {
	return setRegisterRestriction(app, ctx, auth, collection, id, reason, false)
}

func setRegisterRestriction(app App, ctx context.Context, auth *Record, collection, id, reason string, restrict bool) (*Record, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, orgErr(400, "A reason is required.")
	}

	if len(reason) > registerAnnotationReasonMx {
		return nil, orgErr(400, "The reason is too long.")
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
			return orgErr(403, "Only registrars can change the restriction of an entry.")
		}

		if !cfg.IsFinalized(entry) {
			return orgErr(409, "Only a final entry can be restricted. A draft can be edited or deleted.")
		}

		if !cfg.isMutable("restricted") || entry.Collection().Fields.GetByName("restricted") == nil {
			return orgErr(409, "This register does not allow restriction.")
		}

		if RegisterRestricted(entry) == restrict {
			if restrict {
				return orgErr(409, "This entry is already restricted.")
			}

			return orgErr(409, "This entry is not restricted.")
		}

		in := RegisterAnnotationInput{Reason: reason}
		action := AuditActionRegisterUnrestrict

		if restrict {
			in.Kind = RegisterKindDataSubject
			in.Text = "Processing of this entry is restricted (GDPR Article 18). The entry is kept unchanged; it is hidden from normal reads."
			action = AuditActionRegisterRestrict
		} else {
			in.Kind = RegisterKindLegalNote
			in.Text = "The restriction of processing of this entry was lifted."
		}

		note, err := newAnnotation(txApp, cfg, entry, in, auth.Id)
		if err != nil {
			return err
		}

		if err := txApp.SaveWithContext(ctx, note); err != nil {
			return wrapOrgSaveErr(err)
		}

		entry.Set("restricted", restrict)

		if err := txApp.SaveWithContext(registerWriteContext(ctx), entry); err != nil {
			return wrapOrgSaveErr(err)
		}

		out = entry

		return writeRegisterAudit(txApp, ctx, action, cfg, entry, map[string]any{"annotation": note.Id})
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// registerEnrichRedact hides a restricted entry from the records API: only
// the fields of RegisterRedactedFields stay. A superuser sees everything (the
// admin UI is the superuser's tool; the audit log records superuser writes),
// and so does a call that has no request behind it (Go code).
func (app *BaseApp) registerEnrichHook() {
	app.OnRecordEnrich().BindFunc(func(e *RecordEnrichEvent) error {
		if e.RequestInfo == nil || e.RequestInfo.HasSuperuserAuth() {
			return e.Next()
		}

		cache := permanentConfigs(e.App)
		if len(cache.byRegister) == 0 {
			return e.Next()
		}

		if cfg := cache.byRegister[e.Record.Collection().Name]; cfg != nil && RegisterRestricted(e.Record) {
			keep := RegisterRedactedFields(cfg)

			for _, f := range e.Record.Collection().Fields {
				if !containsString(keep, f.GetName()) {
					e.Record.Hide(f.GetName())
				}
			}
		}

		return e.Next()
	})
}
