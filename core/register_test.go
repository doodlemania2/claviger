package core_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// Fork-local (Claviger #87): permanent records. World: org O with a registrar
// (reg1, reg2), an admin without the tag (adm), a plain member (mem) and a
// person who is not in the org (out). Register "baptisms" is the template
// (chain on, one signer).

type regWorld struct {
	app  *tests.TestApp
	org  *core.Record
	user map[string]*core.Record
	cfg  *core.PermanentConfig
	col  *core.Collection
	ctx  context.Context
}

func newRegWorld(t testing.TB, signers int) *regWorld {
	t.Helper()

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)

	w := &regWorld{app: app, user: map[string]*core.Record{}, ctx: context.Background()}

	orgs, _ := app.FindCollectionByNameOrId(core.CollectionNameOrgs)
	w.org = core.NewRecord(orgs)
	w.org.Set("name", "Parish")
	w.org.Set("slug", "parish")
	must(t, app.Save(w.org))

	users, _ := app.FindCollectionByNameOrId("users")
	for _, n := range []string{"reg1", "reg2", "adm", "own", "mem", "out"} {
		u := core.NewRecord(users)
		u.SetEmail(n + "@register.test")
		u.SetPassword("password123")
		u.Set("username", "regw_"+n)
		must(t, app.Save(u))
		w.user[n] = u
	}

	roles, _ := app.FindCollectionByNameOrId(core.CollectionNameOrgRoles)
	tag := core.NewRecord(roles)
	tag.Set("org", w.org.Id)
	tag.Set("name", core.RegistrarTag)
	must(t, app.Save(tag))

	members, _ := app.FindCollectionByNameOrId(core.CollectionNameOrgMembers)
	add := func(who, role string, tags ...string) {
		m := core.NewRecord(members)
		m.Set("org", w.org.Id)
		m.Set("user", w.user[who].Id)
		m.Set("role", role)
		m.Set("status", "active")
		m.Set("tags", tags)
		must(t, app.Save(m))
	}
	add("reg1", "member", tag.Id)
	add("reg2", "member", tag.Id)
	add("adm", "admin")
	add("own", "owner")
	add("mem", "member")

	w.cfg, err = core.CreateSacramentalRegister(app, core.SacramentalRegisterOpts{Name: "baptisms", Signers: signers})
	must(t, err)

	w.col, _ = app.FindCollectionByNameOrId("baptisms")

	return w
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (w *regWorld) draft(t testing.TB, surname string) *core.Record {
	t.Helper()

	r := core.NewRecord(w.col)
	r.Set("org", w.org.Id)
	r.Set("sacrament", "baptism")
	r.Set("surname", surname)
	r.Set("givenNames", "Ann")
	must(t, w.app.Save(r))

	return r
}

func (w *regWorld) superuser(t testing.TB) *core.Record {
	t.Helper()

	if su, err := w.app.FindAuthRecordByEmail(core.CollectionNameSuperusers, "root@register.test"); err == nil {
		return su
	}

	col, err := w.app.FindCollectionByNameOrId(core.CollectionNameSuperusers)
	must(t, err)

	su := core.NewRecord(col)
	su.SetEmail("root@register.test")
	su.SetPassword("password123")
	must(t, w.app.Save(su))

	return su
}

func (w *regWorld) finalize(t testing.TB, who string, e *core.Record) *core.RegisterFinalizeResult {
	t.Helper()

	res, err := core.FinalizeRegisterEntry(w.app, w.ctx, w.user[who], "baptisms", e.Id)
	must(t, err)

	return res
}

func statusOf(err error) int {
	var oe *core.OrgError
	if errors.As(err, &oe) {
		return oe.Status
	}

	return 0
}

func (w *regWorld) fresh(t testing.TB, e *core.Record) *core.Record {
	t.Helper()

	r, err := w.app.FindRecordById("baptisms", e.Id)
	must(t, err)

	return r
}

func TestRegisterTemplateSetUp(t *testing.T) {
	w := newRegWorld(t, 1)

	for _, f := range []string{"finalizedAt", "finalizedBy", "secondSigner", "pendingBy", "pendingAt", "pendingHash", "restricted", "seq", "hash"} {
		if w.col.Fields.GetByName(f) == nil {
			t.Errorf("field %s is missing", f)
		}
	}

	if !w.cfg.Chain || w.cfg.Signers != 1 || w.cfg.FinalizeField != "finalizedAt" || w.cfg.AnnotationCollection != "baptisms_annotations" {
		t.Fatalf("unexpected defaults: %+v", w.cfg)
	}

	if len(w.cfg.MutableFields) != 1 || w.cfg.MutableFields[0] != "restricted" {
		t.Fatalf("mutable fields: %v", w.cfg.MutableFields)
	}

	ann, err := w.app.FindCollectionByNameOrId("baptisms_annotations")
	must(t, err)

	for _, f := range []string{"entry", "kind", "text", "author", "reason", "supersedes", "created"} {
		if ann.Fields.GetByName(f) == nil {
			t.Errorf("annotation field %s is missing", f)
		}
	}

	if ann.CreateRule != nil || ann.UpdateRule != nil || ann.DeleteRule != nil {
		t.Error("annotation write rules must be null")
	}

	for _, name := range []string{"baptisms", "baptisms_annotations"} {
		row, err := w.app.FindFirstRecordByData(core.CollectionNameDataMap, "collection", name)
		if err != nil {
			t.Fatalf("%s: no data map entry: %v", name, err)
		}

		if row.GetString("treatment") != core.DataMapTreatmentRetain || row.GetString("retainReason") == "" {
			t.Errorf("%s: want retain with a reason, got %s %q", name, row.GetString("treatment"), row.GetString("retainReason"))
		}
	}
}

func TestRegisterDraftsStayEditable(t *testing.T) {
	w := newRegWorld(t, 1)

	d := w.draft(t, "Smith")
	d.Set("surname", "Smyth")
	d.Set("restricted", true)
	must(t, w.app.Save(d))

	d.Set("givenNames", "Anna")
	must(t, w.app.Save(d))
	must(t, w.app.Delete(d))
}

func TestRegisterManagedFieldsCannotBeWrittenDirectly(t *testing.T) {
	w := newRegWorld(t, 1)

	// create already final
	r := core.NewRecord(w.col)
	r.Set("org", w.org.Id)
	r.Set("sacrament", "baptism")
	r.Set("surname", "Forged")
	r.Set("finalizedAt", "2024-01-01 00:00:00.000Z")

	if err := w.app.Save(r); err == nil {
		t.Fatal("creating a final entry directly must fail")
	}

	// finalize an existing draft without the route
	d := w.draft(t, "Smith")
	d.Set("finalizedAt", "2024-01-01 00:00:00.000Z")

	if err := w.app.Save(d); err == nil {
		t.Fatal("setting finalizedAt directly must fail")
	}

	d = w.fresh(t, d)
	d.Set("pendingBy", w.user["reg1"].Id)

	if err := w.app.Save(d); err == nil {
		t.Fatal("setting pendingBy directly must fail")
	}

	d = w.fresh(t, d)
	d.Set("hash", "abc")

	if err := w.app.Save(d); err == nil {
		t.Fatal("setting the hash directly must fail")
	}
}

func TestRegisterFinalizeOneSigner(t *testing.T) {
	w := newRegWorld(t, 1)
	d := w.draft(t, "Smith")

	cases := map[string]int{"mem": 403, "adm": 403, "own": 403, "out": 404}
	for who, want := range cases {
		_, err := core.FinalizeRegisterEntry(w.app, w.ctx, w.user[who], "baptisms", d.Id)
		if statusOf(err) != want {
			t.Errorf("%s: want %d, got %v", who, want, err)
		}
	}

	if _, err := core.FinalizeRegisterEntry(w.app, w.ctx, w.superuser(t), "baptisms", d.Id); statusOf(err) != 403 {
		t.Fatalf("superuser starting a finalize: %v", err)
	}

	if w.fresh(t, d).GetDateTime("finalizedAt").IsZero() == false {
		t.Fatal("a refused finalize changed the entry")
	}

	res := w.finalize(t, "reg1", d)
	if !res.Final || res.Pending {
		t.Fatalf("unexpected result %+v", res)
	}

	got := w.fresh(t, d)
	if got.GetDateTime("finalizedAt").IsZero() || got.GetString("finalizedBy") != w.user["reg1"].Id || got.GetString("secondSigner") != "" {
		t.Fatalf("signers not recorded: %v %q %q", got.GetDateTime("finalizedAt"), got.GetString("finalizedBy"), got.GetString("secondSigner"))
	}

	if got.GetInt("seq") != 1 || len(got.GetString("hash")) != 64 {
		t.Fatalf("chain not set: %d %q", got.GetInt("seq"), got.GetString("hash"))
	}

	if _, err := core.FinalizeRegisterEntry(w.app, w.ctx, w.user["reg1"], "baptisms", d.Id); statusOf(err) != 409 {
		t.Fatalf("finalizing twice: %v", err)
	}
}

func TestRegisterFinalEntryIsImmutable(t *testing.T) {
	w := newRegWorld(t, 1)
	d := w.draft(t, "Smith")
	w.finalize(t, "reg1", d)

	// app.Save: a changed field
	e := w.fresh(t, d)
	e.Set("surname", "Jones")

	if err := w.app.Save(e); err == nil || !strings.Contains(err.Error(), "final") {
		t.Fatalf("editing a final entry must fail, got %v", err)
	}

	// clearing the finalize field
	e = w.fresh(t, d)
	e.Set("finalizedAt", "")

	if err := w.app.Save(e); err == nil {
		t.Fatal("clearing the finalize field must fail")
	}

	// the hash cannot be rewritten
	e = w.fresh(t, d)
	e.Set("hash", strings.Repeat("0", 64))

	if err := w.app.Save(e); err == nil {
		t.Fatal("rewriting the hash must fail")
	}

	// a stale in-memory copy of the draft cannot slip through either
	stale := w.draft(t, "Stale")
	copyOf := w.fresh(t, stale)
	w.finalize(t, "reg1", stale)
	copyOf.Set("surname", "Changed")

	if err := w.app.Save(copyOf); err == nil {
		t.Fatal("saving a stale copy of an entry that became final must fail")
	}

	// delete
	if err := w.app.Delete(w.fresh(t, d)); err == nil {
		t.Fatal("deleting a final entry must fail")
	}

	// the mutable field is the one thing that still changes
	e = w.fresh(t, d)
	e.Set("restricted", true)
	must(t, w.app.Save(e))

	if !w.fresh(t, d).GetBool("restricted") || w.fresh(t, d).GetString("surname") != "Smith" {
		t.Fatal("restricted should be set and the surname untouched")
	}

	// a no-op save of an unchanged final entry is fine
	must(t, w.app.Save(w.fresh(t, d)))
}

func TestRegisterCollectionGuards(t *testing.T) {
	w := newRegWorld(t, 1)
	d := w.draft(t, "Smith")
	w.finalize(t, "reg1", d)

	if err := w.app.Delete(w.col); err == nil {
		t.Error("deleting the register must fail")
	}

	ann, _ := w.app.FindCollectionByNameOrId("baptisms_annotations")
	if err := w.app.Delete(ann); err == nil {
		t.Error("deleting the annotation collection must fail")
	}

	col, _ := w.app.FindCollectionByNameOrId("baptisms")
	col.Fields.RemoveByName("finalizedAt")

	if err := w.app.Save(col); err == nil {
		t.Error("removing the finalize field must fail")
	}

	col, _ = w.app.FindCollectionByNameOrId("baptisms")
	col.Fields.GetByName("finalizedAt").(*core.DateField).Name = "closedAt"

	if err := w.app.Save(col); err == nil {
		t.Error("renaming the finalize field must fail")
	}

	// with final entries no data field can be removed
	col, _ = w.app.FindCollectionByNameOrId("baptisms")
	col.Fields.RemoveByName("officiant")

	if err := w.app.Save(col); err == nil {
		t.Error("removing a data field of a register with final entries must fail")
	}

	// adding a field is allowed and does not break the chain
	col, _ = w.app.FindCollectionByNameOrId("baptisms")
	col.Fields.Add(&core.TextField{Name: "remarks"})
	must(t, w.app.Save(col))

	res, err := core.VerifyRegisters(w.app)
	must(t, err)

	if len(res) != 1 || !res[0].OK() {
		t.Fatalf("chain after adding a field: %+v", res)
	}

	// the configuration row
	row, err := w.app.FindFirstRecordByData(core.CollectionNamePermanentCollections, "collection", "baptisms")
	must(t, err)

	row.Set("chain", false)

	if err := w.app.Save(row); err == nil {
		t.Error("turning the chain off must fail")
	}

	row = mustFresh(t, w.app, core.CollectionNamePermanentCollections, row.Id)
	row.Set("signers", 2)
	must(t, w.app.Save(row)) // signers may change

	row = mustFresh(t, w.app, core.CollectionNamePermanentCollections, row.Id)
	if err := w.app.Delete(row); err == nil {
		t.Error("removing a register that has final entries must fail")
	}
}

func mustFresh(t testing.TB, app core.App, col, id string) *core.Record {
	t.Helper()

	r, err := app.FindRecordById(col, id)
	must(t, err)

	return r
}

func TestRegisterDeleteConfigWithoutFinalEntries(t *testing.T) {
	w := newRegWorld(t, 1)
	w.draft(t, "Smith")

	row, err := w.app.FindFirstRecordByData(core.CollectionNamePermanentCollections, "collection", "baptisms")
	must(t, err)
	must(t, w.app.Delete(row))

	// no longer a register: the collection can be edited like any other
	if core.PermanentConfigFor(w.app, "baptisms") != nil {
		t.Fatal("the cache should be reset")
	}
}

func TestRegisterTwoSigners(t *testing.T) {
	w := newRegWorld(t, 2)
	d := w.draft(t, "Smith")

	res := w.finalize(t, "reg1", d)
	if !res.Pending || res.Final {
		t.Fatalf("first step should be pending: %+v", res)
	}

	got := w.fresh(t, d)
	if !got.GetDateTime("finalizedAt").IsZero() || got.GetString("pendingBy") != w.user["reg1"].Id {
		t.Fatal("pending state not recorded")
	}

	if core.RegisterState(w.cfg, got) != "pending" {
		t.Fatalf("state %s", core.RegisterState(w.cfg, got))
	}

	// still a draft: editable
	got.Set("givenNames", "Anna")
	must(t, w.app.Save(got))

	// the same person, a plain member and an outsider cannot confirm
	for who, want := range map[string]int{"reg1": 409, "mem": 403, "out": 404} {
		if _, err := core.FinalizeRegisterEntry(w.app, w.ctx, w.user[who], "baptisms", d.Id); statusOf(err) != want {
			t.Errorf("%s: want %d, got %v", who, want, err)
		}
	}

	// the entry changed after the request: the confirmation is refused
	if _, err := core.FinalizeRegisterEntry(w.app, w.ctx, w.user["reg2"], "baptisms", d.Id); statusOf(err) != 409 {
		t.Fatalf("confirming a changed entry: %v", err)
	}

	// withdraw by somebody else who is not a manager is refused, by the requester works
	if _, err := core.WithdrawRegisterFinalize(w.app, w.ctx, w.user["reg2"], "baptisms", d.Id); statusOf(err) != 403 {
		t.Fatalf("withdraw by another registrar: %v", err)
	}

	_, err := core.WithdrawRegisterFinalize(w.app, w.ctx, w.user["reg1"], "baptisms", d.Id)
	must(t, err)

	if w.fresh(t, d).GetString("pendingBy") != "" {
		t.Fatal("withdraw should clear the request")
	}

	// request again; an org owner or admin without the tag cannot confirm
	w.finalize(t, "reg1", d)

	for _, who := range []string{"adm", "own"} {
		if _, err := core.FinalizeRegisterEntry(w.app, w.ctx, w.user[who], "baptisms", d.Id); statusOf(err) != 403 {
			t.Fatalf("%s confirming without the tag: %v", who, err)
		}
	}

	if w.fresh(t, d).GetString("pendingBy") == "" {
		t.Fatal("a refused confirmation changed the entry")
	}

	// a superuser can never be the confirmer
	if _, err := core.FinalizeRegisterEntry(w.app, w.ctx, w.superuser(t), "baptisms", d.Id); statusOf(err) != 403 {
		t.Fatalf("superuser confirming: %v", err)
	}

	// a second registrar can
	res = w.finalize(t, "reg2", d)
	if !res.Final {
		t.Fatalf("second step should finalize: %+v", res)
	}

	got = w.fresh(t, d)
	if got.GetString("finalizedBy") != w.user["reg1"].Id || got.GetString("secondSigner") != w.user["reg2"].Id || got.GetString("pendingBy") != "" {
		t.Fatalf("signers: %q %q pending %q", got.GetString("finalizedBy"), got.GetString("secondSigner"), got.GetString("pendingBy"))
	}

	// an admin without the tag cannot start a finalization
	d2 := w.draft(t, "Jones")
	if _, err := core.FinalizeRegisterEntry(w.app, w.ctx, w.user["adm"], "baptisms", d2.Id); statusOf(err) != 403 {
		t.Fatalf("admin starting: %v", err)
	}

	// a pending draft can still be deleted
	w.finalize(t, "reg1", d2)
	must(t, w.app.Delete(w.fresh(t, d2)))
}

func TestRegisterAnnotations(t *testing.T) {
	w := newRegWorld(t, 1)
	d := w.draft(t, "Smith")

	// a draft cannot be annotated, by the function or directly
	in := core.RegisterAnnotationInput{Kind: core.RegisterKindCorrection, Text: "Surname is Smyth", Reason: "Parish record seen"}
	if _, err := core.AddRegisterAnnotation(w.app, w.ctx, w.user["reg1"], "baptisms", d.Id, in); statusOf(err) != 409 {
		t.Fatalf("annotating a draft: %v", err)
	}

	annCol, _ := w.app.FindCollectionByNameOrId("baptisms_annotations")
	direct := core.NewRecord(annCol)
	direct.Set("entry", d.Id)
	direct.Set("kind", "correction")
	direct.Set("text", "x")
	direct.Set("reason", "y")

	if err := w.app.Save(direct); err == nil {
		t.Fatal("annotating a draft with app.Save must fail")
	}

	w.finalize(t, "reg1", d)

	if _, err := core.AddRegisterAnnotation(w.app, w.ctx, w.superuser(t), "baptisms", d.Id, in); statusOf(err) != 403 {
		t.Errorf("superuser annotating: %v", err)
	}

	for who, want := range map[string]int{"mem": 403, "adm": 403, "own": 403, "out": 404} {
		if _, err := core.AddRegisterAnnotation(w.app, w.ctx, w.user[who], "baptisms", d.Id, in); statusOf(err) != want {
			t.Errorf("%s: want %d, got %v", who, want, err)
		}
	}

	a1, err := core.AddRegisterAnnotation(w.app, w.ctx, w.user["reg1"], "baptisms", d.Id, in)
	must(t, err)

	if a1.GetString("author") != w.user["reg1"].Id || a1.GetString("org") != w.org.Id {
		t.Fatalf("author/org copy: %q %q", a1.GetString("author"), a1.GetString("org"))
	}

	// reason and kind are required, a retraction needs a target
	bad := []core.RegisterAnnotationInput{
		{Kind: core.RegisterKindCorrection, Text: "x"},
		{Kind: "nonsense", Text: "x", Reason: "y"},
		{Kind: core.RegisterKindRetraction, Text: "x", Reason: "y"},
		{Kind: core.RegisterKindRetraction, Text: "x", Reason: "y", Supersedes: "nosuchannotation"},
		{Kind: core.RegisterKindCorrection, Text: "", Reason: "y"},
	}
	for i, b := range bad {
		if _, err := core.AddRegisterAnnotation(w.app, w.ctx, w.user["reg1"], "baptisms", d.Id, b); statusOf(err) != 400 {
			t.Errorf("bad annotation %d: %v", i, err)
		}
	}

	// the annotation cannot be changed or deleted by anybody
	a1 = mustFresh(t, w.app, "baptisms_annotations", a1.Id)
	a1.Set("text", "edited")

	if err := w.app.Save(a1); err == nil {
		t.Fatal("updating an annotation must fail")
	}

	if err := w.app.Delete(mustFresh(t, w.app, "baptisms_annotations", a1.Id)); err == nil {
		t.Fatal("deleting an annotation must fail")
	}

	// a retraction points to the mistaken annotation
	a2, err := core.AddRegisterAnnotation(w.app, w.ctx, w.user["reg2"], "baptisms", d.Id, core.RegisterAnnotationInput{
		Kind: core.RegisterKindRetraction, Text: "Annotation was a mistake", Reason: "Wrong book", Supersedes: a1.Id,
	})
	must(t, err)

	// a retraction cannot be retracted
	if _, err := core.AddRegisterAnnotation(w.app, w.ctx, w.user["reg2"], "baptisms", d.Id, core.RegisterAnnotationInput{
		Kind: core.RegisterKindCorrection, Text: "x", Reason: "y", Supersedes: a2.Id,
	}); statusOf(err) != 400 {
		t.Fatalf("superseding a retraction: %v", err)
	}

	a3, err := core.AddRegisterAnnotation(w.app, w.ctx, w.user["reg1"], "baptisms", d.Id, core.RegisterAnnotationInput{
		Kind: core.RegisterKindLaterEvent, Text: "Married 2030", Reason: "Notice from the parish",
	})
	must(t, err)

	list, err := core.RegisterAnnotations(w.app, w.cfg, d)
	must(t, err)

	if len(list) != 3 || list[0].Id != a1.Id || list[1].Id != a2.Id || list[2].Id != a3.Id {
		ids := []string{}
		for _, r := range list {
			ids = append(ids, r.Id)
		}

		t.Fatalf("annotations should be oldest first: %v (want %s %s %s)", ids, a1.Id, a2.Id, a3.Id)
	}

	// annotation does not touch the entry or its chain
	res, err := core.VerifyRegisters(w.app)
	must(t, err)

	if !res[0].OK() {
		t.Fatalf("chain: %+v", res[0].Breaks)
	}
}

func TestRegisterChainDetectsTampering(t *testing.T) {
	w := newRegWorld(t, 1)

	var ids []string
	for _, n := range []string{"One", "Two", "Three", "Four"} {
		d := w.draft(t, n)
		w.finalize(t, "reg1", d)
		ids = append(ids, d.Id)
	}

	for i, id := range ids {
		if seq := mustFresh(t, w.app, "baptisms", id).GetInt("seq"); seq != i+1 {
			t.Fatalf("entry %d has seq %d", i, seq)
		}
	}

	verify := func() *core.RegisterVerification {
		res, err := core.VerifyRegisters(w.app)
		must(t, err)

		return res[0]
	}

	v := verify()
	if !v.OK() || v.Finalized != 4 || v.HeadSeq != 4 || len(v.HeadHash) != 64 {
		t.Fatalf("clean chain: %+v", v)
	}

	// restricted is outside the hash
	r := mustFresh(t, w.app, "baptisms", ids[1])
	r.Set("restricted", true)
	must(t, w.app.Save(r))

	if !verify().OK() {
		t.Fatal("setting restricted must not break the chain")
	}

	// raw SQL changes an entry
	_, err := w.app.NonconcurrentDB().NewQuery("UPDATE baptisms SET surname = 'Forged' WHERE id = {:id}").Bind(dbx.Params{"id": ids[1]}).Execute()
	must(t, err)

	v = verify()
	if v.OK() || len(v.Breaks) != 1 || v.Breaks[0].Entry != ids[1] || v.Breaks[0].Seq != 2 {
		t.Fatalf("changed entry: %+v", v.Breaks)
	}

	// an entry is removed from the middle
	_, err = w.app.NonconcurrentDB().NewQuery("UPDATE baptisms SET surname = 'Two' WHERE id = {:id}").Bind(dbx.Params{"id": ids[1]}).Execute()
	must(t, err)
	_, err = w.app.NonconcurrentDB().NewQuery("DELETE FROM baptisms WHERE id = {:id}").Bind(dbx.Params{"id": ids[2]}).Execute()
	must(t, err)

	v = verify()
	if v.OK() {
		t.Fatal("a removed entry must be reported")
	}

	found := false
	for _, b := range v.Breaks {
		if b.Entry == ids[3] && strings.Contains(b.Problem, "expected number 3") {
			found = true
		}
	}

	if !found {
		t.Fatalf("expected a numbering break at the entry after the gap: %+v", v.Breaks)
	}
}

func TestRegisterAuditEntries(t *testing.T) {
	w := newRegWorld(t, 2)
	d := w.draft(t, "Smith")

	ctx := core.WithAuditContext(context.Background(), core.NewAuditActorFromRecord(w.user["reg1"]), "1.2.3.4")
	_, err := core.FinalizeRegisterEntry(w.app, ctx, w.user["reg1"], "baptisms", d.Id)
	must(t, err)

	ctx2 := core.WithAuditContext(context.Background(), core.NewAuditActorFromRecord(w.user["reg2"]), "1.2.3.4")
	_, err = core.FinalizeRegisterEntry(w.app, ctx2, w.user["reg2"], "baptisms", d.Id)
	must(t, err)

	_, err = core.AddRegisterAnnotation(w.app, ctx, w.user["reg1"], "baptisms", d.Id,
		core.RegisterAnnotationInput{Kind: core.RegisterKindLegalNote, Text: "t", Reason: "r"})
	must(t, err)

	for action, actor := range map[string]string{
		core.AuditActionRegisterFinalizeRequest: w.user["reg1"].Id,
		core.AuditActionRegisterFinalize:        w.user["reg2"].Id,
		core.AuditActionRegisterAnnotate:        w.user["reg1"].Id,
	} {
		rows, err := w.app.FindAllRecords(core.CollectionNameAuditLog, dbx.HashExp{"action": action})
		must(t, err)

		if len(rows) != 1 {
			t.Fatalf("%s: %d entries", action, len(rows))
		}

		if rows[0].GetString("actorId") != actor || rows[0].GetString("targetId") != d.Id {
			t.Errorf("%s: actor %q target %q", action, rows[0].GetString("actorId"), rows[0].GetString("targetId"))
		}

		if !strings.Contains(rows[0].GetString("meta"), w.org.Id) {
			t.Errorf("%s: meta has no orgId: %s", action, rows[0].GetString("meta"))
		}
	}
}

func TestAnnotateErasureRequest(t *testing.T) {
	w := newRegWorld(t, 1)
	d := w.draft(t, "Smith")

	if _, err := core.AnnotateErasureRequest(w.app, w.ctx, d, "I want it gone"); statusOf(err) != 409 {
		t.Fatalf("a draft is not annotated: %v", err)
	}

	w.finalize(t, "reg1", d)

	a, err := core.AnnotateErasureRequest(w.app, w.ctx, w.fresh(t, d), "Data subject request of 2030-01-01")
	must(t, err)

	if a.GetString("kind") != core.RegisterKindDataSubject || !strings.Contains(a.GetString("text"), "kept") {
		t.Fatalf("unexpected annotation %v %q", a.GetString("kind"), a.GetString("text"))
	}

	if !w.fresh(t, d).GetBool("restricted") || w.fresh(t, d).GetString("surname") != "Smith" {
		t.Fatal("the entry must be restricted and otherwise untouched")
	}

	if _, err := core.AnnotateErasureRequest(w.app, w.ctx, w.fresh(t, d), " "); statusOf(err) != 400 {
		t.Fatalf("a reason is required: %v", err)
	}

	if !mustVerify(t, w.app).OK() {
		t.Fatal("chain must hold")
	}
}

func mustVerify(t testing.TB, app core.App) *core.RegisterVerification {
	t.Helper()

	res, err := core.VerifyRegisters(app)
	must(t, err)

	return res[0]
}

// A register that is not the template: an existing base collection, registered
// through the configuration row, no chain.
func TestRegisterExistingCollection(t *testing.T) {
	w := newRegWorld(t, 1)

	orgs, _ := w.app.FindCachedCollectionByNameOrId(core.CollectionNameOrgs)
	users, _ := w.app.FindCachedCollectionByNameOrId("users")
	col := core.NewBaseCollection("roll")
	col.Fields.Add(
		&core.RelationField{Name: "org", CollectionId: orgs.Id, MaxSelect: 1, Required: true},
		&core.RelationField{Name: "person", CollectionId: users.Id, MaxSelect: 1},
		&core.TextField{Name: "title"},
		&core.TextField{Name: "note"},
	)
	must(t, w.app.Save(col))

	cfgCol, _ := w.app.FindCachedCollectionByNameOrId(core.CollectionNamePermanentCollections)

	bad := func(mut func(r *core.Record)) {
		t.Helper()

		r := core.NewRecord(cfgCol)
		r.Set("collection", "roll")
		r.Set("subjectPaths", []string{"person"})
		r.Set("retainReason", "Membership roll")
		mut(r)

		if err := w.app.Save(r); err == nil {
			t.Fatal("expected the configuration to be refused")
		}
	}
	bad(func(r *core.Record) { r.Set("collection", "nosuch") })
	bad(func(r *core.Record) { r.Set("collection", "users") })
	bad(func(r *core.Record) { r.Set("collection", core.CollectionNameOrgs) })
	bad(func(r *core.Record) { r.Set("retainReason", "") })
	bad(func(r *core.Record) { r.Set("subjectPaths", []string{}) })
	bad(func(r *core.Record) { r.Set("subjectPaths", []string{"nosuchfield"}) })
	bad(func(r *core.Record) { r.Set("signers", 3) })
	bad(func(r *core.Record) { r.Set("mutableFields", []string{"nosuchfield"}) })
	bad(func(r *core.Record) { r.Set("mutableFields", []string{"finalizedAt"}) })
	bad(func(r *core.Record) { r.Set("annotationCollection", "roll") })

	// nothing was left behind by the refused rows
	col, _ = w.app.FindCollectionByNameOrId("roll")
	if col.Fields.GetByName("finalizedAt") != nil {
		t.Fatal("a refused configuration must not change the collection")
	}

	r := core.NewRecord(cfgCol)
	r.Set("collection", "roll")
	r.Set("subjectPaths", []string{"person"})
	r.Set("mutableFields", []string{"restricted", "note"})
	r.Set("retainReason", "Membership roll")
	must(t, w.app.Save(r))

	cfg := core.PermanentConfigFor(w.app, "roll")
	if cfg == nil || cfg.Chain || cfg.Signers != 1 || cfg.AnnotationCollection != "roll_annotations" {
		t.Fatalf("config %+v", cfg)
	}

	col, _ = w.app.FindCollectionByNameOrId("roll")
	if col.Fields.GetByName("finalizedAt") == nil || col.Fields.GetByName("seq") != nil {
		t.Fatal("fields: finalizedAt expected, seq not (no chain)")
	}

	e := core.NewRecord(col)
	e.Set("org", w.org.Id)
	e.Set("title", "Member")
	must(t, w.app.Save(e))

	_, err := core.FinalizeRegisterEntry(w.app, w.ctx, w.user["reg1"], "roll", e.Id)
	must(t, err)

	e = mustFresh(t, w.app, "roll", e.Id)

	// "note" is mutable here, "title" is not
	e.Set("note", "moved away")
	must(t, w.app.Save(e))

	e = mustFresh(t, w.app, "roll", e.Id)
	e.Set("title", "Changed")

	if err := w.app.Save(e); err == nil {
		t.Fatal("title is not mutable")
	}

	if v := mustVerifyN(t, w.app, "roll"); !v.OK() || v.Chain || v.Finalized != 1 {
		t.Fatalf("verify: %+v", v)
	}
}

func mustVerifyN(t testing.TB, app core.App, name string) *core.RegisterVerification {
	t.Helper()

	res, err := core.VerifyRegisters(app)
	must(t, err)

	for _, v := range res {
		if v.Collection == name {
			return v
		}
	}

	t.Fatalf("no result for %s", name)

	return nil
}
