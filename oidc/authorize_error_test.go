package oidc_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// An authorize request that cannot be trusted (unknown client_id, a redirect_uri
// that is not registered) is answered with the hosted error page: HTML, a fixed
// localized message, nothing the caller sent, no redirect, the status the
// library uses (400).
func TestUntrustedAuthorizeRequestsGetTheHostedErrorPage(t *testing.T) {
	e := newEnv(t)
	clientID, _ := e.createClient(map[string]any{"public": true, "name": "Third Party App"})

	const canary = "canary-9f3a7c"

	for _, tc := range []struct {
		name, client, redirect string
	}{
		{"unknown client", "no-such-client-" + canary, ""},
		{"unregistered redirect_uri", clientID, "https://evil.test/" + canary},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := e.browser()
			target, _ := b.authorizeURL(tc.client, authOpts{redirect: tc.redirect})
			target += "&state=" + canary

			rec := b.do("GET", target, nil, nil)
			body := rec.Body.String()

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", rec.Code, body)
			}

			if loc := rec.Header().Get("Location"); loc != "" {
				t.Fatalf("must not redirect, got %q", loc)
			}

			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
				t.Fatalf("content type %q, want HTML", ct)
			}

			for _, want := range []string{"<!doctype html>", `data-page="error"`, "This sign-in link is not valid. Return to the app and try again.", "/oidc/assets/oidc.css"} {
				if !strings.Contains(body, want) {
					t.Errorf("missing %q in %s", want, body)
				}
			}

			for _, leaked := range []string{canary, "unable to retrieve", "evil.test", url.QueryEscape("https://evil.test")} {
				if strings.Contains(body, leaked) {
					t.Errorf("the page reflects %q: %s", leaked, body)
				}
			}

			if csp := rec.Header().Get("Content-Security-Policy"); csp != pagesCSPForTest {
				t.Errorf("unexpected CSP %q", csp)
			}

			// localized like the other hosted pages
			es := e.browser().do("GET", target, map[string]string{"Accept-Language": "es"}, nil)
			if es.Code != http.StatusBadRequest || !strings.Contains(es.Body.String(), "no es válido") {
				t.Errorf("Spanish page: %d %s", es.Code, es.Body.String())
			}
		})
	}
}
