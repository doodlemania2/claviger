package migrations

import (
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #88, 88-7): the optional, per-register anonymization of
// old entries (see docs/registers.md). Adds two settings to
// _permanentCollections; both default to off. The two entry fields a register
// needs for it are added to the register itself when the setting is turned on.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		col, err := txApp.FindCollectionByNameOrId(core.CollectionNamePermanentCollections)
		if err != nil {
			return nil
		}

		changed := false

		if col.Fields.GetByName("anonymize") == nil {
			col.Fields.Add(&core.BoolField{Name: "anonymize", System: true})

			changed = true
		}

		if col.Fields.GetByName("anonymizeAfterDays") == nil {
			col.Fields.Add(&core.NumberField{Name: "anonymizeAfterDays", System: true, OnlyInt: true, Min: types.Pointer(0.0), Max: types.Pointer(36500.0)})

			changed = true
		}

		if !changed {
			return nil
		}

		return txApp.Save(col)
	}, func(txApp core.App) error {
		col, err := txApp.FindCollectionByNameOrId(core.CollectionNamePermanentCollections)
		if err != nil {
			return nil
		}

		col.Fields.RemoveByName("anonymize")
		col.Fields.RemoveByName("anonymizeAfterDays")

		return txApp.Save(col)
	})
}
