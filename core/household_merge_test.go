package core_test

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"slices"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/filesystem"
)

// Fork-local (Claviger #80, 80-4): merge, split, duplicates, photos and the
// repair of the household copies. World: see household_test.go.

func (w *hhWorld) status(t testing.TB, err error, want int) {
	t.Helper()

	if want == 0 {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		return
	}

	oe, ok := core.AsOrgError(err)
	if !ok || oe.Status != want {
		t.Fatalf("want status %d, got %v", want, err)
	}
}

func (w *hhWorld) membersOf(t testing.TB, h *core.Record) []string {
	t.Helper()

	rows, err := core.HouseholdMembers(w.f.app, h)
	if err != nil {
		t.Fatal(err)
	}

	ids := memberIDs(rows)
	slices.Sort(ids)

	return ids
}

func memberIDs(rows []*core.Record) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Id)
	}

	return out
}

func (w *hhWorld) merge(t testing.TB, user string, into, from *core.Record) error {
	t.Helper()

	return core.MergeHouseholds(w.f.app, w.ctx, w.actor(t, into, user), w.P, into, from.Id)
}

func TestHouseholdMerge(t *testing.T) {
	w := newHHWorld(t)
	app := w.f.app

	n1 := w.noteAbout(t, w.kid1)
	n2 := w.noteAbout(t, w.kid2)

	moved := w.membersOf(t, w.h2)

	w.status(t, w.merge(t, "pat", w.h1, w.h2), 0)

	if got := len(w.membersOf(t, w.h1)); got != 6 {
		t.Fatalf("target has %d people", got)
	}

	if got := w.membersOf(t, w.h2); len(got) != 0 {
		t.Fatalf("source still has %v", got)
	}

	if w.reload(t, w.h2).GetString("status") != core.HouseholdStatusArchived || w.reload(t, w.h1).GetString("status") != core.HouseholdStatusActive {
		t.Fatal("the source must be archived and the target stay active")
	}

	// the copies follow
	if got := w.reload(t, n2).GetString("household"); got != w.h1.Id {
		t.Fatalf("the moved person's note still points at %s", got)
	}

	if got := w.reload(t, n1).GetString("household"); got != w.h1.Id {
		t.Fatalf("an untouched note changed to %s", got)
	}

	// the guardian rule follows at once: qed now guards h1's records
	if got := w.whoCan(t, w.reload(t, n2), *core.HouseholdRuleSet(core.HouseholdRuleOpts{}).Update); !strings.Contains(got, "qed") {
		t.Fatalf("qed should write the minor's note now: %s", got)
	}

	// one entry, ids only
	if got := w.auditCount(t, core.AuditActionHouseholdMerge); got != 1 {
		t.Fatalf("%d merge entries", got)
	}

	entries, _ := app.FindRecordsByFilter(core.CollectionNameAuditLog, "action={:a}", "", 0, 0, map[string]any{"a": core.AuditActionHouseholdMerge})
	raw := entries[0].GetString("meta")

	var meta map[string]any
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		t.Fatal(err)
	}

	if meta["mergedFrom"] != w.h2.Id || meta["orgId"] != w.P.Id || meta["copiesRewritten"].(float64) != 1 {
		t.Fatalf("meta: %s", raw)
	}

	ids, _ := meta["movedMembers"].([]any)
	if len(ids) != len(moved) {
		t.Fatalf("moved members: %s", raw)
	}

	for _, secret := range []string{"Kid Two", "Qed", years(8)} {
		if strings.Contains(entries[0].GetString("after")+entries[0].GetString("before")+raw, secret) {
			t.Fatalf("the audit entry leaks %q", secret)
		}
	}

	// no per-person entries
	if recs, _ := app.FindRecordsByFilter(core.CollectionNameAuditLog, "targetCollection={:c} && action='update'", "", 0, 0,
		map[string]any{"c": core.CollectionNameHouseholdMembers}); len(recs) != 0 {
		t.Fatalf("%d update entries for the people", len(recs))
	}
}

func TestHouseholdMergeRefusals(t *testing.T) {
	w := newHHWorld(t)
	app := w.f.app

	// who may
	w.status(t, w.merge(t, "zed", w.h1, w.h2), 0)
	w = newHHWorld(t)

	for _, u := range []string{"mix", "ned", "yan", "ann", "gwen"} {
		if err := w.merge(t, u, w.h1, w.h2); err == nil {
			t.Fatalf("%s must not merge", u)
		}
	}

	w.status(t, w.merge(t, "mix", w.h1, w.h2), 403)
	w.status(t, w.merge(t, "pat", w.h1, w.h1), 400)
	w.status(t, core.MergeHouseholds(app, w.ctx, w.actor(t, w.h1, "pat"), w.P, w.h1, ""), 400)
	w.status(t, core.MergeHouseholds(app, w.ctx, w.actor(t, w.h1, "pat"), w.P, w.h1, "nope"), 404)

	// another org
	hCol, _ := app.FindCollectionByNameOrId(core.CollectionNameHouseholds)
	orgs, _ := app.FindAllRecords(core.CollectionNameOrgs)

	var Q *core.Record

	for _, o := range orgs {
		if o.GetString("slug") == "qq" {
			Q = o
		}
	}

	other := core.NewRecord(hCol)
	other.Set("org", Q.Id)
	other.Set("name", "Elsewhere")
	other.Set("status", core.HouseholdStatusActive)
	mustSave(t, app, other)

	w.status(t, w.merge(t, "pat", w.h1, other), 404)

	// archived on either side
	for _, pair := range [][2]*core.Record{{w.h1, w.h2}, {w.h2, w.h1}} {
		w = newHHWorld(t)
		app = w.f.app

		into, from := w.h1, w.h2
		if pair[0] == w.h2 {
			into, from = w.h2, w.h1
		}

		if _, err := app.NonconcurrentDB().NewQuery("UPDATE _households SET status='archived' WHERE id={:id}").
			Bind(map[string]any{"id": from.Id}).Execute(); err != nil {
			t.Fatal(err)
		}

		w.status(t, w.merge(t, "pat", into, from), 409)

		// and the archived one as the target
		if _, err := app.NonconcurrentDB().NewQuery("UPDATE _households SET status='archived' WHERE id={:id}").
			Bind(map[string]any{"id": into.Id}).Execute(); err != nil {
			t.Fatal(err)
		}
	}

	// a guard that would break: both households lost their guardians around the guards
	w = newHHWorld(t)
	app = w.f.app

	if _, err := app.NonconcurrentDB().NewQuery("UPDATE _householdMembers SET guardian=false").Execute(); err != nil {
		t.Fatal(err)
	}

	w.status(t, w.merge(t, "pat", w.h1, w.h2), 409)

	// the same login in both
	w = newHHWorld(t)
	app = w.f.app

	mCol, _ := app.FindCollectionByNameOrId(core.CollectionNameHouseholdMembers)
	dup := core.NewRecord(mCol)
	dup.Set("household", w.h1.Id)
	dup.Set("user", w.users["qed"].Id)
	dup.Set("name", "Qed again")
	dup.Set("role", "adult")
	dup.Set("status", core.HouseholdMemberActive)
	mustSave(t, app, dup)

	w.status(t, w.merge(t, "pat", w.h1, w.h2), 409)

	// nothing moved, nothing written
	if len(w.membersOf(t, w.h2)) != 2 || w.auditCount(t, core.AuditActionHouseholdMerge) != 0 {
		t.Fatal("a refused merge must change nothing")
	}
}

func TestHouseholdSplit(t *testing.T) {
	w := newHHWorld(t)
	app := w.f.app

	n1 := w.noteAbout(t, w.kid1)
	n2 := w.noteAbout(t, w.kid2)

	split := func(user string, h *core.Record, ids []string, name string) (*core.Record, *core.Record, error) {
		return core.SplitHousehold(app, w.ctx, w.actor(t, h, user), w.P, h, ids, name)
	}

	// who may
	for _, u := range []string{"ned", "yan", "ann"} {
		if _, _, err := split(u, w.h1, []string{w.nedM.Id, w.kid1.Id}, "Neds"); err == nil {
			t.Fatalf("%s must not split", u)
		}
	}

	_, _, err := split("ned", w.h1, []string{w.nedM.Id, w.kid1.Id}, "Neds")
	w.status(t, err, 403)

	// both sides keep a guardian: the minor alone, and both guardians away
	_, _, err = split("mix", w.h1, []string{w.kid1.Id}, "Kid")
	w.status(t, err, 409)
	_, _, err = split("mix", w.h1, []string{w.mixM.Id, w.nedM.Id}, "Guardians")
	w.status(t, err, 409)

	// the other arguments
	_, _, err = split("mix", w.h1, nil, "X")
	w.status(t, err, 400)
	_, _, err = split("mix", w.h1, []string{w.nedM.Id}, " ")
	w.status(t, err, 400)
	_, _, err = split("mix", w.h1, []string{w.qedM.Id}, "Foreign")
	w.status(t, err, 400)
	_, _, err = split("mix", w.h1, []string{w.mixM.Id, w.nedM.Id, w.yanM.Id, w.kid1.Id}, "All")
	w.status(t, err, 400)

	if w.auditCount(t, core.AuditActionHouseholdSplit) != 0 || len(w.membersOf(t, w.h1)) != 4 {
		t.Fatal("a refused split must change nothing")
	}

	// the head moves a guardian with the child
	from, to, err := split("mix", w.h1, []string{w.nedM.Id, w.kid1.Id}, "  Ned family ")
	w.status(t, err, 0)

	if from.Id != w.h1.Id || to.Id == w.h1.Id || to.GetString("org") != w.P.Id ||
		to.GetString("status") != core.HouseholdStatusActive || to.GetString("name") != "Ned family" {
		t.Fatalf("unexpected households: %v %v", from.PublicExport(), to.PublicExport())
	}

	if got := w.membersOf(t, to); !slices.Equal(got, sorted(w.nedM.Id, w.kid1.Id)) {
		t.Fatalf("new household: %v", got)
	}

	if got := w.membersOf(t, w.h1); !slices.Equal(got, sorted(w.mixM.Id, w.yanM.Id)) {
		t.Fatalf("old household: %v", got)
	}

	if w.reload(t, n1).GetString("household") != to.Id || w.reload(t, n2).GetString("household") != w.h2.Id {
		t.Fatal("the copy must follow the moved person only")
	}

	if w.auditCount(t, core.AuditActionHouseholdSplit) != 1 {
		t.Fatal("one split entry expected")
	}

	entries, _ := app.FindRecordsByFilter(core.CollectionNameAuditLog, "action={:a}", "", 0, 0, map[string]any{"a": core.AuditActionHouseholdSplit})
	if raw := entries[0].GetString("meta"); !strings.Contains(raw, w.h1.Id) || !strings.Contains(raw, w.kid1.Id) || strings.Contains(raw, "Kid One") ||
		entries[0].GetString("targetId") != to.Id {
		t.Fatalf("meta: %s", raw)
	}

	// staff may split too (and a non-head guardian of a household with no head may not)
	_, _, err = split("pat", w.h2, []string{w.kid2.Id}, "Lone")
	w.status(t, err, 409)

	// an archived household cannot be split
	if _, err := app.NonconcurrentDB().NewQuery("UPDATE _households SET status='archived' WHERE id={:id}").Bind(map[string]any{"id": w.h2.Id}).Execute(); err != nil {
		t.Fatal(err)
	}

	_, _, err = split("pat", w.h2, []string{w.kid2.Id}, "Lone")
	w.status(t, err, 409)
}

func sorted(ids ...string) []string {
	slices.Sort(ids)

	return ids
}

func TestHouseholdSplitByStaff(t *testing.T) {
	w := newHHWorld(t)

	_, to, err := core.SplitHousehold(w.f.app, w.ctx, w.actor(t, w.h1, "zed"), w.P, w.h1, []string{w.yanM.Id}, "Yans")
	w.status(t, err, 0)

	if got := w.membersOf(t, to); len(got) != 1 {
		t.Fatalf("%v", got)
	}
}

func TestHouseholdDuplicates(t *testing.T) {
	w := newHHWorld(t)
	app := w.f.app

	mCol, _ := app.FindCollectionByNameOrId(core.CollectionNameHouseholdMembers)
	add := func(h *core.Record, name, birth string) *core.Record {
		m := core.NewRecord(mCol)
		m.Set("household", h.Id)
		m.Set("name", name)
		m.Set("role", "adult")
		m.Set("status", core.HouseholdMemberActive)
		m.Set("birthDate", birth)
		mustSave(t, app, m)

		return m
	}

	// same name, same birth year, other household
	a := add(w.h2, "  kid   ONE ", years(10))
	// same name inside one household, different year
	b := add(w.h1, "Yan", years(40))
	// same name, different household and year: not a candidate
	add(w.h2, "Ned", years(60))

	groups, truncated, err := core.HouseholdDuplicates(app, w.P.Id)
	if err != nil || truncated {
		t.Fatalf("%v %v", err, truncated)
	}

	if len(groups) != 2 {
		t.Fatalf("%+v", groups)
	}

	for _, g := range groups {
		ids := []string{}
		for _, m := range g.Members {
			ids = append(ids, m.Id)
		}

		switch g.Reason {
		case "same_name_birth_year":
			if !slices.Contains(ids, a.Id) || !slices.Contains(ids, w.kid1.Id) || g.Members[0].HouseholdName == "" {
				t.Fatalf("%+v", g)
			}
		case "same_name_household":
			if !slices.Contains(ids, b.Id) || !slices.Contains(ids, w.yanM.Id) {
				t.Fatalf("%+v", g)
			}
		default:
			t.Fatalf("reason %q", g.Reason)
		}
	}

	raw, _ := json.Marshal(groups)
	for _, leak := range []string{"birthDate", years(10), years(40), years(10)[:4] + "-"} {
		if strings.Contains(strings.ToLower(string(raw)), strings.ToLower(leak)) {
			t.Fatalf("the list leaks %q: %s", leak, raw)
		}
	}

	if core.NormalizeHouseholdName("  Mary-Ann  O'Neil ") != "maryann oneil" {
		t.Fatal("normalize")
	}

	// another org sees nothing
	if g, _, _ := core.HouseholdDuplicates(app, "otherorg"); len(g) != 0 {
		t.Fatal("scoped to the org")
	}
}

func TestRepairHouseholdCopies(t *testing.T) {
	w := newHHWorld(t)
	app := w.f.app

	n := w.noteAbout(t, w.kid1)
	w.noteAbout(t, w.kid2)

	if _, err := app.NonconcurrentDB().NewQuery("UPDATE hh_notes SET household={:h} WHERE id={:id}").
		Bind(map[string]any{"h": w.h2.Id, "id": n.Id}).Execute(); err != nil {
		t.Fatal(err)
	}

	rows, err := core.RepairHouseholdCopies(app, true)
	if err != nil || len(rows) != 1 || rows[0].Collection != "hh_notes" || rows[0].Drifted != 1 {
		t.Fatalf("%+v %v", rows, err)
	}

	if w.reload(t, n).GetString("household") != w.h2.Id {
		t.Fatal("a dry run must change nothing")
	}

	if _, err := core.RepairHouseholdCopies(app, false); err != nil {
		t.Fatal(err)
	}

	if w.reload(t, n).GetString("household") != w.h1.Id {
		t.Fatal("not repaired")
	}

	if rows, _ := core.RepairHouseholdCopies(app, false); rows[0].Drifted != 0 {
		t.Fatal("not idempotent")
	}
}

func tinyPNG(t testing.TB, name string) *filesystem.File {
	t.Helper()

	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}

	f, err := filesystem.NewFileFromBytes(buf.Bytes(), name)
	if err != nil {
		t.Fatal(err)
	}

	return f
}

func TestHouseholdMemberPhoto(t *testing.T) {
	w := newHHWorld(t)
	app := w.f.app

	set := func(user string, h, m *core.Record, f *filesystem.File) error {
		_, err := core.SetHouseholdMemberPhoto(app, w.ctx, w.actor(t, h, user), w.P, h, w.reload(t, m), f)

		return err
	}

	// the permission matrix (the PATCH member rules): staff, a guardian, the person
	w.status(t, set("pat", w.h1, w.kid1, tinyPNG(t, "a.png")), 0)
	w.status(t, set("zed", w.h1, w.kid1, tinyPNG(t, "a.png")), 0)
	w.status(t, set("ned", w.h1, w.kid1, tinyPNG(t, "a.png")), 0)
	w.status(t, set("ned", w.h1, w.yanM, tinyPNG(t, "a.png")), 0) // any member
	w.status(t, set("yan", w.h1, w.yanM, tinyPNG(t, "a.png")), 0) // themselves
	w.status(t, set("yan", w.h1, w.kid1, tinyPNG(t, "a.png")), 403)
	w.status(t, set("yan", w.h1, w.mixM, tinyPNG(t, "a.png")), 403)
	w.status(t, set("qed", w.h1, w.kid1, tinyPNG(t, "a.png")), 403) // another household's guardian
	w.status(t, set("ann", w.h1, w.kid1, tinyPNG(t, "a.png")), 403)

	if w.reload(t, w.kid1).GetString("photo") == "" {
		t.Fatal("no photo stored")
	}

	// MIME and size
	txt, _ := filesystem.NewFileFromBytes([]byte("hello, not an image"), "p.png")
	if err := set("pat", w.h1, w.kid1, txt); err == nil {
		t.Fatal("text accepted as a photo")
	}

	gif, _ := filesystem.NewFileFromBytes([]byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"), "p.gif")
	if err := set("pat", w.h1, w.kid1, gif); err == nil {
		t.Fatal("gif accepted as a photo")
	}

	var big bytes.Buffer
	_ = png.Encode(&big, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	big.Write(make([]byte, 5<<20))

	bf, _ := filesystem.NewFileFromBytes(big.Bytes(), "big.png")
	if err := set("pat", w.h1, w.kid1, bf); err == nil {
		t.Fatal("an oversize photo accepted")
	}

	// remove
	if _, err := core.RemoveHouseholdMemberPhoto(app, w.ctx, w.actor(t, w.h1, "yan"), w.P, w.h1, w.reload(t, w.kid1)); err == nil {
		t.Fatal("yan must not remove kid1's photo")
	}

	if _, err := core.RemoveHouseholdMemberPhoto(app, w.ctx, w.actor(t, w.h1, "ned"), w.P, w.h1, w.reload(t, w.kid1)); err != nil {
		t.Fatal(err)
	}

	if w.reload(t, w.kid1).GetString("photo") != "" {
		t.Fatal("photo not removed")
	}

	// the member of another household in the path
	w.status(t, set("pat", w.h1, w.kid2, tinyPNG(t, "a.png")), 404)
}
