package migrations

import (
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #79b): creates the hidden _invitations system
// collection (email invitations to organizations).
//
// It does nothing when the organizations model does not exist (#79a's
// migration found no suitable user collection), so the feature stays off.
// The collection has no API rules at all: list, view, create, update and
// delete are superuser only. Org admins use /api/orgs/{id}/invitations, which
// enforces the role guards, the rate limits and the token handling.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		if _, err := txApp.FindCollectionByNameOrId(core.CollectionNameInvitations); err == nil {
			return nil
		}

		orgs, err := txApp.FindCollectionByNameOrId(core.CollectionNameOrgs)
		if err != nil {
			return nil
		}

		roles, err := txApp.FindCollectionByNameOrId(core.CollectionNameOrgRoles)
		if err != nil {
			return nil
		}

		users, err := core.OrgUserCollection(txApp)
		if err != nil {
			return nil
		}

		inv := core.NewBaseCollection(core.CollectionNameInvitations)
		inv.System = true
		inv.Fields.Add(
			&core.RelationField{Name: "org", System: true, Required: true, CollectionId: orgs.Id, MaxSelect: 1, CascadeDelete: true},
			&core.TextField{Name: "email", System: true, Required: true, Max: 254},
			&core.SelectField{Name: "role", System: true, Required: true, MaxSelect: 1, Values: core.OrgRoles},
			&core.RelationField{Name: "tags", System: true, CollectionId: roles.Id, MaxSelect: core.OrgMaxTags},
			// not required and not cascading: the invitation outlives its inviter
			&core.RelationField{Name: "invitedBy", System: true, CollectionId: users.Id, MaxSelect: 1},
			&core.TextField{Name: "tokenHash", System: true, Required: true, Hidden: true, Max: 128},
			&core.DateField{Name: "expires", System: true, Required: true},
			&core.SelectField{Name: "status", System: true, Required: true, MaxSelect: 1,
				Values: []string{core.InvitationStatusPending, core.InvitationStatusAccepted, core.InvitationStatusRevoked, core.InvitationStatusExpired}},
			&core.RelationField{Name: "acceptedBy", System: true, CollectionId: users.Id, MaxSelect: 1},
			&core.DateField{Name: "acceptedAt", System: true},
			&core.DateField{Name: "lastSentAt", System: true},
			&core.NumberField{Name: "sendCount", System: true, OnlyInt: true},
			&core.AutodateField{Name: "created", System: true, OnCreate: true},
			&core.AutodateField{Name: "updated", System: true, OnCreate: true, OnUpdate: true},
		)
		inv.AddIndex("idx_invitations_tokenHash", true, "tokenHash", "")
		// one live invitation per email and org
		inv.AddIndex("idx_invitations_org_email_pending", true, "org, email", "status = 'pending'")
		inv.AddIndex("idx_invitations_expires", false, "expires", "")

		return txApp.Save(inv)
	}, func(txApp core.App) error {
		col, err := txApp.FindCollectionByNameOrId(core.CollectionNameInvitations)
		if err != nil {
			return nil
		}

		return txApp.Delete(col)
	})
}
