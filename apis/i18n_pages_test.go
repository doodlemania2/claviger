package apis_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/apis"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/mails"
	"github.com/doodlemania2/claviger/tests"
)

// Fork-local (Claviger #97): the language of the hosted pages and emails.

type pageReq struct {
	query, accept, cookie string
}

// getPage serves a hosted page with a language preference.
func getPage(t testing.TB, app *tests.TestApp, url string, p pageReq) (string, *http.Response) {
	t.Helper()

	// servePage builds and caches the mux
	servePage(t, app, "/orgs/parish-1")

	mux, _ := pageMuxes.Load(app)

	target := url
	if p.query != "" {
		target += "?lang=" + p.query
	}

	req := httptest.NewRequest(http.MethodGet, target, nil)
	if p.accept != "" {
		req.Header.Set("Accept-Language", p.accept)
	}

	if p.cookie != "" {
		req.AddCookie(&http.Cookie{Name: "lang", Value: p.cookie})
	}

	rec := httptest.NewRecorder()
	mux.(http.Handler).ServeHTTP(rec, req)

	res := rec.Result()

	return pageBody(t, res), res
}

var (
	htmlLangRe = regexp.MustCompile(`<html lang="([^"]+)"( dir="rtl")?>`)
	dataRe     = regexp.MustCompile(`(?s)<script type="application/json" id="i18n">(.*?)</script>`)
)

func TestHostedPagesAreLocalized(t *testing.T) {
	t.Parallel()

	app := invFactory(t)
	defer app.Cleanup()

	invite := "/orgs/invite/" + invTokenFrank

	for _, tc := range []struct {
		name string
		req  pageReq
		lang string
		rtl  bool
		text []string // in the body
		not  []string
	}{
		{"english by default", pageReq{}, "en", false, []string{"Sign in", "Join Parish One"}, []string{` dir=`}},
		{"?lang=es", pageReq{query: "es"}, "es", false, []string{"Iniciar sesión"}, []string{"Join Parish One", ` dir=`}},
		{"?lang=ar is right-to-left", pageReq{query: "ar"}, "ar", true, []string{"تسجيل الدخول"}, []string{"Join Parish One"}},
		{"Accept-Language", pageReq{accept: "de, pt-BR;q=0.8, es;q=0.5"}, "pt-BR", false, []string{"Entrar"}, nil},
		{"the cookie wins over Accept-Language", pageReq{cookie: "fr", accept: "es"}, "fr", false, []string{"Se connecter"}, nil},
		{"?lang= wins over the cookie", pageReq{query: "ko", cookie: "fr"}, "ko", false, []string{"로그인"}, nil},
		{"an unsupported ?lang= is skipped", pageReq{query: "xx", accept: "vi"}, "vi", false, []string{"Đăng nhập"}, nil},
		{"a region falls back to the language", pageReq{accept: "zh-TW"}, "zh-Hans", false, []string{"登录"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, res := getPage(t, app, invite, tc.req)
			if res.StatusCode != 200 {
				t.Fatalf("status %d", res.StatusCode)
			}

			m := htmlLangRe.FindStringSubmatch(body)
			if m == nil || m[1] != tc.lang || (m[2] != "") != tc.rtl {
				t.Fatalf("html attributes %v, want lang=%s rtl=%v", m, tc.lang, tc.rtl)
			}

			for _, s := range tc.text {
				if !strings.Contains(body, s) {
					t.Errorf("missing %q", s)
				}
			}

			for _, s := range tc.not {
				if strings.Contains(body, s) {
					t.Errorf("unexpected %q", s)
				}
			}

			// the strict policy is the same in every language, and the strings are data, not script
			strictPage(t, res, body)

			if !dataRe.MatchString(body) {
				t.Error("the JSON data block with the script strings is missing")
			}
		})
	}
}

func TestExplicitLanguageIsRemembered(t *testing.T) {
	t.Parallel()

	app := invFactory(t)
	defer app.Cleanup()

	_, res := getPage(t, app, "/orgs/invite/"+invTokenFrank, pageReq{query: "es"})

	var found *http.Cookie

	for _, c := range res.Cookies() {
		if c.Name == "lang" {
			found = c
		}
	}

	if found == nil || found.Value != "es" || !found.HttpOnly || found.Path != "/" {
		t.Fatalf("expected the lang cookie, got %v", res.Cookies())
	}

	// no explicit choice: no cookie
	_, res = getPage(t, app, "/orgs/invite/"+invTokenFrank, pageReq{accept: "es"})
	if len(res.Cookies()) != 0 {
		t.Fatalf("unexpected cookies %v", res.Cookies())
	}
}

func TestScriptStringsComeInTheNegotiatedLanguage(t *testing.T) {
	t.Parallel()

	app := invFactory(t)
	defer app.Cleanup()

	body, _ := getPage(t, app, "/orgs/parish-1", pageReq{query: "ar"})

	m := dataRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no data block")
	}

	var msgs map[string]string
	if err := jsonUnmarshal(m[1], &msgs); err != nil {
		t.Fatal(err)
	}

	if msgs["js.tooMany"] == "" || msgs["js.tooMany"] == "Too many attempts. Try again later." || msgs["js.admin.save"] == "" {
		t.Fatalf("expected Arabic script strings, got %v", msgs["js.tooMany"])
	}

	// only the strings a script needs are sent
	for k := range msgs {
		if !strings.HasPrefix(k, "js.") {
			t.Fatalf("unexpected key %q in the data block", k)
		}
	}

	// the same page in English carries the English strings
	en, _ := getPage(t, app, "/orgs/parish-1", pageReq{})
	if !strings.Contains(dataRe.FindStringSubmatch(en)[1], "Too many attempts. Try again later.") {
		t.Fatal("English script strings are missing")
	}
}

func TestInvitationPageUsesTheAccountsLanguage(t *testing.T) {
	t.Parallel()

	app := invFactory(t)
	defer app.Cleanup()

	users, err := app.FindCollectionByNameOrId("users")
	orgMust(t, err)
	users.Fields.Add(&core.TextField{Name: "lang"})
	orgMust(t, app.Save(users))

	frank, err := app.FindAuthRecordByEmail("users", "frank@orgs.test")
	orgMust(t, err)
	frank.Set("lang", "es")
	orgMust(t, app.Save(frank))

	// the account's preference beats Accept-Language, ?lang= beats the account
	body, _ := getPage(t, app, "/orgs/invite/"+invTokenFrank, pageReq{accept: "fr"})
	if m := htmlLangRe.FindStringSubmatch(body); m == nil || m[1] != "es" {
		t.Fatalf("expected the account's language, got %v", m)
	}

	body, _ = getPage(t, app, "/orgs/invite/"+invTokenFrank, pageReq{query: "ko"})
	if m := htmlLangRe.FindStringSubmatch(body); m == nil || m[1] != "ko" {
		t.Fatalf("expected ?lang=ko, got %v", m)
	}
}

func TestDefaultLanguageSettingAppliesToPagesAndEmails(t *testing.T) {
	t.Parallel()

	app := invFactory(t)
	defer app.Cleanup()

	app.Settings().Meta.DefaultLanguage = "pt-BR"
	orgMust(t, app.Save(app.Settings()))

	body, _ := getPage(t, app, "/orgs/invite/"+invTokenFrank, pageReq{})
	if m := htmlLangRe.FindStringSubmatch(body); m == nil || m[1] != "pt-BR" {
		t.Fatalf("expected the app default, got %v", m)
	}

	// an Accept-Language the catalogs serve still comes first
	body, _ = getPage(t, app, "/orgs/invite/"+invTokenFrank, pageReq{accept: "ar"})
	if m := htmlLangRe.FindStringSubmatch(body); m == nil || m[1] != "ar" {
		t.Fatalf("expected Accept-Language to win, got %v", m)
	}

	org, err := app.FindRecordById(core.CollectionNameOrgs, orgP1)
	orgMust(t, err)
	inv, err := app.FindRecordById(core.CollectionNameInvitations, invNewbie)
	orgMust(t, err)

	orgMust(t, mails.SendOrgInvitation(app, org, inv, "", "tok-x"))

	m := app.TestMailer.LastMessage()
	if strings.Contains(m.Subject, "You are invited") || !strings.Contains(m.Subject, "Parish One") || !strings.Contains(m.HTML, `lang="pt-BR"`) {
		t.Fatalf("expected a Portuguese invitation, got %q\n%s", m.Subject, m.HTML)
	}

	if strings.Contains(m.HTML, "Someone") || regexp.MustCompile(`\{[A-Z_]+\}`).MatchString(m.HTML) {
		t.Fatalf("unresolved text in %s", m.HTML)
	}

	// an org's own template is theirs, in every language
	org.Set("settings", map[string]any{"invitationEmail": map[string]any{"subject": "Join {ORG_NAME}!"}})
	orgMust(t, app.Save(org))
	orgMust(t, mails.SendOrgInvitation(app, org, inv, "Jo", "tok-y"))

	if got := app.TestMailer.LastMessage().Subject; got != "Join Parish One!" {
		t.Fatalf("an org's subject override must be used as it is, got %q", got)
	}
}

func TestPasskeysPageIsLocalizedAndKeepsItsPolicy(t *testing.T) {
	t.Parallel()

	app := invFactory(t)
	defer app.Cleanup()

	en, enRes := getPage(t, app, "/passkeys", pageReq{})
	ar, arRes := getPage(t, app, "/passkeys", pageReq{query: "ar"})
	es, esRes := getPage(t, app, "/passkeys", pageReq{accept: "es-MX"})

	if !strings.Contains(en, `<html lang="en">`) || !strings.Contains(ar, `<html lang="ar" dir="rtl">`) || !strings.Contains(es, `<html lang="es">`) {
		t.Fatal("lang and dir attributes are wrong")
	}

	if strings.Contains(es, "<title>Passkeys</title>") {
		t.Fatalf("the Spanish page is still English")
	}

	if strings.Contains(ar, "Sign in with a passkey") || strings.Contains(ar, "@{") {
		t.Fatal("the Arabic page has English text or an unfilled marker")
	}

	for _, body := range []string{en, ar, es} {
		if strings.Contains(body, "@{") {
			t.Fatal("unfilled marker")
		}
	}

	// the policy does not depend on the language
	for _, res := range []*http.Response{arRes, esRes} {
		if res.Header.Get("Content-Security-Policy") != enRes.Header.Get("Content-Security-Policy") {
			t.Fatal("the CSP must not change with the language")
		}
	}
}

func TestStylesheetsUseLogicalProperties(t *testing.T) {
	t.Parallel()

	css, err := os.ReadFile("orgs_pages/hosted.css")
	orgMust(t, err)

	if physical := regexp.MustCompile(`(?:margin|padding)-(?:left|right)|text-align:\s*(?:left|right)|\b(?:left|right):`).FindString(string(css)); physical != "" {
		t.Fatalf("hosted.css uses a physical property that does not mirror for right-to-left: %q", physical)
	}
}

var _ = apis.NewPageText

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }
