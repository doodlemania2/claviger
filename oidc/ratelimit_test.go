package oidc_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
)

// rateKeys are the app store keys of the in-memory counters (a "restart"
// drops them; the rows in auxiliary.db survive).
const (
	persistentStoreKey = "__clvRateLimitStore__"
	oidcLimitersKey    = "__clavigerOIDCLimiters__"
)

func restart(t *testing.T, e *env) {
	t.Helper()

	if err := e.app.OnTerminate().Trigger(&core.TerminateEvent{App: e.app}); err != nil {
		t.Fatal(err)
	}

	e.app.Store().Remove(oidcLimitersKey)
	e.app.Store().Remove(persistentStoreKey)
}

func (b *browser) tokenPost(clientID, secret, code string) *httptest.ResponseRecorder {
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirectURI}, "code_verifier": {"x"}}
	h := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}

	if secret == "" {
		form.Set("client_id", clientID)
	} else {
		h["Authorization"] = "Basic " + basic(clientID, secret)
	}

	return b.do("POST", "/oidc/oauth/token", h, strings.NewReader(form.Encode()))
}

// introspectPost calls the introspection endpoint with Basic credentials. The
// library authenticates the client first there, so a wrong secret is a 401
// (at the token endpoint a bogus code is refused before the client is looked at).
func (b *browser) introspectPost(clientID, secret string) *httptest.ResponseRecorder {
	h := map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Authorization": "Basic " + basic(clientID, secret)}

	return b.do("POST", "/oidc/oauth/introspect", h, strings.NewReader("token=abc"))
}

// hammer sends requests until one is refused with 429 and returns how many
// were accepted before it (max+2 attempts at most).
func hammer(t *testing.T, max int, do func() *httptest.ResponseRecorder) (int, *httptest.ResponseRecorder) {
	t.Helper()

	for i := 0; i < max+2; i++ {
		if rec := do(); rec.Code == http.StatusTooManyRequests {
			return i, rec
		}
	}

	t.Fatalf("no 429 after %d requests", max+2)

	return 0, nil
}

func TestTokenEndpointPerIPBudgetAndOAuthErrorFormat(t *testing.T) {
	e := newEnv(t)
	clientID, _ := e.createClient(map[string]any{"public": true, "firstParty": true})

	n, rec := hammer(t, 120, func() *httptest.ResponseRecorder { return e.browserAt("203.0.113.7").tokenPost(clientID, "", "bogus") })
	if n != 120 {
		t.Fatalf("the per-IP budget of the token endpoint is 120 a minute, %d passed", n)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the 429 must be JSON: %v: %s", err, rec.Body.String())
	}

	if body["error"] != "temporarily_unavailable" || body["error_description"] == "" {
		t.Fatalf("OAuth error body expected: %v", body)
	}

	if secs, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || secs < 1 || secs > 60 {
		t.Fatalf("Retry-After: %q", rec.Header().Get("Retry-After"))
	}

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content type %q", ct)
	}

	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("a refusal must not be cached")
	}

	// another address is not affected
	if r := e.browserAt("203.0.113.8").tokenPost(clientID, "", "bogus"); r.Code == http.StatusTooManyRequests {
		t.Fatal("the budget is per IP")
	}

	// and it is on although Settings.RateLimits was never enabled
	if e.app.Settings().RateLimits.Enabled {
		t.Fatal("the test needs the generic limiter off")
	}
}

func TestTokenEndpointPerClientBudget(t *testing.T) {
	e := newEnv(t)
	a, _ := e.createClient(map[string]any{"name": "A", "public": true, "firstParty": true})
	b2, _ := e.createClient(map[string]any{"name": "B", "public": true, "firstParty": true})

	// many addresses, each under its own budget, together spend client A's
	passed := 0

	for i := 0; i < 7 && passed < 600; i++ {
		br := e.browserAt(fmt.Sprintf("198.51.100.%d", i+1))
		for j := 0; j < 100 && passed < 600; j++ {
			if rec := br.tokenPost(a, "", "bogus"); rec.Code == http.StatusTooManyRequests {
				t.Fatalf("refused after only %d requests", passed)
			}

			passed++
		}
	}

	fresh := e.browserAt("198.51.100.99")
	if rec := fresh.tokenPost(a, "", "bogus"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("client A's budget (600/min) must be spent: %d", rec.Code)
	}

	if rec := fresh.tokenPost(b2, "", "bogus"); rec.Code == http.StatusTooManyRequests {
		t.Fatal("client B has its own budget")
	}

	// an unregistered client id cannot be used to fill anything
	if rec := fresh.tokenPost("not-a-client", "", "bogus"); rec.Code == http.StatusTooManyRequests {
		t.Fatal("unknown clients have no per-client budget")
	}
}

func TestFailedClientAuthenticationIsStricterAndCountedSeparately(t *testing.T) {
	e := newEnv(t)
	clientID, secret := e.createClient(map[string]any{"firstParty": true})
	other, otherSecret := e.createClient(map[string]any{"name": "Other", "firstParty": true})

	attacker := e.browserAt("203.0.113.50")

	// a failure at the token endpoint (real code, wrong secret) counts like the others
	login := e.browserAt("203.0.113.51").login(clientID, authOpts{})
	if status, _ := e.tokenRequestFrom(attacker, login, clientID, "wrong-secret"); status != http.StatusUnauthorized {
		t.Fatalf("a wrong secret with a valid code must be 401, got %d", status)
	}

	// 9 more wrong secrets answer 401 (invalid_client) ...
	for i := 0; i < 9; i++ {
		rec := attacker.introspectPost(clientID, "wrong-secret")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: expected 401, got %d %s", i+2, rec.Code, rec.Body.String())
		}
	}

	// ... then the endpoints refuse before looking at credentials, even the right one
	rec := attacker.introspectPost(clientID, secret)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after 10 failures the pair must be refused, got %d", rec.Code)
	}

	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("Retry-After missing")
	}

	if r := attacker.tokenPost(clientID, secret, "bogus"); r.Code != http.StatusTooManyRequests {
		t.Fatalf("the lockout covers the token endpoint too: %d", r.Code)
	}

	// the real client, from its own address, is not locked out by the attacker
	if r := e.browserAt("192.0.2.200").introspectPost(clientID, secret); r.Code != http.StatusOK {
		t.Fatalf("a stranger failing on purpose must not lock the client out: %d", r.Code)
	}

	// the same attacker address can still try another client (the key is the pair) until the per-IP budget of 30 ...
	for i := 0; i < 10; i++ {
		if r := attacker.introspectPost(other, "wrong"); r.Code != http.StatusUnauthorized {
			t.Fatalf("other client attempt %d: %d", i+1, r.Code)
		}
	}

	// ... which counts every failed authentication of the address, whatever the client
	for i := 0; i < 10; i++ {
		if r := attacker.introspectPost(fmt.Sprintf("guess-%d", i), "wrong"); r.Code != http.StatusUnauthorized {
			t.Fatalf("guess attempt %d: %d", i+1, r.Code)
		}
	}

	if r := attacker.introspectPost(other, otherSecret); r.Code != http.StatusTooManyRequests {
		t.Fatalf("30 failures from one address end all client authentication from it: %d", r.Code)
	}

	// successful authentication is not counted as a failure
	good := e.browserAt("192.0.2.201")
	for i := 0; i < 40; i++ {
		if r := good.introspectPost(clientID, secret); r.Code != http.StatusOK {
			t.Fatalf("valid credentials were counted as failures: %d", r.Code)
		}
	}
}

// tokenRequestFrom exchanges a code from the given address with a Basic secret.
func (e *env) tokenRequestFrom(b *browser, r loginResult, clientID, secret string) (int, map[string]any) {
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {r.code}, "redirect_uri": {redirectURI}, "code_verifier": {r.pkce.verifier},
	}
	h := map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Authorization": "Basic " + basic(clientID, secret)}

	rec := b.do("POST", "/oidc/oauth/token", h, strings.NewReader(form.Encode()))
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)

	return rec.Code, out
}

func TestIntrospectAndRevokeFailuresAreCounted(t *testing.T) {
	e := newEnv(t)
	clientID, secret := e.createClient(map[string]any{"firstParty": true})

	atk := e.browserAt("203.0.113.60")
	h := map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Authorization": "Basic " + basic(clientID, "nope")}

	for i := 0; i < 10; i++ {
		var path string
		if i%2 == 0 {
			path = "/oidc/oauth/introspect"
		} else {
			path = "/oidc/revoke"
		}

		if rec := atk.do("POST", path, h, strings.NewReader("token=abc")); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: expected 401, got %d", path, rec.Code)
		}
	}

	// the budget is shared by the three client-authenticated endpoints
	for _, path := range []string{"/oidc/oauth/introspect", "/oidc/revoke", "/oidc/oauth/token"} {
		good := map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Authorization": "Basic " + basic(clientID, secret)}
		if rec := atk.do("POST", path, good, strings.NewReader("token=abc")); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("%s: expected 429, got %d", path, rec.Code)
		}
	}
}

func TestEveryProtocolEndpointIsLimited(t *testing.T) {
	e := newEnv(t)
	clientID, secret := e.createClient(map[string]any{"firstParty": true})
	h := map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Authorization": "Basic " + basic(clientID, secret)}

	cases := []struct {
		name string
		max  int
		do   func(b *browser) *httptest.ResponseRecorder
	}{
		{"authorize", 120, func(b *browser) *httptest.ResponseRecorder {
			u, _ := b.authorizeURL(clientID, authOpts{})

			return b.do("GET", u, nil, nil)
		}},
		{"introspect", 600, func(b *browser) *httptest.ResponseRecorder {
			return b.do("POST", "/oidc/oauth/introspect", h, strings.NewReader("token=abc"))
		}},
		{"revoke", 60, func(b *browser) *httptest.ResponseRecorder {
			return b.do("POST", "/oidc/revoke", h, strings.NewReader("token=abc"))
		}},
		{"userinfo", 300, func(b *browser) *httptest.ResponseRecorder {
			// a different token each time, so only the per-IP budget is in play
			return b.do("GET", "/oidc/userinfo", map[string]string{"Authorization": "Bearer t-" + strconv.Itoa(len(b.cookies)+b.counter())}, nil)
		}},
	}

	for i, tc := range cases {
		b := e.browserAt(fmt.Sprintf("203.0.113.%d", 100+i))

		n, rec := hammer(t, tc.max, func() *httptest.ResponseRecorder { return tc.do(b) })
		if n != tc.max {
			t.Fatalf("%s: the per-IP budget is %d a minute, %d passed", tc.name, tc.max, n)
		}

		if tc.name == "authorize" {
			if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") || !strings.Contains(rec.Body.String(), "Too many") {
				t.Fatalf("authorize is a browser page: %s", rec.Body.String())
			}
		} else if !strings.Contains(rec.Body.String(), "temporarily_unavailable") {
			t.Fatalf("%s: OAuth error body expected: %s", tc.name, rec.Body.String())
		}

		if rec.Header().Get("Retry-After") == "" {
			t.Fatalf("%s: Retry-After missing", tc.name)
		}
	}
}

func TestUserinfoPerTokenBudget(t *testing.T) {
	e := newEnv(t)
	clientID, secret := e.createClient(map[string]any{"firstParty": true})
	res := e.browserAt("203.0.113.130").login(clientID, authOpts{})
	out := e.mustExchange(clientID, secret, res)
	access := out["access_token"].(string)

	// one token from many addresses: the per-token budget (60) holds, not the per-IP one
	passed := 0

	for i := 0; i < 61; i++ {
		b := e.browserAt(fmt.Sprintf("198.51.100.%d", 100+i%5))

		rec := b.do("GET", "/oidc/userinfo", map[string]string{"Authorization": "Bearer " + access}, nil)
		if rec.Code == http.StatusTooManyRequests {
			break
		}

		passed++
	}

	if passed != 60 {
		t.Fatalf("a token may call userinfo 60 times a minute, %d passed", passed)
	}

	// another token is not affected
	e2 := e.browserAt("198.51.100.150").login(clientID, authOpts{})
	access2 := e.mustExchange(clientID, secret, e2)["access_token"].(string)

	if status, _ := e.userinfo(access2); status != 200 {
		t.Fatalf("the second token must work: %d", status)
	}
}

func TestRateLimitsSurviveARestart(t *testing.T) {
	e := newEnv(t)
	clientID, secret := e.createClient(map[string]any{"firstParty": true})
	h := map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Authorization": "Basic " + basic(clientID, secret)}
	b := e.browserAt("203.0.113.140")

	revoke := func() *httptest.ResponseRecorder {
		return b.do("POST", "/oidc/revoke", h, strings.NewReader("token=abc"))
	}

	if n, _ := hammer(t, 60, revoke); n != 60 {
		t.Fatalf("%d passed", n)
	}

	// failed client authentication survives too
	atk := e.browserAt("203.0.113.141")
	for i := 0; i < 10; i++ {
		atk.introspectPost(clientID, "wrong")
	}

	restart(t, e)

	if rec := revoke(); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the spent budget must survive a restart, got %d", rec.Code)
	}

	if rec := atk.introspectPost(clientID, secret); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the failed-authentication lockout must survive a restart, got %d", rec.Code)
	}

	// a Settings reload does not reset them either
	if err := e.app.Save(e.app.Settings()); err != nil {
		t.Fatal(err)
	}

	if rec := revoke(); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("a settings reload must not refill the budget, got %d", rec.Code)
	}
}

func TestNoCountersBeforeAClientExists(t *testing.T) {
	e := newEnv(t)
	b := e.browserAt("203.0.113.150")

	for i := 0; i < 130; i++ {
		if rec := b.tokenPost("nobody", "", "x"); rec.Code == http.StatusTooManyRequests {
			t.Fatal("with no client registered the endpoints behave as before")
		}
	}

	if e.app.Store().Has(oidcLimitersKey) {
		t.Fatal("no limiter state may be created while there is no client")
	}
}
