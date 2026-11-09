package oidc_test

import (
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
