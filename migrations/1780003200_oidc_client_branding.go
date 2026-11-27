package migrations

import (
	"github.com/doodlemania2/claviger/core"
)

// Fork-local: _oauthClients.logoUri and accentColor, the calling app's look on the
// hosted sign-in pages. Both are optional and validated again when a page renders
// (https image only, #rrggbb only). It does nothing when the OIDC collections do not exist.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		clients, err := txApp.FindCollectionByNameOrId(core.CollectionNameOAuthClients)
		if err != nil {
			return nil
		}

		changed := false
		if clients.Fields.GetByName("logoUri") == nil {
			clients.Fields.Add(&core.TextField{Name: "logoUri", System: true, Max: 2000})
			changed = true
		}
		if clients.Fields.GetByName("accentColor") == nil {
			clients.Fields.Add(&core.TextField{Name: "accentColor", System: true, Max: 7, Pattern: `^#[0-9a-fA-F]{6}$`})
			changed = true
		}

		if !changed {
			return nil
		}

		return txApp.Save(clients)
	}, nil)
}
