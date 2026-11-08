package oidc_test

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/apis"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/oidc"
	"github.com/doodlemania2/claviger/tests"
	"github.com/doodlemania2/claviger/tools/hook"
	"github.com/go-jose/go-jose/v4"
)

const (
	appURL      = "https://example.com"
	issuerURL   = appURL + "/oidc"
	redirectURI = "https://app.example.net/callback"
	testKey     = "0123456789abcdef0123456789abcdef" // 32 chars
	testEmail   = "test@example.com"
	testPass    = "1234567890"
)

type env struct {
	t     testing.TB
	app   *tests.TestApp
	mux   http.Handler
	super string // superuser auth token
}

type envOpts struct {
	noEncryption bool
	mfa          bool
	webauthn     bool
}

func newEnv(t testing.TB, opts ...envOpts) *env {
	t.Helper()

	var o envOpts
	if len(opts) > 0 {
		o = opts[0]
	}

	if o.noEncryption {
		t.Setenv("pb_test_env", "")
	} else {
		t.Setenv("pb_test_env", testKey)
	}

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatalf("test app: %v", err)
	}
	t.Cleanup(app.Cleanup)

	app.Settings().Meta.AppURL = appURL
	if err := app.Save(app.Settings()); err != nil {
		t.Fatalf("settings: %v", err)
	}

	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}
	users.MFA.Enabled = o.mfa
	users.WebAuthn.Enabled = o.webauthn
	if err := app.Save(users); err != nil {
		t.Fatalf("users: %v", err)
	}

	oidc.Register(app)

	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}

	se := new(core.ServeEvent)
	se.App = app
	se.Router = router

	var mux http.Handler
	err = app.OnServe().Trigger(se, func(e *core.ServeEvent) error {
		// no rate limiting in tests
		e.Router.Bind(&hook.Handler[*core.RequestEvent]{
			Func:     func(re *core.RequestEvent) error { return re.Next() },
			Priority: -9999,
		})

		built, err := e.Router.BuildMux()
		mux = built

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	su, err := app.FindAuthRecordByEmail(core.CollectionNameSuperusers, testEmail)
	if err != nil {
		t.Fatalf("superuser: %v", err)
	}
	token, err := su.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}

	return &env{t: t, app: app, mux: mux, super: token}
}

// browser is a cookie jar plus request helpers.
type browser struct {
	env     *env
	cookies map[string]string
	ip      string // the client address; empty keeps the httptest default
}

func (e *env) browser() *browser { return &browser{env: e, cookies: map[string]string{}} }

func (b *browser) do(method, target string, headers map[string]string, body io.Reader) *httptest.ResponseRecorder {
	b.env.t.Helper()

	if strings.HasPrefix(target, "/") {
		target = appURL + target
	}

	req := httptest.NewRequest(method, target, body)
	if b.ip != "" {
		req.RemoteAddr = b.ip + ":4321"
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
		b.cookies[c.Name] = c.Value
	}

	return rec
}

func (b *browser) json(method, target string, body any, headers map[string]string) (int, map[string]any) {
	b.env.t.Helper()

	raw, _ := json.Marshal(body)
	h := map[string]string{"Content-Type": "application/json"}
	for k, v := range headers {
		h[k] = v
	}

	rec := b.do(method, target, h, strings.NewReader(string(raw)))
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)

	return rec.Code, out
}

// createClient registers a client through the admin API and returns the
// client id and secret.
func (e *env) createClient(body map[string]any) (string, string) {
	e.t.Helper()

	if _, ok := body["authCollection"]; !ok {
		body["authCollection"] = "users"
	}
	if _, ok := body["name"]; !ok {
		body["name"] = "Test app"
	}
	if _, ok := body["redirectUris"]; !ok {
		body["redirectUris"] = []string{redirectURI}
	}

	status, out := e.browser().json("POST", "/api/oidc/clients", body, map[string]string{"Authorization": e.super})
	if status != 200 {
		e.t.Fatalf("create client: %d %v", status, out)
	}

	secret, _ := out["clientSecret"].(string)

	return out["clientId"].(string), secret
}

type pkce struct{ verifier, challenge string }

func newPKCE() pkce {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	v := base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(v))

	return pkce{verifier: v, challenge: base64.RawURLEncoding.EncodeToString(sum[:])}
}

type authOpts struct {
	scope    string
	nonce    string
	pkce     *pkce
	noPKCE   bool
	extra    url.Values
	redirect string
}

func (b *browser) authorizeURL(clientID string, o authOpts) (string, pkce) {
	p := newPKCE()
	if o.pkce != nil {
		p = *o.pkce
	}

	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", firstNonEmpty(o.redirect, redirectURI))
	q.Set("scope", firstNonEmpty(o.scope, "openid profile email offline_access"))
	q.Set("state", "state-0123456789")
	q.Set("nonce", firstNonEmpty(o.nonce, "nonce-0123456789"))
	if !o.noPKCE {
		q.Set("code_challenge", p.challenge)
		q.Set("code_challenge_method", "S256")
	}
	for k, vs := range o.extra {
		q[k] = vs
	}

	return "/oidc/authorize?" + q.Encode(), p
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}

	return b
}

// startAuthorize calls /authorize and returns the auth request id.
func (b *browser) startAuthorize(clientID string, o authOpts) (string, pkce) {
	b.env.t.Helper()

	target, p := b.authorizeURL(clientID, o)
	rec := b.do("GET", target, nil, nil)
	loc := rec.Header().Get("Location")

	if rec.Code != http.StatusFound && rec.Code != http.StatusSeeOther {
		b.env.t.Fatalf("authorize: %d %s", rec.Code, rec.Body.String())
	}

	u, err := url.Parse(loc)
	if err != nil || u.Path != "/oidc/login" {
		b.env.t.Fatalf("authorize did not redirect to the login page: %q", loc)
	}

	return u.Query().Get("authRequestID"), p
}

// signInToken opens the login page (binding the browser) and signs in with
// the password, returning the Claviger auth token.
func (b *browser) signInToken(reqID string) string {
	b.env.t.Helper()

	if rec := b.do("GET", "/oidc/login?authRequestID="+url.QueryEscape(reqID), nil, nil); rec.Code != 200 {
		b.env.t.Fatalf("login page: %d %s", rec.Code, rec.Body.String())
	}

	status, out := b.json("POST", "/api/collections/users/auth-with-password",
		map[string]any{"identity": testEmail, "password": testPass}, nil)
	if status != 200 {
		b.env.t.Fatalf("password sign-in: %d %v", status, out)
	}

	return out["token"].(string)
}

// finish posts the login finish and returns status and body.
func (b *browser) finish(reqID, token string) (int, map[string]any) {
	return b.json("POST", "/oidc/login/finish", map[string]any{"authRequestID": reqID}, map[string]string{"Authorization": token})
}

// resume follows the redirect from finish/consent to the client redirect URI
// and returns the Location of the final redirect.
func (b *browser) resume(redirect string) *url.URL {
	b.env.t.Helper()

	rec := b.do("GET", redirect, nil, nil)
	if rec.Code != http.StatusFound {
		b.env.t.Fatalf("resume %s: %d %s", redirect, rec.Code, rec.Body.String())
	}

	u, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		b.env.t.Fatal(err)
	}

	return u
}

// loginResult is what a completed authorize flow yields.
type loginResult struct {
	code  string
	state string
	pkce  pkce
	token string
}

// login runs authorize, password sign-in and resume for a first-party client.
func (b *browser) login(clientID string, o authOpts) loginResult {
	b.env.t.Helper()

	reqID, p := b.startAuthorize(clientID, o)
	token := b.signInToken(reqID)

	status, out := b.finish(reqID, token)
	if status != 200 {
		b.env.t.Fatalf("finish: %d %v", status, out)
	}

	u := b.resume(out["redirect"].(string))
	if u.Query().Get("code") == "" {
		b.env.t.Fatalf("no code in %s", u)
	}

	return loginResult{code: u.Query().Get("code"), state: u.Query().Get("state"), pkce: p, token: token}
}

// tokenRequest posts to the token endpoint.
func (e *env) tokenRequest(form url.Values, basicUser, basicPass string) (int, map[string]any) {
	e.t.Helper()

	h := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	if basicUser != "" {
		h["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(url.QueryEscape(basicUser)+":"+url.QueryEscape(basicPass)))
	}

	rec := e.browser().do("POST", "/oidc/oauth/token", h, strings.NewReader(form.Encode()))
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)

	return rec.Code, out
}

func (e *env) exchange(clientID, secret string, r loginResult) (int, map[string]any) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {r.code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {r.pkce.verifier},
	}

	if secret == "" {
		form.Set("client_id", clientID)

		return e.tokenRequest(form, "", "")
	}

	return e.tokenRequest(form, clientID, secret)
}

func (e *env) refresh(clientID, secret, refreshToken string) (int, map[string]any) {
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}}

	if secret == "" {
		form.Set("client_id", clientID)

		return e.tokenRequest(form, "", "")
	}

	return e.tokenRequest(form, clientID, secret)
}

func (e *env) mustExchange(clientID, secret string, r loginResult) map[string]any {
	e.t.Helper()

	status, out := e.exchange(clientID, secret, r)
	if status != 200 {
		e.t.Fatalf("token exchange: %d %v", status, out)
	}

	return out
}

// ---- JWT helpers ---------------------------------------------------------

func decodeJWT(t testing.TB, token string) (map[string]any, map[string]any) {
	t.Helper()

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", token)
	}

	var header, claims map[string]any
	for i, dst := range []*map[string]any{&header, &claims} {
		raw, err := base64.RawURLEncoding.DecodeString(parts[i])
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, dst); err != nil {
			t.Fatal(err)
		}
	}

	return header, claims
}

func (e *env) jwks() jose.JSONWebKeySet {
	e.t.Helper()

	rec := e.browser().do("GET", "/oidc/keys", nil, nil)
	if rec.Code != 200 {
		e.t.Fatalf("jwks: %d %s", rec.Code, rec.Body.String())
	}

	var set jose.JSONWebKeySet
	if err := json.Unmarshal(rec.Body.Bytes(), &set); err != nil {
		e.t.Fatal(err)
	}

	return set
}

// verifyJWT checks the signature against the published JWKS and returns the claims.
func (e *env) verifyJWT(token string) (map[string]any, error) {
	e.t.Helper()

	sig, err := jose.ParseSigned(token, []jose.SignatureAlgorithm{jose.RS256, jose.ES256})
	if err != nil {
		return nil, err
	}

	kid := sig.Signatures[0].Header.KeyID
	set := e.jwks()
	keys := set.Key(kid)
	if len(keys) == 0 {
		return nil, errNoKey
	}

	payload, err := sig.Verify(keys[0].Key)
	if err != nil {
		return nil, err
	}

	claims := map[string]any{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, err
	}

	return claims, nil
}

type testError string

func (e testError) Error() string { return string(e) }

const errNoKey = testError("the key id is not in the JWKS")

func (e *env) userinfo(accessToken string) (int, map[string]any) {
	rec := e.browser().do("GET", "/oidc/userinfo", map[string]string{"Authorization": "Bearer " + accessToken}, nil)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)

	return rec.Code, out
}

func (e *env) user() *core.Record {
	rec, err := e.app.FindAuthRecordByEmail("users", testEmail)
	if err != nil {
		e.t.Fatal(err)
	}

	return rec
}

func jsonDecode(b []byte, v any) error { return json.Unmarshal(b, v) }

func basic(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(url.QueryEscape(user) + ":" + url.QueryEscape(pass)))
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// browserAt is a browser that connects from the address.
func (e *env) browserAt(ip string) *browser {
	b := e.browser()
	b.ip = ip

	return b
}

var requestCounter int

func (b *browser) counter() int { requestCounter++; return requestCounter }
