package core_test

import (
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
