package apis_test

import (
	"net/http"
	"strings"
	"testing"
)

// Fork-local (Claviger #80, 80-6): the hosted "My household" page.

func TestHouseholdsMinePage(t *testing.T) {
	t.Parallel()

	app := hhFactory(t)
	defer app.Cleanup()

	body, res := servePage(t, app, "/households/mine")
	if res.StatusCode != http.StatusOK || !strings.Contains(res.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("%d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}

	// strict policy, no inline script or style, no caching
	strictPage(t, res, body)

	for _, want := range []string{
		`data-page="households"`, `data-collection="users"`, `id="households"`, `id="signin"`, `id="hstatus"`,
		`/orgs/assets/households.js`, "My household", `lang="en"`, `id="i18n"`, `class="skip"`, `id="a11yToggle"`,
		// the strings the script needs travel in the data block
		"js.households.notEncrypted", "js.households.photoUnavailable",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}

	// the page is a shell: it carries no household data
	for _, not := range []string{"Smith", "Dave family", "Kid B"} {
		if strings.Contains(body, not) {
			t.Errorf("page leaks %q", not)
		}
	}

	// Spanish (usted) on request
	es, res := servePage(t, app, "/households/mine?lang=es")
	if res.StatusCode != http.StatusOK || !strings.Contains(es, `lang="es"`) || !strings.Contains(es, "Mi hogar") ||
		!strings.Contains(es, "Su iglesia aún no ha terminado") {
		t.Errorf("no Spanish page: %s", es)
	}

	// right-to-left for Arabic
	if ar, _ := servePage(t, app, "/households/mine?lang=ar"); !strings.Contains(ar, `dir="rtl"`) {
		t.Error("Arabic page is not right-to-left")
	}

	// script and style are served as files by the org assets route
	for _, c := range []struct{ file, ct string }{{"households.js", "javascript"}, {"hosted.css", "css"}} {
		js, res := servePage(t, app, "/orgs/assets/"+c.file)
		if res.StatusCode != http.StatusOK || !strings.Contains(res.Header.Get("Content-Type"), c.ct) || len(js) < 500 {
			t.Errorf("%s: %d %s (%d bytes)", c.file, res.StatusCode, res.Header.Get("Content-Type"), len(js))
		}
	}
}

// The script reads its API from the same origin only, and builds no HTML from data.
func TestHouseholdsScriptIsSafe(t *testing.T) {
	t.Parallel()

	app := hhFactory(t)
	defer app.Cleanup()

	js, _ := servePage(t, app, "/orgs/assets/households.js")

	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "http://", "https://"} {
		if strings.Contains(js, bad) {
			t.Errorf("households.js contains %q", bad)
		}
	}

	// the photo routes are used behind a feature check
	for _, want := range []string{"/photo", "photosUnavailable", `fd.append("photo"`, "/api/households"} {
		if !strings.Contains(js, want) {
			t.Errorf("households.js lacks %q", want)
		}
	}
}

// Every household API call the page makes needs a signed-in user.
func TestHouseholdsPageAPIRequiresAuth(t *testing.T) {
	t.Parallel()

	runOrgCases(t, []orgCase{
		{name: "mine", method: http.MethodGet, url: "/api/households/mine", status: 401, factory: hhFactory},
		{name: "patch", method: http.MethodPatch, url: "/api/households/" + hhH1, body: `{"name":"x"}`, status: 401, factory: hhFactory},
	})
}
