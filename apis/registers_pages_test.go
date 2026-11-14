package apis_test

import (
	"net/http"
	"strings"
	"testing"
)

// Fork-local (Claviger #87, 87-5): the hosted registers page of an org.

func TestRegistersPage(t *testing.T) {
	t.Parallel()

	app := consentFactory(t)
	defer app.Cleanup()

	body, res := servePage(t, app, "/orgs/registers/parish-1")
	if res.StatusCode != http.StatusOK || !strings.Contains(res.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("%d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}

	strictPage(t, res, body)

	for _, want := range []string{
		`data-page="registers"`, `data-slug="parish-1"`, `data-collection="users"`, `id="signin"`, `id="pstatus"`, `id="noteForm"`,
		`id="certHost"`, `/orgs/assets/registers.js`, "Registers", `lang="en"`, `id="i18n"`,
		`class="skip"`, `id="a11yToggle"`, "js.registers.confirmFinalize", "js.registers.kind.later_event",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}

	// the page is a shell: it carries no entry
	if strings.Contains(body, "Smith") {
		t.Error("page leaks an entry")
	}

	if ar, _ := servePage(t, app, "/orgs/registers/parish-1?lang=ar"); !strings.Contains(ar, `dir="rtl"`) {
		t.Error("Arabic page is not right-to-left")
	}

	if _, res := servePage(t, app, "/orgs/registers/Not_A_Slug"); res.StatusCode != http.StatusNotFound {
		t.Errorf("a bad slug: %d", res.StatusCode)
	}

	js, res := servePage(t, app, "/orgs/assets/registers.js")
	if res.StatusCode != http.StatusOK || !strings.Contains(res.Header.Get("Content-Type"), "javascript") {
		t.Fatalf("registers.js: %d", res.StatusCode)
	}

	// the certificate frame is built by the script, sandboxed: no script can run in it
	if !strings.Contains(js, `sandbox: "allow-same-origin allow-modals"`) {
		t.Error("registers.js does not sandbox the certificate frame")
	}

	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "http://", "https://"} {
		if strings.Contains(js, bad) {
			t.Errorf("registers.js contains %q", bad)
		}
	}
}
