package apis_test

import (
	"net/http"
	"strings"
	"testing"
)

// Fork-local (Claviger #88): the hosted consent purposes page of an org.

func TestConsentPurposesPage(t *testing.T) {
	t.Parallel()

	app := consentFactory(t)
	defer app.Cleanup()

	body, res := servePage(t, app, "/orgs/consents/parish-1")
	if res.StatusCode != http.StatusOK || !strings.Contains(res.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("%d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}

	strictPage(t, res, body)

	for _, want := range []string{
		`data-page="consentpurposes"`, `data-slug="parish-1"`, `data-collection="users"`, `id="purposeForm"`, `id="signin"`, `id="pstatus"`,
		`/orgs/assets/consents.js`, "Consent purposes", `lang="en"`, `id="i18n"`, `class="skip"`, `id="a11yToggle"`,
		"js.consentPurposes.hasRecords", "Direct marketing can never be consented to for a minor",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}

	// the page is a shell: it carries no purpose of the org
	for _, not := range []string{"We email you the newsletter", "Photos of you"} {
		if strings.Contains(body, not) {
			t.Errorf("page leaks %q", not)
		}
	}

	if ar, _ := servePage(t, app, "/orgs/consents/parish-1?lang=ar"); !strings.Contains(ar, `dir="rtl"`) {
		t.Error("Arabic page is not right-to-left")
	}

	if _, res := servePage(t, app, "/orgs/consents/Not_A_Slug"); res.StatusCode != http.StatusNotFound {
		t.Errorf("a bad slug: %d", res.StatusCode)
	}

	js, res := servePage(t, app, "/orgs/assets/consents.js")
	if res.StatusCode != http.StatusOK || !strings.Contains(res.Header.Get("Content-Type"), "javascript") {
		t.Fatalf("consents.js: %d", res.StatusCode)
	}

	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "http://", "https://"} {
		if strings.Contains(js, bad) {
			t.Errorf("consents.js contains %q", bad)
		}
	}
}
