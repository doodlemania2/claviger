package core_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
	"github.com/doodlemania2/claviger/tools/search"
)

// Fork-local (Claviger #84): the org closure table, its hooks, the repair and
// the ancestor rules (the checks of docs/specs/80-88-congregations.md 2.1).

type closureFixture struct {
	app  *tests.TestApp
	orgs *core.Collection
}

func newClosureFixture(t testing.TB) *closureFixture {
	t.Helper()

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)

	orgs, err := app.FindCollectionByNameOrId(core.CollectionNameOrgs)
	if err != nil {
		t.Fatal(err)
	}

	return &closureFixture{app: app, orgs: orgs}
}

func (f *closureFixture) org(t testing.TB, slug, parent string) *core.Record {
	t.Helper()

	r := core.NewRecord(f.orgs)
	r.Set("name", strings.ToUpper(slug))
	r.Set("slug", slug)
	r.Set("parent", parent)
	if err := f.app.Save(r); err != nil {
		t.Fatalf("org %s: %v", slug, err)
	}

	return r
}

// closure returns "ancestorSlug>descendantSlug:depth" for every row, sorted.
func (f *closureFixture) closure(t testing.TB) []string {
	t.Helper()

	slugs := map[string]string{}
	recs, err := f.app.FindAllRecords(core.CollectionNameOrgs)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		slugs[r.Id] = r.GetString("slug")
	}

	rows, err := f.app.FindAllRecords(core.CollectionNameOrgClosure)
	if err != nil {
		t.Fatal(err)
	}

	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, fmt.Sprintf("%s>%s:%d", slugs[r.GetString("ancestor")], slugs[r.GetString("descendant")], r.GetInt("depth")))
	}
	slices.Sort(out)

	return out
}

func wantRows(t testing.TB, got []string, want ...string) {
	t.Helper()

	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("closure rows differ\n got: %v\nwant: %v", got, want)
	}
}

func TestOrgClosureCollection(t *testing.T) {
	f := newClosureFixture(t)

	col, err := f.app.FindCollectionByNameOrId(core.CollectionNameOrgClosure)
	if err != nil {
		t.Fatal(err)
	}

	if !col.System || col.CreateRule != nil || col.UpdateRule != nil || col.DeleteRule != nil {
		t.Fatalf("expected a system collection with null write rules, got system=%v", col.System)
	}

	if !slices.Contains(core.APIKeyForbiddenCollections, core.CollectionNameOrgClosure) {
		t.Fatal("expected the closure table on the API key forbidden list")
	}
}

func TestOrgClosureCreateAndDelete(t *testing.T) {
	f := newClosureFixture(t)

	d := f.org(t, "dd", "")
	n := f.org(t, "nn", d.Id)
	p := f.org(t, "pp", n.Id)
	f.org(t, "qq", n.Id)

	wantRows(t, f.closure(t),
		"dd>dd:0", "nn>nn:0", "pp>pp:0", "qq>qq:0",
		"dd>nn:1", "dd>pp:2", "dd>qq:2", "nn>pp:1", "nn>qq:1")

	// a parent with children cannot be deleted; the leaf can, with its rows
	if err := f.app.Delete(n); err == nil {
		t.Fatal("expected the delete of a parent to be refused")
	}
	wantRows(t, f.closure(t),
		"dd>dd:0", "nn>nn:0", "pp>pp:0", "qq>qq:0",
		"dd>nn:1", "dd>pp:2", "dd>qq:2", "nn>pp:1", "nn>qq:1")

	if err := f.app.Delete(p); err != nil {
		t.Fatal(err)
	}
	wantRows(t, f.closure(t),
		"dd>dd:0", "nn>nn:0", "qq>qq:0", "dd>nn:1", "dd>qq:2", "nn>qq:1")

	// no per-row audit entries, no extra entries for the maintenance
	entries, _ := f.app.FindAllRecords(core.CollectionNameAuditLog, dbx.HashExp{"targetCollection": core.CollectionNameOrgClosure})
	if len(entries) != 0 {
		t.Fatalf("expected no audit entries for the closure table, got %d", len(entries))
	}
}

func TestOrgClosureReparentSubtree(t *testing.T) {
	f := newClosureFixture(t)

	d := f.org(t, "dd", "")
	n := f.org(t, "nn", d.Id)
	p := f.org(t, "pp", n.Id)
	f.org(t, "yy", p.Id)
	other := f.org(t, "oo", "")

	// move pp (with yy below it) from nn to oo
	p.Set("parent", other.Id)
	if err := f.app.Save(p); err != nil {
		t.Fatal(err)
	}

	wantRows(t, f.closure(t),
		"dd>dd:0", "nn>nn:0", "pp>pp:0", "yy>yy:0", "oo>oo:0",
		"dd>nn:1", "oo>pp:1", "oo>yy:2", "pp>yy:1")

	// one audit entry with counts
	entry, err := f.app.FindFirstRecordByFilter(core.CollectionNameAuditLog, "action='"+core.AuditActionOrgReparent+"'")
	if err != nil {
		t.Fatalf("expected an org_reparent audit entry: %v", err)
	}
	var meta map[string]any
	if err := entry.UnmarshalJSONField("meta", &meta); err != nil {
		t.Fatal(err)
	}
	if meta["closureRemoved"].(float64) != 4 || meta["closureAdded"].(float64) != 2 {
		t.Fatalf("unexpected counts: %v", meta)
	}

	// to the root, then back below dd
	p.Set("parent", "")
	if err := f.app.Save(p); err != nil {
		t.Fatal(err)
	}
	wantRows(t, f.closure(t),
		"dd>dd:0", "nn>nn:0", "pp>pp:0", "yy>yy:0", "oo>oo:0", "dd>nn:1", "pp>yy:1")

	p.Set("parent", d.Id)
	if err := f.app.Save(p); err != nil {
		t.Fatal(err)
	}
	wantRows(t, f.closure(t),
		"dd>dd:0", "nn>nn:0", "pp>pp:0", "yy>yy:0", "oo>oo:0",
		"dd>nn:1", "dd>pp:1", "dd>yy:2", "pp>yy:1")

	// a rename does not touch the table
	p.Set("name", "Renamed")
	if err := f.app.Save(p); err != nil {
		t.Fatal(err)
	}
	if got := len(f.closure(t)); got != 9 {
		t.Fatalf("expected 9 rows, got %d", got)
	}
}

func TestOrgClosureCycles(t *testing.T) {
	f := newClosureFixture(t)

	a := f.org(t, "aa", "")
	b := f.org(t, "bb", a.Id)
	c := f.org(t, "cc", b.Id)
	before := f.closure(t)

	for name, mutate := range map[string]func(){
		"self":       func() { a.Set("parent", a.Id) },
		"child":      func() { a.Set("parent", b.Id) },
		"grandchild": func() { a.Set("parent", c.Id) },
	} {
		a, _ = f.app.FindRecordById(core.CollectionNameOrgs, a.Id)
		mutate()
		wantOrgErr(t, f.app.Save(a), 400)

		if name == "" || !slices.Equal(before, f.closure(t)) {
			t.Fatalf("%s: the failed save changed the table", name)
		}
	}
}

func TestOrgClosureDepthCap(t *testing.T) {
	f := newClosureFixture(t)

	// default: four levels
	if core.OrgMaxDepth() != 4 {
		t.Fatalf("expected the default cap of 4, got %d", core.OrgMaxDepth())
	}

	l1 := f.org(t, "l1", "")
	l2 := f.org(t, "l2", l1.Id)
	l3 := f.org(t, "l3", l2.Id)
	l4 := f.org(t, "l4", l3.Id)

	tooDeep := core.NewRecord(f.orgs)
	tooDeep.Set("name", "L5")
	tooDeep.Set("slug", "l5")
	tooDeep.Set("parent", l4.Id)
	wantOrgErr(t, f.app.Save(tooDeep), 400)

	// moving a whole subtree counts its height: l2 (3 levels with l3, l4) under
	// a new root chain of two would make five
	r1 := f.org(t, "r1", "")
	r2 := f.org(t, "r2", r1.Id)
	l2.Set("parent", r2.Id)
	wantOrgErr(t, f.app.Save(l2), 400)

	r1b := f.org(t, "r1b", "")
	l2.Set("parent", r1b.Id) // 1 + 3 = four levels
	if err := f.app.Save(l2); err != nil {
		t.Fatal(err)
	}

	// raised via the environment, and clamped to the hard cap
	t.Setenv(core.OrgMaxDepthEnv, "5")
	if core.OrgMaxDepth() != 5 {
		t.Fatal("expected 5")
	}

	l2, _ = f.app.FindRecordById(core.CollectionNameOrgs, l2.Id)
	tooDeep = core.NewRecord(f.orgs)
	tooDeep.Set("name", "L5")
	tooDeep.Set("slug", "l5")
	tooDeep.Set("parent", l4.Id)
	if err := f.app.Save(tooDeep); err != nil {
		t.Fatal(err)
	}

	for in, want := range map[string]int{"99": 8, "8": 8, "0": 4, "x": 4, "": 4, "1": 1} {
		t.Setenv(core.OrgMaxDepthEnv, in)
		if got := core.OrgMaxDepth(); got != want {
			t.Fatalf("%q: expected %d, got %d", in, want, got)
		}
	}

	// lowering the cap later does not block unrelated edits of a deep org
	t.Setenv(core.OrgMaxDepthEnv, "2")
	tooDeep, _ = f.app.FindRecordById(core.CollectionNameOrgs, tooDeep.Id)
	tooDeep.Set("name", "Renamed")
	if err := f.app.Save(tooDeep); err != nil {
		t.Fatal(err)
	}
}

func TestOrgClosureRepair(t *testing.T) {
	f := newClosureFixture(t)

	d := f.org(t, "dd", "")
	n := f.org(t, "nn", d.Id)
	p := f.org(t, "pp", n.Id)
	f.org(t, "qq", n.Id)
	_ = p

	good := f.closure(t)

	rep, err := core.RepairOrgClosure(f.app, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Differences() != 0 || rep.Applied || rep.Expected != len(good) {
		t.Fatalf("expected a clean table, got %+v", rep)
	}

	db := f.app.NonconcurrentDB()
	table := "{{" + core.CollectionNameOrgClosure + "}}"
	exec := func(sql string, params dbx.Params) {
		t.Helper()
		if _, err := db.NewQuery(sql).Bind(params).Execute(); err != nil {
			t.Fatal(err)
		}
	}

	// damage: a missing row, a wrong depth, an extra row
	exec("DELETE FROM "+table+" WHERE [[ancestor]]={:a} AND [[descendant]]={:d}", dbx.Params{"a": d.Id, "d": p.Id})
	exec("UPDATE "+table+" SET [[depth]]=7 WHERE [[ancestor]]={:a} AND [[descendant]]={:d}", dbx.Params{"a": n.Id, "d": p.Id})
	exec("INSERT INTO "+table+" ([[id]], [[ancestor]], [[descendant]], [[depth]]) VALUES ({:id}, {:a}, {:d}, 1)",
		dbx.Params{"id": "extrarow0000001", "a": p.Id, "d": d.Id})

	dry, err := core.RepairOrgClosure(f.app, true)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Missing != 1 || dry.WrongDepth != 1 || dry.Extra != 1 || dry.Applied {
		t.Fatalf("unexpected dry run: %+v", dry)
	}
	if slices.Equal(f.closure(t), good) {
		t.Fatal("a dry run must not repair")
	}

	rep, err = core.RepairOrgClosure(f.app, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Missing != 1 || rep.WrongDepth != 1 || rep.Extra != 1 || !rep.Applied {
		t.Fatalf("unexpected repair: %+v", rep)
	}
	wantRows(t, f.closure(t), good...)

	// idempotent, and wiping the table rebuilds it
	exec("DELETE FROM "+table, nil)
	rep, _ = core.RepairOrgClosure(f.app, false)
	if rep.Missing != len(good) {
		t.Fatalf("expected %d rebuilt rows, got %+v", len(good), rep)
	}
	wantRows(t, f.closure(t), good...)

	rep, _ = core.RepairOrgClosure(f.app, false)
	if rep.Differences() != 0 {
		t.Fatalf("expected a second run to be clean, got %+v", rep)
	}
}

// ---- rules ------------------------------------------------------------------

// ancestorWorld is the tree of the spec's prototype: D (diocese), N (deanery)
// below D, P and Q (parishes) below N, Y (ministry) below P.
type ancestorWorld struct {
	f     *closureFixture
	users map[string]*core.Record
	docs  map[string]*core.Record // one record per org
	col   *core.Collection
}

func newAncestorWorld(t testing.TB) *ancestorWorld {
	t.Helper()

	f := newClosureFixture(t)
	w := &ancestorWorld{f: f, users: map[string]*core.Record{}, docs: map[string]*core.Record{}}

	d := f.org(t, "dd", "")
	n := f.org(t, "nn", d.Id)
	p := f.org(t, "pp", n.Id)
	q := f.org(t, "qq", n.Id)
	y := f.org(t, "yy", p.Id)
	orgs := map[string]*core.Record{"D": d, "N": n, "P": p, "Q": q, "Y": y}

	usersCol, _ := f.app.FindCollectionByNameOrId("users")
	for _, name := range []string{"ann", "ned", "pat", "yan", "qed", "gwen", "mix", "zed"} {
		u := core.NewRecord(usersCol)
		u.SetEmail(name + "@closure.test")
		u.SetPassword("password123")
		if err := f.app.Save(u); err != nil {
			t.Fatal(err)
		}
		w.users[name] = u
	}

	rolesCol, _ := f.app.FindCollectionByNameOrId(core.CollectionNameOrgRoles)
	tag := core.NewRecord(rolesCol)
	tag.Set("org", d.Id)
	tag.Set("name", "safeguarding")
	if err := f.app.Save(tag); err != nil {
		t.Fatal(err)
	}

	membersCol, _ := f.app.FindCollectionByNameOrId(core.CollectionNameOrgMembers)
	add := func(org, user, role string, tags ...string) {
		m := core.NewRecord(membersCol)
		m.Set("org", orgs[org].Id)
		m.Set("user", w.users[user].Id)
		m.Set("role", role)
		m.Set("status", "active")
		m.Set("tags", tags)
		if err := f.app.Save(m); err != nil {
			t.Fatal(err)
		}
	}
	add("D", "ann", "admin")
	add("N", "ned", "member")
	add("P", "pat", "admin")
	add("Y", "yan", "member")
	add("Q", "qed", "admin")
	add("D", "gwen", "member", tag.Id)
	add("P", "mix", "member")
	add("D", "mix", "admin")

	w.col = core.NewBaseCollection("closure_docs")
	w.col.Fields.Add(
		&core.TextField{Name: "title"},
		&core.RelationField{Name: "org", CollectionId: f.orgs.Id, MaxSelect: 1, Required: true},
	)
	if err := f.app.Save(w.col); err != nil {
		t.Fatal(err)
	}

	for k, o := range orgs {
		doc := core.NewRecord(w.col)
		doc.Set("title", k)
		doc.Set("org", o.Id)
		if err := f.app.Save(doc); err != nil {
			t.Fatal(err)
		}
		w.docs[k] = doc
	}

	return w
}

// who returns the sorted names of the users that the rule lets see the doc of
// the given org, through CanAccessRecord and through a list query.
func (w *ancestorWorld) who(t testing.TB, rule string, docOrg string) string {
	t.Helper()

	var names []string
	for name, u := range w.users {
		ok, err := w.f.app.CanAccessRecord(w.docs[docOrg], &core.RequestInfo{Auth: u, Context: core.RequestInfoContextDefault}, &rule)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)

	return strings.Join(names, ",")
}

func TestOrgAncestorRules(t *testing.T) {
	w := newAncestorWorld(t)

	rule := func(opts core.OrgAncestorOpts) string {
		r, err := core.OrgAncestorRule("org", opts)
		if err != nil {
			t.Fatal(err)
		}

		return r
	}

	cases := []struct {
		name  string
		rule  string
		doc   string
		wants string
	}{
		{"base, doc in Y", rule(core.OrgAncestorOpts{}), "Y", "ann,gwen,mix,ned,pat,yan"},
		{"base, doc in Q: sibling branch excluded", rule(core.OrgAncestorOpts{}), "Q", "ann,gwen,mix,ned,qed"},
		{"base, doc in D: nobody sees upward", rule(core.OrgAncestorOpts{}), "D", "ann,gwen,mix"},
		{"admin within two levels, doc in Y", rule(core.OrgAncestorOpts{Roles: []string{"admin"}, MaxDepth: 2}), "Y", "pat"},
		{"admin at any depth, doc in Y", rule(core.OrgAncestorOpts{Roles: []string{"admin"}}), "Y", "ann,mix,pat"},
		{"tag at an ancestor, doc in Y", rule(core.OrgAncestorOpts{Tag: "safeguarding"}), "Y", "gwen"},
		{"org-only rule has no oversight", core.OrgMemberRule("org"), "Y", "yan"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := w.who(t, c.rule, c.doc); got != c.wants {
				t.Fatalf("got %q, want %q", got, c.wants)
			}
		})
	}
}

func TestOrgAncestorRuleSet(t *testing.T) {
	w := newAncestorWorld(t)

	set, err := core.OrgOversightRuleSet("org", core.OrgAncestorOpts{})
	if err != nil {
		t.Fatal(err)
	}

	w.col.ListRule, w.col.ViewRule = set.List, set.View
	w.col.CreateRule, w.col.UpdateRule, w.col.DeleteRule = set.Create, set.Update, set.Delete
	if err := w.f.app.Save(w.col); err != nil {
		t.Fatalf("the generated rules must validate: %v", err)
	}

	for _, bad := range []core.OrgAncestorOpts{
		{Roles: []string{"root"}}, {MaxDepth: 9}, {MaxDepth: -1}, {Tag: `a"b`},
	} {
		if _, err := core.OrgAncestorRule("org", bad); err == nil {
			t.Fatalf("expected %+v to be refused", bad)
		}
	}
}

// listWithRule runs the list path of the records API (rule filter plus the
// client's query string) for the given user and returns the titles.
func listWithRule(t testing.TB, app core.App, col *core.Collection, auth *core.Record, rule, query string) ([]string, int64) {
	t.Helper()

	info := &core.RequestInfo{Auth: auth, Context: core.RequestInfoContextDefault}
	resolver := core.NewRecordFieldResolver(app, col, info, true)

	q := app.RecordQuery(col)

	expr, err := search.FilterData(rule).BuildExpr(resolver)
	if err != nil {
		t.Fatal(err)
	}
	q.AndWhere(expr)

	records := []*core.Record{}

	result, err := search.NewProvider(resolver).Query(q).ParseAndExec(query, &records)
	if err != nil {
		t.Fatal(err)
	}

	titles := make([]string, 0, len(records))
	for _, r := range records {
		titles = append(titles, r.GetString("title"))
	}
	slices.Sort(titles)

	return titles, int64(result.TotalItems)
}

func TestOrgAncestorRuleList(t *testing.T) {
	w := newAncestorWorld(t)

	rule, err := core.OrgAncestorRule("org", core.OrgAncestorOpts{})
	if err != nil {
		t.Fatal(err)
	}

	adminRule, _ := core.OrgAncestorRule("org", core.OrgAncestorOpts{Roles: []string{"admin"}, MaxDepth: 2})

	for _, c := range []struct {
		user, rule string
		want       []string
	}{
		{"ann", rule, []string{"D", "N", "P", "Q", "Y"}},
		{"ned", rule, []string{"N", "P", "Q", "Y"}},
		{"pat", rule, []string{"P", "Y"}},
		{"yan", rule, []string{"Y"}},
		{"qed", rule, []string{"Q"}},
		{"zed", rule, []string{}},
		{"pat", adminRule, []string{"P", "Y"}},
		{"ann", adminRule, []string{"D", "N", "P", "Q"}}, // within two levels of D (Y is three)
	} {
		got, total := listWithRule(t, w.f.app, w.col, w.users[c.user], c.rule, "sort=title")
		if !slices.Equal(got, c.want) || int(total) != len(c.want) {
			t.Fatalf("%s: got %v (%d), want %v", c.user, got, total, c.want)
		}
	}

	// archived orgs drop out of oversight
	y, _ := w.f.app.FindFirstRecordByData(core.CollectionNameOrgs, "slug", "yy")
	y.Set("archived", true)
	if err := w.f.app.Save(y); err != nil {
		t.Fatal(err)
	}
	if got, _ := listWithRule(t, w.f.app, w.col, w.users["ann"], rule, "sort=title"); !slices.Equal(got, []string{"D", "N", "P", "Q"}) {
		t.Fatalf("archived: got %v", got)
	}
}

func TestOrgClosureIsAtomic(t *testing.T) {
	f := newClosureFixture(t)

	d := f.org(t, "dd", "")
	before := f.closure(t)

	err := f.app.RunInTransaction(func(txApp core.App) error {
		child := core.NewRecord(f.orgs)
		child.Set("name", "Kid")
		child.Set("slug", "kid")
		child.Set("parent", d.Id)
		if err := txApp.Save(child); err != nil {
			return err
		}

		d2, err := txApp.FindRecordById(core.CollectionNameOrgs, d.Id)
		if err != nil {
			return err
		}
		d2.Set("parent", child.Id)
		wantOrgErr(t, txApp.Save(d2), 400) // a cycle, inside the transaction

		return fmt.Errorf("roll back")
	})
	if err == nil {
		t.Fatal("expected the rollback")
	}

	wantRows(t, f.closure(t), before...)
}

func TestOrgClosureRulesOfTheTable(t *testing.T) {
	w := newAncestorWorld(t)

	col, _ := w.f.app.FindCollectionByNameOrId(core.CollectionNameOrgClosure)

	rowsFor := func(user string) int {
		got, total := listWithRule(t, w.f.app, col, w.users[user], *col.ListRule, "")
		_ = got

		return int(total)
	}

	// the closure rows that touch an org the user belongs to, as ancestor or descendant
	if rowsFor("zed") != 0 {
		t.Fatal("a non-member must see nothing")
	}
	if rowsFor("yan") != 4 { // dd>yy, nn>yy, pp>yy, yy>yy
		t.Fatalf("yan sees %d rows", rowsFor("yan"))
	}
}
