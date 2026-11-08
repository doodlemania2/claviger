package migrations

import (
	"github.com/doodlemania2/claviger/core"
)

// This system migration adds the _totps collection (issue #90): the TOTP
// (authenticator app) enrollment of auth records, one row per record.
//
// No API rules are set (superuser only) and every field is Hidden, so the
// generic records API never serializes the secret, the replay counter or the
// backup code hashes. The only readers and writers are the dedicated
// /totp endpoints, which act on the authenticated record's own row.
//
// Nothing is created for existing accounts: TOTP is opt-in, deploying this
// migration changes no one's sign-in.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		if _, err := txApp.FindCollectionByNameOrId(core.CollectionNameTOTPs); err == nil {
			return nil
		}

		col := core.NewBaseCollection(core.CollectionNameTOTPs)
		col.System = true

		col.Fields.Add(&core.TextField{
			Name:     "collectionRef",
			System:   true,
			Required: true,
			Hidden:   true,
		})
		col.Fields.Add(&core.TextField{
			Name:     "recordRef",
			System:   true,
			Required: true,
			Hidden:   true,
		})
		// base32 secret, or "enc:<base64>" when the app encryption key is configured
		col.Fields.Add(&core.TextField{
			Name:     "secret",
			System:   true,
			Required: true,
			Hidden:   true,
		})
		col.Fields.Add(&core.BoolField{
			Name:   "enabled",
			System: true,
			Hidden: true,
		})
		col.Fields.Add(&core.NumberField{
			Name:   "lastStep",
			System: true,
			Hidden: true,
		})
		// JSON array of bcrypt hashes of the unused backup codes
		col.Fields.Add(&core.JSONField{
			Name:    "backupCodes",
			System:  true,
			Hidden:  true,
			MaxSize: 1 << 16,
		})
		col.Fields.Add(&core.AutodateField{
			Name:     "created",
			System:   true,
			OnCreate: true,
		})
		col.Fields.Add(&core.AutodateField{
			Name:     "updated",
			System:   true,
			OnCreate: true,
			OnUpdate: true,
		})
		col.AddIndex("idx_totps_collectionRef_recordRef", true, "collectionRef,recordRef", "")

		return txApp.Save(col)
	}, func(txApp core.App) error {
		col, err := txApp.FindCollectionByNameOrId(core.CollectionNameTOTPs)
		if err != nil {
			return nil
		}

		return txApp.Delete(col)
	})
}
