package core_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #80, 80-1 to 80-3): the household model, its guards,
// the rule helpers and the graduation job.
//
// World: the org tree of the spec's prototype (D > N > P, Q; Y below P) with
// two households in P.
//
//	H1: mix (head, guardian), ned (spouse, guardian), yan (adult with a login,
//	    19 years old), kid1 (a minor, no login, 10 years old)
//	H2: qed (guardian) and kid2 (a minor)
//
// pat is admin of P (staff), zed is a member of P tagged "registrar" (staff),
// ann (admin of D) and gwen (member of D) are not staff of P.

type hhWorld struct {
	*ancestorWorld
	P    *core.Record
	h1   *core.Record
	h2   *core.Record
	kid1 *core.Record
	kid2 *core.Record
	mixM *core.Record
	nedM *core.Record
	yanM *core.Record
	qedM *core.Record
	note *core.Collection
	ctx  context.Context
}

func years(n int) string { return time.Now().AddDate(-n, 0, -3).Format("2006-01-02") }

func newHHWorld(t testing.TB) *hhWorld {
	t.Helper()

	setTestKEK(t)

	aw := newAncestorWorld(t)
	app := aw.f.app

	// the migration creates plain fields; the server encrypts them at start-up
	if err := core.UpgradeHouseholdEncryption(app); err != nil {
		t.Fatal(err)
	}

	w := &hhWorld{ancestorWorld: aw, ctx: core.WithAuditContext(context.Background(), core.AuditActor{Type: core.AuditActorTypeSystem}, "")}

	orgs, _ := app.FindAllRecords(core.CollectionNameOrgs)
	for _, o := range orgs {
		if o.GetString("slug") == "pp" {
			w.P = o
		}
	}

	// zed: a member of P with the registrar tag
	rolesCol, _ := app.FindCollectionByNameOrId(core.CollectionNameOrgRoles)
	tag := core.NewRecord(rolesCol)
	tag.Set("org", w.P.Id)
	tag.Set("name", core.HouseholdRegistrarTag)
	mustSave(t, app, tag)

	membersCol, _ := app.FindCollectionByNameOrId(core.CollectionNameOrgMembers)
	zm := core.NewRecord(membersCol)
	zm.Set("org", w.P.Id)
	zm.Set("user", w.users["zed"].Id)
	zm.Set("role", "member")
	zm.Set("status", "active")
	zm.Set("tags", []string{tag.Id})
	mustSave(t, app, zm)

	hCol, _ := app.FindCollectionByNameOrId(core.CollectionNameHouseholds)
	mkH := func(name string) *core.Record {
		h := core.NewRecord(hCol)
		h.Set("org", w.P.Id)
		h.Set("name", name)
		h.Set("status", core.HouseholdStatusActive)
		h.Set("address", map[string]any{"street": "1 Main St"})
		h.Set("homePhone", "+1 555 0100")
		mustSave(t, app, h)

		return h
	}
	w.h1, w.h2 = mkH("House One"), mkH("House Two")

	mCol, _ := app.FindCollectionByNameOrId(core.CollectionNameHouseholdMembers)
	mkM := func(h *core.Record, user, name, role string, guardian bool, birth string) *core.Record {
		m := core.NewRecord(mCol)
		m.Set("household", h.Id)

		if user != "" {
			m.Set("user", w.users[user].Id)
		}

		m.Set("name", name)
		m.Set("role", role)
		m.Set("guardian", guardian)
		m.Set("status", core.HouseholdMemberActive)
		m.Set("birthDate", birth)
		mustSave(t, app, m)

		return m
	}
	w.mixM = mkM(w.h1, "mix", "Mix", "head", true, years(40))
	w.nedM = mkM(w.h1, "ned", "Ned", "spouse", true, years(38))
	w.yanM = mkM(w.h1, "yan", "Yan", "adult", false, years(19))
	w.kid1 = mkM(w.h1, "", "Kid One", "child", false, years(10))
	w.qedM = mkM(w.h2, "qed", "Qed", "head", true, years(45))
	w.kid2 = mkM(w.h2, "", "Kid Two", "child", false, years(8))

	// an app collection that describes a person
	note := core.NewBaseCollection("hh_notes")
	note.Fields.Add(
		&core.TextField{Name: "note"},
		&core.RelationField{Name: "household", CollectionId: hCol.Id, MaxSelect: 1},
		&core.RelationField{Name: "member", CollectionId: mCol.Id, MaxSelect: 1},
	)
	set := core.HouseholdRuleSet(core.HouseholdRuleOpts{})
	note.ListRule, note.ViewRule, note.CreateRule, note.UpdateRule, note.DeleteRule = set.List, set.View, set.Create, set.Update, set.Delete
	mustSave(t, app, note)
	w.note = note

	return w
}

func mustSave(t testing.TB, app core.App, r core.Model) {
	t.Helper()

	if err := app.Save(r); err != nil {
		t.Fatal(err)
	}
}

func (w *hhWorld) noteAbout(t testing.TB, m *core.Record) *core.Record {
	t.Helper()

	r := core.NewRecord(w.note)
	r.Set("note", "about "+m.GetString("name"))
	r.Set("member", m.Id)
	r.Set("household", m.GetString("household"))
	mustSave(t, w.f.app, r)

	return r
}

// whoCan returns the sorted users that the rule lets access the record.
func (w *hhWorld) whoCan(t testing.TB, rec *core.Record, rule string) string {
	t.Helper()

	var names []string

	for name, u := range w.users {
		ok, err := w.f.app.CanAccessRecord(rec, &core.RequestInfo{Auth: u, Context: core.RequestInfoContextDefault}, &rule)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}

		if ok {
			names = append(names, name)
		}
	}

	// a guest
	if ok, err := w.f.app.CanAccessRecord(rec, &core.RequestInfo{Context: core.RequestInfoContextDefault}, &rule); err != nil {
		t.Fatal(err)
	} else if ok {
		names = append(names, "GUEST")
	}

	slices.Sort(names)

	return strings.Join(names, ",")
}

func (w *hhWorld) reload(t testing.TB, r *core.Record) *core.Record {
	t.Helper()

	out, err := w.f.app.FindRecordById(r.Collection(), r.Id)
	if err != nil {
		t.Fatal(err)
	}

	return out
}

func (w *hhWorld) actor(t testing.TB, h *core.Record, user string) *core.HouseholdActor {
	t.Helper()

	return core.NewHouseholdActor(w.f.app, w.P, h, w.users[user])
}

func (w *hhWorld) auditCount(t testing.TB, action string) int {
	t.Helper()

	return credAuditCount(t, w.f.app, action)
}

// ---- age -----------------------------------------------------------------------

func TestIsMinorAt(t *testing.T) {
	d := func(s string) time.Time {
		v, err := core.ParseBirthDate(s)
		if err != nil {
			t.Fatal(err)
		}

		return v
	}

	cases := []struct {
		birth, now string
		age        int
		minor      bool
	}{
		{"2010-05-17", "2028-05-16", 18, true},  // the day before the 18th birthday
		{"2010-05-17", "2028-05-17", 18, false}, // the birthday itself
		{"2010-05-17", "2031-05-16", 21, true},
		{"2010-05-17", "2031-05-17", 21, false},
		{"2012-02-29", "2030-02-28", 18, true}, // leap day: the 18th birthday falls on Mar 1
		{"2012-02-29", "2030-03-01", 18, false},
		{"2010-05-17", "2010-05-17", 18, true}, // newborn
	}

	for _, c := range cases {
		if got := core.IsMinorAt(d(c.birth), d(c.now), c.age); got != c.minor {
			t.Errorf("born %s, on %s, age %d: got minor=%v, want %v", c.birth, c.now, c.age, got, c.minor)
		}
	}

	for _, bad := range []string{"", "soon", "17/05/2010", "2010-13-40"} {
		if _, err := core.ParseBirthDate(bad); err == nil {
			t.Errorf("expected %q to be refused", bad)
		}
	}

	if _, err := core.ParseBirthDate("2010-05-17T10:00:00.000Z"); err != nil {
		t.Errorf("a date-time must be accepted: %v", err)
	}
}

func TestHouseholdGraduationAgeSetting(t *testing.T) {
	w := newHHWorld(t)
	app := w.f.app

	if got := core.HouseholdGraduationAge(w.P); got != 18 {
		t.Fatalf("default %d", got)
	}

	for in, want := range map[string]int{`{"graduationAge":16}`: 16, `{"graduationAge":21}`: 21, `{"graduationAge":14}`: 14} {
		w.P.Set("settings", in)
		mustSave(t, app, w.P)

		if got := core.HouseholdGraduationAge(w.P); got != want {
			t.Errorf("%s: got %d", in, got)
		}
	}

	// out of bounds or not a whole number: refused when saved
	for _, in := range []string{`{"graduationAge":13}`, `{"graduationAge":22}`, `{"graduationAge":17.5}`, `{"graduationAge":"18"}`} {
		w.P.Set("settings", in)

		if err := app.Save(w.P); err == nil {
			t.Errorf("%s: expected the save to be refused", in)
		}
	}
}

// ---- the model and its guards ----------------------------------------------------

func TestHouseholdMemberDerivesIsMinor(t *testing.T) {
	w := newHHWorld(t)

	if !w.kid1.GetBool("isMinor") || w.yanM.GetBool("isMinor") || w.mixM.GetBool("isMinor") {
		t.Fatalf("kid1=%v yan=%v mix=%v", w.kid1.GetBool("isMinor"), w.yanM.GetBool("isMinor"), w.mixM.GetBool("isMinor"))
	}

	// the stored flag is overridden by the birth date
	w.kid1.Set("isMinor", false)
	mustSave(t, w.f.app, w.kid1)

	if !w.reload(t, w.kid1).GetBool("isMinor") {
		t.Error("the birth date says minor")
	}

	// the sensitive fields are ciphertext in the database
	if raw := rawColumn(t, w.f.app, w.kid1.Collection(), w.kid1.Id, "birthDate"); !strings.HasPrefix(raw, "enc:v1:") {
		t.Errorf("birthDate is stored as %q", raw)
	}

	if raw := rawColumn(t, w.f.app, w.h1.Collection(), w.h1.Id, "address"); !strings.HasPrefix(raw, "enc:v1:") || strings.Contains(raw, "Main St") {
		t.Errorf("address is stored as %q", raw)
	}

	// a bad birth date is refused
	w.kid1.Set("birthDate", "soon")

	if err := w.f.app.Save(w.kid1); err == nil {
		t.Error("expected a bad birth date to be refused")
	}
}

func TestHouseholdGuards(t *testing.T) {
	w := newHHWorld(t)
	app := w.f.app
	mCol := w.kid1.Collection()

	newMember := func(h *core.Record, name string) *core.Record {
		m := core.NewRecord(mCol)
		m.Set("household", h.Id)
		m.Set("name", name)
		m.Set("status", core.HouseholdMemberActive)

		return m
	}

	// a minor has no login
	m := newMember(w.h1, "Teen")
	m.Set("birthDate", years(12))
	m.Set("user", w.users["gwen"].Id)

	if err := app.Save(m); err == nil || !strings.Contains(err.Error(), "login") {
		t.Errorf("a minor with a login: %v", err)
	}

	// an explicit isMinor (no birth date) counts too
	m = newMember(w.h1, "Teen2")
	m.Set("isMinor", true)
	m.Set("user", w.users["gwen"].Id)

	if err := app.Save(m); err == nil {
		t.Error("an explicit minor with a login must be refused")
	}

	// a minor is not a guardian
	m = newMember(w.h1, "Teen3")
	m.Set("birthDate", years(12))
	m.Set("guardian", true)

	if err := app.Save(m); err == nil || !strings.Contains(err.Error(), "guardian") {
		t.Errorf("a minor guardian: %v", err)
	}

	// an adult may be added with a login, once per household
	m = newMember(w.h1, "Gwen")
	m.Set("user", w.users["gwen"].Id)
	mustSave(t, app, m)

	dup := newMember(w.h1, "Gwen again")
	dup.Set("user", w.users["gwen"].Id)

	if err := app.Save(dup); err == nil {
		t.Error("the same login twice in a household must be refused (unique index)")
	}

	// the same login in another household is fine
	other := newMember(w.h2, "Gwen elsewhere")
	other.Set("user", w.users["gwen"].Id)
	mustSave(t, app, other)

	// two people without a login do not collide
	mustSave(t, app, newMember(w.h1, "No login A"))
	mustSave(t, app, newMember(w.h1, "No login B"))

	// a household with an active minor needs an adult guardian
	h3 := core.NewRecord(w.h1.Collection())
	h3.Set("org", w.P.Id)
	h3.Set("name", "House Three")
	h3.Set("status", core.HouseholdStatusActive)
	mustSave(t, app, h3)

	m = newMember(h3, "Lonely kid")
	m.Set("birthDate", years(5))

	if err := app.Save(m); err == nil || !strings.Contains(err.Error(), "guardian") {
		t.Errorf("a minor in a household with no guardian: %v", err)
	}

	adult := newMember(h3, "Adult")
	mustSave(t, app, adult)

	m = newMember(h3, "Still no guardian")
	m.Set("birthDate", years(5))

	if err := app.Save(m); err == nil {
		t.Error("an adult who is not a guardian does not count")
	}

	adult.Set("guardian", true)
	mustSave(t, app, adult)

	m = newMember(h3, "Kid")
	m.Set("birthDate", years(5))
	mustSave(t, app, m)

	// the last guardian cannot step down, leave or be marked deceased while the minor is active
	for name, change := range map[string]func(r *core.Record){
		"demoted":  func(r *core.Record) { r.Set("guardian", false) },
		"left":     func(r *core.Record) { r.Set("status", core.HouseholdMemberLeft) },
		"deceased": func(r *core.Record) { r.Set("status", core.HouseholdMemberDeceased) },
	} {
		a := w.reload(t, adult)
		change(a)

		if err := app.Save(a); err == nil {
			t.Errorf("%s: the last guardian was changed", name)
		}
	}

	// a second guardian lifts the restriction
	second := newMember(h3, "Second")
	second.Set("guardian", true)
	mustSave(t, app, second)

	a := w.reload(t, adult)
	a.Set("guardian", false)
	mustSave(t, app, a)

	// and a minor who left no longer needs one
	k := w.reload(t, m)
	k.Set("status", core.HouseholdMemberLeft)
	mustSave(t, app, k)

	s := w.reload(t, second)
	s.Set("status", core.HouseholdMemberLeft)
	mustSave(t, app, s)
}

func TestHouseholdOperations(t *testing.T) {
	w := newHHWorld(t)
	app := w.f.app
	ctx := w.ctx

	// self service: a member of P creates a household and becomes its guardian
	h, err := core.CreateHousehold(app, ctx, w.users["yan"], w.P, core.HouseholdInput{Org: w.P.Id, Name: "Yan family"})
	if err == nil {
		t.Fatalf("yan is a member of Y, not of P, but created %v", h)
	}

	// mix is a member of P
	h, err = core.CreateHousehold(app, ctx, w.users["mix"], w.P, core.HouseholdInput{
		Org: w.P.Id, Name: "Mix family", Address: map[string]any{"city": "Springfield"}, HomePhone: "+1 555 0199",
	})
	if err != nil {
		t.Fatal(err)
	}

	ms, _ := core.HouseholdMembers(app, h)
	if len(ms) != 1 || !ms[0].GetBool("guardian") || ms[0].GetString("user") != w.users["mix"].Id || ms[0].GetString("role") != "head" {
		t.Fatalf("unexpected first member: %v", ms)
	}

	// a guardian adds a child; the birth date makes it a minor
	g := w.actor(t, h, "mix")
	if !g.Guardian() {
		t.Fatal("mix must be a guardian of the new household")
	}

	kid, err := core.AddHouseholdMember(app, ctx, g, w.P, h, map[string]any{"name": "Baby", "role": "child", "birthDate": years(2), "allergies": map[string]any{"peanuts": true}})
	if err != nil {
		t.Fatal(err)
	}

	if !kid.GetBool("isMinor") || kid.GetString("source") != core.HouseholdSourceManual {
		t.Errorf("unexpected child %v", kid)
	}

	// a guardian cannot give someone else a login; staff can
	if _, err := core.AddHouseholdMember(app, ctx, g, w.P, h, map[string]any{"name": "Gwen", "user": w.users["gwen"].Id}); err == nil {
		t.Error("a guardian attached a login to someone else")
	}

	staff := w.actor(t, h, "pat")
	if !staff.Staff {
		t.Fatal("pat is admin of P")
	}

	if !w.actor(t, h, "zed").Staff {
		t.Fatal("a member tagged registrar is staff")
	}

	if w.actor(t, h, "ann").Staff || w.actor(t, h, "gwen").Staff {
		t.Fatal("staff of an ancestor org are not staff of P")
	}

	gm, err := core.AddHouseholdMember(app, ctx, staff, w.P, h, map[string]any{"name": "Gwen", "user": w.users["gwen"].Id, "guardian": true})
	if err != nil {
		t.Fatal(err)
	}

	// a stranger can do nothing
	for _, who := range []string{"qed", "ann", "zed2"} {
		a := &core.HouseholdActor{OrgActor: &core.OrgActor{}}
		if _, err := core.AddHouseholdMember(app, ctx, a, w.P, h, map[string]any{"name": "X"}); err == nil {
			t.Errorf("%s added a person", who)
		}
	}

	// another guardian cannot demote or remove a guardian; the person can step down
	if _, err := core.SetHouseholdGuardian(app, ctx, g, w.P, h, gm, false); err == nil {
		t.Error("a guardian demoted another guardian")
	}

	if err := core.RemoveHouseholdMember(app, ctx, g, w.P, h, gm); err == nil {
		t.Error("a guardian removed another guardian")
	}

	if _, err := core.SetHouseholdGuardian(app, ctx, w.actor(t, h, "gwen"), w.P, h, gm, false); err != nil {
		t.Errorf("stepping down: %v", err)
	}

	// the last guardian cannot be removed by anyone while the child is active
	mixM := ms[0]
	if err := core.RemoveHouseholdMember(app, ctx, staff, w.P, h, mixM); err == nil || !strings.Contains(err.Error(), "guardian") {
		t.Errorf("the last guardian was removed: %v", err)
	}

	if _, err := core.SetHouseholdGuardian(app, ctx, g, w.P, h, mixM, false); err == nil {
		t.Error("the last guardian stepped down")
	}

	// staff make gwen a guardian again, then remove mix at once
	if _, err := core.SetHouseholdGuardian(app, ctx, staff, w.P, h, gm, true); err != nil {
		t.Fatal(err)
	}

	before := w.auditCount(t, "delete")
	if err := core.RemoveHouseholdMember(app, ctx, staff, w.P, h, mixM); err != nil {
		t.Fatal(err)
	}

	if w.auditCount(t, "delete") != before+1 {
		t.Error("the removal is not audited")
	}

	if w.actor(t, h, "mix").Member != nil {
		t.Error("mix still has a row")
	}

	// the audit entries of the people carry the org
	entries, _ := app.FindRecordsByFilter(core.CollectionNameAuditLog, "targetCollection={:c} && action='delete'", "", 0, 0, dbx.Params{"c": core.CollectionNameHouseholdMembers})
	if len(entries) == 0 || !strings.Contains(entries[0].GetString("meta"), w.P.Id) {
		t.Errorf("expected meta.orgId on the entry: %v", entries)
	}

	// a person with a login may change their own data, not their role
	gmActor := w.actor(t, h, "gwen")

	// (a non-guardian with a login: gwen stepped down above and was made a guardian again,
	// so use a new person)
	plain, err := core.AddHouseholdMember(app, ctx, staff, w.P, h, map[string]any{"name": "Plain", "user": w.users["yan"].Id})
	if err != nil {
		t.Fatal(err)
	}

	plainActor := w.actor(t, h, "yan")
	if _, err := core.UpdateHouseholdMember(app, ctx, plainActor, w.P, h, plain, map[string]any{"role": "head"}); err == nil {
		t.Error("a person changed their own role")
	}

	if _, err := core.UpdateHouseholdMember(app, ctx, plainActor, w.P, h, plain, map[string]any{"guardian": true}); err == nil {
		t.Error("a person made themselves a guardian")
	}

	if _, err := core.UpdateHouseholdMember(app, ctx, plainActor, w.P, h, gm, map[string]any{"name": "Hijack"}); err == nil {
		t.Error("a person changed another adult")
	}

	if _, err := core.UpdateHouseholdMember(app, ctx, plainActor, w.P, h, plain, map[string]any{"name": "Plain P", "status": "left"}); err != nil {
		t.Errorf("a person may rename and leave: %v", err)
	}

	if _, err := core.UpdateHouseholdMember(app, ctx, gmActor, w.P, h, gm, map[string]any{"name": "Gwen G"}); err != nil {
		t.Error(err)
	}

	// archive: staff only, and no more additions
	if _, err := core.UpdateHousehold(app, ctx, gmActor, w.P, h, map[string]any{"status": "archived"}); err == nil {
		t.Error("a guardian archived the household")
	}

	if _, err := core.UpdateHousehold(app, ctx, staff, w.P, h, map[string]any{"status": "archived"}); err != nil {
		t.Fatal(err)
	}

	if _, err := core.AddHouseholdMember(app, ctx, staff, w.P, h, map[string]any{"name": "Late"}); err == nil {
		t.Error("added to an archived household")
	}

	mine, _ := core.MyHouseholds(app, w.users["gwen"].Id)
	if len(mine) != 0 {
		t.Errorf("an archived household is not listed: %v", mine)
	}

	mine, _ = core.MyHouseholds(app, w.users["ned"].Id)
	if len(mine) != 1 || mine[0].Id != w.h1.Id {
		t.Errorf("ned's households: %v", mine)
	}
}

// ---- rules (80-2) -----------------------------------------------------------------

func TestHouseholdSystemCollectionRules(t *testing.T) {
	w := newHHWorld(t)

	hRule, mRule := core.HouseholdRules()
	col, _ := w.f.app.FindCollectionByNameOrId(core.CollectionNameHouseholds)

	if col.ListRule == nil || *col.ListRule != hRule || col.CreateRule != nil || col.UpdateRule != nil || col.DeleteRule != nil {
		t.Fatalf("unexpected rules on _households: %v", col.ListRule)
	}

	pcol, _ := w.f.app.FindCollectionByNameOrId(core.CollectionNameHouseholdMembers)
	if pcol.ListRule == nil || *pcol.ListRule != mRule || pcol.CreateRule != nil || pcol.UpdateRule != nil || pcol.DeleteRule != nil {
		t.Fatalf("unexpected rules on _householdMembers: %v", pcol.ListRule)
	}

	// the household, its people and its child: the household's people, plus the staff of P
	want := "mix,ned,pat,yan,zed"

	if got := w.whoCan(t, w.h1, *col.ViewRule); got != want {
		t.Errorf("_households view: got %q, want %q", got, want)
	}

	if got := w.whoCan(t, w.kid1, *pcol.ViewRule); got != want {
		t.Errorf("_householdMembers view of the child: got %q, want %q", got, want)
	}

	// the other household
	if got := w.whoCan(t, w.kid2, *pcol.ViewRule); got != "pat,qed,zed" {
		t.Errorf("other household: got %q", got)
	}

	// a person with a login reads their own row even after leaving
	w.yanM.Set("status", core.HouseholdMemberLeft)
	mustSave(t, w.f.app, w.yanM)

	if got := w.whoCan(t, w.yanM, *pcol.ViewRule); got != "mix,ned,pat,yan,zed" {
		t.Errorf("own row after leaving: got %q", got)
	}

	if got := w.whoCan(t, w.h1, *col.ViewRule); got != "mix,ned,pat,zed" {
		t.Errorf("a person who left no longer sees the household: got %q", got)
	}

	// a suspended or tagless member of the org is not staff; an archived org closes staff access
	w.P.Set("archived", true)
	mustSave(t, w.f.app, w.P)

	if got := w.whoCan(t, w.h1, *col.ViewRule); got != "mix,ned" {
		t.Errorf("archived org: got %q", got)
	}
}

func TestHouseholdPersonRules(t *testing.T) {
	w := newHHWorld(t)
	app := w.f.app

	n1 := w.noteAbout(t, w.kid1) // a minor of H1
	n2 := w.noteAbout(t, w.kid2) // a minor of H2
	n3 := w.noteAbout(t, w.yanM) // the graduated adult of H1, with a login
	n4 := w.noteAbout(t, w.mixM) // a guardian

	read := *w.note.ListRule
	write := *w.note.UpdateRule

	// household members read; other households see nothing; guests see nothing
	for rec, want := range map[*core.Record]string{n1: "mix,ned,yan", n2: "qed", n3: "mix,ned,yan", n4: "mix,ned,yan"} {
		if got := w.whoCan(t, rec, read); got != want {
			t.Errorf("read %q: got %q, want %q", rec.GetString("note"), got, want)
		}
	}

	// guardians write about a minor; staff are not guardians; nobody writes about an adult
	for rec, want := range map[*core.Record]string{n1: "mix,ned", n2: "qed", n3: "", n4: ""} {
		if got := w.whoCan(t, rec, write); got != want {
			t.Errorf("write %q: got %q, want %q", rec.GetString("note"), got, want)
		}
	}

	// the copy cannot point at another household's child
	forged := core.NewRecord(w.note)
	forged.Set("member", w.kid2.Id)
	forged.Set("household", w.h1.Id) // mix is a guardian of H1, but kid2 is not in H1

	if got := w.whoCan(t, forged, *w.note.CreateRule); got != "" {
		t.Errorf("a forged copy must not pass the create rule, got %q", got)
	}

	// and BindHouseholdCopy repairs it before it is saved
	core.BindHouseholdCopy(app, "hh_notes", core.HouseholdRuleOpts{})
	mustSave(t, app, forged)

	if got := forged.GetString("household"); got != w.h2.Id {
		t.Errorf("the household copy was not synced: %s", got)
	}

	// REMOVAL ends access: mix's row is marked left
	w.mixM.Set("status", core.HouseholdMemberLeft)
	mustSave(t, app, w.mixM)

	if got := w.whoCan(t, n1, write); got != "ned" {
		t.Errorf("after the removal of mix: write got %q", got)
	}

	if got := w.whoCan(t, n1, read); got != "ned,yan" {
		t.Errorf("after the removal of mix: read got %q", got)
	}

	// deleting the row does the same
	if err := app.Delete(w.reload(t, w.nedM)); err == nil {
		// the model hook is not a delete guard; the route is. The row is gone.
		if got := w.whoCan(t, n1, write); got != "" {
			t.Errorf("after the delete of ned: write got %q", got)
		}
	} else {
		t.Fatal(err)
	}
}

func TestHouseholdGraduationEndsGuardianAccess(t *testing.T) {
	w := newHHWorld(t)
	app := w.f.app

	n1 := w.noteAbout(t, w.kid1)
	write := *w.note.UpdateRule
	read := *w.note.ListRule

	if got := w.whoCan(t, n1, write); got != "mix,ned" {
		t.Fatalf("before: %q", got)
	}

	// kid1 is 10: not yet at 17, graduated at 19
	if rep, err := core.RunHouseholdGraduation(app, w.ctx, time.Now().AddDate(7, 0, 0)); err != nil || rep.Graduated != 0 {
		t.Fatalf("too early: %+v %v", rep, err)
	}

	if !w.reload(t, w.kid1).GetBool("isMinor") {
		t.Fatal("kid1 graduated too early")
	}

	later := time.Now().AddDate(9, 0, 0)

	rep, err := core.RunHouseholdGraduation(app, w.ctx, later)
	if err != nil || rep.Graduated != 1 {
		t.Fatalf("expected one graduation, got %+v %v", rep, err)
	}

	if w.reload(t, w.kid1).GetBool("isMinor") {
		t.Fatal("kid1 is still a minor")
	}

	// kid2 (8) has not graduated, yan (adult) is untouched
	if !w.reload(t, w.kid2).GetBool("isMinor") {
		t.Fatal("kid2 graduated")
	}

	// guardian write access ends, reads stay
	if got := w.whoCan(t, n1, write); got != "" {
		t.Errorf("after graduation write: %q", got)
	}

	if got := w.whoCan(t, n1, read); got != "mix,ned,yan" {
		t.Errorf("after graduation read: %q", got)
	}

	// the person's own records keep their member id: a login can now be given
	// (the model hook reads the clock too, so move it with the job's)
	defer core.SetHouseholdClockForTest(func() time.Time { return later })()

	k := w.reload(t, w.kid1)
	k.Set("user", w.users["gwen"].Id)
	mustSave(t, app, k)

	if got := w.whoCan(t, n1, read); got != "gwen,mix,ned,yan" {
		t.Errorf("after the login: %q", got)
	}

	// one audit entry, no names, no birth date, idempotent
	entries, _ := app.FindRecordsByFilter(core.CollectionNameAuditLog, "action={:a}", "", 0, 0, dbx.Params{"a": core.AuditActionHouseholdGraduated})
	if len(entries) != 1 {
		t.Fatalf("expected one graduation entry, got %d", len(entries))
	}

	e := entries[0]
	if e.GetString("targetId") != w.kid1.Id || e.GetString("targetCollection") != core.CollectionNameHouseholdMembers ||
		!strings.Contains(e.GetString("meta"), w.P.Id) || strings.Contains(e.GetString("after")+e.GetString("meta"), "Kid One") {
		t.Errorf("unexpected entry: %v", e.FieldsData())
	}

	if rep, _ := core.RunHouseholdGraduation(app, w.ctx, later); rep.Graduated != 0 {
		t.Errorf("a second run graduated %d", rep.Graduated)
	}

	if w.auditCount(t, core.AuditActionHouseholdGraduated) != 1 {
		t.Error("a second run wrote another entry")
	}
}

func TestHouseholdGraduationAgeIsPerOrg(t *testing.T) {
	w := newHHWorld(t)
	app := w.f.app

	w.P.Set("settings", `{"graduationAge":21}`)
	mustSave(t, app, w.P)

	// kid1 is 10 years old: 19 in nine years, 21 in eleven
	if rep, _ := core.RunHouseholdGraduation(app, w.ctx, time.Now().AddDate(9, 0, 0)); rep.Graduated != 0 {
		t.Fatalf("graduated at 19 with age 21: %+v", rep)
	}

	// the setting also drives isMinor when a row is saved
	w.yanM.Set("birthDate", years(19))
	w.yanM.Set("user", "")
	mustSave(t, app, w.yanM)

	if !w.reload(t, w.yanM).GetBool("isMinor") {
		t.Error("a 19 year old is a minor when the age is 21")
	}

	// a minor without a birth date is left alone, and counted
	m := core.NewRecord(w.kid1.Collection())
	m.Set("household", w.h1.Id)
	m.Set("name", "Unknown age")
	m.Set("isMinor", true)
	m.Set("status", core.HouseholdMemberActive)
	mustSave(t, app, m)

	rep, err := core.RunHouseholdGraduation(app, w.ctx, time.Now().AddDate(12, 0, 0))
	if err != nil {
		t.Fatal(err)
	}

	if rep.Graduated != 2 || rep.Skipped != 1 { // kid1 (22) and yan (31 in 12y? 19+12), the unknown-age row stays
		t.Errorf("unexpected report %+v", rep)
	}

	if !w.reload(t, m).GetBool("isMinor") {
		t.Error("the row without a birth date changed")
	}
}

// ---- encryption upgrade -------------------------------------------------------------

func TestHouseholdFieldsUpgradeToEncrypted(t *testing.T) {
	// no key when the migration runs
	t.Setenv("CLAVIGER_KEK", "")
	t.Setenv("CLAVIGER_KEK_FILE", "")

	aw := newAncestorWorld(t)
	app := aw.f.app

	col, _ := app.FindCollectionByNameOrId(core.CollectionNameHouseholdMembers)
	if f := col.Fields.GetByName("birthDate"); f == nil || f.Type() != core.FieldTypeText {
		t.Fatalf("expected a plain birthDate without a key, got %v", f)
	}

	var org *core.Record

	orgs, _ := app.FindAllRecords(core.CollectionNameOrgs)
	for _, o := range orgs {
		if o.GetString("slug") == "pp" {
			org = o
		}
	}

	ctx := context.Background()

	// sensitive values are refused while the fields are plain
	if _, err := core.CreateHousehold(app, ctx, aw.users["pat"], org, core.HouseholdInput{
		Org: org.Id, Name: "No key", Address: map[string]any{"a": "b"},
	}); err == nil || !strings.Contains(err.Error(), "not encrypted") {
		t.Fatalf("expected the address to be refused: %v", err)
	}

	h, err := core.CreateHousehold(app, ctx, aw.users["pat"], org, core.HouseholdInput{Org: org.Id, Name: "No key"})
	if err != nil {
		t.Fatal(err)
	}

	m, err := core.AddHouseholdMember(app, ctx, core.NewHouseholdActor(app, org, h, aw.users["pat"]), org, h, map[string]any{"name": "Pat"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := core.UpdateHouseholdMember(app, ctx, core.NewHouseholdActor(app, org, h, aw.users["pat"]), org, h, m,
		map[string]any{"birthDate": "1980-01-02"}); err == nil {
		t.Fatal("expected the birth date to be refused")
	}

	// a value that was written some other way (an import, a SQL console)
	if _, err := app.DB().Update(core.CollectionNameHouseholdMembers, dbx.Params{"birthDate": "1980-01-02"}, dbx.HashExp{"id": m.Id}).Execute(); err != nil {
		t.Fatal(err)
	}

	if _, err := app.DB().Update(core.CollectionNameHouseholds, dbx.Params{"address": `{"city":"Plain"}`, "homePhone": "+1 555 7"}, dbx.HashExp{"id": h.Id}).Execute(); err != nil {
		t.Fatal(err)
	}

	// the key arrives
	setTestKEK(t)

	if err := core.UpgradeHouseholdEncryption(app); err != nil {
		t.Fatal(err)
	}

	col, _ = app.FindCollectionByNameOrId(core.CollectionNameHouseholdMembers)
	for _, n := range []string{"birthDate", "allergies", "medical"} {
		if ty := col.Fields.GetByName(n).Type(); !strings.HasPrefix(ty, "encrypted") {
			t.Errorf("%s is still %s", n, ty)
		}
	}

	hcol, _ := app.FindCollectionByNameOrId(core.CollectionNameHouseholds)
	for _, n := range []string{"address", "homePhone"} {
		if ty := hcol.Fields.GetByName(n).Type(); !strings.HasPrefix(ty, "encrypted") {
			t.Errorf("%s is still %s", n, ty)
		}
	}

	if raw := rawColumn(t, app, col, m.Id, "birthDate"); !strings.HasPrefix(raw, "enc:v1:") {
		t.Errorf("the stored value was not encrypted: %q", raw)
	}

	// (PostgreSQL fails a statement prepared before the change once, then recovers)
	find := func(collection, id string) (*core.Record, error) {
		r, err := app.FindRecordById(collection, id)
		if err != nil && strings.Contains(err.Error(), "cached plan") {
			r, err = app.FindRecordById(collection, id)
		}

		return r, err
	}

	loaded, err := find(core.CollectionNameHouseholdMembers, m.Id)
	if err != nil || loaded.GetString("birthDate") != "1980-01-02" {
		t.Fatalf("the value must read back: %v", err)
	}

	hh, err := find(core.CollectionNameHouseholds, h.Id)
	if err != nil || !strings.Contains(hh.GetString("address"), `"city":"Plain"`) || hh.GetString("homePhone") != "+1 555 7" {
		t.Fatalf("the household values must read back: %v", err)
	}

	if raw := rawColumn(t, app, hcol, h.Id, "address"); !strings.HasPrefix(raw, "enc:v1:") || strings.Contains(raw, "Plain") {
		t.Errorf("the stored address was not encrypted: %q", raw)
	}

	// now sensitive values are accepted, and the run is idempotent
	if err := core.UpgradeHouseholdEncryption(app); err != nil {
		t.Fatal(err)
	}

	if _, err := core.UpdateHousehold(app, ctx, core.NewHouseholdActor(app, org, h, aw.users["pat"]), org, h, map[string]any{"homePhone": "+1 555 0000"}); err != nil {
		t.Fatal(err)
	}
}
