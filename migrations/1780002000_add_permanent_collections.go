package migrations

import (
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #87, 87-1): creates the _permanentCollections system
// collection, the list of registers (collections whose final entries can
// never be deleted or changed, see docs/registers.md).
//
// All rules are null: only superusers list and register collections through
// the records API. The rules of a system collection cannot be edited
// afterwards. The model hook (core/register_hooks.go) validates a new row,
// adds the fields of a register to the collection, creates the annotation
// collection when it does not exist, and makes the guarantee hold.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		if _, err := txApp.FindCollectionByNameOrId(core.CollectionNamePermanentCollections); err == nil {
			return nil
		}

		col := core.NewBaseCollection(core.CollectionNamePermanentCollections)
		col.System = true
		col.Fields.Add(
			&core.TextField{Name: "collection", System: true, Required: true, Max: 100},
			&core.TextField{Name: "finalizeField", System: true, Max: 100},
			&core.JSONField{Name: "mutableFields", System: true, MaxSize: 5000},
			&core.TextField{Name: "annotationCollection", System: true, Max: 100},
			&core.BoolField{Name: "chain", System: true},
			&core.NumberField{Name: "signers", System: true, OnlyInt: true, Min: types.Pointer(1.0), Max: types.Pointer(2.0)},
			&core.JSONField{Name: "subjectPaths", System: true, MaxSize: 5000},
			&core.JSONField{Name: "personalFields", System: true, MaxSize: 5000},
			&core.TextField{Name: "retainReason", System: true, Max: 1000},
			&core.AutodateField{Name: "created", System: true, OnCreate: true},
			&core.AutodateField{Name: "updated", System: true, OnCreate: true, OnUpdate: true},
		)
		col.AddIndex("idx_permanentCollections_collection", true, "collection", "")

		if err := txApp.Save(col); err != nil {
			return err
		}

		return core.RegisterDataMapEntry(txApp, core.DataMapEntry{
			Collection: core.CollectionNamePermanentCollections, Treatment: core.DataMapTreatmentNone,
			Note: "The list of permanent registers (#87). Configuration, not about a person.",
		})
	}, func(txApp core.App) error {
		col, err := txApp.FindCollectionByNameOrId(core.CollectionNamePermanentCollections)
		if err != nil {
			return nil
		}

		return txApp.Delete(col)
	})
}
