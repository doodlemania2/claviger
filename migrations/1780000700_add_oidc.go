package migrations

import (
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #71): creates the hidden system collections of the
// OpenID Connect provider. All rules are null (superusers only through the
// records API; the sensitive fields are re-hidden by the oidc package).
//
// Rows of _oauthRequests, _oauthTokens and _oauthRefreshTokens are written
// with plain SQL at runtime (like _sessions) so token issuance fires no model
// hooks and adds no audit rows.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		create := func(name string, build func(col *core.Collection)) error {
			if _, err := txApp.FindCollectionByNameOrId(name); err == nil {
				return nil
			}

			col := core.NewBaseCollection(name)
			col.System = true
			build(col)

			return txApp.Save(col)
		}

		created := func(col *core.Collection) {
			col.Fields.Add(&core.AutodateField{Name: "created", System: true, OnCreate: true})
		}

		createdUpdated := func(col *core.Collection) {
			created(col)
			col.Fields.Add(&core.AutodateField{Name: "updated", System: true, OnCreate: true, OnUpdate: true})
		}

		if err := create(core.CollectionNameOAuthClients, func(col *core.Collection) {
			col.Fields.Add(&core.TextField{Name: "clientId", System: true, Required: true, Max: 100})
			col.Fields.Add(&core.TextField{Name: "name", System: true, Required: true, Max: 200})
			col.Fields.Add(&core.TextField{Name: "secretHash", System: true, Hidden: true, Max: 200})
			col.Fields.Add(&core.BoolField{Name: "public", System: true})
			col.Fields.Add(&core.BoolField{Name: "firstParty", System: true})
			col.Fields.Add(&core.BoolField{Name: "disabled", System: true})
			col.Fields.Add(&core.BoolField{Name: "requirePkce", System: true})
			col.Fields.Add(&core.TextField{Name: "authCollection", System: true, Required: true, Max: 100})
			col.Fields.Add(&core.JSONField{Name: "redirectUris", System: true})
			col.Fields.Add(&core.JSONField{Name: "postLogoutUris", System: true})
			col.Fields.Add(&core.JSONField{Name: "scopes", System: true})
			col.Fields.Add(&core.SelectField{Name: "accessTokenType", System: true, Values: []string{"jwt", "opaque"}, MaxSelect: 1})
			col.Fields.Add(&core.SelectField{Name: "signingAlg", System: true, Values: []string{"RS256", "ES256"}, MaxSelect: 1})
			createdUpdated(col)

			col.AddIndex("idx_oauthClients_clientId", true, "clientId", "")
		}); err != nil {
			return err
		}

		if err := create(core.CollectionNameOAuthRequests, func(col *core.Collection) {
			col.Fields.Add(&core.TextField{Name: "requestId", System: true, Required: true, Hidden: true, Max: 100})
			col.Fields.Add(&core.TextField{Name: "clientId", System: true, Max: 100})
			col.Fields.Add(&core.TextField{Name: "data", System: true, Hidden: true, Max: 20000})
			col.Fields.Add(&core.TextField{Name: "bindingHash", System: true, Hidden: true, Max: 100})
			col.Fields.Add(&core.TextField{Name: "status", System: true, Max: 20})
			col.Fields.Add(&core.TextField{Name: "collectionRef", System: true, Max: 100})
			col.Fields.Add(&core.TextField{Name: "subject", System: true, Max: 100})
			col.Fields.Add(&core.TextField{Name: "sid", System: true, Max: 100})
			col.Fields.Add(&core.TextField{Name: "amr", System: true, Max: 500})
			col.Fields.Add(&core.TextField{Name: "acr", System: true, Max: 200})
			col.Fields.Add(&core.DateField{Name: "authTime", System: true})
			col.Fields.Add(&core.TextField{Name: "codeHash", System: true, Hidden: true, Max: 100})
			col.Fields.Add(&core.BoolField{Name: "codeUsed", System: true})
			col.Fields.Add(&core.DateField{Name: "codeExpires", System: true})
			col.Fields.Add(&core.TextField{Name: "familyId", System: true, Max: 100})
			col.Fields.Add(&core.DateField{Name: "expires", System: true})
			created(col)

			col.AddIndex("idx_oauthRequests_requestId", true, "requestId", "")
			col.AddIndex("idx_oauthRequests_codeHash", false, "codeHash", "")
			col.AddIndex("idx_oauthRequests_expires", false, "expires", "")
		}); err != nil {
			return err
		}

		if err := create(core.CollectionNameOAuthTokens, func(col *core.Collection) {
			col.Fields.Add(&core.TextField{Name: "tokenId", System: true, Required: true, Max: 100})
			col.Fields.Add(&core.TextField{Name: "clientId", System: true, Max: 100})
			col.Fields.Add(&core.TextField{Name: "collectionRef", System: true, Max: 100})
			col.Fields.Add(&core.TextField{Name: "subject", System: true, Max: 100})
			col.Fields.Add(&core.TextField{Name: "sid", System: true, Max: 100})
			col.Fields.Add(&core.TextField{Name: "scopes", System: true, Max: 2000})
			col.Fields.Add(&core.TextField{Name: "audience", System: true, Max: 2000})
			col.Fields.Add(&core.TextField{Name: "familyId", System: true, Max: 100})
			col.Fields.Add(&core.DateField{Name: "expires", System: true})
			col.Fields.Add(&core.BoolField{Name: "revoked", System: true})
			created(col)

			col.AddIndex("idx_oauthTokens_tokenId", true, "tokenId", "")
			col.AddIndex("idx_oauthTokens_family", false, "familyId", "")
			col.AddIndex("idx_oauthTokens_sid", false, "sid", "")
			col.AddIndex("idx_oauthTokens_expires", false, "expires", "")
		}); err != nil {
			return err
		}

		if err := create(core.CollectionNameOAuthRefreshTokens, func(col *core.Collection) {
			col.Fields.Add(&core.TextField{Name: "tokenHash", System: true, Required: true, Hidden: true, Max: 100})
			col.Fields.Add(&core.TextField{Name: "familyId", System: true, Max: 100})
			col.Fields.Add(&core.TextField{Name: "clientId", System: true, Max: 100})
			col.Fields.Add(&core.TextField{Name: "collectionRef", System: true, Max: 100})
			col.Fields.Add(&core.TextField{Name: "subject", System: true, Max: 100})
			col.Fields.Add(&core.TextField{Name: "sid", System: true, Max: 100})
			col.Fields.Add(&core.TextField{Name: "scopes", System: true, Max: 2000})
			col.Fields.Add(&core.TextField{Name: "audience", System: true, Max: 2000})
			col.Fields.Add(&core.TextField{Name: "amr", System: true, Max: 500})
			col.Fields.Add(&core.TextField{Name: "acr", System: true, Max: 200})
			col.Fields.Add(&core.DateField{Name: "authTime", System: true})
			col.Fields.Add(&core.DateField{Name: "expires", System: true})
			col.Fields.Add(&core.BoolField{Name: "used", System: true})
			col.Fields.Add(&core.BoolField{Name: "revoked", System: true})
			created(col)

			col.AddIndex("idx_oauthRefresh_hash", true, "tokenHash", "")
			col.AddIndex("idx_oauthRefresh_family", false, "familyId", "")
			col.AddIndex("idx_oauthRefresh_sid", false, "sid", "")
			col.AddIndex("idx_oauthRefresh_expires", false, "expires", "")
		}); err != nil {
			return err
		}

		if err := create(core.CollectionNameOAuthConsents, func(col *core.Collection) {
			col.Fields.Add(&core.TextField{Name: "collectionRef", System: true, Required: true, Max: 100})
			col.Fields.Add(&core.TextField{Name: "recordRef", System: true, Required: true, Max: 100})
			col.Fields.Add(&core.TextField{Name: "clientId", System: true, Required: true, Max: 100})
			col.Fields.Add(&core.JSONField{Name: "scopes", System: true})
			createdUpdated(col)

			col.AddIndex("idx_oauthConsents_owner", true, "collectionRef, recordRef, clientId", "")
		}); err != nil {
			return err
		}

		return create(core.CollectionNameSigningKeys, func(col *core.Collection) {
			col.Fields.Add(&core.TextField{Name: "kid", System: true, Required: true, Max: 100})
			col.Fields.Add(&core.SelectField{Name: "alg", System: true, Required: true, Values: []string{"RS256", "ES256"}, MaxSelect: 1})
			col.Fields.Add(&core.TextField{Name: "privateKey", System: true, Required: true, Hidden: true, Max: 20000})
			col.Fields.Add(&core.TextField{Name: "publicJwk", System: true, Max: 5000})
			col.Fields.Add(&core.BoolField{Name: "encrypted", System: true})
			col.Fields.Add(&core.DateField{Name: "retiresAt", System: true})
			created(col)

			col.AddIndex("idx_signingKeys_kid", true, "kid", "")
			// at most one active (not retiring) key per algorithm
			col.AddIndex("idx_signingKeys_active", true, "alg", "retiresAt = ''")
		})
	}, func(txApp core.App) error {
		for _, name := range []string{
			core.CollectionNameOAuthClients,
			core.CollectionNameOAuthRequests,
			core.CollectionNameOAuthTokens,
			core.CollectionNameOAuthRefreshTokens,
			core.CollectionNameOAuthConsents,
			core.CollectionNameSigningKeys,
		} {
			if col, err := txApp.FindCollectionByNameOrId(name); err == nil {
				if err := txApp.Delete(col); err != nil {
					return err
				}
			}
		}

		return nil
	})
}
