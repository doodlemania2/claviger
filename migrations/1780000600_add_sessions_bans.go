package migrations

import (
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #93): creates the hidden _sessions and _bans system
// collections. All rules are null (superusers only through the records API);
// users reach their own sessions through the dedicated /api/sessions endpoints.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		if _, err := txApp.FindCollectionByNameOrId(core.CollectionNameSessions); err != nil {
			col := core.NewBaseCollection(core.CollectionNameSessions)
			col.System = true

			col.Fields.Add(&core.TextField{Name: "collectionRef", System: true, Required: true})
			col.Fields.Add(&core.TextField{Name: "recordRef", System: true, Required: true})
			col.Fields.Add(&core.AutodateField{Name: "created", System: true, OnCreate: true})
			col.Fields.Add(&core.DateField{Name: "lastSeen", System: true})
			col.Fields.Add(&core.DateField{Name: "expires", System: true})
			col.Fields.Add(&core.TextField{Name: "userAgent", System: true, Max: 255})
			col.Fields.Add(&core.TextField{Name: "ip", System: true, Max: 64})
			col.Fields.Add(&core.TextField{Name: "authMethod", System: true, Max: 32})
			col.Fields.Add(&core.BoolField{Name: "revoked", System: true})
			col.Fields.Add(&core.TextField{Name: "revokedReason", System: true, Max: 255})

			col.AddIndex("idx_sessions_owner", false, "collectionRef, recordRef", "")
			col.AddIndex("idx_sessions_expires", false, "expires", "")

			if err := txApp.Save(col); err != nil {
				return err
			}
		}

		if _, err := txApp.FindCollectionByNameOrId(core.CollectionNameBans); err != nil {
			col := core.NewBaseCollection(core.CollectionNameBans)
			col.System = true

			col.Fields.Add(&core.TextField{Name: "collectionRef", System: true, Required: true})
			col.Fields.Add(&core.TextField{Name: "recordRef", System: true, Required: true})
			col.Fields.Add(&core.TextField{Name: "reason", System: true, Max: 500})
			col.Fields.Add(&core.DateField{Name: "until", System: true})
			col.Fields.Add(&core.TextField{Name: "createdBy", System: true, Max: 100})
			col.Fields.Add(&core.AutodateField{Name: "created", System: true, OnCreate: true})

			col.AddIndex("idx_bans_owner", true, "collectionRef, recordRef", "")

			if err := txApp.Save(col); err != nil {
				return err
			}
		}

		return nil
	}, func(txApp core.App) error {
		for _, name := range []string{core.CollectionNameSessions, core.CollectionNameBans} {
			if col, err := txApp.FindCollectionByNameOrId(name); err == nil {
				if err := txApp.Delete(col); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
