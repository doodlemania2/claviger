package migrations

import (
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #83, 83-1): creates the hidden _recoveryCodes system
// collection (staff-vouched recovery codes, see docs/recovery.md) and
// registers it in the data map.
//
// All rules are null: the records API shows the table to superusers only.
// The rows hold the public reference and the SHA-256 hash of live codes, and
// nobody but the routes needs to read them. Staff see the open requests of
// their org through GET /api/orgs/{id}/recovery, which never returns a hash,
// a reference or a code. (The rules of a system collection cannot be edited
// afterwards, so the closed choice is also the safe one.)
//
// It does nothing when the organizations model does not exist.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		if _, err := txApp.FindCollectionByNameOrId(core.CollectionNameRecoveryCodes); err == nil {
			return nil
		}

		orgs, err := txApp.FindCollectionByNameOrId(core.CollectionNameOrgs)
		if err != nil {
			return nil
		}

		members, err := txApp.FindCollectionByNameOrId(core.CollectionNameOrgMembers)
		if err != nil {
			return nil
		}

		rel, _ := members.Fields.GetByName("user").(*core.RelationField)
		if rel == nil {
			return nil
		}

		userCollectionId := rel.CollectionId

		col := core.NewBaseCollection(core.CollectionNameRecoveryCodes)
		col.System = true
		col.Fields.Add(
			// the person being recovered; the rows go with the account
			&core.RelationField{Name: "user", System: true, Required: true, CollectionId: userCollectionId, MaxSelect: 1, CascadeDelete: true},
			&core.RelationField{Name: "org", System: true, Required: true, CollectionId: orgs.Id, MaxSelect: 1, CascadeDelete: true},
			// staff: not required, so that deleting a staff account is never blocked
			&core.RelationField{Name: "issuedBy", System: true, CollectionId: userCollectionId, MaxSelect: 1},
			&core.RelationField{Name: "confirmedBy", System: true, CollectionId: userCollectionId, MaxSelect: 1},
			&core.SelectField{Name: "channel", System: true, Required: true, MaxSelect: 1, Values: core.RecoveryChannels},
			&core.SelectField{Name: "verification", System: true, Required: true, MaxSelect: 1, Values: core.RecoveryVerifications},
			&core.TextField{Name: "note", System: true, Max: core.RecoveryNoteMax},
			// pending requests for an owner or admin have no code until a second staff member confirms
			&core.BoolField{Name: "elevated", System: true},
			&core.TextField{Name: "ref", System: true, Hidden: true, Max: 8},
			&core.TextField{Name: "codeHash", System: true, Hidden: true, Max: 64},
			&core.DateField{Name: "expires", System: true, Required: true},
			&core.DateField{Name: "usedAt", System: true},
			&core.DateField{Name: "reportedAt", System: true},
			&core.NumberField{Name: "attempts", System: true, OnlyInt: true, Min: types.Pointer(0.0)},
			&core.AutodateField{Name: "created", System: true, OnCreate: true},
			&core.AutodateField{Name: "updated", System: true, OnCreate: true, OnUpdate: true},
		)
		col.AddIndex("idx_recoveryCodes_ref", false, "ref", "")
		col.AddIndex("idx_recoveryCodes_user", false, "user, org", "")
		col.AddIndex("idx_recoveryCodes_issuedBy", false, "issuedBy, created", "")

		if err := txApp.Save(col); err != nil {
			return err
		}

		return core.RegisterDataMapEntry(txApp, core.DataMapEntry{
			Collection: core.CollectionNameRecoveryCodes, Treatment: core.DataMapTreatmentErase,
			SubjectPaths:   []string{"user"},
			PersonalFields: []string{"issuedBy", "confirmedBy", "note"},
			AgeOutBasis:    "expires",
			Note:           "Staff-vouched recovery codes (#83). Export: who vouched, how, when. Never the reference or the hash. Rows are deleted 30 days after they expire.",
		})
	}, func(txApp core.App) error {
		col, err := txApp.FindCollectionByNameOrId(core.CollectionNameRecoveryCodes)
		if err != nil {
			return nil
		}

		return txApp.Delete(col)
	})
}
