package migrations

import (
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #88, 88-4): erasure.
//
//   - _erasureLog: one row per erased subject, a one-way hash of the subject
//     id, the date and the collections touched. Nothing in it can be turned
//     back into a person. After a restore the app replays it and erases the
//     subjects that reappeared (core.ReplayErasureLog).
//   - _dataRequests gets the fields an erasure needs: the confirmation and
//     cancel token hashes (hidden), the grace period, the identity check
//     recorded by staff (meta) and the resume cursor of the job (progress).
//   - the _orgTransfers and _householdMembers data map entries are brought in
//     line with the erasure engine (a transfer of a person without a login is
//     reached through its member relation; the person's row is anonymized, not
//     deleted).
//
// The records API cannot create, change or delete a row of _erasureLog or an
// erasure row of _dataRequests (hooks in core/erasure.go): only the erasure
// functions write them.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		if _, err := txApp.FindCollectionByNameOrId(core.CollectionNameErasureLog); err != nil {
			col := core.NewBaseCollection(core.CollectionNameErasureLog)
			col.System = true
			col.Fields.Add(
				&core.TextField{Name: "subjectHash", System: true, Required: true, Max: 64},
				&core.DateField{Name: "erasedAt", System: true},
				&core.JSONField{Name: "collections", System: true, MaxSize: 1 << 20},
				// how many times a restore brought the subject back and the replay erased it again
				&core.NumberField{Name: "replays", System: true, OnlyInt: true},
				&core.DateField{Name: "lastReplayAt", System: true},
				&core.AutodateField{Name: "created", System: true, OnCreate: true},
				&core.AutodateField{Name: "updated", System: true, OnCreate: true, OnUpdate: true},
			)
			col.AddIndex("idx_erasureLog_subject", true, "subjectHash", "")

			if err := txApp.Save(col); err != nil {
				return err
			}
		}

		if err := core.RegisterDataMapEntry(txApp, core.BuiltinDataMapEntry(core.CollectionNameErasureLog)); err != nil {
			return err
		}

		if dr, err := txApp.FindCollectionByNameOrId(core.CollectionNameDataRequests); err == nil && dr.Fields.GetByName("graceUntil") == nil {
			dr.Fields.Add(
				&core.DateField{Name: "confirmedAt", System: true},
				&core.DateField{Name: "graceUntil", System: true},
				&core.DateField{Name: "cancelledAt", System: true},
				&core.TextField{Name: "tokenHash", System: true, Hidden: true, Max: 64},
				&core.DateField{Name: "tokenExpires", System: true, Hidden: true},
				&core.TextField{Name: "cancelHash", System: true, Hidden: true, Max: 64},
				&core.JSONField{Name: "meta", System: true, MaxSize: 1 << 16},
				&core.JSONField{Name: "progress", System: true, MaxSize: 1 << 20},
			)
			dr.AddIndex("idx_dataRequests_tokenHash", false, "tokenHash", "")
			dr.AddIndex("idx_dataRequests_cancelHash", false, "cancelHash", "")

			if err := txApp.Save(dr); err != nil {
				return err
			}
		}

		for _, name := range []string{core.CollectionNameOrgTransfers, core.CollectionNameHouseholdMembers} {
			if _, err := txApp.FindCollectionByNameOrId(name); err != nil {
				continue
			}

			if err := core.RegisterDataMapEntry(txApp, core.BuiltinDataMapEntry(name)); err != nil {
				return err
			}
		}

		return nil
	}, func(txApp core.App) error {
		if col, err := txApp.FindCollectionByNameOrId(core.CollectionNameErasureLog); err == nil {
			return txApp.Delete(col)
		}

		return nil
	})
}
