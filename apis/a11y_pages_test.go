package apis_test

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/mails"
)

// Fork-local (Claviger #83, 83-3 and 83-4): accessible mode on the hosted pages
// and the default language of an org.

func serveRaw(t testing.TB, app http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

	return rec
}

func TestAccessibleModeFilesAreServedAsFiles(t *testing.T) {
	t.Parallel()

	app := invFactory(t)
	defer app.Cleanup()

	servePage(t, app, "/orgs/parish-1")
	mux, _ := pageMuxes.Load(app)

	for _, tc := range []struct {
		path, contentType string
		status            int
		contains          string
	}{
		{"/a11y/a11y-mode.css", "text/css", 200, `html[data-a11y="large"]`},
		{"/a11y/a11y-mode.js", "application/javascript", 200, "localStorage"},
		{"/passkeys/assets/passkeys-page.css", "text/css", 200, ".card"},
		{"/passkeys/assets/passkeys-page.js", "application/javascript", 200, "PasskeysClient"},
		{"/a11y/passkeys-page.js", "", 404, ""},
		{"/a11y/other.js", "", 404, ""},
		{"/passkeys/assets/a11y-mode.css", "", 404, ""},
	} {
		rec := serveRaw(t, mux.(http.Handler), tc.path)
		if rec.Code != tc.status {
			t.Errorf("%s: status %d, want %d", tc.path, rec.Code, tc.status)
			continue
		}

		if tc.status != 200 {
			continue
		}

		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, tc.contentType) {
			t.Errorf("%s: content type %q", tc.path, ct)
		}

		if rec.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(rec.Body.String(), tc.contains) {
			t.Errorf("%s: nosniff or content is wrong", tc.path)
		}
	}

	// every read and write of storage in the script is guarded
	js := serveRaw(t, mux.(http.Handler), "/a11y/a11y-mode.js").Body.String()
	for _, call := range []string{"getItem", "setItem", "removeItem"} {
		i := strings.Index(js, call)
		if i < 0 {
			t.Fatalf("expected a %s call", call)
		}
	}

	if strings.Count(js, "try {") < 2 || strings.Count(js, "catch (e)") < 2 {
		t.Error("localStorage access must be wrapped in try/catch")
	}
}

var (
	toggleRe  = regexp.MustCompile(`<button type="button" id="a11yToggle" class="a11y-toggle" aria-pressed="false" hidden>([^<]+)</button>`)
	styleAttr = regexp.MustCompile(`\sstyle=`)
)

func TestEveryHostedPageCarriesAccessibleMode(t *testing.T) {
	t.Parallel()

	app := invFactory(t)
	defer app.Cleanup()

	for _, path := range []string{
		"/orgs/invite/" + invTokenFrank,
		"/orgs/invite/not-a-token",
		"/orgs/parish-1",
		"/passkeys",
	} {
		for _, lang := range []string{"", "es", "ar"} {
			body, res := getPage(t, app, path, pageReq{query: lang})

			m := toggleRe.FindStringSubmatch(body)
			if m == nil {
				t.Fatalf("%s?lang=%s: the toggle is missing or is not a button with aria-pressed", path, lang)
			}

			if lang == "es" && m[1] != "Texto más grande y alto contraste" {
				t.Errorf("%s: toggle label %q", path, m[1])
			}

			for _, want := range []string{`href="/a11y/a11y-mode.css"`, `src="/a11y/a11y-mode.js"`, `<main `, `class="skip" href="#app"`, `id="app"`} {
				if !strings.Contains(body, want) {
					t.Errorf("%s?lang=%s: missing %s", path, lang, want)
				}
			}

			// no inline script or style, whatever the page
			if styleAttr.MatchString(body) || strings.Contains(body, "<style") || inlineHandlerRe.MatchString(body) {
				t.Errorf("%s: inline style or handler", path)
			}

			for _, s := range inlineScriptRe.FindAllString(body, -1) {
				if !strings.Contains(s, " src=") && !strings.Contains(s, `type="application/json"`) {
					t.Errorf("%s: inline script %q", path, s)
				}
			}

			if csp := res.Header.Get("Content-Security-Policy"); strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
				t.Errorf("%s: CSP %q", path, csp)
			}
		}
	}
}

func TestPasskeysPageIsReadyForAssistiveTechnology(t *testing.T) {
	t.Parallel()

	app := invFactory(t)
	defer app.Cleanup()

	body, _ := getPage(t, app, "/passkeys", pageReq{})

	// every field has a label tied to it with for/id
	for _, id := range []string{"identity", "recoveryEmail", "pwEmail", "pwPass"} {
		if !strings.Contains(body, `<label for="`+id+`">`) || !strings.Contains(body, `id="`+id+`"`) {
			t.Errorf("the field %s has no label tied to it", id)
		}
	}

	// the controls that reveal a form are buttons that say so
	for _, id := range []string{"linkPasswordLogin", "linkRecovery"} {
		if !regexp.MustCompile(`<button type="button" class="linklike" id="` + id + `" aria-controls="\w+" aria-expanded="false">`).MatchString(body) {
			t.Errorf("%s is not a disclosure button", id)
		}
	}

	// a place for the error next to each form, and one landmark
	for _, id := range []string{"loginMsg", "pwMsg", "recoveryMsg"} {
		if !strings.Contains(body, `id="`+id+`"`) {
			t.Errorf("no place for the error of %s", id)
		}
	}

	if strings.Count(body, "<main ") != 1 || strings.Contains(body, "autofocus") || strings.Contains(body, "<a href=\"#\"") {
		t.Error("expected one <main>, no autofocus and no links used as buttons")
	}
}

func TestOrgsPagesTieErrorsToTheirFields(t *testing.T) {
	t.Parallel()

	app := invFactory(t)
	defer app.Cleanup()

	body, _ := getPage(t, app, "/orgs/parish-1", pageReq{})

	if n := strings.Count(body, `aria-describedby="status"`); n != 3 {
		t.Errorf("the sign-in fields must point at the message: %d", n)
	}

	if strings.Count(body, `<th scope="col">`) < 15 || strings.Contains(body, "<th></th>") {
		t.Error("table headers need a scope and a name")
	}
}

func TestOrgDefaultLocale(t *testing.T) {
	t.Parallel()

	app := invFactory(t)
	defer app.Cleanup()

	org, err := app.FindRecordById(core.CollectionNameOrgs, orgP1)
	orgMust(t, err)

	invite := "/orgs/invite/" + invTokenFrank
	lang := func(r pageReq) string {
		body, _ := getPage(t, app, invite, r)
		if m := htmlLangRe.FindStringSubmatch(body); m != nil {
			return m[1]
		}

		return ""
	}

	// no locale: unchanged
	if got := lang(pageReq{}); got != "en" {
		t.Fatalf("no org locale: %s", got)
	}

	org.Set("settings", map[string]any{"locale": "es"})
	orgMust(t, app.Save(org))

	for _, tc := range []struct {
		name string
		req  pageReq
		want string
	}{
		{"the org's locale is the default", pageReq{}, "es"},
		{"Accept-Language wins", pageReq{accept: "fr"}, "fr"},
		{"?lang= wins", pageReq{query: "ar", accept: "fr"}, "ar"},
		{"the lang cookie wins", pageReq{cookie: "ko"}, "ko"},
	} {
		if got := lang(tc.req); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}

	// an unusable locale falls back to the app default, not to English by accident
	app.Settings().Meta.DefaultLanguage = "pt-BR"
	orgMust(t, app.Save(app.Settings()))

	for _, bad := range []any{"xx", "", 7, nil} {
		org.Set("settings", map[string]any{"locale": bad})
		orgMust(t, app.Save(org))

		if got := lang(pageReq{}); got != "pt-BR" {
			t.Errorf("locale %v: %s, want the app default", bad, got)
		}
	}

	// the invitation email follows the org too
	org.Set("settings", map[string]any{"locale": "fr"})
	orgMust(t, app.Save(org))

	inv, err := app.FindRecordById(core.CollectionNameInvitations, invNewbie)
	orgMust(t, err)
	orgMust(t, mails.SendOrgInvitation(app, org, inv, "", "tok-org-locale"))

	m := app.TestMailer.LastMessage()
	if !strings.Contains(m.HTML, `lang="fr"`) || strings.Contains(m.Subject, "You are invited") {
		t.Fatalf("expected a French invitation, got %q\n%s", m.Subject, m.HTML)
	}

	if got := core.OrgDefaultLanguage(app, nil); got != "pt-BR" {
		t.Errorf("no org: %s", got)
	}
}
