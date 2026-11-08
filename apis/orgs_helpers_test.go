package apis_test

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #79a): shared fixtures of the organization tests.
//
// People: alice owns P1, bob is admin of P1, carol is a member of P1 (tagged
// "youth") and admin of P2, dave is a viewer of P1, erin is a member of P2,
// frank belongs nowhere, gina is admin of the diocese D (parent of P1) only,
// sam is a suspended member of P1, hank owns the archived org A.

var orgPeople = []string{"alice", "bob", "carol", "dave", "erin", "frank", "gina", "sam", "hank", "ivan"}

const (
	orgD  = "orgdiocese00001"
	orgP1 = "orgparish100001"
	orgP2 = "orgparish200001"
	orgA  = "orgarchived0001"

	docP1 = "docparish1aaaaa"
	docP2 = "docparish2aaaaa"
	docA  = "docarchivedaaaa"
)

func orgUserID(name string) string {
	return (name + "xxxxxxxxxxxxxxx")[:15]
}

var (
	orgTokensOnce sync.Once
	orgTokens     map[string]string
)

// orgToken returns a stable auth token of a seeded user (users share the
// collection secret of the fixture and have a fixed tokenKey).
func orgToken(t testing.TB, name string) string {
	t.Helper()

	orgTokensOnce.Do(func() {
		app, err := tests.NewTestApp()
		if err != nil {
			t.Fatal(err)
		}
		defer app.Cleanup()

		users, _ := app.FindCollectionByNameOrId("users")
		orgTokens = map[string]string{}

		for _, n := range orgPeople {
			r := core.NewRecord(users)
			r.Id = orgUserID(n)
			r.Set("tokenKey", orgTokenKey(n))

			tok, err := r.NewAuthToken()
			if err != nil {
				t.Fatal(err)
			}

			orgTokens[n] = tok
		}
	})

	return orgTokens[name]
}

func orgTokenKey(name string) string {
	return strings.Repeat(name, 10)[:30] + "0123456789abcdefghij"
}

func orgAuth(t testing.TB, who string) map[string]string {
	switch who {
	case "":
		return nil
	case "super":
		return map[string]string{"Authorization": auditSuperuserToken}
	}

	return map[string]string{"Authorization": orgToken(t, who)}
}

func orgMust(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// seedOrgs creates the people, organizations and memberships.
func seedOrgs(t testing.TB, app core.App) {
	t.Helper()

	users, err := app.FindCollectionByNameOrId("users")
	orgMust(t, err)

	for _, n := range orgPeople {
		r := core.NewRecord(users)
		r.Id = orgUserID(n)
		r.SetEmail(n + "@orgs.test")
		r.SetVerified(true)
		r.Set("tokenKey", orgTokenKey(n))
		r.SetPassword("password123")
		orgMust(t, app.Save(r))
	}

	orgs, err := app.FindCollectionByNameOrId(core.CollectionNameOrgs)
	orgMust(t, err)

	mkOrg := func(id, name, slug, parent string, archived bool) {
		r := core.NewRecord(orgs)
		r.Id = id
		r.Set("name", name)
		r.Set("slug", slug)
		r.Set("parent", parent)
		r.Set("archived", archived)
		orgMust(t, app.Save(r))
	}
	mkOrg(orgD, "Diocese", "diocese", "", false)
	mkOrg(orgP1, "Parish One", "parish-1", orgD, false)
	mkOrg(orgP2, "Parish Two", "parish-2", "", false)
	mkOrg(orgA, "Closed Parish", "closed", "", true)

	roles, err := app.FindCollectionByNameOrId(core.CollectionNameOrgRoles)
	orgMust(t, err)

	youth := core.NewRecord(roles)
	youth.Id = "tagyouthp1aaaaa"
	youth.Set("org", orgP1)
	youth.Set("name", "youth")
	orgMust(t, app.Save(youth))

	members, err := app.FindCollectionByNameOrId(core.CollectionNameOrgMembers)
	orgMust(t, err)

	n := 0
	mk := func(org, who, role, status string, tags ...string) {
		n++
		r := core.NewRecord(members)
		r.Id = "mem" + strings.Repeat("0", 11) + string(rune('a'+n))
		r.Set("org", org)
		r.Set("user", orgUserID(who))
		r.Set("role", role)
		r.Set("status", status)
		r.Set("tags", tags)
		orgMust(t, app.Save(r))
	}
	mk(orgP1, "alice", "owner", "active")
	mk(orgP1, "bob", "admin", "active")
	mk(orgP1, "carol", "member", "active", youth.Id)
	mk(orgP1, "dave", "viewer", "active")
	mk(orgP2, "erin", "member", "active")
	mk(orgP2, "carol", "admin", "active")
	mk(orgD, "gina", "admin", "active")
	mk(orgP1, "sam", "member", "suspended")
	mk(orgA, "hank", "owner", "active")
	mk(orgP2, "ivan", "owner", "active")
}

func orgsFactory(t testing.TB) *tests.TestApp {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}

	seedOrgs(t, app)

	return app
}

// orgCase is one ApiScenario against the seeded app.
type orgCase struct {
	name    string
	who     string
	method  string
	url     string
	body    string
	status  int
	has     []string
	hasNot  []string
	before  func(t testing.TB, app *tests.TestApp)
	after   func(t testing.TB, app *tests.TestApp)
	factory func(t testing.TB) *tests.TestApp
}

func runOrgCases(t *testing.T, cases []orgCase) {
	t.Helper()

	for _, c := range cases {
		c := c

		var body io.Reader
		if c.body != "" {
			body = strings.NewReader(c.body)
		}

		if c.method == "" {
			c.method = http.MethodGet
		}

		factory := c.factory
		if factory == nil {
			factory = orgsFactory
		}

		s := tests.ApiScenario{
			Name:               c.name,
			Method:             c.method,
			URL:                c.url,
			Body:               body,
			Headers:            orgAuth(t, c.who),
			ExpectedStatus:     c.status,
			ExpectedContent:    c.has,
			NotExpectedContent: c.hasNot,
			TestAppFactory:     factory,
		}

		if len(c.has) == 0 && len(c.hasNot) == 0 && c.status != 204 {
			s.ExpectedContent = []string{`"`} // any JSON body
		}

		if c.before != nil {
			s.BeforeTestFunc = func(t testing.TB, app *tests.TestApp, _ *core.ServeEvent) { c.before(t, app) }
		}

		if c.after != nil {
			s.AfterTestFunc = func(t testing.TB, app *tests.TestApp, _ *http.Response) { c.after(t, app) }
		}

		s.Test(t)
	}
}

// seedDocs creates a base collection "name" with a required org relation and
// the given rules, plus one record per org (P1, P2, archived A).
func seedDocs(t testing.TB, app core.App, name string, field string, multi bool, rules core.OrgRuleSet) {
	t.Helper()

	orgs, err := app.FindCollectionByNameOrId(core.CollectionNameOrgs)
	orgMust(t, err)

	c := core.NewBaseCollection(name)
	c.Fields.Add(&core.TextField{Name: "title"})

	max := 1
	if multi {
		max = 5
	}

	c.Fields.Add(&core.RelationField{Name: field, CollectionId: orgs.Id, MaxSelect: max, Required: true})
	c.AddIndex("idx_"+name+"_"+field, false, field, "")
	c.ListRule, c.ViewRule, c.CreateRule, c.UpdateRule, c.DeleteRule = rules.List, rules.View, rules.Create, rules.Update, rules.Delete
	orgMust(t, app.Save(c))

	for id, org := range map[string]string{docP1: orgP1, docP2: orgP2, docA: orgA} {
		r := core.NewRecord(c)
		r.Id = id
		r.Set("title", "t-"+org)

		if multi {
			r.Set(field, []string{org})
		} else {
			r.Set(field, org)
		}

		orgMust(t, app.Save(r))
	}
}

func ptr(s string) *string { return types.Pointer(s) }
