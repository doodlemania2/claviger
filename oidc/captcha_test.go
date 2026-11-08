package oidc_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/captcha"
)

// fakeCaptcha is the provider: it accepts exactly one token value.
type fakeCaptcha struct {
	mu   sync.Mutex
	good string
	hits int
	last captcha.Request
}

func (f *fakeCaptcha) Name() string { return "turnstile" }

func (f *fakeCaptcha) Verify(_ context.Context, req captcha.Request) (captcha.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.hits++
	f.last = req

	if req.Token == f.good {
		return captcha.Result{Success: true}, nil
	}

	return captcha.Result{Reason: "invalid-input-response"}, nil
}

// the store key apis/captcha.go reads a replacement verifier from
const captchaVerifierStoreKey = "__clvCaptchaVerifier__"

func requireCaptchaFor(t *testing.T, e *env, actions core.CaptchaActions) *fakeCaptcha {
	t.Helper()

	e.app.Settings().Captcha = core.CaptchaConfig{
		Provider: core.CaptchaProviderTurnstile, SiteKey: "site-key", SecretKey: "secret", Actions: actions,
	}
	if err := e.app.Save(e.app.Settings()); err != nil {
		t.Fatal(err)
	}

	fake := &fakeCaptcha{good: "good-token"}
	e.app.Store().Set(captchaVerifierStoreKey, captcha.Verifier(fake))

	return fake
}

func TestLoginPageCSPOpensOnlyTheCaptchaProvider(t *testing.T) {
	e := newEnv(t)
	clientID, _ := e.createClient(map[string]any{"firstParty": true, "public": true})

	csp := func() string {
		b := e.browser()
		reqID, _ := b.startAuthorize(clientID, authOpts{})
		rec := b.do("GET", "/oidc/login?authRequestID="+url.QueryEscape(reqID), nil, nil)
		if rec.Code != 200 {
			t.Fatalf("login page: %d", rec.Code)
		}

		return rec.Header().Get("Content-Security-Policy")
	}

	base := csp()
	if strings.Contains(base, "cloudflare") || strings.Contains(base, "frame-src") || strings.Contains(base, "unsafe-inline") {
		t.Fatalf("no captcha required: the policy must stay strict: %s", base)
	}

	// required only for an action the hosted page never performs: unchanged
	requireCaptchaFor(t, e, core.CaptchaActions{SignUp: true})
	if got := csp(); got != base {
		t.Fatalf("signup captcha must not change the sign-in page policy:\n%s", got)
	}

	requireCaptchaFor(t, e, core.CaptchaActions{Login: true})

	got := csp()
	for _, want := range []string{
		"script-src 'self' https://challenges.cloudflare.com;",
		"connect-src 'self' https://challenges.cloudflare.com;",
		"frame-src https://challenges.cloudflare.com;",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("policy lacks %q: %s", want, got)
		}
	}

	if strings.Contains(got, "unsafe-inline") || strings.Contains(got, "default-src 'self'") || strings.Contains(got, "*") {
		t.Fatalf("the extension must stay narrow: %s", got)
	}

	// passkey login alone also opens it
	requireCaptchaFor(t, e, core.CaptchaActions{PasskeyLogin: true})
	if !strings.Contains(csp(), "challenges.cloudflare.com") {
		t.Fatal("a passkey login captcha must open the provider too")
	}

	// the consent page never gets the widget domains
	b := e.browser()
	_, _ = b.startAuthorize(clientID, authOpts{})
	rec := b.do("GET", "/oidc/logged-out", nil, nil)
	if strings.Contains(rec.Header().Get("Content-Security-Policy"), "cloudflare") {
		t.Fatal("only the sign-in page may load the widget")
	}
}

func TestLoginPageServesCaptchaClientAndAdvertisesTheWidget(t *testing.T) {
	e := newEnv(t)
	requireCaptchaFor(t, e, core.CaptchaActions{Login: true, PasskeyLogin: true})

	b := e.browser()

	js := b.do("GET", "/oidc/assets/login.js", nil, nil).Body.String()
	for _, want := range []string{"X-Captcha-Token", "getCaptchaToken", "captchaBox", `getCaptchaToken("login")`} {
		if !strings.Contains(js, want) {
			t.Fatalf("login.js lacks %q", want)
		}
	}

	// the widget configuration comes from the public auth-methods response
	status, out := b.json("GET", "/api/collections/users/auth-methods", nil, nil)
	if status != 200 {
		t.Fatalf("auth-methods: %d", status)
	}

	cap, _ := out["captcha"].(map[string]any)
	if cap == nil || cap["provider"] != "turnstile" || cap["siteKey"] != "site-key" {
		t.Fatalf("auth-methods must advertise the widget: %v", out["captcha"])
	}

	if _, leaked := cap["secretKey"]; leaked {
		t.Fatal("the secret key must never be published")
	}

	// the hosted page carries the container the widget renders into, and no inline script
	clientID, _ := e.createClient(map[string]any{"firstParty": true, "public": true})
	reqID, _ := b.startAuthorize(clientID, authOpts{})
	html := b.do("GET", "/oidc/login?authRequestID="+url.QueryEscape(reqID), nil, nil).Body.String()

	if !strings.Contains(html, `id="captchaBox"`) {
		t.Fatal("no captcha container on the sign-in page")
	}

	if strings.Contains(html, "<script>") || strings.Contains(html, " onclick=") {
		t.Fatal("the page must not use inline script")
	}
}

func TestSignInThroughTheHostedPageNeedsAValidCaptcha(t *testing.T) {
	e := newEnv(t)
	fake := requireCaptchaFor(t, e, core.CaptchaActions{Login: true})
	clientID, _ := e.createClient(map[string]any{"firstParty": true, "public": true})

	b := e.browser()
	reqID, _ := b.startAuthorize(clientID, authOpts{})
	if rec := b.do("GET", "/oidc/login?authRequestID="+url.QueryEscape(reqID), nil, nil); rec.Code != 200 {
		t.Fatalf("login page: %d", rec.Code)
	}

	creds := map[string]any{"identity": testEmail, "password": testPass}
	signIn := func(headers map[string]string) (int, map[string]any) {
		return b.json("POST", "/api/collections/users/auth-with-password", creds, headers)
	}

	// no token
	status, out := signIn(nil)
	if status != http.StatusBadRequest || !strings.Contains(jsonString(out), "captcha_failed") {
		t.Fatalf("no token must fail with captcha_failed: %d %v", status, out)
	}

	// a rejected token
	status, _ = signIn(map[string]string{"X-Captcha-Token": "forged"})
	if status != http.StatusBadRequest {
		t.Fatalf("a rejected token must fail: %d", status)
	}

	// a valid token, then the rest of the flow completes
	status, out = signIn(map[string]string{"X-Captcha-Token": "good-token"})
	if status != 200 {
		t.Fatalf("a valid token must sign in: %d %v", status, out)
	}

	if fake.last.Action != core.CaptchaActionLogin {
		t.Fatalf("the token must be checked for the login action, got %q", fake.last.Action)
	}

	status, fin := b.finish(reqID, out["token"].(string))
	if status != 200 || fin["redirect"] == nil {
		t.Fatalf("finish: %d %v", status, fin)
	}

	if u := b.resume(fin["redirect"].(string)); u.Query().Get("code") == "" {
		t.Fatalf("no code issued: %s", u)
	}
}

func TestPasskeyLoginBeginNeedsCaptchaOnTheHostedFlow(t *testing.T) {
	e := newEnv(t, envOpts{webauthn: true})
	requireCaptchaFor(t, e, core.CaptchaActions{PasskeyLogin: true})

	b := e.browser()
	body := map[string]any{"identity": testEmail}

	status, out := b.json("POST", "/api/collections/users/auth-with-webauthn/login-begin", body, nil)
	if status != http.StatusBadRequest || !strings.Contains(jsonString(out), "captcha_failed") {
		t.Fatalf("login-begin without a token: %d %v", status, out)
	}

	status, _ = b.json("POST", "/api/collections/users/auth-with-webauthn/login-begin", body, map[string]string{"X-Captcha-Token": "good-token"})
	if status != 200 {
		t.Fatalf("login-begin with a valid token: %d", status)
	}
}

func jsonString(v any) string {
	b, _ := jsonMarshal(v)

	return string(b)
}
