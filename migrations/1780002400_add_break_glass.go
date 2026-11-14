package migrations

import (
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #86, 86-2): creates the _breakGlass system collection
// (time-boxed, two-person access to a pastoral note) and the _pastoralHolds
// collection (notes about a person held back from their data export until an
// org owner decides), and registers both in the data map. See
// docs/pastoral-notes.md.
//
// All rules are null: the records API shows both tables to superusers only;
// every read and write goes through /api/breakglass and
// /api/gdpr/pastoral-holds. The read rules of a pastoral notes collection join
// _breakGlass through @collection, which does not need a rule on it. It does
// nothing when the organizations model does not exist.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		orgs, err := txApp.FindCollectionByNameOrId(core.CollectionNameOrgs)
		if err != nil {
			return nil
		}

		if _, err := txApp.FindCollectionByNameOrId(core.CollectionNameBreakGlass); err != nil {
			col := core.NewBaseCollection(core.CollectionNameBreakGlass)
			col.System = true
			col.Fields.Add(
				// what is opened: the collection name and the id of the note
				&core.TextField{Name: "collection", System: true, Required: true, Max: 100},
				&core.TextField{Name: "recordId", System: true, Required: true, Max: 100},
				&core.RelationField{Name: "org", System: true, Required: true, CollectionId: orgs.Id, MaxSelect: 1, CascadeDelete: true},
				// ids as text, not relations: deleting an account must never be blocked
				&core.TextField{Name: "grantee", System: true, Required: true, Max: 100},
				&core.TextField{Name: "reason", System: true, Required: true, Max: 2000},
				&core.SelectField{Name: "basis", System: true, Required: true, MaxSelect: 1, Values: core.BreakGlassBases},
				&core.NumberField{Name: "hours", System: true, OnlyInt: true, Min: types.Pointer(1.0), Max: types.Pointer(24.0)},
				&core.DateField{Name: "requestedAt", System: true, Required: true},
				&core.DateField{Name: "activeFrom", System: true},
				&core.DateField{Name: "expires", System: true},
				&core.TextField{Name: "approvedBy", System: true, Max: 100},
				&core.DateField{Name: "ownerNotifiedAt", System: true},
				&core.BoolField{Name: "revoked", System: true},
				&core.TextField{Name: "revokedBy", System: true, Max: 100},
				&core.DateField{Name: "revokedAt", System: true},
				&core.NumberField{Name: "readCount", System: true, OnlyInt: true, Min: types.Pointer(0.0)},
				&core.AutodateField{Name: "created", System: true, OnCreate: true},
				&core.AutodateField{Name: "updated", System: true, OnCreate: true, OnUpdate: true},
			)
			col.AddIndex("idx_breakGlass_grantee", false, "grantee, collection, recordId", "")
			col.AddIndex("idx_breakGlass_org", false, "org, revoked, expires", "")

			if err := txApp.Save(col); err != nil {
				return err
			}
		}

		if _, err := txApp.FindCollectionByNameOrId(core.CollectionNamePastoralHolds); err != nil {
			col := core.NewBaseCollection(core.CollectionNamePastoralHolds)
			col.System = true
			col.Fields.Add(
				&core.TextField{Name: "collection", System: true, Required: true, Max: 100},
				&core.TextField{Name: "recordId", System: true, Required: true, Max: 100},
				&core.RelationField{Name: "org", System: true, Required: true, CollectionId: orgs.Id, MaxSelect: 1, CascadeDelete: true},
				// the person the note is about, as an id (the account may go away)
				&core.TextField{Name: "subject", System: true, Required: true, Max: 100},
				&core.SelectField{Name: "state", System: true, Required: true, MaxSelect: 1, Values: core.PastoralHoldStates},
				&core.DateField{Name: "heldAt", System: true},
				&core.TextField{Name: "decidedBy", System: true, Max: 100},
				&core.DateField{Name: "decidedAt", System: true},
				&core.TextField{Name: "note", System: true, Max: 1000},
				&core.AutodateField{Name: "created", System: true, OnCreate: true},
				&core.AutodateField{Name: "updated", System: true, OnCreate: true, OnUpdate: true},
			)
			col.AddIndex("idx_pastoralHolds_note", true, "collection, recordId", "")
			col.AddIndex("idx_pastoralHolds_org", false, "org, state", "")

			if err := txApp.Save(col); err != nil {
				return err
			}
		}

		for _, entry := range []core.DataMapEntry{
			{
				Collection: core.CollectionNameBreakGlass, Treatment: core.DataMapTreatmentRetain,
				SubjectPaths:   []string{"grantee", "approvedBy"},
				PersonalFields: []string{"grantee", "approvedBy", "revokedBy", "reason"}, AgeOutBasis: "requestedAt",
				RetainReason: "Evidence of who opened a pastoral note, why and for how long. Kept so that access to the most sensitive data stays accountable.",
				Note:         "Break-glass grants (#86). Export: the person's own grants (basis, times, who approved) without the reason, which only org owners read.",
			},
			{
				Collection: core.CollectionNamePastoralHolds, Treatment: core.DataMapTreatmentErase,
				SubjectPaths:   []string{"subject"},
				PersonalFields: []string{"subject", "decidedBy", "note"},
				Note:           "Held pastoral notes (#86). Export: that a note is held or released, never what it says. Deleted when the person is erased (UnlinkPastoralSubject).",
			},
		} {
			if err := core.RegisterDataMapEntry(txApp, entry); err != nil {
				return err
			}
		}

		return nil
	}, func(txApp core.App) error {
		for _, name := range []string{core.CollectionNamePastoralHolds, core.CollectionNameBreakGlass} {
			if col, err := txApp.FindCollectionByNameOrId(name); err == nil {
				if err := txApp.Delete(col); err != nil {
					return err
				}
			}
		}

		return nil
	})
}
