package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"github.com/doodlemania2/claviger/tools/hook"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #86, 86-5): expiring prayer requests (spec 8.6).
//
// A note of kind "prayer" gets expiresAt = now + the org's prayerExpiryDays
// (an org setting, 7 to 730, default 90) when it is created without one. The
// author can set any expiresAt, and can renew a note (RenewPastoralNote). The
// expiry itself is not a job of its own: the retention engine (retention.go)
// archives at expiresAt and deletes after the archive period, through two
// ordinary policies (docs/retention.md). Archiving sets archived, which the
// locked read rule hides from everyone but the author; archivedAt is kept by
// the hook below and starts the archive period.

const (
	PastoralKindCare   = "care"
	PastoralKindPrayer = "prayer"

	// PastoralExpiryDaysSetting is the key in _orgs.settings.
	PastoralExpiryDaysSetting = "prayerExpiryDays"
	DefaultPastoralExpiryDays = 90
	MinPastoralExpiryDays     = 7
	MaxPastoralExpiryDays     = 730
	AuditActionPastoralRenew  = "pastoral_renew"
	pastoralExpiryLock        = ` && @request.body.kind:isset = false && @request.body.archivedAt:isset = false`
)

// PastoralKinds are the values of the kind field.
var PastoralKinds = []string{PastoralKindCare, PastoralKindPrayer}

// PastoralExpiryDays returns the default life of a prayer request in the org.
func PastoralExpiryDays(org *Record) int {
	if org == nil {
		return DefaultPastoralExpiryDays
	}

	if v, ok := credentialOrgSettings(org)[PastoralExpiryDaysSetting].(float64); ok &&
		v == float64(int(v)) && int(v) >= MinPastoralExpiryDays && int(v) <= MaxPastoralExpiryDays {
		return int(v)
	}

	return DefaultPastoralExpiryDays
}

func validateOrgPastoralExpiry(e *RecordEvent) error {
	if v, ok := credentialOrgSettings(e.Record)[PastoralExpiryDaysSetting]; ok {
		f, isNum := v.(float64)
		if !isNum || f != float64(int(f)) || int(f) < MinPastoralExpiryDays || int(f) > MaxPastoralExpiryDays {
			return validation.Errors{"settings": validation.NewError("validation_prayer_expiry_days",
				fmt.Sprintf("%s must be a whole number from %d to %d.", PastoralExpiryDaysSetting, MinPastoralExpiryDays, MaxPastoralExpiryDays))}
		}
	}

	return e.Next()
}

func (app *BaseApp) registerPastoralExpiryHooks() {
	app.OnRecordValidate(CollectionNameOrgs).Bind(&hook.Handler[*RecordEvent]{
		Id:   "__clvPastoralExpirySetting__",
		Func: validateOrgPastoralExpiry,
	})

	app.OnRecordCreate().Bind(&hook.Handler[*RecordEvent]{
		Id: "__clvPastoralExpiryCreate__",
		Func: func(e *RecordEvent) error {
			col := e.Record.Collection()

			if !IsPastoralCollection(col) {
				return e.Next()
			}

			if col.Fields.GetByName("kind") != nil && e.Record.GetString("kind") == PastoralKindPrayer &&
				col.Fields.GetByName("expiresAt") != nil && e.Record.GetDateTime("expiresAt").IsZero() {
				var org *Record
				if o, err := e.App.FindRecordById(CollectionNameOrgs, e.Record.GetString("org")); err == nil {
					org = o
				}

				e.Record.Set("expiresAt", types.NowDateTime().Add(time.Duration(PastoralExpiryDays(org))*24*time.Hour))
			}

			if col.Fields.GetByName("archivedAt") != nil {
				if e.Record.GetBool("archived") {
					e.Record.Set("archivedAt", types.NowDateTime())
				} else {
					e.Record.Set("archivedAt", "")
				}
			}

			return e.Next()
		},
	})

	// archivedAt is the server's: it is set when archived turns true, kept
	// while it stays true, and cleared when it turns false. Whoever archives
	// (the retention job, a moderator, the author) cannot choose it.
	app.OnRecordUpdate().Bind(&hook.Handler[*RecordEvent]{
		Id: "__clvPastoralExpiryUpdate__",
		Func: func(e *RecordEvent) error {
			col := e.Record.Collection()

			if !IsPastoralCollection(col) || col.Fields.GetByName("archivedAt") == nil {
				return e.Next()
			}

			orig := e.Record.Original()
			wasArchived := orig != nil && orig.GetBool("archived")

			switch {
			case !e.Record.GetBool("archived"):
				e.Record.Set("archivedAt", "")
			case wasArchived && orig != nil && !orig.GetDateTime("archivedAt").IsZero():
				e.Record.Set("archivedAt", orig.GetDateTime("archivedAt"))
			default:
				e.Record.Set("archivedAt", types.NowDateTime())
			}

			return e.Next()
		},
	})
}

// ErrPastoralNotAuthor is returned when someone but the author renews a note.
var ErrPastoralNotAuthor = errors.New("only the author can renew a note")

// RenewPastoralNote gives a note a new expiry (days from now; 0 means the
// org's default) and brings it back from the archive. Only the author, as an
// active member of the note's org, can do it: nobody else, and no superuser.
// It writes one audit entry (pastoral_renew) with ids and the new date.
func RenewPastoralNote(app App, ctx context.Context, col *Collection, noteId string, actor *Record, days int) (*Record, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	if !IsPastoralCollection(col) || col.Fields.GetByName("expiresAt") == nil {
		return nil, errors.New("not a pastoral notes collection")
	}

	if days < 0 || days > MaxPastoralExpiryDays {
		return nil, validation.Errors{"days": validation.NewError("validation_prayer_expiry_days",
			fmt.Sprintf("days must be from 1 to %d.", MaxPastoralExpiryDays))}
	}

	var note *Record

	err := app.RunInTransaction(func(txApp App) error {
		var err error

		note, err = txApp.FindRecordById(col, noteId)
		if err != nil {
			return err
		}

		if actor == nil || actor.IsSuperuser() || note.GetString("author") != actor.Id {
			return ErrPastoralNotAuthor
		}

		org, err := txApp.FindRecordById(CollectionNameOrgs, note.GetString("org"))
		if err != nil {
			return err
		}

		if ActiveOrgMember(txApp, org, actor.Id) == nil {
			return ErrPastoralNotAuthor
		}

		if days == 0 {
			days = PastoralExpiryDays(org)
		}

		expires := types.NowDateTime().Add(time.Duration(days) * 24 * time.Hour)

		note.Set("expiresAt", expires)
		note.Set("archived", false)

		if err := txApp.SaveWithContext(ctx, note); err != nil {
			return err
		}

		if !auditAvailable(txApp) {
			return nil
		}

		entry := &AuditEntry{
			Action:         AuditActionPastoralRenew,
			CollectionId:   col.Id,
			CollectionName: col.Name,
			RecordId:       note.Id,
			ChangedFields:  []string{},
			Meta:           map[string]any{"orgId": org.Id, "noteId": note.Id, "collection": col.Name, "expiresAt": expires.String(), "days": days},
		}
		applyAuditContext(ctx, entry)

		return WriteAuditEntry(txApp, ctx, entry, nil)
	})
	if err != nil {
		return nil, err
	}

	return note, nil
}

// UpgradePastoralExpiry adds the kind and archivedAt fields to the pastoral
// notes collections made before 86-5, and moves their update rule to the new
// locked text when it is exactly the old locked one. A rule somebody changed
// is left alone. It returns the names of the collections it changed.
func UpgradePastoralExpiry(app App) ([]string, error) {
	cols, err := app.FindAllCollections(CollectionTypeBase)
	if err != nil {
		return nil, err
	}

	var changed []string

	for _, col := range cols {
		if !IsPastoralCollection(col) {
			continue
		}

		dirty := false

		if col.Fields.GetByName("kind") == nil {
			col.Fields.Add(&SelectField{Name: "kind", MaxSelect: 1, Values: PastoralKinds})

			dirty = true
		}

		if col.Fields.GetByName("archivedAt") == nil {
			col.Fields.Add(&DateField{Name: "archivedAt"})

			dirty = true
		}

		want := PastoralRules(col.Name)
		legacy := strings.Replace(*want.Update, pastoralExpiryLock, "", 1)

		if col.UpdateRule != nil && *col.UpdateRule == legacy {
			col.UpdateRule = want.Update
			dirty = true
		}

		if !dirty {
			continue
		}

		if err := app.Save(col); err != nil {
			return changed, err
		}

		changed = append(changed, col.Name)
	}

	return changed, nil
}
