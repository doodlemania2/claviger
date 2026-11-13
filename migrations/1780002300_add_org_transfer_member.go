package migrations

import (
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #84): lets a transfer be about a household member who
// has no login (a minor). The _orgTransfers row gets a `member` relation to
// _householdMembers (the row goes with the member), and `person` is no longer
// required: a transfer has a person (a user) or a member, never both. It
// updates the data map entry to match. See docs/orgs.md, "Transfers".
//
// It does nothing when the transfers or households collections do not exist.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		col, err := txApp.FindCollectionByNameOrId(core.CollectionNameOrgTransfers)
		if err != nil {
			return nil
		}

		if col.Fields.GetByName("member") != nil {
			return nil
		}

		hm, err := txApp.FindCollectionByNameOrId(core.CollectionNameHouseholdMembers)
		if err != nil {
			return nil
		}

		if person, _ := col.Fields.GetByName("person").(*core.RelationField); person != nil {
			person.Required = false
		}

		col.Fields.Add(&core.RelationField{Name: "member", System: true, CollectionId: hm.Id, MaxSelect: 1, CascadeDelete: true})
		col.AddIndex("idx_orgTransfers_member", false, "member, fromOrg, state", "")

		if err := txApp.Save(col); err != nil {
			return err
		}

		return core.RegisterDataMapEntry(txApp, core.DataMapEntry{
			Collection: core.CollectionNameOrgTransfers, Treatment: core.DataMapTreatmentErase,
			SubjectPaths:   []string{"person"},
			PersonalFields: []string{"startedBy", "decidedBy"},
			Note:           "Consent records of a move between orgs (#84). Export: both orgs, state, who started and decided. Pending rows expire after the transfer lifetime. A transfer of a member without a login (a minor) is about the household member row and goes with it.",
		})
	}, func(txApp core.App) error {
		col, err := txApp.FindCollectionByNameOrId(core.CollectionNameOrgTransfers)
		if err != nil || col.Fields.GetByName("member") == nil {
			return nil
		}

		col.RemoveIndex("idx_orgTransfers_member")
		col.Fields.RemoveByName("member")

		return txApp.Save(col)
	})
}
