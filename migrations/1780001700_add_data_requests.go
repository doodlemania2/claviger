package migrations

import (
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #88, 88-1): creates the _dataRequests system
// collection, one row per data subject request (an export now; erasure is a
// later milestone). The finished export ZIP is the row's hidden, protected
// file, so it lives in the app file storage (local or S3), is removed with
// the row, and is deleted by the expiry cron.
//
// All rules are null (superusers only through the records API). People use
// the /api/me/export and /api/gdpr routes, which check who may see which row.
//
// The built-in data map entry is registered here (the data map seed ran in
// 1780001500) and listed in core.builtinDataMap for fresh databases.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		if _, err := txApp.FindCollectionByNameOrId(core.CollectionNameDataRequests); err == nil {
			return nil // already created
		}

		col := core.NewBaseCollection(core.CollectionNameDataRequests)
		col.System = true

		col.Fields.Add(&core.SelectField{Name: "type", System: true, Required: true, MaxSelect: 1, Values: core.DataRequestTypes})
		col.Fields.Add(&core.SelectField{Name: "state", System: true, Required: true, MaxSelect: 1, Values: core.DataRequestStates})
		// the subject
		col.Fields.Add(&core.TextField{Name: "collectionRef", System: true, Required: true})
		col.Fields.Add(&core.TextField{Name: "recordRef", System: true, Required: true})
		// who asked (the subject for self service, a superuser otherwise)
		col.Fields.Add(&core.TextField{Name: "requestedByCollection", System: true})
		col.Fields.Add(&core.TextField{Name: "requestedBy", System: true})
		col.Fields.Add(&core.BoolField{Name: "selfService", System: true})
		col.Fields.Add(&core.DateField{Name: "dueAt", System: true})
		col.Fields.Add(&core.DateField{Name: "startedAt", System: true})
		col.Fields.Add(&core.DateField{Name: "completedAt", System: true})
		col.Fields.Add(&core.DateField{Name: "expires", System: true})
		col.Fields.Add(&core.DateField{Name: "downloadedAt", System: true})
		// counts and what was left out; never content
		col.Fields.Add(&core.JSONField{Name: "result", System: true, MaxSize: 1 << 20})
		col.Fields.Add(&core.TextField{Name: "error", System: true, Max: 2000})
		col.Fields.Add(&core.FileField{
			Name: "file", System: true, MaxSelect: 1, MaxSize: 1 << 40,
			Protected: true, Hidden: true,
		})
		col.Fields.Add(&core.AutodateField{Name: "created", System: true, OnCreate: true})
		col.Fields.Add(&core.AutodateField{Name: "updated", System: true, OnCreate: true, OnUpdate: true})

		col.AddIndex("idx_dataRequests_subject", false, "collectionRef, recordRef", "")
		col.AddIndex("idx_dataRequests_state", false, "state", "")

		if err := txApp.Save(col); err != nil {
			return err
		}

		return core.RegisterDataMapEntry(txApp, core.DataMapEntry{
			Collection:   core.CollectionNameDataRequests,
			Treatment:    core.DataMapTreatmentErase,
			SubjectPaths: []string{"recordRef"},
			Note:         "Export: the request, its state and dates (never the export file). The export ZIP is deleted with the row.",
		})
	}, func(txApp core.App) error {
		col, err := txApp.FindCollectionByNameOrId(core.CollectionNameDataRequests)
		if err != nil {
			return nil
		}
		return txApp.Delete(col)
	})
}
