package sso

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// The "oidc" connection type: OpenID Connect authorization code flow with PKCE
// against an upstream provider (Okta, Microsoft Entra ID, Google Workspace,
// Keycloak, ...).
//
// Discovery gives the endpoints and the issuer; the ID token is verified
// (signature against the provider's JWKS with an asymmetric algorithm only,
// issuer, audience, authorized party, expiry, not-before, nonce) before one
// claim of it is believed. Nothing is taken from an unverified token, and
// the userinfo endpoint is not used.

const (
	typeOIDC       = "oidc"
	maxBody        = 1 << 20
	clockSkew      = 2 * time.Minute
	discoveryTTL   = 10 * time.Minute
	oidcDiscoPath  = "/.well-known/openid-configuration"
	maxIDTokenSize = 64 << 10
)

// the algorithms an ID token may use: never "none", never a shared-secret one
var idTokenAlgs = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512,
	jose.PS256, jose.PS384, jose.PS512,
	jose.ES256, jose.ES384, jose.ES512,
	jose.EdDSA,
}

type oidcProvider struct{}

func init() { Register(oidcProvider{}) }

func (oidcProvider) Type() string { return typeOIDC }

// NormalizeIssuer strips a pasted discovery URL down to the issuer.
func NormalizeIssuer(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimSuffix(v, oidcDiscoPath)

	return strings.TrimRight(v, "/")
}

func (oidcProvider) Validate(c *Connection) error {
	if c.Issuer == "" {
		return errors.New("the issuer (or discovery) URL is required")
	}

	if err := checkEndpointURL(NormalizeIssuer(c.Issuer)); err != nil {
		return fmt.Errorf("issuer: %w", err)
	}

	if c.ClientID == "" {
		return errors.New("the client id is required")
	}

	if c.ClientSecret == "" {
		return errors.New("the client secret is required")
	}

	if !slices.Contains(c.Scopes, "openid") {
		return errors.New(`the scopes must include "openid"`)
	}

	return nil
}

// checkEndpointURL accepts https URLs, and http only for a literal loopback
// address (a development provider; the dialer still refuses it unless
// CLAVIGER_SSO_ALLOW_PRIVATE_NETWORKS is set).
func checkEndpointURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("not a valid URL")
	}

	if u.User != nil || u.Fragment != "" {
		return errors.New("must not contain credentials or a fragment")
	}

	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if ip, err := netip.ParseAddr(u.Hostname()); err == nil && ip.IsLoopback() {
			return nil
		}

		if u.Hostname() == "localhost" {
			return nil
		}
	}

	return errors.New("must use https")
}

type discoveryDoc struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	AuthMethods           []string `json:"token_endpoint_auth_methods_supported"`
	CodeChallengeMethods  []string `json:"code_challenge_methods_supported"`
}

type discoveryEntry struct {
	doc     *discoveryDoc
	fetched time.Time
}

var (
	discoMu    sync.Mutex
	discoCache = map[string]discoveryEntry{}
)

func getJSON(ctx context.Context, rawURL string, out any) error {
	if err := checkEndpointURL(rawURL); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Accept", "application/json")

	res, err := idpClient().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d", req.URL.Host, res.StatusCode)
	}

	return json.NewDecoder(io.LimitReader(res.Body, maxBody)).Decode(out)
}

func discover(ctx context.Context, issuer string) (*discoveryDoc, error) {
	issuer = NormalizeIssuer(issuer)

	discoMu.Lock()
	if e, ok := discoCache[issuer]; ok && time.Since(e.fetched) < discoveryTTL {
		discoMu.Unlock()

		return e.doc, nil
	}
	discoMu.Unlock()

	var doc discoveryDoc
	if err := getJSON(ctx, issuer+oidcDiscoPath, &doc); err != nil {
		return nil, fmt.Errorf("discovery failed: %w", err)
	}

	if NormalizeIssuer(doc.Issuer) != issuer {
		return nil, fmt.Errorf("the provider reports the issuer %q, not %q", doc.Issuer, issuer)
	}

	for _, ep := range []string{doc.AuthorizationEndpoint, doc.TokenEndpoint, doc.JWKSURI} {
		if err := checkEndpointURL(ep); err != nil {
			return nil, fmt.Errorf("discovery lists an unusable endpoint: %w", err)
		}
	}

	discoMu.Lock()
	discoCache[issuer] = discoveryEntry{doc: &doc, fetched: time.Now()}
	discoMu.Unlock()

	return &doc, nil
}

// Discover fetches and checks the provider's discovery document (used by the
// routes' "test connection" action).
func Discover(ctx context.Context, issuer string) error {
	_, err := discover(ctx, issuer)

	return err
}

// PKCEChallenge is the S256 challenge of a code verifier.
func PKCEChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))

	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (oidcProvider) Begin(ctx context.Context, c *Connection, in BeginInput) (*BeginOutput, error) {
	doc, err := discover(ctx, c.Issuer)
	if err != nil {
		return nil, err
	}

	u, err := url.Parse(doc.AuthorizationEndpoint)
	if err != nil {
		return nil, err
	}

	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", c.ClientID)
	q.Set("redirect_uri", in.CallbackURL)
	q.Set("scope", strings.Join(c.Scopes, " "))
	q.Set("state", in.State)
	q.Set("nonce", in.Nonce)
	q.Set("code_challenge", PKCEChallenge(in.CodeVerifier))
	q.Set("code_challenge_method", "S256")

	if in.LoginHint != "" {
		q.Set("login_hint", in.LoginHint)
	}

	u.RawQuery = q.Encode()

	return &BeginOutput{RedirectURL: u.String()}, nil
}

type tokenResponse struct {
	IDToken string `json:"id_token"`
	Error   string `json:"error"`
	ErrDesc string `json:"error_description"`
}

func (oidcProvider) Complete(ctx context.Context, c *Connection, in CompleteInput) (*Identity, error) {
	q := in.Query
	if q == nil {
		q = in.Form
	}

	if e := q.Get("error"); e != "" {
		return nil, newErr(http.StatusBadRequest, "provider_error", "The identity provider refused the sign-in ("+e+").")
	}

	code := q.Get("code")
	if code == "" {
		return nil, newErr(http.StatusBadRequest, "invalid_response", "The identity provider sent no authorization code.")
	}

	doc, err := discover(ctx, c.Issuer)
	if err != nil {
		return nil, err
	}

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {in.CallbackURL},
		"code_verifier": {in.CodeVerifier},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, doc.TokenEndpoint, nil)
	if err != nil {
		return nil, err
	}

	// client_secret_basic unless the provider says it only does _post
	if len(doc.AuthMethods) > 0 && !slices.Contains(doc.AuthMethods, "client_secret_basic") &&
		slices.Contains(doc.AuthMethods, "client_secret_post") {
		form.Set("client_id", c.ClientID)
		form.Set("client_secret", c.ClientSecret)
	} else {
		req.SetBasicAuth(url.QueryEscape(c.ClientID), url.QueryEscape(c.ClientSecret))
	}

	body := form.Encode()
	req.Body = io.NopCloser(strings.NewReader(body))
	req.ContentLength = int64(len(body))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(body)), nil }
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	res, err := idpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request failed: %w", err)
	}
	defer res.Body.Close()

	var tok tokenResponse
	if err := json.NewDecoder(io.LimitReader(res.Body, maxBody)).Decode(&tok); err != nil {
		return nil, fmt.Errorf("token response is not JSON (status %d)", res.StatusCode)
	}

	if res.StatusCode != http.StatusOK || tok.Error != "" {
		return nil, newErr(http.StatusBadGateway, "token_failed", "The identity provider did not accept the sign-in ("+tok.Error+").")
	}

	if tok.IDToken == "" || len(tok.IDToken) > maxIDTokenSize {
		return nil, newErr(http.StatusBadGateway, "invalid_response", "The identity provider sent no usable ID token.")
	}

	claims, err := verifyIDToken(ctx, doc, c, tok.IDToken, in.Nonce)
	if err != nil {
		return nil, newErr(http.StatusUnauthorized, "invalid_token", "The ID token could not be verified: "+err.Error())
	}

	return identityFromClaims(c, claims)
}

func verifyIDToken(ctx context.Context, doc *discoveryDoc, c *Connection, raw, nonce string) (map[string]any, error) {
	jws, err := jose.ParseSigned(raw, idTokenAlgs)
	if err != nil {
		return nil, errors.New("not a signed token with an accepted algorithm")
	}

	if len(jws.Signatures) != 1 {
		return nil, errors.New("expected one signature")
	}

	var keys jose.JSONWebKeySet
	if err := getJSON(ctx, doc.JWKSURI, &keys); err != nil {
		return nil, fmt.Errorf("could not load the signing keys: %w", err)
	}

	kid := jws.Signatures[0].Header.KeyID

	var candidates []jose.JSONWebKey

	if kid != "" {
		candidates = keys.Key(kid)
	} else if len(keys.Keys) == 1 {
		candidates = keys.Keys
	}

	var payload []byte

	for _, k := range candidates {
		if k.Use != "" && k.Use != "sig" {
			continue
		}

		if p, err := jws.Verify(k); err == nil {
			payload = p

			break
		}
	}

	if payload == nil {
		return nil, errors.New("the signature does not match a key of the provider")
	}

	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, errors.New("the claims are not JSON")
	}

	if iss, _ := claims["iss"].(string); NormalizeIssuer(iss) != NormalizeIssuer(doc.Issuer) {
		return nil, errors.New("wrong issuer")
	}

	aud := stringList(claims["aud"])
	if !slices.Contains(aud, c.ClientID) {
		return nil, errors.New("the token is not for this client")
	}

	if azp, _ := claims["azp"].(string); (len(aud) > 1 || azp != "") && azp != "" && azp != c.ClientID {
		return nil, errors.New("wrong authorized party")
	}

	now := time.Now()

	exp, ok := numericTime(claims["exp"])
	if !ok || now.After(exp.Add(clockSkew)) {
		return nil, errors.New("the token has expired")
	}

	if nbf, ok := numericTime(claims["nbf"]); ok && now.Add(clockSkew).Before(nbf) {
		return nil, errors.New("the token is not valid yet")
	}

	if iat, ok := numericTime(claims["iat"]); ok && now.Add(clockSkew).Before(iat) {
		return nil, errors.New("the token was issued in the future")
	}

	got, _ := claims["nonce"].(string)
	if nonce == "" || subtle.ConstantTimeCompare([]byte(got), []byte(nonce)) != 1 {
		return nil, errors.New("wrong nonce")
	}

	if sub, _ := claims["sub"].(string); sub == "" {
		return nil, errors.New("no subject")
	}

	return claims, nil
}

func identityFromClaims(c *Connection, claims map[string]any) (*Identity, error) {
	id := &Identity{Claims: claims}
	id.Subject, _ = claims["sub"].(string)
	id.Email, _ = claims["email"].(string)
	id.Email = strings.TrimSpace(id.Email)
	id.Name, _ = claims["name"].(string)

	// a provider that does not say (Entra ID) is not vouching; Entra's own
	// signal for a verified domain owner is the optional claim xms_edov
	id.EmailVerified = truthy(claims["email_verified"]) || truthy(claims["xms_edov"])

	id.Groups = stringList(claimPath(claims, c.GroupsClaim))

	return id, nil
}

// claimPath reads a top-level claim, or a dotted path into nested objects.
func claimPath(claims map[string]any, path string) any {
	if v, ok := claims[path]; ok {
		return v
	}

	var cur any = claims

	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}

		cur = m[part]
	}

	return cur
}

func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return strings.EqualFold(x, "true") || x == "1"
	case float64:
		return x == 1
	}

	return false
}

func stringList(v any) []string {
	switch x := v.(type) {
	case string:
		if x == "" {
			return nil
		}

		return []string{x}
	case []any:
		out := make([]string, 0, len(x))

		for _, e := range x {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}

		return out
	case []string:
		return x
	}

	return nil
}

func numericTime(v any) (time.Time, bool) {
	f, ok := v.(float64)
	if !ok {
		return time.Time{}, false
	}

	return time.Unix(int64(f), 0), true
}
