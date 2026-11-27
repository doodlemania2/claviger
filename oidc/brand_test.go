package oidc_test

import (
	"net/url"
	"strings"
	"testing"
)

// Every page carries the Claviger mark; a client's logo and accent show only when valid,
// and only the logo's own origin widens the policy.
func TestSignInPageBranding(t *testing.T) {
	e := newEnv(t)

	page := func(clientID string) (string, string) {
		t.Helper()

		b := e.browser()
		reqID, _ := b.startAuthorize(clientID, authOpts{})
		rec := b.do("GET", "/oidc/login?authRequestID="+url.QueryEscape(reqID), nil, nil)
		if rec.Code != 200 {
			t.Fatalf("login page: %d", rec.Code)
		}

		return rec.Body.String(), rec.Header().Get("Content-Security-Policy")
	}

	// no client look: the Claviger mark and line only, a strict policy
	plain, _ := e.createClient(map[string]any{"firstParty": true, "public": true})
	html, csp := page(plain)
	for _, want := range []string{"/oidc/assets/brand/claviger-mark.svg", "Secured by Claviger"} {
		if !strings.Contains(html, want) {
			t.Errorf("plain page lacks %q", want)
		}
	}
	if strings.Contains(html, `class="clientlogo"`) || strings.Contains(html, "accent.css") || strings.Contains(csp, "cdn.example") {
		t.Errorf("a client without branding must show none: %s", csp)
	}

	// a branded client
	branded, _ := e.createClient(map[string]any{"firstParty": true, "public": true, "logoUri": "https://cdn.example.test/chores.png", "accentColor": "#1f9d55"})
	html, csp = page(branded)
	if !strings.Contains(html, `class="clientlogo" src="https://cdn.example.test/chores.png"`) || !strings.Contains(html, "accent.css?c="+branded) {
		t.Errorf("the branded page lacks the logo or accent: %s", html)
	}
	if !strings.Contains(csp, "img-src 'self' data: https://cdn.example.test") || strings.Contains(csp, "unsafe-inline") {
		t.Errorf("the policy must open only the logo origin: %s", csp)
	}

	rec := e.browser().do("GET", "/oidc/assets/accent.css?c="+branded, nil, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "--accent: #1f9d55") {
		t.Errorf("accent.css: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.browser().do("GET", "/oidc/assets/accent.css?c="+plain, nil, nil); rec.Code != 404 {
		t.Errorf("a client without an accent must 404, got %d", rec.Code)
	}

	// values that are not an https image or a #rrggbb colour are ignored
	for _, bad := range []string{"javascript:alert(1)", "http://evil.test/x.png", "data:image/png;base64,AAAA", "https://u:p@evil.test/x.png"} {
		id, _ := e.createClient(map[string]any{"firstParty": true, "public": true, "logoUri": bad})
		html, csp = page(id)
		if strings.Contains(html, `class="clientlogo"`) || strings.Contains(csp, "evil.test") {
			t.Errorf("logo %q must be ignored", bad)
		}
	}

	for _, name := range []string{"claviger-mark.svg", "claviger-mark-dark.svg"} {
		if rec := e.browser().do("GET", "/oidc/assets/brand/"+name, nil, nil); rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "image/svg+xml") {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	if rec := e.browser().do("GET", "/oidc/assets/brand/other.svg", nil, nil); rec.Code != 404 {
		t.Errorf("an unknown brand file must 404, got %d", rec.Code)
	}
}
