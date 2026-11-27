package sso

// Preset is the starting point for a well-known OpenID Connect provider: the
// values to put in a new connection, and what has to be configured on the
// provider's side. It is served by GET /api/orgs/{id}/sso/presets and
// written up in docs/inbound-sso.md.
type Preset struct {
	Id   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
	// IssuerExample shows the shape of the issuer; the part in braces is the
	// organization's own.
	IssuerExample string   `json:"issuerExample"`
	Scopes        []string `json:"scopes"`
	GroupsClaim   string   `json:"groupsClaim"`
	// Notes are the provider-side steps that matter, in English; the admin
	// pages translate them with their own strings.
	Notes []string `json:"notes"`
}

// Presets are the supported starting points.
var Presets = []Preset{
	{
		Id: "okta", Name: "Okta", Type: typeOIDC,
		IssuerExample: "https://{your-okta-domain}/oauth2/default",
		Scopes:        []string{"openid", "email", "profile", "groups"},
		GroupsClaim:   "groups",
		Notes: []string{
			"Create an OIDC web application. Sign-in redirect URI: <app url>/api/sso/callback.",
			"Use the issuer of the authorization server that issues the tokens (the \"default\" custom server, or the org server).",
			"Add a groups claim to the ID token (Security > API > Authorization Servers > Claims: name groups, include in ID token, filter by regex) to map groups to roles.",
			"Okta sends email_verified; an address that Okta has not verified is not linked to an existing account.",
		},
	},
	{
		Id: "entra", Name: "Microsoft Entra ID", Type: typeOIDC,
		IssuerExample: "https://login.microsoftonline.com/{tenant-id}/v2.0",
		Scopes:        []string{"openid", "email", "profile"},
		GroupsClaim:   "groups",
		Notes: []string{
			"Register a web application. Redirect URI (Web): <app url>/api/sso/callback. Create a client secret.",
			"Use the tenant id in the issuer. The multi-tenant endpoints (common, organizations) have no fixed issuer and do not work.",
			"Entra does not send email_verified. Add the optional ID-token claims email and xms_edov (verified domain owner): with xms_edov the address counts as verified.",
			"Groups: under Token configuration add a groups claim. It carries group object ids, so the role mapping keys are object ids. Users in more than 200 groups get no groups claim (overage); assign the application to the relevant groups and emit only those.",
		},
	},
	{
		Id: "google", Name: "Google Workspace", Type: typeOIDC,
		IssuerExample: "https://accounts.google.com",
		Scopes:        []string{"openid", "email", "profile"},
		GroupsClaim:   "hd",
		Notes: []string{
			"Create an OAuth client (Web application) in Google Cloud Console. Authorized redirect URI: <app url>/api/sso/callback.",
			"Set the consent screen to Internal so only your Workspace can sign in.",
			"Google sends email_verified, and hd (the Workspace domain). It sends no groups: the role mapping can only key on hd, so everyone gets the default role.",
		},
	},
	{
		Id: "oidc", Name: "Other OpenID Connect provider", Type: typeOIDC,
		IssuerExample: "https://idp.example.org/realms/main",
		Scopes:        []string{"openid", "email", "profile"},
		GroupsClaim:   "groups",
		Notes: []string{
			"Any provider with a discovery document at <issuer>/.well-known/openid-configuration, the authorization code flow with PKCE, and asymmetric ID-token signatures (RS/PS/ES/EdDSA).",
		},
	},
}
