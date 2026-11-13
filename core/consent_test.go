package core_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #88, 88-2): consent purposes, state rows, append-only
// events, guardian consent, rule gating and the export.
//
// World: the household world of the #80 tests. H1 has the guardians mix and
// ned, an adult with a login (yan) and the minor kid1; H2 has qed and kid2.
// pat is admin of P and zed is a registrar of P (both staff). ann is admin of
// D, above P, and is not staff of P.

func consentPurpose(t testing.TB, w *hhWorld, in map[string]any) *core.Record {
	t.Helper()

	p, err := core.CreateConsentPurpose(w.f.app, w.ctx, in)
	if err != nil {
		t.Fatal(err)
	}

	return p
}

func consentFixture(t testing.TB) (*hhWorld, *core.Record) {
	t.Helper()

	w := newHHWorld(t)
	news := consentPurpose(t, w, map[string]any{
		"key": "newsletter", "name": "Newsletter", "text": "We email you the newsletter.",
	})

	return w, news
}

func countRows(t testing.TB, w *hhWorld, collection, filter string) int {
	t.Helper()

	rows, err := w.f.app.FindRecordsByFilter(collection, filter, "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	return len(rows)
}

func TestConsentPurposeVersioningAndDefaults(t *testing.T) {
	w, news := consentFixture(t)
	app := w.f.app

	if news.GetInt("version") != 1 || news.GetString("status") != "active" {
		t.Fatalf("new purpose: version %d status %s", news.GetInt("version"), news.GetString("status"))
	}

	// a change that is not the text keeps the version
	p, err := core.UpdateConsentPurpose(app, w.ctx, w.reload(t, news), map[string]any{"name": "News"})
	if err != nil || p.GetInt("version") != 1 {
		t.Fatalf("rename: %v version %d", err, p.GetInt("version"))
	}

	// a new text raises it, and a version sent by the caller is ignored
	p, err = core.UpdateConsentPurpose(app, w.ctx, w.reload(t, news), map[string]any{"text": "A new text."})
	if err != nil || p.GetInt("version") != 2 {
		t.Fatalf("text change: %v version %d", err, p.GetInt("version"))
	}

	// the key is fixed
	if _, err := core.UpdateConsentPurpose(app, w.ctx, w.reload(t, news), map[string]any{"key": "other"}); err == nil {
		t.Fatal("the key must not change")
	}

	// a marketing purpose with no minimum age gets 18
	m := consentPurpose(t, w, map[string]any{"key": "promo", "name": "Promotions", "text": "Offers.", "marketing": true})
	if m.GetInt("minAge") != core.DefaultMarketingMinAge {
		t.Fatalf("marketing minAge = %d", m.GetInt("minAge"))
	}

	// bad input
	for name, in := range map[string]map[string]any{
		"key":      {"key": "Bad Key", "name": "x", "text": "x"},
		"basis":    {"key": "okay", "name": "x", "text": "x", "lawfulBasis": "whim"},
		"minAge":   {"key": "okay", "name": "x", "text": "x", "minAge": 99},
		"no text":  {"key": "okay", "name": "x"},
		"dup key":  {"key": "newsletter", "name": "x", "text": "x"},
		"bad org":  {"key": "okay", "name": "x", "text": "x", "org": "nope"},
		"bad kind": {"key": "okay", "name": "x", "text": "x", "minAge": "old"},
	} {
		if _, err := core.CreateConsentPurpose(app, w.ctx, in); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestConsentSelfGrantWithdrawAndEvents(t *testing.T) {
	w, _ := consentFixture(t)
	app := w.f.app
	yan := w.users["yan"]

	res, err := core.ConsentSelf(app, w.ctx, yan, "newsletter", "", core.ConsentActionGrant)
	if err != nil || !res.Changed {
		t.Fatalf("grant: %v %+v", err, res)
	}

	st := res.State
	if core.ConsentStatus(st) != core.ConsentStatusGranted || st.GetString("subject") != yan.Id ||
		st.GetString("subjectType") != "user" || st.GetString("givenBy") != yan.Id || st.GetString("method") != "web" ||
		st.GetInt("policyVersion") != 1 {
		t.Fatalf("state: %+v", st.FieldsData())
	}

	// granting again at the same version changes nothing and writes no event
	res, err = core.ConsentSelf(app, w.ctx, yan, "newsletter", "", core.ConsentActionGrant)
	if err != nil || res.Changed || countRows(t, w, core.CollectionNameConsentEvents, "1=1") != 1 {
		t.Fatalf("second grant: %v %+v", err, res)
	}

	if res, err = core.ConsentSelf(app, w.ctx, yan, "newsletter", "", core.ConsentActionWithdraw); err != nil ||
		core.ConsentStatus(res.State) != core.ConsentStatusWithdrawn {
		t.Fatalf("withdraw: %v", err)
	}

	// nothing left to withdraw
	if _, err = core.ConsentSelf(app, w.ctx, yan, "newsletter", "", core.ConsentActionWithdraw); err == nil {
		t.Fatal("a second withdrawal must fail")
	}

	// granting again after a withdrawal clears it
	if res, err = core.ConsentSelf(app, w.ctx, yan, "newsletter", "", core.ConsentActionGrant); err != nil ||
		core.ConsentStatus(res.State) != core.ConsentStatusGranted {
		t.Fatalf("regrant: %v", err)
	}

	// one state row, three events with the text that was shown
	if n := countRows(t, w, core.CollectionNameConsents, "1=1"); n != 1 {
		t.Fatalf("state rows = %d", n)
	}

	events, _ := app.FindRecordsByFilter(core.CollectionNameConsentEvents, "1=1", "created", 0, 0)
	if len(events) != 3 {
		t.Fatalf("events = %d", len(events))
	}

	for i, want := range []string{"grant", "withdraw", "grant"} {
		if events[i].GetString("action") != want || events[i].GetString("text") != "We email you the newsletter." ||
			events[i].GetInt("policyVersion") != 1 || events[i].GetString("actor") != yan.Id {
			t.Errorf("event %d: %+v", i, events[i].FieldsData())
		}
	}

	// an unknown purpose, an archived one, and a superuser
	if _, err = core.ConsentSelf(app, w.ctx, yan, "nope", "", core.ConsentActionGrant); err == nil {
		t.Error("unknown purpose")
	}

	p, _ := core.FindConsentPurpose(app, "newsletter")
	if _, err = core.UpdateConsentPurpose(app, w.ctx, p, map[string]any{"status": "archived"}); err != nil {
		t.Fatal(err)
	}

	if _, err = core.ConsentSelf(app, w.ctx, w.users["mix"], "newsletter", "", core.ConsentActionGrant); err == nil {
		t.Error("archived purpose")
	}

	// the purpose has records now: it cannot be deleted
	if err = core.DeleteConsentPurpose(app, w.ctx, w.reload(t, p)); err == nil {
		t.Error("a used purpose must not be deleted")
	}
}

func TestConsentOutdatedAfterTextChange(t *testing.T) {
	w, news := consentFixture(t)
	app := w.f.app

	res, err := core.ConsentSelf(app, w.ctx, w.users["yan"], "newsletter", "", core.ConsentActionGrant)
	if err != nil {
		t.Fatal(err)
	}

	if core.ConsentOutdated(res.State, w.reload(t, news)) {
		t.Fatal("current consent is not outdated")
	}

	if _, err := core.UpdateConsentPurpose(app, w.ctx, w.reload(t, news), map[string]any{"text": "New terms."}); err != nil {
		t.Fatal(err)
	}

	state := w.reload(t, res.State)
	if !core.ConsentOutdated(state, w.reload(t, news)) || state.GetInt("policyVersion") != 1 {
		t.Fatal("the consent must show as given on version 1")
	}

	// consenting again records version 2 with the new text
	res, err = core.ConsentSelf(app, w.ctx, w.users["yan"], "newsletter", "", core.ConsentActionGrant)
	if err != nil || !res.Changed || res.State.GetInt("policyVersion") != 2 || res.Event.GetString("text") != "New terms." {
		t.Fatalf("regrant on v2: %v", err)
	}
}

func TestConsentGuardianAndMinors(t *testing.T) {
	w, _ := consentFixture(t)
	app := w.f.app

	camp := consentPurpose(t, w, map[string]any{"key": "photos", "name": "Photos", "text": "Photos.", "requiresGuardian": true})
	promo := consentPurpose(t, w, map[string]any{"key": "promo", "name": "Promo", "text": "Offers.", "marketing": true})
	_ = promo

	// an active guardian consents for the minor of the household
	res, err := core.ConsentSelf(app, w.ctx, w.users["mix"], "photos", w.kid1.Id, core.ConsentActionGrant)
	if err != nil {
		t.Fatal(err)
	}

	if res.State.GetString("subject") != w.kid1.Id || res.State.GetString("subjectType") != "member" ||
		res.State.GetString("givenBy") != w.mixM.Id || res.State.GetString("org") != w.P.Id {
		t.Fatalf("guardian grant: %+v", res.State.FieldsData())
	}

	// the other guardian can withdraw it
	if _, err := core.ConsentSelf(app, w.ctx, w.users["ned"], "photos", w.kid1.Id, core.ConsentActionWithdraw); err != nil {
		t.Fatal(err)
	}

	for name, c := range map[string]struct{ user, member, purpose string }{
		"guardian of another household": {"qed", w.kid1.Id, "newsletter"},
		"adult who is not a guardian":   {"yan", w.kid1.Id, "newsletter"},
		"not in the household at all":   {"gwen", w.kid1.Id, "newsletter"},
		"a guardian for an adult":       {"mix", w.yanM.Id, "newsletter"},
		"marketing for a minor":         {"mix", w.kid1.Id, "promo"},
	} {
		if _, err := core.ConsentSelf(app, w.ctx, w.users[c.user], c.purpose, c.member, core.ConsentActionGrant); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}

	// an adult may consent to marketing for themselves
	if _, err := core.ConsentSelf(app, w.ctx, w.users["yan"], "promo", "", core.ConsentActionGrant); err != nil {
		t.Errorf("adult marketing: %v", err)
	}

	// a minor's marketing consent may always be withdrawn, and not granted by staff either
	org := w.P
	if _, err := core.ConsentStaff(app, w.ctx, w.users["pat"], org, core.ConsentStaffInput{
		Purpose: "promo", Member: w.kid1.Id, Action: "grant", Method: "paper", EvidenceRef: "box 4", GivenBy: w.mixM.Id,
	}); err == nil {
		t.Error("staff must not grant marketing for a minor")
	}

	_ = camp
}

func TestConsentOverview(t *testing.T) {
	w, _ := consentFixture(t)
	app := w.f.app

	consentPurpose(t, w, map[string]any{"key": "promo", "name": "Promo", "text": "Offers.", "marketing": true})
	consentPurpose(t, w, map[string]any{"key": "pspecial", "name": "P only", "text": "x", "org": w.P.Id})
	q, err := app.FindFirstRecordByData(core.CollectionNameOrgs, "slug", "qq")
	if err != nil {
		t.Fatal(err)
	}

	consentPurpose(t, w, map[string]any{"key": "qspecial", "name": "Q only", "text": "x", "org": q.Id})

	if _, err := core.ConsentSelf(app, w.ctx, w.users["mix"], "newsletter", w.kid1.Id, core.ConsentActionGrant); err != nil {
		t.Fatal(err)
	}

	subjects, err := core.ConsentOverview(app, w.users["mix"])
	if err != nil {
		t.Fatal(err)
	}

	// mix and kid1, not kid2 (another household)
	if len(subjects) != 2 || subjects[0].Id != w.users["mix"].Id || subjects[1].Id != w.kid1.Id {
		t.Fatalf("subjects: %d", len(subjects))
	}

	status := func(s *core.ConsentOverviewSubject) map[string]string {
		out := map[string]string{}

		for _, p := range s.Purposes {
			out[p.Purpose.GetString("key")] = p.Status
			if p.Purpose.GetString("key") == "promo" && s.Minor() && (p.CanGrant || p.Reason != core.ConsentReasonMarketingMinor) {
				t.Errorf("promo for the minor: %+v", p)
			}
		}

		return out
	}

	kid := status(subjects[1])
	if kid["newsletter"] != "granted" || kid["promo"] != "none" {
		t.Errorf("kid: %v", kid)
	}

	if _, ok := kid["pspecial"]; !ok {
		t.Errorf("the org's purpose is offered to its households: %v", kid)
	}

	if _, ok := kid["qspecial"]; ok {
		t.Errorf("another org's purpose must not be offered: %v", kid)
	}

	// the superuser has no consents of their own
	if s, _ := core.ConsentOverview(app, nil); len(s) != 0 {
		t.Error("no auth, no subjects")
	}
}

func TestConsentStaffRecord(t *testing.T) {
	w, _ := consentFixture(t)
	app := w.f.app

	in := func(m func(*core.ConsentStaffInput)) core.ConsentStaffInput {
		i := core.ConsentStaffInput{
			Purpose: "newsletter", Member: w.kid1.Id, Action: "grant", Method: "paper",
			EvidenceRef: "form 12, binder A", GivenBy: w.mixM.Id,
		}
		if m != nil {
			m(&i)
		}

		return i
	}

	// the admin and the registrar can record; a plain member and an admin of an ancestor cannot
	res, err := core.ConsentStaff(app, w.ctx, w.users["pat"], w.P, in(nil))
	if err != nil {
		t.Fatal(err)
	}

	if st := res.State; st.GetString("method") != "paper" || st.GetString("evidenceRef") != "form 12, binder A" ||
		st.GetString("givenBy") != w.mixM.Id || res.Event.GetString("actor") != w.users["pat"].Id {
		t.Fatalf("state: %+v", st.FieldsData())
	}

	if _, err := core.ConsentStaff(app, w.ctx, w.users["zed"], w.P, in(func(i *core.ConsentStaffInput) { i.Action = "withdraw" })); err != nil {
		t.Fatalf("registrar: %v", err)
	}

	for name, c := range map[string]struct {
		user string
		mod  func(*core.ConsentStaffInput)
	}{
		"member, not staff":         {"mix", nil},
		"admin of an ancestor":      {"ann", nil},
		"no evidence":               {"pat", func(i *core.ConsentStaffInput) { i.EvidenceRef = " " }},
		"no guardian for a minor":   {"pat", func(i *core.ConsentStaffInput) { i.GivenBy = "" }},
		"guardian of another house": {"pat", func(i *core.ConsentStaffInput) { i.GivenBy = w.qedM.Id }},
		"a minor as the guardian":   {"pat", func(i *core.ConsentStaffInput) { i.GivenBy = w.kid2.Id }},
		"bad method":                {"pat", func(i *core.ConsentStaffInput) { i.Method = "web" }},
		"both member and user":      {"pat", func(i *core.ConsentStaffInput) { i.User = w.users["yan"].Id }},
		"neither":                   {"pat", func(i *core.ConsentStaffInput) { i.Member = "" }},
		"a user outside the org":    {"pat", func(i *core.ConsentStaffInput) { i.Member, i.User, i.GivenBy = "", w.users["ann"].Id, "" }},
		"unknown purpose":           {"pat", func(i *core.ConsentStaffInput) { i.Purpose = "nope" }},
		"unknown action":            {"pat", func(i *core.ConsentStaffInput) { i.Action = "erase" }},
		"unknown member":            {"pat", func(i *core.ConsentStaffInput) { i.Member = "nonexistent0000" }},
	} {
		if _, err := core.ConsentStaff(app, w.ctx, w.users[c.user], w.P, in(c.mod)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}

	// an adult is recorded without a guardian; the subject gave it
	res, err = core.ConsentStaff(app, w.ctx, w.users["pat"], w.P, in(func(i *core.ConsentStaffInput) {
		i.Member, i.GivenBy, i.Method = w.yanM.Id, "", ""
	}))
	if err != nil || res.State.GetString("givenBy") != w.yanM.Id || res.State.GetString("method") != "staff" {
		t.Fatalf("adult: %v", err)
	}

	// a user of the org
	if _, err := core.ConsentStaff(app, w.ctx, w.users["pat"], w.P, in(func(i *core.ConsentStaffInput) {
		i.Member, i.User, i.GivenBy = "", w.users["mix"].Id, ""
	})); err != nil {
		t.Fatalf("user: %v", err)
	}
}

func TestConsentEventsAreAppendOnly(t *testing.T) {
	w, _ := consentFixture(t)
	app := w.f.app

	res, err := core.ConsentSelf(app, w.ctx, w.users["yan"], "newsletter", "", core.ConsentActionGrant)
	if err != nil {
		t.Fatal(err)
	}

	ev := w.reload(t, res.Event)

	ev.Set("action", "withdraw")

	if err := app.Save(ev); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("update must be refused, got %v", err)
	}

	if err := app.SaveNoValidate(w.reload(t, res.Event)); err != nil && !strings.Contains(err.Error(), "append-only") {
		t.Fatal(err)
	}

	if err := app.Delete(w.reload(t, res.Event)); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("delete must be refused, got %v", err)
	}

	if got := w.reload(t, res.Event); got.GetString("action") != "grant" {
		t.Fatal("the event changed")
	}

	// appending is still possible
	if _, err := core.ConsentSelf(app, w.ctx, w.users["yan"], "newsletter", "", core.ConsentActionWithdraw); err != nil {
		t.Fatal(err)
	}

	// the records API cannot write either: the write rules are null
	col, _ := app.FindCollectionByNameOrId(core.CollectionNameConsentEvents)
	if col.CreateRule != nil || col.UpdateRule != nil || col.DeleteRule != nil {
		t.Error("write rules must be null")
	}
}

func TestConsentPseudonymizeKeepsEvidence(t *testing.T) {
	w, _ := consentFixture(t)
	app := w.f.app

	if _, err := core.ConsentSelf(app, w.ctx, w.users["mix"], "newsletter", w.kid1.Id, core.ConsentActionGrant); err != nil {
		t.Fatal(err)
	}

	if _, err := core.ConsentSelf(app, w.ctx, w.users["yan"], "newsletter", "", core.ConsentActionGrant); err != nil {
		t.Fatal(err)
	}

	n, err := core.PseudonymizeConsents(app, w.ctx, w.kid1.Id)
	if err != nil || n != 1 {
		t.Fatalf("pseudonymize: %d %v", n, err)
	}

	p := core.ConsentPseudonym(w.kid1.Id)
	if !strings.HasPrefix(p, "erased-") || strings.Contains(p, w.kid1.Id) {
		t.Fatalf("pseudonym %q", p)
	}

	events, _ := app.FindRecordsByFilter(core.CollectionNameConsentEvents, "subject={:s}", "", 0, 0, map[string]any{"s": p})
	if len(events) != 1 {
		t.Fatalf("pseudonymized events = %d", len(events))
	}

	if e := events[0]; e.GetString("action") != "grant" || e.GetString("purpose") != "newsletter" ||
		e.GetString("text") == "" || e.GetInt("policyVersion") != 1 || e.GetDateTime("pseudonymizedAt").IsZero() ||
		e.GetString("evidenceRef") != "" {
		t.Errorf("evidence changed: %+v", e.FieldsData())
	}

	// the guardian's id is not on the kept row as the subject, and the state row followed
	if countRows(t, w, core.CollectionNameConsents, "subject='"+p+"'") != 1 || countRows(t, w, core.CollectionNameConsents, "subject='"+w.kid1.Id+"'") != 0 {
		t.Error("the state row must carry the pseudonym")
	}

	// the other person's rows are untouched, and the guard still holds for everyone else
	if countRows(t, w, core.CollectionNameConsentEvents, "subject='"+w.users["yan"].Id+"'") != 1 {
		t.Error("another subject changed")
	}

	if err := app.Save(events[0]); err == nil {
		t.Error("a plain save must still be refused")
	}
}

// the spec's example rule, with the sign-in check in front
const newsletterRule = `@request.auth.id != "" && @collection._consents:c.subject ?= member && ` +
	`@collection._consents:c.purpose ?= "newsletter" && @collection._consents:c.withdrawnAt ?= ""`

func TestConsentRuleGating(t *testing.T) {
	w, _ := consentFixture(t)
	app := w.f.app

	mCol, _ := app.FindCollectionByNameOrId(core.CollectionNameHouseholdMembers)
	hCol, _ := app.FindCollectionByNameOrId(core.CollectionNameHouseholds)

	items := core.NewBaseCollection("news_items")
	items.Fields.Add(
		&core.TextField{Name: "title"},
		&core.RelationField{Name: "household", CollectionId: hCol.Id, MaxSelect: 1},
		&core.RelationField{Name: "member", CollectionId: mCol.Id, MaxSelect: 1},
	)
	rule := newsletterRule
	items.ListRule, items.ViewRule = &rule, &rule
	mustSave(t, app, items)

	item := func(m *core.Record) *core.Record {
		r := core.NewRecord(items)
		r.Set("title", "for "+m.GetString("name"))
		r.Set("member", m.Id)
		mustSave(t, app, r)

		return r
	}

	forKid1, forKid2 := item(w.kid1), item(w.kid2)
	all := "ann,gwen,mix,ned,pat,qed,yan,zed" // every signed-in user; a guest is never let in

	if got := w.whoCan(t, forKid1, newsletterRule); got != "" {
		t.Fatalf("before any consent: %q", got)
	}

	// granted: the row opens
	if _, err := core.ConsentSelf(app, w.ctx, w.users["mix"], "newsletter", w.kid1.Id, core.ConsentActionGrant); err != nil {
		t.Fatal(err)
	}

	if got := w.whoCan(t, forKid1, newsletterRule); got != all {
		t.Fatalf("after the grant: %q", got)
	}

	// another purpose and another person do not open it
	consentPurpose(t, w, map[string]any{"key": "photos", "name": "Photos", "text": "Photos."})

	if _, err := core.ConsentSelf(app, w.ctx, w.users["qed"], "photos", w.kid2.Id, core.ConsentActionGrant); err != nil {
		t.Fatal(err)
	}

	if got := w.whoCan(t, forKid2, newsletterRule); got != "" {
		t.Fatalf("kid2 consented to another purpose: %q", got)
	}

	// withdrawn: the date is set, so the empty-date comparison fails
	if _, err := core.ConsentSelf(app, w.ctx, w.users["ned"], "newsletter", w.kid1.Id, core.ConsentActionWithdraw); err != nil {
		t.Fatal(err)
	}

	if got := w.whoCan(t, forKid1, newsletterRule); got != "" {
		t.Fatalf("after the withdrawal: %q", got)
	}

	// granted again: open again
	if _, err := core.ConsentSelf(app, w.ctx, w.users["mix"], "newsletter", w.kid1.Id, core.ConsentActionGrant); err != nil {
		t.Fatal(err)
	}

	if got := w.whoCan(t, forKid1, newsletterRule); got != all {
		t.Fatalf("after the second grant: %q", got)
	}

	// a user subject: only the person who consented sees their own
	own := `@collection._consents:c.subject ?= @request.auth.id && @collection._consents:c.purpose ?= "newsletter" && @collection._consents:c.withdrawnAt ?= ""`

	if _, err := core.ConsentSelf(app, w.ctx, w.users["yan"], "newsletter", "", core.ConsentActionGrant); err != nil {
		t.Fatal(err)
	}

	if got := w.whoCan(t, forKid1, own); got != "yan" {
		t.Fatalf("user subject: %q", got)
	}
}

func TestConsentExportIncludesOwnConsentsAndHistory(t *testing.T) {
	w, _ := consentFixture(t)
	app := w.f.app

	// yan consents as a user, then withdraws; staff also record a consent on
	// paper under yan's household member row; a guardian consents for kid1
	for _, a := range []string{core.ConsentActionGrant, core.ConsentActionWithdraw} {
		if _, err := core.ConsentSelf(app, w.ctx, w.users["yan"], "newsletter", "", a); err != nil {
			t.Fatal(err)
		}
	}

	consentPurpose(t, w, map[string]any{"key": "photos", "name": "Photos", "text": "Photos."})

	if _, err := core.ConsentStaff(app, w.ctx, w.users["pat"], w.P, core.ConsentStaffInput{
		Purpose: "photos", Member: w.yanM.Id, Action: "grant", Method: "paper", EvidenceRef: "form 7",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := core.ConsentSelf(app, w.ctx, w.users["mix"], "newsletter", w.kid1.Id, core.ConsentActionGrant); err != nil {
		t.Fatal(err)
	}

	z := buildExport(t, app, w.users["yan"])

	states := z.rows(t, core.CollectionNameConsents)
	events := z.rows(t, core.CollectionNameConsentEvents)

	purposes := func(rows []map[string]any) []string {
		var out []string
		for _, r := range rows {
			out = append(out, r["purpose"].(string))
		}

		slices.Sort(out)

		return out
	}

	if got := purposes(states); !slices.Equal(got, []string{"newsletter", "photos"}) {
		t.Errorf("consents: %v", got)
	}

	// grant, withdraw (user) and the paper grant (member row)
	if got := purposes(events); !slices.Equal(got, []string{"newsletter", "newsletter", "photos"}) {
		t.Errorf("events: %v", got)
	}

	for _, r := range events {
		if r["text"] == "" || r["action"] == "" {
			t.Errorf("event without text or action: %v", r)
		}
	}

	// a guardian's export does not carry the child's consents (a minor's row is not reached through a login)
	zm := buildExport(t, app, w.users["mix"])
	if rows := zm.rows(t, core.CollectionNameConsents); len(rows) != 0 {
		t.Errorf("the guardian's export holds %d rows about the child", len(rows))
	}

	if strings.Contains(z.all(), w.kid1.Id) {
		t.Error("the export names another person's member row")
	}

	if z.manifest.Partial() {
		t.Errorf("partial export: %+v", z.manifest.Errors)
	}
}
