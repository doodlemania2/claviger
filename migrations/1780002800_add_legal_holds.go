package migrations

import (
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #88, 88-3): creates the _legalHolds system collection
// (a person, or one record, frozen against erasure and retention) and registers
// it in the data map. All rules are null: the records API shows it to
// superusers only, and holds are placed and lifted through /api/gdpr/holds,
// which needs a fresh passkey check and writes the audit entry. See
// docs/retention.md.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		if _, err := txApp.FindCollectionByNameOrId(core.CollectionNameLegalHolds); err != nil {
			col := core.NewBaseCollection(core.CollectionNameLegalHolds)
			col.System = true
			col.Fields.Add(
				&core.SelectField{Name: "scope", System: true, Required: true, MaxSelect: 1, Values: core.LegalHoldScopes},
				// scope subject: the person (an id, as text: the account may go away)
				&core.TextField{Name: "subject", System: true, Max: 100},
				&core.TextField{Name: "subjectCollection", System: true, Max: 100},
				// scope record
				&core.TextField{Name: "collection", System: true, Max: 100},
				&core.TextField{Name: "recordId", System: true, Max: 100},
				&core.TextField{Name: "reason", System: true, Required: true, Max: 1000},
				&core.TextField{Name: "reference", System: true, Max: 200},
				&core.TextField{Name: "placedBy", System: true, Max: 100},
				&core.DateField{Name: "placedAt", System: true},
				&core.DateField{Name: "liftedAt", System: true},
				&core.TextField{Name: "liftedBy", System: true, Max: 100},
				&core.AutodateField{Name: "created", System: true, OnCreate: true},
				&core.AutodateField{Name: "updated", System: true, OnCreate: true, OnUpdate: true},
			)
			col.AddIndex("idx_legalHolds_state", false, "scope, liftedAt", "")
			col.AddIndex("idx_legalHolds_subject", false, "subjectCollection, subject", "")
			col.AddIndex("idx_legalHolds_record", false, "collection, recordId", "")

			if err := txApp.Save(col); err != nil {
				return err
			}
		}

		return core.RegisterDataMapEntry(txApp, core.BuiltinDataMapEntry(core.CollectionNameLegalHolds))
	}, func(txApp core.App) error {
		if col, err := txApp.FindCollectionByNameOrId(core.CollectionNameLegalHolds); err == nil {
			return txApp.Delete(col)
		}

		return nil
	})
}
