package mails_test

import (
	"strings"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/mails"
	"github.com/doodlemania2/claviger/tests"
)

func TestSendCredentialReminder(t *testing.T) {
	cases := []struct {
		name      string
		settings  string
		holder    string
		coords    []string
		wantTo    []string
		wantInSub string
	}{
		{"holder and coordinators", ``, "holder@x.test", []string{"c1@x.test", "c2@x.test"},
			[]string{"holder@x.test", "c1@x.test", "c2@x.test"}, "expires on 5 March 2031"},
		{"holder without an address is skipped", ``, "", []string{"c1@x.test"}, []string{"c1@x.test"}, "expires on"},
		{"coordinator without an address is skipped", ``, "holder@x.test", []string{""}, []string{"holder@x.test"}, "expires on"},
		{"a holder who coordinates gets one mail", ``, "holder@x.test", []string{"holder@x.test"}, []string{"holder@x.test"}, "expires on"},
		{"org language", `{"locale":"es"}`, "holder@x.test", nil, []string{"holder@x.test"}, "vence el 2031-03-05"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			app, err := tests.NewTestApp()
			if err != nil {
				t.Fatal(err)
			}
			defer app.Cleanup()

			orgs, _ := app.FindCollectionByNameOrId(core.CollectionNameOrgs)
			org := core.NewRecord(orgs)
			org.Set("name", "St Mark")
			org.Set("slug", "stmark")

			if c.settings != "" {
				org.Set("settings", c.settings)
			}

			if err := app.Save(org); err != nil {
				t.Fatal(err)
			}

			users, _ := app.FindCollectionByNameOrId("users")
			mk := func(email string) *core.Record {
				u := core.NewRecord(users)
				u.SetEmail(email)
				u.SetPassword("password123")
				u.Set("verified", true)

				if email == "" {
					u.SetEmail("nologin-" + time.Now().Format("150405.000000000") + "@x.test")
					u.Set("emailVisibility", false)
				}

				if err := app.Save(u); err != nil {
					t.Fatal(err)
				}

				if email == "" {
					// a person without an address: drop it after saving
					u.SetEmail("")
				}

				return u
			}

			r := &core.CredentialReminder{
				Org: org, Holder: mk(c.holder), TypeName: "Safe Environment",
				Expires: time.Date(2031, 3, 5, 0, 0, 0, 0, time.UTC), DaysLeft: 7, Threshold: 7,
			}

			for _, e := range c.coords {
				if e == c.holder && e != "" {
					r.Coordinators = append(r.Coordinators, r.Holder)
				} else {
					r.Coordinators = append(r.Coordinators, mk(e))
				}
			}

			app.TestMailer.Reset()

			if err := mails.SendCredentialReminder(app, r); err != nil {
				t.Fatal(err)
			}

			if app.TestMailer.TotalSend() != len(c.wantTo) {
				t.Fatalf("sent %d mails, want %d", app.TestMailer.TotalSend(), len(c.wantTo))
			}

			for i, to := range c.wantTo {
				m := app.TestMailer.Messages()[i]
				if m.To[0].Address != to {
					t.Errorf("mail %d to %s, want %s", i, m.To[0].Address, to)
				}

				if !strings.Contains(m.Subject, c.wantInSub) || !strings.Contains(m.HTML, "Safe Environment") ||
					!strings.Contains(m.HTML, "St Mark") {
					t.Errorf("mail %d: unexpected content:\n%s\n%s", i, m.Subject, m.HTML)
				}
			}
		})
	}
}

func TestCredentialReminderSenderIsRegistered(t *testing.T) {
	if core.CredentialReminderSender == nil {
		t.Fatal("the mails package must register the reminder sender")
	}
}
