package oidc_test

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/oidc"
)

func TestClientTemplatesParse(t *testing.T) {
	tpls, err := oidc.ClientTemplates()
	if err != nil {
		t.Fatal(err)
	}

	if len(tpls) < 4 {
		t.Fatalf("%d templates", len(tpls))
	}

	if tpls[len(tpls)-1].ID != "generic" {
		t.Fatalf("generic is not last: %s", tpls[len(tpls)-1].ID)
	}

	re := regexp.MustCompile(`\{([a-z][a-z0-9_]*)\}`)
	withPlaceholder := 0
	seen := map[string]bool{}

	for _, tpl := range tpls {
		if seen[tpl.ID] {
			t.Fatalf("duplicate id %s", tpl.ID)
		}
		seen[tpl.ID] = true

		if tpl.Verified {
			t.Fatalf("%s is marked verified; no template has been tested end to end", tpl.ID)
		}

		if len(tpl.Claims) == 0 {
			t.Fatalf("%s lists no claims", tpl.ID)
		}

		used := map[string]bool{}
		for _, uri := range tpl.RedirectURIs {
			if !strings.HasPrefix(uri, "https://") && !strings.HasPrefix(uri, "{") {
				t.Fatalf("%s: redirect %q is not https", tpl.ID, uri)
			}
			for _, m := range re.FindAllStringSubmatch(uri, -1) {
				used[m[1]] = true
			}
		}

		if len(used) > 0 {
			withPlaceholder++
		}

		for _, p := range tpl.Placeholders {
			if !used[p.Key] {
				t.Fatalf("%s: placeholder %s is never used", tpl.ID, p.Key)
			}
		}
	}

	if withPlaceholder == 0 {
		t.Fatal("no template has a placeholder")
	}
}

func TestClientTemplatesRoute(t *testing.T) {
	e := newEnv(t)

	// superusers only
	if rec := e.browser().do("GET", "/api/oidc/client-templates", nil, nil); rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("open to guests: %d", rec.Code)
	}

	userToken, _ := e.user().NewAuthToken()
	if rec := e.browser().do("GET", "/api/oidc/client-templates", map[string]string{"Authorization": userToken}, nil); rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
		t.Fatalf("open to users: %d", rec.Code)
	}

	rec := e.browser().do("GET", "/api/oidc/client-templates", map[string]string{"Authorization": e.super}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("superuser: %d %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	for _, want := range []string{`"id":"generic"`, `"id":"ministry-platform"`, `{identity_provider_id}`, `"claims"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("response lacks %s: %s", want, body)
		}
	}
}
