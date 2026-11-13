package cmd_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/cmd"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// Fork-local (Claviger #87): claviger registers verify.
func TestRegistersVerifyCommand(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	run := func() (string, error) {
		c := cmd.NewRegistersCommand(app)
		buf := new(bytes.Buffer)
		c.SetOut(buf)
		c.SetErr(buf)
		c.SetArgs([]string{"verify"})
		err := c.Execute()

		return buf.String(), err
	}

	out, err := run()
	if err != nil || !strings.Contains(out, "no permanent registers") {
		t.Fatalf("no registers: %v %q", err, out)
	}

	orgs, _ := app.FindCollectionByNameOrId(core.CollectionNameOrgs)
	org := core.NewRecord(orgs)
	org.Set("name", "Parish")
	org.Set("slug", "parish-cli")

	if err := app.Save(org); err != nil {
		t.Fatal(err)
	}

	if _, err := core.CreateSacramentalRegister(app, core.SacramentalRegisterOpts{Name: "baptisms"}); err != nil {
		t.Fatal(err)
	}

	su, _ := app.FindAuthRecordByEmail(core.CollectionNameSuperusers, "test@example.com")
	col, _ := app.FindCollectionByNameOrId("baptisms")

	var ids []string

	for _, n := range []string{"One", "Two"} {
		r := core.NewRecord(col)
		r.Set("org", org.Id)
		r.Set("sacrament", "baptism")
		r.Set("surname", n)

		if err := app.Save(r); err != nil {
			t.Fatal(err)
		}

		if _, err := core.FinalizeRegisterEntry(app, context.Background(), su, "baptisms", r.Id); err != nil {
			t.Fatal(err)
		}

		ids = append(ids, r.Id)
	}

	out, err = run()
	if err != nil || !strings.Contains(out, "baptisms: ok, 2 final entries, head 2") {
		t.Fatalf("clean: %v %q", err, out)
	}

	if _, err := app.NonconcurrentDB().NewQuery("UPDATE baptisms SET surname = 'Forged' WHERE id = {:id}").Bind(dbx.Params{"id": ids[0]}).Execute(); err != nil {
		t.Fatal(err)
	}

	out, err = run()
	if err == nil || !strings.Contains(out, "1 break(s)") || !strings.Contains(out, ids[0]) {
		t.Fatalf("tampered: %v %q", err, out)
	}
}
