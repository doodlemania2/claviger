package migrations

import (
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #92): creates _ssoConnections, one upstream identity
// provider of an organization (docs/inbound-sso.md). All rules are null: the
// records API shows the table to superusers only; org owners and admins
// manage it through /api/orgs/{id}/sso/connections. It does nothing when the
// organizations model does not exist.
//
// "type" picks the protocol implementation (package sso); the type-specific
// settings that have no column of their own live in "config", so adding SAML
// needs no migration.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		if _, err := txApp.FindCollectionByNameOrId(core.CollectionNameSSOConnections); err == nil {
			return nil
		}

		orgs, err := txApp.FindCollectionByNameOrId(core.CollectionNameOrgs)
		if err != nil {
			return nil
		}

		col := core.NewBaseCollection(core.CollectionNameSSOConnections)
		col.System = true
		col.Fields.Add(
			&core.RelationField{Name: "org", System: true, Required: true, CollectionId: orgs.Id, MaxSelect: 1, CascadeDelete: true},
			&core.TextField{Name: "name", System: true, Required: true, Max: 100},
			&core.SelectField{Name: "type", System: true, Required: true, MaxSelect: 1, Values: core.SSOTypes},
			&core.BoolField{Name: "enabled", System: true},
			// refuse password and OTP sign-in for the verified domains of this connection
			&core.BoolField{Name: "ssoRequired", System: true},
			// create accounts on first sign-in
			&core.BoolField{Name: "jit", System: true},
			&core.TextField{Name: "issuer", System: true, Max: 500},
			&core.TextField{Name: "clientId", System: true, Max: 500},
			// encrypted with the app encryption key when it is set (core.SetSSOSecret)
			&core.TextField{Name: "clientSecret", System: true, Hidden: true, Max: 4000},
			&core.TextField{Name: "scopes", System: true, Max: 500},
			&core.TextField{Name: "groupsClaim", System: true, Max: 100},
			// {"<group or claim value>": "<org role>"}
			&core.JSONField{Name: "roleMapping", System: true, MaxSize: 1 << 16},
			&core.TextField{Name: "defaultRole", System: true, Max: 20},
			// settings of one connection type without a column of their own (SAML metadata, ...)
			&core.JSONField{Name: "config", System: true, MaxSize: 1 << 20},
			&core.AutodateField{Name: "created", System: true, OnCreate: true},
			&core.AutodateField{Name: "updated", System: true, OnCreate: true, OnUpdate: true},
		)
		col.AddIndex("idx_ssoConnections_org", false, "org", "")

		if err := txApp.Save(col); err != nil {
			return err
		}

		return core.RegisterDataMapEntry(txApp, core.DataMapEntry{
			Collection: core.CollectionNameSSOConnections, Treatment: core.DataMapTreatmentNone,
			Note: "Identity provider settings of an organization (#92). Not about a person; the client secret is never exported.",
		})
	}, func(txApp core.App) error {
		if col, err := txApp.FindCollectionByNameOrId(core.CollectionNameSSOConnections); err == nil {
			return txApp.Delete(col)
		}

		return nil
	})
}
