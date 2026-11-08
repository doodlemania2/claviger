package oidc_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
)

const ssoCookie = "claviger_oidc_sso"

// ssoOutcome is where an authorize request ended up in a browser that may hold
// a single sign-on session.
type ssoOutcome struct {
	kind string   // "code", "error", "login" (the sign-in page), "consent"
	to   *url.URL // the final redirect (code, error) or the consent location
	req  string   // the authorize request id (login, consent)
	pkce pkce
}

func (b *browser) authorizeSilently(clientID string, o authOpts) ssoOutcome {
	b.env.t.Helper()

	target, p := b.authorizeURL(clientID, o)
	rec := b.do("GET", target, nil, nil)

	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || rec.Code != http.StatusFound {
		b.env.t.Fatalf("authorize: %d %s", rec.Code, rec.Body.String())
	}

	if loc.Path != "/oidc/login" {
		// the library answered the client directly
		return ssoOutcome{kind: kindOf(loc), to: loc, pkce: p}
	}

	reqID := loc.Query().Get("authRequestID")

	page := b.do("GET", loc.String(), nil, nil)
	if page.Code == http.StatusOK {
		return ssoOutcome{kind: "login", req: reqID, pkce: p}
	}

	next, err := url.Parse(page.Header().Get("Location"))
	if err != nil || page.Code != http.StatusFound {
		b.env.t.Fatalf("login page: %d %s", page.Code, page.Body.String())
	}

	switch {
	case next.Path == "/oidc/consent":
		return ssoOutcome{kind: "consent", to: next, req: reqID, pkce: p}
	case next.Path == "/oidc/authorize/callback":
		final := b.resume(next.String())

		return ssoOutcome{kind: kindOf(final), to: final, req: reqID, pkce: p}
	}

	// an error redirect straight to the client
	return ssoOutcome{kind: kindOf(next), to: next, req: reqID, pkce: p}
}

func kindOf(u *url.URL) string {
	if u.Query().Get("error") != "" {
		return "error"
	}

	return "code"
}

func (o ssoOutcome) errCode() string { return o.to.Query().Get("error") }

func (o ssoOutcome) result(token string) loginResult {
	return loginResult{code: o.to.Query().Get("code"), state: o.to.Query().Get("state"), pkce: o.pkce, token: token}
}

func ssoRowCount(t *testing.T, e *env) int {
	t.Helper()

	var n int
	if err := e.app.ConcurrentDB().NewQuery("SELECT COUNT(*) FROM {{" + core.CollectionNameOAuthSso + "}}").Row(&n); err != nil {
		t.Fatal(err)
	}

	return n
}

func TestSSOSecondClientAuthorizesWithoutCredentials(t *testing.T) {
	e := newEnv(t)
	a, _ := e.createClient(map[string]any{"name": "A", "public": true, "firstParty": true})
	c, _ := e.createClient(map[string]any{"name": "C", "public": true, "firstParty": true})

	b := e.browser()
	first := b.login(a, authOpts{})
	tokA := e.mustExchange(a, "", first)

	if b.cookies[ssoCookie] == "" {
		t.Fatal("a sign-in on the hosted page must start the single sign-on session")
	}

	// no credentials: the second client gets a code straight away
	out := b.authorizeSilently(c, authOpts{})
	if out.kind != "code" {
		t.Fatalf("expected a code without credentials, got %s %v", out.kind, out.to)
	}

	tokC := e.mustExchange(c, "", out.result(""))

	_, idA := decodeJWT(t, tokA["id_token"].(string))
	_, idC := decodeJWT(t, tokC["id_token"].(string))

	if idA["sid"] == "" || idA["sid"] != idC["sid"] {
		t.Fatalf("both clients must hold the same Claviger session: %v vs %v", idA["sid"], idC["sid"])
	}

	if idA["auth_time"] != idC["auth_time"] || idA["acr"] != idC["acr"] || idA["sub"] != idC["sub"] {
		t.Fatalf("the original authentication must be asserted: %v vs %v", idA, idC)
	}

	// still bound to the browser that asked: another browser cannot resume it
	other := e.browser()
	reqID, _ := b.startAuthorize(c, authOpts{})
	if rec := other.do("GET", "/oidc/login?authRequestID="+url.QueryEscape(reqID), nil, nil); rec.Code == http.StatusFound {
		t.Fatalf("another browser must not be able to use the request: %d", rec.Code)
	}
}

func TestSSOCookieIsOpaqueAndOnlyItsHashIsStored(t *testing.T) {
	e := newEnv(t)
	a, _ := e.createClient(map[string]any{"public": true, "firstParty": true})

	b := e.browser()
	res := b.login(a, authOpts{})

	// the Set-Cookie of the sign-in
	var cookie *http.Cookie

	reqID, _ := b.startAuthorize(a, authOpts{extra: url.Values{"prompt": {"login"}}})
	token := b.signInToken(reqID)

	rec := b.do("POST", "/oidc/login/finish", map[string]string{"Content-Type": "application/json", "Authorization": token},
		strings.NewReader(`{"authRequestID":"`+reqID+`"}`))

	for _, c := range rec.Result().Cookies() {
		if c.Name == ssoCookie {
			cookie = c
		}
	}

	if cookie == nil {
		t.Fatal("no single sign-on cookie")
	}

	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/oidc" || cookie.MaxAge <= 0 {
		t.Fatalf("cookie attributes: %+v", cookie)
	}

	if strings.Contains(cookie.Value, ".") || strings.Contains(cookie.Value, res.token) || strings.Contains(cookie.Value, token) {
		t.Fatalf("the cookie must hold no token: %q", cookie.Value)
	}

	if len(cookie.Value) != 43 { // 32 random bytes, base64url
		t.Fatalf("expected 32 random bytes, got %q", cookie.Value)
	}

	// the row stores the hash, never the value
	var n int
	if err := e.app.ConcurrentDB().NewQuery("SELECT COUNT(*) FROM {{" + core.CollectionNameOAuthSso + "}} WHERE [[ssoHash]] = {:v}").
		Bind(dbx.Params{"v": cookie.Value}).Row(&n); err != nil || n != 0 {
		t.Fatalf("the raw cookie value is stored: %d %v", n, err)
	}

	if err := e.app.ConcurrentDB().NewQuery("SELECT COUNT(*) FROM {{" + core.CollectionNameOAuthSso + "}} WHERE [[ssoHash]] = {:v}").
		Bind(dbx.Params{"v": sha256Hex(cookie.Value)}).Row(&n); err != nil || n != 1 {
		t.Fatalf("the hash must be stored: %d %v", n, err)
	}

	// and the records API never returns it
	status, out := e.browser().json("GET", "/api/collections/"+core.CollectionNameOAuthSso+"/records", nil, map[string]string{"Authorization": e.super})
	if status != 200 || strings.Contains(jsonString(out), "ssoHash") || strings.Contains(jsonString(out), sha256Hex(cookie.Value)) {
		t.Fatalf("the hash must stay hidden: %d %v", status, out)
	}
}

func TestSSOStillAsksForConsent(t *testing.T) {
	e := newEnv(t)
	first, _ := e.createClient(map[string]any{"name": "Mine", "public": true, "firstParty": true})
	third, _ := e.createClient(map[string]any{"name": "Third", "public": true})

	b := e.browser()
	b.login(first, authOpts{})

	out := b.authorizeSilently(third, authOpts{})
	if out.kind != "consent" {
		t.Fatalf("a client that is not first party needs consent even with SSO, got %s", out.kind)
	}

	status, body := b.json("POST", "/oidc/consent", map[string]any{"authRequestID": out.req, "allow": true}, nil)
	if status != 200 {
		t.Fatalf("consent: %d %v", status, body)
	}

	u := b.resume(body["redirect"].(string))
	if u.Query().Get("code") == "" {
		t.Fatalf("no code after consent: %s", u)
	}

	// the grant is remembered: next time no page at all
	if again := b.authorizeSilently(third, authOpts{}); again.kind != "code" {
		t.Fatalf("a remembered consent needs no page, got %s", again.kind)
	}
}

func TestSSOPromptNone(t *testing.T) {
	e := newEnv(t)
	first, _ := e.createClient(map[string]any{"name": "Mine", "public": true, "firstParty": true})
	third, _ := e.createClient(map[string]any{"name": "Third", "public": true})
	none := authOpts{extra: url.Values{"prompt": {"none"}}}

	b := e.browser()

	// no session: login_required, straight to the client
	if out := b.authorizeSilently(first, none); out.kind != "error" || out.errCode() != "login_required" {
		t.Fatalf("prompt=none without a session: %s %v", out.kind, out.to)
	}

	b.login(first, authOpts{})

	// with a session: silent success
	if out := b.authorizeSilently(first, none); out.kind != "code" {
		t.Fatalf("prompt=none with a session must succeed: %s %v", out.kind, out.to)
	}

	// a client that still needs consent cannot be satisfied without a page
	if out := b.authorizeSilently(third, none); out.kind != "error" || out.errCode() != "consent_required" {
		t.Fatalf("prompt=none needing consent: %s %v", out.kind, out.to)
	}

	// none cannot be combined
	rec := b.do("GET", mustAuthURL(b, first, authOpts{extra: url.Values{"prompt": {"none login"}}}), nil, nil)
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=invalid_request") {
		t.Fatalf("prompt=none login: %d %q", rec.Code, loc)
	}

	// the session ends: login_required again
	user := e.user()
	if _, err := core.RevokeSessions(e.app, user.Collection().Id, user.Id, "test", ""); err != nil {
		t.Fatal(err)
	}

	if out := b.authorizeSilently(first, none); out.kind != "error" || out.errCode() != "login_required" {
		t.Fatalf("prompt=none after the revocation: %s %v", out.kind, out.to)
	}
}

func mustAuthURL(b *browser, clientID string, o authOpts) string {
	u, _ := b.authorizeURL(clientID, o)

	return u
}

func TestSSOPromptLoginAndMaxAgeForceReauthentication(t *testing.T) {
	e := newEnv(t)
	a, _ := e.createClient(map[string]any{"public": true, "firstParty": true})

	b := e.browser()
	b.login(a, authOpts{})
	oldCookie := b.cookies[ssoCookie]

	if out := b.authorizeSilently(a, authOpts{}); out.kind != "code" {
		t.Fatalf("baseline: %s", out.kind)
	}

	for name, o := range map[string]authOpts{
		"prompt=login":          {extra: url.Values{"prompt": {"login"}}},
		"prompt=select_account": {extra: url.Values{"prompt": {"select_account"}}},
		"max_age=0":             {extra: url.Values{"max_age": {"0"}}},
	} {
		if out := b.authorizeSilently(a, o); out.kind != "login" {
			t.Fatalf("%s must ask for credentials, got %s", name, out.kind)
		}
	}

	// max_age compares with the real sign-in time
	if _, err := e.app.NonconcurrentDB().NewQuery("UPDATE {{" + core.CollectionNameOAuthSso + "}} SET [[authTime]] = {:t}").
		Bind(dbx.Params{"t": time.Now().Add(-2 * time.Hour).UTC().Format("2006-01-02 15:04:05.000Z")}).Execute(); err != nil {
		t.Fatal(err)
	}

	if out := b.authorizeSilently(a, authOpts{extra: url.Values{"max_age": {"3600"}}}); out.kind != "login" {
		t.Fatalf("a sign-in older than max_age must be repeated, got %s", out.kind)
	}

	if out := b.authorizeSilently(a, authOpts{extra: url.Values{"max_age": {"86400"}}}); out.kind != "code" {
		t.Fatalf("a recent enough sign-in satisfies max_age, got %s", out.kind)
	}

	// the forced sign-in replaces the session: new cookie, old row gone
	reqID, _ := b.startAuthorize(a, authOpts{extra: url.Values{"prompt": {"login"}}})
	token := b.signInToken(reqID)

	if status, out := b.finish(reqID, token); status != 200 {
		t.Fatalf("finish: %d %v", status, out)
	}

	if b.cookies[ssoCookie] == oldCookie || b.cookies[ssoCookie] == "" {
		t.Fatal("a fresh sign-in must rotate the session cookie")
	}

	if n := ssoRowCount(t, e); n != 1 {
		t.Fatalf("the old session must be dropped, %d rows", n)
	}
}

func TestSSOEndsWithTheClavigerSession(t *testing.T) {
	user := func(e *env) *core.Record { return e.user() }

	cases := map[string]func(t *testing.T, e *env){
		"session revoked": func(t *testing.T, e *env) {
			if _, err := core.RevokeSessions(e.app, user(e).Collection().Id, user(e).Id, "revoked by user", ""); err != nil {
				t.Fatal(err)
			}
		},
		"ban": func(t *testing.T, e *env) {
			bans, err := e.app.FindCollectionByNameOrId(core.CollectionNameBans)
			if err != nil {
				t.Fatal(err)
			}

			ban := core.NewRecord(bans)
			ban.Set("collectionRef", user(e).Collection().Id)
			ban.Set("recordRef", user(e).Id)
			ban.Set("reason", "test ban")

			if err := e.app.Save(ban); err != nil {
				t.Fatal(err)
			}
		},
		"password change": func(t *testing.T, e *env) {
			u := user(e)
			u.SetPassword("brand-new-password-1")

			if err := e.app.Save(u); err != nil {
				t.Fatal(err)
			}
		},
		"session expired": func(t *testing.T, e *env) {
			if _, err := e.app.NonconcurrentDB().NewQuery("UPDATE {{" + core.CollectionNameSessions + "}} SET [[expires]] = {:t}").
				Bind(dbx.Params{"t": time.Now().Add(-time.Minute).UTC().Format("2006-01-02 15:04:05.000Z")}).Execute(); err != nil {
				t.Fatal(err)
			}

			core.InvalidateSessionCache(e.app)
		},
		"account deleted": func(t *testing.T, e *env) {
			if err := e.app.Delete(user(e)); err != nil {
				t.Fatal(err)
			}
		},
	}

	for name, end := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			a, _ := e.createClient(map[string]any{"public": true, "firstParty": true})

			b := e.browser()
			b.login(a, authOpts{})

			if out := b.authorizeSilently(a, authOpts{}); out.kind != "code" {
				t.Fatalf("baseline: %s", out.kind)
			}

			end(t, e)

			if out := b.authorizeSilently(a, authOpts{}); out.kind != "login" {
				t.Fatalf("after %q the session must not carry a request, got %s", name, out.kind)
			}
		})
	}
}

func TestSSOEndSession(t *testing.T) {
	e := newEnv(t)
	a, _ := e.createClient(map[string]any{
		"public": true, "firstParty": true, "postLogoutUris": []string{"https://app.example.net/bye"},
	})
	c, _ := e.createClient(map[string]any{"name": "C", "public": true, "firstParty": true})

	b := e.browser()
	tok := e.mustExchange(a, "", b.login(a, authOpts{}))

	q := url.Values{"id_token_hint": {tok["id_token"].(string)}, "post_logout_redirect_uri": {"https://app.example.net/bye"}}

	rec := b.do("GET", "/oidc/end_session?"+q.Encode(), nil, nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("end_session: %d %s", rec.Code, rec.Body.String())
	}

	cleared := false

	for _, ck := range rec.Result().Cookies() {
		if ck.Name == ssoCookie && ck.MaxAge < 0 {
			cleared = true
		}
	}

	if !cleared {
		t.Fatal("end_session must clear the single sign-on cookie")
	}

	if n := ssoRowCount(t, e); n != 0 {
		t.Fatalf("the session row must be gone, %d left", n)
	}

	// even a browser that kept the cookie gets nothing from it
	b.cookies[ssoCookie] = "stale-value"

	if out := b.authorizeSilently(c, authOpts{}); out.kind != "login" {
		t.Fatalf("after logout the next client must ask for credentials, got %s", out.kind)
	}
}

func TestSSOEndSessionWithoutHintEndsTheBrowsersSession(t *testing.T) {
	e := newEnv(t)
	a, _ := e.createClient(map[string]any{"public": true, "firstParty": true})

	b := e.browser()
	tok := e.mustExchange(a, "", b.login(a, authOpts{}))

	rec := b.do("GET", "/oidc/end_session", nil, nil)
	if rec.Code >= 500 {
		t.Fatalf("end_session without hint: %d", rec.Code)
	}

	if out := b.authorizeSilently(a, authOpts{}); out.kind != "login" {
		t.Fatalf("the session must have ended, got %s", out.kind)
	}

	if status, _ := e.refresh(a, "", tok["refresh_token"].(string)); status == 200 {
		t.Fatal("the Claviger session behind the cookie must be revoked too")
	}
}

func TestSSOOnlyForClientsOfTheSameCollection(t *testing.T) {
	e := newEnv(t)
	a, _ := e.createClient(map[string]any{"public": true, "firstParty": true})

	other := core.NewAuthCollection("members")
	if err := e.app.Save(other); err != nil {
		t.Fatal(err)
	}

	m, _ := e.createClient(map[string]any{"name": "M", "authCollection": "members", "public": true, "firstParty": true})

	b := e.browser()
	b.login(a, authOpts{})

	if out := b.authorizeSilently(m, authOpts{}); out.kind != "login" {
		t.Fatalf("a session of one collection must not sign in to a client of another, got %s", out.kind)
	}
}
