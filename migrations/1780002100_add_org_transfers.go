package migrations

import (
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #84, 84-4): creates the _orgTransfers system
// collection (member transfers between orgs, with the person's consent) and
// registers it in the data map. See docs/orgs.md, "Transfers".
//
// All rules are null: the records API shows the table to superusers only;
// every read and write goes through /api/orgs/{id}/transfers. It does nothing
// when the organizations model does not exist.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		if _, err := txApp.FindCollectionByNameOrId(core.CollectionNameOrgTransfers); err == nil {
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

		col := core.NewBaseCollection(core.CollectionNameOrgTransfers)
		col.System = true
		col.Fields.Add(
			// the rows go with the account and with either org
			&core.RelationField{Name: "person", System: true, Required: true, CollectionId: rel.CollectionId, MaxSelect: 1, CascadeDelete: true},
			&core.RelationField{Name: "fromOrg", System: true, Required: true, CollectionId: orgs.Id, MaxSelect: 1, CascadeDelete: true},
			&core.RelationField{Name: "toOrg", System: true, Required: true, CollectionId: orgs.Id, MaxSelect: 1, CascadeDelete: true},
			&core.SelectField{Name: "state", System: true, Required: true, MaxSelect: 1, Values: core.OrgTransferStates},
			&core.SelectField{Name: "sourceOutcome", System: true, Required: true, MaxSelect: 1, Values: core.OrgTransferOutcomes},
			// ids as text, not relations: a superuser may start one (and lives in
			// another collection), and deleting an account must never be blocked
			&core.TextField{Name: "startedBy", System: true, Max: 100},
			&core.DateField{Name: "expiresAt", System: true, Required: true},
			&core.DateField{Name: "decidedAt", System: true},
			&core.TextField{Name: "decidedBy", System: true, Max: 100},
			&core.AutodateField{Name: "created", System: true, OnCreate: true},
			&core.AutodateField{Name: "updated", System: true, OnCreate: true, OnUpdate: true},
		)
		col.AddIndex("idx_orgTransfers_person", false, "person, fromOrg, state", "")
		col.AddIndex("idx_orgTransfers_fromOrg", false, "fromOrg, state", "")
		col.AddIndex("idx_orgTransfers_toOrg", false, "toOrg, state", "")
		col.AddIndex("idx_orgTransfers_expiry", false, "state, expiresAt", "")

		if err := txApp.Save(col); err != nil {
			return err
		}

		return core.RegisterDataMapEntry(txApp, core.DataMapEntry{
			Collection: core.CollectionNameOrgTransfers, Treatment: core.DataMapTreatmentErase,
			SubjectPaths:   []string{"person"},
			PersonalFields: []string{"startedBy", "decidedBy"},
			Note:           "Consent records of a move between orgs (#84). Export: both orgs, state, who started and decided. Pending rows expire after the transfer lifetime.",
		})
	}, func(txApp core.App) error {
		col, err := txApp.FindCollectionByNameOrId(core.CollectionNameOrgTransfers)
		if err != nil {
			return nil
		}

		return txApp.Delete(col)
	})
}
