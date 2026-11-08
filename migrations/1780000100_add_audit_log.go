package migrations

import (
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #74): creates the hidden, append-only _auditLog system
// collection. All rules are null (superusers only for list/view); the
// create/update/delete guards live in core/audit_model.go so they also hold
// for Go and JS hooks.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		if _, err := txApp.FindCollectionByNameOrId(core.CollectionNameAuditLog); err == nil {
			return nil // already created
		}

		col := core.NewBaseCollection(core.CollectionNameAuditLog)
		col.System = true

		text := func(name string) {
			col.Fields.Add(&core.TextField{Name: name, System: true})
		}
		jsonField := func(name string) {
			col.Fields.Add(&core.JSONField{Name: name, System: true, MaxSize: 4 << 20})
		}

		col.Fields.Add(&core.TextField{Name: "action", System: true, Required: true})
		text("actorType")
		text("actorCollection")
		text("actorId")
		text("targetCollectionId")
		text("targetCollection")
		text("targetId")
		jsonField("changedFields")
		jsonField("before")
		jsonField("after")
		text("ip")
		jsonField("meta")
		col.Fields.Add(&core.AutodateField{Name: "created", System: true, OnCreate: true})

		col.AddIndex("idx_auditLog_created", false, "created", "")
		col.AddIndex("idx_auditLog_record", false, "targetCollection, targetId", "")
		col.AddIndex("idx_auditLog_actor", false, "actorId", "")

		return txApp.Save(col)
	}, func(txApp core.App) error {
		// the collection is intentionally undeletable through the app;
		// reverting means dropping the table and its definition by hand
		return nil
	})
}
