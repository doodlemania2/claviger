package apis_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/apis"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

func TestPasskeysPageAndClientCoverNewFlows(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)

	get := func(path string) string {
		rec := httptest.NewRecorder()
		env.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d", path, rec.Code)
		}
		return rec.Body.String()
	}

	// the page script is a file since #83
	page := get("/passkeys") + get("/passkeys/assets/passkeys-page.js")
	for _, want := range []string{
		`autocomplete="username webauthn"`, // conditional UI hook
		"autofillLogin",
		"requestReset", // recovery for passkey-only collections
		"passkeyOnly",
		"lastUsed",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("passkeys page is missing %q", want)
		}
	}

	client := get("/api/passkeys-client.js")
	for _, want := range []string{
		"login-begin-discoverable",
		"login-finish-discoverable",
		`mediation: "conditional"`,
		"step-up-begin",
		"step-up-finish",
		"passkey_step_up_required",
		"authWithPasskeyAutofill",
		"stepUpPasskey",
	} {
		if !strings.Contains(client, want) {
			t.Errorf("passkeys client is missing %q", want)
		}
	}
}

func TestPasskeysPageCSPCaptcha(t *testing.T) {
	t.Parallel()

	const original = "default-src 'self'; " +
		"script-src 'self'; " +
		"style-src 'self'; " +
		"img-src 'self' data:; " +
		"connect-src 'self'; " +
		"frame-ancestors 'self'; " +
		"base-uri 'self'; " +
		"form-action 'self'"

	if got := apis.PasskeysPageCSPForTest(core.CaptchaConfig{}); got != original {
		t.Fatalf("CSP must be unchanged without captcha, got %q", got)
	}

	// provider set but no action requires it: still unchanged
	if got := apis.PasskeysPageCSPForTest(core.CaptchaConfig{Provider: "turnstile", SiteKey: "a", SecretKey: "b"}); got != original {
		t.Fatalf("CSP must be unchanged while no action is enabled, got %q", got)
	}

	on := core.CaptchaActions{PasskeyLogin: true}

	scenarios := []struct {
		provider string
		want     []string
		notWant  []string
	}{
		{"turnstile", []string{"script-src 'self' https://challenges.cloudflare.com;", "frame-src 'self' https://challenges.cloudflare.com;"}, []string{"hcaptcha", "google"}},
		{"hcaptcha", []string{"https://*.hcaptcha.com", "frame-src 'self' https://hcaptcha.com"}, []string{"cloudflare", "google"}},
		{"recaptchaV2", []string{"https://www.google.com/recaptcha/", "https://www.gstatic.com/recaptcha/", "frame-src"}, []string{"cloudflare", "hcaptcha"}},
		{"recaptchaV3", []string{"https://www.google.com/recaptcha/"}, []string{"cloudflare", "hcaptcha"}},
	}

	for _, s := range scenarios {
		got := apis.PasskeysPageCSPForTest(core.CaptchaConfig{Provider: s.provider, SiteKey: "a", SecretKey: "b", Actions: on})
		for _, w := range s.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: CSP %q is missing %q", s.provider, got, w)
			}
		}
		for _, w := range s.notWant {
			if strings.Contains(got, w) {
				t.Errorf("%s: CSP %q must not mention %q", s.provider, got, w)
			}
		}
		if strings.Contains(got, "*.google") || strings.Contains(got, " *;") || strings.Contains(got, "default-src *") {
			t.Errorf("%s: CSP too wide: %q", s.provider, got)
		}
		if !strings.Contains(got, "frame-ancestors 'self'") {
			t.Errorf("%s: frame-ancestors lost: %q", s.provider, got)
		}
	}
}

func TestPasskeysPageServesCaptchaLoaderWithoutProviderScripts(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)

	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/passkeys", nil))

	body := rec.Body.String()

	// the page script is a file since #83 (no 'unsafe-inline'), so the loader is there
	js := httptest.NewRecorder()
	env.mux.ServeHTTP(js, httptest.NewRequest(http.MethodGet, "/passkeys/assets/passkeys-page.js", nil))

	if !strings.Contains(js.Body.String(), "getCaptchaToken") {
		t.Error("page should know how to render a captcha on demand")
	}
	// the provider scripts are injected on demand, never referenced by a <script src>
	if strings.Contains(body, `<script src="https://`) {
		t.Error("page must not load third party scripts statically")
	}
	if csp := rec.Header().Get("Content-Security-Policy"); strings.Contains(csp, "cloudflare") || strings.Contains(csp, "hcaptcha") || strings.Contains(csp, "google") {
		t.Errorf("default CSP must not mention captcha providers: %q", csp)
	}
}

func TestAdminUICSPCaptcha(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	if got := apis.AdminUICSPForTest(app); got != apis.DefaultCSPForTest {
		t.Fatalf("admin UI CSP must be upstream's by default, got %q", got)
	}

	// required for another action only: the admin login is unaffected
	app.Settings().Captcha = core.CaptchaConfig{Provider: "turnstile", SiteKey: "a", SecretKey: "b", Actions: core.CaptchaActions{SignUp: true}}
	if got := apis.AdminUICSPForTest(app); got != apis.DefaultCSPForTest {
		t.Fatalf("admin UI CSP must not change for sign-up only, got %q", got)
	}

	app.Settings().Captcha.Actions.Login = true
	got := apis.AdminUICSPForTest(app)
	for _, want := range []string{
		"script-src 'self' http://127.0.0.1:* https://challenges.cloudflare.com;",
		"frame-src 'self' https://challenges.cloudflare.com;",
		"connect-src 'self' http://127.0.0.1:* https://nominatim.openstreetmap.org https://challenges.cloudflare.com;",
		"frame-ancestors 'none'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("CSP %q is missing %q", got, want)
		}
	}
	if strings.Contains(got, "hcaptcha") || strings.Contains(got, "google") {
		t.Errorf("only the chosen provider may be allowed: %q", got)
	}
}
