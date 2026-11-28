package apis_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/apis"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/sso"
	"github.com/doodlemania2/claviger/tests"
	"github.com/doodlemania2/claviger/tools/hook"
	"github.com/go-jose/go-jose/v4"
)

// Fork-local (Claviger #92): the harness of the single sign-on tests: a mock
// OpenID Connect provider (httptest server that issues signed ID tokens), a
// stub DNS resolver, and a small browser that follows the redirects of the
// sign-in by hand.

const (
	ssoClientID     = "claviger-test-client"
	ssoClientSecret = "claviger-test-secret"
	ssoAppURL       = "http://app.test"
	ssoDomain       = "example.org"
)

// ---- mock identity provider -----------------------------------------------------

type mockIdP struct {
	t      testing.TB
	srv    *httptest.Server
	key    *rsa.PrivateKey
	other  *rsa.PrivateKey // signs tokens the provider's JWKS does not list
	mu     sync.Mutex
	codes  map[string]*mockCode
	nextID int
}

type mockCode struct {
	claims      map[string]any
	nonce       string
	challenge   string
	redirectURI string
	badSig      bool
}

func newMockIdP(t testing.TB) *mockIdP {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	m := &mockIdP{t: t, key: key, other: other, codes: map[string]*mockCode{}}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                m.srv.URL,
			"authorization_endpoint":                m.srv.URL + "/authorize",
			"token_endpoint":                        m.srv.URL + "/token",
			"jwks_uri":                              m.srv.URL + "/jwks",
			"token_endpoint_auth_methods_supported": []string{"client_secret_basic"},
			"code_challenge_methods_supported":      []string{"S256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}}
		_ = json.NewEncoder(w).Encode(set)
	})
	mux.HandleFunc("/token", m.token)

	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)

	restore := sso.SetHTTPClient(m.srv.Client())
	t.Cleanup(restore)

	return m
}

func (m *mockIdP) token(w http.ResponseWriter, r *http.Request) {
	fail := func(code string) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": code})
	}

	_ = r.ParseForm()

	id, secret, ok := r.BasicAuth()
	if !ok || id != ssoClientID || secret != ssoClientSecret {
		fail("invalid_client")

		return
	}

	m.mu.Lock()
	c := m.codes[r.PostForm.Get("code")]
	delete(m.codes, r.PostForm.Get("code")) // a code is used once
	m.mu.Unlock()

	if c == nil || r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("redirect_uri") != c.redirectURI {
		fail("invalid_grant")

		return
	}

	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != c.challenge {
		fail("invalid_grant")

		return
	}

	claims := map[string]any{
		"iss": m.srv.URL, "aud": ssoClientID, "exp": time.Now().Add(5 * time.Minute).Unix(),
		"iat": time.Now().Unix(), "nonce": c.nonce,
	}
	for k, v := range c.claims {
		if v == nil {
			delete(claims, k)

			continue
		}

		claims[k] = v
	}

	signKey := m.key
	if c.badSig {
		signKey = m.other
	}

	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: signKey, KeyID: "k1"}},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		fail("server_error")

		return
	}

	payload, _ := json.Marshal(claims)

	jws, err := signer.Sign(payload)
	if err != nil {
		fail("server_error")

		return
	}

	raw, _ := jws.CompactSerialize()

	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "unused", "token_type": "Bearer", "id_token": raw})
}

// approve plays the person signing in at the provider: it checks the
// authorization request the browser was sent with, records what the token will
// say, and returns the (code, state) the provider would redirect back with.
func (m *mockIdP) approve(location string, claims map[string]any, badSig bool) (code, state string) {
	m.t.Helper()

	u, err := url.Parse(location)
	if err != nil || !strings.HasPrefix(location, m.srv.URL+"/authorize") {
		m.t.Fatalf("not an authorization request to the provider: %q", location)
	}

	q := u.Query()
	if q.Get("response_type") != "code" || q.Get("client_id") != ssoClientID || q.Get("code_challenge_method") != "S256" ||
		q.Get("code_challenge") == "" || q.Get("nonce") == "" || q.Get("state") == "" ||
		!strings.Contains(q.Get("scope"), "openid") {
		m.t.Fatalf("incomplete authorization request: %v", q)
	}

	m.mu.Lock()
	m.nextID++
	code = "code-" + strings.Repeat("x", 8) + string(rune('a'+m.nextID))
	m.codes[code] = &mockCode{
		claims: claims, nonce: q.Get("nonce"), challenge: q.Get("code_challenge"),
		redirectURI: q.Get("redirect_uri"), badSig: badSig,
	}
	m.mu.Unlock()

	return code, q.Get("state")
}

// ---- DNS stub ---------------------------------------------------------------------

type stubDNS map[string][]string

func (s stubDNS) LookupTXT(_ context.Context, name string) ([]string, error) {
	if v, ok := s[name]; ok {
		return v, nil
	}

	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

// ---- the app and a browser ----------------------------------------------------------

type ssoEnv struct {
	t   testing.TB
	app *tests.TestApp
	mux http.Handler
	idp *mockIdP
	dns stubDNS
}

func newSSOEnv(t testing.TB) *ssoEnv {
	t.Helper()

	app := orgsFactory(t)
	t.Cleanup(app.Cleanup)

	app.Settings().Meta.AppURL = ssoAppURL
	orgMust(t, app.Save(app.Settings()))

	// the fixture's users collection asks for a second factor; these tests are about the first
	users, err := app.FindCollectionByNameOrId("users")
	orgMust(t, err)

	users.MFA.Enabled = false
	orgMust(t, app.Save(users))

	router, err := newTestRouter(app)
	if err != nil {
		t.Fatal(err)
	}

	env := &ssoEnv{t: t, app: app, mux: router, idp: newMockIdP(t), dns: stubDNS{}}
	t.Cleanup(sso.SetResolver(env.dns))

	return env
}

func newTestRouter(app *tests.TestApp) (http.Handler, error) {
	router, err := apis.NewRouter(app)
	if err != nil {
		return nil, err
	}

	se := new(core.ServeEvent)
	se.App = app
	se.Router = router

	var mux http.Handler

	err = app.OnServe().Trigger(se, func(e *core.ServeEvent) error {
		e.Router.Bind(&hook.Handler[*core.RequestEvent]{
			Func:     func(re *core.RequestEvent) error { return re.Next() },
			Priority: -9999, // no rate limiting
		})

		built, err := e.Router.BuildMux()
		mux = built

		return err
	})

	return mux, err
}

type ssoBrowser struct {
	env     *ssoEnv
	cookies map[string]string
}

func (e *ssoEnv) browser() *ssoBrowser { return &ssoBrowser{env: e, cookies: map[string]string{}} }

func (b *ssoBrowser) do(method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	b.env.t.Helper()

	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, ssoAppURL+target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, ssoAppURL+target, nil)
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	for k, v := range b.cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}

	rec := httptest.NewRecorder()
	b.env.mux.ServeHTTP(rec, req)

	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 || c.Value == "" {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c.Value
		}
	}

	return rec
}

// admin does a request as alice (owner of the organization P1).
func (e *ssoEnv) admin(method, target, body string) *httptest.ResponseRecorder {
	return e.browser().do(method, target, body, map[string]string{"Authorization": orgToken(e.t, "alice")})
}

func (e *ssoEnv) as(who, method, target, body string) *httptest.ResponseRecorder {
	return e.browser().do(method, target, body, map[string]string{"Authorization": orgToken(e.t, who)})
}

func (e *ssoEnv) must(rec *httptest.ResponseRecorder, status int) map[string]any {
	e.t.Helper()

	if rec.Code != status {
		e.t.Fatalf("expected %d, got %d: %s", status, rec.Code, rec.Body.String())
	}

	if rec.Body.Len() == 0 {
		return nil
	}

	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)

	return out
}

type ssoSetup struct {
	Name        string
	Enabled     bool
	SSORequired bool
	JIT         bool
	RoleMapping map[string]string
	DefaultRole string
	GroupsClaim string
	Verified    bool
}

// setupConnection creates a connection and a claim of example.org through the
// API (as alice) and, when asked, publishes the TXT record and verifies it.
func (e *ssoEnv) setupConnection(o ssoSetup) (connID, domainID string) {
	e.t.Helper()

	if o.Name == "" {
		o.Name = "Mock IdP"
	}

	body := map[string]any{
		"name": o.Name, "issuer": e.idp.srv.URL, "clientId": ssoClientID, "clientSecret": ssoClientSecret,
		"enabled": o.Enabled, "ssoRequired": o.SSORequired, "jit": o.JIT,
	}
	if o.RoleMapping != nil {
		body["roleMapping"] = o.RoleMapping
	}

	if o.DefaultRole != "" {
		body["defaultRole"] = o.DefaultRole
	}

	if o.GroupsClaim != "" {
		body["groupsClaim"] = o.GroupsClaim
	}

	raw, _ := json.Marshal(body)
	conn := e.must(e.admin(http.MethodPost, "/api/orgs/"+orgP1+"/sso/connections", string(raw)), 200)
	connID, _ = conn["id"].(string)

	dom := e.must(e.admin(http.MethodPost, "/api/orgs/"+orgP1+"/sso/domains",
		`{"domain":"`+strings.ToUpper(ssoDomain)+`","connection":"`+connID+`"}`), 200)
	domainID, _ = dom["id"].(string)

	if o.Verified {
		e.publishTXT(domainID, ssoDomain)
		got := e.must(e.admin(http.MethodPost, "/api/orgs/"+orgP1+"/sso/domains/"+domainID+"/verify", ""), 200)

		if got["verified"] != true {
			e.t.Fatalf("expected the domain to verify: %v", got)
		}
	}

	return connID, domainID
}

// publishTXT puts the claim's verification record in the stub DNS.
func (e *ssoEnv) publishTXT(domainID, name string) {
	rec, err := e.app.FindRecordById(core.CollectionNameSSODomains, domainID)
	orgMust(e.t, err)

	e.dns[name] = []string{"v=spf1 -all", core.SSOVerificationPrefix + rec.GetString("token")}
}

// signIn runs the browser through start, the provider and the callback and
// returns the page the browser is sent back to.
func (e *ssoEnv) signIn(b *ssoBrowser, connID string, claims map[string]any) *url.URL {
	e.t.Helper()

	start := b.do(http.MethodGet, "/api/sso/start?connection="+connID+"&returnTo="+url.QueryEscape("/oidc/login?request=abc"), "", nil)
	if start.Code != http.StatusFound {
		e.t.Fatalf("start: %d %s", start.Code, start.Body.String())
	}

	code, state := e.idp.approve(start.Header().Get("Location"), claims, false)

	return e.callback(b, code, state)
}

func (e *ssoEnv) callback(b *ssoBrowser, code, state string) *url.URL {
	e.t.Helper()

	cb := b.do(http.MethodGet, "/api/sso/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), "", nil)
	if cb.Code != http.StatusSeeOther {
		e.t.Fatalf("callback: %d %s", cb.Code, cb.Body.String())
	}

	u, err := url.Parse(cb.Header().Get("Location"))
	if err != nil {
		e.t.Fatal(err)
	}

	return u
}

func idClaims(sub, email string, verified bool, extra map[string]any) map[string]any {
	c := map[string]any{"sub": sub, "email": email, "email_verified": verified, "name": "Test " + sub}
	for k, v := range extra {
		c[k] = v
	}

	return c
}
