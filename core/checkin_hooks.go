package core

import (
	"fmt"

	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"github.com/doodlemania2/claviger/tools/hook"
)

// Fork-local (Claviger #82, 82-1): the append-only guard of checkin_log and the
// consistency checks of the pickup list.

func (app *BaseApp) registerCheckinHooks() {
	// checkin_log is append-only for the records API, Go hooks and JS hooks
	// (not raw SQL), the same way _auditLog is. Creating is open to code: the
	// collection has null write rules, so only the routes create. The retention
	// job ends rows after purgeAfter; nothing else may delete.
	guard := func(me *ModelEvent) error {
		re, ok := newRecordEventFromModelEvent(me)
		if !ok || re.Record.Collection().Name != CheckinLogCollection {
			return me.Next()
		}

		if me.Type == ModelEventTypeDelete && me.Context != nil && me.Context.Value(retentionDeleteCtxKey{}) != nil {
			return me.Next()
		}

		return fmt.Errorf("the %s collection is append-only: %s is not allowed", CheckinLogCollection, me.Type)
	}

	app.OnModelUpdate().Bind(&hook.Handler[*ModelEvent]{Id: "__pbCheckinLogGuard__", Func: guard, Priority: -100000})
	app.OnModelDelete().Bind(&hook.Handler[*ModelEvent]{Id: "__pbCheckinLogGuard__", Func: guard, Priority: -100000})

	// a pickup authorization names a child of its own household, in that household's
	// org (the org on the row is a copy the rules read); household and child never move
	app.OnRecordValidate(PickupAuthCollection).Bind(&hook.Handler[*RecordEvent]{
		Id: "__pbPickupAuthValidate__",
		Func: func(e *RecordEvent) error {
			r := e.Record

			if orig := r.Original(); !r.IsNew() && orig != nil {
				if orig.GetString("household") != r.GetString("household") || orig.GetString("child") != r.GetString("child") {
					return validation.Errors{"child": validation.NewError("validation_immutable", "The household and the child cannot change.")}
				}
			}

			h, err := e.App.FindRecordById(CollectionNameHouseholds, r.GetString("household"))
			if err != nil {
				return validation.Errors{"household": validation.NewError("validation_not_found", "No such household.")}
			}

			c, err := e.App.FindRecordById(CollectionNameHouseholdMembers, r.GetString("child"))
			if err != nil || c.GetString("household") != h.Id || !c.GetBool("isMinor") {
				return validation.Errors{"child": validation.NewError("validation_invalid_child", "The child must be a minor of the household.")}
			}

			if ab := r.GetString("approvedBy"); ab != "" {
				if g, err := e.App.FindRecordById(CollectionNameHouseholdMembers, ab); err != nil || g.GetString("household") != h.Id || !g.GetBool("guardian") {
					return validation.Errors{"approvedBy": validation.NewError("validation_invalid_guardian", "The approver must be a guardian of the household.")}
				}
			}

			r.Set("org", h.GetString("org"))

			return e.Next()
		},
	})
}
