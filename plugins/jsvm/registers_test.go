package jsvm

import (
	"context"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
	"github.com/dop251/goja"
)

// Fork-local (Claviger #87): the guard of the permanent registers sits at the
// model layer, so scripts cannot get around it either: $app.save and
// $app.delete in a pb_hooks script, and a JS model hook that tries to change a
// final entry.
func TestRegistersHoldForJSHooks(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	orgs, _ := app.FindCollectionByNameOrId(core.CollectionNameOrgs)
	org := core.NewRecord(orgs)
	org.Set("name", "Parish")
	org.Set("slug", "parish-js")

	if err := app.Save(org); err != nil {
		t.Fatal(err)
	}

	if _, err := core.CreateSacramentalRegister(app, core.SacramentalRegisterOpts{Name: "baptisms"}); err != nil {
		t.Fatal(err)
	}

	col, _ := app.FindCollectionByNameOrId("baptisms")

	mk := func(surname string) *core.Record {
		r := core.NewRecord(col)
		r.Set("org", org.Id)
		r.Set("sacrament", "baptism")
		r.Set("surname", surname)

		if err := app.Save(r); err != nil {
			t.Fatal(err)
		}

		return r
	}

	final, draft := mk("Final"), mk("Draft")

	// Only a registrar of the org can finalize (superusers never sign).
	users, _ := app.FindCollectionByNameOrId("users")
	reg := core.NewRecord(users)
	reg.SetEmail("registrar@js.test")
	reg.SetPassword("password123")
	reg.Set("username", "js_registrar")

	if err := app.Save(reg); err != nil {
		t.Fatal(err)
	}

	roles, _ := app.FindCollectionByNameOrId(core.CollectionNameOrgRoles)
	tag := core.NewRecord(roles)
	tag.Set("org", org.Id)
	tag.Set("name", core.RegistrarTag)

	if err := app.Save(tag); err != nil {
		t.Fatal(err)
	}

	members, _ := app.FindCollectionByNameOrId(core.CollectionNameOrgMembers)
	m := core.NewRecord(members)
	m.Set("org", org.Id)
	m.Set("user", reg.Id)
	m.Set("role", "member")
	m.Set("status", "active")
	m.Set("tags", []string{tag.Id})

	if err := app.Save(m); err != nil {
		t.Fatal(err)
	}

	if _, err := core.FinalizeRegisterEntry(app, context.Background(), reg, "baptisms", final.Id); err != nil {
		t.Fatal(err)
	}

	vmFactory := func() *goja.Runtime {
		vm := goja.New()
		vm.SetFieldNameMapper(FieldMapper{})
		BindCore(vm)
		vm.Set("$app", app)
		vm.Set("finalId", final.Id)
		vm.Set("draftId", draft.Id)

		return vm
	}

	pool := newPool(1, vmFactory)
	vm := vmFactory()
	hooksBinds(app, vm, pool)

	run := func(script string) string {
		t.Helper()

		v, err := vm.RunString(`(function () {
			try { ` + script + ` return "ok" } catch (e) { return "error: " + e }
		})()`)
		if err != nil {
			t.Fatal(err)
		}

		return v.String()
	}

	// $app.save / $app.delete of a final entry
	if got := run(`const r = $app.findRecordById("baptisms", finalId); r.set("surname", "Hacked"); $app.save(r);`); !strings.HasPrefix(got, "error") {
		t.Fatalf("$app.save of a final entry: %s", got)
	}

	if got := run(`const r = $app.findRecordById("baptisms", finalId); r.set("finalizedAt", ""); $app.save(r);`); !strings.HasPrefix(got, "error") {
		t.Fatalf("clearing finalizedAt: %s", got)
	}

	if got := run(`const r = $app.findRecordById("baptisms", finalId); $app.delete(r);`); !strings.HasPrefix(got, "error") {
		t.Fatalf("$app.delete of a final entry: %s", got)
	}

	// a JS hook that edits the entry during an allowed save does not get through either
	if _, err := vm.RunString(`onRecordUpdate((e) => { e.record.set("surname", "FromHook"); e.next() }, "baptisms")`); err != nil {
		t.Fatal(err)
	}

	if got := run(`const r = $app.findRecordById("baptisms", finalId); r.set("restricted", true); $app.save(r);`); !strings.HasPrefix(got, "error") {
		t.Fatalf("a hook changing a final entry: %s", got)
	}

	// drafts stay editable from scripts
	if got := run(`const r = $app.findRecordById("baptisms", draftId); r.set("givenNames", "Ann"); $app.save(r);`); got != "ok" {
		// the hook above also edits drafts; that is allowed
		t.Fatalf("a draft must stay editable: %s", got)
	}

	if got := run(`const r = $app.findRecordById("baptisms", draftId); $app.delete(r);`); got != "ok" {
		t.Fatalf("a draft must stay deletable: %s", got)
	}

	stored, _ := app.FindRecordById("baptisms", final.Id)
	if stored.GetString("surname") != "Final" || stored.GetBool("restricted") {
		t.Fatalf("the final entry changed: %q restricted=%v", stored.GetString("surname"), stored.GetBool("restricted"))
	}
}
