package migrations

import (
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #79c): SCIM 2.0 provisioning.
//
// Creates the hidden _scimTokens and _scimGroups system collections and adds
// the externalId field (the identity provider's own key for a user) to
// _orgMembers, with a unique index per org. Like _invitations it does nothing
// when the organizations model does not exist, so the feature stays off.
//
// Neither collection has API rules (superusers only through the records API).
// Org admins manage tokens through /api/orgs/{id}/scim-tokens, which never
// returns tokenHash (Hidden, and its name makes the audit log redact it).
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		if _, err := txApp.FindCollectionByNameOrId(core.CollectionNameSCIMTokens); err == nil {
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

		users, err := core.OrgUserCollection(txApp)
		if err != nil {
			return nil
		}

		if members.Fields.GetByName("externalId") == nil {
			members.Fields.Add(&core.TextField{Name: "externalId", System: true, Max: 255})
			members.AddIndex("idx_orgMembers_org_externalId", true, "org, externalId", "`externalId` != ''")

			if err := txApp.Save(members); err != nil {
				return err
			}
		}

		tokens := core.NewBaseCollection(core.CollectionNameSCIMTokens)
		tokens.System = true
		tokens.Fields.Add(
			&core.RelationField{Name: "org", System: true, Required: true, CollectionId: orgs.Id, MaxSelect: 1, CascadeDelete: true},
			&core.TextField{Name: "name", System: true, Required: true, Max: 100},
			&core.TextField{Name: "prefix", System: true, Required: true},
			&core.TextField{Name: "tokenHash", System: true, Required: true, Hidden: true},
			&core.DateField{Name: "expires", System: true},
			&core.DateField{Name: "lastUsed", System: true},
			&core.BoolField{Name: "revoked", System: true},
			// not required and not cascading: the token outlives its creator
			&core.RelationField{Name: "createdBy", System: true, CollectionId: users.Id, MaxSelect: 1},
			&core.AutodateField{Name: "created", System: true, OnCreate: true},
		)
		tokens.AddIndex("idx_scimTokens_prefix", true, "prefix", "")
		tokens.AddIndex("idx_scimTokens_org", false, "org", "")

		if err := txApp.Save(tokens); err != nil {
			return err
		}

		groups := core.NewBaseCollection(core.CollectionNameSCIMGroups)
		groups.System = true
		groups.Fields.Add(
			&core.RelationField{Name: "org", System: true, Required: true, CollectionId: orgs.Id, MaxSelect: 1, CascadeDelete: true},
			&core.TextField{Name: "displayName", System: true, Required: true, Max: 64},
			&core.TextField{Name: "externalId", System: true, Max: 255},
			&core.RelationField{Name: "members", System: true, CollectionId: users.Id, MaxSelect: 100000},
			&core.AutodateField{Name: "created", System: true, OnCreate: true},
			&core.AutodateField{Name: "updated", System: true, OnCreate: true, OnUpdate: true},
		)
		groups.AddIndex("idx_scimGroups_org_displayName", true, "org, displayName", "")
		groups.AddIndex("idx_scimGroups_org_externalId", true, "org, externalId", "`externalId` != ''")

		return txApp.Save(groups)
	}, func(txApp core.App) error {
		for _, name := range []string{core.CollectionNameSCIMGroups, core.CollectionNameSCIMTokens} {
			if col, err := txApp.FindCollectionByNameOrId(name); err == nil {
				if err := txApp.Delete(col); err != nil {
					return err
				}
			}
		}

		if members, err := txApp.FindCollectionByNameOrId(core.CollectionNameOrgMembers); err == nil {
			if members.Fields.GetByName("externalId") != nil {
				members.RemoveIndex("idx_orgMembers_org_externalId")
				members.Fields.RemoveByName("externalId")

				return txApp.Save(members)
			}
		}

		return nil
	})
}
