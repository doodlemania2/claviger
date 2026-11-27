package migrations

import (
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/dialect"
)

// note: this migration will be deleted in future version

func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		// fork (#106): MySQL has no CREATE INDEX IF NOT EXISTS (the init migration created it)
		if d := core.DialectOf(txApp); d.Name() != dialect.MySQLName || d.IndexOwner(txApp.DB(), "idx__collections_type") == "" {
			_, err := txApp.DB().NewQuery(d.AdaptDDL("CREATE INDEX IF NOT EXISTS idx__collections_type on {{_collections}} ([[type]]);")).Execute()
			if err != nil {
				return err
			}
		}

		// reset mfas and otps delete rule
		collectionNames := []string{core.CollectionNameMFAs, core.CollectionNameOTPs}
		for _, name := range collectionNames {
			col, err := txApp.FindCollectionByNameOrId(name)
			if err != nil {
				return err
			}

			if col.DeleteRule != nil {
				col.DeleteRule = nil
				err = txApp.SaveNoValidate(col)
				if err != nil {
					return err
				}
			}
		}

		return nil
	}, nil)
}
