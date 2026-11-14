package core_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// Fork-local (Claviger #88, 88-3): the retention engine and legal holds.
//
// World: an app collection ret_items (title and note are personal, note is
// encrypted, owner links to a user) with a data map row that makes it
// anonymizable. Rows are aged by writing the created column directly.

type retWorld struct {
	app   *tests.TestApp
	col   *core.Collection
	users []*core.Record
	ctx   context.Context
}

func newRetWorld(t testing.TB) *retWorld {
	t.Helper()

	setTestKEK(t)

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)

	w := &retWorld{app: app, ctx: core.WithAuditContext(context.Background(), core.AuditActor{Type: core.AuditActorTypeSystem}, "")}

	usersCol, _ := app.FindCollectionByNameOrId("users")
	for _, n := range []string{"ann", "bob"} {
		u := core.NewRecord(usersCol)
		u.SetEmail(n + "@retention.test")
		u.SetPassword("password123")
		must(t, app.Save(u))
		w.users = append(w.users, u)
	}

	col := core.NewBaseCollection("ret_items")
	col.Fields.Add(
		&core.TextField{Name: "title"},
		&core.EncryptedTextField{Name: "note", Max: 1000},
		&core.RelationField{Name: "owner", CollectionId: usersCol.Id, MaxSelect: 1},
		&core.BoolField{Name: "archived"},
		&core.DateField{Name: "expires"},
		&core.AutodateField{Name: "created", OnCreate: true},
		&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
	)
	must(t, app.Save(col))
	w.col = col

	dm, _ := app.FindCollectionByNameOrId(core.CollectionNameDataMap)
	row := core.NewRecord(dm)
	row.Set("collection", "ret_items")
	row.Set("treatment", core.DataMapTreatmentAnonymize)
	row.Set("subjectPaths", []string{"owner"})
	row.Set("personalFields", []string{"title", "note"})
	must(t, app.Save(row))

	return w
}

// item creates a row that is `ageDays` old.
func (w *retWorld) item(t testing.TB, title string, ageDays int, owner *core.Record) *core.Record {
	t.Helper()

	r := core.NewRecord(w.col)
	r.Set("title", title)
	r.Set("note", "private "+title)

	if owner != nil {
		r.Set("owner", owner.Id)
	}

	must(t, w.app.Save(r))

	old := time.Now().UTC().AddDate(0, 0, -ageDays).Format("2006-01-02 15:04:05.000Z")
	_, err := w.app.DB().NewQuery("UPDATE {{ret_items}} SET [[created]]={:c} WHERE [[id]]={:id}").Bind(dbx.Params{"c": old, "id": r.Id}).Execute()
	must(t, err)

	return r
}

func (w *retWorld) count(t testing.TB) int {
	t.Helper()

	n, err := w.app.CountRecords("ret_items")
	must(t, err)

	return int(n)
}

// policy creates a policy and, when live is set, arms it.
func (w *retWorld) policy(t testing.TB, action string, live bool, mutate ...func(*core.RetentionPolicyInput)) *core.Record {
	t.Helper()

	str := func(s string) *string { return &s }
	days, one := 30, true

	in := core.RetentionPolicyInput{
		Collection: str("ret_items"), Basis: str("created"), After: &days, Unit: str("days"), Action: str(action),
		// a request body cannot arm a policy at creation
		Enabled: &one,
	}
	for _, m := range mutate {
		m(&in)
	}

	p, err := core.CreateRetentionPolicy(w.app, w.ctx, in)
	must(t, err)

	if p.GetBool("enabled") || !p.GetBool("dryRun") {
		t.Fatalf("a new policy must be disabled and a dry run: enabled=%v dryRun=%v", p.GetBool("enabled"), p.GetBool("dryRun"))
	}

	if live {
		w.arm(t, p)
	}

	return p
}

func (w *retWorld) arm(t testing.TB, p *core.Record) {
	t.Helper()

	on, off := true, false
	_, err := core.UpdateRetentionPolicy(w.app, w.ctx, p.Id, core.RetentionPolicyInput{Enabled: &on, DryRun: &off})
	must(t, err)
}

func (w *retWorld) run(t testing.TB, opts core.RetentionRunOptions) *core.RetentionRunResult {
	t.Helper()

	res, err := core.RunRetention(w.ctx, w.app, opts)
	must(t, err)

	return res
}

func TestRetentionPolicyStartsDisabledAndDry(t *testing.T) {
	w := newRetWorld(t)
	p := w.policy(t, "delete", false)

	if p.GetBool("enabled") || !p.GetBool("dryRun") {
		t.Fatal("not disabled and dry")
	}

	// nothing is seeded
	all, _ := w.app.FindAllRecords(core.CollectionNameRetentionPolicies)
	if len(all) != 1 {
		t.Fatalf("only the policy made here exists, got %d", len(all))
	}

	// the records API route is closed; so is a direct save outside the functions
	if err := w.app.Save(core.NewRecord(p.Collection())); err == nil {
		t.Fatal("a policy saved outside CreateRetentionPolicy must be refused")
	}
}

func TestRetentionDryRunChangesNothing(t *testing.T) {
	w := newRetWorld(t)
	for i := range 5 {
		w.item(t, fmt.Sprintf("old%d", i), 60, nil)
	}
	for i := range 2 {
		w.item(t, fmt.Sprintf("new%d", i), 3, nil)
	}

	// a live-looking policy whose dryRun flag is still on
	p := w.policy(t, "delete", false)
	on := true
	_, err := core.UpdateRetentionPolicy(w.app, w.ctx, p.Id, core.RetentionPolicyInput{Enabled: &on})
	must(t, err)

	res := w.run(t, core.RetentionRunOptions{})
	if len(res.Policies) != 1 || !res.Policies[0].DryRun || res.Policies[0].Matched != 5 || res.Policies[0].Affected != 5 {
		t.Fatalf("dry run counts: %+v", res.Policies)
	}

	if w.count(t) != 7 {
		t.Fatalf("a dry run deleted rows: %d left", w.count(t))
	}

	// a preview of a policy that is not even enabled changes nothing either
	w.arm(t, p)

	prev := w.run(t, core.RetentionRunOptions{Preview: true, PolicyId: p.Id})
	if !prev.Preview || !prev.Policies[0].DryRun || prev.Policies[0].Affected != 5 || w.count(t) != 7 {
		t.Fatalf("preview: %+v rows=%d", prev.Policies, w.count(t))
	}

	if n := len(auditEntriesFor(t, w.app, core.AuditActionRetentionRun)); n != 2 {
		t.Fatalf("one audit entry per run, got %d", n)
	}

	// the live run is the same count, and changes the rows
	live := w.run(t, core.RetentionRunOptions{})
	if live.Policies[0].DryRun || live.Policies[0].Affected != 5 || w.count(t) != 2 {
		t.Fatalf("live: %+v rows=%d", live.Policies, w.count(t))
	}
}

func TestRetentionActions(t *testing.T) {
	t.Run("delete", func(t *testing.T) {
		w := newRetWorld(t)
		old := w.item(t, "old", 90, nil)
		fresh := w.item(t, "fresh", 1, nil)
		w.policy(t, "delete", true)

		res := w.run(t, core.RetentionRunOptions{})
		if res.Policies[0].Affected != 1 {
			t.Fatalf("%+v", res.Policies[0])
		}

		if _, err := w.app.FindRecordById("ret_items", old.Id); err == nil {
			t.Fatal("the old row must be gone")
		}

		if _, err := w.app.FindRecordById("ret_items", fresh.Id); err != nil {
			t.Fatal("the fresh row must stay")
		}

		// no per-row delete entry: it would keep the deleted data in the log
		if n := len(auditEntriesFor(t, w.app, core.AuditActionDelete)); n != 0 {
			for _, e := range auditEntriesFor(t, w.app, core.AuditActionDelete) {
				if e.GetString("collectionName") == "ret_items" {
					t.Fatalf("a delete entry for a retention delete: %s", e.GetString("before"))
				}
			}
		}
	})

	t.Run("anonymize", func(t *testing.T) {
		w := newRetWorld(t)
		owner := w.users[0]
		old := w.item(t, "old", 90, owner)
		w.policy(t, "anonymize", true)

		res := w.run(t, core.RetentionRunOptions{})
		if res.Policies[0].Affected != 1 {
			t.Fatalf("%+v", res.Policies[0])
		}

		got, err := w.app.FindRecordById("ret_items", old.Id)
		must(t, err)

		if got.GetString("title") != "" || got.GetString("note") != "" {
			t.Fatalf("personal fields must be blank: %q %q", got.GetString("title"), got.GetString("note"))
		}

		if raw := rawColumn(t, w.app, w.col, old.Id, "note"); strings.Contains(raw, "private") {
			t.Fatalf("the note is still in the row: %q", raw)
		}

		if got.GetString("owner") != owner.Id {
			t.Fatal("the owner link is not a personal field of the map and stays")
		}

		// a second run finds nothing to change
		again := w.run(t, core.RetentionRunOptions{})
		if again.Policies[0].Affected != 0 || again.Policies[0].Unchanged != 1 {
			t.Fatalf("second run: %+v", again.Policies[0])
		}
	})

	t.Run("archive", func(t *testing.T) {
		w := newRetWorld(t)
		old := w.item(t, "old", 90, nil)
		w.policy(t, "archive", true)

		res := w.run(t, core.RetentionRunOptions{})
		if res.Policies[0].Affected != 1 {
			t.Fatalf("%+v", res.Policies[0])
		}

		got, _ := w.app.FindRecordById("ret_items", old.Id)
		if !got.GetBool("archived") || got.GetString("title") != "old" {
			t.Fatal("archive sets the flag and keeps the row")
		}

		if again := w.run(t, core.RetentionRunOptions{}); again.Policies[0].Unchanged != 1 || again.Policies[0].Affected != 0 {
			t.Fatalf("second run: %+v", again.Policies[0])
		}
	})

	t.Run("named date field and filter", func(t *testing.T) {
		w := newRetWorld(t)
		a := w.item(t, "a", 1, nil)
		b := w.item(t, "b", 1, nil)
		past := time.Now().UTC().AddDate(0, 0, -2).Format("2006-01-02 15:04:05.000Z")
		a.Set("expires", past)
		b.Set("expires", past)
		must(t, w.app.Save(a))
		must(t, w.app.Save(b))

		zero := 0
		basis, filter := "expires", `title = "a"`
		w.policy(t, "delete", true, func(in *core.RetentionPolicyInput) { in.Basis, in.After, in.Filter = &basis, &zero, &filter })

		res := w.run(t, core.RetentionRunOptions{})
		if res.Policies[0].Affected != 1 || w.count(t) != 1 {
			t.Fatalf("%+v rows=%d", res.Policies[0], w.count(t))
		}
	})
}

func TestRetentionRefusals(t *testing.T) {
	w := newRetWorld(t)
	str := func(s string) *string { return &s }
	zero, thirty := 0, 30

	create := func(mutate func(*core.RetentionPolicyInput)) error {
		in := core.RetentionPolicyInput{Collection: str("ret_items"), Basis: str("created"), After: &thirty, Unit: str("days"), Action: str("delete")}
		mutate(&in)
		_, err := core.CreateRetentionPolicy(w.app, w.ctx, in)

		return err
	}

	// permanent registers (#87), their annotations, accounts and system tables
	cfg, err := core.CreateSacramentalRegister(w.app, core.SacramentalRegisterOpts{Name: "baptisms"})
	must(t, err)

	for _, name := range []string{"baptisms", cfg.AnnotationCollection, "users", core.CollectionNameAuditLog, core.CollectionNameLegalHolds, core.CollectionNamePermanentCollections} {
		if name == "" {
			continue
		}

		if err := create(func(in *core.RetentionPolicyInput) { in.Collection = str(name); in.Basis = str("created") }); err == nil {
			t.Fatalf("a policy on %s must be refused", name)
		}
	}

	// the data map needs personal fields before an anonymize policy is enabled
	other := core.NewBaseCollection("ret_other")
	other.Fields.Add(&core.TextField{Name: "title"}, &core.AutodateField{Name: "created", OnCreate: true})
	must(t, w.app.Save(other))

	p, err := core.CreateRetentionPolicy(w.app, w.ctx, core.RetentionPolicyInput{
		Collection: str("ret_other"), Basis: str("created"), After: &thirty, Unit: str("days"), Action: str("anonymize"),
	})
	must(t, err) // disabled: allowed

	on := true
	if _, err := core.UpdateRetentionPolicy(w.app, w.ctx, p.Id, core.RetentionPolicyInput{Enabled: &on}); err == nil ||
		!strings.Contains(err.Error(), "data map") {
		t.Fatalf("enabling anonymize without data map fields must be refused: %v", err)
	}

	// other bad input
	bad := map[string]func(*core.RetentionPolicyInput){
		"unknown collection":   func(in *core.RetentionPolicyInput) { in.Collection = str("nope") },
		"unknown basis":        func(in *core.RetentionPolicyInput) { in.Basis = str("title") },
		"zero on created":      func(in *core.RetentionPolicyInput) { in.After = &zero },
		"archive no field":     func(in *core.RetentionPolicyInput) { in.Collection = str("ret_other"); in.Action = str("archive") },
		"age out no birthDate": func(in *core.RetentionPolicyInput) { in.Basis = str("ageOut") },
		"bad filter":           func(in *core.RetentionPolicyInput) { in.Filter = str("nope = 1") },
		"request filter":       func(in *core.RetentionPolicyInput) { in.Filter = str(`@request.auth.id != ""`) },
	}

	for name, m := range bad {
		if err := create(m); err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
}

func TestRetentionSkipsLegalHolds(t *testing.T) {
	w := newRetWorld(t)

	byRecord := w.item(t, "held record", 90, nil)
	byPerson := w.item(t, "held person", 90, w.users[0])
	free := w.item(t, "free", 90, w.users[1])

	su, err := w.app.FindAuthRecordByEmail(core.CollectionNameSuperusers, "test@example.com")
	if err != nil {
		su = nil
	}

	h1, err := core.PlaceLegalHold(w.app, w.ctx, su, core.LegalHoldInput{Scope: "record", Collection: "ret_items", RecordId: byRecord.Id, Reason: "claim", Reference: "case 1"})
	must(t, err)

	_, err = core.PlaceLegalHold(w.app, w.ctx, su, core.LegalHoldInput{Scope: "subject", Subject: w.users[0].Id, Reason: "claim 2"})
	must(t, err)

	w.policy(t, "delete", true)

	res := w.run(t, core.RetentionRunOptions{})
	if got := res.Policies[0]; got.Matched != 3 || got.Held != 2 || got.Affected != 1 {
		t.Fatalf("%+v", got)
	}

	if _, err := w.app.FindRecordById("ret_items", byRecord.Id); err != nil {
		t.Fatal("a record under a hold must stay")
	}

	if _, err := w.app.FindRecordById("ret_items", byPerson.Id); err != nil {
		t.Fatal("a row about a held person must stay")
	}

	if _, err := w.app.FindRecordById("ret_items", free.Id); err == nil {
		t.Fatal("an unheld row is deleted")
	}

	// nothing else deletes a held record either
	if err := w.app.Delete(byRecord); err == nil {
		t.Fatal("a held record must not be deletable")
	}

	// lifting frees the row, and the hold stays as evidence
	_, err = core.LiftLegalHold(w.app, w.ctx, h1.Id, su)
	must(t, err)

	if _, err := core.LiftLegalHold(w.app, w.ctx, h1.Id, su); !errors.Is(err, core.ErrLegalHoldLifted) {
		t.Fatalf("a second lift: %v", err)
	}

	if held, _ := core.IsRecordLegallyHeld(w.app, byRecord); held {
		t.Fatal("lifted")
	}

	for _, action := range []string{core.AuditActionLegalHold, core.AuditActionLegalHoldLift} {
		es := auditEntriesFor(t, w.app, action)
		if len(es) == 0 {
			t.Fatalf("no %s entry", action)
		}

		for _, e := range es {
			if raw := e.GetString("meta"); strings.Contains(raw, "claim") || strings.Contains(raw, "case 1") {
				t.Fatalf("the reason or reference leaked into the audit entry: %s", raw)
			}
		}
	}

	// the table is not writable around the functions
	hc, _ := w.app.FindCollectionByNameOrId(core.CollectionNameLegalHolds)
	if err := w.app.Save(core.NewRecord(hc)); err == nil {
		t.Fatal("a hold saved around PlaceLegalHold must be refused")
	}
}

func TestRetentionAnonymizeAndAnonymizedHoldDoNotCollide(t *testing.T) {
	w := newRetWorld(t)
	held := w.item(t, "held", 90, w.users[0])

	_, err := core.PlaceLegalHold(w.app, w.ctx, nil, core.LegalHoldInput{Scope: "subject", Subject: w.users[0].Id, Reason: "r"})
	must(t, err)

	w.policy(t, "anonymize", true)

	res := w.run(t, core.RetentionRunOptions{})
	if res.Policies[0].Held != 1 || res.Policies[0].Affected != 0 {
		t.Fatalf("%+v", res.Policies[0])
	}

	got, _ := w.app.FindRecordById("ret_items", held.Id)
	if got.GetString("title") != "held" {
		t.Fatal("a held row is not anonymized")
	}
}

func TestRetentionSkipsOpenPastoralHolds(t *testing.T) {
	setTestKEK(t)

	f := newOrgFixture(t)
	app := f.app

	org := f.org1
	col, err := core.CreatePastoralNotes(app, core.PastoralNotesOpts{})
	must(t, err)

	mk := func(label string) *core.Record {
		r := core.NewRecord(col)
		r.Set("org", org.Id)
		r.Set("author", f.alice.Id)
		r.Set("visibility", "private")
		r.Set("body", label)
		must(t, app.Save(r))

		return r
	}

	held, other := mk("held"), mk("plain")

	_, err = app.DB().NewQuery("UPDATE {{" + col.Name + "}} SET [[created]]='2000-01-01 00:00:00.000Z'").Execute()
	must(t, err)

	hc, _ := app.FindCollectionByNameOrId(core.CollectionNamePastoralHolds)
	h := core.NewRecord(hc)
	h.Set("collection", col.Name)
	h.Set("recordId", held.Id)
	h.Set("org", org.Id)
	h.Set("subject", f.bob.Id)
	h.Set("state", core.PastoralHoldHeld)
	must(t, app.Save(h))

	ctx := core.WithAuditContext(context.Background(), core.AuditActor{Type: core.AuditActorTypeSystem}, "")
	str := func(s string) *string { return &s }
	days := 30

	p, err := core.CreateRetentionPolicy(app, ctx, core.RetentionPolicyInput{Collection: str(col.Name), Basis: str("created"), After: &days, Unit: str("days"), Action: str("delete")})
	must(t, err)

	on, off := true, false
	_, err = core.UpdateRetentionPolicy(app, ctx, p.Id, core.RetentionPolicyInput{Enabled: &on, DryRun: &off})
	must(t, err)

	res, err := core.RunRetention(ctx, app, core.RetentionRunOptions{})
	must(t, err)

	if got := res.Policies[0]; got.Held != 1 || got.Affected != 1 {
		t.Fatalf("%+v", got)
	}

	if _, err := app.FindRecordById(col, held.Id); err != nil {
		t.Fatal("a note with an open pastoral hold must stay")
	}

	if _, err := app.FindRecordById(col, other.Id); err == nil {
		t.Fatal("an unheld note is deleted")
	}

	// once the review decided, the note is no longer held
	h.Set("state", core.PastoralHoldReleased)
	must(t, app.Save(h))

	res, err = core.RunRetention(ctx, app, core.RetentionRunOptions{})
	must(t, err)

	if res.Policies[0].Affected != 1 {
		t.Fatalf("%+v", res.Policies[0])
	}
}

func TestRetentionAgeOutReadsEncryptedBirthDate(t *testing.T) {
	w := newHHWorld(t)
	app := w.f.app

	if raw := rawColumn(t, app, w.kid1.Collection(), w.yanM.Id, "birthDate"); !strings.HasPrefix(raw, "enc:v1:") {
		t.Fatalf("birthDate must be encrypted at rest: %q", raw)
	}

	str := func(s string) *string { return &s }
	one, two := 1, 2

	// yan (19) became an adult 1 year and 3 months ago; kid1 (10) is a minor
	mk := func(after int, filter string) *core.Record {
		p, err := core.CreateRetentionPolicy(app, w.ctx, core.RetentionPolicyInput{
			Collection: str(core.CollectionNameHouseholdMembers), Basis: str("ageOut"), After: &after, Unit: str("years"),
			Action: str("anonymize"), Filter: str(filter),
		})
		must(t, err)

		return p
	}

	p1 := mk(one, `role = "adult" || role = "child"`)

	res, err := core.RunRetention(w.ctx, app, core.RetentionRunOptions{Preview: true, PolicyId: p1.Id})
	must(t, err)

	if got := res.Policies[0]; got.Matched != 1 || got.Affected != 1 {
		t.Fatalf("one year after yan graduated: %+v", got)
	}

	p2 := mk(two, `role = "adult" || role = "child"`)

	res, err = core.RunRetention(w.ctx, app, core.RetentionRunOptions{Preview: true, PolicyId: p2.Id})
	must(t, err)

	if got := res.Policies[0]; got.Matched != 0 {
		t.Fatalf("two years: %+v", got)
	}

	// live
	on, off := true, false
	_, err = core.UpdateRetentionPolicy(app, w.ctx, p1.Id, core.RetentionPolicyInput{Enabled: &on, DryRun: &off})
	must(t, err)

	res, err = core.RunRetention(w.ctx, app, core.RetentionRunOptions{})
	must(t, err)

	if got := res.Policies[0]; got.Affected != 1 {
		t.Fatalf("%+v", got)
	}

	yan, _ := app.FindRecordById(core.CollectionNameHouseholdMembers, w.yanM.Id)
	kid, _ := app.FindRecordById(core.CollectionNameHouseholdMembers, w.kid1.Id)

	if yan.GetString("name") != "" || yan.GetString("birthDate") != "" {
		t.Fatalf("yan is anonymized: %q %q", yan.GetString("name"), yan.GetString("birthDate"))
	}

	if kid.GetString("name") != "Kid One" {
		t.Fatal("a minor is never reached by the age-out basis")
	}
}

func TestRetentionResumesAfterInterruption(t *testing.T) {
	w := newRetWorld(t)

	const total = 9

	for i := range total {
		w.item(t, fmt.Sprintf("old%02d", i), 90, nil)
	}

	w.item(t, "fresh", 1, nil)
	w.policy(t, "delete", true)

	// stop after the second batch of two rows, like a restart would
	stopped := errors.New("restart")

	core.SetRetentionBatchHook(func(_ string, batch int) error {
		if batch == 2 {
			return stopped
		}

		return nil
	})
	t.Cleanup(func() { core.SetRetentionBatchHook(nil) })

	_, err := core.RunRetention(w.ctx, w.app, core.RetentionRunOptions{BatchSize: 2})
	if !errors.Is(err, stopped) {
		t.Fatalf("the first run must stop: %v", err)
	}

	if w.count(t) != total+1-4 {
		t.Fatalf("two batches of two rows were processed: %d left", w.count(t))
	}

	if n := len(auditEntriesFor(t, w.app, core.AuditActionRetentionRun)); n != 0 {
		t.Fatalf("an interrupted run writes no entry yet: %d", n)
	}

	ps, _ := w.app.FindAllRecords(core.CollectionNameRetentionPolicies)
	if ps[0].GetString("runStatus") != "running" {
		t.Fatalf("the run is marked running: %q", ps[0].GetString("runStatus"))
	}

	// a policy in the middle of a run cannot be edited
	off := false
	if _, err := core.UpdateRetentionPolicy(w.app, w.ctx, ps[0].Id, core.RetentionPolicyInput{Enabled: &off}); !errors.Is(err, core.ErrRetentionRunning) {
		t.Fatalf("edit during a run: %v", err)
	}

	core.SetRetentionBatchHook(nil)

	res, err := core.RunRetention(w.ctx, w.app, core.RetentionRunOptions{BatchSize: 2})
	must(t, err)

	if !res.Resumed {
		t.Fatal("the second run resumes the first")
	}

	// every row exactly once: the counts add up across the two segments
	if got := res.Policies[0]; got.Affected != total || got.Matched != total || got.Failed != 0 {
		t.Fatalf("totals across the restart: %+v", got)
	}

	if w.count(t) != 1 {
		t.Fatalf("only the fresh row is left: %d", w.count(t))
	}

	es := auditEntriesFor(t, w.app, core.AuditActionRetentionRun)
	if len(es) != 1 || !strings.Contains(strings.ReplaceAll(es[0].GetString("meta"), " ", ""), `"resumed":true`) {
		t.Fatalf("one entry for the whole run, marked resumed: %d", len(es))
	}

	ps, _ = w.app.FindAllRecords(core.CollectionNameRetentionPolicies)
	if ps[0].GetString("runStatus") != "" || ps[0].GetDateTime("lastRunAt").IsZero() {
		t.Fatal("the run state is cleared and the result stored")
	}

	// a third run is a fresh run that finds nothing
	third := w.run(t, core.RetentionRunOptions{})
	if third.Resumed || third.Policies[0].Matched != 0 {
		t.Fatalf("%+v", third)
	}
}

func TestRetentionAuditEntryHoldsCountsOnly(t *testing.T) {
	w := newRetWorld(t)
	w.item(t, "Secret Name", 90, nil)

	name := "Quarterly cleanup of secret things"
	w.policy(t, "delete", true, func(in *core.RetentionPolicyInput) { in.Name = &name })

	w.run(t, core.RetentionRunOptions{})

	es := auditEntriesFor(t, w.app, core.AuditActionRetentionRun)
	if len(es) != 1 {
		t.Fatalf("one entry per run, got %d", len(es))
	}

	raw := strings.ReplaceAll(es[0].GetString("meta"), " ", "")
	for _, leak := range []string{"SecretName", "private", "secretthings"} {
		if strings.Contains(raw, leak) {
			t.Fatalf("content in the audit entry: %s", raw)
		}
	}

	if !strings.Contains(raw, `"affected":1`) {
		t.Fatalf("counts per policy: %s", raw)
	}
}

func TestRetentionDeletesConsentEvidence(t *testing.T) {
	w := newRetWorld(t)

	col, err := w.app.FindCollectionByNameOrId(core.CollectionNameConsentEvents)
	must(t, err)

	ev := core.NewRecord(col)
	ev.Set("subject", w.users[0].Id)
	ev.Set("subjectType", "user")
	ev.Set("purpose", "newsletter")
	ev.Set("action", "grant")
	ev.Set("method", "web")
	must(t, w.app.SaveNoValidate(ev))

	_, err = w.app.DB().NewQuery("UPDATE {{" + col.Name + "}} SET [[created]]='2000-01-01 00:00:00.000Z'").Execute()
	must(t, err)

	// outside the job the table is append-only
	if err := w.app.Delete(ev); err == nil {
		t.Fatal("consent events are append-only")
	}

	str := func(s string) *string { return &s }
	days := 365

	p, err := core.CreateRetentionPolicy(w.app, w.ctx, core.RetentionPolicyInput{
		Collection: str(core.CollectionNameConsentEvents), Basis: str("created"), After: &days, Unit: str("days"), Action: str("delete"),
	})
	must(t, err)

	w.arm(t, p)

	res := w.run(t, core.RetentionRunOptions{})
	if res.Policies[0].Affected != 1 {
		t.Fatalf("%+v", res.Policies[0])
	}
}

func TestRetentionWebhookEventFilters(t *testing.T) {
	for _, ev := range []string{
		core.CollectionNameRetentionPolicies + ":" + core.AuditActionRetentionRun,
		core.CollectionNameLegalHolds + ":" + core.AuditActionLegalHold,
		core.CollectionNameLegalHolds + ":" + core.AuditActionLegalHoldLift,
	} {
		if err := core.ValidateWebhookEvents([]string{ev}); err != nil {
			t.Errorf("%s: %v", ev, err)
		}
	}
}

func TestUpgradePastoralExpiryAddsFieldsAndKeepsRulesLocked(t *testing.T) {
	setTestKEK(t)

	f := newOrgFixture(t)

	col, err := core.CreatePastoralNotes(f.app, core.PastoralNotesOpts{})
	must(t, err)

	// what a collection made before 86-5 looks like
	col.Fields.RemoveByName("kind")
	col.Fields.RemoveByName("archivedAt")

	legacy := strings.Replace(*col.UpdateRule, ` && @request.body.kind:isset = false && @request.body.archivedAt:isset = false`, "", 1)
	col.UpdateRule = &legacy
	must(t, f.app.Save(col))

	changed, err := core.UpgradePastoralExpiry(f.app)
	must(t, err)

	if len(changed) != 1 {
		t.Fatalf("one collection upgraded: %v", changed)
	}

	col, _ = f.app.FindCollectionByNameOrId(col.Name)
	if col.Fields.GetByName("kind") == nil || col.Fields.GetByName("archivedAt") == nil {
		t.Fatal("fields added")
	}

	want := core.PastoralRules(col.Name)
	if *col.UpdateRule != *want.Update {
		t.Fatal("the locked update rule is the new text")
	}

	// idempotent
	if again, _ := core.UpgradePastoralExpiry(f.app); len(again) != 0 {
		t.Fatalf("second run changed %v", again)
	}
}
