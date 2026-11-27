package core_test

import (
	"strings"
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #88, 88-7): restriction of processing and the optional
// anonymization of permanent register entries.

func (w *regWorld) auditRows(t testing.TB, action string) []*core.Record {
	t.Helper()

	rows, err := w.app.FindAllRecords(core.CollectionNameAuditLog, dbx.HashExp{"action": action})
	must(t, err)

	return rows
}

func TestRegisterRestrictAndLift(t *testing.T) {
	w := newRegWorld(t, 1)
	a := w.draft(t, "Smith")
	b := w.draft(t, "Jones")
	w.finalize(t, "reg1", a)
	w.finalize(t, "reg1", b)

	type tc struct {
		name   string
		who    string
		entry  *core.Record
		reason string
		status int
	}

	for _, c := range []tc{
		{"draft", "reg1", w.draft(t, "Draft"), "r", 409},
		{"reason required", "reg1", a, " ", 400},
		{"admin is not a registrar", "adm", a, "r", 403},
		{"owner is not a registrar", "own", a, "r", 403},
		{"plain member", "mem", a, "r", 403},
		{"stranger", "out", a, "r", 404},
	} {
		if _, err := core.RestrictRegisterEntry(w.app, w.ctx, w.user[c.who], "baptisms", c.entry.Id, c.reason); statusOf(err) != c.status {
			t.Errorf("%s: status %d (%v), want %d", c.name, statusOf(err), err, c.status)
		}
	}

	su := w.superuser(t)
	if _, err := core.RestrictRegisterEntry(w.app, w.ctx, su, "baptisms", a.Id, "r"); statusOf(err) != 403 {
		t.Fatalf("a superuser cannot restrict: %v", err)
	}

	before := mustVerify(t, w.app)

	got, err := core.RestrictRegisterEntry(w.app, w.ctx, w.user["reg1"], "baptisms", a.Id, "Person objects to accuracy")
	must(t, err)

	if !got.GetBool("restricted") || w.fresh(t, a).GetString("surname") != "Smith" {
		t.Fatal("restricted, content untouched")
	}

	if _, err := core.RestrictRegisterEntry(w.app, w.ctx, w.user["reg2"], "baptisms", a.Id, "again"); statusOf(err) != 409 {
		t.Fatalf("already restricted: %v", err)
	}

	notes, _ := core.RegisterAnnotations(w.app, w.cfg, w.fresh(t, a))
	if len(notes) != 1 || notes[0].GetString("kind") != core.RegisterKindDataSubject || notes[0].GetString("author") != w.user["reg1"].Id ||
		notes[0].GetString("reason") != "Person objects to accuracy" {
		t.Fatalf("marginal note: %+v", notes)
	}

	if len(w.auditRows(t, core.AuditActionRegisterRestrict)) != 1 {
		t.Fatal("restriction must be audited")
	}

	if after := mustVerify(t, w.app); !after.OK() || after.HeadHash != before.HeadHash {
		t.Fatalf("chain must be unchanged: %+v", after)
	}

	// lifting
	if _, err := core.LiftRegisterRestriction(w.app, w.ctx, w.user["adm"], "baptisms", a.Id, "r"); statusOf(err) != 403 {
		t.Fatalf("only a registrar lifts: %v", err)
	}

	if _, err := core.LiftRegisterRestriction(w.app, w.ctx, w.user["reg2"], "baptisms", b.Id, "r"); statusOf(err) != 409 {
		t.Fatalf("not restricted: %v", err)
	}

	if _, err := core.LiftRegisterRestriction(w.app, w.ctx, w.user["reg2"], "baptisms", a.Id, "Resolved"); err != nil {
		t.Fatal(err)
	}

	if w.fresh(t, a).GetBool("restricted") {
		t.Fatal("restriction must be lifted")
	}

	notes, _ = core.RegisterAnnotations(w.app, w.cfg, w.fresh(t, a))
	if len(notes) != 2 || notes[1].GetString("kind") != core.RegisterKindLegalNote {
		t.Fatalf("lift note: %+v", notes)
	}

	if len(w.auditRows(t, core.AuditActionRegisterUnrestrict)) != 1 || !mustVerify(t, w.app).OK() {
		t.Fatal("lifting must be audited and the chain must hold")
	}
}

func TestRegisterRestrictedReadsAreGated(t *testing.T) {
	w := newRegWorld(t, 1)
	d := w.draft(t, "Smith")
	w.finalize(t, "reg1", d)

	read := func(who *core.Record, reason string) *core.RegisterReadResult {
		t.Helper()

		r, err := core.ReadRegisterEntry(w.app, w.ctx, who, "baptisms", d.Id, reason)
		must(t, err)

		return r
	}

	if r := read(w.user["adm"], ""); !r.Revealed || r.Restricted {
		t.Fatal("an unrestricted entry is readable by staff")
	}

	// the erasure flow restricts the entry
	_, err := core.AnnotateErasureRequest(w.app, w.ctx, w.fresh(t, d), "Erasure request")
	must(t, err)

	for _, c := range []struct {
		name      string
		who       *core.Record
		reason    string
		revealed  bool
		canReveal bool
	}{
		{"admin, no reason", w.user["adm"], "", false, false},
		{"admin with a reason", w.user["adm"], "curious", false, false},
		{"owner with a reason", w.user["own"], "curious", false, false},
		{"registrar, no reason", w.user["reg1"], "", false, true},
		{"registrar, blank reason", w.user["reg1"], "  ", false, true},
		{"registrar with a reason", w.user["reg1"], "Certificate for the person", true, true},
		{"superuser, no reason", w.superuser(t), "", false, true},
		{"superuser with a reason", w.superuser(t), "Support request 12", true, true},
	} {
		r := read(c.who, c.reason)
		if !r.Restricted || r.Revealed != c.revealed || r.CanReveal != c.canReveal {
			t.Errorf("%s: restricted=%v revealed=%v canReveal=%v", c.name, r.Restricted, r.Revealed, r.CanReveal)
		}
	}

	rows := w.auditRows(t, core.AuditActionRegisterRestrictedRead)
	if len(rows) != 2 {
		t.Fatalf("%d audited reveals, want 2", len(rows))
	}

	for _, r := range rows {
		if !strings.Contains(r.GetString("meta"), "reason") || r.GetString("targetId") != d.Id {
			t.Errorf("audit entry %s", r.GetString("meta"))
		}
	}

	if _, err := core.ReadRegisterEntry(w.app, w.ctx, w.user["mem"], "baptisms", d.Id, "x"); statusOf(err) != 403 {
		t.Fatalf("a plain member is no staff: %v", err)
	}

	if !mustVerify(t, w.app).OK() {
		t.Fatal("chain must hold")
	}
}

// anonWorld builds a register with anonymization on (30 days) and three final
// entries in a chain.
func newAnonWorld(t *testing.T) (*regWorld, []*core.Record) {
	t.Helper()

	w := newRegWorld(t, 1)

	cfg, err := core.CreateSacramentalRegister(w.app, core.SacramentalRegisterOpts{Name: "marriages", Anonymize: true, AnonymizeAfterDays: 30})
	must(t, err)

	w.cfg = cfg
	w.col, _ = w.app.FindCollectionByNameOrId("marriages")

	var out []*core.Record

	for _, name := range []string{"Smith", "Jones", "Brown"} {
		r := core.NewRecord(w.col)
		r.Set("org", w.org.Id)
		r.Set("sacrament", "marriage")
		r.Set("surname", name)
		r.Set("givenNames", "Ann")
		r.Set("eventPlace", "St Mary")
		r.Set("details", map[string]any{"father": "Bob " + name})
		r.Set("subject", w.user["mem"].Id)
		must(t, w.app.Save(r))

		_, err := core.FinalizeRegisterEntry(w.app, w.ctx, w.user["reg1"], "marriages", r.Id)
		must(t, err)

		out = append(out, r)
	}

	return w, out
}

func verifyMarriages(t testing.TB, app core.App) *core.RegisterVerification {
	t.Helper()

	res, err := core.VerifyRegisters(app)
	must(t, err)

	for _, v := range res {
		if v.Collection == "marriages" {
			return v
		}
	}

	t.Fatal("no marriages register")

	return nil
}

func TestRegisterAnonymization(t *testing.T) {
	w, entries := newAnonWorld(t)
	mid := entries[1]

	anon := func(who string, id string) (*core.Record, error) {
		return core.AnonymizeRegisterEntry(w.app, w.ctx, w.user[who], "marriages", id, "Retention period over")
	}

	if _, err := anon("reg1", mid.Id); statusOf(err) != 409 {
		t.Fatalf("not due yet: %v", err)
	}

	later := time.Now().AddDate(0, 0, 31)
	defer core.SetRegisterClock(func() time.Time { return later })()

	for _, c := range []struct {
		who    string
		status int
	}{{"adm", 403}, {"mem", 403}, {"out", 404}} {
		if _, err := anon(c.who, mid.Id); statusOf(err) != c.status {
			t.Errorf("%s: %v", c.who, err)
		}
	}

	if _, err := core.AnonymizeRegisterEntry(w.app, w.ctx, w.superuser(t), "marriages", mid.Id, "r"); statusOf(err) != 403 {
		t.Fatalf("a superuser cannot anonymize: %v", err)
	}

	if _, err := core.AnonymizeRegisterEntry(w.app, w.ctx, w.user["reg1"], "marriages", mid.Id, " "); statusOf(err) != 400 {
		t.Fatalf("reason required: %v", err)
	}

	before := verifyMarriages(t, w.app)
	hashBefore := mustFreshN(t, w.app, "marriages", mid.Id).GetString("hash")

	// a legal hold blocks it
	hold, err := core.PlaceLegalHold(w.app, w.ctx, w.superuser(t), core.LegalHoldInput{
		Scope: core.LegalHoldScopeRecord, Collection: "marriages", RecordId: mid.Id, Reason: "court case", Reference: "X",
	})
	must(t, err)

	if _, err := anon("reg1", mid.Id); statusOf(err) != 409 {
		t.Fatalf("held: %v", err)
	}

	_, err = core.LiftLegalHold(w.app, w.ctx, hold.Id, w.superuser(t))
	must(t, err)

	got, err := anon("reg1", mid.Id)
	must(t, err)

	if got.GetString("surname") == "Jones" || !strings.HasPrefix(got.GetString("surname"), "anon-") ||
		strings.Contains(got.GetString("details"), "Jones") || got.GetString("subject") != "" || got.GetString("eventPlace") == "St Mary" {
		t.Fatalf("personal fields must be pseudonymized: %v", got.PublicExport())
	}

	if got.GetString("sacrament") != "marriage" || got.GetString("hash") != hashBefore || got.GetInt("seq") != 2 {
		t.Fatal("non-personal fields and the chain link must stay")
	}

	// the other entries are untouched, and verification still passes
	if mustFreshN(t, w.app, "marriages", entries[0].Id).GetString("surname") != "Smith" {
		t.Fatal("other entries untouched")
	}

	after := verifyMarriages(t, w.app)
	if !after.OK() || after.Finalized != 3 || after.HeadHash != before.HeadHash {
		t.Fatalf("verification after anonymization: %+v", after)
	}

	if _, err := anon("reg1", mid.Id); statusOf(err) != 409 {
		t.Fatalf("only once: %v", err)
	}

	notes, _ := core.RegisterAnnotations(w.app, w.cfg, got)
	if len(notes) != 1 || !strings.Contains(notes[0].GetString("text"), got.GetString("anonDigest")) || notes[0].GetString("author") != w.user["reg1"].Id {
		t.Fatalf("signed marginal note: %+v", notes)
	}

	if len(w.auditRows(t, core.AuditActionRegisterAnonymize)) != 1 {
		t.Fatal("anonymization must be audited")
	}

	// the model guard still holds for an anonymized entry
	got = mustFreshN(t, w.app, "marriages", mid.Id)
	got.Set("surname", "Jones")

	if err := w.app.Save(got); err == nil {
		t.Fatal("an anonymized entry is still immutable")
	}

	// tampering outside the application is a break: the tombstone, its neighbour, its digest
	for _, c := range []struct{ name, sql string }{
		{"surname of the tombstone", "UPDATE marriages SET surname='Jones' WHERE id='" + mid.Id + "'"},
		{"hash of the tombstone", "UPDATE marriages SET hash='" + strings.Repeat("a", 64) + "' WHERE id='" + mid.Id + "'"},
		{"predecessor", "UPDATE marriages SET surname='Smyth' WHERE id='" + entries[0].Id + "'"},
	} {
		t.Log("tampering with", c.name)
		w.tamperAndRollBack(t, c.sql)
	}

	if !verifyMarriages(t, w.app).OK() {
		t.Fatal("restored")
	}
}

// tamperAndRollBack runs a raw statement (outside every hook), checks, and undoes
// it by running the whole thing in a transaction that is rolled back.
func (w *regWorld) tamperAndRollBack(t testing.TB, sql string) {
	t.Helper()

	var rollback = errRollback{}

	err := w.app.RunInTransaction(func(tx core.App) error {
		if _, err := tx.DB().NewQuery(sql).Execute(); err != nil {
			t.Fatal(err)
		}

		res, err := core.VerifyRegisters(tx)
		must(t, err)

		broken := false
		for _, v := range res {
			if v.Collection == "marriages" && !v.OK() {
				broken = true
			}
		}

		if !broken {
			t.Errorf("tampering %q must break verification", sql)
		}

		return rollback
	})
	if err != rollback {
		t.Fatal(err)
	}
}

type errRollback struct{}

func (errRollback) Error() string { return "rollback" }

func mustFreshN(t testing.TB, app core.App, col, id string) *core.Record {
	t.Helper()

	r, err := app.FindRecordById(col, id)
	must(t, err)

	return r
}

func TestRegisterAnonymizationIsOffByDefault(t *testing.T) {
	w := newRegWorld(t, 1)
	d := w.draft(t, "Smith")
	w.finalize(t, "reg1", d)

	defer core.SetRegisterClock(func() time.Time { return time.Now().AddDate(10, 0, 0) })()

	if _, err := core.AnonymizeRegisterEntry(w.app, w.ctx, w.user["reg1"], "baptisms", d.Id, "r"); statusOf(err) != 409 {
		t.Fatalf("the default is off: %v", err)
	}

	if w.cfg.Anonymize {
		t.Fatal("default off")
	}
}

func TestRegisterAnonymizeDue(t *testing.T) {
	w, entries := newAnonWorld(t)

	// restrict the first one: a restricted entry can still be anonymized
	_, err := core.RestrictRegisterEntry(w.app, w.ctx, w.user["reg1"], "marriages", entries[0].Id, "r")
	must(t, err)

	done, err := core.AnonymizeDueRegisterEntries(w.app, w.ctx, w.user["reg1"], "marriages", w.org.Id, "Retention")
	must(t, err)

	if len(done) != 0 {
		t.Fatalf("nothing is due yet: %v", done)
	}

	later := time.Now().AddDate(0, 0, 31)
	defer core.SetRegisterClock(func() time.Time { return later })()

	if _, err := core.AnonymizeDueRegisterEntries(w.app, w.ctx, w.user["adm"], "marriages", w.org.Id, "r"); statusOf(err) != 403 {
		t.Fatalf("admin: %v", err)
	}

	done, err = core.AnonymizeDueRegisterEntries(w.app, w.ctx, w.user["reg1"], "marriages", w.org.Id, "Retention")
	must(t, err)

	if len(done) != 3 {
		t.Fatalf("anonymized %d, want 3", len(done))
	}

	if v := verifyMarriages(t, w.app); !v.OK() || v.Finalized != 3 {
		t.Fatalf("all anonymized, chain holds: %+v", v)
	}

	done, _ = core.AnonymizeDueRegisterEntries(w.app, w.ctx, w.user["reg1"], "marriages", w.org.Id, "Retention")
	if len(done) != 0 {
		t.Fatal("second run does nothing")
	}
}

func TestRegisterAnonymizeConfigValidation(t *testing.T) {
	w := newRegWorld(t, 1)

	if _, err := core.CreateSacramentalRegister(w.app, core.SacramentalRegisterOpts{Name: "confirmations", Anonymize: true}); err == nil {
		t.Fatal("anonymization needs a retention period")
	}

	// turning it on later adds the fields
	rec, err := w.app.FindFirstRecordByData(core.CollectionNamePermanentCollections, "collection", "baptisms")
	must(t, err)

	rec.Set("anonymize", true)
	rec.Set("anonymizeAfterDays", 90)
	must(t, w.app.Save(rec))

	col, _ := w.app.FindCollectionByNameOrId("baptisms")
	if col.Fields.GetByName("anonymizedAt") == nil || col.Fields.GetByName("anonDigest") == nil {
		t.Fatal("the entry fields must be added when anonymization is turned on")
	}

	if !core.PermanentConfigFor(w.app, "baptisms").Anonymize {
		t.Fatal("setting is live")
	}
}
