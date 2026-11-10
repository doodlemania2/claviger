package migrations

import (
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #81): creates the hidden _credentialTypes and
// _credentials system collections (safeguarding credentials, see
// docs/credentials.md) and registers them in the data map.
//
// It does nothing when the organizations model or the closure table does not
// exist. All write rules are null: credentials are written by the routes under
// /api/orgs/{id}, which enforce who may record and verify (the rules of a
// system collection cannot be edited afterwards).
//
// Read rules:
//   - _credentials: the holder reads their own; coordinators (owner or admin,
//     or a member tagged "safeguarding") of the issuing org or of any ancestor.
//   - _credentialTypes: members of the defining org or of any org below it.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		if _, err := txApp.FindCollectionByNameOrId(core.CollectionNameCredentials); err == nil {
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

		if _, err := txApp.FindCollectionByNameOrId(core.CollectionNameOrgClosure); err != nil {
			return nil
		}

		rel, _ := members.Fields.GetByName("user").(*core.RelationField)
		if rel == nil {
			return nil
		}

		userCollectionId := rel.CollectionId

		// credential types
		typesCol := core.NewBaseCollection(core.CollectionNameCredentialTypes)
		typesCol.System = true
		typesCol.Fields.Add(
			&core.RelationField{Name: "org", System: true, Required: true, CollectionId: orgs.Id, MaxSelect: 1, CascadeDelete: true},
			&core.TextField{Name: "key", System: true, Required: true, Pattern: `^[a-z0-9_]{2,40}$`, Max: 40},
			&core.TextField{Name: "name", System: true, Required: true, Max: 200},
			&core.NumberField{Name: "validMonths", System: true, OnlyInt: true, Min: types.Pointer(0.0), Max: types.Pointer(120.0)},
			&core.BoolField{Name: "requiresDocument", System: true},
			&core.BoolField{Name: "requiresVerifier", System: true},
			&core.SelectField{Name: "status", System: true, Required: true, MaxSelect: 1,
				Values: []string{core.CredentialTypeActive, core.CredentialTypeRetired}},
			&core.AutodateField{Name: "created", System: true, OnCreate: true},
			&core.AutodateField{Name: "updated", System: true, OnCreate: true, OnUpdate: true},
		)
		typesCol.AddIndex("idx_credentialTypes_org_key", true, "org, key", "")

		// credentials
		creds := core.NewBaseCollection(core.CollectionNameCredentials)
		creds.System = true
		creds.Fields.Add(
			&core.RelationField{Name: "user", System: true, Required: true, CollectionId: userCollectionId, MaxSelect: 1, CascadeDelete: true},
			// no cascade: an org that still has credentials cannot be deleted by accident
			&core.RelationField{Name: "org", System: true, Required: true, CollectionId: orgs.Id, MaxSelect: 1},
			&core.TextField{Name: "type", System: true, Required: true, Pattern: `^[a-z0-9_]{2,40}$`, Max: 40},
			&core.DateField{Name: "issuedAt", System: true},
			&core.DateField{Name: "expires", System: true, Required: true},
			&core.TextField{Name: "issuer", System: true, Max: 200},
			&core.RelationField{Name: "recordedBy", System: true, CollectionId: userCollectionId, MaxSelect: 1},
			&core.RelationField{Name: "verifiedBy", System: true, CollectionId: userCollectionId, MaxSelect: 1},
			&core.BoolField{Name: "verified", System: true},
			&core.FileField{Name: "document", System: true, Protected: true, MaxSelect: 1, MaxSize: 10 << 20,
				MimeTypes: []string{"application/pdf", "image/png", "image/jpeg"}},
			&core.BoolField{Name: "revoked", System: true},
			&core.TextField{Name: "revokedReason", System: true, Hidden: true, Max: 500},
			&core.DateField{Name: "revokedAt", System: true},
			&core.SelectField{Name: "status", System: true, MaxSelect: 1,
				Values: []string{core.CredentialStatusPending, core.CredentialStatusValid, core.CredentialStatusLapsed, core.CredentialStatusRevoked}},
			&core.NumberField{Name: "reminderStage", System: true, OnlyInt: true, Min: types.Pointer(0.0)},
			&core.DateField{Name: "notifiedAt", System: true},
			&core.DateField{Name: "lapsedAt", System: true},
			&core.AutodateField{Name: "created", System: true, OnCreate: true},
			&core.AutodateField{Name: "updated", System: true, OnCreate: true, OnUpdate: true},
		)
		creds.AddIndex("idx_credentials_user_type", false, "user, type", "")
		creds.AddIndex("idx_credentials_expires", false, "expires", "")
		creds.AddIndex("idx_credentials_org", false, "org", "")

		// Saved without rules first so the rules can be finalized (they refer
		// to other system collections) the way the org migration does.
		if err := txApp.Save(typesCol); err != nil {
			return err
		}

		if err := txApp.Save(creds); err != nil {
			return err
		}

		admins, err := core.OrgAncestorRule("org", core.OrgAncestorOpts{Roles: []string{core.OrgRoleOwner, core.OrgRoleAdmin}})
		if err != nil {
			return err
		}

		tagged, err := core.OrgAncestorRule("org", core.OrgAncestorOpts{
			Roles: []string{core.OrgRoleOwner, core.OrgRoleAdmin, core.OrgRoleMember}, Tag: core.CredentialCoordinatorTag,
		})
		if err != nil {
			return err
		}

		creds.ListRule = types.Pointer(`@request.auth.id != "" && (user = @request.auth.id || (` + admins + `) || (` + tagged + `))`)
		creds.ViewRule = creds.ListRule

		typesCol.ListRule = types.Pointer(`@request.auth.id != "" && org.archived = false && ` +
			`@collection._orgClosure:q.ancestor ?= org && @collection._orgMembers:m.org ?= @collection._orgClosure:q.descendant && ` +
			`@collection._orgMembers:m.user ?= @request.auth.id && @collection._orgMembers:m.status ?= "active"`)
		typesCol.ViewRule = typesCol.ListRule

		for _, col := range []*core.Collection{typesCol, creds} {
			if err := txApp.SaveNoValidate(col); err != nil {
				return err
			}
		}

		if err := core.RegisterDataMapEntry(txApp, core.DataMapEntry{
			Collection: core.CollectionNameCredentials, Treatment: core.DataMapTreatmentErase,
			SubjectPaths:   []string{"user"},
			PersonalFields: []string{"issuer", "document", "revokedReason", "recordedBy", "verifiedBy"},
			AgeOutBasis:    "expires",
			Note:           "Safeguarding credential attestations (#81). Export: all fields, including the revoke reason. Erased after the credential retention period set by the congregation.",
		}); err != nil {
			return err
		}

		return core.RegisterDataMapEntry(txApp, core.DataMapEntry{
			Collection: core.CollectionNameCredentialTypes, Treatment: core.DataMapTreatmentNone,
			Note: "Credential type definitions (#81). Not about a person.",
		})
	}, func(txApp core.App) error {
		for _, name := range []string{core.CollectionNameCredentials, core.CollectionNameCredentialTypes} {
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
