package apis_test

import (
	"net/http"
	"strings"
	"testing"
)

// Fork-local (Claviger #81, 81-4): the hosted "My credentials" page and
// GET /api/me/access-status.

func TestCredentialsMinePage(t *testing.T) {
	t.Parallel()

	app := credFactory(t)
	defer app.Cleanup()

	body, res := servePage(t, app, "/credentials/mine")
	if res.StatusCode != http.StatusOK || !strings.Contains(res.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("%d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}

	strictPage(t, res, body)

	for _, want := range []string{
		`data-page="credentials"`, `data-collection="users"`, `id="credentials"`, `id="signin"`, `id="cstatus"`,
		`/orgs/assets/credentials.js`, "My credentials", `lang="en"`, `id="i18n"`, `class="skip"`, `id="a11yToggle"`,
		"js.credentials.detail.lapsed", "js.credentials.emptyListNote",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}

	// the page is a shell: it carries no credential data
	for _, not := range []string{"safe_environment", "strict_check"} {
		if strings.Contains(body, not) {
			t.Errorf("page leaks %q", not)
		}
	}

	if ar, _ := servePage(t, app, "/credentials/mine?lang=ar"); !strings.Contains(ar, `dir="rtl"`) {
		t.Error("Arabic page is not right-to-left")
	}

	js, res := servePage(t, app, "/orgs/assets/credentials.js")
	if res.StatusCode != http.StatusOK || !strings.Contains(res.Header.Get("Content-Type"), "javascript") {
		t.Fatalf("credentials.js: %d", res.StatusCode)
	}

	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "http://", "https://", `method: "PATCH"`, `method: "DELETE"`} {
		if strings.Contains(js, bad) {
			t.Errorf("credentials.js contains %q", bad)
		}
	}
}

func TestMyAccessStatus(t *testing.T) {
	t.Parallel()

	const url = "/api/me/access-status"

	runOrgCases(t, []orgCase{
		credCase("guests", "", "GET", url, "", 401),
		// carol holds a valid safe_environment credential and has a strict_check type waiting in P1
		{name: "a holder sees their own standing, with the coordinators by name", who: "carol", method: "GET", url: url, status: 200, factory: credFactory,
			has: []string{`"key":"safe_environment"`, `"standing":"cleared"`, `"key":"strict_check"`, `"standing":"missing"`, `"coordinators":[`},
			hasNot: []string{`"email"`, `revokedReason`}},
		// dave's strict_check credential is waiting for a second verifier
		credCase("a pending credential is shown as waiting", "dave", "GET", url, "", 200, `"standing":"pending"`),
		// erin is in P2 only: she must not see P1 types
		{name: "another org's types stay out", who: "erin", method: "GET", url: url, status: 200, factory: credFactory,
			has: []string{`"name":"Parish Two"`}, hasNot: []string{`strict_check`}},
	})
}
