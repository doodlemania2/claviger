package oidc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/doodlemania2/claviger/core"
	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"
)

const (
	// AccessTokenLifetime is the lifetime of access tokens.
	AccessTokenLifetime = 5 * time.Minute

	// IDTokenLifetime is the lifetime of ID tokens.
	IDTokenLifetime = time.Hour

	// RefreshTokenLifetime is how long a refresh token (and so a rotation
	// chain that is used at least this often) stays valid. A refresh also
	// needs the Claviger session behind it to be alive.
	RefreshTokenLifetime = 30 * 24 * time.Hour

	// MaxSessionLifetime is the absolute cap of a Claviger session that OIDC
	// refreshes keep alive, counted from the sign-in. A refresh extends the
	// session by the collection's auth token duration, never past this: after
	// 30 days of uninterrupted use the user signs in again.
	MaxSessionLifetime = 30 * 24 * time.Hour

	authRequestLifetime = 10 * time.Minute
	authCodeLifetime    = time.Minute
)

// DefaultScopes are the scopes a client may use when it lists none.
var DefaultScopes = []string{oidc.ScopeOpenID, oidc.ScopeProfile, oidc.ScopeEmail, oidc.ScopeOfflineAccess}

// randomToken returns n random bytes, base64url encoded.
func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)

	return base64.RawURLEncoding.EncodeToString(b)
}

// hashToken is the stored form of codes, refresh tokens, secrets and bindings.
// The inputs are high entropy random values, so a plain SHA-256 is enough.
func hashToken(v string) string {
	sum := sha256.Sum256([]byte(v))

	return hex.EncodeToString(sum[:])
}

func hashEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// Client is a registered relying party (a _oauthClients row).
type Client struct {
	app    core.App
	record *core.Record

	ClientID       string
	Name           string
	SecretHash     string
	Public         bool
	FirstParty     bool
	Disabled       bool
	RequirePKCE    bool
	AuthCollection string
	Redirects      []string
	PostLogout     []string
	Scopes         []string
	Opaque         bool
	Alg            string
	OrgID          string // optional org binding (#79a)
	devMode        bool
}

func (s *Store) clientFromRecord(rec *core.Record) *Client {
	c := &Client{
		app:            s.app,
		record:         rec,
		ClientID:       rec.GetString("clientId"),
		Name:           rec.GetString("name"),
		SecretHash:     rec.GetString("secretHash"),
		Public:         rec.GetBool("public"),
		FirstParty:     rec.GetBool("firstParty"),
		Disabled:       rec.GetBool("disabled"),
		RequirePKCE:    rec.GetBool("requirePkce"),
		AuthCollection: rec.GetString("authCollection"),
		Opaque:         rec.GetString("accessTokenType") == "opaque",
		Alg:            rec.GetString("signingAlg"),
		OrgID:          rec.GetString("org"),
		devMode:        s.app.IsDev(),
	}

	_ = rec.UnmarshalJSONField("redirectUris", &c.Redirects)
	_ = rec.UnmarshalJSONField("postLogoutUris", &c.PostLogout)
	_ = rec.UnmarshalJSONField("scopes", &c.Scopes)

	if c.Alg != AlgES256 {
		c.Alg = AlgRS256
	}

	if len(c.Scopes) == 0 {
		c.Scopes = DefaultScopes
	}

	// PKCE is mandatory for public clients whatever the flag says
	if c.Public {
		c.RequirePKCE = true
	}

	return c
}

// findClient loads an enabled client.
func (s *Store) findClient(clientID string) (*Client, error) {
	if clientID == "" {
		return nil, errors.New("unknown client")
	}

	rec, err := s.app.FindFirstRecordByData(core.CollectionNameOAuthClients, "clientId", clientID)
	if err != nil {
		return nil, errors.New("unknown client")
	}

	c := s.clientFromRecord(rec)
	if c.Disabled {
		return nil, errors.New("client is disabled")
	}

	return c, nil
}

// collection resolves the client's auth collection.
func (c *Client) collection() (*core.Collection, error) {
	col, err := c.app.FindCachedCollectionByNameOrId(c.AuthCollection)
	if err != nil {
		return nil, err
	}

	if !col.IsAuth() || col.Name == core.CollectionNameSuperusers {
		return nil, errors.New("invalid auth collection for the client")
	}

	return col, nil
}

func (c *Client) allows(scope string) bool {
	if scope == oidc.ScopeOpenID {
		return true
	}

	return slices.Contains(c.Scopes, scope)
}

// ---- op.Client ---------------------------------------------------------

func (c *Client) GetID() string                    { return c.ClientID }
func (c *Client) RedirectURIs() []string           { return c.Redirects }
func (c *Client) PostLogoutRedirectURIs() []string { return c.PostLogout }

func (c *Client) ApplicationType() op.ApplicationType {
	if c.Public {
		return op.ApplicationTypeNative
	}

	return op.ApplicationTypeWeb
}

func (c *Client) AuthMethod() oidc.AuthMethod {
	if c.Public {
		return oidc.AuthMethodNone
	}

	return oidc.AuthMethodBasic
}

// ResponseTypes: only the authorization code flow. The implicit and hybrid
// flows are not offered.
func (c *Client) ResponseTypes() []oidc.ResponseType {
	return []oidc.ResponseType{oidc.ResponseTypeCode}
}

func (c *Client) GrantTypes() []oidc.GrantType {
	return []oidc.GrantType{oidc.GrantTypeCode, oidc.GrantTypeRefreshToken}
}

func (c *Client) LoginURL(id string) string {
	return "/oidc/login?authRequestID=" + url.QueryEscape(id)
}

func (c *Client) AccessTokenType() op.AccessTokenType {
	if c.Opaque {
		return op.AccessTokenTypeBearer
	}

	return op.AccessTokenTypeJWT
}

func (c *Client) IDTokenLifetime() time.Duration { return IDTokenLifetime }

// DevMode allows http redirect URIs for web clients. Only dev mode does.
func (c *Client) DevMode() bool { return c.devMode }

func (c *Client) RestrictAdditionalIdTokenScopes() func([]string) []string {
	return func(scopes []string) []string { return scopes }
}

func (c *Client) RestrictAdditionalAccessTokenScopes() func([]string) []string {
	return func(scopes []string) []string { return scopes }
}

func (c *Client) IsScopeAllowed(scope string) bool     { return c.allows(scope) }
func (c *Client) IDTokenUserinfoClaimsAssertion() bool { return true }
func (c *Client) ClockSkew() time.Duration             { return 0 }

// ---- op.OPStorage client part -------------------------------------------

// GetClientByClientID implements op.Storage.
func (s *Store) GetClientByClientID(_ context.Context, clientID string) (op.Client, error) {
	c, err := s.findClient(clientID)
	if err != nil {
		return nil, err
	}

	return c, nil
}

// AuthorizeClientIDSecret implements op.Storage.
func (s *Store) AuthorizeClientIDSecret(_ context.Context, clientID, secret string) error {
	c, err := s.findClient(clientID)
	if err != nil || c.Public || c.SecretHash == "" || secret == "" {
		// same answer for every failure, and the same work
		_ = hashEqual(hashToken(secret), hashToken("x"))

		return errors.New("invalid client credentials")
	}

	if !hashEqual(c.SecretHash, hashToken(secret)) {
		return errors.New("invalid client credentials")
	}

	return nil
}

// validRedirectURI is the registration-time check of a redirect URI: absolute,
// no fragment, no wildcard, no userinfo, https unless loopback (or a custom
// scheme, for native apps).
func validRedirectURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Fragment != "" || u.User != nil || strings.Contains(raw, "*") {
		return false
	}

	switch u.Scheme {
	case "https":
		return u.Host != ""
	case "http":
		host := u.Hostname()

		return host == "localhost" || host == "127.0.0.1" || host == "::1"
	case "javascript", "data", "vbscript", "file":
		return false
	}

	// custom scheme of a native app (reverse domain style)
	return strings.Contains(u.Scheme, ".") || strings.Contains(u.Scheme, "-")
}
