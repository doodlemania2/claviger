package core

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"ganigeorgiev.com/pocketbase/dbx"
	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"github.com/doodlemania2/claviger/tools/hook"
	"github.com/doodlemania2/claviger/tools/router"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #88, 88-3): legal holds (spec 10.6).
//
// A hold freezes a subject (every row the data map ties to that person) or one
// record. While it is active:
//
//   - retention never deletes, anonymizes or archives a held row;
//   - no record in scope can be deleted by anything: a guard on the model
//     delete refuses it, so a cascade, the records API, a hook or the retention
//     job all stop at the same place. (The erasure job of 88-4 must also call
//     LegalHeldAmong before it anonymizes: an update is not guarded here.)
//
// The rows live in _legalHolds (system, superusers only). They are written by
// PlaceLegalHold and LiftLegalHold, which the /api/gdpr/holds routes call after
// a fresh passkey check; the records API refuses to create, change or delete a
// hold. A lifted hold stays as evidence. Both acts write one audit entry
// (gdpr_hold, gdpr_hold_lift) with ids only: never the reason or the reference.
//
// "Who a row is about" comes from the subject paths of the collection's data
// map entry, the same paths the personal data export uses. A collection with no
// entry (or a "none" entry) is never reached by a subject hold; a hold on one
// record always works.

const (
	LegalHoldScopeSubject = "subject"
	LegalHoldScopeRecord  = "record"

	AuditActionLegalHold     = "gdpr_hold"
	AuditActionLegalHoldLift = "gdpr_hold_lift"

	legalHoldReasonMax    = 1000
	legalHoldReferenceMax = 200
)

// LegalHoldScopes are the values of the scope field.
var LegalHoldScopes = []string{LegalHoldScopeSubject, LegalHoldScopeRecord}

// ErrLegalHoldLifted is returned when a hold is lifted a second time.
var ErrLegalHoldLifted = errors.New("the hold is already lifted")

// LegalHoldInput describes the hold to place.
type LegalHoldInput struct {
	Scope string `json:"scope" form:"scope"`

	// Subject (scope subject): the id of the person, and the name of their
	// auth collection (default: the org user collection).
	Subject           string `json:"subject" form:"subject"`
	SubjectCollection string `json:"subjectCollection" form:"subjectCollection"`

	// Collection and RecordId (scope record): the held record.
	Collection string `json:"collection" form:"collection"`
	RecordId   string `json:"recordId" form:"recordId"`

	Reason    string `json:"reason" form:"reason"`
	Reference string `json:"reference" form:"reference"`
}

// LegalHoldsAvailable reports whether the _legalHolds collection exists.
func LegalHoldsAvailable(app App) bool {
	_, err := app.FindCachedCollectionByNameOrId(CollectionNameLegalHolds)

	return err == nil
}

// PlaceLegalHold creates the hold and its audit entry in one transaction.
// placedBy is the superuser who placed it. Invalid input comes back as
// validation.Errors.
func PlaceLegalHold(app App, ctx context.Context, placedBy *Record, in LegalHoldInput) (*Record, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	col, err := app.FindCachedCollectionByNameOrId(CollectionNameLegalHolds)
	if err != nil {
		return nil, errors.New("legal holds are not available")
	}

	in.Scope = strings.TrimSpace(in.Scope)
	in.Subject = strings.TrimSpace(in.Subject)
	in.Collection = strings.TrimSpace(in.Collection)
	in.RecordId = strings.TrimSpace(in.RecordId)
	in.Reason = strings.TrimSpace(in.Reason)
	in.Reference = strings.TrimSpace(in.Reference)

	errs := validation.Errors{}

	if in.Reason == "" {
		errs["reason"] = validation.NewError("validation_required", "A reason is required.")
	} else if len(in.Reason) > legalHoldReasonMax {
		errs["reason"] = validation.NewError("validation_max_text_constraint", "The reason is too long.")
	}

	if len(in.Reference) > legalHoldReferenceMax {
		errs["reference"] = validation.NewError("validation_max_text_constraint", "The reference is too long.")
	}

	rec := NewRecord(col)

	switch in.Scope {
	case LegalHoldScopeSubject:
		if in.SubjectCollection == "" {
			in.SubjectCollection = OrgUserCollectionName()
		}

		subCol, err := app.FindCachedCollectionByNameOrId(in.SubjectCollection)
		if err != nil || !subCol.IsAuth() {
			errs["subjectCollection"] = validation.NewError("validation_unknown_collection", "No auth collection with this name exists.")
		} else if in.Subject == "" {
			errs["subject"] = validation.NewError("validation_required", "A subject is required.")
		} else if _, err := app.FindRecordById(subCol, in.Subject); err != nil {
			errs["subject"] = validation.NewError("validation_not_found", "No such person.")
		}

		rec.Set("subject", in.Subject)
		rec.Set("subjectCollection", in.SubjectCollection)
	case LegalHoldScopeRecord:
		target, err := app.FindCachedCollectionByNameOrId(in.Collection)
		switch {
		case err != nil || target.IsView():
			errs["collection"] = validation.NewError("validation_unknown_collection", "No collection with this name exists.")
		case in.RecordId == "":
			errs["recordId"] = validation.NewError("validation_required", "A record id is required.")
		default:
			if _, err := app.FindRecordById(target, in.RecordId); err != nil {
				errs["recordId"] = validation.NewError("validation_not_found", "No such record.")
			}
		}

		rec.Set("collection", in.Collection)
		rec.Set("recordId", in.RecordId)
	default:
		errs["scope"] = validation.NewError("validation_in_invalid", "The scope must be subject or record.")
	}

	if err := errs.Filter(); err != nil {
		return nil, err
	}

	rec.Set("scope", in.Scope)
	rec.Set("reason", in.Reason)
	rec.Set("reference", in.Reference)

	if placedBy != nil {
		rec.Set("placedBy", placedBy.Id)
	}

	rec.Set("placedAt", types.NowDateTime())

	err = app.RunInTransaction(func(txApp App) error {
		if err := txApp.SaveWithContext(legalHoldWriteContext(ctx), rec); err != nil {
			return err
		}

		return writeLegalHoldAudit(txApp, ctx, AuditActionLegalHold, rec)
	})
	if err != nil {
		return nil, err
	}

	return rec, nil
}

// LiftLegalHold ends a hold (it stays in the table). A second lift returns
// ErrLegalHoldLifted.
func LiftLegalHold(app App, ctx context.Context, id string, liftedBy *Record) (*Record, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	var rec *Record

	err := app.RunInTransaction(func(txApp App) error {
		var err error

		rec, err = txApp.FindRecordById(CollectionNameLegalHolds, id)
		if err != nil {
			return err
		}

		if !rec.GetDateTime("liftedAt").IsZero() {
			return ErrLegalHoldLifted
		}

		rec.Set("liftedAt", types.NowDateTime())

		if liftedBy != nil {
			rec.Set("liftedBy", liftedBy.Id)
		}

		if err := txApp.SaveWithContext(legalHoldWriteContext(ctx), rec); err != nil {
			return err
		}

		return writeLegalHoldAudit(txApp, ctx, AuditActionLegalHoldLift, rec)
	})
	if err != nil {
		return nil, err
	}

	return rec, nil
}

// ActiveLegalHolds returns the holds that are not lifted.
func ActiveLegalHolds(app App) ([]*Record, error) {
	if !LegalHoldsAvailable(app) {
		return nil, nil
	}

	return app.FindRecordsByFilter(CollectionNameLegalHolds, `liftedAt = ""`, "placedAt", 0, 0)
}

// LegalHeldAmong returns which of the record ids of col are under an active
// hold: a hold on the record itself, or a hold on a subject the record is
// about according to the data map. ids that are not held are absent.
func LegalHeldAmong(app App, col *Collection, ids []string) (map[string]bool, error) {
	out := map[string]bool{}

	if col == nil || len(ids) == 0 {
		return out, nil
	}

	holds, err := ActiveLegalHolds(app)
	if err != nil || len(holds) == 0 {
		return out, err
	}

	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}

	var subjects []*Record

	for _, h := range holds {
		switch h.GetString("scope") {
		case LegalHoldScopeRecord:
			if h.GetString("collection") == col.Name {
				if _, ok := wanted[h.GetString("recordId")]; ok {
					out[h.GetString("recordId")] = true
				}
			}
		case LegalHoldScopeSubject:
			subjects = append(subjects, h)
		}
	}

	if len(subjects) == 0 {
		return out, nil
	}

	paths := legalHoldSubjectPaths(app, col.Name)
	if len(paths) == 0 {
		return out, nil
	}

	inIds := make([]any, 0, len(ids))
	for _, id := range ids {
		inIds = append(inIds, id)
	}

	done := map[string]struct{}{}

	for _, h := range subjects {
		key := h.GetString("subjectCollection") + "/" + h.GetString("subject")
		if _, ok := done[key]; ok {
			continue
		}

		done[key] = struct{}{}

		subCol, err := app.FindCachedCollectionByNameOrId(h.GetString("subjectCollection"))
		if err != nil {
			continue
		}

		// the account may be gone while the hold stays: a stub with the id
		// still matches every path that stores the id
		subject, err := app.FindRecordById(subCol, h.GetString("subject"))
		if err != nil {
			subject = NewRecord(subCol)
			subject.Id = h.GetString("subject")
		}

		where, matches, _ := dataExportSubjectFilter(app, col, paths, subject)
		if where == nil {
			continue
		}

		var rows []*Record

		if err := app.RecordQuery(col).AndWhere(dbx.And(dbx.In("id", inIds...), where)).All(&rows); err != nil {
			return out, err
		}

		for _, r := range rows {
			if matches == nil || matches(r) {
				out[r.Id] = true
			}
		}
	}

	return out, nil
}

// IsRecordLegallyHeld reports whether the record is under an active hold.
func IsRecordLegallyHeld(app App, rec *Record) (bool, error) {
	if rec == nil || rec.Collection() == nil {
		return false, nil
	}

	held, err := LegalHeldAmong(app, rec.Collection(), []string{rec.Id})

	return held[rec.Id], err
}

func legalHoldSubjectPaths(app App, collection string) []string {
	row, err := app.FindFirstRecordByData(CollectionNameDataMap, "collection", collection)
	if err != nil || row.GetString("treatment") == DataMapTreatmentNone {
		return nil
	}

	var paths []string

	_ = row.UnmarshalJSONField("subjectPaths", &paths)

	return paths
}

func writeLegalHoldAudit(app App, ctx context.Context, action string, hold *Record) error {
	if !auditAvailable(app) {
		return nil
	}

	meta := map[string]any{
		"holdId": hold.Id,
		"scope":  hold.GetString("scope"),
	}

	if hold.GetString("scope") == LegalHoldScopeSubject {
		meta["subjectCollection"] = hold.GetString("subjectCollection")
		meta["subject"] = hold.GetString("subject")
	} else {
		meta["collection"] = hold.GetString("collection")
		meta["recordId"] = hold.GetString("recordId")
	}

	entry := &AuditEntry{
		Action:         action,
		CollectionId:   hold.Collection().Id,
		CollectionName: CollectionNameLegalHolds,
		RecordId:       hold.Id,
		ChangedFields:  []string{},
		Meta:           meta,
	}
	applyAuditContext(ctx, entry)

	return WriteAuditEntry(app, ctx, entry, nil)
}

// ---- hooks ---------------------------------------------------------------------

type legalHoldWriteCtxKey struct{}

// legalHoldWriteContext marks a write as coming from PlaceLegalHold or
// LiftLegalHold. The key type is unexported, so no route, hook or script can
// forge it.
func legalHoldWriteContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, legalHoldWriteCtxKey{}, true)
}

func (app *BaseApp) registerLegalHoldHooks() {
	app.OnRecordValidate(CollectionNameLegalHolds).Bind(&hook.Handler[*RecordEvent]{
		Id: "__clvLegalHoldValidate__",
		Func: func(e *RecordEvent) error {
			if e.Context == nil || e.Context.Value(legalHoldWriteCtxKey{}) == nil {
				return validation.Errors{"scope": validation.NewError("validation_forbidden", "Holds are placed and lifted through /api/gdpr/holds.")}
			}

			return e.Next()
		},
	})

	for _, bindFn := range []func(tags ...string) *hook.TaggedHook[*RecordRequestEvent]{
		app.OnRecordCreateRequest, app.OnRecordUpdateRequest, app.OnRecordDeleteRequest,
	} {
		bindFn(CollectionNameLegalHolds).Bind(&hook.Handler[*RecordRequestEvent]{
			Id:       "__clvLegalHoldGuard__",
			Priority: -100000,
			Func: func(e *RecordRequestEvent) error {
				return router.NewForbiddenError("Holds are placed and lifted through /api/gdpr/holds, with a fresh passkey check.", nil)
			},
		})
	}

	// nothing deletes a held record: not a request, not a cascade, not the
	// retention job
	app.OnModelDelete().Bind(&hook.Handler[*ModelEvent]{
		Id:       "__clvLegalHoldDelete__",
		Priority: -95000,
		Func: func(me *ModelEvent) error {
			re, ok := newRecordEventFromModelEvent(me)
			if !ok || re.Record.Collection().Name == CollectionNameLegalHolds || !LegalHoldsAvailable(me.App) {
				return me.Next()
			}

			held, err := IsRecordLegallyHeld(me.App, re.Record)
			if err != nil {
				return fmt.Errorf("failed to check the legal holds: %w", err)
			}

			if held {
				return router.NewApiError(409, "The record is under a legal hold and cannot be deleted.", nil)
			}

			return me.Next()
		},
	})
}
