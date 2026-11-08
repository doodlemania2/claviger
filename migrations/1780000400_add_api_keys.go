package migrations

import (
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #95): creates the hidden _apiKeys system collection.
//
// All rules are null (superusers only through the records API). Users manage
// their own keys through the dedicated /api/apikeys endpoints, which never
// return secretHash. secretHash is Hidden as a second barrier, and its name
// makes the audit log redact it.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		if _, err := txApp.FindCollectionByNameOrId(core.CollectionNameAPIKeys); err == nil {
			return nil // already created
		}

		col := core.NewBaseCollection(core.CollectionNameAPIKeys)
		col.System = true

		col.Fields.Add(&core.TextField{Name: "collectionRef", System: true, Required: true})
		col.Fields.Add(&core.TextField{Name: "recordRef", System: true, Required: true})
		col.Fields.Add(&core.TextField{Name: "name", System: true, Required: true, Max: 100})
		col.Fields.Add(&core.TextField{Name: "prefix", System: true, Required: true})
		col.Fields.Add(&core.TextField{Name: "secretHash", System: true, Required: true, Hidden: true})
		col.Fields.Add(&core.JSONField{Name: "scopes", System: true, MaxSize: 64 << 10})
		col.Fields.Add(&core.DateField{Name: "expires", System: true})
		col.Fields.Add(&core.DateField{Name: "lastUsed", System: true})
		col.Fields.Add(&core.BoolField{Name: "revoked", System: true})
		col.Fields.Add(&core.AutodateField{Name: "created", System: true, OnCreate: true})

		col.AddIndex("idx_apiKeys_prefix", true, "prefix", "")
		col.AddIndex("idx_apiKeys_owner", false, "collectionRef, recordRef", "")

		return txApp.Save(col)
	}, func(txApp core.App) error {
		col, err := txApp.FindCollectionByNameOrId(core.CollectionNameAPIKeys)
		if err != nil {
			return nil
		}
		return txApp.Delete(col)
	})
}
