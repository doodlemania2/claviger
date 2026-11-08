package migrations

import (
	"github.com/doodlemania2/claviger/core"
)

// This system migration adds a "lastUsed" datetime to _webauthnCredentials so
// users can tell a stale passkey from one in daily use (device management,
// issue #73).
//
// The field is Hidden=true like the other authenticator-derived fields (see
// 1779672000_lock_webauthn_credentials.go): the generic records API never
// serializes it. The owner-facing list endpoint
// (GET .../auth-with-webauthn/credentials) reads it explicitly.
//
// Existing credentials get no backfill: an empty lastUsed means "no use
// recorded yet" (the credential predates the field or was never used since).
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		col, err := txApp.FindCollectionByNameOrId(core.CollectionNameWebAuthnCredentials)
		if err != nil {
			return nil
		}

		if col.Fields.GetByName("lastUsed") != nil {
			return nil
		}

		col.Fields.Add(&core.DateField{
			Name:   "lastUsed",
			System: true,
			Hidden: true,
		})

		return txApp.Save(col)
	}, func(txApp core.App) error {
		col, err := txApp.FindCollectionByNameOrId(core.CollectionNameWebAuthnCredentials)
		if err != nil {
			return nil
		}

		if col.Fields.GetByName("lastUsed") == nil {
			return nil
		}

		col.Fields.RemoveByName("lastUsed")

		return txApp.Save(col)
	})
}
