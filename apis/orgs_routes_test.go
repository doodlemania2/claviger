package apis_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// Fork-local (Claviger #79a): /api/orgs routes and their guards.

func q(s string) string { return `"` + s + `"` }

func memberID(t testing.TB, app core.App, org, who string) string {
	t.Helper()

	m, err := core.FindOrgMember(app, org, orgUserID(who))
	if err != nil {
		t.Fatalf("no membership of %s in %s: %v", who, org, err)
	}

	return m.Id
}

func TestOrgsList(t *testing.T) {
	t.Parallel()

	runOrgCases(t, []orgCase{
		{name: "guest", url: "/api/orgs", status: 401},
		{name: "owner sees the org and the role", who: "alice", url: "/api/orgs", status: 200,
			has: []string{`"role":"owner"`, `"slug":"parish-1"`, `"totalItems":1`}, hasNot: []string{`parish-2`}},
		{name: "two memberships", who: "carol", url: "/api/orgs", status: 200,
			has: []string{`"totalItems":2`, `"slug":"parish-1"`, `"slug":"parish-2"`}},
		{name: "outsider", who: "frank", url: "/api/orgs", status: 200, has: []string{`"totalItems":0`}},
		{name: "suspended member", who: "sam", url: "/api/orgs", status: 200, has: []string{`"totalItems":0`}},
		{name: "archived orgs are hidden", who: "hank", url: "/api/orgs", status: 200, has: []string{`"totalItems":0`}},
		{name: "owner lists archived orgs on request", who: "hank", url: "/api/orgs?archived=true", status: 200,
			has: []string{`"slug":"closed"`, `"archived":true`}},
		{name: "superuser lists every live org", who: "super", url: "/api/orgs", status: 200,
			has: []string{`"totalItems":3`, `"slug":"diocese"`, `"slug":"parish-1"`}, hasNot: []string{`"slug":"closed"`}},
		{name: "superuser lists archived orgs", who: "super", url: "/api/orgs?archived=true", status: 200,
			has: []string{`"totalItems":1`, `"slug":"closed"`}},
		{name: "superuser search", who: "super", url: "/api/orgs?q=two", status: 200,
			has: []string{`"totalItems":1`, `"slug":"parish-2"`}},
		{name: "paging", who: "super", url: "/api/orgs?perPage=1&page=2", status: 200,
			has: []string{`"page":2`, `"perPage":1`, `"totalItems":3`}},
	})
}

func TestOrgsView(t *testing.T) {
	t.Parallel()

	runOrgCases(t, []orgCase{
		{name: "by id", who: "bob", url: "/api/orgs/" + orgP1, status: 200, has: []string{`"role":"admin"`, `"id":"` + orgP1 + `"`}},
		{name: "by slug", who: "bob", url: "/api/orgs/parish-1", status: 200, has: []string{`"id":"` + orgP1 + `"`}},
		{name: "outsiders get a 404", who: "frank", url: "/api/orgs/" + orgP1, status: 404},
		{name: "suspended members get a 404", who: "sam", url: "/api/orgs/" + orgP1, status: 404},
		{name: "other org members get a 404", who: "erin", url: "/api/orgs/" + orgP1, status: 404},
		{name: "guests", url: "/api/orgs/" + orgP1, status: 401},
		{name: "the owner of an archived org still sees it", who: "hank", url: "/api/orgs/" + orgA, status: 200, has: []string{`"archived":true`}},
		{name: "superuser", who: "super", url: "/api/orgs/" + orgA, status: 200},
		{name: "unknown", who: "super", url: "/api/orgs/nope", status: 404},
	})
}

func TestOrgsCreate(t *testing.T) {
	t.Parallel()

	runOrgCases(t, []orgCase{
		{name: "guest", method: http.MethodPost, url: "/api/orgs", body: `{"name":"New"}`, status: 401},
		{name: "users cannot create by default", who: "frank", method: http.MethodPost, url: "/api/orgs", body: `{"name":"New Church"}`, status: 403},
		{name: "superuser, slug from the name", who: "super", method: http.MethodPost, url: "/api/orgs",
			body: `{"name":"New Church"}`, status: 200, has: []string{`"slug":"new-church"`}},
		{name: "superuser with an owner by email", who: "super", method: http.MethodPost, url: "/api/orgs",
			body: `{"name":"New Church","owner":"frank@orgs.test"}`, status: 200,
			after: func(t testing.TB, app *tests.TestApp) {
				org, err := core.FindOrg(app, "new-church")
				orgMust(t, err)
				m, err := core.FindOrgMember(app, org.Id, orgUserID("frank"))
				orgMust(t, err)
				if m.GetString("role") != "owner" {
					t.Fatalf("expected owner, got %s", m.GetString("role"))
				}
			}},
		{name: "duplicate slug", who: "super", method: http.MethodPost, url: "/api/orgs", body: `{"name":"X","slug":"parish-1"}`, status: 409},
		{name: "invalid slug", who: "super", method: http.MethodPost, url: "/api/orgs", body: `{"name":"X","slug":"Bad Slug"}`, status: 400},
		{name: "missing name", who: "super", method: http.MethodPost, url: "/api/orgs", body: `{}`, status: 400},
		{name: "unknown owner", who: "super", method: http.MethodPost, url: "/api/orgs", body: `{"name":"Xylophone","owner":"nobody@x.test"}`, status: 404},
		{name: "parent", who: "super", method: http.MethodPost, url: "/api/orgs", body: `{"name":"Child","parent":"` + orgP2 + `"}`, status: 200,
			has: []string{`"parent":"` + orgP2 + `"`}},
		{name: "only one level of hierarchy", who: "super", method: http.MethodPost, url: "/api/orgs", body: `{"name":"Grandchild","parent":"` + orgP1 + `"}`, status: 400},
	})
}

func TestOrgsCreateEnabled(t *testing.T) {
	t.Setenv(core.OrgUsersCanCreateEnv, "true") // not parallel: the environment is process wide

	enable := orgsFactory

	runOrgCases(t, []orgCase{
		{name: "enabled: a user creates and becomes the owner", who: "frank", method: http.MethodPost, url: "/api/orgs",
			body: `{"name":"Frank Church"}`, status: 200, has: []string{`"role":"owner"`}, factory: enable,
			after: func(t testing.TB, app *tests.TestApp) {
				org, err := core.FindOrg(app, "frank-church")
				orgMust(t, err)
				if core.ActiveOrgMember(app, org, orgUserID("frank")) == nil {
					t.Fatal("the creator is not an active member")
				}
			}},
		{name: "enabled: a user cannot set a parent", who: "frank", method: http.MethodPost, url: "/api/orgs",
			body: `{"name":"Frank Church","parent":"` + orgP2 + `"}`, status: 403, factory: enable},
		{name: "enabled: a user cannot name another owner", who: "frank", method: http.MethodPost, url: "/api/orgs",
			body: `{"name":"Frank Church","owner":"alice@orgs.test"}`, status: 200, factory: enable,
			after: func(t testing.TB, app *tests.TestApp) {
				org, err := core.FindOrg(app, "frank-church")
				orgMust(t, err)
				if core.ActiveOrgMember(app, org, orgUserID("alice")) != nil {
					t.Fatal("alice must not become a member")
				}
			}},
	})
}

func TestOrgsUpdateArchiveDelete(t *testing.T) {
	t.Parallel()

	patch := func(name, who, id, body string, status int, has ...string) orgCase {
		return orgCase{name: name, who: who, method: http.MethodPatch, url: "/api/orgs/" + id, body: body, status: status, has: has}
	}

	runOrgCases(t, []orgCase{
		patch("admin renames", "bob", orgP1, `{"name":"Renamed"}`, 200, `"name":"Renamed"`),
		patch("admin changes the slug", "bob", orgP1, `{"slug":"renamed"}`, 200, `"slug":"renamed"`),
		patch("slug conflict", "bob", orgP1, `{"slug":"parish-2"}`, 409),
		patch("member cannot rename", "carol", orgP1, `{"name":"Nope"}`, 403),
		patch("outsider", "frank", orgP1, `{"name":"Nope"}`, 404),
		patch("admin cannot archive", "bob", orgP1, `{"archived":true}`, 403),
		patch("owner archives", "alice", orgP1, `{"archived":true}`, 200, `"archived":true`),
		patch("owner restores", "hank", orgA, `{"archived":false}`, 200, `"archived":false`),
		patch("archived orgs refuse other changes from owners", "hank", orgA, `{"name":"Nope"}`, 409),
		patch("admin cannot set the parent", "bob", orgP1, `{"parent":""}`, 403),
		patch("superuser sets the parent", "super", orgP2, `{"parent":"`+orgD+`"}`, 200),
		patch("an org with children cannot get a parent", "super", orgD, `{"parent":"`+orgP2+`"}`, 400),
		patch("an org cannot be its own parent", "super", orgP2, `{"parent":"`+orgP2+`"}`, 400),
		{
			name: "archiving hides the org from member rules", who: "alice", method: http.MethodPatch, url: "/api/orgs/" + orgP1,
			body: `{"archived":true}`, status: 200,
			after: func(t testing.TB, app *tests.TestApp) {
				recs, err := app.FindRecordsByFilter(core.CollectionNameOrgs, "archived = true", "", 0, 0)
				orgMust(t, err)
				if len(recs) != 2 {
					t.Fatalf("expected 2 archived orgs, got %d", len(recs))
				}
			},
		},

		{name: "delete needs an archived org", who: "alice", method: http.MethodDelete, url: "/api/orgs/" + orgP1,
			body: `{"confirm":"parish-1"}`, status: 409},
		{name: "delete needs the confirmation", who: "hank", method: http.MethodDelete, url: "/api/orgs/" + orgA, status: 400},
		{name: "delete with a wrong confirmation", who: "hank", method: http.MethodDelete, url: "/api/orgs/" + orgA,
			body: `{"confirm":"nope"}`, status: 400},
		{name: "owner deletes an archived org with its members and tags", who: "hank", method: http.MethodDelete,
			url: "/api/orgs/" + orgA, body: `{"confirm":"closed"}`, status: 204,
			after: func(t testing.TB, app *tests.TestApp) {
				if _, err := app.FindRecordById(core.CollectionNameOrgs, orgA); err == nil {
					t.Fatal("org still exists")
				}
				left, _ := app.FindRecordsByFilter(core.CollectionNameOrgMembers, "org={:o}", "", 0, 0, map[string]any{"o": orgA})
				if len(left) != 0 {
					t.Fatalf("expected the memberships to be gone, got %d", len(left))
				}
			}},
		{name: "superuser deletes an archived org", who: "super", method: http.MethodDelete,
			url: "/api/orgs/" + orgA, body: `{"confirm":"closed"}`, status: 204},
		{
			name: "an archived org that owns records cannot be deleted", who: "hank", method: http.MethodDelete,
			url: "/api/orgs/" + orgA, body: `{"confirm":"closed"}`, status: 409,
			factory: func(t testing.TB) *tests.TestApp {
				app := orgsFactory(t)
				seedDocs(t, app, "docs", "org", false, core.OrgRulePresets("org")[0].Rules)

				return app
			},
		},
		{name: "an org with child orgs cannot be deleted", who: "super", method: http.MethodDelete,
			url: "/api/orgs/" + orgD, body: `{"confirm":"diocese"}`, status: 409,
			factory: func(t testing.TB) *tests.TestApp {
				app := orgsFactory(t)
				d, err := app.FindRecordById(core.CollectionNameOrgs, orgD)
				orgMust(t, err)
				d.Set("archived", true)
				orgMust(t, app.Save(d))

				return app
			}},
		{name: "admins cannot delete", who: "bob", method: http.MethodDelete, url: "/api/orgs/" + orgP1, body: `{"confirm":"parish-1"}`, status: 403},
	})
}

func TestOrgMembersList(t *testing.T) {
	t.Parallel()

	runOrgCases(t, []orgCase{
		{name: "admins see every member with emails and tags", who: "bob", url: "/api/orgs/" + orgP1 + "/members", status: 200,
			has: []string{`"totalItems":5`, `"email":"alice@orgs.test"`, `"email":"sam@orgs.test"`, `"status":"suspended"`, `"tags":["youth"]`}},
		{name: "viewers see active members without emails", who: "dave", url: "/api/orgs/" + orgP1 + "/members", status: 200,
			has: []string{`"totalItems":4`, `"role":"owner"`}, hasNot: []string{`@orgs.test`, `"suspended"`}},
		{name: "filter by role", who: "bob", url: "/api/orgs/" + orgP1 + "/members?role=viewer", status: 200,
			has: []string{`"totalItems":1`, `"role":"viewer"`}},
		{name: "filter by status", who: "alice", url: "/api/orgs/" + orgP1 + "/members?status=suspended", status: 200,
			has: []string{`"totalItems":1`, `"email":"sam@orgs.test"`}},
		{name: "outsiders", who: "erin", url: "/api/orgs/" + orgP1 + "/members", status: 404},
		{name: "suspended members", who: "sam", url: "/api/orgs/" + orgP1 + "/members", status: 404},
		{name: "archived orgs are closed to their owners", who: "hank", url: "/api/orgs/" + orgA + "/members", status: 404},
		{name: "superuser", who: "super", url: "/api/orgs/" + orgA + "/members", status: 200, has: []string{`"totalItems":1`}},
		{name: "guest", url: "/api/orgs/" + orgP1 + "/members", status: 401},
	})
}

func TestOrgMemberGuards(t *testing.T) {
	t.Parallel()

	add := func(name, who, org, body string, status int, has ...string) orgCase {
		return orgCase{name: name, who: who, method: http.MethodPost, url: "/api/orgs/" + org + "/members", body: body, status: status, has: has}
	}

	runOrgCases(t, []orgCase{
		add("admin adds a member", "bob", orgP1, `{"user":"`+orgUserID("frank")+`"}`, 200, `"role":"member"`, `"status":"active"`),
		add("admin adds by email with tags", "bob", orgP1, `{"user":"frank@orgs.test","role":"viewer","tags":["choir","youth"]}`, 200,
			`"role":"viewer"`, `"tags":["choir","youth"]`),
		add("admin grants admin", "bob", orgP1, `{"user":"frank@orgs.test","role":"admin"}`, 200),
		add("admin cannot grant owner", "bob", orgP1, `{"user":"frank@orgs.test","role":"owner"}`, 403),
		add("owner grants owner", "alice", orgP1, `{"user":"frank@orgs.test","role":"owner"}`, 200, `"role":"owner"`),
		add("member cannot add", "carol", orgP1, `{"user":"frank@orgs.test"}`, 403),
		add("viewer cannot add", "dave", orgP1, `{"user":"frank@orgs.test"}`, 403),
		add("outsider gets a 404", "frank", orgP1, `{"user":"frank@orgs.test"}`, 404),
		add("admin of another org gets a 404", "erin", orgP1, `{"user":"frank@orgs.test"}`, 404),
		add("duplicate", "bob", orgP1, `{"user":"dave@orgs.test"}`, 409),
		add("unknown user", "bob", orgP1, `{"user":"ghost@orgs.test"}`, 404),
		add("missing user", "bob", orgP1, `{}`, 404),
		add("invalid role", "bob", orgP1, `{"user":"frank@orgs.test","role":"king"}`, 400),
		add("invalid status", "bob", orgP1, `{"user":"frank@orgs.test","status":"gone"}`, 400),
		add("invalid tag", "bob", orgP1, `{"user":"frank@orgs.test","tags":["a\"b"]}`, 400),
		add("added suspended", "bob", orgP1, `{"user":"frank@orgs.test","status":"suspended"}`, 200, `"status":"suspended"`),
		add("archived orgs refuse changes, even from owners", "hank", orgA, `{"user":"frank@orgs.test"}`, 409),
		add("superuser adds an owner", "super", orgP1, `{"user":"frank@orgs.test","role":"owner"}`, 200),
		add("guest", "", orgP1, `{"user":"frank@orgs.test"}`, 401),
	})
}

func TestOrgMemberUpdateGuards(t *testing.T) {
	t.Parallel()

	// the path needs the membership id, which depends on the seeded app, so
	// every case resolves it in a wrapper factory
	type uc struct {
		name, who, org, target, body string
		status                       int
		has                          []string
		before                       func(t testing.TB, app *tests.TestApp)
	}

	cases := []uc{
		{"admin demotes a viewer-level member", "bob", orgP1, "carol", `{"role":"viewer"}`, 200, []string{`"role":"viewer"`}, nil},
		{"admin promotes to admin", "bob", orgP1, "dave", `{"role":"admin"}`, 200, nil, nil},
		{"admin cannot promote to owner", "bob", orgP1, "dave", `{"role":"owner"}`, 403, nil, nil},
		{"admin cannot edit an owner", "bob", orgP1, "alice", `{"role":"member"}`, 403, nil, nil},
		{"admin cannot suspend an owner", "bob", orgP1, "alice", `{"status":"suspended"}`, 403, nil, nil},
		{"admin suspends a member", "bob", orgP1, "carol", `{"status":"suspended"}`, 200, []string{`"status":"suspended"`}, nil},
		{"admin reactivates a member", "bob", orgP1, "sam", `{"status":"active"}`, 200, []string{`"status":"active"`}, nil},
		{"admin sets tags", "bob", orgP1, "dave", `{"tags":["choir","usher"]}`, 200, []string{`"tags":["choir","usher"]`}, nil},
		{"admin clears tags", "bob", orgP1, "carol", `{"tags":[]}`, 200, []string{`"tags":[]`}, nil},
		{"admin cannot change their own status", "bob", orgP1, "bob", `{"status":"suspended"}`, 403, nil, nil},
		{"admin can lower themselves", "bob", orgP1, "bob", `{"role":"member"}`, 200, nil, nil},
		{"member cannot edit", "carol", orgP1, "dave", `{"role":"member"}`, 403, nil, nil},
		{"member cannot raise themselves", "carol", orgP1, "carol", `{"role":"admin"}`, 403, nil, nil},
		{"viewer cannot edit", "dave", orgP1, "carol", `{"role":"viewer"}`, 403, nil, nil},
		{"outsiders get a 404", "erin", orgP1, "carol", `{"role":"viewer"}`, 404, nil, nil},
		{"the last owner cannot be demoted", "alice", orgP1, "alice", `{"role":"admin"}`, 409, nil, nil},
		{"the last owner cannot be suspended by a superuser", "super", orgP1, "alice", `{"status":"suspended"}`, 409, nil, nil},
		{"the last owner cannot be demoted by a superuser", "super", orgP1, "alice", `{"role":"member"}`, 409, nil, nil},
		{"an owner can step down once another owner exists", "alice", orgP1, "alice", `{"role":"admin"}`, 200, nil, addOwner(orgP1, "frank")},
		{"an owner demotes a co-owner", "alice", orgP1, "=memcoowner00001", `{"role":"member"}`, 200, nil, addOwner(orgP1, "frank")},
		{"a suspended co-owner does not count", "alice", orgP1, "alice", `{"role":"member"}`, 409, nil, addSuspendedOwner(orgP1, "frank")},
		{"superuser edits an owner when others exist", "super", orgP1, "alice", `{"role":"member"}`, 200, nil, addOwner(orgP1, "frank")},
		{"invalid role", "bob", orgP1, "dave", `{"role":"king"}`, 400, nil, nil},
		{"invalid status", "bob", orgP1, "dave", `{"status":"gone"}`, 400, nil, nil},
		{"a member of another org is a 404", "bob", orgP1, "erin", `{"role":"viewer"}`, 404, nil, nil},
	}

	var out []orgCase
	for _, c := range cases {
		c := c
		out = append(out, orgCase{
			name: c.name, who: c.who, method: http.MethodPatch, body: c.body, status: c.status, has: c.has,
			url: memberURL(c.org, c.target),
			factory: func(t testing.TB) *tests.TestApp {
				app := orgsFactory(t)
				if c.before != nil {
					c.before(t, app)
				}

				return app
			},
		})
	}

	runOrgCasesResolved(t, out)
}

func memberURL(org, target string) string {
	if strings.HasPrefix(target, "=") {
		return "/api/orgs/" + org + "/members/" + target[1:]
	}

	return "/api/orgs/" + org + "/members/@" + target + "@" + org
}

func addOwner(org, who string) func(t testing.TB, app *tests.TestApp) {
	return func(t testing.TB, app *tests.TestApp) {
		col, _ := app.FindCollectionByNameOrId(core.CollectionNameOrgMembers)
		r := core.NewRecord(col)
		r.Id = "memcoowner00001"
		r.Set("org", org)
		r.Set("user", orgUserID(who))
		r.Set("role", "owner")
		r.Set("status", "active")
		orgMust(t, app.Save(r))
	}
}

func addSuspendedOwner(org, who string) func(t testing.TB, app *tests.TestApp) {
	return func(t testing.TB, app *tests.TestApp) {
		col, _ := app.FindCollectionByNameOrId(core.CollectionNameOrgMembers)
		r := core.NewRecord(col)
		r.Id = "memcoowner00001"
		r.Set("org", org)
		r.Set("user", orgUserID(who))
		r.Set("role", "owner")
		r.Set("status", "suspended")
		orgMust(t, app.Save(r))
	}
}

func TestOrgMemberRemoveGuards(t *testing.T) {
	t.Parallel()

	type rc struct {
		name, who, org, target string
		status                 int
		before                 func(t testing.TB, app *tests.TestApp)
	}

	cases := []rc{
		{"admin removes a viewer", "bob", orgP1, "dave", 204, nil},
		{"admin removes a suspended member", "bob", orgP1, "sam", 204, nil},
		{"admin cannot remove an owner", "bob", orgP1, "alice", 403, nil},
		{"owner removes an admin", "alice", orgP1, "bob", 204, nil},
		{"member cannot remove others", "carol", orgP1, "dave", 403, nil},
		{"a member can leave", "carol", orgP1, "carol", 204, nil},
		{"a viewer can leave", "dave", orgP1, "dave", 204, nil},
		{"the last owner cannot leave", "alice", orgP1, "alice", 409, nil},
		{"the last owner cannot be removed by a superuser", "super", orgP1, "alice", 409, nil},
		{"an owner can leave once another owner exists", "alice", orgP1, "alice", 204, addOwner(orgP1, "frank")},
		{"outsiders get a 404", "erin", orgP1, "dave", 404, nil},
		{"superuser removes anyone but the last owner", "super", orgP1, "bob", 204, nil},
		{"a member of another org is a 404", "bob", orgP1, "erin", 404, nil},
	}

	var out []orgCase
	for _, c := range cases {
		c := c
		out = append(out, orgCase{
			name: c.name, who: c.who, method: http.MethodDelete, status: c.status,
			url: memberURL(c.org, c.target),
			factory: func(t testing.TB) *tests.TestApp {
				app := orgsFactory(t)
				if c.before != nil {
					c.before(t, app)
				}

				return app
			},
		})
	}

	runOrgCasesResolved(t, out)
}

// runOrgCasesResolved is runOrgCases for urls that carry "@who@org": the
// membership id of that person in that org, known only after seeding. The
// seeding is deterministic, so a throwaway app resolves the ids first.
func runOrgCasesResolved(t *testing.T, cases []orgCase) {
	t.Helper()

	probe, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Cleanup()

	seedOrgs(t, probe)

	for i := range cases {
		u := cases[i].url
		for {
			start := strings.Index(u, "/@")
			if start < 0 {
				break
			}

			rest := u[start+2:]
			who, rest2, _ := strings.Cut(rest, "@")
			org := rest2

			m, err := core.FindOrgMember(probe, org, orgUserID(who))
			if err != nil {
				// a person who is not a member (yet): use a plausible missing id
				u = u[:start] + "/missingmember00"

				break
			}

			u = u[:start] + "/" + m.Id
		}

		cases[i].url = u
	}

	runOrgCases(t, cases)
}
