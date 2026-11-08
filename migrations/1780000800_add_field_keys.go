package migrations

import (
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #75): creates the hidden _fieldKeys system collection
// that holds the wrapped data keys of the encryptedText fields. All rules are
// null (superusers only), and the model-level guard in core/field_keys.go
// refuses every write through the app, so the rows are only ever created by
// the server when an encrypted field is added to a collection.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		if _, err := txApp.FindCollectionByNameOrId(core.CollectionNameFieldKeys); err == nil {
			return nil // already created
		}

		col := core.NewBaseCollection(core.CollectionNameFieldKeys)
		col.System = true

		col.Fields.Add(&core.TextField{Name: "collectionRef", System: true, Required: true})
		col.Fields.Add(&core.TextField{Name: "fieldRef", System: true, Required: true})
		// random id of the data key, part of every ciphertext of that key
		col.Fields.Add(&core.TextField{Name: "keyId", System: true, Required: true})
		// fingerprint of the key-encryption key that wrapped it
		col.Fields.Add(&core.TextField{Name: "kekId", System: true, Required: true})
		// base64 of the wrapped data key
		col.Fields.Add(&core.TextField{Name: "wrapped", System: true, Required: true, Max: 1000})
		// set when a newer key replaces this one (rotation); empty = current
		col.Fields.Add(&core.TextField{Name: "retired", System: true})
		col.Fields.Add(&core.TextField{Name: "created", System: true})

		col.AddIndex("idx_fieldKeys_keyId", true, "keyId", "")
		col.AddIndex("idx_fieldKeys_field", false, "collectionRef, fieldRef", "")

		return txApp.Save(col)
	}, func(txApp core.App) error {
		// the collection is intentionally undeletable through the app;
		// reverting means dropping the table and its definition by hand
		return nil
	})
}
