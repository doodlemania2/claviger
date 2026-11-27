package migrations

import (
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #92): creates _ssoFlows, the state of an inbound
// single sign-on in progress. A row is created when the browser is sent to the
// identity provider (state hash, nonce, PKCE verifier, browser binding hash),
// consumed by the callback, and replaced by a one-time exchange code (hash) for
// the page that started the sign-in. Rows live minutes and are deleted when
// used or expired. Nothing here can be turned into a session without the
// browser's binding cookie.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		if _, err := txApp.FindCollectionByNameOrId(core.CollectionNameSSOFlows); err == nil {
			return nil
		}

		conns, err := txApp.FindCollectionByNameOrId(core.CollectionNameSSOConnections)
		if err != nil {
			return nil
		}

		col := core.NewBaseCollection(core.CollectionNameSSOFlows)
		col.System = true
		col.Fields.Add(
			&core.RelationField{Name: "connection", System: true, Required: true, CollectionId: conns.Id, MaxSelect: 1, CascadeDelete: true},
			&core.TextField{Name: "stateHash", System: true, Required: true, Max: 64},
			&core.TextField{Name: "bindHash", System: true, Hidden: true, Max: 64},
			&core.TextField{Name: "nonce", System: true, Hidden: true, Max: 200},
			&core.TextField{Name: "verifier", System: true, Hidden: true, Max: 200},
			&core.TextField{Name: "returnTo", System: true, Max: 1000},
			&core.SelectField{Name: "phase", System: true, Required: true, MaxSelect: 1, Values: []string{"pending", "done"}},
			&core.TextField{Name: "codeHash", System: true, Hidden: true, Max: 64},
			// the signed-in account, set when the phase is "done"
			&core.TextField{Name: "recordRef", System: true, Max: 100},
			&core.TextField{Name: "collectionRef", System: true, Max: 100},
			&core.DateField{Name: "expires", System: true, Required: true},
			&core.AutodateField{Name: "created", System: true, OnCreate: true},
		)
		col.AddIndex("idx_ssoFlows_state", true, "stateHash", "")
		col.AddIndex("idx_ssoFlows_code", false, "codeHash", "`codeHash` != ''")
		col.AddIndex("idx_ssoFlows_expires", false, "expires", "")

		if err := txApp.Save(col); err != nil {
			return err
		}

		return core.RegisterDataMapEntry(txApp, core.DataMapEntry{
			Collection: core.CollectionNameSSOFlows, Treatment: core.DataMapTreatmentErase,
			SubjectPaths: []string{"recordRef"},
			Note:         "Sign-in state of an inbound SSO in progress (#92). Lives minutes; holds only hashes and the account id.",
		})
	}, func(txApp core.App) error {
		if col, err := txApp.FindCollectionByNameOrId(core.CollectionNameSSOFlows); err == nil {
			return txApp.Delete(col)
		}

		return nil
	})
}
