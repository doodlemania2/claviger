package migrations

import (
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #114): the hidden system collection behind the single
// sign-on session of the OpenID Connect sign-in page. A row maps the hash of an
// opaque random cookie value to a Claviger session (sid); the cookie itself
// carries no token. Rows are written with plain SQL at runtime, superusers only
// through the records API, and the hash is hidden again by the oidc package.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		if _, err := txApp.FindCollectionByNameOrId(core.CollectionNameOAuthSso); err == nil {
			return nil
		}

		col := core.NewBaseCollection(core.CollectionNameOAuthSso)
		col.System = true
		col.Fields.Add(&core.TextField{Name: "ssoHash", System: true, Required: true, Hidden: true, Max: 100})
		col.Fields.Add(&core.TextField{Name: "collectionRef", System: true, Max: 100})
		col.Fields.Add(&core.TextField{Name: "subject", System: true, Max: 100})
		col.Fields.Add(&core.TextField{Name: "sid", System: true, Max: 100})
		col.Fields.Add(&core.TextField{Name: "amr", System: true, Max: 500})
		col.Fields.Add(&core.TextField{Name: "acr", System: true, Max: 200})
		col.Fields.Add(&core.DateField{Name: "authTime", System: true})
		col.Fields.Add(&core.DateField{Name: "expires", System: true})
		col.Fields.Add(&core.AutodateField{Name: "created", System: true, OnCreate: true})

		col.AddIndex("idx_oauthSso_hash", true, "ssoHash", "")
		col.AddIndex("idx_oauthSso_sid", false, "sid", "")
		col.AddIndex("idx_oauthSso_expires", false, "expires", "")

		return txApp.Save(col)
	}, func(txApp core.App) error {
		if col, err := txApp.FindCollectionByNameOrId(core.CollectionNameOAuthSso); err == nil {
			return txApp.Delete(col)
		}

		return nil
	})
}
