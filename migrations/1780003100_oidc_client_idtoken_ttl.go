package migrations

import (
	"github.com/doodlemania2/claviger/core"
)

// Fork-local: _oauthClients.idTokenSeconds, the per-client ID token lifetime.
// Empty or 0 keeps the default of one hour. Some relying parties send the ID
// token as their API bearer token and want it short; the bounds are 60 seconds
// to one hour. It does nothing when the OIDC collections do not exist.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		clients, err := txApp.FindCollectionByNameOrId(core.CollectionNameOAuthClients)
		if err != nil {
			return nil
		}

		if clients.Fields.GetByName("idTokenSeconds") != nil {
			return nil
		}

		min, max := 60.0, 3600.0
		clients.Fields.Add(&core.NumberField{Name: "idTokenSeconds", System: true, Min: &min, Max: &max, OnlyInt: true})

		return txApp.Save(clients)
	}, nil)
}
