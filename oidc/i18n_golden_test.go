package oidc_test

import (
	"flag"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// The golden files hold the English hosted pages exactly as master rendered
// them before localization (Claviger #97). The only allowed difference is the
// inline JSON data block with the strings the page scripts need (stripped
// before comparing). Regenerate with -update-i18n-golden for a deliberate change.
var updateGolden = flag.Bool("update-i18n-golden", false, "rewrite the i18n golden files")

var (
	i18nDataBlockRe = regexp.MustCompile(`(?s)<script type="application/json" id="i18n">.*?</script>\n?`)
	requestIDRe     = regexp.MustCompile(`data-request="[^"]*"`)
)

func checkGolden(t *testing.T, name, got string) {
	t.Helper()

	got = requestIDRe.ReplaceAllString(i18nDataBlockRe.ReplaceAllString(got, ""), `data-request="ID"`)
	path := filepath.Join("testdata", "i18n_golden", name)

	if *updateGolden {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}

		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if string(want) != got {
		t.Fatalf("%s differs from the golden file:\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func TestEnglishHostedPagesAreIdenticalToTheGolden(t *testing.T) {
	e := newEnv(t)
	thirdParty, _ := e.createClient(map[string]any{"public": true, "name": "Third Party App"})

	b := e.browser()
	reqID, _ := b.startAuthorize(thirdParty, authOpts{})
	checkGolden(t, "login.html", b.do("GET", "/oidc/login?authRequestID="+url.QueryEscape(reqID), nil, nil).Body.String())

	token := b.signInToken(reqID)
	status, out := b.finish(reqID, token)
	if status != 200 {
		t.Fatalf("finish: %d %v", status, out)
	}

	checkGolden(t, "consent.html", b.do("GET", out["redirect"].(string), nil, nil).Body.String())
	checkGolden(t, "logged_out.html", e.browser().do("GET", "/oidc/logged-out", nil, nil).Body.String())
	checkGolden(t, "error_expired.html", e.browser().do("GET", "/oidc/login?authRequestID=nope", nil, nil).Body.String())
	checkGolden(t, "error_consent_expired.html", e.browser().do("GET", "/oidc/consent?authRequestID=nope", nil, nil).Body.String())
}
