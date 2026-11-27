package migrations

import (
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #92): creates _ssoDomains, the email domains an
// organization claims for single sign-on. A claim is "pending" until its DNS
// TXT record is seen, then "verified"; only a verified claim routes anyone.
// Two organizations may both hold a pending claim for a domain, but a domain
// is verified for at most one (partial unique index).
//
// All rules are null (superusers only through the records API); the routes are
// /api/orgs/{id}/sso/domains. It does nothing without _ssoConnections.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		if _, err := txApp.FindCollectionByNameOrId(core.CollectionNameSSODomains); err == nil {
			return nil
		}

		orgs, err := txApp.FindCollectionByNameOrId(core.CollectionNameOrgs)
		if err != nil {
			return nil
		}

		conns, err := txApp.FindCollectionByNameOrId(core.CollectionNameSSOConnections)
		if err != nil {
			return nil
		}

		col := core.NewBaseCollection(core.CollectionNameSSODomains)
		col.System = true
		col.Fields.Add(
			&core.RelationField{Name: "org", System: true, Required: true, CollectionId: orgs.Id, MaxSelect: 1, CascadeDelete: true},
			&core.RelationField{Name: "connection", System: true, Required: true, CollectionId: conns.Id, MaxSelect: 1, CascadeDelete: true},
			&core.TextField{Name: "domain", System: true, Required: true, Max: 253},
			// the value the TXT record must carry is "claviger-verification=<token>"
			&core.TextField{Name: "token", System: true, Required: true, Max: 100},
			&core.SelectField{Name: "status", System: true, Required: true, MaxSelect: 1,
				Values: []string{core.SSODomainPending, core.SSODomainVerified}},
			&core.DateField{Name: "verifiedAt", System: true},
			&core.DateField{Name: "lastCheckedAt", System: true},
			&core.AutodateField{Name: "created", System: true, OnCreate: true},
			&core.AutodateField{Name: "updated", System: true, OnCreate: true, OnUpdate: true},
		)
		col.AddIndex("idx_ssoDomains_org_domain", true, "org, domain", "")
		col.AddIndex("idx_ssoDomains_verified", true, "domain", "`status` = 'verified'")
		col.AddIndex("idx_ssoDomains_connection", false, "connection", "")

		if err := txApp.Save(col); err != nil {
			return err
		}

		return core.RegisterDataMapEntry(txApp, core.DataMapEntry{
			Collection: core.CollectionNameSSODomains, Treatment: core.DataMapTreatmentNone,
			Note: "Email domains an organization claims for single sign-on (#92). Not about a person.",
		})
	}, func(txApp core.App) error {
		if col, err := txApp.FindCollectionByNameOrId(core.CollectionNameSSODomains); err == nil {
			return txApp.Delete(col)
		}

		return nil
	})
}
