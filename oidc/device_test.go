package oidc_test

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
)

// The OAuth 2.0 device authorization grant (RFC 8628, #96).

const deviceGrant = "urn:ietf:params:oauth:grant-type:device_code"

var userCodeFormat = regexp.MustCompile(`^[BCDFGHJKLMNPQRSTVWXZ]{4}-[BCDFGHJKLMNPQRSTVWXZ]{4}$`)

type deviceAuth struct {
	deviceCode, userCode, uri, complete string
	expiresIn, interval                 int
}

// deviceStart is what a CLI does first.
func (e *env) deviceStart(clientID, secret, scope string) (int, map[string]any) {
	e.t.Helper()

	form := url.Values{}
	if scope != "" {
		form.Set("scope", scope)
	}

	h := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}

	if secret == "" {
		form.Set("client_id", clientID)
	} else {
		h["Authorization"] = "Basic " + basic(clientID, secret)
	}

	rec := e.browser().do("POST", "/oidc/device_authorization", h, strings.NewReader(form.Encode()))
	out := map[string]any{}
	_ = jsonDecode(rec.Body.Bytes(), &out)

	return rec.Code, out
}

func (e *env) mustDeviceStart(clientID, secret, scope string) deviceAuth {
	e.t.Helper()

	status, out := e.deviceStart(clientID, secret, scope)
	if status != 200 {
		e.t.Fatalf("device authorization: %d %v", status, out)
	}

	d := deviceAuth{
		deviceCode: out["device_code"].(string), userCode: out["user_code"].(string),
		uri: out["verification_uri"].(string), complete: out["verification_uri_complete"].(string),
	}
	d.expiresIn = int(out["expires_in"].(float64))
	d.interval = int(out["interval"].(float64))

	return d
}

// devicePoll is one poll of the token endpoint.
func (e *env) devicePoll(clientID, secret string, d deviceAuth) (int, map[string]any) {
	e.t.Helper()

	form := url.Values{"grant_type": {deviceGrant}, "device_code": {d.deviceCode}}

	if secret == "" {
		form.Set("client_id", clientID)

		return e.tokenRequest(form, "", "")
	}

	return e.tokenRequest(form, clientID, secret)
}

// rewind makes the last poll look old, standing in for waiting out the interval.
func (e *env) rewindPolls() {
	e.t.Helper()

	if _, err := e.app.NonconcurrentDB().NewQuery("UPDATE {{" + core.CollectionNameOAuthDeviceCodes + "}} SET [[lastPoll]] = {:t}").
		Bind(dbx.Params{"t": time.Now().Add(-time.Hour).UTC().Format("2006-01-02 15:04:05.000Z")}).Execute(); err != nil {
		e.t.Fatal(err)
	}
}

// enterCode types the code on the verification page and returns the ref.
func (b *browser) enterCode(userCode string) (string, *http.Response) {
	b.env.t.Helper()

	rec := b.do("POST", "/oidc/device", map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
		strings.NewReader(url.Values{"user_code": {userCode}}.Encode()))
	res := rec.Result()

	if rec.Code != http.StatusSeeOther {
		return "", res
	}

	u, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || u.Path != "/oidc/device/verify" {
		b.env.t.Fatalf("unexpected redirect %q", rec.Header().Get("Location"))
	}

	return u.Query().Get("ref"), res
}

// deviceSignIn signs in on the verification page of the ref.
func (b *browser) deviceSignIn(ref, email, password string) {
	b.env.t.Helper()

	page := b.do("GET", "/oidc/device/verify?ref="+url.QueryEscape(ref), nil, nil)
	if page.Code != 200 || !strings.Contains(page.Body.String(), `data-page="login"`) ||
		!strings.Contains(page.Body.String(), `data-finish="/oidc/device/finish"`) {
		b.env.t.Fatalf("sign-in page: %d %s", page.Code, page.Body.String())
	}

	status, out := b.json("POST", "/api/collections/users/auth-with-password", map[string]any{"identity": email, "password": password}, nil)
	if status != 200 {
		b.env.t.Fatalf("sign-in: %d %v", status, out)
	}

	status, fin := b.json("POST", "/oidc/device/finish", map[string]any{"ref": ref}, map[string]string{"Authorization": out["token"].(string)})
	if status != 200 || fin["redirect"] != "/oidc/device/verify?ref="+url.QueryEscape(ref) {
		b.env.t.Fatalf("finish: %d %v", status, fin)
	}
}

func (b *browser) decide(ref string, allow bool) (int, map[string]any) {
	return b.json("POST", "/oidc/device/decision", map[string]any{"ref": ref, "allow": allow}, nil)
}

func deviceClient(e *env, extra map[string]any) (string, string) {
	body := map[string]any{"name": "Kitchen display", "firstParty": true, "allowDeviceFlow": true}
	for k, v := range extra {
		body[k] = v
	}

	return e.createClient(body)
}

func TestDeviceFlowEndToEnd(t *testing.T) {
	e := newEnv(t)
	clientID, _ := deviceClient(e, map[string]any{"public": true})

	d := e.mustDeviceStart(clientID, "", "openid profile email offline_access")

	if !userCodeFormat.MatchString(d.userCode) {
		t.Fatalf("user_code %q must be XXXX-XXXX of unambiguous letters", d.userCode)
	}

	if len(d.deviceCode) < 22 {
		t.Fatalf("device_code too short: %q", d.deviceCode)
	}

	if d.uri != appURL+"/oidc/device" || d.complete != d.uri+"?user_code="+d.userCode || d.expiresIn != 600 || d.interval != 5 {
		t.Fatalf("unexpected response: %+v", d)
	}

	// only hashes are stored
	var n int
	for _, v := range []string{d.deviceCode, d.userCode, strings.ReplaceAll(d.userCode, "-", "")} {
		if err := e.app.ConcurrentDB().NewQuery("SELECT COUNT(*) FROM {{" + core.CollectionNameOAuthDeviceCodes + "}} WHERE [[deviceHash]] = {:v} OR [[userCodeHash]] = {:v}").
			Bind(dbx.Params{"v": v}).Row(&n); err != nil || n != 0 {
			t.Fatalf("a raw code is stored: %q %d %v", v, n, err)
		}
	}

	if err := e.app.ConcurrentDB().NewQuery("SELECT COUNT(*) FROM {{" + core.CollectionNameOAuthDeviceCodes + "}} WHERE [[deviceHash]] = {:a} AND [[userCodeHash]] = {:b}").
		Bind(dbx.Params{"a": sha256Hex(d.deviceCode), "b": sha256Hex(strings.ReplaceAll(d.userCode, "-", ""))}).Row(&n); err != nil || n != 1 {
		t.Fatalf("the hashes must be stored: %d %v", n, err)
	}

	status, out := e.devicePoll(clientID, "", d)
	if status != 400 || out["error"] != "authorization_pending" {
		t.Fatalf("before the approval: %d %v", status, out)
	}

	// on the phone: the code from the QR link, typed in lower case without the dash
	b := e.browser()
	if page := b.do("GET", d.complete, nil, nil); page.Code != 200 || !strings.Contains(page.Body.String(), `value="`+d.userCode+`"`) {
		t.Fatalf("the verification_uri_complete must prefill the code: %d %s", page.Code, page.Body.String())
	}

	ref, _ := b.enterCode(strings.ToLower(strings.ReplaceAll(d.userCode, "-", "")))
	if ref == "" {
		t.Fatal("a valid code must be accepted however it is typed")
	}

	b.deviceSignIn(ref, testEmail, testPass)

	approve := b.do("GET", "/oidc/device/verify?ref="+url.QueryEscape(ref), nil, nil)
	for _, want := range []string{`data-page="device-approve"`, "Kitchen display", "Your email address", testEmail, "Allow"} {
		if !strings.Contains(approve.Body.String(), want) {
			t.Fatalf("the approval page must show %q:\n%s", want, approve.Body.String())
		}
	}

	// still pending until the person decides (and the poll is within its interval: wait it out)
	e.rewindPolls()

	if status, out = e.devicePoll(clientID, "", d); status != 400 || out["error"] != "authorization_pending" {
		t.Fatalf("signed in but not approved: %d %v", status, out)
	}

	if status, body := b.decide(ref, true); status != 200 {
		t.Fatalf("approve: %d %v", status, body)
	}

	if page := b.do("GET", "/oidc/device/verify?ref="+url.QueryEscape(ref), nil, nil); !strings.Contains(page.Body.String(), "Device connected") {
		t.Fatalf("the result page: %s", page.Body.String())
	}

	e.rewindPolls()

	status, tok := e.devicePoll(clientID, "", d)
	if status != 200 {
		t.Fatalf("poll after the approval: %d %v", status, tok)
	}

	for _, k := range []string{"access_token", "id_token", "refresh_token"} {
		if s, _ := tok[k].(string); s == "" {
			t.Fatalf("missing %s: %v", k, tok)
		}
	}

	// the tokens carry the real sign-in: sid, acr, amr, auth_time
	_, id := decodeJWT(t, tok["id_token"].(string))
	_, access := decodeJWT(t, tok["access_token"].(string))

	if id["sub"] != e.user().Id || id["sid"] == "" || id["sid"] == nil || id["acr"] != "urn:claviger:acr:password" ||
		id["auth_time"] == nil || id["aud"] == nil || id["collection"] != "users" {
		t.Fatalf("id token claims: %v", id)
	}

	if amr, _ := id["amr"].([]any); len(amr) == 0 || amr[0] != "pwd" {
		t.Fatalf("amr: %v", id["amr"])
	}

	if access["sid"] != id["sid"] {
		t.Fatalf("access token sid: %v vs %v", access["sid"], id["sid"])
	}

	if _, err := e.verifyJWT(tok["id_token"].(string)); err != nil {
		t.Fatalf("id token signature: %v", err)
	}

	if status, info := e.userinfo(tok["access_token"].(string)); status != 200 || info["email"] != testEmail {
		t.Fatalf("userinfo: %d %v", status, info)
	}

	// the refresh token works and is bound to the same session
	if st, ref2 := e.refresh(clientID, "", tok["refresh_token"].(string)); st != 200 {
		t.Fatalf("refresh: %d %v", st, ref2)
	} else if _, c := decodeJWT(t, ref2["id_token"].(string)); c["sid"] != id["sid"] || c["acr"] != id["acr"] {
		t.Fatalf("refreshed claims: %v", c)
	}

	// one device_code, one set of tokens
	e.rewindPolls()

	if status, out = e.devicePoll(clientID, "", d); status == 200 || out["error"] != "expired_token" {
		t.Fatalf("a used code must not yield tokens again: %d %v", status, out)
	}

	// revoking the Claviger session ends everything the device holds
	u := e.user()
	if _, err := core.RevokeSessions(e.app, u.Collection().Id, u.Id, "test", ""); err != nil {
		t.Fatal(err)
	}

	if st, _ := e.userinfo(tok["access_token"].(string)); st == 200 {
		t.Fatal("a revoked session must end the device's tokens")
	}

	// the approval was audited, with the account as actor
	recs, err := e.app.FindRecordsByFilter(core.CollectionNameAuditLog, "action = 'oidc_device_approve'", "", 10, 0)
	if err != nil || len(recs) != 1 {
		t.Fatalf("expected one approve audit entry: %d %v", len(recs), err)
	}

	if recs[0].GetString("actorId") != u.Id || recs[0].GetString("actorType") != "record" || recs[0].GetString("ip") == "" {
		t.Fatalf("the audit entry must name the account: %v", recs[0].FieldsData())
	}
}

func TestDeviceFlowConfidentialClient(t *testing.T) {
	e := newEnv(t)
	clientID, secret := deviceClient(e, nil)

	d := e.mustDeviceStart(clientID, secret, "openid")

	// polling without credentials is refused
	if status, out := e.devicePoll(clientID, "", d); status != 401 || out["error"] != "invalid_client" {
		t.Fatalf("a confidential client must authenticate: %d %v", status, out)
	}

	b := e.browser()
	ref, _ := b.enterCode(d.userCode)
	b.deviceSignIn(ref, testEmail, testPass)

	if status, body := b.decide(ref, true); status != 200 {
		t.Fatalf("approve: %d %v", status, body)
	}

	e.rewindPolls()

	if status, tok := e.devicePoll(clientID, secret, d); status != 200 || tok["access_token"] == nil {
		t.Fatalf("poll: %d %v", status, tok)
	}
}

func TestDeviceFlowSlowDown(t *testing.T) {
	e := newEnv(t)
	clientID, _ := deviceClient(e, map[string]any{"public": true})
	d := e.mustDeviceStart(clientID, "", "openid")

	if status, out := e.devicePoll(clientID, "", d); status != 400 || out["error"] != "authorization_pending" {
		t.Fatalf("first poll: %d %v", status, out)
	}

	// an immediate second poll is too fast
	status, out := e.devicePoll(clientID, "", d)
	if status != 400 || out["error"] != "slow_down" {
		t.Fatalf("polling faster than the interval: %d %v", status, out)
	}

	// and the interval grew by 5 seconds
	var iv float64
	if err := e.app.ConcurrentDB().NewQuery("SELECT [[pollInterval]] FROM {{" + core.CollectionNameOAuthDeviceCodes + "}}").Row(&iv); err != nil || iv != 10000 {
		t.Fatalf("the interval must be 10 s now: %v %v", iv, err)
	}

	// waiting a whole old interval is not enough any more; waiting long enough is
	if _, err := e.app.NonconcurrentDB().NewQuery("UPDATE {{" + core.CollectionNameOAuthDeviceCodes + "}} SET [[lastPoll]] = {:t}").
		Bind(dbx.Params{"t": time.Now().Add(-7 * time.Second).UTC().Format("2006-01-02 15:04:05.000Z")}).Execute(); err != nil {
		t.Fatal(err)
	}

	if status, out = e.devicePoll(clientID, "", d); out["error"] != "slow_down" {
		t.Fatalf("7 s is less than the new interval: %d %v", status, out)
	}

	e.rewindPolls()

	if status, out = e.devicePoll(clientID, "", d); out["error"] != "authorization_pending" {
		t.Fatalf("after waiting: %d %v", status, out)
	}
}

func TestDeviceFlowExpired(t *testing.T) {
	e := newEnv(t)
	clientID, _ := deviceClient(e, map[string]any{"public": true})
	d := e.mustDeviceStart(clientID, "", "openid")

	b := e.browser()
	ref, _ := b.enterCode(d.userCode)
	b.deviceSignIn(ref, testEmail, testPass)

	if status, _ := b.decide(ref, true); status != 200 {
		t.Fatal("approve")
	}

	// the approval came too late for the device
	if _, err := e.app.NonconcurrentDB().NewQuery("UPDATE {{" + core.CollectionNameOAuthDeviceCodes + "}} SET [[expires]] = {:t}").
		Bind(dbx.Params{"t": time.Now().Add(-time.Minute).UTC().Format("2006-01-02 15:04:05.000Z")}).Execute(); err != nil {
		t.Fatal(err)
	}

	e.rewindPolls()

	if status, out := e.devicePoll(clientID, "", d); status != 400 || out["error"] != "expired_token" {
		t.Fatalf("an expired code: %d %v", status, out)
	}

	// the verification page no longer knows it
	if _, res := e.browser().enterCode(d.userCode); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("an expired code must not be accepted: %d", res.StatusCode)
	}
}

func TestDeviceFlowDenied(t *testing.T) {
	e := newEnv(t)
	clientID, _ := deviceClient(e, map[string]any{"public": true})
	d := e.mustDeviceStart(clientID, "", "openid")

	b := e.browser()
	ref, _ := b.enterCode(d.userCode)
	b.deviceSignIn(ref, testEmail, testPass)

	if status, body := b.decide(ref, false); status != 200 {
		t.Fatalf("deny: %d %v", status, body)
	}

	e.rewindPolls()

	if status, out := e.devicePoll(clientID, "", d); status != 400 || out["error"] != "access_denied" {
		t.Fatalf("a denied request: %d %v", status, out)
	}

	// and it stays denied
	e.rewindPolls()

	if _, out := e.devicePoll(clientID, "", d); out["error"] != "access_denied" {
		t.Fatalf("still denied: %v", out)
	}

	// a decision is final
	if status, _ := b.decide(ref, true); status != http.StatusBadRequest {
		t.Fatalf("approving after denying: %d", status)
	}

	recs, err := e.app.FindRecordsByFilter(core.CollectionNameAuditLog, "action = 'oidc_device_deny'", "", 10, 0)
	if err != nil || len(recs) != 1 {
		t.Fatalf("expected one deny audit entry: %d %v", len(recs), err)
	}
}

func TestDeviceFlowNeedsTheClientFlag(t *testing.T) {
	e := newEnv(t)

	off, offSecret := e.createClient(map[string]any{"public": true, "firstParty": true})
	_ = offSecret

	status, out := e.deviceStart(off, "", "openid")
	if status != 400 || out["error"] != "unauthorized_client" {
		t.Fatalf("a client without the flag: %d %v", status, out)
	}

	if n := deviceRowCount(t, e); n != 0 {
		t.Fatalf("no row may be created, got %d", n)
	}

	// turning the flag off kills the polls of codes already issued
	on, _ := deviceClient(e, map[string]any{"public": true})
	d := e.mustDeviceStart(on, "", "openid")

	if _, err := e.app.NonconcurrentDB().NewQuery("UPDATE {{" + core.CollectionNameOAuthClients + "}} SET [[allowDeviceFlow]] = FALSE WHERE [[clientId]] = {:c}").
		Bind(dbx.Params{"c": on}).Execute(); err != nil {
		t.Fatal(err)
	}

	if status, out = e.devicePoll(on, "", d); status != 400 || out["error"] == "authorization_pending" {
		t.Fatalf("polling after the flag was turned off: %d %v", status, out)
	}

	// a device code of one client is no good for another
	other, _ := deviceClient(e, map[string]any{"public": true, "name": "Other"})

	if status, out = e.devicePoll(other, "", d); out["error"] == "authorization_pending" || status == 200 {
		t.Fatalf("another client's code: %d %v", status, out)
	}

	// an unknown device code is just denied
	if status, out = e.devicePoll(other, "", deviceAuth{deviceCode: "nope-nope-nope-nope-nn"}); status == 200 {
		t.Fatalf("unknown code: %d %v", status, out)
	}
}

func deviceRowCount(t *testing.T, e *env) int {
	t.Helper()

	var n int
	if err := e.app.ConcurrentDB().NewQuery("SELECT COUNT(*) FROM {{" + core.CollectionNameOAuthDeviceCodes + "}}").Row(&n); err != nil {
		t.Fatal(err)
	}

	return n
}

func TestDeviceCodeEntryIsRateLimited(t *testing.T) {
	e := newEnv(t)
	clientID, _ := deviceClient(e, map[string]any{"public": true})
	d := e.mustDeviceStart(clientID, "", "openid")

	attacker := e.browserAt("203.0.113.77")

	// ten wrong codes, then the door is shut, even for the right code
	for i := 0; i < 10; i++ {
		if ref, res := attacker.enterCode("BCDF-GHJK"); ref != "" || res.StatusCode != http.StatusBadRequest {
			t.Fatalf("attempt %d: %d", i, res.StatusCode)
		}
	}

	ref, res := attacker.enterCode(d.userCode)
	if ref != "" || res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") == "" {
		t.Fatalf("after ten wrong codes the right one must be refused: %d %q", res.StatusCode, ref)
	}

	// somebody else is not affected
	if ref, _ := e.browserAt("203.0.113.78").enterCode(d.userCode); ref == "" {
		t.Fatal("the budget is per IP")
	}

	// malformed codes count as wrong ones, and leak nothing about the format
	other := e.browserAt("203.0.113.79")
	for i := 0; i < 10; i++ {
		other.enterCode("123")
	}

	if _, res := other.enterCode(d.userCode); res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("malformed attempts count: %d", res.StatusCode)
	}
}

func TestDeviceAuthorizationEndpointIsLimited(t *testing.T) {
	e := newEnv(t)
	clientID, _ := deviceClient(e, map[string]any{"public": true})
	b := e.browserAt("203.0.113.90")

	passed := 0

	for i := 0; i < 40; i++ {
		rec := b.do("POST", "/oidc/device_authorization", map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
			strings.NewReader(url.Values{"client_id": {clientID}, "scope": {"openid"}}.Encode()))

		if rec.Code == http.StatusTooManyRequests {
			if !strings.Contains(rec.Body.String(), "temporarily_unavailable") || rec.Header().Get("Retry-After") == "" {
				t.Fatalf("429 body: %s", rec.Body.String())
			}

			break
		}

		passed++
	}

	if passed != 30 {
		t.Fatalf("the device_authorization budget is 30 a minute per IP, %d passed", passed)
	}
}

func TestDeviceFlowBrowserBinding(t *testing.T) {
	e := newEnv(t)
	clientID, _ := deviceClient(e, map[string]any{"public": true})
	d := e.mustDeviceStart(clientID, "", "openid")

	b := e.browser()
	ref, _ := b.enterCode(d.userCode)

	// the code is taken: a second browser cannot bind to the same authorization
	thief := e.browser()
	if ref2, res := thief.enterCode(d.userCode); ref2 != "" || res.StatusCode != http.StatusForbidden {
		t.Fatalf("a second browser: %d", res.StatusCode)
	}

	// nor use the ref without the binding cookie
	if page := thief.do("GET", "/oidc/device/verify?ref="+url.QueryEscape(ref), nil, nil); page.Code != http.StatusForbidden {
		t.Fatalf("verify from another browser: %d", page.Code)
	}

	status, out := thief.json("POST", "/api/collections/users/auth-with-password", map[string]any{"identity": testEmail, "password": testPass}, nil)
	if status != 200 {
		t.Fatal("sign-in")
	}

	if status, _ := thief.json("POST", "/oidc/device/finish", map[string]any{"ref": ref}, map[string]string{"Authorization": out["token"].(string)}); status != http.StatusForbidden {
		t.Fatalf("finish from another browser: %d", status)
	}

	// the bound browser can reload and carry on
	if ref3, _ := b.enterCode(d.userCode); ref3 != ref {
		t.Fatalf("the same browser may enter the code again: %q vs %q", ref3, ref)
	}

	// approving needs a sign-in first
	if status, _ := b.decide(ref, true); status != http.StatusBadRequest {
		t.Fatalf("deciding before signing in: %d", status)
	}

	// an API-key-like or stale token cannot finish the sign-in
	if status, _ := b.json("POST", "/oidc/device/finish", map[string]any{"ref": ref}, map[string]string{"Authorization": e.super}); status != http.StatusUnauthorized {
		t.Fatalf("a superuser token for another collection: %d", status)
	}
}

func TestDeviceFlowUsesTheBrowsersAccounts(t *testing.T) {
	e := newEnv(t)
	clientID, _ := deviceClient(e, map[string]any{"public": true})
	other := e.addUser(secondEmail)

	// the browser is signed in to two accounts already
	b := e.browser()
	b.signInAs(clientID, "users", testEmail, testPass)
	b.signInAs(clientID, "users", secondEmail, pass2)

	d := e.mustDeviceStart(clientID, "", "openid")
	ref, _ := b.enterCode(d.userCode)

	// even with accounts to pick from, approval stays a deliberate step: the chooser comes first
	page := b.do("GET", "/oidc/device/verify?ref="+url.QueryEscape(ref), nil, nil)
	if !strings.Contains(page.Body.String(), `data-page="chooser"`) || !strings.Contains(page.Body.String(), secondEmail) ||
		!strings.Contains(page.Body.String(), `data-flow="device"`) {
		t.Fatalf("the device flow must offer the signed-in accounts: %s", page.Body.String())
	}

	status, body := b.json("POST", "/oidc/account/select", map[string]any{"flow": "device", "ref": ref, "account": ssoIDOf(t, e, other.Id)}, nil)
	if status != 200 || body["redirect"] != "/oidc/device/verify?ref="+url.QueryEscape(ref) {
		t.Fatalf("select: %d %v", status, body)
	}

	approve := b.do("GET", "/oidc/device/verify?ref="+url.QueryEscape(ref), nil, nil)
	if !strings.Contains(approve.Body.String(), `data-page="device-approve"`) || !strings.Contains(approve.Body.String(), secondEmail) {
		t.Fatalf("the approval page for the chosen account: %s", approve.Body.String())
	}

	if status, _ := b.decide(ref, true); status != 200 {
		t.Fatal("approve")
	}

	e.rewindPolls()

	status, tok := e.devicePoll(clientID, "", d)
	if status != 200 {
		t.Fatalf("poll: %d %v", status, tok)
	}

	if _, id := decodeJWT(t, tok["id_token"].(string)); id["sub"] != other.Id {
		t.Fatalf("the chosen account is the subject: %v", id["sub"])
	}
}

func TestDeviceFlowRefusedForAnotherCollectionAccount(t *testing.T) {
	e := newEnv(t)

	members := core.NewAuthCollection("members")
	if err := e.app.Save(members); err != nil {
		t.Fatal(err)
	}

	clientID, _ := deviceClient(e, map[string]any{"public": true, "authCollection": "members"})
	d := e.mustDeviceStart(clientID, "", "openid")

	b := e.browser()
	ref, _ := b.enterCode(d.userCode)

	// a users account cannot approve a device of a members client
	status, out := b.json("POST", "/api/collections/users/auth-with-password", map[string]any{"identity": testEmail, "password": testPass}, nil)
	if status != 200 {
		t.Fatal("sign-in")
	}

	if status, _ := b.json("POST", "/oidc/device/finish", map[string]any{"ref": ref}, map[string]string{"Authorization": out["token"].(string)}); status != http.StatusUnauthorized {
		t.Fatalf("an account of the wrong collection: %d", status)
	}
}

func TestDeviceRowsAreHiddenFromTheRecordsAPI(t *testing.T) {
	e := newEnv(t)
	clientID, _ := deviceClient(e, map[string]any{"public": true})
	d := e.mustDeviceStart(clientID, "", "openid")

	b := e.browser()
	b.enterCode(d.userCode)

	status, out := b.json("GET", "/api/collections/"+core.CollectionNameOAuthDeviceCodes+"/records", nil, map[string]string{"Authorization": e.super})
	if status != 200 {
		t.Fatalf("list: %d %v", status, out)
	}

	raw := jsonString(out)
	for _, hidden := range []string{"deviceHash", "userCodeHash", "bindingHash", sha256Hex(d.deviceCode), sha256Hex(strings.ReplaceAll(d.userCode, "-", ""))} {
		if strings.Contains(raw, hidden) {
			t.Fatalf("%q must stay hidden: %s", hidden, raw)
		}
	}
}

func TestDeviceScopesAreFilteredToTheClientAndIncludeOpenID(t *testing.T) {
	e := newEnv(t)
	clientID, _ := deviceClient(e, map[string]any{"public": true, "scopes": []string{"openid", "email"}})

	d := e.mustDeviceStart(clientID, "", "profile email admin")

	var scopes string
	if err := e.app.ConcurrentDB().NewQuery("SELECT [[scopes]] FROM {{" + core.CollectionNameOAuthDeviceCodes + "}} WHERE [[deviceHash]] = {:h}").
		Bind(dbx.Params{"h": sha256Hex(d.deviceCode)}).Row(&scopes); err != nil {
		t.Fatal(err)
	}

	if scopes != `["openid","email"]` {
		t.Fatalf("the grant is limited to what the client may use, plus openid: %s", scopes)
	}
}

func TestNoDeviceBehaviourWithoutClients(t *testing.T) {
	e := newEnv(t)

	// nothing registered: the verification page answers, nothing is stored or counted
	rec := e.browser().do("POST", "/oidc/device", map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
		strings.NewReader("user_code=BCDF-GHJK"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code entry: %d", rec.Code)
	}

	var n int
	if err := e.app.ConcurrentDB().NewQuery("SELECT COUNT(*) FROM {{_rateLimits}}").Row(&n); err == nil && n != 0 {
		t.Fatalf("no counters may be written before a client exists, got %d", n)
	}
}
