package migrations

import (
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #96): the OAuth 2.0 device authorization grant (RFC 8628)
// of the OpenID Connect provider.
//
//   - _oauthDeviceCodes: the hidden system collection behind a pending device
//     authorization. A row holds the SHA-256 of the device_code and of the
//     (normalised) user_code, never the codes. Rows are written with plain SQL
//     at runtime, superusers only through the records API.
//   - _oauthClients.allowDeviceFlow: the per-client switch (off by default).
//
// It does nothing when the OIDC collections do not exist.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		clients, err := txApp.FindCollectionByNameOrId(core.CollectionNameOAuthClients)
		if err != nil {
			return nil
		}

		if clients.Fields.GetByName("allowDeviceFlow") == nil {
			clients.Fields.Add(&core.BoolField{Name: "allowDeviceFlow", System: true})

			if err := txApp.Save(clients); err != nil {
				return err
			}
		}

		if _, err := txApp.FindCollectionByNameOrId(core.CollectionNameOAuthDeviceCodes); err == nil {
			return nil
		}

		col := core.NewBaseCollection(core.CollectionNameOAuthDeviceCodes)
		col.System = true
		col.Fields.Add(&core.TextField{Name: "deviceHash", System: true, Required: true, Hidden: true, Max: 100})
		col.Fields.Add(&core.TextField{Name: "userCodeHash", System: true, Required: true, Hidden: true, Max: 100})
		col.Fields.Add(&core.TextField{Name: "clientId", System: true, Max: 100})
		col.Fields.Add(&core.TextField{Name: "scopes", System: true, Max: 2000})
		col.Fields.Add(&core.TextField{Name: "status", System: true, Max: 20})
		col.Fields.Add(&core.TextField{Name: "bindingHash", System: true, Hidden: true, Max: 100})
		col.Fields.Add(&core.TextField{Name: "collectionRef", System: true, Max: 100})
		col.Fields.Add(&core.TextField{Name: "subject", System: true, Max: 100})
		col.Fields.Add(&core.TextField{Name: "sid", System: true, Max: 100})
		col.Fields.Add(&core.TextField{Name: "amr", System: true, Max: 500})
		col.Fields.Add(&core.TextField{Name: "acr", System: true, Max: 200})
		col.Fields.Add(&core.DateField{Name: "authTime", System: true})
		col.Fields.Add(&core.TextField{Name: "familyId", System: true, Max: 100})
		col.Fields.Add(&core.DateField{Name: "lastPoll", System: true})
		col.Fields.Add(&core.NumberField{Name: "pollInterval", System: true})
		col.Fields.Add(&core.DateField{Name: "expires", System: true})
		col.Fields.Add(&core.AutodateField{Name: "created", System: true, OnCreate: true})

		col.AddIndex("idx_oauthDeviceCodes_device", true, "deviceHash", "")
		col.AddIndex("idx_oauthDeviceCodes_userCode", true, "userCodeHash", "")
		col.AddIndex("idx_oauthDeviceCodes_expires", false, "expires", "")

		return txApp.Save(col)
	}, func(txApp core.App) error {
		if col, err := txApp.FindCollectionByNameOrId(core.CollectionNameOAuthDeviceCodes); err == nil {
			if err := txApp.Delete(col); err != nil {
				return err
			}
		}

		if clients, err := txApp.FindCollectionByNameOrId(core.CollectionNameOAuthClients); err == nil {
			clients.Fields.RemoveByName("allowDeviceFlow")

			return txApp.Save(clients)
		}

		return nil
	})
}
