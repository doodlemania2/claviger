package core

import (
	"context"
	"strings"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #86, GDPR): pastoral notes about a person are not put
// in the person's automatic data export.
//
// A pastoral note can name or describe other people (the family, a
// confidant), so handing it to the person it is about is a decision for the
// org, not for a job. When the export reaches a note whose subject is the
// person, it records the note in _pastoralHolds as "held" and leaves it out;
// the manifest says how many notes are held, not what they say. An owner of the
// note's org reviews each hold (the review is a logged read) and either
// releases it, after which the person's next export includes the note, or
// withholds it with a reason. Superusers do not decide this: the owner of the
// org does.

const (
	PastoralHoldHeld      = "held"
	PastoralHoldReleased  = "released"
	PastoralHoldWithheld  = "withheld"
	AuditActionHoldCreate = "pastoral_hold"
	AuditActionHoldReview = "pastoral_hold_review"
	AuditActionHoldFree   = "pastoral_release"
	AuditActionHoldKeep   = "pastoral_withhold"
)

// PastoralHoldStates are the values of the state field.
var PastoralHoldStates = []string{PastoralHoldHeld, PastoralHoldReleased, PastoralHoldWithheld}

// holdPastoralForExport is called by the export for a note about the subject.
// It returns true when the note must be left out of the file (not released).
func holdPastoralForExport(app App, col *Collection, note *Record, subjectId string) (held bool, err error) {
	rows, err := app.FindCachedCollectionByNameOrId(CollectionNamePastoralHolds)
	if err != nil {
		// without the hold table nothing is exported: fail closed
		return true, nil
	}

	hold, err := app.FindFirstRecordByFilter(CollectionNamePastoralHolds, "collection={:c} && recordId={:r}",
		dbx.Params{"c": col.Name, "r": note.Id})
	if err == nil {
		return hold.GetString("state") != PastoralHoldReleased, nil
	}

	hold = NewRecord(rows)
	hold.Set("collection", col.Name)
	hold.Set("recordId", note.Id)
	hold.Set("org", note.GetString("org"))
	hold.Set("subject", subjectId)
	hold.Set("state", PastoralHoldHeld)
	hold.Set("heldAt", types.NowDateTime())

	if err := app.Save(hold); err != nil {
		return true, err
	}

	ctx := WithAuditContext(context.Background(), AuditActor{Type: AuditActorTypeSystem}, "")

	return true, writePastoralHoldAudit(app, ctx, AuditActionHoldCreate, hold, nil)
}

// PastoralHoldView is a hold with the note it points at.
type PastoralHoldView struct {
	Hold *Record
	// Note is set by ReviewPastoralHold only.
	Note *Record
}

func requireHoldOwner(app App, auth *Record, hold *Record) (*Record, error) {
	if auth == nil || auth.IsSuperuser() {
		return nil, orgErr(403, "Only an owner of the note's org can review held notes.")
	}

	org, err := app.FindRecordById(CollectionNameOrgs, hold.GetString("org"))
	if err != nil || !isOrgOwner(app, org, auth.Id) {
		return nil, orgErr(404, "Hold not found.")
	}

	return org, nil
}

// ListPastoralHolds lists the holds of an org for an owner of it. state "" lists all.
func ListPastoralHolds(app App, auth *Record, orgId, state string) ([]*Record, error) {
	if auth == nil || auth.IsSuperuser() {
		return nil, orgErr(403, "Only an owner of the org can review held notes.")
	}

	org, err := app.FindRecordById(CollectionNameOrgs, orgId)
	if err != nil || !isOrgOwner(app, org, auth.Id) {
		return nil, orgErr(404, "Org not found.")
	}

	filter := "org={:o}"
	params := dbx.Params{"o": org.Id}

	if state != "" {
		filter += " && state={:s}"
		params["s"] = state
	}

	return app.FindRecordsByFilter(CollectionNamePastoralHolds, filter, "-created", 200, 0, params)
}

// ReviewPastoralHold returns the hold with the note. The read is logged.
func ReviewPastoralHold(app App, ctx context.Context, auth *Record, holdId string) (*PastoralHoldView, error) {
	hold, err := app.FindRecordById(CollectionNamePastoralHolds, holdId)
	if err != nil {
		return nil, orgErr(404, "Hold not found.")
	}

	if _, err := requireHoldOwner(app, auth, hold); err != nil {
		return nil, err
	}

	col, err := app.FindCachedCollectionByNameOrId(hold.GetString("collection"))
	if err != nil {
		return nil, orgErr(404, "The note no longer exists.")
	}

	note, err := app.FindRecordById(col, hold.GetString("recordId"))
	if err != nil {
		return nil, orgErr(404, "The note no longer exists.")
	}

	if err := writePastoralHoldAudit(app, ctx, AuditActionRead, hold, map[string]any{
		"collection": col.Name, "recordIds": []string{note.Id}, "grantId": "", "purpose": "gdpr_review",
	}); err != nil {
		return nil, err
	}

	return &PastoralHoldView{Hold: hold, Note: note}, nil
}

// DecidePastoralHold releases or withholds a held note. A released note is part
// of the person's next export. A decision can be changed (release a withheld
// note, or withhold a released one); each change is logged.
func DecidePastoralHold(app App, ctx context.Context, auth *Record, holdId string, release bool, note string) (*Record, error) {
	hold, err := app.FindRecordById(CollectionNamePastoralHolds, holdId)
	if err != nil {
		return nil, orgErr(404, "Hold not found.")
	}

	if _, err := requireHoldOwner(app, auth, hold); err != nil {
		return nil, err
	}

	note = strings.TrimSpace(note)
	if len(note) > 1000 {
		return nil, orgErr(400, "The note is too long.")
	}

	state, action := PastoralHoldReleased, AuditActionHoldFree

	if !release {
		state, action = PastoralHoldWithheld, AuditActionHoldKeep

		if note == "" {
			return nil, orgErr(400, "A reason is required to withhold a note.")
		}
	}

	if hold.GetString("state") == state {
		return nil, orgErr(409, "The note is already "+state+".")
	}

	err = app.RunInTransaction(func(txApp App) error {
		fresh, err := txApp.FindRecordById(CollectionNamePastoralHolds, hold.Id)
		if err != nil {
			return orgErr(404, "Hold not found.")
		}

		fresh.Set("state", state)
		fresh.Set("decidedBy", auth.Id)
		fresh.Set("decidedAt", types.NowDateTime())
		fresh.Set("note", note)

		if err := txApp.Save(fresh); err != nil {
			return wrapOrgSaveErr(err)
		}

		hold = fresh

		return writePastoralHoldAudit(txApp, ctx, action, fresh, map[string]any{"decidedBy": auth.Id, "state": state})
	})
	if err != nil {
		return nil, err
	}

	return hold, nil
}

func writePastoralHoldAudit(app App, ctx context.Context, action string, hold *Record, extra map[string]any) error {
	if !auditAvailable(app) {
		return nil
	}

	meta := map[string]any{
		"orgId":      hold.GetString("org"),
		"noteId":     hold.GetString("recordId"),
		"collection": hold.GetString("collection"),
		"holdId":     hold.Id,
		"subject":    hold.GetString("subject"),
	}

	for k, v := range extra {
		meta[k] = v
	}

	entry := &AuditEntry{
		Action:         action,
		CollectionName: CollectionNamePastoralHolds,
		RecordId:       hold.Id,
		Meta:           meta,
	}

	if c, err := app.FindCachedCollectionByNameOrId(CollectionNamePastoralHolds); err == nil {
		entry.CollectionId = c.Id
	}

	if action == AuditActionRead {
		if col, err := app.FindCachedCollectionByNameOrId(hold.GetString("collection")); err == nil {
			entry.CollectionName, entry.CollectionId = col.Name, col.Id
			entry.RecordId = hold.GetString("recordId")
		}
	}

	applyAuditContext(ctx, entry)

	return WriteAuditEntry(app, ctx, entry, nil)
}
