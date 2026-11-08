package azurekv

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TokenCredential returns an access token for a scope such as
// "https://vault.azure.net/.default".
type TokenCredential interface {
	Token(ctx context.Context, scope string) (string, error)

	// Invalidate drops the cached token of the scope (after a 401).
	Invalidate(scope string)
}

// Environment variables read by [CredentialFromEnv]. The AZURE_* names are the
// ones the Azure SDKs and the workload identity webhook already use.
const (
	EnvAuthMode            = "CLAVIGER_KEK_AZURE_AUTH"
	EnvTenantID            = "AZURE_TENANT_ID"
	EnvClientID            = "AZURE_CLIENT_ID"
	EnvClientSecret        = "AZURE_CLIENT_SECRET"
	EnvFederatedTokenFile  = "AZURE_FEDERATED_TOKEN_FILE"
	EnvAuthorityHost       = "AZURE_AUTHORITY_HOST"
	EnvIdentityEndpoint    = "IDENTITY_ENDPOINT"
	EnvIdentityHeader      = "IDENTITY_HEADER"
	defaultAuthorityHost   = "https://login.microsoftonline.com"
	defaultIMDSEndpoint    = "http://169.254.169.254/metadata/identity/oauth2/token"
	imdsAPIVersion         = "2018-02-01"
	identityAPIVersion     = "2019-08-01"
	jwtBearerAssertionType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
)

// Authentication modes of EnvAuthMode.
const (
	AuthAuto             = "auto"
	AuthManagedIdentity  = "managed-identity"
	AuthWorkloadIdentity = "workload-identity"
	AuthClientSecret     = "client-secret"
)

// CredentialFromEnv picks a credential from the environment.
//
// With CLAVIGER_KEK_AZURE_AUTH unset or "auto": workload identity when
// AZURE_FEDERATED_TOKEN_FILE is set, else client secret when
// AZURE_CLIENT_SECRET is set, else managed identity (AZURE_CLIENT_ID, when
// set, selects a user-assigned identity). An explicit mode skips the guessing.
func CredentialFromEnv(getenv func(string) string, opts Options) (TokenCredential, error) {
	mode := strings.ToLower(strings.TrimSpace(getenv(EnvAuthMode)))
	if mode == "" {
		mode = AuthAuto
	}

	tenant := strings.TrimSpace(getenv(EnvTenantID))
	clientID := strings.TrimSpace(getenv(EnvClientID))
	secret := getenv(EnvClientSecret)
	file := strings.TrimSpace(getenv(EnvFederatedTokenFile))
	authority := strings.TrimSpace(getenv(EnvAuthorityHost))

	if mode == AuthAuto {
		switch {
		case file != "":
			mode = AuthWorkloadIdentity
		case secret != "":
			mode = AuthClientSecret
		default:
			mode = AuthManagedIdentity
		}
	}

	switch mode {
	case AuthManagedIdentity:
		return NewManagedIdentity(clientID, getenv(EnvIdentityEndpoint), getenv(EnvIdentityHeader), opts), nil

	case AuthWorkloadIdentity:
		if file == "" || clientID == "" || tenant == "" {
			return nil, fmt.Errorf("%w: workload identity needs %s, %s and %s", ErrNoCredential, EnvFederatedTokenFile, EnvClientID, EnvTenantID)
		}
		return NewWorkloadIdentity(authority, tenant, clientID, file, opts), nil

	case AuthClientSecret:
		if secret == "" || clientID == "" || tenant == "" {
			return nil, fmt.Errorf("%w: client secret auth needs %s, %s and %s", ErrNoCredential, EnvClientSecret, EnvClientID, EnvTenantID)
		}
		return NewClientSecret(authority, tenant, clientID, secret, opts), nil
	}

	return nil, fmt.Errorf("%s=%q is not one of %s, %s, %s, %s", EnvAuthMode, mode, AuthAuto, AuthManagedIdentity, AuthWorkloadIdentity, AuthClientSecret)
}

// ---------------------------------------------------------------------------

// flexNumber decodes a JSON number or a numeric string.
type flexNumber string

func (f *flexNumber) UnmarshalJSON(b []byte) error {
	*f = flexNumber(strings.Trim(string(b), `"`))
	return nil
}

type tokenResponse struct {
	AccessToken string     `json:"access_token"`
	ExpiresIn   flexNumber `json:"expires_in"`
	ExpiresOn   flexNumber `json:"expires_on"`
}

// expiry computes the expiry time of a token response.
func (r tokenResponse) expiry(now time.Time) time.Time {
	if n, err := strconv.ParseInt(string(r.ExpiresOn), 10, 64); err == nil && n > 1e9 {
		return time.Unix(n, 0)
	}

	if n, err := strconv.ParseInt(string(r.ExpiresIn), 10, 64); err == nil && n > 0 {
		return now.Add(time.Duration(n) * time.Second)
	}

	// App Service style "1/2/2026 3:04:05 PM +00:00"
	for _, layout := range []string{"1/2/2006 3:04:05 PM -07:00", "01/02/2006 15:04:05 -07:00"} {
		if tm, err := time.Parse(layout, string(r.ExpiresOn)); err == nil {
			return tm
		}
	}

	return now.Add(5 * time.Minute)
}

type cachedToken struct {
	value   string
	expires time.Time
	issued  time.Time
}

// tokenCache caches tokens per scope and refreshes them shortly before expiry.
type tokenCache struct {
	mu     sync.Mutex
	now    func() time.Time
	tokens map[string]cachedToken
	fetch  func(ctx context.Context, scope string) (tokenResponse, error)
}

func (c *tokenCache) Token(ctx context.Context, scope string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()

	if t, ok := c.tokens[scope]; ok {
		// refresh 5 minutes early, or at 80% of the lifetime for short tokens
		margin := 5 * time.Minute
		if life := t.expires.Sub(t.issued); life > 0 && life < 25*time.Minute {
			margin = life / 5
		}
		if now.Before(t.expires.Add(-margin)) {
			return t.value, nil
		}
	}

	resp, err := c.fetch(ctx, scope)
	if err != nil {
		return "", err
	}
	if resp.AccessToken == "" {
		return "", &Error{Op: "token", Definite: true, Message: "the token endpoint returned no access token"}
	}

	if c.tokens == nil {
		c.tokens = map[string]cachedToken{}
	}
	c.tokens[scope] = cachedToken{value: resp.AccessToken, issued: now, expires: resp.expiry(now)}

	return resp.AccessToken, nil
}

func (c *tokenCache) Invalidate(scope string) {
	c.mu.Lock()
	delete(c.tokens, scope)
	c.mu.Unlock()
}

func decodeToken(op string, status int, body []byte) (tokenResponse, error) {
	if status != http.StatusOK {
		return tokenResponse{}, serviceError(op, status, body)
	}

	var r tokenResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return tokenResponse{}, &Error{Op: op, Status: status, Definite: true, Message: "the token response is not valid JSON"}
	}

	return r, nil
}

// resourceOf converts a ".default" scope to the resource form used by the
// managed identity endpoints.
func resourceOf(scope string) string {
	return strings.TrimSuffix(strings.TrimSuffix(scope, "/.default"), "/")
}

// ---------------------------------------------------------------------------
// managed identity

type managedIdentity struct {
	*tokenCache
	tr       *transport
	clientID string
	endpoint string // IDENTITY_ENDPOINT (empty: IMDS)
	header   string
	imds     string
}

// NewManagedIdentity returns a credential for the managed identity of the
// host. When identityEndpoint and identityHeader (IDENTITY_ENDPOINT and
// IDENTITY_HEADER: Container Apps, App Service, Functions) are set they are
// used; otherwise the instance metadata service (VMs, AKS node identity).
// clientID selects a user-assigned identity; empty means the system-assigned.
func NewManagedIdentity(clientID, identityEndpoint, identityHeader string, opts Options) TokenCredential {
	tr := newTransport(opts)
	if opts.Timeout <= 0 {
		// the metadata endpoints are local: fail fast
		tr.timeout = 3 * time.Second
	}
	if opts.MaxAttempts <= 0 {
		tr.maxAttempts = 5
	}

	m := &managedIdentity{
		tr:       tr,
		clientID: strings.TrimSpace(clientID),
		endpoint: strings.TrimSpace(identityEndpoint),
		header:   identityHeader,
		imds:     opts.IMDSEndpoint,
	}
	if m.imds == "" {
		m.imds = defaultIMDSEndpoint
	}
	if m.endpoint == "" || m.header == "" {
		m.endpoint = ""
	}

	m.tokenCache = &tokenCache{now: tr.now, fetch: m.fetch}

	return m
}

func (m *managedIdentity) fetch(ctx context.Context, scope string) (tokenResponse, error) {
	q := url.Values{}
	q.Set("resource", resourceOf(scope))

	var base string
	if m.endpoint != "" {
		base = m.endpoint
		q.Set("api-version", identityAPIVersion)
		if m.clientID != "" {
			q.Set("client_id", m.clientID)
		}
	} else {
		base = m.imds
		q.Set("api-version", imdsAPIVersion)
		if m.clientID != "" {
			q.Set("client_id", m.clientID)
		}
	}

	target := base + "?" + q.Encode()
	if strings.Contains(base, "?") {
		target = base + "&" + q.Encode()
	}

	// 404 and 410 from IMDS mean "not ready yet": retry them
	retry := func(s int) bool { return defaultRetryable(s) || s == http.StatusNotFound || s == http.StatusGone }

	status, body, err := m.tr.do(ctx, "managed identity token", retry, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, err
		}
		if m.endpoint != "" {
			req.Header.Set("X-IDENTITY-HEADER", m.header)
		} else {
			req.Header.Set("Metadata", "true")
		}
		return req, nil
	})
	if err != nil {
		return tokenResponse{}, err
	}

	return decodeToken("managed identity token", status, body)
}

// ---------------------------------------------------------------------------
// AAD client credentials (secret or federated assertion)

type aadCredential struct {
	*tokenCache
	tr        *transport
	tokenURL  string
	clientID  string
	secret    string // client secret, or
	assertion func() (string, error)
	op        string
}

func tokenURL(authority, tenant string) string {
	authority = strings.TrimSpace(authority)
	if authority == "" {
		authority = defaultAuthorityHost
	}
	if !strings.Contains(authority, "://") {
		authority = "https://" + authority
	}
	return strings.TrimRight(authority, "/") + "/" + url.PathEscape(tenant) + "/oauth2/v2.0/token"
}

// NewClientSecret returns a client-credentials credential (development).
// authority is the AAD host (AZURE_AUTHORITY_HOST), empty for the public cloud.
func NewClientSecret(authority, tenant, clientID, secret string, opts Options) TokenCredential {
	tr := newTransport(opts)
	c := &aadCredential{tr: tr, tokenURL: tokenURL(authority, tenant), clientID: clientID, secret: secret, op: "client secret token"}
	c.tokenCache = &tokenCache{now: tr.now, fetch: c.fetch}
	return c
}

// NewWorkloadIdentity returns a credential that exchanges the federated token
// in tokenFile (re-read on every refresh) for an access token.
func NewWorkloadIdentity(authority, tenant, clientID, tokenFile string, opts Options) TokenCredential {
	tr := newTransport(opts)
	c := &aadCredential{
		tr:       tr,
		tokenURL: tokenURL(authority, tenant),
		clientID: clientID,
		op:       "workload identity token",
		assertion: func() (string, error) {
			b, err := os.ReadFile(tokenFile)
			if err != nil {
				return "", fmt.Errorf("cannot read the federated token file: %w", pathFree(err))
			}
			return strings.TrimSpace(string(b)), nil
		},
	}
	c.tokenCache = &tokenCache{now: tr.now, fetch: c.fetch}
	return c
}

// pathFree strips the path from an *os.PathError.
func pathFree(err error) error {
	if pe, ok := err.(*os.PathError); ok {
		return pe.Err
	}
	return err
}

func (c *aadCredential) fetch(ctx context.Context, scope string) (tokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", c.clientID)
	form.Set("scope", scope)

	if c.assertion != nil {
		a, err := c.assertion()
		if err != nil {
			return tokenResponse{}, &Error{Op: c.op, Definite: true, Err: err}
		}
		form.Set("client_assertion_type", jwtBearerAssertionType)
		form.Set("client_assertion", a)
	} else {
		form.Set("client_secret", c.secret)
	}

	payload := form.Encode()

	status, body, err := c.tr.do(ctx, c.op, nil, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return req, nil
	})
	if err != nil {
		return tokenResponse{}, err
	}

	return decodeToken(c.op, status, body)
}
