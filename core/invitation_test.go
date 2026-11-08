package core_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #79b): invitation model and token handling.

// wantInvErr is wantOrgErr that also sees invitation errors.
func wantInvErr(t *testing.T, err error, status int) {
	t.Helper()

	if oe, ok := core.AsOrgError(err); !ok || oe.Status != status {
		t.Fatalf("expected an org error with status %d, got %v", status, err)
	}
}

func (f *orgFixture) actor(t *testing.T, org, user *core.Record) *core.OrgActor {
	t.Helper()

	return core.NewOrgActor(f.app, org, user)
}

func (f *orgFixture) invite(t *testing.T, email, role string, tags ...string) (*core.Record, string) {
	t.Helper()

	inv, token, err := core.CreateInvitation(f.app, context.Background(), f.actor(t, f.org1, f.alice), f.org1,
		core.InvitationInput{Email: email, Role: role, Tags: tags})
	if err != nil {
		t.Fatal(err)
	}

	return inv, token
}

func (f *orgFixture) newUser(t *testing.T, email string, verified bool) *core.Record {
	t.Helper()

	users, _ := f.app.FindCollectionByNameOrId("users")
	r := core.NewRecord(users)
	r.SetEmail(email)
	r.SetPassword("password123")
	r.SetVerified(verified)

	if err := f.app.Save(r); err != nil {
		t.Fatal(err)
	}

	return r
}

func (f *orgFixture) age(t *testing.T, inv *core.Record, field string, d time.Duration) {
	t.Helper()

	inv.Set(field, time.Now().Add(-d))
	if err := f.app.Save(inv); err != nil {
		t.Fatal(err)
	}
}

func TestInvitationCollection(t *testing.T) {
	f := newOrgFixture(t)

	col, err := f.app.FindCollectionByNameOrId(core.CollectionNameInvitations)
	if err != nil {
		t.Fatal(err)
	}

	if !col.System || col.ListRule != nil || col.ViewRule != nil || col.CreateRule != nil || col.UpdateRule != nil || col.DeleteRule != nil {
		t.Fatal("_invitations must be a system collection with no API rules")
	}

	if !col.Fields.GetByName("tokenHash").GetHidden() {
		t.Fatal("tokenHash must be hidden")
	}

	var found bool
	for _, j := range f.app.Cron().Jobs() {
		if j.Id() == "__pbInvitationCleanup__" {
			found = true
		}
	}
	if !found {
		t.Fatal("the invitation cleanup cron is not registered")
	}
}

func TestInvitationCreateGuards(t *testing.T) {
	f := newOrgFixture(t)
	ctx := context.Background()
	carol := f.newUser(t, "carol@x.test", true)
	f.addMember(t, f.org1, carol, "admin", "active")

	// a plain member cannot invite
	_, _, err := core.CreateInvitation(f.app, ctx, f.actor(t, f.org1, f.bob), f.org1, core.InvitationInput{Email: "n@x.test"})
	wantInvErr(t, err, 403)

	// an admin cannot invite above their own role
	_, _, err = core.CreateInvitation(f.app, ctx, f.actor(t, f.org1, carol), f.org1, core.InvitationInput{Email: "n@x.test", Role: "owner"})
	wantInvErr(t, err, 403)

	// but can invite at their own level
	if _, _, err = core.CreateInvitation(f.app, ctx, f.actor(t, f.org1, carol), f.org1, core.InvitationInput{Email: "n@x.test", Role: "admin"}); err != nil {
		t.Fatal(err)
	}

	// an outsider has no standing
	_, _, err = core.CreateInvitation(f.app, ctx, f.actor(t, f.org2, f.bob), f.org2, core.InvitationInput{Email: "o@x.test"})
	wantInvErr(t, err, 403)

	for _, bad := range []string{"", "nope", "a b@x.test", "Name <n@x.test>", "n@localhost"} {
		_, _, err = core.CreateInvitation(f.app, ctx, f.actor(t, f.org1, f.alice), f.org1, core.InvitationInput{Email: bad})
		wantInvErr(t, err, 400)
	}

	_, _, err = core.CreateInvitation(f.app, ctx, f.actor(t, f.org1, f.alice), f.org1, core.InvitationInput{Email: "z@x.test", Role: "king"})
	wantInvErr(t, err, 400)

	// duplicate pending (normalized), existing member
	_, _, err = core.CreateInvitation(f.app, ctx, f.actor(t, f.org1, f.alice), f.org1, core.InvitationInput{Email: "  N@X.test "})
	wantInvErr(t, err, 409)

	_, _, err = core.CreateInvitation(f.app, ctx, f.actor(t, f.org1, f.alice), f.org1, core.InvitationInput{Email: "BOB@x.test"})
	wantInvErr(t, err, 409)

	// the same email may be invited by another org
	if _, _, err = core.CreateInvitation(f.app, ctx, f.actor(t, f.org2, f.alice), f.org2, core.InvitationInput{Email: "n@x.test"}); err == nil {
		t.Fatal("alice has no standing in org2")
	}

	// archived orgs refuse
	f.org1.Set("archived", true)
	if err := f.app.Save(f.org1); err != nil {
		t.Fatal(err)
	}
	_, _, err = core.CreateInvitation(f.app, ctx, &core.OrgActor{Record: f.alice, Role: core.OrgRoleOwner}, f.org1, core.InvitationInput{Email: "late@x.test"})
	wantInvErr(t, err, 409)
}

func TestInvitationTokenIsHashedAndRandom(t *testing.T) {
	f := newOrgFixture(t)

	inv1, tok1 := f.invite(t, "a@x.test", "member")
	_, tok2 := f.invite(t, "b@x.test", "member")

	if tok1 == tok2 || len(tok1) < 40 {
		t.Fatalf("tokens must be long and unique: %q %q", tok1, tok2)
	}

	if inv1.GetString("tokenHash") != core.HashInvitationToken(tok1) || strings.Contains(inv1.GetString("tokenHash"), tok1) {
		t.Fatal("only the hash may be stored")
	}

	if inv1.GetString("status") != "pending" || inv1.GetInt("sendCount") != 1 || inv1.GetString("invitedBy") != f.alice.Id {
		t.Fatalf("unexpected row: %v", inv1.PublicExport())
	}

	exp := inv1.GetDateTime("expires").Time()
	if d := time.Until(exp); d < 6*24*time.Hour+23*time.Hour || d > 7*24*time.Hour+time.Minute {
		t.Fatalf("expected a 7 day expiry, got %v", d)
	}
}

func TestInvitationOrgSettings(t *testing.T) {
	f := newOrgFixture(t)

	f.org1.Set("settings", map[string]any{"invitationExpiryDays": 2, "invitationsPerHour": 2})
	if err := f.app.Save(f.org1); err != nil {
		t.Fatal(err)
	}

	inv, _ := f.invite(t, "a@x.test", "member")
	if d := time.Until(inv.GetDateTime("expires").Time()); d > 49*time.Hour || d < 47*time.Hour {
		t.Fatalf("expected a 2 day expiry, got %v", d)
	}

	f.invite(t, "b@x.test", "member")

	_, _, err := core.CreateInvitation(f.app, context.Background(), f.actor(t, f.org1, f.alice), f.org1, core.InvitationInput{Email: "c@x.test"})
	wantInvErr(t, err, 429)
}

func TestInvitationHourlyLimit(t *testing.T) {
	f := newOrgFixture(t)

	for i := 0; i < core.InvitationsPerHour; i++ {
		f.invite(t, "u"+string(rune('a'+i))+"@x.test", "member")
	}

	_, _, err := core.CreateInvitation(f.app, context.Background(), f.actor(t, f.org1, f.alice), f.org1, core.InvitationInput{Email: "over@x.test"})
	wantInvErr(t, err, 429)

	// other orgs have their own budget
	if _, _, err := core.CreateInvitation(f.app, context.Background(), &core.OrgActor{System: true}, f.org2, core.InvitationInput{Email: "ok@x.test"}); err != nil {
		t.Fatal(err)
	}
}

func TestInvitationTagsAreCreatedAndApplied(t *testing.T) {
	f := newOrgFixture(t)

	inv, tok := f.invite(t, "t@x.test", "member", "youth", "choir")
	if got := core.OrgTagNames(f.app, inv); len(got) != 2 {
		t.Fatalf("expected two tags, got %v", got)
	}

	user := f.newUser(t, "t@x.test", true)

	m, _, err := core.AcceptInvitation(f.app, context.Background(), tok, user)
	if err != nil {
		t.Fatal(err)
	}

	if got := core.OrgTagNames(f.app, m); len(got) != 2 || m.GetString("source") != core.OrgSourceInvitation || m.GetString("role") != "member" {
		t.Fatalf("unexpected membership %v %v", got, m.PublicExport())
	}
}

func TestInvitationResend(t *testing.T) {
	f := newOrgFixture(t)
	ctx := context.Background()
	inv, old := f.invite(t, "r@x.test", "member")
	alice := f.actor(t, f.org1, f.alice)

	// right after sending
	_, err := core.ResendInvitation(f.app, ctx, alice, f.org1, inv)
	wantInvErr(t, err, 429)

	f.age(t, inv, "lastSentAt", 2*time.Minute)
	f.age(t, inv, "expires", -time.Hour) // close to expiry

	fresh, _ := f.app.FindRecordById(core.CollectionNameInvitations, inv.Id)

	tok, err := core.ResendInvitation(f.app, ctx, alice, f.org1, fresh)
	if err != nil {
		t.Fatal(err)
	}

	if tok == old {
		t.Fatal("the token must rotate")
	}

	if _, _, err := core.LookupInvitation(f.app, ctx, old); err == nil {
		t.Fatal("the old token must stop working")
	}

	if _, _, err := core.LookupInvitation(f.app, ctx, tok); err != nil {
		t.Fatal(err)
	}

	if fresh.GetInt("sendCount") != 2 || time.Until(fresh.GetDateTime("expires").Time()) < 6*24*time.Hour {
		t.Fatalf("expected sendCount 2 and a renewed expiry: %v", fresh.PublicExport())
	}

	// an admin cannot resend an invitation above their own role
	own, _ := f.invite(t, "o@x.test", "owner")
	f.age(t, own, "lastSentAt", 2*time.Minute)

	admin := f.newUser(t, "adm@x.test", true)
	f.addMember(t, f.org1, admin, "admin", "active")
	_, err = core.ResendInvitation(f.app, ctx, f.actor(t, f.org1, admin), f.org1, own)
	wantInvErr(t, err, 403)
}

func TestInvitationResendExpiredAndRevoked(t *testing.T) {
	f := newOrgFixture(t)
	ctx := context.Background()
	alice := f.actor(t, f.org1, f.alice)

	inv, tok := f.invite(t, "e@x.test", "member")
	f.age(t, inv, "expires", time.Hour)
	f.age(t, inv, "lastSentAt", 2*time.Hour)

	if _, _, err := core.LookupInvitation(f.app, ctx, tok); err == nil {
		t.Fatal("an expired invitation must fail")
	}

	fresh, _ := f.app.FindRecordById(core.CollectionNameInvitations, inv.Id)
	if fresh.GetString("status") != "expired" {
		t.Fatalf("expected lazy expiry, got %s", fresh.GetString("status"))
	}

	// an expired invitation can be renewed
	if _, err := core.ResendInvitation(f.app, ctx, alice, f.org1, fresh); err != nil {
		t.Fatal(err)
	}

	if fresh.GetString("status") != "pending" {
		t.Fatal("a resend must make an expired invitation pending again")
	}

	// revoke, then neither resend nor revoke again
	if err := core.RevokeInvitation(f.app, ctx, alice, f.org1, fresh); err != nil {
		t.Fatal(err)
	}

	f.age(t, fresh, "lastSentAt", 2*time.Hour)

	_, err := core.ResendInvitation(f.app, ctx, alice, f.org1, fresh)
	wantInvErr(t, err, 409)
	wantInvErr(t, core.RevokeInvitation(f.app, ctx, alice, f.org1, fresh), 409)

	// a revoked invitation frees the email
	if _, _, err := core.CreateInvitation(f.app, ctx, alice, f.org1, core.InvitationInput{Email: "e@x.test"}); err != nil {
		t.Fatal(err)
	}
}

func TestInvitationAccept(t *testing.T) {
	f := newOrgFixture(t)
	ctx := context.Background()

	inv, tok := f.invite(t, "Joiner@X.test", "admin")
	joiner := f.newUser(t, "joiner@x.test", true)

	m, done, err := core.AcceptInvitation(f.app, ctx, tok, joiner)
	if err != nil {
		t.Fatal(err)
	}

	if m.GetString("role") != "admin" || m.GetString("user") != joiner.Id || m.GetString("org") != f.org1.Id {
		t.Fatalf("unexpected member %v", m.PublicExport())
	}

	stored, _ := f.app.FindRecordById(core.CollectionNameInvitations, inv.Id)
	if stored.GetString("status") != "accepted" || stored.GetString("acceptedBy") != joiner.Id || stored.GetDateTime("acceptedAt").IsZero() || done.Id != inv.Id {
		t.Fatalf("unexpected invitation %v", stored.PublicExport())
	}

	// single use
	_, _, err = core.AcceptInvitation(f.app, ctx, tok, joiner)
	wantInvErr(t, err, 400)

	other := f.newUser(t, "other@x.test", true)
	_, _, err = core.AcceptInvitation(f.app, ctx, tok, other)
	wantInvErr(t, err, 400)
}

func TestInvitationAcceptRefusals(t *testing.T) {
	f := newOrgFixture(t)
	ctx := context.Background()

	_, tok := f.invite(t, "who@x.test", "member")

	// different email
	stranger := f.newUser(t, "stranger@x.test", true)
	_, _, err := core.AcceptInvitation(f.app, ctx, tok, stranger)
	wantInvErr(t, err, 403)

	if core.InvitationErrorCode(err) != core.InvitationCodeMismatch {
		t.Fatalf("unexpected code %q", core.InvitationErrorCode(err))
	}

	// unverified account with the right email is never linked
	unverified := f.newUser(t, "who@x.test", false)
	_, _, err = core.AcceptInvitation(f.app, ctx, tok, unverified)
	wantInvErr(t, err, 403)

	if core.InvitationErrorCode(err) != core.InvitationCodeNotVerified {
		t.Fatalf("unexpected code %q", core.InvitationErrorCode(err))
	}

	if _, err := core.FindOrgMember(f.app, f.org1.Id, unverified.Id); err == nil {
		t.Fatal("no membership may exist after a refusal")
	}

	// and the invitation is still usable once the email is verified
	unverified.SetVerified(true)
	if err := f.app.Save(unverified); err != nil {
		t.Fatal(err)
	}

	if _, _, err := core.AcceptInvitation(f.app, ctx, tok, unverified); err != nil {
		t.Fatal(err)
	}

	// garbage and empty tokens
	for _, bad := range []string{"", "nope", strings.Repeat("x", 500)} {
		_, _, err = core.AcceptInvitation(f.app, ctx, bad, unverified)
		wantInvErr(t, err, 400)
	}
}

func TestInvitationAcceptStates(t *testing.T) {
	f := newOrgFixture(t)
	ctx := context.Background()
	alice := f.actor(t, f.org1, f.alice)

	invE, tokE := f.invite(t, "exp@x.test", "member")
	f.age(t, invE, "expires", time.Minute)

	invR, tokR := f.invite(t, "rev@x.test", "member")
	if err := core.RevokeInvitation(f.app, ctx, alice, f.org1, invR); err != nil {
		t.Fatal(err)
	}

	for email, tok := range map[string]string{"exp@x.test": tokE, "rev@x.test": tokR} {
		_, _, err := core.AcceptInvitation(f.app, ctx, tok, f.newUser(t, email, true))
		wantInvErr(t, err, 400)
	}

	// archived org
	_, tokA := f.invite(t, "arch@x.test", "member")
	f.org1.Set("archived", true)
	if err := f.app.Save(f.org1); err != nil {
		t.Fatal(err)
	}

	_, _, err := core.AcceptInvitation(f.app, ctx, tokA, f.newUser(t, "arch@x.test", true))
	wantInvErr(t, err, 400)

	// a superuser (other collection) never matches
	su, _ := f.app.FindAuthRecordByEmail(core.CollectionNameSuperusers, "test@example.com")
	if su != nil {
		_, _, err = core.AcceptInvitation(f.app, ctx, tokA, su)
		wantInvErr(t, err, 400)
	}
}

func TestInvitationAcceptWhenAlreadyMember(t *testing.T) {
	f := newOrgFixture(t)
	ctx := context.Background()

	inv, tok := f.invite(t, "dup@x.test", "member")
	user := f.newUser(t, "dup@x.test", true)
	f.addMember(t, f.org1, user, "viewer", "active")

	_, _, err := core.AcceptInvitation(f.app, ctx, tok, user)
	wantInvErr(t, err, 409)

	// the transaction rolled back: still pending
	stored, _ := f.app.FindRecordById(core.CollectionNameInvitations, inv.Id)
	if stored.GetString("status") != "pending" {
		t.Fatalf("expected pending, got %s", stored.GetString("status"))
	}
}

func TestInvitationExpiryAndCleanup(t *testing.T) {
	f := newOrgFixture(t)
	ctx := context.Background()

	stale, _ := f.invite(t, "s@x.test", "member")
	live, _ := f.invite(t, "l@x.test", "member")
	old, _ := f.invite(t, "o@x.test", "member")
	young, _ := f.invite(t, "y@x.test", "member")

	f.age(t, stale, "expires", time.Minute)

	n, err := core.ExpireInvitations(f.app, ctx)
	if err != nil || n != 1 {
		t.Fatalf("expected one invitation to expire, got %d %v", n, err)
	}

	for id, want := range map[string]string{stale.Id: "expired", live.Id: "pending"} {
		r, _ := f.app.FindRecordById(core.CollectionNameInvitations, id)
		if r.GetString("status") != want {
			t.Fatalf("%s: expected %s, got %s", id, want, r.GetString("status"))
		}
	}

	// finished invitations are deleted after 90 days, pending ones never
	alice := f.actor(t, f.org1, f.alice)
	if err := core.RevokeInvitation(f.app, ctx, alice, f.org1, old); err != nil {
		t.Fatal(err)
	}
	if err := core.RevokeInvitation(f.app, ctx, alice, f.org1, young); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{old.Id, stale.Id} {
		if _, err := f.app.DB().NewQuery("UPDATE {{" + core.CollectionNameInvitations + "}} SET [[updated]] = {:t} WHERE [[id]] = {:id}").
			Bind(dbx.Params{"t": time.Now().AddDate(0, 0, -91).UTC().Format("2006-01-02 15:04:05.000Z"), "id": id}).Execute(); err != nil {
			t.Fatal(err)
		}
	}

	n, err = core.DeleteOldInvitations(f.app, ctx)
	if err != nil || n != 2 {
		t.Fatalf("expected two deletions, got %d %v", n, err)
	}

	left, _ := f.app.FindAllRecords(core.CollectionNameInvitations)
	if len(left) != 2 {
		t.Fatalf("expected the live and the young revoked invitation to stay, got %d", len(left))
	}
}

func TestInvitationOrgDeleteCascades(t *testing.T) {
	f := newOrgFixture(t)

	f.invite(t, "c@x.test", "member")

	f.org1.Set("archived", true)
	if err := f.app.Save(f.org1); err != nil {
		t.Fatal(err)
	}

	if err := core.DeleteOrg(f.app, context.Background(), f.org1); err != nil {
		t.Fatal(err)
	}

	left, _ := f.app.FindAllRecords(core.CollectionNameInvitations)
	if len(left) != 0 {
		t.Fatalf("expected the invitations to cascade, got %d", len(left))
	}
}
