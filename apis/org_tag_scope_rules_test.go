package apis_test

import (
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// Fork-local (Claviger #84): the collection rules generated from a role tag
// (OrgTagRule, OrgAncestorRule with a tag, HouseholdStaffRule) only count a
// tag whose _orgRoles row belongs to the org of the membership. The API and
// the model hook refuse a tag of another org, so the rows here are written
// without hooks, as a bad import or a direct database write would.
//
//	erin  (member of P1): only tags of P2, named "registrar" and "youth"
//	ivan  (member of P1): the same two, PLUS a tag of P1 with another name
//	carol (member of P1): the real P1 "youth" and "registrar" tags
//
// ivan is the case a naive `tags.name ?= x && tags.org ?= y` could get wrong
// (two different tags satisfying the two conditions); the rules must grant
// nothing to him either.

func tagScopeFactory(t testing.TB) *tests.TestApp {
	app := hhFactory(t)

	roles, err := app.FindCollectionByNameOrId(core.CollectionNameOrgRoles)
	orgMust(t, err)

	mk := func(id, org, name string) {
		r := core.NewRecord(roles)
		r.Id = id
		r.Set("org", org)
		r.Set("name", name)
		orgMust(t, app.Save(r))
	}
	mk("foreignregistra", orgP2, core.HouseholdRegistrarTag)
	mk("foreignyouthaaa", orgP2, "youth")
	mk("ownchoiraaaaaaa", orgP1, "choir")

	setTags := func(who string, tags ...string) {
		m, err := core.FindOrgMember(app, orgP1, orgUserID(who))
		orgMust(t, err)
		m.Set("tags", tags)
		orgMust(t, app.UnsafeWithoutHooks().Save(m))
	}
	setTags("erin", "foreignregistra", "foreignyouthaaa")
	setTags("ivan", "foreignregistra", "foreignyouthaaa", "ownchoiraaaaaaa")

	// carol gets the real P1 youth tag next to her registrar tag
	youth, err := app.FindFirstRecordByFilter(core.CollectionNameOrgRoles, "org={:o} && name='youth'", map[string]any{"o": orgP1})
	orgMust(t, err)

	carol, err := core.FindOrgMember(app, orgP1, orgUserID("carol"))
	orgMust(t, err)

	have := false
	for _, id := range carol.GetStringSlice("tags") {
		have = have || id == youth.Id
	}

	if !have {
		carol.Set("tags", append(carol.GetStringSlice("tags"), youth.Id))
		orgMust(t, app.Save(carol))
	}

	tagRule, err := core.OrgTagRule("org", "youth")
	orgMust(t, err)

	ancRule, err := core.OrgAncestorRule("org", core.OrgAncestorOpts{Tag: "youth"})
	orgMust(t, err)

	seedDocs(t, app, "tagdocs", "org", false, core.OrgRuleSet{List: ptr(tagRule), View: ptr(tagRule)})
	seedDocs(t, app, "ancdocs", "org", false, core.OrgRuleSet{List: ptr(ancRule), View: ptr(ancRule)})

	return app
}

func tagScopeCase(name, who, url string, status int, has, hasNot []string) orgCase {
	return orgCase{name: name, who: who, url: url, status: status, has: has, hasNot: hasNot, factory: tagScopeFactory}
}

func TestOrgTagRulesIgnoreTagsOfOtherOrgs(t *testing.T) {
	hhKEK(t)

	none := []string{`"totalItems":0`}
	docs := func(c string) string { return "/api/collections/" + c + "/records?perPage=50" }
	view := func(c, id string) string { return "/api/collections/" + c + "/records/" + id }
	hh := "/api/collections/" + core.CollectionNameHouseholds + "/records?perPage=50"
	hm := "/api/collections/" + core.CollectionNameHouseholdMembers + "/records?perPage=50"

	var cases []orgCase

	for _, c := range []string{"tagdocs", "ancdocs"} {
		cases = append(cases,
			tagScopeCase(c+": own tag", "carol", docs(c), 200, []string{`"id":"` + docP1 + `"`}, []string{docP2}),
			tagScopeCase(c+": own tag, view", "carol", view(c, docP1), 200, []string{`"id":"` + docP1 + `"`}, nil),
			tagScopeCase(c+": foreign tag alone", "erin", docs(c), 200, none, nil),
			tagScopeCase(c+": foreign tag alone, view", "erin", view(c, docP1), 404, nil, nil),
			tagScopeCase(c+": foreign tag plus an own tag of another name", "ivan", docs(c), 200, none, nil),
			tagScopeCase(c+": foreign tag plus an own tag, view", "ivan", view(c, docP1), 404, nil, nil),
		)
	}

	cases = append(cases,
		// staff of the org: the households of P1 (two) and all six people
		tagScopeCase("households: real registrar", "carol", hh, 200, []string{`"totalItems":2`}, nil),
		tagScopeCase("households: foreign registrar alone", "erin", hh, 200, []string{`"totalItems":1`, `"name":"Erin family"`}, []string{"Dave family"}),
		tagScopeCase("households: foreign registrar plus an own tag", "ivan", hh, 200, []string{`"totalItems":1`}, []string{"Dave family"}),
		tagScopeCase("household people: real registrar", "carol", hm, 200, []string{`"totalItems":6`}, nil),
		tagScopeCase("household people: foreign registrar alone", "erin", hm, 200, []string{`"totalItems":4`}, []string{hhDave, hhKidB}),
		tagScopeCase("household people: foreign registrar plus an own tag", "ivan", hm, 200, []string{`"totalItems":4`}, []string{hhDave, hhKidB}),
		tagScopeCase("household person view: foreign registrar", "ivan", view(core.CollectionNameHouseholdMembers, hhDave), 404, nil, nil),
		tagScopeCase("household person view: real registrar", "carol", view(core.CollectionNameHouseholdMembers, hhDave), 200, []string{`"name":"Dave D"`}, nil),
	)

	runOrgCases(t, cases)
}
