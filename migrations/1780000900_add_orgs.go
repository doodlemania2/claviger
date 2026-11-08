package migrations

import (
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #79a): creates the hidden system collections of the
// organizations model (_orgs, _orgMembers, _orgRoles) and the optional `org`
// relation of OIDC clients (org-bound clients).
//
// The list and view rules are fixed here, because the rules of a system
// collection cannot be edited afterwards: members see their own organization,
// its members and its tags, and nothing else. Create, update and delete rules
// stay null (superusers only); real changes go through /api/orgs, which
// enforces the last-owner and role-escalation guards.
//
// _orgMembers.user points at one auth collection: CLAVIGER_ORGS_USER_COLLECTION
// (default "users", falling back to the default users collection id). If no
// such collection exists yet the migration does nothing and the feature stays
// off; with no organizations created nothing changes in behavior.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		if _, err := txApp.FindCollectionByNameOrId(core.CollectionNameOrgs); err == nil {
			return nil
		}

		users, err := txApp.FindCollectionByNameOrId(core.OrgUserCollectionName())
		if err != nil {
			users, err = txApp.FindCollectionByNameOrId("_pb_users_auth_")
		}
		if err != nil || !users.IsAuth() || users.Name == core.CollectionNameSuperusers {
			return nil
		}

		const live = `@request.auth.id != "" && org.archived = false && ` +
			`org._orgMembers_via_org.user ?= @request.auth.id && org._orgMembers_via_org.status ?= "active"`

		orgs := core.NewBaseCollection(core.CollectionNameOrgs)
				orgs.Fields.Add(
			&core.TextField{Name: "name", System: true, Required: true, Max: 200},
			&core.TextField{Name: "slug", System: true, Required: true, Pattern: `^[a-z0-9-]{2,64}$`, Max: 64},
			&core.BoolField{Name: "archived", System: true},
			&core.JSONField{Name: "settings", System: true, MaxSize: 20000},
			&core.AutodateField{Name: "created", System: true, OnCreate: true},
			&core.AutodateField{Name: "updated", System: true, OnCreate: true, OnUpdate: true},
		)
		orgs.AddIndex("idx_orgs_slug", true, "slug", "")
		// the parent relation points at the collection itself, so it needs the id
		if err := txApp.Save(orgs); err != nil {
			return err
		}
		orgs.Fields.Add(&core.RelationField{Name: "parent", System: true, CollectionId: orgs.Id, MaxSelect: 1})
		orgs.AddIndex("idx_orgs_parent", false, "parent", "")
		if err := txApp.Save(orgs); err != nil {
			return err
		}

		roles := core.NewBaseCollection(core.CollectionNameOrgRoles)
				roles.Fields.Add(
			&core.RelationField{Name: "org", System: true, Required: true, CollectionId: orgs.Id, MaxSelect: 1, CascadeDelete: true},
			&core.TextField{Name: "name", System: true, Required: true, Max: 64},
			&core.TextField{Name: "description", System: true, Max: 500},
			&core.AutodateField{Name: "created", System: true, OnCreate: true},
			&core.AutodateField{Name: "updated", System: true, OnCreate: true, OnUpdate: true},
		)
		roles.AddIndex("idx_orgRoles_org_name", true, "org, name", "")
		if err := txApp.Save(roles); err != nil {
			return err
		}

		members := core.NewBaseCollection(core.CollectionNameOrgMembers)
				members.Fields.Add(
			&core.RelationField{Name: "org", System: true, Required: true, CollectionId: orgs.Id, MaxSelect: 1, CascadeDelete: true},
			&core.RelationField{Name: "user", System: true, Required: true, CollectionId: users.Id, MaxSelect: 1, CascadeDelete: true},
			&core.SelectField{Name: "role", System: true, Required: true, MaxSelect: 1, Values: core.OrgRoles},
			&core.SelectField{Name: "status", System: true, Required: true, MaxSelect: 1,
				Values: []string{core.OrgStatusActive, core.OrgStatusSuspended}},
			&core.RelationField{Name: "tags", System: true, CollectionId: roles.Id, MaxSelect: core.OrgMaxTags},
			&core.SelectField{Name: "source", System: true, MaxSelect: 1,
				Values: []string{core.OrgSourceManual, core.OrgSourceInvitation, core.OrgSourceSCIM, core.OrgSourceSSO}},
			&core.AutodateField{Name: "created", System: true, OnCreate: true},
			&core.AutodateField{Name: "updated", System: true, OnCreate: true, OnUpdate: true},
		)
		members.AddIndex("idx_orgMembers_org_user", true, "org, user", "")
		members.AddIndex("idx_orgMembers_user", false, "user", "")
		members.AddIndex("idx_orgMembers_org_role", false, "org, role", "")
		if err := txApp.Save(members); err != nil {
			return err
		}

		// the rules reference the back-relation of _orgMembers, so they can
		// only be set once it exists
		orgs.ListRule = types.Pointer(`@request.auth.id != "" && archived = false && ` +
			`_orgMembers_via_org.user ?= @request.auth.id && _orgMembers_via_org.status ?= "active"`)
		orgs.ViewRule = orgs.ListRule
		roles.ListRule = types.Pointer(live)
		roles.ViewRule = roles.ListRule
		members.ListRule = types.Pointer(live)
		members.ViewRule = members.ListRule
		// The system flag and the rules cannot be changed once a collection is
		// a system collection, and the rules need all three collections to
		// exist, so they are finalized last without validation (the rules are
		// covered by the tests).
		for _, col := range []*core.Collection{orgs, roles, members} {
			col.System = true
			if err := txApp.SaveNoValidate(col); err != nil {
				return err
			}
		}

		// org-bound OIDC clients
		if clients, err := txApp.FindCollectionByNameOrId(core.CollectionNameOAuthClients); err == nil {
			if clients.Fields.GetByName("org") == nil {
				clients.Fields.Add(&core.RelationField{Name: "org", System: true, CollectionId: orgs.Id, MaxSelect: 1})

				return txApp.Save(clients)
			}
		}

		return nil
	}, func(txApp core.App) error {
		if clients, err := txApp.FindCollectionByNameOrId(core.CollectionNameOAuthClients); err == nil {
			if clients.Fields.GetByName("org") != nil {
				clients.Fields.RemoveByName("org")
				if err := txApp.Save(clients); err != nil {
					return err
				}
			}
		}

		for _, name := range []string{core.CollectionNameOrgMembers, core.CollectionNameOrgRoles, core.CollectionNameOrgs} {
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
