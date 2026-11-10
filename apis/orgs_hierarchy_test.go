package apis_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// Fork-local (Claviger #84, 84-3 and 84-5): kind labels, the hierarchy config,
// the rule snippet route and the counts-only status route.

func withKinds(t testing.TB, app *tests.TestApp) {
	t.Helper()

	for id, kind := range map[string]string{orgD: "region", orgP1: "congregation", orgP2: "congregation"} {
		o, err := app.FindRecordById(core.CollectionNameOrgs, id)
		orgMust(t, err)
		o.Set("kind", kind)
		orgMust(t, app.Save(o))
	}
}

func TestOrgKindRoutes(t *testing.T) {
	t.Parallel()

	runOrgCases(t, []orgCase{
		{name: "superuser creates with a kind", who: "super", method: http.MethodPost, url: "/api/orgs",
			body: `{"name":"Region North","kind":" region "}`, status: 200, has: []string{`"kind":"region"`}},
		{name: "kind is optional", who: "super", method: http.MethodPost, url: "/api/orgs",
			body: `{"name":"No Kind"}`, status: 200, has: []string{`"kind":""`}},
		{name: "kind too long", who: "super", method: http.MethodPost, url: "/api/orgs",
			body: `{"name":"Long","kind":"` + strings.Repeat("k", 65) + `"}`, status: 400},
		{name: "kind with a control character", who: "super", method: http.MethodPost, url: "/api/orgs",
			body: `{"name":"Ctl","kind":"a\nb"}`, status: 400},
		{name: "admin sets the kind", who: "bob", method: http.MethodPatch, url: "/api/orgs/" + orgP1,
			body: `{"kind":"ministry"}`, status: 200, has: []string{`"kind":"ministry"`}},
		{name: "member cannot", who: "carol", method: http.MethodPatch, url: "/api/orgs/" + orgP1,
			body: `{"kind":"ministry"}`, status: 403},
		{name: "kind is listed", who: "alice", url: "/api/orgs", status: 200, before: withKinds,
			has: []string{`"kind":"congregation"`}},
	})
}

func TestOrgsConfig(t *testing.T) {
	t.Setenv(core.OrgMaxDepthEnv, "6")

	runOrgCases(t, []orgCase{
		{name: "guest", url: "/api/orgs/config", status: 401},
		{name: "user", who: "alice", url: "/api/orgs/config", status: 403},
		{name: "superuser sees the effective depth and kinds", who: "super", url: "/api/orgs/config", status: 200, before: withKinds,
			has: []string{`"maxDepth":6`, `"hardMaxDepth":8`, `"defaultDepth":4`, `"envVar":"CLAVIGER_ORGS_MAX_DEPTH"`, `"kinds":["congregation","region"]`}},
	})
}

func TestOrgsConfigClampsDepth(t *testing.T) {
	t.Setenv(core.OrgMaxDepthEnv, "99")

	runOrgCases(t, []orgCase{
		{name: "clamped to the hard cap", who: "super", url: "/api/orgs/config", status: 200, has: []string{`"maxDepth":8`}},
	})
}

func TestOrgAncestorRuleRoute(t *testing.T) {
	t.Parallel()

	// the route must return exactly what core.OrgAncestorRule renders
	want := func(field string, opts core.OrgAncestorOpts) string {
		r, err := core.OrgAncestorRule(field, opts)
		orgMust(t, err)

		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		orgMust(t, enc.Encode(r))

		return `"rule":` + strings.TrimSpace(buf.String())
	}

	q := func(kv ...string) string {
		v := url.Values{}
		for i := 0; i < len(kv); i += 2 {
			v.Set(kv[i], kv[i+1])
		}

		return "/api/orgs/ancestor-rule?" + v.Encode()
	}

	runOrgCases(t, []orgCase{
		{name: "guest", url: q(), status: 401},
		{name: "user", who: "alice", url: q(), status: 403},
		{name: "defaults", who: "super", url: q(), status: 200, has: []string{want("org", core.OrgAncestorOpts{})}},
		{name: "roles and depth", who: "super", url: q("field", "church", "roles", "admin, owner", "maxDepth", "2"), status: 200,
			has: []string{want("church", core.OrgAncestorOpts{Roles: []string{"admin", "owner"}, MaxDepth: 2})}},
		{name: "tag form", who: "super", url: q("roles", "member", "tag", "safeguarding"), status: 200,
			has: []string{want("org", core.OrgAncestorOpts{Roles: []string{"member"}, Tag: "safeguarding"})}},
		{name: "the full set is returned", who: "super", url: q("maxDepth", "1"), status: 200, has: []string{`"ruleSet":`, `"listRule":`}},
		{name: "bad role", who: "super", url: q("roles", "pope"), status: 400},
		{name: "bad depth", who: "super", url: q("maxDepth", "9"), status: 400},
		{name: "depth not a number", who: "super", url: q("maxDepth", "x"), status: 400},
		{name: "bad tag", who: "super", url: q("tag", `x" || 1=1`), status: 400},
	})
}

func TestOrgStatus(t *testing.T) {
	t.Parallel()

	personal := []string{"@orgs.test", "alice", "gina", "Parish One"}

	runOrgCases(t, []orgCase{
		{name: "guest", url: "/api/orgs/" + orgP1 + "/status", status: 401},
		{name: "member is refused", who: "carol", url: "/api/orgs/" + orgP1 + "/status", status: 403},
		{name: "viewer is refused", who: "dave", url: "/api/orgs/" + orgP1 + "/status", status: 403},
		{name: "outsider gets a 404", who: "frank", url: "/api/orgs/" + orgP1 + "/status", status: 404},
		{name: "suspended member gets a 404", who: "sam", url: "/api/orgs/" + orgP1 + "/status", status: 404},
		{name: "admin of the child cannot read the parent", who: "bob", url: "/api/orgs/" + orgD + "/status", status: 404},
		{name: "admin sees counts of a leaf", who: "bob", url: "/api/orgs/" + orgP1 + "/status", status: 200, before: withKinds,
			has:    []string{`"level":2`, `"height":0`, `"descendants":0`, `"descendantsByKind":[]`, `"members":{"active":4,"suspended":1}`},
			hasNot: personal},
		{name: "owner by slug", who: "alice", url: "/api/orgs/parish-1/status", status: 200, has: []string{`"level":2`}},
		{name: "admin of the diocese counts the tree", who: "gina", url: "/api/orgs/" + orgD + "/status", status: 200, before: withKinds,
			has:    []string{`"level":1`, `"height":1`, `"descendants":1`, `"descendantsByKind":[{"kind":"congregation","count":1}]`, `"members":{"active":1,"suspended":0}`},
			hasNot: personal},
		{name: "superuser", who: "super", url: "/api/orgs/" + orgD + "/status", status: 200, has: []string{`"maxDepth":4`}},
		{name: "unknown org", who: "super", url: "/api/orgs/nope/status", status: 404},
		{name: "unlabeled descendants have an empty kind", who: "gina", url: "/api/orgs/" + orgD + "/status", status: 200,
			has: []string{`"descendantsByKind":[{"kind":"","count":1}]`}},
		{name: "archived descendants are counted apart", who: "gina", url: "/api/orgs/" + orgD + "/status", status: 200,
			before: func(t testing.TB, app *tests.TestApp) {
				o, err := app.FindRecordById(core.CollectionNameOrgs, orgP1)
				orgMust(t, err)
				o.Set("archived", true)
				orgMust(t, app.Save(o))
			},
			has: []string{`"descendants":0`, `"archivedDescendants":1`}},
	})
}
