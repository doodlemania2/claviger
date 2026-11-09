package core

// Fork-local (Claviger #71): names of the hidden system collections behind the
// OpenID Connect provider (package oidc). The collections are created by
// migrations/1780000700_add_oidc.go
// (and 1780000710_add_oidc_sso.go). The provider itself lives in the oidc
// package; only the names are shared so the migration does not import it.
const (
	// CollectionNameOAuthClients holds the registered relying parties.
	CollectionNameOAuthClients = "_oauthClients"

	// CollectionNameOAuthRequests holds pending authorize requests and, once
	// the user signed in, the single-use authorization code.
	CollectionNameOAuthRequests = "_oauthRequests"

	// CollectionNameOAuthTokens holds the issued access tokens (by id, never the token itself).
	CollectionNameOAuthTokens = "_oauthTokens"

	// CollectionNameOAuthRefreshTokens holds the hashed refresh tokens.
	CollectionNameOAuthRefreshTokens = "_oauthRefreshTokens"

	// CollectionNameOAuthConsents remembers what a user granted a client.
	CollectionNameOAuthConsents = "_oauthConsents"

	// CollectionNameOAuthSso holds the single sign-on sessions of the hosted
	// sign-in page: a hashed random cookie value bound to a Claviger session.
	CollectionNameOAuthSso = "_oauthSso"

	// CollectionNameOAuthDeviceCodes holds the pending device authorizations
	// (RFC 8628): the hashed device_code and user_code, and the state of the
	// approval (#96). Created by migrations/1780001300_add_oidc_device.go.
	CollectionNameOAuthDeviceCodes = "_oauthDeviceCodes"

	// CollectionNameSigningKeys holds the (encrypted) token signing keys.
	CollectionNameSigningKeys = "_signingKeys"
)
