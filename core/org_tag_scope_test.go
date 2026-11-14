package core_test

import (
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #84): a role tag only counts when its _orgRoles row
// belongs to the org of the membership. The API and the model hook refuse a
// tag of another org, so the row is written here without hooks, as a bad
// import or a direct write to the database would.
func TestOrgTagFromAnotherOrgGrantsNothing(t *testing.T) {
	w := newHHWorld(t)
	app := w.f.app

	var other *core.Record

	orgs, _ := app.FindAllRecords(core.CollectionNameOrgs)
	for _, o := range orgs {
		if o.Id != w.P.Id {
			other = o

			break
		}
	}

	rolesCol, _ := app.FindCollectionByNameOrId(core.CollectionNameOrgRoles)
	foreign := core.NewRecord(rolesCol)
	foreign.Set("org", other.Id)
	foreign.Set("name", core.RegistrarTag)
	mustSave(t, app, foreign)

	membersCol, _ := app.FindCollectionByNameOrId(core.CollectionNameOrgMembers)
	m := core.NewRecord(membersCol)
	m.Set("org", w.P.Id)
	m.Set("user", w.users["gwen"].Id)
	m.Set("role", "member")
	m.Set("status", "active")
	m.Set("tags", []string{foreign.Id})

	// the hook refuses it
	if err := app.Save(m); err == nil {
		t.Fatal("the hook should refuse a tag of another org")
	}

	// written without hooks it is stored, and must still grant nothing
	if err := app.UnsafeWithoutHooks().Save(m); err != nil {
		t.Fatal(err)
	}

	if names := core.OrgTagNames(app, m); len(names) != 0 {
		t.Fatalf("a tag of another org counted: %v", names)
	}

	if a := core.NewHouseholdActor(app, w.P, nil, w.users["gwen"]); a.Staff {
		t.Error("registrar of another org is staff of P")
	}

	if err := core.CheckOrgConsentPurposeAccess(app, w.P, w.users["gwen"]); err == nil {
		t.Error("registrar of another org can manage consent purposes of P")
	}

	// the tag of its own org still counts
	own, _ := app.FindFirstRecordByFilter(core.CollectionNameOrgRoles, "org={:o} && name='registrar'", map[string]any{"o": w.P.Id})
	m.Set("tags", []string{own.Id, foreign.Id})
	mustSave(t, app.UnsafeWithoutHooks(), m)

	if names := core.OrgTagNames(app, m); len(names) != 1 || names[0] != core.RegistrarTag {
		t.Fatalf("own tag: %v", names)
	}
}

// The rules printed before the tag's org was checked are rewritten, anything
// else is left alone, and a second run changes nothing.
func TestUpgradeOrgTagRules(t *testing.T) {
	w := newHHWorld(t)
	app := w.f.app

	orgs, _ := app.FindCollectionByNameOrId(core.CollectionNameOrgs)

	const memberPrefix = `org.archived = false && org._orgMembers_via_org.user ?= @request.auth.id && org._orgMembers_via_org.status ?= "active"`

	legacyTag := memberPrefix + ` && org._orgMembers_via_org.tags.name ?= "youth"`
	legacyAnc := `org.archived = false && @collection._orgClosure:k.descendant ?= org && @collection._orgMembers:m.org ?= @collection._orgClosure:k.ancestor && ` +
		`@collection._orgMembers:m.user ?= @request.auth.id && @collection._orgMembers:m.status ?= "active" && ` +
		`(@collection._orgMembers:m.role ?= "admin" || @collection._orgMembers:m.role ?= "owner") && @collection._orgClosure:k.depth ?<= 2 && ` +
		`@collection._orgMembers:m.tags.name ?= "safeguarding"`
	edited := legacyTag + ` && org.name != ""`

	mk := func(name string, rule string) {
		c := core.NewBaseCollection(name)
		c.Fields.Add(&core.RelationField{Name: "org", CollectionId: orgs.Id, MaxSelect: 1})
		c.ListRule, c.ViewRule = &rule, &rule
		mustSave(t, app, c)
	}
	mk("tagged", legacyTag)
	mk("ancestor", legacyAnc)
	mk("edited", edited)

	// the system household rules, as migration 1800 wrote them
	oldH := strings.ReplaceAll(core.HouseholdStaffRule("org"), ` && org._orgMembers_via_org.tags.org ?= org._orgMembers_via_org.org`, "")
	if oldH == core.HouseholdStaffRule("org") {
		t.Fatal("the staff rule has no tag scope")
	}

	hh, _ := app.FindCollectionByNameOrId(core.CollectionNameHouseholds)
	hRule, _ := core.HouseholdRules()
	legacyHouseholds := strings.ReplaceAll(hRule, core.HouseholdStaffRule("org"), oldH)
	hh.ListRule, hh.ViewRule = &legacyHouseholds, &legacyHouseholds
	if err := app.SaveNoValidate(hh); err != nil {
		t.Fatal(err)
	}

	// and _credentials, as migration 1600 wrote it
	cc, err := app.FindCollectionByNameOrId(core.CollectionNameCredentials)
	if err != nil {
		t.Fatal(err)
	}

	credNew := *cc.ListRule
	credOld := strings.ReplaceAll(credNew, ` && @collection._orgMembers:m.tags.org ?= @collection._orgMembers:m.org`, "")

	if credOld == credNew {
		t.Fatal("the credentials rule has no tag scope")
	}

	cc.ListRule, cc.ViewRule = &credOld, &credOld
	if err := app.SaveNoValidate(cc); err != nil {
		t.Fatal(err)
	}

	changes, err := core.UpgradeOrgTagRules(app)
	if err != nil {
		t.Fatal(err)
	}

	byCol := map[string]string{}
	for _, c := range changes {
		byCol[c.Collection+"."+c.Rule] = c.New
	}

	wantTag, _ := core.OrgTagRule("org", "youth")
	wantAnc, _ := core.OrgAncestorRule("org", core.OrgAncestorOpts{Roles: []string{"admin", "owner"}, MaxDepth: 2, Tag: "safeguarding"})

	if byCol["tagged.listRule"] != wantTag || byCol["tagged.viewRule"] != wantTag {
		t.Errorf("tag rule: %q", byCol["tagged.listRule"])
	}

	if byCol["ancestor.listRule"] != wantAnc {
		t.Errorf("ancestor rule: %q", byCol["ancestor.listRule"])
	}

	if byCol["_households.listRule"] != hRule {
		t.Errorf("household rule: %q", byCol["_households.listRule"])
	}

	if byCol["_credentials.listRule"] != credNew || byCol["_credentials.viewRule"] != credNew {
		t.Errorf("credentials rule: %q", byCol["_credentials.listRule"])
	}

	for k := range byCol {
		if strings.HasPrefix(k, "edited.") {
			t.Errorf("an edited rule was rewritten: %s", k)
		}
	}

	got, _ := app.FindCollectionByNameOrId("edited")
	if got.ListRule == nil || *got.ListRule != edited {
		t.Error("the edited rule changed")
	}

	got, _ = app.FindCollectionByNameOrId("tagged")
	if got.ListRule == nil || *got.ListRule != wantTag {
		t.Error("the tag rule was not saved")
	}

	again, err := core.UpgradeOrgTagRules(app)
	if err != nil || len(again) != 0 {
		t.Errorf("second run: %v %v", again, err)
	}
}
