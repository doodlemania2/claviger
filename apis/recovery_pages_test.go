package apis_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Fork-local (Claviger #83, 83-2): the hosted recovery pages.

func (env *recoveryEnv) get(url string) (*http.Response, string) {
	req := httptest.NewRequest(http.MethodGet, url, nil)
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)

	return rec.Result(), rec.Body.String()
}

func TestRecoveryPersonPage(t *testing.T) {
	t.Parallel()

	env := newRecoveryEnv(t)

	res, body := env.get("/recovery")
	if res.StatusCode != http.StatusOK || !strings.Contains(res.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("%d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}

	strictPage(t, res, body)

	for _, want := range []string{`id="codeForm"`, `id="code"`, `/orgs/assets/recovery.js`, "Get back into your account", `lang="en"`, `id="i18n"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}

	// Spanish on request; the strings the script needs travel in the data block
	_, es := env.get("/recovery?lang=es")
	if !strings.Contains(es, "Recupere el acceso a su cuenta") || !strings.Contains(es, `lang="es"`) || !strings.Contains(es, "Ese código no funcionó") {
		t.Errorf("no Spanish page: %s", es)
	}
}

func TestRecoveryStaffPage(t *testing.T) {
	t.Parallel()

	env := newRecoveryEnv(t)

	res, body := env.get("/orgs/recovery/parish-1")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("%d", res.StatusCode)
	}

	strictPage(t, res, body)

	for _, want := range []string{`data-slug="parish-1"`, `data-collection="users"`, `id="issueForm"`, `/orgs/assets/recoverystaff.js`, `maxlength="200"`, `id="open"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}

	// the page is a shell: it does not tell which slugs exist
	_, other := env.get("/orgs/recovery/no-such-parish")
	if strings.ReplaceAll(other, "no-such-parish", "parish-1") != body {
		t.Error("the page differs for an unknown slug")
	}

	if r, _ := env.get("/orgs/recovery/BAD_SLUG!"); r.StatusCode != http.StatusNotFound {
		t.Errorf("bad slug: %d", r.StatusCode)
	}

	// the other org pages still route
	if r, _ := env.get("/orgs/parish-1"); r.StatusCode != http.StatusOK {
		t.Errorf("/orgs/parish-1: %d", r.StatusCode)
	}

	if r, _ := env.get("/orgs/assets/hosted.css"); r.StatusCode != http.StatusOK {
		t.Errorf("assets: %d", r.StatusCode)
	}
}

func TestRecoveryAssetsAndMarkup(t *testing.T) {
	t.Parallel()

	env := newRecoveryEnv(t)

	for _, c := range []struct{ page, script string }{{"/recovery", "recovery.js"}, {"/orgs/recovery/parish-1", "recoverystaff.js"}} {
		res, js := env.get("/orgs/assets/" + c.script)
		if res.StatusCode != http.StatusOK || !strings.Contains(res.Header.Get("Content-Type"), "javascript") || len(js) < 500 {
			t.Errorf("%s: %d %s", c.script, res.StatusCode, res.Header.Get("Content-Type"))
		}

		// never parse strings as HTML; the code and the token live in the page only
		for _, bad := range []string{"innerHTML", "eval(", "document.write", "localStorage", "document.cookie"} {
			if strings.Contains(js, bad) {
				t.Errorf("%s uses %s", c.script, bad)
			}
		}

		// every element the script looks up exists in its page
		_, html := env.get(c.page)
		have := map[string]bool{}

		for _, m := range idRe.FindAllStringSubmatch(html, -1) {
			have[m[1]] = true
		}

		for _, m := range jsIDRe.FindAllStringSubmatch(js, -1) {
			if !have[m[1]] {
				t.Errorf("%s uses #%s which %s does not have", c.script, m[1], c.page)
			}
		}
	}

	// the person's page never touches a staff session
	if _, js := env.get("/orgs/assets/recovery.js"); strings.Contains(js, "session.") {
		t.Error("the person's page must not use the staff session")
	}
}
