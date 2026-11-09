package cmd_test

import (
	"bytes"
	"strings"
	"testing"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/cmd"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// Fork-local (Claviger #84): claviger orgs closure-repair.
func TestOrgsClosureRepairCommand(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	orgs, err := app.FindCollectionByNameOrId(core.CollectionNameOrgs)
	if err != nil {
		t.Fatal(err)
	}

	parent := ""
	for _, slug := range []string{"aa", "bb", "cc"} {
		r := core.NewRecord(orgs)
		r.Set("name", slug)
		r.Set("slug", slug)
		r.Set("parent", parent)
		if err := app.Save(r); err != nil {
			t.Fatal(err)
		}
		parent = r.Id
	}

	run := func(args ...string) string {
		t.Helper()

		var out bytes.Buffer

		c := cmd.NewOrgsCommand(app)
		c.SetOut(&out)
		c.SetArgs(args)
		if err := c.Execute(); err != nil {
			t.Fatal(err)
		}

		return out.String()
	}

	if out := run("closure-repair"); !strings.Contains(out, "consistent") || !strings.Contains(out, "expected 6, found 6") {
		t.Fatalf("unexpected output: %s", out)
	}

	if _, err := app.NonconcurrentDB().NewQuery("DELETE FROM {{" + core.CollectionNameOrgClosure + "}} WHERE [[depth]]=2").Execute(); err != nil {
		t.Fatal(err)
	}

	if out := run("closure-repair", "--dry-run"); !strings.Contains(out, "1 missing") || !strings.Contains(out, "dry run") {
		t.Fatalf("unexpected output: %s", out)
	}

	if n, _ := app.CountRecords(core.CollectionNameOrgClosure, dbx.NewExp("1=1")); n != 5 {
		t.Fatalf("a dry run must not change the table, got %d rows", n)
	}

	if out := run("closure-repair"); !strings.Contains(out, "repaired") {
		t.Fatalf("unexpected output: %s", out)
	}

	if n, _ := app.CountRecords(core.CollectionNameOrgClosure, dbx.NewExp("1=1")); n != 6 {
		t.Fatalf("expected 6 rows, got %d", n)
	}
}
