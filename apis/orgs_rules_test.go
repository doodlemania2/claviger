package apis_test

import (
	"net/http"
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// Fork-local (Claviger #79a): the rule cookbook of docs/orgs.md, run against
// real org-owned collections. These tests are the guard for a sync that
// changes how back-relation joins or the multi-match operators behave: the
// whole design rests on same-row `_via_` joins.

func presetFactory(preset string, field string) func(t testing.TB) *tests.TestApp {
	return func(t testing.TB) *tests.TestApp {
		app := orgsFactory(t)

		for _, p := range core.OrgRulePresets(field) {
			if p.Key == preset {
				seedDocs(t, app, "docs", field, false, p.Rules)
			}
		}

		return app
	}
}

func listCase(preset, who string, want ...string) orgCase {
	all := []string{docP1, docP2, docA}
	c := orgCase{
		name: preset + " list as " + who, who: who, url: "/api/collections/docs/records?perPage=50",
		status: 200, factory: presetFactory(preset, "org"),
	}

	for _, id := range all {
		found := false
		for _, w := range want {
			found = found || w == id
		}

		if found {
			c.has = append(c.has, `"id":"`+id+`"`)
		} else {
			c.hasNot = append(c.hasNot, `"id":"`+id+`"`)
		}
	}

	if len(want) == 0 {
		c.has = append(c.has, `"totalItems":0`)
	}

	return c
}

func TestOrgRulesMembersPresetRead(t *testing.T) {
	t.Parallel()

	runOrgCases(t, []orgCase{
		listCase("members", "alice", docP1),
		listCase("members", "bob", docP1),
		listCase("members", "dave", docP1), // viewer reads
		listCase("members", "carol", docP1, docP2),
		listCase("members", "erin", docP2),
		listCase("members", "frank"),
		listCase("members", "gina"), // diocese admin has no parish membership
		listCase("members", "sam"),  // suspended
		listCase("members", "hank"), // owner of an archived org: nothing
		listCase("members", ""),
		listCase("members", "super", docP1, docP2, docA),
		{
			name: "view own org doc", who: "alice", url: "/api/collections/docs/records/" + docP1,
			status: 200, factory: presetFactory("members", "org"),
		},
		{
			name: "view other org doc", who: "alice", url: "/api/collections/docs/records/" + docP2,
			status: 404, factory: presetFactory("members", "org"),
		},
		{
			name: "view archived org doc as its owner", who: "hank", url: "/api/collections/docs/records/" + docA,
			status: 404, factory: presetFactory("members", "org"),
		},
	})
}

func updateCase(preset, who, doc, body string, status int) orgCase {
	return orgCase{
		name: preset + " update " + who + " " + doc + " " + body, who: who, method: http.MethodPatch,
		url: "/api/collections/docs/records/" + doc, body: body, status: status, factory: presetFactory(preset, "org"),
	}
}

func deleteCase(preset, who, doc string, status int) orgCase {
	return orgCase{
		name: preset + " delete " + who + " " + doc, who: who, method: http.MethodDelete,
		url: "/api/collections/docs/records/" + doc, status: status, factory: presetFactory(preset, "org"),
	}
}

func createCase(preset, who, org string, status int) orgCase {
	return orgCase{
		name: preset + " create " + who + " in " + org, who: who, method: http.MethodPost,
		url: "/api/collections/docs/records", body: `{"title":"n","org":"` + org + `"}`, status: status,
		factory: presetFactory(preset, "org"),
	}
}

func TestOrgRulesMembersPresetWrite(t *testing.T) {
	t.Parallel()

	title := `{"title":"x"}`

	runOrgCases(t, []orgCase{
		// update: owner, admin and member write; viewer, suspended and outsiders do not
		updateCase("members", "alice", docP1, title, 200),
		updateCase("members", "bob", docP1, title, 200),
		updateCase("members", "carol", docP1, title, 200),
		updateCase("members", "dave", docP1, title, 404),
		updateCase("members", "sam", docP1, title, 404),
		updateCase("members", "gina", docP1, title, 404),
		updateCase("members", "erin", docP1, title, 404),
		// same-row proof: carol is only a member of P1 but admin of P2; being admin
		// somewhere must not elevate her where she is a member (see the delete cases)
		updateCase("members", "carol", docP2, title, 200),
		updateCase("members", "hank", docA, title, 404), // archived org
		// moving a record
		updateCase("members", "carol", docP1, `{"org":"`+orgP2+`"}`, 200), // writer in both
		updateCase("members", "erin", docP2, `{"org":"`+orgP1+`"}`, 404),  // not a member of the target
		updateCase("members", "bob", docP1, `{"org":"`+orgP2+`"}`, 404),   // not a member of the target
		updateCase("members", "carol", docP1, `{"org":"`+orgA+`"}`, 404),  // not a member of the target
		updateCase("members", "erin", docP2, `{"title":"y"}`, 200),        // body without org is fine
		updateCase("members", "carol", docP1, `{"org":"`+orgP1+`"}`, 200), // staying put is fine

		// delete: owner and admin only
		deleteCase("members", "alice", docP1, 204),
		deleteCase("members", "bob", docP1, 204),
		deleteCase("members", "carol", docP1, 404), // member in P1 (admin in P2: same-row property)
		deleteCase("members", "carol", docP2, 204),
		deleteCase("members", "dave", docP1, 404),
		deleteCase("members", "erin", docP2, 404),
		deleteCase("members", "sam", docP1, 404),
		deleteCase("members", "hank", docA, 404),

		// create: the new record's own org decides
		createCase("members", "carol", orgP1, 200),
		createCase("members", "carol", orgP2, 200),
		createCase("members", "bob", orgP1, 200),
		createCase("members", "bob", orgP2, 400),
		createCase("members", "dave", orgP1, 400),
		createCase("members", "sam", orgP1, 400),
		createCase("members", "frank", orgP1, 400),
		createCase("members", "hank", orgA, 400),
		createCase("members", "", orgP1, 400),
		createCase("members", "super", orgP1, 200),
	})
}

func TestOrgRulesOtherPresets(t *testing.T) {
	t.Parallel()

	title := `{"title":"x"}`

	runOrgCases(t, []orgCase{
		// members-all: members delete too
		{
			name: "members-all delete as member", who: "carol", method: http.MethodDelete,
			url: "/api/collections/docs/records/" + docP1, status: 204, factory: presetFactory("members-all", "org"),
		},
		{
			name: "members-all delete as viewer", who: "dave", method: http.MethodDelete,
			url: "/api/collections/docs/records/" + docP1, status: 404, factory: presetFactory("members-all", "org"),
		},
		// admins: only owners and admins write
		updateCase("admins", "bob", docP1, title, 200),
		updateCase("admins", "alice", docP1, title, 200),
		updateCase("admins", "carol", docP1, title, 404),
		updateCase("admins", "carol", docP2, title, 200),
		updateCase("admins", "dave", docP1, title, 404),
		createCase("admins", "bob", orgP1, 200),
		createCase("admins", "carol", orgP1, 400),
		createCase("admins", "carol", orgP2, 200),
		deleteCase("admins", "bob", docP1, 204),
		deleteCase("admins", "carol", docP1, 404),
		listCase("admins", "dave", docP1), // reads stay open to every active member
		// readonly: nobody but superusers writes
		updateCase("readonly", "alice", docP1, title, 403),
		createCase("readonly", "alice", orgP1, 403),
		deleteCase("readonly", "alice", docP1, 403),
		listCase("readonly", "carol", docP1, docP2),
		createCase("readonly", "super", orgP1, 200),
	})
}

func customRulesFactory(field string, multi bool, rules core.OrgRuleSet) func(t testing.TB) *tests.TestApp {
	return func(t testing.TB) *tests.TestApp {
		app := orgsFactory(t)
		seedDocs(t, app, "docs", field, multi, rules)

		return app
	}
}

func TestOrgRulesTagsAndHierarchy(t *testing.T) {
	t.Parallel()

	tagRule, err := core.OrgTagRule("org", "youth")
	if err != nil {
		t.Fatal(err)
	}

	tagRules := core.OrgRuleSet{List: ptr(tagRule), View: ptr(tagRule)}
	parentRule := core.OrgParentRule("org")
	parentRules := core.OrgRuleSet{List: ptr(parentRule), View: ptr(parentRule)}
	parentAdmin := core.OrgParentRule("org", "owner", "admin")
	parentAdminRules := core.OrgRuleSet{List: ptr(parentAdmin), View: ptr(parentAdmin)}

	has := func(ids ...string) []string {
		out := []string{}
		for _, id := range ids {
			out = append(out, `"id":"`+id+`"`)
		}

		return out
	}

	runOrgCases(t, []orgCase{
		// tag gate: the tag is matched on the same membership row
		{name: "tag: tagged member", who: "carol", url: "/api/collections/docs/records", status: 200,
			has: has(docP1), hasNot: has(docP2), factory: customRulesFactory("org", false, tagRules)},
		{name: "tag: admin without the tag", who: "bob", url: "/api/collections/docs/records", status: 200,
			has: []string{`"totalItems":0`}, factory: customRulesFactory("org", false, tagRules)},
		{name: "tag: outsider", who: "erin", url: "/api/collections/docs/records", status: 200,
			has: []string{`"totalItems":0`}, factory: customRulesFactory("org", false, tagRules)},

		// one level up: members of the parent org reach the child org's records
		{name: "parent: diocese admin reads the parish doc", who: "gina", url: "/api/collections/docs/records", status: 200,
			has: has(docP1), hasNot: has(docP2, docA), factory: customRulesFactory("org", false, parentRules)},
		{name: "parent: parish viewer still reads", who: "dave", url: "/api/collections/docs/records", status: 200,
			has: has(docP1), factory: customRulesFactory("org", false, parentRules)},
		{name: "parent: other parish member sees its own only", who: "erin", url: "/api/collections/docs/records", status: 200,
			has: has(docP2), hasNot: has(docP1), factory: customRulesFactory("org", false, parentRules)},
		{name: "parent: stranger", who: "frank", url: "/api/collections/docs/records", status: 200,
			has: []string{`"totalItems":0`}, factory: customRulesFactory("org", false, parentRules)},

		// parent admins only
		{name: "parent admin only: diocese admin", who: "gina", url: "/api/collections/docs/records", status: 200,
			has: has(docP1), factory: customRulesFactory("org", false, parentAdminRules)},
		{name: "parent admin only: unrelated parish member keeps only own org", who: "erin", url: "/api/collections/docs/records", status: 200,
			has: has(docP2), hasNot: has(docP1), factory: customRulesFactory("org", false, parentAdminRules)},
		{name: "parent admin only: own org members keep their access", who: "dave", url: "/api/collections/docs/records", status: 200,
			has: has(docP1), factory: customRulesFactory("org", false, parentAdminRules)},
	})
}

func TestOrgRulesMultiOrgRecords(t *testing.T) {
	t.Parallel()

	rule := `orgs.archived ?= false && orgs._orgMembers_via_org.user ?= @request.auth.id && orgs._orgMembers_via_org.status ?= "active"`
	rules := core.OrgRuleSet{List: ptr(rule), View: ptr(rule)}

	runOrgCases(t, []orgCase{
		{name: "multi-org: P1 viewer", who: "dave", url: "/api/collections/docs/records", status: 200,
			has: []string{`"id":"` + docP1 + `"`}, hasNot: []string{`"id":"` + docP2 + `"`}, factory: customRulesFactory("orgs", true, rules)},
		{name: "multi-org: P2 member", who: "erin", url: "/api/collections/docs/records", status: 200,
			has: []string{`"id":"` + docP2 + `"`}, hasNot: []string{`"id":"` + docP1 + `"`}, factory: customRulesFactory("orgs", true, rules)},
		{name: "multi-org: outsider", who: "frank", url: "/api/collections/docs/records", status: 200,
			has: []string{`"totalItems":0`}, factory: customRulesFactory("orgs", true, rules)},
		{
			name: "multi-org record shared by two orgs", who: "erin", url: "/api/collections/docs/records", status: 200,
			has: []string{`"id":"` + docP1 + `"`, `"totalItems":2`},
			factory: func(t testing.TB) *tests.TestApp {
				app := customRulesFactory("orgs", true, rules)(t)
				r, err := app.FindRecordById("docs", docP1)
				orgMust(t, err)
				r.Set("orgs", []string{orgP1, orgP2})
				orgMust(t, app.Save(r))

				return app
			},
		},
	})
}

func TestOrgMembershipCollectionRules(t *testing.T) {
	t.Parallel()

	runOrgCases(t, []orgCase{
		{name: "members see co-members", who: "alice", url: "/api/collections/_orgMembers/records?perPage=50", status: 200,
			has:    []string{`"user":"` + orgUserID("bob") + `"`, `"user":"` + orgUserID("alice") + `"`},
			hasNot: []string{`"user":"` + orgUserID("erin") + `"`, `"user":"` + orgUserID("gina") + `"`}},
		{name: "a member of two orgs sees both", who: "carol", url: "/api/collections/_orgMembers/records?perPage=50", status: 200,
			has: []string{`"user":"` + orgUserID("erin") + `"`, `"user":"` + orgUserID("bob") + `"`}},
		{name: "outsiders see nothing", who: "frank", url: "/api/collections/_orgMembers/records", status: 200,
			has: []string{`"totalItems":0`}},
		{name: "suspended members see nothing", who: "sam", url: "/api/collections/_orgMembers/records", status: 200,
			has: []string{`"totalItems":0`}},
		{name: "owner of an archived org sees nothing", who: "hank", url: "/api/collections/_orgMembers/records", status: 200,
			has: []string{`"totalItems":0`}},
		{name: "guests see nothing", who: "", url: "/api/collections/_orgMembers/records", status: 200,
			has: []string{`"totalItems":0`}},
		{name: "members list their orgs", who: "carol", url: "/api/collections/_orgs/records", status: 200,
			has: []string{`"id":"` + orgP1 + `"`, `"id":"` + orgP2 + `"`}, hasNot: []string{`"id":"` + orgD + `"`}},
		{name: "archived orgs are hidden from members", who: "hank", url: "/api/collections/_orgs/records", status: 200,
			has: []string{`"totalItems":0`}},
		{name: "tags are visible to members", who: "dave", url: "/api/collections/_orgRoles/records", status: 200,
			has: []string{`"name":"youth"`}},
		{name: "tags are hidden from outsiders", who: "erin", url: "/api/collections/_orgRoles/records", status: 200,
			has: []string{`"totalItems":0`}},
		// no write rules: the records API is superuser only
		{name: "a member cannot add members through the records API", who: "bob", method: http.MethodPost,
			url: "/api/collections/_orgMembers/records", status: 403,
			body: `{"org":"` + orgP1 + `","user":"` + orgUserID("frank") + `","role":"member","status":"active"}`},
		{name: "an owner cannot edit members through the records API", who: "alice", method: http.MethodPatch,
			url: "/api/collections/_orgMembers/records/mem0000000000b", status: 403, body: `{"role":"owner"}`},
		{name: "an owner cannot delete members through the records API", who: "alice", method: http.MethodDelete,
			url: "/api/collections/_orgMembers/records/mem0000000000b", status: 403},
		{name: "a member cannot create orgs through the records API", who: "alice", method: http.MethodPost,
			url: "/api/collections/_orgs/records", status: 403, body: `{"name":"x","slug":"xx"}`},
	})
}
