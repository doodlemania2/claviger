package apis_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/sso"
	"github.com/doodlemania2/claviger/sso/samltest"
)

// Fork-local (Claviger #92): SAML 2.0 sign-in through the real routes, with an
// in-process identity provider (sso/samltest) that signs with a generated
// certificate.

type samlEnv struct {
	*ssoEnv
	saml *samltest.IdP
}

func newSAMLEnv(t testing.TB) *samlEnv {
	t.Helper()

	return &samlEnv{ssoEnv: newSSOEnv(t), saml: samltest.New(t)}
}

type samlSetup struct {
	Enabled       bool
	Authoritative bool
	Config        map[string]any
	Roles         map[string]string
}

// setupSAML creates a SAML connection and a verified claim of example.org.
func (e *samlEnv) setupSAML(o samlSetup) string {
	e.t.Helper()

	cfg := map[string]any{"metadataXml": string(e.saml.MetadataXML()), "emailAuthoritative": o.Authoritative}
	for k, v := range o.Config {
		cfg[k] = v
	}

	body := map[string]any{"name": "SAML IdP", "type": "saml", "enabled": o.Enabled, "jit": true, "config": cfg}
	if o.Roles != nil {
		body["roleMapping"] = o.Roles
	}

	raw, _ := json.Marshal(body)
	conn := e.must(e.admin(http.MethodPost, "/api/orgs/"+orgP1+"/sso/connections", string(raw)), 200)
	connID, _ := conn["id"].(string)

	dom := e.must(e.admin(http.MethodPost, "/api/orgs/"+orgP1+"/sso/domains",
		`{"domain":"`+ssoDomain+`","connection":"`+connID+`"}`), 200)
	domID, _ := dom["id"].(string)

	e.publishTXT(domID, ssoDomain)

	if got := e.must(e.admin(http.MethodPost, "/api/orgs/"+orgP1+"/sso/domains/"+domID+"/verify", ""), 200); got["verified"] != true {
		e.t.Fatalf("the domain did not verify: %v", got)
	}

	return connID
}

// oidcOnly creates an OIDC connection without a domain claim.
func (e *samlEnv) oidcOnly() string {
	e.t.Helper()

	conn := e.must(e.admin(http.MethodPost, "/api/orgs/"+orgP1+"/sso/connections",
		`{"name":"OIDC","issuer":"`+e.idp.srv.URL+`","clientId":"`+ssoClientID+`","clientSecret":"`+ssoClientSecret+`","enabled":true}`), 200)

	id, _ := conn["id"].(string)

	return id
}

// start runs the browser to the identity provider and returns what it received.
func (e *samlEnv) start(b *ssoBrowser, connID string) (*samltest.Request, *httptest.ResponseRecorder) {
	e.t.Helper()

	rec := b.do(http.MethodGet, "/api/sso/start?connection="+connID+"&returnTo="+url.QueryEscape("/oidc/login?request=abc"), "", nil)
	if rec.Code != http.StatusFound {
		e.t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}

	req, err := e.saml.ParseRedirect(rec.Header().Get("Location"))
	if err != nil {
		e.t.Fatal(err)
	}

	return req, rec
}

// acs posts the response to the assertion consumer, as the identity provider's
// auto-submitting form does, and returns the page the browser lands on.
func (e *samlEnv) acs(b *ssoBrowser, relayState, response string) *url.URL {
	e.t.Helper()

	form := url.Values{"SAMLResponse": {response}, "RelayState": {relayState}}

	req := httptest.NewRequest(http.MethodPost, ssoAppURL+"/api/sso/callback", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	for k, v := range b.cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}

	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)

	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 || c.Value == "" {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c.Value
		}
	}

	if rec.Code != http.StatusSeeOther {
		e.t.Fatalf("callback: %d %s", rec.Code, rec.Body.String())
	}

	u, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		e.t.Fatal(err)
	}

	return u
}

func TestSAMLSignIn(t *testing.T) {
	t.Run("a new person is created and signed in", func(t *testing.T) {
		env := newSAMLEnv(t)
		connID := env.setupSAML(samlSetup{Enabled: true, Authoritative: true, Roles: map[string]string{"eng": "admin"}})

		b := env.browser()
		req, _ := env.start(b, connID)

		back := env.acs(b, req.RelayState, env.saml.Response(t, req, samltest.Options{
			Email: "sam@" + ssoDomain, Attrs: map[string][]string{"name": {"Sam Saml"}, "groups": {"eng"}},
		}))

		if back.Path != "/oidc/login" || back.Query().Get("sso_error") != "" || back.Query().Get("sso_code") == "" {
			t.Fatalf("expected a sign-in code on the return page: %s", back)
		}

		u := env.userByEmail("sam@" + ssoDomain)
		if u == nil || !u.Verified() {
			t.Fatalf("the person should exist and (the IdP being authoritative) have a verified address: %v", u)
		}

		if env.externalAuths(connID) != 1 {
			t.Fatal("the account should be linked to the connection")
		}

		member, err := env.app.FindFirstRecordByFilter(core.CollectionNameOrgMembers, "org={:o} && user={:u}",
			map[string]any{"o": orgP1, "u": u.Id})
		if err != nil || member.GetString("role") != "admin" {
			t.Fatalf("the group should map to the admin role: %v %v", err, member)
		}

		ex := b.do(http.MethodPost, "/api/sso/exchange", `{"code":"`+back.Query().Get("sso_code")+`"}`, nil)
		if ex.Code != 200 || !strings.Contains(ex.Body.String(), `"token"`) {
			t.Fatalf("exchange: %d %s", ex.Code, ex.Body.String())
		}
	})

	t.Run("refused responses sign nobody in", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			opts samltest.Options
		}{
			{"unsigned", samltest.Options{Sign: samltest.SignNone}},
			{"another certificate", samltest.Options{Sign: samltest.SignAssertionOtherKey}},
			{"wrong audience", samltest.Options{Audience: "https://other.test/sp"}},
			{"wrapped", samltest.Options{Wrap: samltest.WrapCopySignature, EvilEmail: "ceo@" + ssoDomain}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				env := newSAMLEnv(t)
				connID := env.setupSAML(samlSetup{Enabled: true, Authoritative: true})

				b := env.browser()
				req, _ := env.start(b, connID)

				tc.opts.Email = "sam@" + ssoDomain

				back := env.acs(b, req.RelayState, env.saml.Response(t, req, tc.opts))
				if back.Query().Get("sso_error") != "invalid_token" || back.Query().Get("sso_code") != "" {
					t.Fatalf("expected invalid_token: %s", back)
				}

				if env.userByEmail("sam@"+ssoDomain) != nil || env.userByEmail("ceo@"+ssoDomain) != nil {
					t.Fatal("no account may be created")
				}
			})
		}
	})

	t.Run("a state, and with it an assertion, is used once", func(t *testing.T) {
		env := newSAMLEnv(t)
		connID := env.setupSAML(samlSetup{Enabled: true, Authoritative: true})

		b := env.browser()
		req, _ := env.start(b, connID)
		resp := env.saml.Response(t, req, samltest.Options{Email: "sam@" + ssoDomain})

		if back := env.acs(b, req.RelayState, resp); back.Query().Get("sso_code") == "" {
			t.Fatalf("first use should work: %s", back)
		}

		form := url.Values{"SAMLResponse": {resp}, "RelayState": {req.RelayState}}
		r := httptest.NewRequest(http.MethodPost, ssoAppURL+"/api/sso/callback", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		w := httptest.NewRecorder()
		env.mux.ServeHTTP(w, r)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("a replayed response must be refused: %d", w.Code)
		}
	})

	t.Run("a response answering another sign-in is refused", func(t *testing.T) {
		env := newSAMLEnv(t)
		connID := env.setupSAML(samlSetup{Enabled: true, Authoritative: true})

		b1, b2 := env.browser(), env.browser()
		req1, _ := env.start(b1, connID)
		req2, _ := env.start(b2, connID)

		// a valid response for flow 1, delivered with flow 2's state
		back := env.acs(b2, req2.RelayState, env.saml.Response(t, req1, samltest.Options{Email: "sam@" + ssoDomain}))
		if back.Query().Get("sso_error") != "invalid_token" {
			t.Fatalf("InResponseTo must tie the response to the flow: %s", back)
		}
	})

	t.Run("an email the IdP is not authoritative for is not linked to an existing account", func(t *testing.T) {
		env := newSAMLEnv(t)
		connID := env.setupSAML(samlSetup{Enabled: true, Authoritative: false})
		env.seedUser("pat@"+ssoDomain, true)

		b := env.browser()
		req, _ := env.start(b, connID)

		back := env.acs(b, req.RelayState, env.saml.Response(t, req, samltest.Options{Email: "pat@" + ssoDomain}))
		if back.Query().Get("sso_error") != "email_unverified" {
			t.Fatalf("expected email_unverified: %s", back)
		}

		// with the connection marking the IdP authoritative, the same sign-in links
		env2 := newSAMLEnv(t)
		conn2 := env2.setupSAML(samlSetup{Enabled: true, Authoritative: true})
		env2.seedUser("pat@"+ssoDomain, true)

		b2 := env2.browser()
		req2, _ := env2.start(b2, conn2)

		back = env2.acs(b2, req2.RelayState, env2.saml.Response(t, req2, samltest.Options{Email: "pat@" + ssoDomain}))
		if back.Query().Get("sso_code") == "" {
			t.Fatalf("expected a sign-in: %s", back)
		}
	})

	t.Run("an email outside the verified domains is refused", func(t *testing.T) {
		env := newSAMLEnv(t)
		connID := env.setupSAML(samlSetup{Enabled: true, Authoritative: true})

		b := env.browser()
		req, _ := env.start(b, connID)

		back := env.acs(b, req.RelayState, env.saml.Response(t, req, samltest.Options{Email: "sam@other.example"}))
		if back.Query().Get("sso_error") != "domain_not_verified" {
			t.Fatalf("expected domain_not_verified: %s", back)
		}
	})
}

func TestSAMLBrowserBinding(t *testing.T) {
	cookieOf := func(rec *httptest.ResponseRecorder) *http.Cookie {
		for _, c := range rec.Result().Cookies() {
			if c.Name == "claviger_sso_bind" {
				return c
			}
		}

		return nil
	}

	t.Run("the SAML binding cookie can ride a cross-site POST, the OIDC one stays Lax", func(t *testing.T) {
		env := newSAMLEnv(t)
		samlID := env.setupSAML(samlSetup{Enabled: true})

		_, rec := env.start(env.browser(), samlID)

		c := cookieOf(rec)
		if c == nil || c.SameSite != http.SameSiteNoneMode || !c.Secure || !c.HttpOnly || c.Path != "/api/sso" {
			t.Fatalf("SAML binding cookie should be SameSite=None; Secure; HttpOnly: %+v", c)
		}

		oidcID := env.oidcOnly()

		start := env.browser().do(http.MethodGet, "/api/sso/start?connection="+oidcID+"&returnTo=/oidc/login", "", nil)

		c = cookieOf(start)
		if c == nil || c.SameSite != http.SameSiteLaxMode || !c.HttpOnly {
			t.Fatalf("the OIDC binding cookie must stay SameSite=Lax: %+v", c)
		}
	})

	t.Run("another browser cannot complete the sign-in, and the code is useless to it", func(t *testing.T) {
		env := newSAMLEnv(t)
		connID := env.setupSAML(samlSetup{Enabled: true, Authoritative: true})

		victim := env.browser()
		req, _ := env.start(victim, connID)
		resp := env.saml.Response(t, req, samltest.Options{Email: "sam@" + ssoDomain})

		// an attacker's browser presents a valid response and the state, without the binding cookie
		back := env.acs(env.browser(), req.RelayState, resp)
		if back.Query().Get("sso_error") != "other_browser" {
			t.Fatalf("expected other_browser: %s", back)
		}

		if env.userByEmail("sam@"+ssoDomain) != nil {
			t.Fatal("no account may be created")
		}
	})

	t.Run("the right browser completes it and only it can exchange the code", func(t *testing.T) {
		env := newSAMLEnv(t)
		connID := env.setupSAML(samlSetup{Enabled: true, Authoritative: true})

		b := env.browser()
		req, _ := env.start(b, connID)

		back := env.acs(b, req.RelayState, env.saml.Response(t, req, samltest.Options{Email: "sam@" + ssoDomain}))
		code := back.Query().Get("sso_code")

		if rec := env.browser().do(http.MethodPost, "/api/sso/exchange", `{"code":"`+code+`"}`, nil); rec.Code != http.StatusBadRequest {
			t.Fatalf("a code without the binding cookie must fail: %d", rec.Code)
		}
	})
}

func TestSAMLMetadataEndpoints(t *testing.T) {
	env := newSAMLEnv(t)
	connID := env.setupSAML(samlSetup{Enabled: false})

	t.Run("SP metadata names the entity id and the ACS", func(t *testing.T) {
		rec := env.browser().do(http.MethodGet, "/api/sso/"+connID+"/saml/metadata", "", nil)
		if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "samlmetadata+xml") {
			t.Fatalf("metadata: %d %s", rec.Code, rec.Header().Get("Content-Type"))
		}

		body := rec.Body.String()
		for _, want := range []string{
			`entityID="` + ssoAppURL + `/api/sso/` + connID + `/saml/metadata"`,
			`Location="` + ssoAppURL + `/api/sso/callback"`,
			"SPSSODescriptor",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("metadata lacks %q:\n%s", want, body)
			}
		}
	})

	t.Run("it is not served for an OIDC connection or an unknown one", func(t *testing.T) {
		oidcID := env.oidcOnly()

		for _, id := range []string{oidcID, "nope"} {
			if rec := env.browser().do(http.MethodGet, "/api/sso/"+id+"/saml/metadata", "", nil); rec.Code != http.StatusNotFound {
				t.Fatalf("%s: expected 404, got %d", id, rec.Code)
			}
		}
	})

	t.Run("a disabled connection cannot start a sign-in", func(t *testing.T) {
		rec := env.browser().do(http.MethodGet, "/api/sso/start?connection="+connID+"&returnTo=/x", "", nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rec.Code)
		}
	})
}

func TestSAMLConnectionSettingsAndMetadataImport(t *testing.T) {
	env := newSAMLEnv(t)

	post := func(cfg map[string]any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(map[string]any{"name": "S", "type": "saml", "config": cfg})

		return env.admin(http.MethodPost, "/api/orgs/"+orgP1+"/sso/connections", string(raw))
	}

	t.Run("pasted XML is checked when saved", func(t *testing.T) {
		if rec := post(map[string]any{"metadataXml": string(env.saml.MetadataXML())}); rec.Code != 200 {
			t.Fatalf("valid XML: %d %s", rec.Code, rec.Body.String())
		}

		if rec := post(map[string]any{"metadataXml": "<not-metadata/>"}); rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid XML must be refused: %d", rec.Code)
		}

		if rec := post(map[string]any{}); rec.Code != http.StatusBadRequest {
			t.Fatalf("a connection without an IdP must be refused: %d", rec.Code)
		}
	})

	t.Run("a metadata URL must be https", func(t *testing.T) {
		if rec := post(map[string]any{"metadataUrl": "http://idp.example.org/metadata"}); rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
	})

	t.Run("a metadata URL is fetched with the guarded client and signs people in", func(t *testing.T) {
		hits := 0
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits++

			_, _ = w.Write(env.saml.MetadataXML())
		}))
		defer srv.Close()

		// the default client would refuse this loopback server: that is the guard
		rec := post(map[string]any{"metadataUrl": srv.URL + "/metadata"})
		if rec.Code != 200 {
			t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
		}

		var conn map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &conn)
		id, _ := conn["id"].(string)

		if got := env.must(env.admin(http.MethodPost, "/api/orgs/"+orgP1+"/sso/connections/"+id+"/test", ""), 200); got["ok"] != false {
			t.Fatalf("the private network guard must refuse the loopback metadata server: %v (hits=%d)", got, hits)
		}

		defer sso.SetHTTPClient(srv.Client())()

		if got := env.must(env.admin(http.MethodPost, "/api/orgs/"+orgP1+"/sso/connections/"+id+"/test", ""), 200); got["ok"] != true {
			t.Fatalf("the test action should fetch the metadata: %v", got)
		}

		if hits != 1 {
			t.Fatalf("expected one metadata fetch, got %d", hits)
		}
	})
}
