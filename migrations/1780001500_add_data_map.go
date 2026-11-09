package migrations

import (
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #88, 88-0): creates the _dataMap system collection and
// seeds one built-in entry for every system collection that exists at this
// point (core.builtinDataMap). A system collection created by a later
// migration registers itself with core.RegisterDataMapEntry. See
// docs/data-map.md.
//
// All rules are null (superusers only). Built-in rows are refused by the
// records API; superusers register their own collections there.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		if _, err := txApp.FindCollectionByNameOrId(core.CollectionNameDataMap); err == nil {
			return nil // already created
		}

		col := core.NewBaseCollection(core.CollectionNameDataMap)
		col.System = true
		col.Fields.Add(
			&core.TextField{Name: "collection", System: true, Required: true, Max: 100},
			&core.SelectField{Name: "treatment", System: true, Required: true, MaxSelect: 1, Values: core.DataMapTreatments},
			&core.JSONField{Name: "subjectPaths", System: true, MaxSize: 10000},
			&core.JSONField{Name: "personalFields", System: true, MaxSize: 10000},
			&core.TextField{Name: "retainReason", System: true, Max: 1000},
			&core.TextField{Name: "ageOutBasis", System: true, Max: 200},
			&core.TextField{Name: "note", System: true, Max: 2000},
			&core.BoolField{Name: "builtin", System: true},
			&core.AutodateField{Name: "created", System: true, OnCreate: true},
			&core.AutodateField{Name: "updated", System: true, OnCreate: true, OnUpdate: true},
		)
		col.AddIndex("idx_dataMap_collection", true, "collection", "")

		if err := txApp.Save(col); err != nil {
			return err
		}

		return core.SeedBuiltinDataMap(txApp)
	}, func(txApp core.App) error {
		if col, err := txApp.FindCollectionByNameOrId(core.CollectionNameDataMap); err == nil {
			return txApp.Delete(col)
		}
		return nil
	})
}
