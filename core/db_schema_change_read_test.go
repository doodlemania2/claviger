package core_test

import (
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// Reads right after a column change must not fail. On PostgreSQL, pgx's
// default statement cache made the first reuse per connection fail with
// "cached plan must not change result type".
func TestReadAfterCollectionSchemaChange(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	col := core.NewBaseCollection("schema_change_read")
	col.Fields.Add(&core.TextField{Name: "a"})
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}

	r := core.NewRecord(col)
	r.Set("a", "x")
	if err := app.Save(r); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 10; i++ {
		if _, err := app.FindRecordById(col.Name, r.Id); err != nil {
			t.Fatalf("read %d before the change: %v", i, err)
		}
	}

	col.Fields.Add(&core.NumberField{Name: "b"})
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 10; i++ {
		if _, err := app.FindRecordById(col.Name, r.Id); err != nil {
			t.Fatalf("read %d after adding a field: %v", i, err)
		}
	}
}
