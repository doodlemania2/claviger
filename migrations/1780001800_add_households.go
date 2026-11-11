package migrations

import (
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #80, 80-1): creates the hidden _households and
// _householdMembers system collections (households and guardian-managed
// minors, see docs/households.md) and registers them in the data map.
//
// It does nothing when the organizations model does not exist. All write
// rules are null: rows are written by the routes under /api/households and by
// the daily graduation job, which enforce who may change what (the rules of a
// system collection cannot be edited afterwards). The list and view rules are
// in core.HouseholdRules.
//
// The address, phone, birth date, allergies and medical notes are created as
// plain fields. The server turns them into encrypted fields at the first start
// with a key-encryption key (core.UpgradeHouseholdEncryption), and the routes
// refuse to store sensitive values in them before that. The migration does not
// depend on a key, so an installation without one can upgrade, and a database
// migrated under one key can be opened with another.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		if _, err := txApp.FindCollectionByNameOrId(core.CollectionNameHouseholdMembers); err == nil {
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

		// plain fields first: the migration must not depend on a key (a
		// database is migrated once and may be opened with another key later).
		// core.UpgradeHouseholdEncryption turns them into encrypted fields at
		// the first start with a key, and the routes refuse sensitive values
		// until then.
		// not system fields: the upgrade replaces them with encrypted fields
		// (the type of a field cannot change under the same id)
		text := func(name string, max int) core.Field {
			return &core.TextField{Name: name, Max: max}
		}

		// text, not json, so that the column is TEXT on every database and the
		// values can be encrypted in place later
		jsonField := func(name string, max int64) core.Field {
			return &core.TextField{Name: name, Max: int(max)}
		}

		households := core.NewBaseCollection(core.CollectionNameHouseholds)
		households.System = true
		households.Fields.Add(
			// no cascade: an org that still has households cannot be deleted by accident
			&core.RelationField{Name: "org", System: true, Required: true, CollectionId: orgs.Id, MaxSelect: 1},
			&core.TextField{Name: "name", System: true, Required: true, Max: 200},
			jsonField("address", 64<<10),
			text("homePhone", 100),
			&core.SelectField{Name: "status", System: true, Required: true, MaxSelect: 1, Values: core.HouseholdStatuses},
			&core.AutodateField{Name: "created", System: true, OnCreate: true},
			&core.AutodateField{Name: "updated", System: true, OnCreate: true, OnUpdate: true},
		)
		households.AddIndex("idx_households_org", false, "org", "")

		if err := txApp.Save(households); err != nil {
			return err
		}

		people := core.NewBaseCollection(core.CollectionNameHouseholdMembers)
		people.System = true
		people.Fields.Add(
			&core.RelationField{Name: "household", System: true, Required: true, CollectionId: households.Id, MaxSelect: 1, CascadeDelete: true},
			&core.RelationField{Name: "user", System: true, CollectionId: userCollectionId, MaxSelect: 1},
			&core.TextField{Name: "name", System: true, Required: true, Max: 200},
			&core.SelectField{Name: "role", System: true, MaxSelect: 1, Values: core.HouseholdRoles},
			&core.BoolField{Name: "guardian", System: true},
			&core.BoolField{Name: "isMinor", System: true},
			text("birthDate", 32),
			&core.FileField{Name: "photo", System: true, Protected: true, MaxSelect: 1, MaxSize: 5 << 20,
				MimeTypes: []string{"image/png", "image/jpeg", "image/webp"}},
			jsonField("allergies", 64<<10),
			jsonField("medical", 64<<10),
			&core.SelectField{Name: "status", System: true, MaxSelect: 1, Values: core.HouseholdMemberStatuses},
			&core.SelectField{Name: "source", System: true, MaxSelect: 1, Values: core.HouseholdSources},
			&core.AutodateField{Name: "created", System: true, OnCreate: true},
			&core.AutodateField{Name: "updated", System: true, OnCreate: true, OnUpdate: true},
		)
		people.AddIndex("idx_householdMembers_household", false, "household", "")
		people.AddIndex("idx_householdMembers_household_user", true, "household, user", "`user` != ''")
		people.AddIndex("idx_householdMembers_user", false, "user", "")

		if err := txApp.Save(people); err != nil {
			return err
		}

		// the rules refer to _orgMembers through the household's org, so they
		// are finalized after both collections exist
		hRule, mRule := core.HouseholdRules()
		households.ListRule = types.Pointer(hRule)
		households.ViewRule = households.ListRule
		people.ListRule = types.Pointer(mRule)
		people.ViewRule = people.ListRule

		for _, col := range []*core.Collection{households, people} {
			if err := txApp.SaveNoValidate(col); err != nil {
				return err
			}
		}

		if err := core.RegisterDataMapEntry(txApp, core.DataMapEntry{
			Collection: core.CollectionNameHouseholdMembers, Treatment: core.DataMapTreatmentErase,
			SubjectPaths:   []string{"user"},
			PersonalFields: []string{"name", "birthDate", "photo", "allergies", "medical"},
			Note:           "A person of a household (#80). Export: the person's own row only, never another member's details. A minor has no login, so a minor's row is not reached through a user.",
		}); err != nil {
			return err
		}

		return core.RegisterDataMapEntry(txApp, core.DataMapEntry{
			Collection: core.CollectionNameHouseholds, Treatment: core.DataMapTreatmentExport,
			SubjectPaths:   []string{core.CollectionNameHouseholdMembers + "_via_household.user"},
			PersonalFields: []string{"address", "homePhone"},
			Note:           "The shared contact data of the household (#80), exported to each member with a login. Kept on erasure of one member: it belongs to the others too.",
		})
	}, func(txApp core.App) error {
		for _, name := range []string{core.CollectionNameHouseholdMembers, core.CollectionNameHouseholds} {
			col, err := txApp.FindCollectionByNameOrId(name)
			if err != nil {
				continue
			}

			if err := txApp.Delete(col); err != nil {
				return err
			}
		}

		return nil
	})
}
