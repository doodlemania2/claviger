package migrations

import (
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #75, M3): adds the strictMode column to _fieldKeys.
//
// "1" on the active key row of a field means the field is in strict mode:
// a stored value without the enc:v1: prefix is a read error instead of being
// read as legacy plaintext. It lives here, and not in the field options,
// because the options are part of the exported collection JSON.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		col, err := txApp.FindCollectionByNameOrId(core.CollectionNameFieldKeys)
		if err != nil {
			return nil // the base migration did not run (nothing to extend)
		}

		if col.Fields.GetByName("strictMode") != nil {
			return nil // already added
		}

		col.Fields.Add(&core.TextField{Name: "strictMode", System: true})

		return txApp.Save(col)
	}, func(txApp core.App) error {
		return nil
	})
}
