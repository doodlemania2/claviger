package oidc_test

import (
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #97): the hosted pages in the negotiated language.

var (
	pageLangRe = regexp.MustCompile(`<html lang="([^"]+)"( dir="rtl")?>`)
	pageDataRe = regexp.MustCompile(`(?s)<script type="application/json" id="i18n">(.*?)</script>`)
)

func TestSignInAndConsentPagesAreLocalized(t *testing.T) {
	e := newEnv(t)
	clientID, _ := e.createClient(map[string]any{"public": true, "name": "Third Party App"})

	b := e.browser()
	reqID, _ := b.startAuthorize(clientID, authOpts{})
	login := "/oidc/login?authRequestID=" + url.QueryEscape(reqID)

	for _, tc := range []struct {
		name, query, accept, lang string
		rtl                       bool
		text                      string
	}{
		{"english", "", "", "en", false, "to continue to <strong>Third Party App</strong>"},
		{"spanish by Accept-Language", "", "es-ES,es;q=0.9", "es", false, "para continuar a <strong>Third Party App</strong>"},
		{"arabic by ?lang= is right-to-left", "&lang=ar", "es", "ar", true, "تسجيل الدخول"},
		{"portuguese region", "", "pt-PT", "pt-BR", false, "Entrar"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			delete(b.cookies, "lang") // a ?lang= is remembered in a cookie, and the cookie wins over Accept-Language

			h := map[string]string{}
			if tc.accept != "" {
				h["Accept-Language"] = tc.accept
			}

			rec := b.do("GET", login+tc.query, h, nil)
			body := rec.Body.String()

			m := pageLangRe.FindStringSubmatch(body)
			if rec.Code != 200 || m == nil || m[1] != tc.lang || (m[2] != "") != tc.rtl {
				t.Fatalf("%d, html attributes %v, want %s rtl=%v", rec.Code, m, tc.lang, tc.rtl)
			}

			if tc.lang != "en" && strings.Contains(body, "Sign in") {
				t.Errorf("English text left on the %s page", tc.lang)
			}

			if !strings.Contains(body, tc.text) && !strings.Contains(body, strings.ReplaceAll(tc.text, "'", "&#39;")) {
				t.Errorf("missing %q in %s", tc.text, body)
			}

			// no inline script, the same policy in every language
			if csp := rec.Header().Get("Content-Security-Policy"); csp != pagesCSPForTest || strings.Contains(csp, "unsafe-inline") {
				t.Errorf("unexpected CSP %q", csp)
			}

			if !pageDataRe.MatchString(body) || strings.Contains(strings.ReplaceAll(pageDataRe.ReplaceAllString(body, ""), `<script type="module" src=`, ""), "<script") {
				t.Error("the strings must travel as a JSON data block and nothing else may be inline script")
			}
		})
	}

	// the consent page follows the account's own preference
	users, err := e.app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}

	users.Fields.Add(&core.TextField{Name: "lang"})

	if err := e.app.Save(users); err != nil {
		t.Fatal(err)
	}

	user := e.user()
	user.Set("lang", "ar")

	if err := e.app.Save(user); err != nil {
		t.Fatal(err)
	}

	token := b.signInToken(reqID)

	status, out := b.finish(reqID, token)
	if status != 200 {
		t.Fatalf("finish: %d %v", status, out)
	}

	consent := b.do("GET", out["redirect"].(string), map[string]string{"Accept-Language": "fr"}, nil).Body.String()
	if m := pageLangRe.FindStringSubmatch(consent); m == nil || m[1] != "ar" || m[2] == "" {
		t.Fatalf("the consent page must use the account's language, got %v", m)
	}

	if !strings.Contains(consent, "Third Party App") || !strings.Contains(consent, "test@example.com") || strings.Contains(consent, "Your email address") {
		t.Fatalf("unexpected consent page %s", consent)
	}
}

const pagesCSPForTest = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

func TestErrorPagesAndJSONRefusalsAreLocalized(t *testing.T) {
	e := newEnv(t)

	rec := e.browser().do("GET", "/oidc/login?authRequestID=nope", map[string]string{"Accept-Language": "fr"}, nil)
	if rec.Code != 400 || strings.Contains(rec.Body.String(), "This sign-in link has expired") || !strings.Contains(rec.Body.String(), `<html lang="fr">`) {
		t.Fatalf("expected a French error page: %d %s", rec.Code, rec.Body.String())
	}

	out := e.browser().do("GET", "/oidc/logged-out", map[string]string{"Accept-Language": "ko"}, nil).Body.String()
	if strings.Contains(out, "You are signed out") || !strings.Contains(out, `<html lang="ko">`) {
		t.Fatalf("expected a Korean logged-out page: %s", out)
	}

	status, body := e.browser().json("POST", "/oidc/login/finish", map[string]any{"authRequestID": "nope"}, map[string]string{"Accept-Language": "es"})
	msg, _ := body["message"].(string)

	if status != 400 || msg == "" || strings.Contains(msg, "This sign-in has expired") {
		t.Fatalf("expected a Spanish refusal: %d %v", status, body)
	}
}

// assertLocalizedExtPage checks the html attributes, the CSP and the absence
// of inline script of a chooser or device page.
func assertLocalizedExtPage(t *testing.T, rec *httptest.ResponseRecorder, lang string, rtl bool, wants, notWants []string) string {
	t.Helper()

	body := rec.Body.String()

	m := pageLangRe.FindStringSubmatch(body)
	if m == nil || m[1] != lang || (m[2] != "") != rtl {
		t.Fatalf("html attributes %v, want %s rtl=%v\n%s", m, lang, rtl, body)
	}

	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("missing %q in\n%s", w, body)
		}
	}

	for _, w := range notWants {
		if strings.Contains(body, w) {
			t.Errorf("English text %q left in the %s page", w, lang)
		}
	}

	if csp := rec.Header().Get("Content-Security-Policy"); csp != pagesCSPForTest || strings.Contains(csp, "unsafe-inline") {
		t.Errorf("unexpected CSP %q", csp)
	}

	if strings.Contains(strings.ReplaceAll(pageDataRe.ReplaceAllString(body, ""), `<script type="module" src=`, ""), "<script") {
		t.Error("nothing but the JSON data block and the module script may be a script")
	}

	return body
}

func TestChooserPageIsLocalized(t *testing.T) {
	e := newEnv(t)
	c, _ := e.createClient(map[string]any{"public": true, "firstParty": true})
	b, _, _ := twoAccounts(t, e, c)

	target, _ := b.authorizeURL(c, authOpts{})
	loc := b.do("GET", target, nil, nil).Header().Get("Location")

	for _, tc := range []struct {
		lang, accept string
		rtl          bool
		wants        []string
	}{
		{"es", "es-ES,es;q=0.9", false, []string{"Elige una cuenta", "Usar otra cuenta", "Cerrar sesión en todas las cuentas", `data-page="chooser"`}},
		{"ar", "ar", true, []string{"اختر حسابًا", "استخدام حساب آخر", "تسجيل الخروج من جميع الحسابات"}},
	} {
		t.Run(tc.lang, func(t *testing.T) {
			delete(b.cookies, "lang")

			rec := b.do("GET", loc, map[string]string{"Accept-Language": tc.accept}, nil)
			if rec.Code != 200 {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}

			body := assertLocalizedExtPage(t, rec, tc.lang, tc.rtl, tc.wants, []string{"Choose an account", "Use another account", "Sign out"})

			if !pageDataRe.MatchString(body) || !strings.Contains(body, "js.chooser.failed") {
				t.Error("the script strings must travel in the JSON data block")
			}
		})
	}

	// the refusal of the JSON endpoints follows the language too
	status, out := b.json("POST", "/oidc/account/select", map[string]any{"flow": "authorize", "ref": "nope"}, map[string]string{"Accept-Language": "es"})
	if msg, _ := out["message"].(string); status != 400 || msg == "" || strings.Contains(msg, "expired") {
		t.Fatalf("expected a Spanish refusal: %d %v", status, out)
	}

	// English stays as it was
	delete(b.cookies, "lang")

	rec := b.do("GET", loc, nil, nil)
	for _, want := range []string{`<html lang="en">`, "Choose an account", "Use another account", "Sign out of all accounts"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestDevicePagesAreLocalized(t *testing.T) {
	e := newEnv(t)

	// the column has to exist before the first query of the accounts (a cached plan on Postgres)
	users, err := e.app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}

	users.Fields.Add(&core.TextField{Name: "lang"})

	if err := e.app.Save(users); err != nil {
		t.Fatal(err)
	}
	clientID, _ := deviceClient(e, map[string]any{"public": true})
	d := e.mustDeviceStart(clientID, "", "openid profile")

	b := e.browser()

	code := b.do("GET", "/oidc/device", map[string]string{"Accept-Language": "es"}, nil)
	assertLocalizedExtPage(t, code, "es", false, []string{"Conectar un dispositivo", "Introduce el código", "Continuar"}, []string{"Connect a device", "Enter the code"})

	delete(b.cookies, "lang")

	bad := b.do("POST", "/oidc/device", map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Accept-Language": "ar"},
		strings.NewReader(url.Values{"user_code": {"ZZZZ-ZZZZ"}}.Encode()))
	assertLocalizedExtPage(t, bad, "ar", true, []string{"ربط جهاز", "لم يتم التعرف على هذا الرمز"}, []string{"Connect a device", "not recognised"})

	delete(b.cookies, "lang")

	ref, _ := b.enterCode(d.userCode)
	if ref == "" {
		t.Fatal("the code must be accepted")
	}

	b.deviceSignIn(ref, testEmail, testPass)

	verify := "/oidc/device/verify?ref=" + url.QueryEscape(ref)

	// the approval page: Accept-Language first ...
	rec := b.do("GET", verify, map[string]string{"Accept-Language": "fr"}, nil)
	body := assertLocalizedExtPage(t, rec, "fr", false, []string{"Autoriser", testEmail, "Annuler", `data-page="device-approve"`}, []string{"Allow this device?", "Cancel"})

	if !pageDataRe.MatchString(body) || !strings.Contains(body, "js.oidc.decisionFailed") {
		t.Error("the script strings must travel in the JSON data block")
	}

	delete(b.cookies, "lang")

	// ... and the account's own language wins over it
	user := e.user()
	user.Set("lang", "ar")

	if err := e.app.Save(user); err != nil {
		t.Fatal(err)
	}

	rec = b.do("GET", verify, map[string]string{"Accept-Language": "fr"}, nil)
	assertLocalizedExtPage(t, rec, "ar", true, []string{"هل تريد السماح لهذا الجهاز؟", testEmail}, []string{"Allow this device?"})

	// the decision refusals and the result page are localized too
	status, out := b.json("POST", "/oidc/device/decision", map[string]any{"ref": "nope", "allow": true}, map[string]string{"Accept-Language": "es"})
	if msg, _ := out["message"].(string); status == 200 || msg == "" || strings.Contains(msg, "expired") {
		t.Fatalf("expected a Spanish refusal: %d %v", status, out)
	}

	if status, out := b.decide(ref, true); status != 200 {
		t.Fatalf("decision: %d %v", status, out)
	}

	delete(b.cookies, "lang")

	done := b.do("GET", verify, map[string]string{"Accept-Language": "es"}, nil)
	assertLocalizedExtPage(t, done, "es", false, []string{"Dispositivo conectado", "Ya puedes volver a tu dispositivo."}, []string{"Device connected"})
}
