package cmd_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/cmd"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// Fork-local (Claviger #80): claviger households repair-copies.
func TestHouseholdsRepairCopiesCommand(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	orgs, _ := app.FindCollectionByNameOrId(core.CollectionNameOrgs)
	org := core.NewRecord(orgs)
	org.Set("name", "aa")
	org.Set("slug", "aa")

	if err := app.Save(org); err != nil {
		t.Fatal(err)
	}

	hCol, _ := app.FindCollectionByNameOrId(core.CollectionNameHouseholds)
	mCol, _ := app.FindCollectionByNameOrId(core.CollectionNameHouseholdMembers)

	mk := func(name string) *core.Record {
		h := core.NewRecord(hCol)
		h.Set("org", org.Id)
		h.Set("name", name)
		h.Set("status", "active")

		if err := app.Save(h); err != nil {
			t.Fatal(err)
		}

		return h
	}
	h1, h2 := mk("one"), mk("two")

	m := core.NewRecord(mCol)
	m.Set("household", h1.Id)
	m.Set("name", "Pat")
	m.Set("status", "active")

	if err := app.Save(m); err != nil {
		t.Fatal(err)
	}

	notes := core.NewBaseCollection("hh_notes")
	notes.Fields.Add(
		&core.RelationField{Name: "household", CollectionId: hCol.Id, MaxSelect: 1},
		&core.RelationField{Name: "member", CollectionId: mCol.Id, MaxSelect: 1},
	)

	if err := app.Save(notes); err != nil {
		t.Fatal(err)
	}

	n := core.NewRecord(notes)
	n.Set("member", m.Id)
	n.Set("household", h2.Id) // drifted

	if err := app.Save(n); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) string {
		t.Helper()

		var out bytes.Buffer

		c := cmd.NewHouseholdsCommand(app)
		c.SetOut(&out)
		c.SetArgs(args)

		if err := c.Execute(); err != nil {
			t.Fatal(err)
		}

		return out.String()
	}

	// the validation hook is not bound to hh_notes, so the drift above was stored as set
	if out := run("repair-copies", "--dry-run"); !strings.Contains(out, "hh_notes: 1 drifted") || !strings.Contains(out, "dry run") {
		t.Fatalf("dry run: %s", out)
	}

	if out := run("repair-copies"); !strings.Contains(out, "repaired") {
		t.Fatalf("repair: %s", out)
	}

	if got, _ := app.FindRecordById("hh_notes", n.Id); got.GetString("household") != h1.Id {
		t.Fatal("not repaired")
	}

	if out := run("repair-copies"); !strings.Contains(out, "consistent") {
		t.Fatalf("second run: %s", out)
	}
}
