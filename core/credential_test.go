package core_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #81): the credential rule snippets (the checks of
// docs/specs/80-88-congregations.md 2.2), the credential model and the
// lifecycle job. Dates are fixed years (2020 is past, 2099 is future) or an
// explicit "now" passed to the job, so nothing depends on the wall clock.

type credWorld struct {
	*ancestorWorld
	orgs map[string]*core.Record
}

func newCredWorld(t testing.TB) *credWorld {
	t.Helper()

	w := newAncestorWorld(t)
	cw := &credWorld{ancestorWorld: w, orgs: map[string]*core.Record{}}

	recs, _ := w.f.app.FindAllRecords(core.CollectionNameOrgs)
	for _, r := range recs {
		cw.orgs[strings.ToUpper(r.GetString("slug")[:1])] = r
	}

	return cw
}

func (w *credWorld) seed(t testing.TB, user, org, typ, expires string, verified, revoked bool) *core.Record {
	t.Helper()

	col, err := w.f.app.FindCollectionByNameOrId(core.CollectionNameCredentials)
	if err != nil {
		t.Fatal(err)
	}

	exp, err := types.ParseDateTime(expires)
	if err != nil {
		t.Fatal(err)
	}

	c := core.NewRecord(col)
	c.Set("user", w.users[user].Id)
	c.Set("org", w.orgs[org].Id)
	c.Set("type", typ)
	c.Set("issuedAt", "2019-01-01 00:00:00.000Z")
	c.Set("expires", exp)
	c.Set("verified", verified)
	c.Set("revoked", revoked)
	c.Set("status", core.CredentialStatusValid)

	if err := w.f.app.Save(c); err != nil {
		t.Fatal(err)
	}

	return c
}

func (w *credWorld) gate(t testing.TB, opts core.CredentialRuleOpts) string {
	t.Helper()

	r, err := core.CredentialGatedMemberRule("org", opts)
	if err != nil {
		t.Fatal(err)
	}

	return r
}

func TestCredentialGateRules(t *testing.T) {
	w := newCredWorld(t)

	// yan (member of Y): valid credential issued by the diocese D
	yan := w.seed(t, "yan", "D", "safe_environment", "2099-01-01 00:00:00.000Z", true, false)
	// pat (admin of P): expired credential issued by P
	w.seed(t, "pat", "P", "safe_environment", "2020-01-01 00:00:00.000Z", true, false)
	// ned (member of N): revoked
	w.seed(t, "ned", "N", "safe_environment", "2099-01-01 00:00:00.000Z", true, true)
	// qed (admin of Q): a different type
	w.seed(t, "qed", "Q", "other_training", "2099-01-01 00:00:00.000Z", true, false)
	// gwen (D): waiting for the second verifier
	w.seed(t, "gwen", "D", "safe_environment", "2099-01-01 00:00:00.000Z", false, false)
	// mix (member of P): valid credential issued by P
	w.seed(t, "mix", "P", "safe_environment", "2099-01-01 00:00:00.000Z", true, false)

	opts := func(scope string, depth int) core.CredentialRuleOpts {
		return core.CredentialRuleOpts{Type: "safe_environment", Scope: scope, MaxDepth: depth}
	}

	cases := []struct {
		name  string
		rule  string
		doc   string
		wants string
	}{
		// valid, expired, revoked, wrong type and unverified all in one view:
		// only the people with a current, verified credential pass
		{"org scope, doc in P", w.gate(t, opts(core.CredentialScopeOrg, 0)), "P", "mix"},
		{"any scope, doc in P", w.gate(t, opts(core.CredentialScopeAny, 0)), "P", "mix"},
		{"ancestors scope, doc in P (self counts)", w.gate(t, opts(core.CredentialScopeAncestors, 0)), "P", "mix"},
		{"default scope is the record's org", w.gate(t, opts("", 0)), "P", "mix"},
		// ancestor-issued
		{"org scope, doc in Y: the D credential does not count", w.gate(t, opts(core.CredentialScopeOrg, 0)), "Y", ""},
		{"any scope, doc in Y", w.gate(t, opts(core.CredentialScopeAny, 0)), "Y", "yan"},
		{"ancestors scope, doc in Y", w.gate(t, opts(core.CredentialScopeAncestors, 0)), "Y", "yan"},
		{"ancestors, depth 3 (Y<P<N<D)", w.gate(t, opts(core.CredentialScopeAncestors, 3)), "Y", "yan"},
		{"ancestors, depth 2 is too close", w.gate(t, opts(core.CredentialScopeAncestors, 2)), "Y", ""},
		// wrong type
		{"other type", w.gate(t, core.CredentialRuleOpts{Type: "other_training", Scope: core.CredentialScopeOrg}), "Q", "qed"},
	}

	for _, c := range cases {
		if got := w.who(t, c.rule, c.doc); got != c.wants {
			t.Errorf("%s: got %q, want %q", c.name, got, c.wants)
		}
	}

	// the lapse needs no job: move the expiry into the past and ask again
	yan.Set("expires", "2020-06-01 00:00:00.000Z")
	if err := w.f.app.Save(yan); err != nil {
		t.Fatal(err)
	}

	if got := w.who(t, w.gate(t, opts(core.CredentialScopeAncestors, 0)), "Y"); got != "" {
		t.Errorf("expected access to lapse with the expiry, got %q", got)
	}
}

func TestCredentialRuleSet(t *testing.T) {
	set, err := core.CredentialRuleSet("org", core.CredentialRuleOpts{Type: "safe_environment", Scope: core.CredentialScopeAncestors, MaxDepth: 2})
	if err != nil {
		t.Fatal(err)
	}

	for name, r := range map[string]*string{"list": set.List, "view": set.View, "create": set.Create, "update": set.Update} {
		if r == nil || !strings.Contains(*r, `@collection._credentials:c.expires ?> @now`) ||
			!strings.Contains(*r, `@collection._orgClosure:q.depth ?<= 2`) {
			t.Errorf("%s rule is not gated: %v", name, r)
		}
	}

	if set.Delete == nil || strings.Contains(*set.Delete, "_credentials") {
		t.Errorf("delete must stay ungated: %v", set.Delete)
	}

	for _, bad := range []core.CredentialRuleOpts{
		{Type: `x" || true || "`}, {Type: "a"}, {Type: "safe_environment", Scope: "nope"},
		{Type: "safe_environment", MaxDepth: -1}, {Type: "safe_environment", MaxDepth: 99},
	} {
		if _, err := core.CredentialGateRule("org", bad); err == nil {
			t.Errorf("expected %+v to be refused", bad)
		}
	}
}

// ---- lifecycle ------------------------------------------------------------

func credAuditCount(t testing.TB, app core.App, action string) int {
	t.Helper()

	recs, err := app.FindRecordsByFilter(core.CollectionNameAuditLog, "action={:a}", "", 0, 0, map[string]any{"a": action})
	if err != nil {
		t.Fatal(err)
	}

	return len(recs)
}

func at(s string) time.Time {
	d, err := types.ParseDateTime(s)
	if err != nil {
		panic(err)
	}

	return d.Time()
}

func TestCredentialLifecycle(t *testing.T) {
	t.Setenv(core.WebhooksAllowLocalhostEnv, "true")

	w := newCredWorld(t)
	app := w.f.app
	ctx := core.WithAuditContext(context.Background(), core.AuditActor{Type: core.AuditActorTypeSystem}, "")

	hooks, _ := app.FindCollectionByNameOrId(core.CollectionNameWebhooks)
	ep := core.NewRecord(hooks)
	ep.Set("url", "http://localhost:1/hook")
	ep.Set("events", []string{"_credentials:credential_expiring", "_credentials:credential_lapsed"})
	ep.Set("enabled", true)
	if err := core.SetWebhookSecret(app, ep, core.GenerateWebhookSecret()); err != nil {
		t.Fatal(err)
	}
	if err := app.Save(ep); err != nil {
		t.Fatal(err)
	}

	// expires 2030-01-20: 19 days after the first "now"
	soon := w.seed(t, "pat", "P", "safe_environment", "2030-01-20 00:00:00.000Z", true, false)
	// far away, a revoked one and an unverified one never get a reminder
	w.seed(t, "mix", "P", "safe_environment", "2031-01-01 00:00:00.000Z", true, false)
	w.seed(t, "ned", "N", "safe_environment", "2030-01-10 00:00:00.000Z", true, true)
	w.seed(t, "gwen", "D", "safe_environment", "2030-01-10 00:00:00.000Z", false, false)

	run := func(now string) core.CredentialLifecycleReport {
		rep, err := core.RunCredentialLifecycle(app, ctx, at(now))
		if err != nil {
			t.Fatal(err)
		}

		return *rep
	}

	// 19 days left: the 30 day threshold. Idempotent.
	if rep := run("2030-01-01 00:00:00.000Z"); rep.Expiring != 1 || rep.Lapsed != 0 {
		t.Fatalf("first run: %+v", rep)
	}

	if rep := run("2030-01-01 12:00:00.000Z"); rep.Expiring != 0 || rep.Lapsed != 0 {
		t.Fatalf("second run must do nothing: %+v", rep)
	}

	// 5 days left: the 7 day threshold, once
	// (the unverified credential expired on 01-10 and lapses in this run)
	if rep := run("2030-01-15 00:00:00.000Z"); rep.Expiring != 1 || rep.Lapsed != 1 {
		t.Fatalf("7 day run: %+v", rep)
	}

	if rep := run("2030-01-16 00:00:00.000Z"); rep.Expiring != 0 {
		t.Fatalf("7 day rerun: %+v", rep)
	}

	if n := credAuditCount(t, app, core.AuditActionCredentialExpiring); n != 2 {
		t.Fatalf("expected 2 expiring entries, got %d", n)
	}

	// lapse: the one that was reminded; the revoked one is never touched
	if rep := run("2030-01-21 00:00:00.000Z"); rep.Lapsed != 1 {
		t.Fatalf("lapse run: %+v", rep)
	}

	if rep := run("2030-01-22 00:00:00.000Z"); rep.Lapsed != 0 || rep.Expiring != 0 {
		t.Fatalf("lapse rerun: %+v", rep)
	}

	if n := credAuditCount(t, app, core.AuditActionCredentialLapsed); n != 2 {
		t.Fatalf("expected 2 lapsed entries, got %d", n)
	}

	got, _ := app.FindRecordById(core.CollectionNameCredentials, soon.Id)
	if got.GetString("status") != core.CredentialStatusLapsed || got.GetDateTime("lapsedAt").IsZero() {
		t.Fatalf("expected the credential to be marked lapsed: %v %v", got.GetString("status"), got.GetDateTime("lapsedAt"))
	}

	// the entry names the org, type and holder
	entries, _ := app.FindRecordsByFilter(core.CollectionNameAuditLog, "action={:a} && targetId={:id}", "", 0, 0,
		map[string]any{"a": core.AuditActionCredentialLapsed, "id": soon.Id})
	if len(entries) != 1 || !strings.Contains(entries[0].GetString("meta"), w.orgs["P"].Id) {
		t.Fatalf("unexpected lapsed entry: %v", entries)
	}

	// webhooks: 2 expiring + 2 lapsed deliveries
	del, _ := app.FindAllRecords(core.CollectionNameWebhookDeliveries)

	var kinds []string
	for _, d := range del {
		kinds = append(kinds, d.GetString("eventType"))
	}

	slices.Sort(kinds)

	if strings.Join(kinds, ",") != "credential.expiring,credential.expiring,credential.lapsed,credential.lapsed" {
		t.Fatalf("unexpected deliveries: %v", kinds)
	}
}

func TestCredentialReminderDaysSetting(t *testing.T) {
	w := newCredWorld(t)
	p := w.orgs["P"]

	if got := core.CredentialReminderDays(p); !slices.Equal(got, []int{30, 7}) {
		t.Fatalf("default: %v", got)
	}

	p.Set("settings", `{"credentialReminderDays":[3, 45, 3, 0, 9999]}`)

	if got := core.CredentialReminderDays(p); !slices.Equal(got, []int{45, 3}) {
		t.Fatalf("custom: %v", got)
	}

	if core.CredentialDocumentsEnabled(p) {
		t.Fatal("documents must be off by default")
	}

	p.Set("settings", `{"credentialDocuments":true}`)

	if !core.CredentialDocumentsEnabled(p) {
		t.Fatal("expected documents on")
	}
}

func TestCredentialStanding(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	col, _ := app.FindCollectionByNameOrId(core.CollectionNameCredentials)
	mk := func(expires string, verified, revoked bool) *core.Record {
		c := core.NewRecord(col)
		c.Set("expires", expires)
		c.Set("verified", verified)
		c.Set("revoked", revoked)

		return c
	}

	now := at("2030-01-01 00:00:00.000Z")

	cases := []struct {
		creds []*core.Record
		want  string
	}{
		{nil, core.CredentialStandingMissing},
		{[]*core.Record{mk("2031-01-01 00:00:00.000Z", true, false)}, core.CredentialStandingCleared},
		{[]*core.Record{mk("2030-02-01 00:00:00.000Z", true, false)}, core.CredentialStandingExpiring},
		{[]*core.Record{mk("2031-01-01 00:00:00.000Z", false, false)}, core.CredentialStandingPending},
		{[]*core.Record{mk("2029-01-01 00:00:00.000Z", true, false)}, core.CredentialStandingLapsed},
		{[]*core.Record{mk("2031-01-01 00:00:00.000Z", true, true)}, core.CredentialStandingRevoked},
		// the best one wins
		{[]*core.Record{mk("2029-01-01 00:00:00.000Z", true, false), mk("2032-01-01 00:00:00.000Z", true, false)}, core.CredentialStandingCleared},
		{[]*core.Record{mk("2031-01-01 00:00:00.000Z", true, true), mk("2029-01-01 00:00:00.000Z", true, false)}, core.CredentialStandingLapsed},
	}

	for i, c := range cases {
		if got, _ := core.CredentialStanding(c.creds, now, 60); got != c.want {
			t.Errorf("case %d: got %s, want %s", i, got, c.want)
		}
	}
}

func TestOrgCoordinators(t *testing.T) {
	w := newCredWorld(t)
	app := w.f.app

	ids := func(org string) []string {
		t.Helper()

		recs, err := core.OrgCoordinators(app, w.orgs[org].Id)
		if err != nil {
			t.Fatal(err)
		}

		var out []string
		for _, r := range recs {
			out = append(out, r.Email())
		}

		slices.Sort(out)

		return out
	}

	// the tag wins over the admins of the same org
	if got := ids("D"); !slices.Equal(got, []string{"gwen@closure.test"}) {
		t.Fatalf("D: %v", got)
	}

	// nobody is tagged in P: the admins (mix is only a member there)
	if got := ids("P"); !slices.Equal(got, []string{"pat@closure.test"}) {
		t.Fatalf("P: %v", got)
	}

	// a suspended tagged member does not count: the fallback applies again
	m, _ := core.FindOrgMember(app, w.orgs["D"].Id, w.users["gwen"].Id)
	m.Set("status", "suspended")

	if err := app.Save(m); err != nil {
		t.Fatal(err)
	}

	if got := ids("D"); !slices.Equal(got, []string{"ann@closure.test", "mix@closure.test"}) {
		t.Fatalf("D without gwen: %v", got)
	}

	// an org with no admins and no tag has no coordinators
	if got := ids("Y"); len(got) != 0 {
		t.Fatalf("Y: %v", got)
	}
}

func TestCredentialReminderDaysValidation(t *testing.T) {
	w := newCredWorld(t)
	org := w.orgs["Q"]

	cases := []struct {
		settings string
		ok       bool
	}{
		{`{}`, true},
		{`{"credentialReminderDays":[30,7]}`, true},
		{`{"credentialReminderDays":[365,1,2,3,4]}`, true},
		{`{"credentialReminderDays":[1,2,3,4,5,6]}`, false},
		{`{"credentialReminderDays":[]}`, false},
		{`{"credentialReminderDays":[0]}`, false},
		{`{"credentialReminderDays":[366]}`, false},
		{`{"credentialReminderDays":[7,7]}`, false},
		{`{"credentialReminderDays":[1.5]}`, false},
		{`{"credentialReminderDays":["7"]}`, false},
		{`{"credentialReminderDays":7}`, false},
	}

	for _, c := range cases {
		org.Set("settings", c.settings)

		err := w.f.app.Save(org)
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v, want ok=%v", c.settings, err, c.ok)
		}
	}
}

func TestCredentialBlocksOrgDelete(t *testing.T) {
	w := newCredWorld(t)
	app := w.f.app
	ctx := context.Background()

	y, q := w.orgs["Y"], w.f.org(t, "zz", "")

	w.seed(t, "yan", "Y", "safe_environment", "2099-01-01 00:00:00.000Z", true, false)

	for _, o := range []*core.Record{y, q} {
		o.Set("archived", true)

		if err := app.Save(o); err != nil {
			t.Fatal(err)
		}
	}

	// the helper and a direct record delete are both refused with a 400
	wantOrgErr(t, core.DeleteOrg(app, ctx, y), 400)
	wantOrgErr(t, app.Delete(y), 400)

	if _, err := app.FindRecordById(core.CollectionNameOrgs, y.Id); err != nil {
		t.Fatal("the org must still exist")
	}

	// an org without credentials still deletes
	if err := core.DeleteOrg(app, ctx, q); err != nil {
		t.Fatalf("delete without credentials: %v", err)
	}
}

func TestCredentialReminderDelivery(t *testing.T) {
	w := newCredWorld(t)
	app := w.f.app
	ctx := core.WithAuditContext(context.Background(), core.AuditActor{Type: core.AuditActorTypeSystem}, "")

	var sent []*core.CredentialReminder

	prev := core.CredentialReminderSender
	core.CredentialReminderSender = func(_ core.App, r *core.CredentialReminder) error {
		sent = append(sent, r)

		return nil
	}

	t.Cleanup(func() { core.CredentialReminderSender = prev })

	// pat (P, no tagged coordinator: the admins) and mix (D, gwen is tagged)
	w.seed(t, "pat", "P", "safe_environment", "2030-01-20 00:00:00.000Z", true, false)
	w.seed(t, "mix", "D", "safe_environment", "2030-01-25 00:00:00.000Z", true, false)

	// a revoked and an unverified credential are never reminded
	w.seed(t, "ned", "N", "safe_environment", "2030-01-20 00:00:00.000Z", true, true)
	w.seed(t, "yan", "Y", "safe_environment", "2030-01-20 00:00:00.000Z", false, false)

	run := func(now string) {
		t.Helper()

		if _, err := core.RunCredentialLifecycle(app, ctx, at(now)); err != nil {
			t.Fatal(err)
		}
	}

	emails := func(r *core.CredentialReminder) []string {
		var out []string
		for _, c := range r.Coordinators {
			out = append(out, c.Email())
		}

		slices.Sort(out)

		return out
	}

	run("2030-01-01 00:00:00.000Z")

	if len(sent) != 2 {
		t.Fatalf("first run: %d reminders", len(sent))
	}

	byHolder := map[string]*core.CredentialReminder{}
	for _, r := range sent {
		byHolder[r.Holder.Email()] = r
	}

	if r := byHolder["pat@closure.test"]; r == nil || r.Threshold != 30 || r.DaysLeft != 19 ||
		!slices.Equal(emails(r), []string{"pat@closure.test"}) {
		t.Fatalf("pat: %+v", r)
	}

	if r := byHolder["mix@closure.test"]; r == nil || !slices.Equal(emails(r), []string{"gwen@closure.test"}) {
		t.Fatalf("mix: %+v", r)
	}

	// reruns, also after a restart (state is in the row): nothing is sent twice
	run("2030-01-01 12:00:00.000Z")
	run("2030-01-02 00:00:00.000Z")

	if len(sent) != 2 {
		t.Fatalf("rerun resent: %d", len(sent))
	}

	// the 7 day offset: one more each, once
	run("2030-01-15 00:00:00.000Z")
	run("2030-01-15 06:00:00.000Z")

	// pat has 5 days left, mix still 10
	if len(sent) != 3 {
		t.Fatalf("7 day run: %d reminders", len(sent))
	}

	if last := sent[len(sent)-1]; last.Threshold != 7 || last.TypeName != "safe_environment" {
		t.Fatalf("unexpected: %+v", last)
	}

	run("2030-01-19 00:00:00.000Z")
	run("2030-01-20 00:00:00.000Z")

	if len(sent) != 4 || sent[3].Holder.Email() != "mix@closure.test" {
		t.Fatalf("mix 7 day run: %d reminders", len(sent))
	}
}
