package core_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
	_ "github.com/doodlemania2/claviger/migrations"
)

// bootstrapFresh returns an app with only the system migrations applied, i.e.
// exactly the collections a new installation gets (no test fixtures).
func bootstrapFresh(t testing.TB) core.App {
	t.Helper()
	app := core.NewBaseApp(core.BaseAppConfig{DataDir: t.TempDir()})
	if err := app.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.ResetBootstrapState() })
	return app
}

// The CI check of #88: every system collection must have a _dataMap entry
// ("none" is an entry). When this fails, add one line to builtinDataMap in
// core/data_map_builtin.go (or call core.RegisterDataMapEntry from the
// migration that creates the collection). See docs/data-map.md.
func TestDataMapCoversSystemCollections(t *testing.T) {
	app := bootstrapFresh(t)

	unmapped, err := core.UnmappedCollections(app)
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range unmapped {
		if c.System {
			t.Errorf("system collection %q has no _dataMap entry: add it to builtinDataMap (core/data_map_builtin.go)", c.Name)
		}
	}

	// the default users collection is registered too
	for _, c := range unmapped {
		if c.Name == "users" {
			t.Error("users should have a built-in entry")
		}
	}
}

func TestDataMapEntriesAreWellFormed(t *testing.T) {
	app := bootstrapFresh(t)

	records, err := app.FindAllRecords(core.CollectionNameDataMap)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) < 25 {
		t.Fatalf("expected the built-in entries, got %d", len(records))
	}

	seen := map[string]bool{}
	for _, r := range records {
		name := r.GetString("collection")
		if seen[name] {
			t.Errorf("duplicate entry %s", name)
		}
		seen[name] = true

		if !r.GetBool("builtin") {
			t.Errorf("%s: seeded entries must be builtin", name)
		}
		if _, err := app.FindCollectionByNameOrId(name); err != nil {
			t.Errorf("%s: entry for a collection that does not exist", name)
		}

		var paths []string
		_ = json.Unmarshal([]byte(r.GetString("subjectPaths")), &paths)
		if r.GetString("treatment") != core.DataMapTreatmentNone && len(paths) == 0 {
			t.Errorf("%s: no subject path", name)
		}
		if r.GetString("treatment") == core.DataMapTreatmentNone && len(paths) != 0 {
			t.Errorf("%s: a none entry must not name subject paths", name)
		}

		// subject paths and personal fields must be real fields of the
		// collection (payload and similar are fields too); email-like
		// paths on _invitations are fields as well
		col, _ := app.FindCollectionByNameOrId(name)
		var fields []string
		_ = json.Unmarshal([]byte(r.GetString("personalFields")), &fields)
		for _, f := range append(paths, fields...) {
			// "<collection>_via_<field>.<user field>": a back-relation path (#80)
			if strings.Contains(f, "_via_") {
				continue
			}
			if col.Fields.GetByName(f) == nil {
				t.Errorf("%s: unknown field %q", name, f)
			}
		}
	}
}

func TestUnmappedCollectionsReportsAppCollections(t *testing.T) {
	app := bootstrapFresh(t)

	extra := core.NewBaseCollection("members_notes")
	extra.Fields.Add(&core.TextField{Name: "body"})
	if err := app.Save(extra); err != nil {
		t.Fatal(err)
	}

	view := core.NewViewCollection("members_view")
	view.ViewQuery = "select id from members_notes"
	if err := app.Save(view); err != nil {
		t.Fatal(err)
	}

	unmapped, err := core.UnmappedCollections(app)
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, c := range unmapped {
		names = append(names, c.Name)
	}
	if !strings.Contains(strings.Join(names, ","), "members_notes") {
		t.Fatalf("members_notes should be unmapped, got %v", names)
	}
	if strings.Contains(strings.Join(names, ","), "members_view") {
		t.Fatalf("view collections hold no data of their own, got %v", names)
	}

	// registering it (custom entry) removes it from the list
	col, _ := app.FindCollectionByNameOrId(core.CollectionNameDataMap)
	r := core.NewRecord(col)
	r.Set("collection", "members_notes")
	r.Set("treatment", core.DataMapTreatmentErase)
	r.Set("subjectPaths", []string{"id"})
	if err := app.Save(r); err != nil {
		t.Fatal(err)
	}
	unmapped, _ = core.UnmappedCollections(app)
	for _, c := range unmapped {
		if c.Name == "members_notes" {
			t.Fatal("members_notes is mapped now")
		}
	}
}

func TestDataMapValidation(t *testing.T) {
	app := bootstrapFresh(t)
	col, _ := app.FindCollectionByNameOrId(core.CollectionNameDataMap)

	scenarios := []struct {
		name   string
		set    map[string]any
		failOn string
	}{
		{"unknown collection", map[string]any{"collection": "nope", "treatment": "erase", "subjectPaths": []string{"id"}}, "collection"},
		{"retain needs a reason", map[string]any{"collection": "users2", "treatment": "retain", "subjectPaths": []string{"id"}}, "retainReason"},
		{"erase needs a subject", map[string]any{"collection": "users2", "treatment": "erase"}, "subjectPaths"},
		{"bad treatment", map[string]any{"collection": "users2", "treatment": "shred", "subjectPaths": []string{"id"}}, "treatment"},
		{"none needs nothing", map[string]any{"collection": "users2", "treatment": "none"}, ""},
		{"retain with reason", map[string]any{"collection": "users2", "treatment": "retain", "subjectPaths": []string{"id"}, "retainReason": "tax law"}, ""},
		{"the aux logs table may be mapped", map[string]any{"collection": "_logs", "treatment": "none"}, ""},
	}

	other := core.NewBaseCollection("users2")
	other.Fields.Add(&core.TextField{Name: "name"})
	if err := app.Save(other); err != nil {
		t.Fatal(err)
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			r := core.NewRecord(col)
			r.Load(s.set)
			err := app.Save(r)
			if s.failOn == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				_ = app.Delete(r)
				return
			}
			if err == nil || !strings.Contains(err.Error(), s.failOn) {
				t.Fatalf("expected a %s error, got %v", s.failOn, err)
			}
		})
	}
}

func TestRegisterDataMapEntryIsIdempotentAndKeepsCustomRows(t *testing.T) {
	app := bootstrapFresh(t)
	count := func() int {
		n, err := app.CountRecords(core.CollectionNameDataMap)
		if err != nil {
			t.Fatal(err)
		}
		return int(n)
	}

	before := count()
	entry := core.DataMapEntry{Collection: core.CollectionNameFieldKeys, Treatment: core.DataMapTreatmentNone, Note: "changed"}
	for i := 0; i < 2; i++ {
		if err := core.RegisterDataMapEntry(app, entry); err != nil {
			t.Fatal(err)
		}
	}
	if count() != before {
		t.Fatalf("registering twice must not add rows (%d -> %d)", before, count())
	}
	r, _ := app.FindFirstRecordByData(core.CollectionNameDataMap, "collection", core.CollectionNameFieldKeys)
	if r.GetString("note") != "changed" {
		t.Fatal("built-in row should be updated in place")
	}

	// a custom row of the same collection is never overwritten
	r.Set("builtin", false)
	r.Set("note", "mine")
	if err := app.Save(r); err != nil {
		t.Fatal(err)
	}
	if err := core.RegisterDataMapEntry(app, entry); err != nil {
		t.Fatal(err)
	}
	r, _ = app.FindFirstRecordByData(core.CollectionNameDataMap, "collection", core.CollectionNameFieldKeys)
	if r.GetString("note") != "mine" {
		t.Fatal("custom row was overwritten")
	}
}
