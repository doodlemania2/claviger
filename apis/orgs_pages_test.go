package apis_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/doodlemania2/claviger/apis"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// Fork-local (Claviger #79b): the hosted organization pages.

var (
	inlineScriptRe  = regexp.MustCompile(`(?is)<script(?:\s[^>]*)?>`)
	inlineHandlerRe = regexp.MustCompile(`(?i)\son[a-z]+\s*=`)
	idRe            = regexp.MustCompile(`id="([A-Za-z0-9_-]+)"`)
	jsIDRe          = regexp.MustCompile(`(?:querySelector\("#|getElementById\(")([A-Za-z0-9_-]+)"`)
)

// strictPage checks what makes the CSP meaningful: no inline script, no inline
// handler, no style attribute or element.
func strictPage(t testing.TB, res *http.Response, body string) {
	t.Helper()

	csp := res.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "style-src 'self'", "frame-ancestors 'none'", "base-uri 'none'"} {
		if !strings.Contains(csp, want) {
			t.Fatalf("CSP %q misses %q", csp, want)
		}
	}

	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Fatalf("CSP must be strict: %q", csp)
	}

	for _, m := range inlineScriptRe.FindAllString(body, -1) {
		if !strings.Contains(m, " src=") {
			t.Fatalf("inline script %q", m)
		}
	}

	if inlineHandlerRe.MatchString(body) || strings.Contains(body, "style=") || strings.Contains(body, "<style") {
		t.Fatal("inline handlers and styles are not allowed")
	}

	if res.Header.Get("Cache-Control") != "no-store" || res.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("unexpected cache headers %v", res.Header)
	}
}

func pageBody(t testing.TB, res *http.Response) string {
	t.Helper()

	b, err := io.ReadAll(res.Body)
	orgMust(t, err)

	return string(b)
}

func TestInvitationPage(t *testing.T) {
	t.Parallel()

	page := func(name, token string, status int, has []string, check func(t testing.TB, app *tests.TestApp, res *http.Response)) tests.ApiScenario {
		return tests.ApiScenario{
			Name: name, Method: http.MethodGet, URL: "/orgs/invite/" + token, TestAppFactory: invFactory,
			ExpectedStatus: status, ExpectedContent: has,
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
				if check != nil {
					check(t, app, res)
				}
			},
		}
	}

	for _, sc := range []tests.ApiScenario{
		page("shows the org, role and email, and offers sign-in for an existing account", invTokenFrank, 200,
			[]string{"Join Parish One", "<strong>admin</strong>", "frank@orgs.test", `data-has-account="true"`, `id="pw"`, `src="/orgs/assets/invite.js"`},
			nil),
		page("offers sign-up when there is no account", invTokenNewbie, 200,
			[]string{"Join Parish One", `data-has-account="false"`, `id="toSignUp"`, `id="signupForm"`}, nil),
		page("expired", invTokenExpired, 404, []string{"invalid or has expired"}, nil),
		page("revoked", invTokenRevoked, 404, []string{"invalid or has expired"}, nil),
		page("used", invTokenUsed, 404, []string{"invalid or has expired"}, nil),
		page("unknown", "nope", 404, []string{"invalid or has expired"}, nil),
	} {
		sc := sc
		check := sc.AfterTestFunc
		sc.AfterTestFunc = func(t testing.TB, app *tests.TestApp, res *http.Response) {
			// the recorder's body is already consumed by the scenario: render again
			strictHeaders(t, res)
			check(t, app, res)
		}
		sc.Test(t)
	}
}

// strictHeaders is strictPage without the body checks (the scenario has read it).
func strictHeaders(t testing.TB, res *http.Response) {
	t.Helper()

	csp := res.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe-") {
		t.Fatalf("unexpected CSP %q", csp)
	}

	if res.Header.Get("Cache-Control") != "no-store" || res.Header.Get("Referrer-Policy") != "no-referrer" ||
		res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("unexpected headers %v", res.Header)
	}
}

func TestInvitationPageIsStrictAndEscaped(t *testing.T) {
	t.Parallel()

	app := invFactory(t)
	defer app.Cleanup()

	org, _ := app.FindRecordById(core.CollectionNameOrgs, orgP1)
	org.Set("name", `"><script>alert(1)</script>`)
	orgMust(t, app.Save(org))

	for _, url := range []string{"/orgs/invite/" + invTokenFrank, "/orgs/invite/" + invTokenNewbie, "/orgs/parish-1"} {
		body, res := servePage(t, app, url)

		strictPage(t, res, body)

		if strings.Contains(body, "<script>alert(1)") {
			t.Fatalf("%s: the org name must be escaped", url)
		}
	}
}

var pageMuxes sync.Map // *tests.TestApp -> http.Handler

// servePage serves a GET against the app's full router (OnServe routes
// included). The mux is built once per app: every NewRouter call binds the
// serve hooks again.
func servePage(t testing.TB, app *tests.TestApp, url string) (string, *http.Response) {
	t.Helper()

	mux, ok := pageMuxes.Load(app)
	if !ok {
		router, err := apis.NewRouter(app)
		orgMust(t, err)

		se := &core.ServeEvent{App: app, Router: router}

		var built http.Handler

		orgMust(t, app.OnServe().Trigger(se, func(e *core.ServeEvent) error {
			m, err := e.Router.BuildMux()
			built = m

			return err
		}))

		mux = built
		pageMuxes.Store(app, built)
	}

	rec := httptest.NewRecorder()
	mux.(http.Handler).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))

	res := rec.Result()

	return pageBody(t, res), res
}

func TestOrgAdminPage(t *testing.T) {
	t.Parallel()

	app := invFactory(t)
	defer app.Cleanup()

	body, res := servePage(t, app, "/orgs/parish-1")
	if res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}

	strictPage(t, res, body)

	// a shell: nothing about the org, so slugs cannot be probed
	if strings.Contains(body, "Parish One") || !strings.Contains(body, `data-slug="parish-1"`) || !strings.Contains(body, `data-collection="users"`) {
		t.Fatalf("unexpected shell %s", body)
	}

	// an unknown slug looks the same
	other, res2 := servePage(t, app, "/orgs/no-such-org")
	if res2.StatusCode != 200 || !strings.Contains(other, `data-slug="no-such-org"`) {
		t.Fatal("unknown slugs must render the same shell")
	}

	if _, res3 := servePage(t, app, "/orgs/Bad_Slug"); res3.StatusCode != 404 {
		t.Fatalf("expected 404 for an invalid slug, got %d", res3.StatusCode)
	}
}

func TestOrgPagesAssets(t *testing.T) {
	t.Parallel()

	app := invFactory(t)
	defer app.Cleanup()

	for file, ct := range map[string]string{
		"hosted.css": "text/css", "auth.js": "application/javascript", "invite.js": "application/javascript", "admin.js": "application/javascript",
	} {
		body, res := servePage(t, app, "/orgs/assets/"+file)
		if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), ct) || res.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s: unexpected response %d %v", file, res.StatusCode, res.Header)
		}

		if strings.HasSuffix(file, ".js") && (strings.Contains(body, "innerHTML") || strings.Contains(body, "eval(") || strings.Contains(body, "document.write")) {
			t.Fatalf("%s: never parse strings as HTML", file)
		}
	}

	for _, bad := range []string{"nope.js", "..%2f..%2fgo.mod", "orgs_pages", "x.txt"} {
		if _, res := servePage(t, app, "/orgs/assets/"+bad); res.StatusCode != 404 {
			t.Fatalf("%s: expected 404, got %d", bad, res.StatusCode)
		}
	}
}

// Every element the scripts look up must exist in the page they belong to.
func TestOrgPagesScriptsMatchMarkup(t *testing.T) {
	t.Parallel()

	app := invFactory(t)
	defer app.Cleanup()

	check := func(pageURL, script string, extra ...string) {
		html, _ := servePage(t, app, pageURL)
		js, _ := servePage(t, app, "/orgs/assets/"+script)
		auth, _ := servePage(t, app, "/orgs/assets/auth.js")

		have := map[string]bool{}
		for _, m := range idRe.FindAllStringSubmatch(html, -1) {
			have[m[1]] = true
		}

		for _, src := range []string{js, auth} {
			for _, m := range jsIDRe.FindAllStringSubmatch(src, -1) {
				if !have[m[1]] && (src != js || m[1] != "toSignUp") { // toSignUp only exists when there is no account yet
					t.Errorf("%s uses #%s which %s does not have", script, m[1], pageURL)
				}
			}
		}
	}

	check("/orgs/invite/"+invTokenNewbie, "invite.js")
	check("/orgs/parish-1", "admin.js")
}

func TestHostedPagesCaptchaCSP(t *testing.T) {
	t.Parallel()

	app := invFactory(t)
	defer app.Cleanup()

	_, res := servePage(t, app, "/orgs/invite/"+invTokenNewbie)
	if strings.Contains(res.Header.Get("Content-Security-Policy"), "cloudflare") {
		t.Fatal("no provider domains without a captcha")
	}

	app.Settings().Captcha = core.CaptchaConfig{Provider: core.CaptchaProviderTurnstile, SiteKey: "s", SecretKey: "k"}
	app.Settings().Captcha.Actions.SignUp = true

	_, res = servePage(t, app, "/orgs/invite/"+invTokenNewbie)

	csp := res.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self' https://challenges.cloudflare.com") || !strings.Contains(csp, "frame-src https://challenges.cloudflare.com") ||
		strings.Contains(csp, "unsafe-") {
		t.Fatalf("unexpected CSP %q", csp)
	}
}
