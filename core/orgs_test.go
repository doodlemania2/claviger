package core_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// Fork-local (Claviger #79a): model-level guards of the organizations model.

type orgFixture struct {
	app           *tests.TestApp
	org1, org2    *core.Record
	alice, bob    *core.Record // users
	mAlice, mBob  *core.Record // alice owns org1, bob is a member of org1
	orgs, members *core.Collection
}

func newOrgFixture(t *testing.T) *orgFixture {
	t.Helper()

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)

	f := &orgFixture{app: app}
	f.orgs, _ = app.FindCollectionByNameOrId(core.CollectionNameOrgs)
	f.members, _ = app.FindCollectionByNameOrId(core.CollectionNameOrgMembers)
	users, _ := app.FindCollectionByNameOrId("users")

	mkUser := func(email string) *core.Record {
		r := core.NewRecord(users)
		r.SetEmail(email)
		r.SetPassword("password123")
		if err := app.Save(r); err != nil {
			t.Fatal(err)
		}

		return r
	}
	f.alice, f.bob = mkUser("alice@x.test"), mkUser("bob@x.test")

	mkOrg := func(name, slug string) *core.Record {
		r := core.NewRecord(f.orgs)
		r.Set("name", name)
		r.Set("slug", slug)
		if err := app.Save(r); err != nil {
			t.Fatal(err)
		}

		return r
	}
	f.org1, f.org2 = mkOrg("One", "one"), mkOrg("Two", "two")

	f.mAlice = f.addMember(t, f.org1, f.alice, "owner", "active")
	f.mBob = f.addMember(t, f.org1, f.bob, "member", "active")

	return f
}

func (f *orgFixture) addMember(t *testing.T, org, user *core.Record, role, status string) *core.Record {
	t.Helper()

	m := core.NewRecord(f.members)
	m.Set("org", org.Id)
	m.Set("user", user.Id)
	m.Set("role", role)
	m.Set("status", status)
	if err := f.app.Save(m); err != nil {
		t.Fatal(err)
	}

	return m
}

func wantOrgErr(t *testing.T, err error, status int) {
	t.Helper()

	var oe *core.OrgError
	if !errors.As(err, &oe) || oe.Status != status {
		t.Fatalf("expected an org error with status %d, got %v", status, err)
	}
}

func TestOrgLastOwnerUpdateGuard(t *testing.T) {
	f := newOrgFixture(t)

	f.mAlice.Set("role", "admin")
	wantOrgErr(t, f.app.Save(f.mAlice), 409)

	f.mAlice, _ = f.app.FindRecordById(core.CollectionNameOrgMembers, f.mAlice.Id)
	f.mAlice.Set("status", "suspended")
	wantOrgErr(t, f.app.Save(f.mAlice), 409)

	// editing other fields of the last owner is fine
	f.mAlice, _ = f.app.FindRecordById(core.CollectionNameOrgMembers, f.mAlice.Id)
	f.mAlice.Set("source", "sso")
	if err := f.app.Save(f.mAlice); err != nil {
		t.Fatal(err)
	}

	// with a second active owner the first can step down
	f.mBob.Set("role", "owner")
	if err := f.app.Save(f.mBob); err != nil {
		t.Fatal(err)
	}

	f.mAlice, _ = f.app.FindRecordById(core.CollectionNameOrgMembers, f.mAlice.Id)
	f.mAlice.Set("role", "admin")
	if err := f.app.Save(f.mAlice); err != nil {
		t.Fatal(err)
	}

	// and now bob is the last one
	f.mBob, _ = f.app.FindRecordById(core.CollectionNameOrgMembers, f.mBob.Id)
	f.mBob.Set("role", "viewer")
	wantOrgErr(t, f.app.Save(f.mBob), 409)
}

func TestOrgLastOwnerDeleteGuard(t *testing.T) {
	f := newOrgFixture(t)

	wantOrgErr(t, f.app.Delete(f.mAlice), 409)

	// deleting the user cascades to the membership and is refused too
	wantOrgErr(t, f.app.Delete(f.alice), 409)
}

func TestOrgOtherOrgsOwnersDoNotCount(t *testing.T) {
	f := newOrgFixture(t)

	f.addMember(t, f.org2, f.alice, "owner", "active") // owner elsewhere

	wantOrgErr(t, f.app.Delete(f.mAlice), 409)
}

func TestOrgDeleteBypassesTheOwnerGuard(t *testing.T) {
	f := newOrgFixture(t)

	// not archived
	wantOrgErr(t, core.DeleteOrg(f.app, context.Background(), f.org1), 409)

	f.org1.Set("archived", true)
	if err := f.app.Save(f.org1); err != nil {
		t.Fatal(err)
	}

	if err := core.DeleteOrg(f.app, context.Background(), f.org1); err != nil {
		t.Fatal(err)
	}

	if n, _ := f.app.CountRecords(core.CollectionNameOrgMembers, nil); n != 0 {
		t.Fatalf("expected the memberships to be gone, got %d", n)
	}
}

func TestOrgDirectDeleteExplainsTheRoute(t *testing.T) {
	f := newOrgFixture(t)

	// the plain model delete cascades to the last owner and is refused
	err := f.app.Delete(f.org1)
	if err == nil {
		t.Fatal("expected the delete to be refused")
	}
}

func TestOrgTagsMustBelongToTheSameOrg(t *testing.T) {
	f := newOrgFixture(t)

	roles, _ := f.app.FindCollectionByNameOrId(core.CollectionNameOrgRoles)
	tag := core.NewRecord(roles)
	tag.Set("org", f.org2.Id)
	tag.Set("name", "elsewhere")
	if err := f.app.Save(tag); err != nil {
		t.Fatal(err)
	}

	f.mBob.Set("tags", []string{tag.Id})
	wantOrgErr(t, f.app.Save(f.mBob), 400)

	own := core.NewRecord(roles)
	own.Set("org", f.org1.Id)
	own.Set("name", "own")
	if err := f.app.Save(own); err != nil {
		t.Fatal(err)
	}

	f.mBob, _ = f.app.FindRecordById(core.CollectionNameOrgMembers, f.mBob.Id)
	f.mBob.Set("tags", []string{own.Id})
	if err := f.app.Save(f.mBob); err != nil {
		t.Fatal(err)
	}

	// duplicate names within an org are refused by the unique index
	dup := core.NewRecord(roles)
	dup.Set("org", f.org1.Id)
	dup.Set("name", "own")
	if err := f.app.Save(dup); err == nil {
		t.Fatal("expected the duplicate tag to be refused")
	}
}

func TestOrgHierarchyBasics(t *testing.T) {
	f := newOrgFixture(t)

	child := core.NewRecord(f.orgs)
	child.Set("name", "Child")
	child.Set("slug", "child")
	child.Set("parent", f.org1.Id)
	if err := f.app.Save(child); err != nil {
		t.Fatal(err)
	}

	// since #84 deeper trees are allowed (see org_closure_test.go for the cap)
	grand := core.NewRecord(f.orgs)
	grand.Set("name", "Grand")
	grand.Set("slug", "grand")
	grand.Set("parent", child.Id)
	if err := f.app.Save(grand); err != nil {
		t.Fatal(err)
	}

	// a parent with children cannot be deleted
	f.org1, _ = f.app.FindRecordById(core.CollectionNameOrgs, f.org1.Id)
	wantOrgErr(t, f.app.Delete(f.org1), 409)
}

func TestOrgSlugAndMembershipUniqueness(t *testing.T) {
	f := newOrgFixture(t)

	dup := core.NewRecord(f.orgs)
	dup.Set("name", "Dup")
	dup.Set("slug", "one")
	if err := f.app.Save(dup); err == nil {
		t.Fatal("expected the duplicate slug to be refused")
	}

	bad := core.NewRecord(f.orgs)
	bad.Set("name", "Bad")
	bad.Set("slug", "Bad Slug")
	if err := f.app.Save(bad); err == nil {
		t.Fatal("expected the invalid slug to be refused")
	}

	for _, slug := range core.OrgReservedSlugs {
		reserved := core.NewRecord(f.orgs)
		reserved.Set("name", "Reserved")
		reserved.Set("slug", slug)
		if err := f.app.Save(reserved); err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("expected the reserved slug %q to be refused, got %v", slug, err)
		}
	}

	again := core.NewRecord(f.members)
	again.Set("org", f.org1.Id)
	again.Set("user", f.bob.Id)
	again.Set("role", "viewer")
	again.Set("status", "active")
	if err := f.app.Save(again); err == nil {
		t.Fatal("expected the second membership to be refused")
	}
}

func TestOrgRuleSnippetsAreStable(t *testing.T) {
	// The strings below are quoted in docs/orgs.md. A change here must update
	// the docs (and re-run the cookbook scenarios in apis).
	if got, want := core.OrgMemberRule("org"),
		`org.archived = false && org._orgMembers_via_org.user ?= @request.auth.id && org._orgMembers_via_org.status ?= "active"`; got != want {
		t.Fatalf("member rule changed:\n%s", got)
	}

	if got, want := core.OrgMemberRule("org", "owner", "admin"),
		`org.archived = false && org._orgMembers_via_org.user ?= @request.auth.id && org._orgMembers_via_org.status ?= "active" && (org._orgMembers_via_org.role ?= "owner" || org._orgMembers_via_org.role ?= "admin")`; got != want {
		t.Fatalf("admin rule changed:\n%s", got)
	}

	if _, err := core.OrgTagRule("org", `a"b`); err == nil {
		t.Fatal("expected the tag with a quote to be refused")
	}
}

func TestOrgMembersCollectionTargetsTheConfiguredUserCollection(t *testing.T) {
	f := newOrgFixture(t)

	col, err := core.OrgUserCollection(f.app)
	if err != nil || col.Name != "users" {
		t.Fatalf("expected users, got %v %v", col, err)
	}
}
