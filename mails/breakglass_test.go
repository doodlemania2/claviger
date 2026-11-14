package mails_test

import (
	"strings"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/mails"
	"github.com/doodlemania2/claviger/tests"
)

func TestSendBreakGlassNotice(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	orgs, _ := app.FindCollectionByNameOrId(core.CollectionNameOrgs)
	org := core.NewRecord(orgs)
	org.Set("name", "St Mark <b>")
	org.Set("slug", "stmark")

	if err := app.Save(org); err != nil {
		t.Fatal(err)
	}

	users, _ := app.FindCollectionByNameOrId("users")
	mkUser := func(email, name string) *core.Record {
		u := core.NewRecord(users)
		u.SetEmail(email)
		u.SetPassword("password123")
		u.Set("name", name)

		if err := app.Save(u); err != nil {
			t.Fatal(err)
		}

		return u
	}

	author, owner, grantee, approver := mkUser("author@x.test", "Ann"), mkUser("owner@x.test", "Olga"),
		mkUser("grantee@x.test", "Gus"), mkUser("approver@x.test", "Abe")

	bg, _ := app.FindCollectionByNameOrId(core.CollectionNameBreakGlass)
	grant := core.NewRecord(bg)
	grant.Set("basis", "safety")
	grant.Set("reason", "Someone is at <script>risk</script> tonight")

	note := core.NewRecord(users) // only the author field is read
	note.Set("author", author.Id)

	n := &core.BreakGlassNotice{
		Grant: grant, Note: note, Org: org, Grantee: grantee, Approver: approver,
		Expires: time.Now().Add(time.Hour), Recipients: []*core.Record{author, owner}, AuthorReachable: false,
	}

	app.TestMailer.Reset()

	sent, err := mails.SendBreakGlassNotice(app, n)
	if err != nil || sent != 2 {
		t.Fatalf("sent %d: %v", sent, err)
	}

	for _, m := range app.TestMailer.Messages() {
		for _, want := range []string{"Gus", "Abe", "St Mark &lt;b&gt;", "at &lt;script&gt;risk", "cannot be switched off", "breakglass tag"} {
			if !strings.Contains(m.HTML, want) {
				t.Fatalf("notice to %s lacks %q:\n%s", m.To[0].Address, want, m.HTML)
			}
		}

		if strings.Contains(m.HTML, "<script>") {
			t.Fatal("the reason must be escaped")
		}
	}

	if first := app.TestMailer.Messages()[0]; !strings.Contains(first.HTML, "you wrote the note") {
		t.Fatal("the author's notice says why she receives it")
	}
}
