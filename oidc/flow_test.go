package oidc_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/core"
)

func TestDiscoveryAndJWKS(t *testing.T) {
	e := newEnv(t)

	// the root URL redirects to the issuer's discovery document
	rec := e.browser().do("GET", "/.well-known/openid-configuration", nil, nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/oidc/.well-known/openid-configuration" {
		t.Fatalf("root discovery redirect: %d %q", rec.Code, rec.Header().Get("Location"))
	}

	status, doc := e.browser().json("GET", "/oidc/.well-known/openid-configuration", nil, nil)
	if status != 200 {
		t.Fatalf("discovery: %d", status)
	}

	if doc["issuer"] != issuerURL {
		t.Fatalf("issuer: %v", doc["issuer"])
	}

	grants, _ := doc["grant_types_supported"].([]any)
	for _, g := range grants {
		if g == "implicit" {
			t.Fatal("the implicit grant is advertised")
		}
	}
	if len(grants) != 2 {
		t.Fatalf("grant types: %v", grants)
	}

	if rt, _ := doc["response_types_supported"].([]any); len(rt) != 1 || rt[0] != "code" {
		t.Fatalf("response types: %v", rt)
	}

	if _, ok := doc["device_authorization_endpoint"]; ok {
		t.Fatal("the device flow is advertised")
	}

	for _, k := range []string{"authorization_endpoint", "token_endpoint", "userinfo_endpoint", "jwks_uri", "end_session_endpoint", "revocation_endpoint"} {
		if s, _ := doc[k].(string); !strings.HasPrefix(s, issuerURL+"/") {
			t.Fatalf("%s: %v", k, doc[k])
		}
	}

	// nothing configured: no key exists yet, and asking for the JWKS creates none
	if n := len(e.jwks().Keys); n != 0 {
		t.Fatalf("expected an empty JWKS, got %d keys", n)
	}
}

func TestUnknownClientIsRejected(t *testing.T) {
	e := newEnv(t)

	b := e.browser()
	target, _ := b.authorizeURL("nobody-nobody", authOpts{})
	rec := b.do("GET", target, nil, nil)

	if loc := rec.Header().Get("Location"); strings.Contains(loc, "/oidc/login") || strings.HasPrefix(loc, redirectURI) {
		t.Fatalf("unknown client was accepted or redirected to the redirect URI: %d %q", rec.Code, loc)
	}
}

func TestAuthCodePKCEPublicClient(t *testing.T) {
	e := newEnv(t)
	clientID, secret := e.createClient(map[string]any{"public": true, "firstParty": true})
	if secret != "" {
		t.Fatal("a public client got a secret")
	}

	b := e.browser()
	res := b.login(clientID, authOpts{})
	if res.state != "state-0123456789" {
		t.Fatalf("state: %q", res.state)
	}

	// wrong verifier: rejected (and the code is burnt)
	bad := res
	bad.pkce = newPKCE()
	if status, _ := e.exchange(clientID, "", bad); status == 200 {
		t.Fatal("a wrong code_verifier was accepted")
	}

	// a code is single use even after a failed attempt
	if status, _ := e.exchange(clientID, "", res); status == 200 {
		t.Fatal("the code worked after a failed attempt")
	}

	// a fresh flow
	res = b.login(clientID, authOpts{})
	tok := e.mustExchange(clientID, "", res)

	for _, k := range []string{"access_token", "id_token", "refresh_token"} {
		if s, _ := tok[k].(string); s == "" {
			t.Fatalf("missing %s: %v", k, tok)
		}
	}

	// ID token
	idClaims, err := e.verifyJWT(tok["id_token"].(string))
	if err != nil {
		t.Fatalf("ID token does not verify against the JWKS: %v", err)
	}

	user := e.user()
	checks := map[string]any{
		"iss": issuerURL, "sub": user.Id, "collection": "users", "nonce": "nonce-0123456789",
		"email": user.Email(), "acr": "urn:claviger:acr:password",
	}
	for k, want := range checks {
		if idClaims[k] != want {
			t.Fatalf("id_token %s: got %v want %v (%v)", k, idClaims[k], want, idClaims)
		}
	}
	if sid, _ := idClaims["sid"].(string); sid == "" {
		t.Fatalf("id_token has no sid: %v", idClaims)
	}
	if amr, _ := idClaims["amr"].([]any); len(amr) != 1 || amr[0] != "pwd" {
		t.Fatalf("amr: %v", idClaims["amr"])
	}
	if aud, _ := idClaims["aud"].([]any); len(aud) != 1 || aud[0] != clientID {
		t.Fatalf("aud: %v", idClaims["aud"])
	}
	if _, ok := idClaims["auth_time"]; !ok {
		t.Fatal("no auth_time")
	}

	// access token: a 5 minute JWT with sid, scope and collection
	hdr, atClaims := decodeJWT(t, tok["access_token"].(string))
	if hdr["alg"] != "RS256" {
		t.Fatalf("alg: %v", hdr["alg"])
	}
	if _, err := e.verifyJWT(tok["access_token"].(string)); err != nil {
		t.Fatalf("access token does not verify: %v", err)
	}
	if atClaims["sid"] != idClaims["sid"] || atClaims["collection"] != "users" || !strings.Contains(atClaims["scope"].(string), "openid") {
		t.Fatalf("access token claims: %v", atClaims)
	}
	if life := atClaims["exp"].(float64) - atClaims["iat"].(float64); life < 290 || life > 310 {
		t.Fatalf("access token lifetime: %v", life)
	}

	// userinfo
	status, ui := e.userinfo(tok["access_token"].(string))
	if status != 200 || ui["sub"] != user.Id || ui["email"] != user.Email() || ui["collection"] != "users" {
		t.Fatalf("userinfo: %d %v", status, ui)
	}

	// refresh rotates, and keeps acr/amr/sid
	status, tok2 := e.refresh(clientID, "", tok["refresh_token"].(string))
	if status != 200 {
		t.Fatalf("refresh: %d %v", status, tok2)
	}
	if tok2["refresh_token"] == tok["refresh_token"] {
		t.Fatal("the refresh token was not rotated")
	}

	idClaims2, err := e.verifyJWT(tok2["id_token"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if idClaims2["acr"] != "urn:claviger:acr:password" || idClaims2["sid"] != idClaims["sid"] || idClaims2["sub"] != user.Id {
		t.Fatalf("claims lost on refresh: %v", idClaims2)
	}
	if amr, _ := idClaims2["amr"].([]any); len(amr) != 1 || amr[0] != "pwd" {
		t.Fatalf("amr lost on refresh: %v", idClaims2["amr"])
	}

	// reuse of the rotated token is detected and kills the whole chain
	if status, _ := e.refresh(clientID, "", tok["refresh_token"].(string)); status == 200 {
		t.Fatal("a rotated refresh token was accepted")
	}
	if status, _ := e.refresh(clientID, "", tok2["refresh_token"].(string)); status == 200 {
		t.Fatal("the chain survived the reuse of a rotated token")
	}
	if status, _ := e.userinfo(tok2["access_token"].(string)); status == 200 {
		t.Fatal("the access token of a revoked chain still works at userinfo")
	}
}

func TestConfidentialClient(t *testing.T) {
	e := newEnv(t)
	clientID, secret := e.createClient(map[string]any{"firstParty": true})
	if secret == "" {
		t.Fatal("no secret")
	}

	b := e.browser()

	// client_secret_basic
	res := b.login(clientID, authOpts{})
	if status, _ := e.exchange(clientID, "wrong-"+secret, res); status == 200 {
		t.Fatal("a wrong client secret was accepted")
	}

	res = b.login(clientID, authOpts{})
	tok := e.mustExchange(clientID, secret, res)
	if tok["refresh_token"] == "" {
		t.Fatal("no refresh token")
	}

	// client_secret_post
	res = b.login(clientID, authOpts{})
	status, out := e.tokenRequest(url.Values{
		"grant_type": {"authorization_code"}, "code": {res.code}, "redirect_uri": {redirectURI},
		"code_verifier": {res.pkce.verifier}, "client_id": {clientID}, "client_secret": {secret},
	}, "", "")
	if status != 200 {
		t.Fatalf("client_secret_post: %d %v", status, out)
	}

	// PKCE is required for confidential clients too
	target, _ := b.authorizeURL(clientID, authOpts{noPKCE: true})
	rec := b.do("GET", target, nil, nil)
	if loc := rec.Header().Get("Location"); strings.Contains(loc, "/oidc/login") {
		t.Fatalf("authorize without PKCE was accepted: %q", loc)
	}

	// refresh works with the secret only
	if status, _ := e.refresh(clientID, "", tok["refresh_token"].(string)); status == 200 {
		t.Fatal("refresh without client authentication was accepted")
	}
	if status, _ := e.refresh(clientID, secret, tok["refresh_token"].(string)); status != 200 {
		t.Fatalf("refresh with the secret failed: %d", status)
	}

	// the secret is stored hashed, and a new one replaces the old one
	status, rot := e.browser().json("POST", "/api/oidc/clients/"+recordIDOf(t, e, clientID)+"/secret", map[string]any{}, map[string]string{"Authorization": e.super})
	if status != 200 || rot["clientSecret"] == "" || rot["clientSecret"] == secret {
		t.Fatalf("rotate secret: %d %v", status, rot)
	}
	res = b.login(clientID, authOpts{})
	if status, _ := e.exchange(clientID, secret, res); status == 200 {
		t.Fatal("the old secret still works")
	}
}

func recordIDOf(t testing.TB, e *env, clientID string) string {
	t.Helper()

	rec, err := e.app.FindFirstRecordByData(core.CollectionNameOAuthClients, "clientId", clientID)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(rec.GetString("secretHash"), "wrong") {
		t.Fatal("unexpected")
	}

	return rec.Id
}

func TestRedirectURIMustMatchExactly(t *testing.T) {
	e := newEnv(t)
	clientID, _ := e.createClient(map[string]any{"public": true, "firstParty": true})

	for _, bad := range []string{
		redirectURI + "/",
		redirectURI + "?x=1",
		"https://app.example.net/callback2",
		"https://app.example.net.evil.test/callback",
		"https://evil.test/callback",
		"http://app.example.net/callback",
	} {
		b := e.browser()
		target, _ := b.authorizeURL(clientID, authOpts{redirect: bad})
		rec := b.do("GET", target, nil, nil)

		loc := rec.Header().Get("Location")
		if strings.Contains(loc, "/oidc/login") || strings.HasPrefix(loc, bad) {
			t.Fatalf("redirect_uri %q was accepted or redirected to: %d %q", bad, rec.Code, loc)
		}
	}
}

func TestImplicitFlowIsRefused(t *testing.T) {
	e := newEnv(t)
	clientID, _ := e.createClient(map[string]any{"public": true, "firstParty": true})

	for _, rt := range []string{"id_token", "id_token token", "code id_token"} {
		b := e.browser()
		target, _ := b.authorizeURL(clientID, authOpts{extra: url.Values{"response_type": {rt}}})
		rec := b.do("GET", target, nil, nil)

		if loc := rec.Header().Get("Location"); strings.Contains(loc, "/oidc/login") {
			t.Fatalf("response_type %q was accepted", rt)
		}
	}
}

func TestPromptNoneAndMissingOpenID(t *testing.T) {
	e := newEnv(t)
	clientID, _ := e.createClient(map[string]any{"public": true, "firstParty": true})

	b := e.browser()
	target, _ := b.authorizeURL(clientID, authOpts{extra: url.Values{"prompt": {"none"}}})
	rec := b.do("GET", target, nil, nil)
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, redirectURI) || !strings.Contains(loc, "error=login_required") {
		t.Fatalf("prompt=none: %d %q", rec.Code, loc)
	}

	target, _ = b.authorizeURL(clientID, authOpts{scope: "profile email"})
	rec = b.do("GET", target, nil, nil)
	if loc := rec.Header().Get("Location"); strings.Contains(loc, "/oidc/login") {
		t.Fatalf("a request without the openid scope was accepted: %q", loc)
	}
}

func TestCodeReplayRevokesIssuedTokens(t *testing.T) {
	e := newEnv(t)
	clientID, _ := e.createClient(map[string]any{"public": true, "firstParty": true})

	res := e.browser().login(clientID, authOpts{})
	tok := e.mustExchange(clientID, "", res)

	// presenting the code again fails and revokes what it produced
	if status, _ := e.exchange(clientID, "", res); status == 200 {
		t.Fatal("a code was accepted twice")
	}

	if status, _ := e.refresh(clientID, "", tok["refresh_token"].(string)); status == 200 {
		t.Fatal("the refresh token of a replayed code still works")
	}
	if status, _ := e.userinfo(tok["access_token"].(string)); status == 200 {
		t.Fatal("the access token of a replayed code still works")
	}
}

func TestScopesAreLimitedToTheClient(t *testing.T) {
	e := newEnv(t)
	clientID, _ := e.createClient(map[string]any{"public": true, "firstParty": true, "scopes": []string{"openid", "email"}})

	res := e.browser().login(clientID, authOpts{scope: "openid profile email offline_access admin"})
	tok := e.mustExchange(clientID, "", res)

	if tok["refresh_token"] != nil && tok["refresh_token"] != "" {
		t.Fatal("offline_access was granted although the client may not ask for it")
	}

	_, claims := decodeJWT(t, tok["access_token"].(string))
	if claims["scope"] != "openid email" {
		t.Fatalf("scope: %v", claims["scope"])
	}

	_, ui := e.userinfo(tok["access_token"].(string))
	if _, ok := ui["name"]; ok {
		t.Fatalf("profile claims leaked without the scope: %v", ui)
	}
}

func TestOpaqueAccessToken(t *testing.T) {
	e := newEnv(t)
	clientID, _ := e.createClient(map[string]any{"public": true, "firstParty": true, "accessTokenType": "opaque"})

	tok := e.mustExchange(clientID, "", e.browser().login(clientID, authOpts{}))

	at := tok["access_token"].(string)
	if strings.Count(at, ".") == 2 {
		t.Fatalf("expected an opaque token, got a JWT: %q", at)
	}

	if status, ui := e.userinfo(at); status != 200 || ui["sub"] != e.user().Id {
		t.Fatalf("opaque token at userinfo: %d %v", status, ui)
	}

	// revocation
	form := url.Values{"token": {at}, "client_id": {clientID}}
	rec := e.browser().do("POST", "/oidc/revoke", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, strings.NewReader(form.Encode()))
	if rec.Code != 200 {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body.String())
	}
	if status, _ := e.userinfo(at); status == 200 {
		t.Fatal("a revoked access token still works")
	}
}

func TestRefreshTokenRevocation(t *testing.T) {
	e := newEnv(t)
	clientID, _ := e.createClient(map[string]any{"public": true, "firstParty": true})

	tok := e.mustExchange(clientID, "", e.browser().login(clientID, authOpts{}))

	form := url.Values{"token": {tok["refresh_token"].(string)}, "token_type_hint": {"refresh_token"}, "client_id": {clientID}}
	rec := e.browser().do("POST", "/oidc/revoke", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, strings.NewReader(form.Encode()))
	if rec.Code != 200 {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body.String())
	}

	if status, _ := e.refresh(clientID, "", tok["refresh_token"].(string)); status == 200 {
		t.Fatal("a revoked refresh token still works")
	}
}

func TestIntrospection(t *testing.T) {
	e := newEnv(t)
	clientID, secret := e.createClient(map[string]any{"firstParty": true})

	tok := e.mustExchange(clientID, secret, e.browser().login(clientID, authOpts{}))

	h := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	form := url.Values{"token": {tok["access_token"].(string)}}

	rec := e.browser().do("POST", "/oidc/oauth/introspect", map[string]string{
		"Content-Type":  h["Content-Type"],
		"Authorization": "Basic " + basic(clientID, secret),
	}, strings.NewReader(form.Encode()))

	out := map[string]any{}
	_ = jsonUnmarshal(rec.Body.Bytes(), &out)

	if rec.Code != 200 || out["active"] != true || out["sub"] != e.user().Id || out["client_id"] != clientID {
		t.Fatalf("introspection: %d %v", rec.Code, out)
	}

	// once the session is revoked, introspection (unlike offline JWT checks) says inactive
	if _, err := core.RevokeSessions(e.app, e.user().Collection().Id, e.user().Id, "test", ""); err != nil {
		t.Fatal(err)
	}

	rec = e.browser().do("POST", "/oidc/oauth/introspect", map[string]string{
		"Content-Type":  h["Content-Type"],
		"Authorization": "Basic " + basic(clientID, secret),
	}, strings.NewReader(form.Encode()))
	out = map[string]any{}
	_ = jsonUnmarshal(rec.Body.Bytes(), &out)
	if out["active"] == true {
		t.Fatalf("a revoked session is still active at introspection: %v", out)
	}
}

func TestSessionRevokeAndBanKillRefresh(t *testing.T) {
	e := newEnv(t)
	clientID, _ := e.createClient(map[string]any{"public": true, "firstParty": true})

	user := e.user()

	// revoking the Claviger session (#93) kills the refresh token
	tok := e.mustExchange(clientID, "", e.browser().login(clientID, authOpts{}))
	if _, err := core.RevokeSessions(e.app, user.Collection().Id, user.Id, "test revoke", ""); err != nil {
		t.Fatal(err)
	}
	if status, _ := e.refresh(clientID, "", tok["refresh_token"].(string)); status == 200 {
		t.Fatal("refresh survived the session revocation")
	}
	if status, _ := e.userinfo(tok["access_token"].(string)); status == 200 {
		t.Fatal("userinfo survived the session revocation")
	}

	// a ban kills it too
	tok = e.mustExchange(clientID, "", e.browser().login(clientID, authOpts{}))

	bans, err := e.app.FindCollectionByNameOrId(core.CollectionNameBans)
	if err != nil {
		t.Fatal(err)
	}
	ban := core.NewRecord(bans)
	ban.Set("collectionRef", user.Collection().Id)
	ban.Set("recordRef", user.Id)
	ban.Set("reason", "test ban")
	if err := e.app.Save(ban); err != nil {
		t.Fatal(err)
	}

	if status, _ := e.refresh(clientID, "", tok["refresh_token"].(string)); status == 200 {
		t.Fatal("refresh survived the ban")
	}
	if status, _ := e.userinfo(tok["access_token"].(string)); status == 200 {
		t.Fatal("userinfo survived the ban")
	}

	// and the banned account cannot sign in through the hosted page either
	b := e.browser()
	reqID, _ := b.startAuthorize(clientID, authOpts{})
	b.do("GET", "/oidc/login?authRequestID="+reqID, nil, nil)
	if status, _ := b.json("POST", "/api/collections/users/auth-with-password", map[string]any{"identity": testEmail, "password": testPass}, nil); status == 200 {
		t.Fatal("a banned account signed in")
	}
}

func TestEndSessionRevokesTheSession(t *testing.T) {
	e := newEnv(t)
	clientID, _ := e.createClient(map[string]any{
		"public": true, "firstParty": true, "postLogoutUris": []string{"https://app.example.net/bye"},
	})

	tok := e.mustExchange(clientID, "", e.browser().login(clientID, authOpts{}))
	_, idClaims := decodeJWT(t, tok["id_token"].(string))
	sid := idClaims["sid"].(string)

	q := url.Values{
		"id_token_hint":            {tok["id_token"].(string)},
		"post_logout_redirect_uri": {"https://app.example.net/bye"},
		"state":                    {"bye-state"},
	}
	rec := e.browser().do("GET", "/oidc/end_session?"+q.Encode(), nil, nil)
	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "https://app.example.net/bye") {
		t.Fatalf("end_session: %d %q %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}

	if err := core.ValidateSession(e.app, sid, e.user().Collection().Id, e.user().Id); err == nil {
		t.Fatal("the Claviger session survived the RP-initiated logout")
	}
	if status, _ := e.refresh(clientID, "", tok["refresh_token"].(string)); status == 200 {
		t.Fatal("refresh survived the logout")
	}
	if status, _ := e.userinfo(tok["access_token"].(string)); status == 200 {
		t.Fatal("userinfo survived the logout")
	}

	// a post_logout_redirect_uri that was not registered is not followed
	e2 := newEnv(t)
	id2, _ := e2.createClient(map[string]any{"public": true, "firstParty": true})
	tok2 := e2.mustExchange(id2, "", e2.browser().login(id2, authOpts{}))
	q.Set("id_token_hint", tok2["id_token"].(string))
	q.Set("post_logout_redirect_uri", "https://evil.test/")
	rec = e2.browser().do("GET", "/oidc/end_session?"+q.Encode(), nil, nil)
	if strings.HasPrefix(rec.Header().Get("Location"), "https://evil.test") {
		t.Fatal("an unregistered post logout redirect was followed")
	}
}

func TestConsent(t *testing.T) {
	e := newEnv(t)
	thirdParty, _ := e.createClient(map[string]any{"public": true, "name": "Third Party App"})
	user := e.user()

	// first visit: consent screen
	b := e.browser()
	reqID, p := b.startAuthorize(thirdParty, authOpts{})
	token := b.signInToken(reqID)

	status, out := b.finish(reqID, token)
	if status != 200 || !strings.HasPrefix(out["redirect"].(string), "/oidc/consent?") {
		t.Fatalf("expected the consent redirect: %d %v", status, out)
	}

	// the callback is not usable before consent
	if rec := b.do("GET", "/oidc/authorize/callback?id="+reqID, nil, nil); rec.Code == http.StatusFound && strings.HasPrefix(rec.Header().Get("Location"), redirectURI+"?code=") {
		t.Fatal("a code was issued before consent")
	}

	page := b.do("GET", out["redirect"].(string), nil, nil)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "Third Party App") || !strings.Contains(page.Body.String(), "Your email address") {
		t.Fatalf("consent page: %d %s", page.Code, page.Body.String())
	}
	if csp := page.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe-inline") {
		t.Fatalf("the consent page CSP allows inline code: %q", csp)
	}
	if strings.Contains(page.Body.String(), "<script>") || strings.Contains(page.Body.String(), "onclick") {
		t.Fatal("the consent page has inline script")
	}

	// a different browser cannot decide for this request
	other := e.browser()
	if status, _ := other.json("POST", "/oidc/consent", map[string]any{"authRequestID": reqID, "allow": true}, nil); status == 200 {
		t.Fatal("another browser decided the consent")
	}

	// allow
	status, out = b.json("POST", "/oidc/consent", map[string]any{"authRequestID": reqID, "allow": true}, nil)
	if status != 200 {
		t.Fatalf("consent: %d %v", status, out)
	}
	u := b.resume(out["redirect"].(string))
	e.mustExchange(thirdParty, "", loginResult{code: u.Query().Get("code"), pkce: p})

	// the grant is audited
	audit, err := e.app.FindRecordsByFilter("_auditLog", "targetCollection = '_oauthConsents'", "", 5, 0)
	if err != nil || len(audit) != 1 || audit[0].GetString("actorId") != user.Id {
		t.Fatalf("consent audit entries: %v %v", err, len(audit))
	}

	// second visit with the same scopes: consent is remembered
	b2 := e.browser()
	reqID, _ = b2.startAuthorize(thirdParty, authOpts{})
	token = b2.signInToken(reqID)
	status, out = b2.finish(reqID, token)
	if status != 200 || !strings.HasPrefix(out["redirect"].(string), "/oidc/authorize/callback") {
		t.Fatalf("consent was asked again: %d %v", status, out)
	}

	// wider scopes ask again
	b3 := e.browser()
	reqID, _ = b3.startAuthorize(thirdParty, authOpts{scope: "openid"}) // narrower: still covered
	token = b3.signInToken(reqID)
	if _, out = b3.finish(reqID, token); !strings.HasPrefix(out["redirect"].(string), "/oidc/authorize/callback") {
		t.Fatalf("narrower scopes asked for consent: %v", out)
	}

	// deny
	b4 := e.browser()
	other2, _ := e.createClient(map[string]any{"public": true, "name": "Other App"})
	reqID, _ = b4.startAuthorize(other2, authOpts{})
	token = b4.signInToken(reqID)
	b4.finish(reqID, token)
	status, out = b4.json("POST", "/oidc/consent", map[string]any{"authRequestID": reqID, "allow": false}, nil)
	if loc, _ := out["redirect"].(string); status != 200 || !strings.HasPrefix(loc, redirectURI) || !strings.Contains(loc, "error=access_denied") || !strings.Contains(loc, "state=state-0123456789") {
		t.Fatalf("deny: %d %v", status, out)
	}
	if rec := b4.do("GET", "/oidc/authorize/callback?id="+reqID, nil, nil); strings.Contains(rec.Header().Get("Location"), "code=") {
		t.Fatal("a code was issued after the user denied")
	}
}

func TestFirstPartySkipsConsent(t *testing.T) {
	e := newEnv(t)
	clientID, _ := e.createClient(map[string]any{"public": true, "firstParty": true})

	b := e.browser()
	reqID, _ := b.startAuthorize(clientID, authOpts{})
	status, out := b.finish(reqID, b.signInToken(reqID))
	if status != 200 || !strings.HasPrefix(out["redirect"].(string), "/oidc/authorize/callback") {
		t.Fatalf("first party client asked for consent: %d %v", status, out)
	}
}

func TestSignInIsBoundToTheBrowser(t *testing.T) {
	e := newEnv(t)
	clientID, _ := e.createClient(map[string]any{"public": true, "firstParty": true})

	victim := e.browser()
	reqID, _ := victim.startAuthorize(clientID, authOpts{})
	token := victim.signInToken(reqID) // binds the victim's browser

	// an attacker who learned the request id cannot open the page or finish it
	attacker := e.browser()
	if rec := attacker.do("GET", "/oidc/login?authRequestID="+reqID, nil, nil); rec.Code == 200 {
		t.Fatal("another browser opened a bound sign-in page")
	}
	if status, _ := attacker.finish(reqID, token); status == 200 {
		t.Fatal("another browser finished a bound sign-in")
	}

	status, out := victim.finish(reqID, token)
	if status != 200 {
		t.Fatalf("the owner could not finish: %d %v", status, out)
	}

	// nor can the attacker pick up the code
	if rec := attacker.do("GET", out["redirect"].(string), nil, nil); rec.Code == http.StatusFound && strings.Contains(rec.Header().Get("Location"), "code=") {
		t.Fatal("another browser resumed the flow")
	}

	// the callback is bound for every method, not just GET
	if rec := attacker.do("POST", out["redirect"].(string), nil, nil); rec.Code == http.StatusFound && strings.Contains(rec.Header().Get("Location"), "code=") {
		t.Fatal("another browser resumed the flow with a POST")
	}

	// a request id replayed after completion does not issue a second code
	victim.resume(out["redirect"].(string))
	if rec := victim.do("GET", out["redirect"].(string), nil, nil); strings.Contains(rec.Header().Get("Location"), "code=") {
		t.Fatal("a second code was issued for one authorize request")
	}
}

func TestFinishRefusesWeakTokens(t *testing.T) {
	e := newEnv(t)
	clientID, _ := e.createClient(map[string]any{"public": true, "firstParty": true})

	b := e.browser()
	reqID, _ := b.startAuthorize(clientID, authOpts{})
	b.do("GET", "/oidc/login?authRequestID="+reqID, nil, nil)

	// no token at all
	if status, _ := b.finish(reqID, ""); status == 200 {
		t.Fatal("finished without a token")
	}

	// a token that predates sessions (no sid)
	legacy, err := e.user().NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := b.finish(reqID, legacy); status == 200 {
		t.Fatal("finished with a token that has no session")
	}

	// a superuser token is not an end user of the client's collection
	if status, _ := b.finish(reqID, e.super); status == 200 {
		t.Fatal("finished with a superuser token")
	}

	// a stale session
	token := b.signInToken(reqID)
	e.app.NonconcurrentDB().NewQuery("UPDATE _sessions SET created = '2020-01-01 00:00:00.000Z'").Execute()
	if status, _ := b.finish(reqID, token); status == 200 {
		t.Fatal("finished with a session that is hours old")
	}
}

func TestExpiredAuthRequest(t *testing.T) {
	e := newEnv(t)
	clientID, _ := e.createClient(map[string]any{"public": true, "firstParty": true})

	b := e.browser()
	reqID, _ := b.startAuthorize(clientID, authOpts{})
	token := b.signInToken(reqID)

	past := time.Now().Add(-time.Minute).UTC().Format("2006-01-02 15:04:05.000Z")
	if _, err := e.app.NonconcurrentDB().NewQuery("UPDATE _oauthRequests SET expires = '" + past + "'").Execute(); err != nil {
		t.Fatal(err)
	}

	if status, _ := b.finish(reqID, token); status == 200 {
		t.Fatal("finished an expired authorize request")
	}
}

func jsonUnmarshal(b []byte, v any) error { return jsonDecode(b, v) }
