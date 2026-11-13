package core_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #84, 84-4): member transfers between orgs.
//
// World: alice owns org1, bob is a member of org1, carol is an admin of org1
// and dora a member of org2 only. org2 is the usual target.

type trWorld struct {
	*orgFixture
	carol, dora *core.Record
	ctx         context.Context
}

func newTrWorld(t *testing.T) *trWorld {
	t.Helper()

	f := newOrgFixture(t)
	users, _ := f.app.FindCollectionByNameOrId("users")

	mk := func(email string) *core.Record {
		r := core.NewRecord(users)
		r.SetEmail(email)
		r.SetPassword("password123")
		mustSave(t, f.app, r)

		return r
	}

	w := &trWorld{orgFixture: f, carol: mk("carol@x.test"), dora: mk("dora@x.test"),
		ctx: core.WithAuditContext(context.Background(), core.AuditActor{Type: core.AuditActorTypeSystem}, "")}
	f.addMember(t, f.org1, w.carol, "admin", "active")
	f.addMember(t, f.org2, w.dora, "owner", "active")

	return w
}

func (w *trWorld) actor(user *core.Record, org *core.Record) *core.OrgActor {
	return core.NewOrgActor(w.app, org, user)
}

func (w *trWorld) start(t *testing.T, by, person *core.Record, outcome string) *core.Record {
	t.Helper()

	tr, err := core.StartOrgTransfer(w.app, w.ctx, w.actor(by, w.org1), w.org1,
		core.OrgTransferInput{User: person.Id, To: w.org2.GetString("slug"), Outcome: outcome})
	if err != nil {
		t.Fatal(err)
	}

	return tr
}

func (w *trWorld) audit(t *testing.T, action string) []*core.Record {
	t.Helper()

	rows, err := w.app.FindRecordsByFilter(core.CollectionNameAuditLog, "action={:a}", "", 0, 0, dbx.Params{"a": action})
	if err != nil {
		t.Fatal(err)
	}

	return rows
}

func TestOrgTransferStartGuards(t *testing.T) {
	w := newTrWorld(t)

	start := func(by *core.Record, org *core.Record, in core.OrgTransferInput) error {
		_, err := core.StartOrgTransfer(w.app, w.ctx, w.actor(by, org), org, in)

		return err
	}

	// the member is bob; an admin and the person start
	wantOrgErr(t, start(w.dora, w.org1, core.OrgTransferInput{User: w.bob.Id, To: w.org2.Id}), 403)
	wantOrgErr(t, start(w.alice, w.org1, core.OrgTransferInput{User: w.bob.Id, To: w.org1.Id}), 400)
	wantOrgErr(t, start(w.alice, w.org1, core.OrgTransferInput{User: w.bob.Id, To: "nope"}), 404)
	wantOrgErr(t, start(w.alice, w.org1, core.OrgTransferInput{User: w.bob.Id, To: w.org2.Id, Outcome: "delete"}), 400)
	// not a member of the source
	wantOrgErr(t, start(w.alice, w.org1, core.OrgTransferInput{User: w.dora.Id, To: w.org2.Id}), 404)
	// the last owner stays
	wantOrgErr(t, start(w.alice, w.org1, core.OrgTransferInput{User: w.alice.Id, To: w.org2.Id}), 409)
	// an admin cannot move the owner above them
	wantOrgErr(t, start(w.carol, w.org1, core.OrgTransferInput{User: w.alice.Id, To: w.org2.Id}), 403)

	// already a member of the target
	w.addMember(t, w.org2, w.bob, "member", "active")
	wantOrgErr(t, start(w.alice, w.org1, core.OrgTransferInput{User: w.bob.Id, To: w.org2.Id}), 409)

	// an archived target
	org3 := core.NewRecord(w.orgs)
	org3.Set("name", "Three")
	org3.Set("slug", "three")
	mustSave(t, w.app, org3)
	org3.Set("archived", true)
	mustSave(t, w.app, org3)
	wantOrgErr(t, start(w.alice, w.org1, core.OrgTransferInput{User: w.carol.Id, To: org3.Id}), 409)

	// no duplicate pending transfer
	if err := start(w.alice, w.org1, core.OrgTransferInput{User: w.carol.Id, To: w.org2.Id}); err != nil {
		t.Fatal(err)
	}
	wantOrgErr(t, start(w.alice, w.org1, core.OrgTransferInput{User: w.carol.Id, To: w.org2.Id}), 409)
	wantOrgErr(t, start(w.bob, w.org1, core.OrgTransferInput{User: w.carol.Id, To: w.org2.Id}), 403)

	if n := len(w.audit(t, core.AuditActionOrgTransferStart)); n != 1 {
		t.Fatalf("expected one start entry, got %d", n)
	}
}

func TestOrgTransferPersonStartsOwn(t *testing.T) {
	w := newTrWorld(t)

	tr := w.start(t, w.bob, w.bob, "")
	if tr.GetString("state") != core.OrgTransferPending || tr.GetString("sourceOutcome") != core.OrgTransferSuspend {
		t.Fatalf("unexpected transfer: %v", tr.PublicExport())
	}

	// the lifetime defaults to 14 days
	left := time.Until(tr.GetDateTime("expiresAt").Time())
	if left < 13*24*time.Hour || left > 15*24*time.Hour {
		t.Fatalf("unexpected lifetime %v", left)
	}
}

func TestOrgTransferLifetimeSettings(t *testing.T) {
	w := newTrWorld(t)

	if d := core.OrgTransferLifetime(w.org1); d != 14*24*time.Hour {
		t.Fatalf("default: %v", d)
	}

	t.Setenv(core.OrgTransferDaysEnv, "30")

	if d := core.OrgTransferLifetime(w.org1); d != 30*24*time.Hour {
		t.Fatalf("env: %v", d)
	}

	w.org1.Set("settings", `{"transferDays": 3}`)

	if d := core.OrgTransferLifetime(w.org1); d != 3*24*time.Hour {
		t.Fatalf("org setting: %v", d)
	}

	w.org1.Set("settings", `{"transferDays": 400}`)

	if d := core.OrgTransferLifetime(w.org1); d != 30*24*time.Hour {
		t.Fatalf("out of range falls back: %v", d)
	}
}

func TestOrgTransferAcceptSuspends(t *testing.T) {
	w := newTrWorld(t)
	tr := w.start(t, w.alice, w.bob, "suspend")

	// an admin or owner can never accept alone, nor can a superuser
	for _, who := range []*core.Record{w.alice, w.carol, w.dora} {
		_, err := core.AcceptOrgTransfer(w.app, w.ctx, who, tr)
		wantOrgErr(t, err, 403)
	}

	su, _ := w.app.FindAuthRecordByEmail(core.CollectionNameSuperusers, "test@example.com")
	if su == nil {
		su = core.NewRecord(mustCol(t, w.app, core.CollectionNameSuperusers))
		su.SetEmail("su@x.test")
		su.SetPassword("password123")
		mustSave(t, w.app, su)
	}

	_, err := core.AcceptOrgTransfer(w.app, w.ctx, su, tr)
	wantOrgErr(t, err, 403)

	// nothing moved
	if m, err := core.FindOrgMember(w.app, w.org1.Id, w.bob.Id); err != nil || m.GetString("status") != "active" {
		t.Fatal("source membership changed before consent")
	}

	// some records of bob in org1 that stay
	cred := mustCol(t, w.app, core.CollectionNameCredentials)
	_ = cred

	res, err := core.AcceptOrgTransfer(w.app, w.ctx, w.bob, tr)
	if err != nil {
		t.Fatal(err)
	}

	if res.Transfer.GetString("state") != core.OrgTransferAccepted || res.Transfer.GetString("decidedBy") != w.bob.Id {
		t.Fatalf("unexpected transfer %v", res.Transfer.PublicExport())
	}

	src, err := core.FindOrgMember(w.app, w.org1.Id, w.bob.Id)
	if err != nil || src.GetString("status") != core.OrgStatusSuspended {
		t.Fatalf("source should be suspended: %v %v", src, err)
	}

	dst, err := core.FindOrgMember(w.app, w.org2.Id, w.bob.Id)
	if err != nil || dst.GetString("status") != core.OrgStatusActive || dst.GetString("role") != core.OrgRoleMember {
		t.Fatalf("target membership missing: %v %v", dst, err)
	}

	// exactly one transfer entry, with both orgs
	rows := w.audit(t, core.AuditActionOrgTransferAccept)
	if len(rows) != 1 {
		t.Fatalf("expected one accept entry, got %d", len(rows))
	}

	meta := rows[0].GetString("meta")
	for _, want := range []string{w.org1.Id, w.org2.Id, w.bob.Id, `"suspend"`} {
		if !strings.Contains(meta, want) {
			t.Fatalf("meta %s misses %s", meta, want)
		}
	}

	// the transfer cannot be used twice
	_, err = core.AcceptOrgTransfer(w.app, w.ctx, w.bob, tr)
	wantOrgErr(t, err, 409)
}

func TestOrgTransferAcceptRemoves(t *testing.T) {
	w := newTrWorld(t)
	tr := w.start(t, w.bob, w.bob, "remove")

	if _, err := core.AcceptOrgTransfer(w.app, w.ctx, w.bob, tr); err != nil {
		t.Fatal(err)
	}

	if _, err := core.FindOrgMember(w.app, w.org1.Id, w.bob.Id); err == nil {
		t.Fatal("the source membership should be gone")
	}

	if _, err := core.FindOrgMember(w.app, w.org2.Id, w.bob.Id); err != nil {
		t.Fatal("the target membership should exist")
	}
}

func TestOrgTransferAcceptIsAtomic(t *testing.T) {
	w := newTrWorld(t)
	tr := w.start(t, w.alice, w.bob, "remove")

	// the person becomes a member of the target in the meantime
	w.addMember(t, w.org2, w.bob, "viewer", "active")

	_, err := core.AcceptOrgTransfer(w.app, w.ctx, w.bob, tr)
	wantOrgErr(t, err, 409)

	if m, err := core.FindOrgMember(w.app, w.org1.Id, w.bob.Id); err != nil || m.GetString("status") != "active" {
		t.Fatal("the source membership must be untouched after a refused accept")
	}

	// a failure inside the transaction rolls back the target membership too:
	// the last-owner hook refuses the removal of the source owner
	w2 := newTrWorld(t)
	tr2 := w2.start(t, w2.bob, w2.bob, "remove")
	w2.mBob.Set("role", "owner")
	mustSave(t, w2.app, w2.mBob)
	w2.mAlice.Set("status", "suspended")
	mustSave(t, w2.app, w2.mAlice)

	_, err = core.AcceptOrgTransfer(w2.app, w2.ctx, w2.bob, tr2)
	wantOrgErr(t, err, 409)

	if _, err := core.FindOrgMember(w2.app, w2.org2.Id, w2.bob.Id); err == nil {
		t.Fatal("the target membership must be rolled back")
	}

	if got, _ := w2.app.FindRecordById(core.CollectionNameOrgTransfers, tr2.Id); got.GetString("state") != core.OrgTransferPending {
		t.Fatal("the transfer must stay pending")
	}
}

func TestOrgTransferDeclineAndCancel(t *testing.T) {
	w := newTrWorld(t)

	tr := w.start(t, w.alice, w.bob, "suspend")

	_, err := core.DeclineOrgTransfer(w.app, w.ctx, w.alice, tr)
	wantOrgErr(t, err, 403)

	out, err := core.DeclineOrgTransfer(w.app, w.ctx, w.bob, tr)
	if err != nil || out.GetString("state") != core.OrgTransferDeclined {
		t.Fatalf("decline: %v %v", out, err)
	}

	if len(w.audit(t, core.AuditActionOrgTransferDecline)) != 1 {
		t.Fatal("expected a decline entry")
	}

	// a declined transfer can be started again
	tr = w.start(t, w.alice, w.bob, "suspend")

	// a stranger cannot cancel, the starter and the person can
	_, err = core.CancelOrgTransfer(w.app, w.ctx, w.dora, tr)
	wantOrgErr(t, err, 403)

	out, err = core.CancelOrgTransfer(w.app, w.ctx, w.carol, tr) // an admin of the source
	if err != nil || out.GetString("state") != core.OrgTransferCancelled {
		t.Fatalf("cancel: %v %v", out, err)
	}

	_, err = core.AcceptOrgTransfer(w.app, w.ctx, w.bob, tr)
	wantOrgErr(t, err, 409)

	tr = w.start(t, w.alice, w.bob, "suspend")
	if _, err := core.CancelOrgTransfer(w.app, w.ctx, w.bob, tr); err != nil {
		t.Fatal(err)
	}
}

func TestOrgTransferExpiry(t *testing.T) {
	w := newTrWorld(t)
	tr := w.start(t, w.alice, w.bob, "suspend")

	tr.Set("expiresAt", types.NowDateTime().Add(-time.Hour))
	mustSave(t, w.app, tr)

	// a late accept is refused and closes the transfer
	_, err := core.AcceptOrgTransfer(w.app, w.ctx, w.bob, tr)
	wantOrgErr(t, err, 409)

	got, _ := w.app.FindRecordById(core.CollectionNameOrgTransfers, tr.Id)
	if got.GetString("state") != core.OrgTransferExpired {
		t.Fatalf("state %s", got.GetString("state"))
	}

	// the sweep expires the others
	tr2 := w.start(t, w.alice, w.carol, "suspend")
	tr2.Set("expiresAt", types.NowDateTime().Add(-time.Minute))
	mustSave(t, w.app, tr2)

	fresh := w.start(t, w.alice, w.bob, "suspend")

	if err := core.ExpireOrgTransfers(w.app, w.ctx, time.Now()); err != nil {
		t.Fatal(err)
	}

	for id, want := range map[string]string{tr2.Id: core.OrgTransferExpired, fresh.Id: core.OrgTransferPending} {
		r, _ := w.app.FindRecordById(core.CollectionNameOrgTransfers, id)
		if r.GetString("state") != want {
			t.Fatalf("%s: got %s want %s", id, r.GetString("state"), want)
		}
	}

	if n := len(w.audit(t, core.AuditActionOrgTransferExpire)); n != 2 {
		t.Fatalf("expected two expire entries, got %d", n)
	}
}

func TestOrgTransferStays(t *testing.T) {
	w := newTrWorld(t)

	// a collection of the data map with an org and a user relation
	ctypes := mustCol(t, w.app, core.CollectionNameCredentialTypes)
	creds := mustCol(t, w.app, core.CollectionNameCredentials)

	ct := core.NewRecord(ctypes)
	ct.Set("org", w.org1.Id)
	ct.Set("key", "safe_environment")
	ct.Set("name", "Safe environment")
	ct.Set("status", "active")
	mustSave(t, w.app, ct)

	for i := 0; i < 2; i++ {
		c := core.NewRecord(creds)
		c.Set("org", w.org1.Id)
		c.Set("user", w.bob.Id)
		c.Set("type", ct.Id)
		c.Set("issuedAt", "2030-01-01 00:00:00.000Z")
		c.Set("expires", "2031-01-01 00:00:00.000Z")
		c.Set("status", "valid")

		if err := w.app.SaveNoValidate(c); err != nil {
			t.Fatal(err)
		}
	}

	tr := w.start(t, w.bob, w.bob, "remove")

	res, err := core.AcceptOrgTransfer(w.app, w.ctx, w.bob, tr)
	if err != nil {
		t.Fatal(err)
	}

	var found int64
	for _, s := range res.Stays {
		if s.Collection == core.CollectionNameCredentials {
			found = s.Count
		}

		if s.Collection == core.CollectionNameOrgMembers || s.Collection == core.CollectionNameOrgTransfers {
			t.Fatalf("%s must not be listed", s.Collection)
		}
	}

	if found != 2 {
		t.Fatalf("expected 2 credentials to stay, got %+v", res.Stays)
	}

	// nothing stays for another person
	other, _ := core.OrgTransferStays(w.app, w.org1.Id, w.carol.Id)
	for _, s := range other {
		if s.Collection == core.CollectionNameCredentials {
			t.Fatal("carol has no credentials")
		}
	}
}

func TestOrgTransferMinorNeedsGuardian(t *testing.T) {
	w := newTrWorld(t)

	hhs := mustCol(t, w.app, core.CollectionNameHouseholds)
	people := mustCol(t, w.app, core.CollectionNameHouseholdMembers)

	h := core.NewRecord(hhs)
	h.Set("org", w.org1.Id)
	h.Set("name", "Bobs")
	h.Set("status", "active")

	if err := w.app.SaveNoValidate(h); err != nil {
		t.Fatal(err)
	}

	row := func(user *core.Record, name string, guardian, minor bool) {
		r := core.NewRecord(people)
		r.Set("household", h.Id)
		r.Set("user", user.Id)
		r.Set("name", name)
		r.Set("guardian", guardian)
		r.Set("isMinor", minor)
		r.Set("status", "active")

		// the model refuses a minor with a login; imported data could hold one
		if err := w.app.SaveNoValidate(r); err != nil {
			t.Fatal(err)
		}
	}
	row(w.bob, "Bob", false, true) // a minor with a login
	row(w.carol, "Carol", true, false)

	// the minor cannot start or decide alone
	_, err := core.StartOrgTransfer(w.app, w.ctx, w.actor(w.bob, w.org1), w.org1,
		core.OrgTransferInput{User: w.bob.Id, To: w.org2.Id})
	wantOrgErr(t, err, 403)

	// the guardian starts it (Carol is also an admin, the guardian path is what counts)
	tr := w.start(t, w.carol, w.bob, "suspend")

	_, err = core.AcceptOrgTransfer(w.app, w.ctx, w.bob, tr)
	wantOrgErr(t, err, 403)

	_, err = core.AcceptOrgTransfer(w.app, w.ctx, w.alice, tr)
	wantOrgErr(t, err, 403)

	if ids := core.GuardedUserIds(w.app, w.carol.Id); len(ids) != 1 || ids[0] != w.bob.Id {
		t.Fatalf("guarded: %v", ids)
	}

	if _, err := core.AcceptOrgTransfer(w.app, w.ctx, w.carol, tr); err != nil {
		t.Fatal(err)
	}

	if _, err := core.FindOrgMember(w.app, w.org2.Id, w.bob.Id); err != nil {
		t.Fatal("the minor should be in the target org")
	}
}

func TestOrgTransferNotice(t *testing.T) {
	w := newTrWorld(t)

	var got []*core.OrgTransferNotice

	prev := core.OrgTransferNotifier
	core.OrgTransferNotifier = func(_ core.App, n *core.OrgTransferNotice) error {
		got = append(got, n)

		return nil
	}
	t.Cleanup(func() { core.OrgTransferNotifier = prev })

	w.start(t, w.alice, w.bob, "suspend")
	w.start(t, w.carol, w.carol, "suspend") // started by the person: no notice

	if len(got) != 1 || got[0].Person.Id != w.bob.Id || len(got[0].Recipients) != 1 || got[0].Recipients[0].Id != w.bob.Id {
		t.Fatalf("unexpected notices: %+v", got)
	}

	if got[0].FromOrg.Id != w.org1.Id || got[0].ToOrg.Id != w.org2.Id {
		t.Fatal("the notice should name both orgs")
	}
}

func TestOrgTransferCollectionIsRegistered(t *testing.T) {
	w := newTrWorld(t)

	col := mustCol(t, w.app, core.CollectionNameOrgTransfers)

	for _, rule := range []*string{col.ListRule, col.ViewRule, col.CreateRule, col.UpdateRule, col.DeleteRule} {
		if rule != nil {
			t.Fatal("the transfers collection must have null rules")
		}
	}

	dm, err := w.app.FindFirstRecordByData(core.CollectionNameDataMap, "collection", core.CollectionNameOrgTransfers)
	if err != nil || dm.GetString("treatment") != core.DataMapTreatmentErase {
		t.Fatalf("data map entry: %v %v", dm, err)
	}
}

func mustCol(t *testing.T, app core.App, name string) *core.Collection {
	t.Helper()

	c, err := app.FindCollectionByNameOrId(name)
	if err != nil {
		t.Fatal(err)
	}

	return c
}

func TestOrgTransferMemberNotice(t *testing.T) {
	w := newTrWorld(t)

	hhs := mustCol(t, w.app, core.CollectionNameHouseholds)
	people := mustCol(t, w.app, core.CollectionNameHouseholdMembers)

	h := core.NewRecord(hhs)
	h.Set("org", w.org1.Id)
	h.Set("name", "Carols")
	h.Set("status", "active")

	if err := w.app.SaveNoValidate(h); err != nil {
		t.Fatal(err)
	}

	row := func(user *core.Record, name string, guardian, minor bool) *core.Record {
		r := core.NewRecord(people)
		r.Set("household", h.Id)

		if user != nil {
			r.Set("user", user.Id)
		}

		r.Set("name", name)
		r.Set("guardian", guardian)
		r.Set("isMinor", minor)
		r.Set("status", "active")

		if err := w.app.SaveNoValidate(r); err != nil {
			t.Fatal(err)
		}

		return r
	}
	row(w.carol, "Carol", true, false)
	row(w.bob, "Bob", true, false)
	kid := row(nil, "Kid", false, true)

	var got []*core.OrgTransferNotice

	prev := core.OrgTransferNotifier
	core.OrgTransferNotifier = func(_ core.App, n *core.OrgTransferNotice) error {
		got = append(got, n)

		return nil
	}
	t.Cleanup(func() { core.OrgTransferNotifier = prev })

	// an owner who is no guardian starts it: both guardians are told
	tr, err := core.StartOrgTransfer(w.app, w.ctx, w.actor(w.alice, w.org1), w.org1,
		core.OrgTransferInput{Member: kid.Id, To: w.org2.GetString("slug")})
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 || got[0].Person.Id != kid.Id || len(got[0].Recipients) != 2 {
		t.Fatalf("unexpected notices: %+v", got)
	}

	if ids := core.GuardedMemberIds(w.app, w.carol.Id); len(ids) != 1 || ids[0] != kid.Id {
		t.Fatalf("guarded members: %v", ids)
	}

	// only a guardian decides; a guardian without a household in the target cannot accept
	if _, err := core.AcceptOrgTransfer(w.app, w.ctx, w.alice, tr); err == nil {
		t.Fatal("the owner must not accept for the child")
	} else {
		wantOrgErr(t, err, 403)
	}

	_, err = core.AcceptOrgTransfer(w.app, w.ctx, w.carol, tr)
	wantOrgErr(t, err, 409)

	// the guardian starts it: the other guardian is told, the starter is not
	got = nil

	if _, err := core.CancelOrgTransfer(w.app, w.ctx, w.carol, tr); err != nil {
		t.Fatal(err)
	}

	if _, err := core.StartOrgTransfer(w.app, w.ctx, w.actor(w.carol, w.org1), w.org1,
		core.OrgTransferInput{Member: kid.Id, To: w.org2.GetString("slug")}); err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 || len(got[0].Recipients) != 1 || got[0].Recipients[0].Id != w.bob.Id {
		t.Fatalf("unexpected notices: %+v", got)
	}
}
