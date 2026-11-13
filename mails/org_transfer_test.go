package mails_test

import (
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/mails"
	"github.com/doodlemania2/claviger/tests"
	"github.com/doodlemania2/claviger/tools/types"
)

func TestSendOrgTransferNotice(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	orgs, _ := app.FindCollectionByNameOrId(core.CollectionNameOrgs)
	mkOrg := func(name, slug string) *core.Record {
		o := core.NewRecord(orgs)
		o.Set("name", name)
		o.Set("slug", slug)

		if err := app.Save(o); err != nil {
			t.Fatal(err)
		}

		return o
	}

	users, _ := app.FindCollectionByNameOrId("users")
	mkUser := func(email string) *core.Record {
		u := core.NewRecord(users)
		u.SetEmail(email)
		u.SetPassword("password123")

		if err := app.Save(u); err != nil {
			t.Fatal(err)
		}

		return u
	}

	person, guardian := mkUser("person@x.test"), mkUser("guardian@x.test")

	tr := core.NewRecord(mustTransfersCol(t, app))
	tr.Set("expiresAt", types.NowDateTime())

	n := &core.OrgTransferNotice{
		Transfer: tr, FromOrg: mkOrg("St Mark", "stmark"), ToOrg: mkOrg("St Luke <b>", "stluke"),
		Person: person, Recipients: []*core.Record{person, guardian}, Starter: "Pat",
	}

	app.TestMailer.Reset()

	if err := mails.SendOrgTransferNotice(app, n); err != nil {
		t.Fatal(err)
	}

	if app.TestMailer.TotalSend() != 2 {
		t.Fatalf("sent %d mails", app.TestMailer.TotalSend())
	}

	for i, m := range app.TestMailer.Messages() {
		if !strings.Contains(m.Subject, "St Mark") || !strings.Contains(m.HTML, "St Luke &lt;b&gt;") ||
			!strings.Contains(m.HTML, "Pat") || strings.Contains(m.HTML, "St Luke <b>") {
			t.Errorf("mail %d: unexpected content:\n%s\n%s", i, m.Subject, m.HTML)
		}
	}

	// only the guardian is told that a guardian decides
	msgs := app.TestMailer.Messages()
	if strings.Contains(msgs[0].HTML, "as a guardian") || !strings.Contains(msgs[1].HTML, "as a guardian") {
		t.Error("the guardian paragraph belongs in the guardian's mail only")
	}
}

func TestOrgTransferNotifierIsRegistered(t *testing.T) {
	if core.OrgTransferNotifier == nil {
		t.Fatal("the mails package must register the transfer notifier")
	}
}

func mustTransfersCol(t *testing.T, app core.App) *core.Collection {
	t.Helper()

	c, err := app.FindCollectionByNameOrId(core.CollectionNameOrgTransfers)
	if err != nil {
		t.Fatal(err)
	}

	return c
}
