package oidc

import (
	"regexp"
	"slices"

	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/hook"
)

var clientIDRegex = regexp.MustCompile(`^[A-Za-z0-9._~-]{8,100}$`)

// hiddenFields lists, per system collection, what the records API must never
// return. The API unhides every hidden field for superusers, so it is forced
// hidden again in OnRecordEnrich (the same trick as _totps).
var hiddenFields = map[string][]string{
	core.CollectionNameOAuthClients:       {"secretHash"},
	core.CollectionNameOAuthRequests:      {"requestId", "data", "bindingHash", "codeHash"},
	core.CollectionNameOAuthRefreshTokens: {"tokenHash"},
	core.CollectionNameOAuthSso:           {"ssoHash"},
	core.CollectionNameSigningKeys:        {"privateKey"},
}

func (m *module) bindRecordHooks() {
	for name, fields := range hiddenFields {
		app := m.app

		app.OnRecordEnrich(name).Bind(&hook.Handler[*core.RecordEnrichEvent]{
			Id: "__clavigerOIDCHide_" + name + "__",
			Func: func(e *core.RecordEnrichEvent) error {
				e.Record.Hide(fields...)

				err := e.Next()

				e.Record.Hide(fields...)

				return err
			},
			Priority: 100,
		})
	}

	m.app.OnRecordValidate(core.CollectionNameOAuthClients).Bind(&hook.Handler[*core.RecordEvent]{
		Id: "__clavigerOIDCClientValidate__",
		Func: func(e *core.RecordEvent) error {
			if errs := m.validateClient(e.Record); len(errs) > 0 {
				return errs
			}

			return e.Next()
		},
	})
}

func fieldErr(code, msg string) error { return validation.NewError(code, msg) }

// validateClient checks what the field definitions cannot.
func (m *module) validateClient(rec *core.Record) validation.Errors {
	errs := validation.Errors{}

	if !clientIDRegex.MatchString(rec.GetString("clientId")) {
		errs["clientId"] = fieldErr("validation_invalid_client_id", "Must be 8 to 100 characters: letters, digits and . _ ~ -")
	}

	col, err := m.app.FindCachedCollectionByNameOrId(rec.GetString("authCollection"))
	if err != nil || !col.IsAuth() || col.Name == core.CollectionNameSuperusers {
		errs["authCollection"] = fieldErr("validation_invalid_auth_collection", "Must name an auth collection other than _superusers.")
	}

	var redirects, postLogout, scopes []string
	_ = rec.UnmarshalJSONField("redirectUris", &redirects)
	_ = rec.UnmarshalJSONField("postLogoutUris", &postLogout)
	_ = rec.UnmarshalJSONField("scopes", &scopes)

	if len(redirects) == 0 {
		errs["redirectUris"] = fieldErr("validation_required", "At least one redirect URI is required.")
	}

	for _, u := range redirects {
		if !validRedirectURI(u) {
			errs["redirectUris"] = fieldErr("validation_invalid_redirect_uri",
				"Must be an absolute https URL (http only on localhost) without wildcards or a fragment: "+u)

			break
		}
	}

	for _, u := range postLogout {
		if !validRedirectURI(u) {
			errs["postLogoutUris"] = fieldErr("validation_invalid_redirect_uri", "Invalid post logout redirect URI: "+u)

			break
		}
	}

	for _, sc := range scopes {
		if sc == "" || len(sc) > 100 || slices.ContainsFunc([]rune(sc), func(r rune) bool { return r <= ' ' || r == '"' || r == '\\' || r > '~' }) {
			errs["scopes"] = fieldErr("validation_invalid_scope", "Invalid scope: "+sc)

			break
		}
	}

	if rec.GetBool("public") && rec.GetString("secretHash") != "" {
		errs["public"] = fieldErr("validation_public_with_secret", "A public client has no secret.")
	}

	if !rec.GetBool("public") && rec.GetString("secretHash") == "" {
		errs["public"] = fieldErr("validation_missing_secret",
			"A confidential client is created with POST /api/oidc/clients, which generates its secret.")
	}

	return errs
}
