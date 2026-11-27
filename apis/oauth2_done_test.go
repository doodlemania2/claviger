package apis_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/tests"
)

// The window the OAuth2 sign-in ends on is public, outside the admin UI, and has no inline code.
func TestOAuth2DonePage(t *testing.T) {
	t.Parallel()

	scenarios := []tests.ApiScenario{
		{
			Name:            "success",
			Method:          http.MethodGet,
			URL:             "/api/oauth2-done?r=success",
			ExpectedStatus:  200,
			ExpectedContent: []string{"You can close this window.", `<script src="/api/oauth2-done.js">`},
			ExpectedEvents:  map[string]int{"*": 0},
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
				csp := res.Header.Get("Content-Security-Policy")
				if !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "/_/") {
					t.Fatalf("unexpected policy %q", csp)
				}
			},
		},
		{
			Name:            "anything but success reads as a failure",
			Method:          http.MethodGet,
			URL:             "/api/oauth2-done?r=failure",
			ExpectedStatus:  200,
			ExpectedContent: []string{"Sign-in did not complete."},
			ExpectedEvents:  map[string]int{"*": 0},
		},
		{
			Name:            "no parameter",
			Method:          http.MethodGet,
			URL:             "/api/oauth2-done",
			ExpectedStatus:  200,
			ExpectedContent: []string{"Sign-in did not complete."},
			ExpectedEvents:  map[string]int{"*": 0},
		},
		{
			Name:            "the script closes the window",
			Method:          http.MethodGet,
			URL:             "/api/oauth2-done.js",
			ExpectedStatus:  200,
			ExpectedContent: []string{"window.close()"},
			ExpectedEvents:  map[string]int{"*": 0},
		},
	}

	for _, s := range scenarios {
		s.Test(t)
	}
}
