package core

// Fork-local (Claviger #71): names of the hidden system collections behind the
// OpenID Connect provider (package oidc). The collections are created by
// migrations/1780000700_add_oidc.go. The provider itself lives in the oidc
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

	// CollectionNameSigningKeys holds the (encrypted) token signing keys.
	CollectionNameSigningKeys = "_signingKeys"
)
