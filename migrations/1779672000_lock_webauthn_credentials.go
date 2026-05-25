package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"
)

// This system migration hardens the _webauthnCredentials collection:
//
//   - audit H5/L3: marks Hidden=true on every authenticator-derived field so a
//     compromised owner-rule on the collection cannot be used to enumerate
//     authenticator-specific data (credentialId, aaguid, transports, flags,
//     attestationType) or to lift the sign count of a victim's authenticator.
//   - signCount gets a minimum value of 0; a negative sign count is never a
//     legitimate state and would otherwise pass the existing NumberField
//     validator that allows any int by default.
//
// publicKey was already Hidden=true in the original collection migration and
// is therefore not touched here.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		col, err := txApp.FindCollectionByNameOrId(core.CollectionNameWebAuthnCredentials)
		if err != nil {
			return nil
		}

		hide := func(name string) {
			f := col.Fields.GetByName(name)
			if f == nil {
				return
			}
			switch v := f.(type) {
			case *core.TextField:
				v.Hidden = true
			case *core.JSONField:
				v.Hidden = true
			case *core.NumberField:
				v.Hidden = true
			}
		}

		hide("credentialId")
		hide("aaguid")
		hide("flags")
		hide("signCount")
		hide("transport")
		hide("attestationType")

		if scField, ok := col.Fields.GetByName("signCount").(*core.NumberField); ok && scField != nil {
			scField.Min = types.Pointer(float64(0))
		}

		return txApp.Save(col)
	}, func(txApp core.App) error {
		col, err := txApp.FindCollectionByNameOrId(core.CollectionNameWebAuthnCredentials)
		if err != nil {
			return nil
		}

		unhide := func(name string) {
			f := col.Fields.GetByName(name)
			if f == nil {
				return
			}
			switch v := f.(type) {
			case *core.TextField:
				v.Hidden = false
			case *core.JSONField:
				v.Hidden = false
			case *core.NumberField:
				v.Hidden = false
				v.Min = nil
			}
		}

		unhide("credentialId")
		unhide("aaguid")
		unhide("flags")
		unhide("signCount")
		unhide("transport")
		unhide("attestationType")

		return txApp.Save(col)
	})
}
