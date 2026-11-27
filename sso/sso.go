// Package sso implements inbound enterprise single sign-on (#92): an
// organization connects its own identity provider, proves it owns its email
// domains with a DNS TXT record, and its people then sign in to Claviger
// through that provider.
//
// # The connection-type seam
//
// A connection type is a [Provider] registered with [Register]. The engine
// (flow state, state/nonce/PKCE bookkeeping, the browser binding, domain
// routing, account linking, just-in-time provisioning, role mapping, the
// one-time exchange code) is protocol independent. A Provider only does the
// protocol part:
//
//   - Validate checks the connection settings when an admin saves them;
//   - Begin builds the URL the browser is sent to;
//   - Complete verifies what the identity provider sent back to the callback
//     and returns the asserted [Identity].
//
// "oidc" is implemented in oidc.go. A SAML 2.0 type implements the same
// interface in its own file (sso/saml.go) and calls Register in init(): it
// reads its settings (IdP metadata, entity id, certificates) from
// Connection.Config, uses the flow's State as the RelayState, and receives the
// POSTed SAMLResponse in CompleteInput.Form (the callback route accepts POST).
// Nothing else in the engine or the routes changes; the "saml" value is already
// accepted by the type field of _ssoConnections.
package sso

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/doodlemania2/claviger/core"
)

// Identity is what an identity provider asserted about the person, after the
// Provider verified it. The engine decides what it is worth (see
// ProvisionLogin): a provider only reports.
type Identity struct {
	// Subject is the provider's stable identifier of the person (OIDC "sub",
	// SAML NameID). Required.
	Subject string
	// Email is the asserted address; EmailVerified says the provider vouches
	// that the person controls it.
	Email         string
	EmailVerified bool
	Name          string
	// Groups are the values of the configured groups claim (or SAML attribute).
	Groups []string
	// Claims is the raw set of verified claims or attributes, for debugging and
	// for mappings on other claims.
	Claims map[string]any
}

// BeginInput is what a Provider gets to start a sign-in.
type BeginInput struct {
	// CallbackURL is the redirect URI registered at the identity provider
	// (<app url>/api/sso/callback; the same for every connection).
	CallbackURL string
	// State is the opaque value the provider must return unchanged (OIDC state,
	// SAML RelayState). Nonce and CodeVerifier belong to this one sign-in.
	State        string
	Nonce        string
	CodeVerifier string
	// LoginHint is the email the person typed, when known.
	LoginHint string
}

// BeginOutput is where to send the browser.
type BeginOutput struct {
	RedirectURL string
}

// CompleteInput is what the callback received.
type CompleteInput struct {
	CallbackURL  string
	Nonce        string
	CodeVerifier string
	Method       string
	// Query and Form carry the parameters of the callback request (a GET
	// carries Query, a POST such as the SAML assertion consumer carries Form).
	Query url.Values
	Form  url.Values
}

// Provider is one connection type.
type Provider interface {
	// Type is the value of _ssoConnections.type.
	Type() string
	// Validate checks the settings of a connection. A nil error means it can be
	// saved; it must not call the network.
	Validate(c *Connection) error
	Begin(ctx context.Context, c *Connection, in BeginInput) (*BeginOutput, error)
	Complete(ctx context.Context, c *Connection, in CompleteInput) (*Identity, error)
}

var (
	providersMu sync.RWMutex
	providers   = map[string]Provider{}
)

// Register adds a connection type. It panics on a duplicate type.
func Register(p Provider) {
	providersMu.Lock()
	defer providersMu.Unlock()

	if _, ok := providers[p.Type()]; ok {
		panic("sso: duplicate provider type " + p.Type())
	}

	providers[p.Type()] = p
}

// ProviderFor returns the implementation of a connection type.
func ProviderFor(typ string) (Provider, bool) {
	providersMu.RLock()
	defer providersMu.RUnlock()

	p, ok := providers[typ]

	return p, ok
}

// Types lists the registered connection types.
func Types() []string {
	providersMu.RLock()
	defer providersMu.RUnlock()

	out := make([]string, 0, len(providers))
	for t := range providers {
		out = append(out, t)
	}

	slices.Sort(out)

	return out
}

// Error is a failure of an SSO operation. Code is stable and shown to people
// through the hosted page (the "js.sso.err.<code>" message).
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func newErr(status int, code, msg string) *Error {
	return &Error{Status: status, Code: code, Message: msg}
}

// AsError unwraps an *Error.
func AsError(err error) (*Error, bool) {
	var e *Error

	ok := errors.As(err, &e)

	return e, ok
}

// Connection is the decoded form of a _ssoConnections row.
type Connection struct {
	Record *core.Record

	Id          string
	OrgId       string
	Name        string
	Type        string
	Enabled     bool
	SSORequired bool
	JIT         bool

	Issuer       string
	ClientID     string
	ClientSecret string
	Scopes       []string
	GroupsClaim  string
	RoleMapping  map[string]string
	DefaultRole  string

	// Config holds the settings of types that need more than the columns above.
	Config map[string]any
}

// DefaultScopes are requested when a connection names none.
var DefaultScopes = []string{"openid", "email", "profile"}

// LoadConnection decodes a connection record, opening the client secret.
func LoadConnection(app core.App, rec *core.Record) (*Connection, error) {
	c := &Connection{
		Record:      rec,
		Id:          rec.Id,
		OrgId:       rec.GetString("org"),
		Name:        rec.GetString("name"),
		Type:        rec.GetString("type"),
		Enabled:     rec.GetBool("enabled"),
		SSORequired: rec.GetBool("ssoRequired"),
		JIT:         rec.GetBool("jit"),
		Issuer:      strings.TrimSpace(rec.GetString("issuer")),
		ClientID:    strings.TrimSpace(rec.GetString("clientId")),
		GroupsClaim: strings.TrimSpace(rec.GetString("groupsClaim")),
		DefaultRole: strings.TrimSpace(rec.GetString("defaultRole")),
		Scopes:      strings.Fields(rec.GetString("scopes")),
		RoleMapping: map[string]string{},
		Config:      map[string]any{},
	}

	if len(c.Scopes) == 0 {
		c.Scopes = slices.Clone(DefaultScopes)
	}

	if c.GroupsClaim == "" {
		c.GroupsClaim = "groups"
	}

	if raw := rec.GetString("roleMapping"); raw != "" && raw != "null" {
		if err := rec.UnmarshalJSONField("roleMapping", &c.RoleMapping); err != nil {
			return nil, fmt.Errorf("the role mapping of connection %s is not valid: %w", rec.Id, err)
		}
	}

	if raw := rec.GetString("config"); raw != "" && raw != "null" {
		if err := rec.UnmarshalJSONField("config", &c.Config); err != nil {
			return nil, fmt.Errorf("the config of connection %s is not valid: %w", rec.Id, err)
		}
	}

	secret, err := core.OpenSSOSecret(app, rec.GetString("clientSecret"))
	if err != nil {
		return nil, err
	}

	c.ClientSecret = secret

	return c, nil
}

// FindConnection loads a connection by id.
func FindConnection(app core.App, id string) (*Connection, error) {
	rec, err := app.FindRecordById(core.CollectionNameSSOConnections, id)
	if err != nil {
		return nil, err
	}

	return LoadConnection(app, rec)
}

// ValidateConnection runs the generic checks and the type's own.
func ValidateConnection(c *Connection) error {
	p, ok := ProviderFor(c.Type)
	if !ok {
		return fmt.Errorf("the connection type %q is not available", c.Type)
	}

	if c.DefaultRole != "" && core.OrgRoleRank(c.DefaultRole) == 0 {
		return errors.New("the default role is not an organization role")
	}

	if c.DefaultRole == core.OrgRoleOwner {
		return errors.New("single sign-on cannot assign the owner role")
	}

	for k, role := range c.RoleMapping {
		if strings.TrimSpace(k) == "" {
			return errors.New("the role mapping has an empty key")
		}

		if core.OrgRoleRank(role) == 0 {
			return fmt.Errorf("the role mapping names %q, which is not an organization role", role)
		}

		if role == core.OrgRoleOwner {
			return errors.New("single sign-on cannot assign the owner role")
		}
	}

	return p.Validate(c)
}

// ---- injectable edges ------------------------------------------------------

// TXTResolver looks up TXT records. net.DefaultResolver implements it; tests
// stub it.
type TXTResolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

var (
	edgeMu     sync.RWMutex
	txtLookup  TXTResolver = net.DefaultResolver
	httpClient *http.Client
)

// SetResolver replaces the DNS resolver and returns a function that restores
// the previous one.
func SetResolver(r TXTResolver) (restore func()) {
	edgeMu.Lock()
	prev := txtLookup
	txtLookup = r
	edgeMu.Unlock()

	return func() {
		edgeMu.Lock()
		txtLookup = prev
		edgeMu.Unlock()
	}
}

// SetHTTPClient replaces the client used to reach identity providers and
// returns a function that restores the previous one. nil restores the default
// (a client that refuses private network addresses).
func SetHTTPClient(c *http.Client) (restore func()) {
	edgeMu.Lock()
	prev := httpClient
	httpClient = c
	edgeMu.Unlock()

	return func() {
		edgeMu.Lock()
		httpClient = prev
		edgeMu.Unlock()
	}
}

func resolver() TXTResolver {
	edgeMu.RLock()
	defer edgeMu.RUnlock()

	return txtLookup
}

func idpClient() *http.Client {
	edgeMu.RLock()
	c := httpClient
	edgeMu.RUnlock()

	if c != nil {
		return c
	}

	return defaultClient
}

// AllowPrivateNetworksEnv lets the server reach identity providers on private
// or loopback addresses and over plain http (for an on-premises provider or a
// development setup). Off by default: an organization admin supplies the
// provider URL, so the server must not become a way into the internal network.
const AllowPrivateNetworksEnv = "CLAVIGER_SSO_ALLOW_PRIVATE_NETWORKS"

func allowPrivateNetworks() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(AllowPrivateNetworksEnv)))

	return v == "true" || v == "1"
}

var defaultClient = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, Control: guardDial}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConns:        10,
		IdleConnTimeout:     60 * time.Second,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("too many redirects")
		}

		return nil
	},
}
