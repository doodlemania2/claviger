package migrations

import (
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #88, 88-3): creates the _retentionPolicies system
// collection (what the retention job deletes, anonymizes or archives, and when)
// and registers it in the data map. All rules are null: the records API shows
// it to superusers only, and every write goes through
// /api/gdpr/retention/policies, which creates policies disabled and as a dry
// run. Nothing is seeded: no policy ships enabled. See docs/retention.md.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		if _, err := txApp.FindCollectionByNameOrId(core.CollectionNameRetentionPolicies); err != nil {
			col := core.NewBaseCollection(core.CollectionNameRetentionPolicies)
			col.System = true
			col.Fields.Add(
				&core.TextField{Name: "name", System: true, Max: 200},
				&core.TextField{Name: "collection", System: true, Required: true, Max: 100},
				// created, updated, ageOut or a date field of the collection
				&core.TextField{Name: "basis", System: true, Required: true, Max: 100},
				&core.NumberField{Name: "after", System: true, OnlyInt: true, Min: types.Pointer(0.0), Max: types.Pointer(36500.0)},
				&core.SelectField{Name: "unit", System: true, Required: true, MaxSelect: 1, Values: core.RetentionUnits},
				&core.SelectField{Name: "action", System: true, Required: true, MaxSelect: 1, Values: core.RetentionActions},
				&core.TextField{Name: "filter", System: true, Max: 2000},
				&core.BoolField{Name: "enabled", System: true},
				&core.BoolField{Name: "dryRun", System: true},
				// the state of a run in progress, so that a restart continues it
				&core.TextField{Name: "runId", System: true, Max: 100},
				&core.SelectField{Name: "runStatus", System: true, MaxSelect: 1, Values: []string{"running", "done"}},
				&core.JSONField{Name: "runCounts", System: true, MaxSize: 20000},
				&core.DateField{Name: "lastRunAt", System: true},
				&core.JSONField{Name: "lastResult", System: true, MaxSize: 20000},
				&core.AutodateField{Name: "created", System: true, OnCreate: true},
				&core.AutodateField{Name: "updated", System: true, OnCreate: true, OnUpdate: true},
			)
			col.AddIndex("idx_retentionPolicies_collection", false, "collection", "")
			col.AddIndex("idx_retentionPolicies_enabled", false, "enabled, runStatus", "")

			if err := txApp.Save(col); err != nil {
				return err
			}
		}

		return core.RegisterDataMapEntry(txApp, core.BuiltinDataMapEntry(core.CollectionNameRetentionPolicies))
	}, func(txApp core.App) error {
		if col, err := txApp.FindCollectionByNameOrId(core.CollectionNameRetentionPolicies); err == nil {
			return txApp.Delete(col)
		}

		return nil
	})
}
