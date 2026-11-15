package core_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
)

// the permanent registers (#87): an erasure ends partial, with the entry kept,
// a data_subject_note annotation and restricted set (spec 10.7)
func TestErasureRegisterEntryIsRetainedAnnotatedAndRestricted(t *testing.T) {
	w := newErWorld(t)
	app := w.app()
	yan := w.users["yan"]

	cfg, err := core.CreateSacramentalRegister(app, core.SacramentalRegisterOpts{Name: "baptisms"})
	if err != nil {
		t.Fatal(err)
	}

	col, _ := app.FindCollectionByNameOrId("baptisms")

	mk := func(surname string) *core.Record {
		r := core.NewRecord(col)
		r.Set("org", w.P.Id)
		r.Set("sacrament", "baptism")
		r.Set("surname", surname)
		r.Set("givenNames", "Yan")
		r.Set("subject", yan.Id)
		mustSave(t, app, r)

		return r
	}

	final, draft := mk("Final"), mk("Draft")

	if _, err := core.FinalizeRegisterEntry(app, w.ctx, w.users["zed"], "baptisms", final.Id); err != nil {
		t.Fatal(err)
	}

	req, res := w.erase(t, yan)

	if req.GetString("state") != core.DataRequestStatePartial {
		t.Fatalf("state %s, want partial; result %+v", req.GetString("state"), res)
	}

	var kept *core.ErasureRetained

	for i := range res.Retained {
		if res.Retained[i].Collection == "baptisms" && res.Retained[i].Code == "register" {
			kept = &res.Retained[i]
		}
	}

	if kept == nil || kept.Count != 1 || kept.Reason == "" {
		t.Fatalf("the final entry must be listed as retained with a reason: %+v", res.Retained)
	}

	// the entry is still there, intact, restricted
	got := w.fresh(t, final)
	if got.GetString("surname") != "Final" || got.GetString("givenNames") != "Yan" || !got.GetBool("restricted") {
		t.Errorf("entry: %v", got.FieldsData())
	}

	notes, err := core.RegisterAnnotations(app, cfg, got)
	if err != nil {
		t.Fatal(err)
	}

	found := 0

	for _, n := range notes {
		if n.GetString("kind") == core.RegisterKindDataSubject {
			found++

			if !strings.Contains(n.GetString("reason"), req.Id) {
				t.Errorf("the note should name the request: %q", n.GetString("reason"))
			}
		}
	}

	if found != 1 {
		t.Errorf("want one data_subject_note annotation, got %d", found)
	}

	// the draft is not a final entry: kept for a registrar to decide, not annotated
	if d := w.fresh(t, draft); d.GetString("surname") != "Draft" {
		t.Error("the draft must be kept")
	}

	hasDraft := false

	for _, k := range res.Retained {
		if k.Code == "register_draft" {
			hasDraft = true
		}
	}

	if !hasDraft {
		t.Errorf("the draft should be in the retained list: %+v", res.Retained)
	}

	// the account itself is anonymized
	if !core.IsErasedAccount(w.fresh(t, yan)) {
		t.Error("the account must be anonymized")
	}

	// the person was told, with the retained list
	n := w.lastNotice("result")
	if n == nil || len(n.Result.Retained) == 0 {
		t.Fatalf("notice %+v", n)
	}
}

func TestErasureLegalHoldsSkipRows(t *testing.T) {
	t.Run("a hold on the person keeps everything the map reaches", func(t *testing.T) {
		w := newErWorld(t)
		app := w.app()
		gwen := w.users["gwen"]
		w.seedPerson(t, gwen)

		if _, err := core.PlaceLegalHold(app, w.ctx, w.su, core.LegalHoldInput{
			Scope: core.LegalHoldScopeSubject, Subject: gwen.Id, SubjectCollection: "users", Reason: "litigation", Reference: "L-9",
		}); err != nil {
			t.Fatal(err)
		}

		sessions := w.count(t, core.CollectionNameSessions, "recordRef = {:id}", dbx.Params{"id": gwen.Id})

		req, res := w.erase(t, gwen)

		if req.GetString("state") != core.DataRequestStatePartial {
			t.Fatalf("state %s err %q %+v", req.GetString("state"), req.GetString("error"), res)
		}

		// rows are kept, and the hold's reason does not leak into the result
		if n := w.count(t, core.CollectionNameSessions, "recordRef = {:id}", dbx.Params{"id": gwen.Id}); n != sessions {
			t.Errorf("sessions: %d of %d remain", n, sessions)
		}

		if core.IsErasedAccount(w.fresh(t, gwen)) {
			t.Error("a held account must not be anonymized")
		}

		if core.FindActiveBan(app, gwen.Collection().Id, gwen.Id) != nil {
			t.Error("the account must be unlocked when it was not erased")
		}

		var held int

		for _, k := range res.Retained {
			if k.Code == "legal_hold" {
				held += k.Count
			}

			if strings.Contains(k.Reason, "litigation") {
				t.Errorf("the hold's reason leaked: %q", k.Reason)
			}
		}

		if held < 2 {
			t.Errorf("held rows in the retained list: %+v", res.Retained)
		}

		if len(res.Holds) != 1 {
			t.Errorf("the hold id belongs in the result: %v", res.Holds)
		}
	})

	t.Run("a hold on one record keeps that record only", func(t *testing.T) {
		w := newErWorld(t)
		app := w.app()
		gwen := w.users["gwen"]
		w.seedPerson(t, gwen)

		ext, _ := app.FindFirstRecordByData(core.CollectionNameExternalAuths, "recordRef", gwen.Id)

		if _, err := core.PlaceLegalHold(app, w.ctx, w.su, core.LegalHoldInput{
			Scope: core.LegalHoldScopeRecord, Collection: core.CollectionNameExternalAuths, RecordId: ext.Id, Reason: "evidence",
		}); err != nil {
			t.Fatal(err)
		}

		req, res := w.erase(t, gwen)

		if req.GetString("state") != core.DataRequestStatePartial {
			t.Fatalf("state %s %+v", req.GetString("state"), res)
		}

		if _, err := app.FindRecordById(core.CollectionNameExternalAuths, ext.Id); err != nil {
			t.Error("the held record must stay")
		}

		if n := w.count(t, core.CollectionNameSessions, "recordRef = {:id}", dbx.Params{"id": gwen.Id}); n != 0 {
			t.Errorf("the other rows must go: %d sessions", n)
		}

		if !core.IsErasedAccount(w.fresh(t, gwen)) {
			t.Error("the account is not held, it must be anonymized")
		}
	})
}

func TestErasureUnlinksPastoralNotesAndKeepsThem(t *testing.T) {
	w := newErWorld(t)
	app := w.app()
	pat, gwen := w.users["pat"], w.users["gwen"]

	col, err := core.CreatePastoralNotes(app, core.PastoralNotesOpts{})
	if err != nil {
		t.Fatal(err)
	}

	note := core.NewRecord(col)
	note.Set("org", w.P.Id)
	note.Set("author", pat.Id)
	note.Set("subject", gwen.Id)
	note.Set("visibility", "clergy")
	note.Set("body", "a private matter")
	mustSave(t, app, note)

	// a hold the export would have created
	hold := w.seed(t, core.CollectionNamePastoralHolds, map[string]any{
		"collection": col.Name, "recordId": note.Id, "org": w.P.Id, "subject": gwen.Id, "state": core.PastoralHoldHeld,
	})

	if _, res := w.erase(t, gwen); res.Anonymized["pastoral notes (link removed)"] != 1 {
		t.Fatalf("result %+v", res)
	}

	got, err := app.FindRecordById(col, note.Id)
	if err != nil {
		t.Fatal("a pastoral note is kept for its author")
	}

	if got.GetString("subject") != "" || got.GetString("author") != pat.Id || got.GetString("body") != "a private matter" {
		t.Errorf("note: %v", got.FieldsData())
	}

	if _, err := app.FindRecordById(core.CollectionNamePastoralHolds, hold.Id); err == nil {
		t.Error("the hold about the person must go")
	}
}

func TestErasureScrubIsTheOnlyWayToChangeTheAuditLog(t *testing.T) {
	w := newErWorld(t)
	app := w.app()

	rows, err := app.FindRecordsByFilter(core.CollectionNameAuditLog, "", "", 1, 0)
	if err != nil || len(rows) == 0 {
		t.Fatal("no audit rows")
	}

	e := rows[0]

	// nobody updates or deletes an entry: not the app, not a hook, not a context the caller invents
	e.Set("action", "forged")

	if err := app.Save(e); err == nil {
		t.Error("an update of an entry must be refused")
	}

	e2, _ := app.FindRecordById(core.CollectionNameAuditLog, e.Id)
	e2.Set("ip", "")

	if err := app.Save(e2); err == nil {
		t.Error("an update without the scrub context must be refused")
	}

	if err := app.Delete(e2); err == nil {
		t.Error("a delete must be refused")
	}

	// the scrub changes the personal columns and nothing else
	target := w.users["gwen"]
	before := w.count(t, core.CollectionNameAuditLog, "targetId = {:id}", dbx.Params{"id": target.Id})

	n, err := core.ScrubAuditForErasure(app, w.ctx, core.AuditScrubTarget{Ids: []string{target.Id}, Needles: []string{target.Email()}})
	if err != nil || n < before {
		t.Fatalf("scrubbed %d of %d: %v", n, before, err)
	}

	// idempotent
	if n, err := core.ScrubAuditForErasure(app, w.ctx, core.AuditScrubTarget{Ids: []string{target.Id}, Needles: []string{target.Email()}}); err != nil || n != 0 {
		t.Fatalf("second scrub: %d %v", n, err)
	}

	after, _ := app.FindRecordsByFilter(core.CollectionNameAuditLog, "targetId = {:p}", "", 0, 0, dbx.Params{"p": core.ErasurePseudonym(target.Id)})
	if len(after) != before {
		t.Fatalf("%d entries under the pseudonym, want %d", len(after), before)
	}

	// entries that only mention the person lose their values too
	if n := w.count(t, core.CollectionNameAuditLog, "after ~ {:e} || before ~ {:e}", dbx.Params{"e": target.Email()}); n != 0 {
		t.Errorf("%d entries still hold the address", n)
	}

	for _, a := range after {
		if a.GetString("action") != "create" || a.GetString("targetCollection") != "users" {
			t.Errorf("action and collection must stay: %v", a.FieldsData())
		}
	}
}

func TestErasureGraceLocksThenCancelUnlocksAndErasesNothing(t *testing.T) {
	w := newErWorld(t)
	app := w.app()
	gwen := w.users["gwen"]
	w.seedPerson(t, gwen)

	days := 14
	req := w.request(t, gwen, &days)

	if req.GetString("state") != core.DataRequestStateGrace {
		t.Fatalf("state %s", req.GetString("state"))
	}

	// locked: a ban with the reason, sessions revoked at once
	ban := core.FindActiveBan(app, gwen.Collection().Id, gwen.Id)
	if ban == nil || ban.GetString("reason") != core.BanReasonPendingErasure {
		t.Fatalf("ban %v", ban)
	}

	until := req.GetDateTime("graceUntil").Time()
	if d := time.Until(until); d < 13*24*time.Hour || d > 14*24*time.Hour+time.Minute {
		t.Errorf("graceUntil %v", until)
	}

	// nothing is erased while the grace period runs, even when the job runs
	core.ProcessErasures(app)

	if n := w.count(t, core.CollectionNameSessions, "recordRef = {:id}", dbx.Params{"id": gwen.Id}); n == 0 {
		t.Fatal("nothing may be erased during the grace period")
	}

	if core.IsErasedAccount(w.fresh(t, gwen)) {
		t.Fatal("the account must not be anonymized during the grace period")
	}

	// the person was told, with the cancel link
	g := w.lastNotice("grace")
	if g == nil || g.Token == "" {
		t.Fatalf("grace notice %+v", g)
	}

	// cancel by the link
	if _, err := core.CancelErasureByToken(app, w.ctx, g.Token, "203.0.113.1"); err != nil {
		t.Fatal(err)
	}

	if core.FindActiveBan(app, gwen.Collection().Id, gwen.Id) != nil {
		t.Error("the cancel must lift the ban")
	}

	r, _ := app.FindRecordById(core.CollectionNameDataRequests, req.Id)
	if r.GetString("state") != core.DataRequestStateCancelled {
		t.Errorf("state %s", r.GetString("state"))
	}

	// the job finds nothing to run, and the link works once
	core.ProcessErasures(app)

	if core.IsErasedAccount(w.fresh(t, gwen)) || w.count(t, core.CollectionNameSessions, "recordRef = {:id}", dbx.Params{"id": gwen.Id}) == 0 {
		t.Error("a cancelled erasure must not run")
	}

	if _, err := core.CancelErasureByToken(app, w.ctx, g.Token, ""); err == nil {
		t.Error("the cancel link must not work twice")
	}

	// a person can start again
	if _, err := core.RequestErasure(app, core.ErasureRequestInput{Subject: gwen, Requester: w.su, Mode: core.ErasureModeAdmin, IdentityCheck: "id", GraceDays: &days, Ctx: w.ctx}); err != nil {
		t.Fatal(err)
	}
}

func TestErasureGraceEndsAndTheJobRuns(t *testing.T) {
	w := newErWorld(t)
	app := w.app()
	gwen := w.users["gwen"]

	days := 14
	req := w.request(t, gwen, &days)

	// the grace period ends
	_, err := app.NonconcurrentDB().Update(core.CollectionNameDataRequests,
		dbx.Params{"graceUntil": time.Now().Add(-time.Minute).UTC().Format("2006-01-02 15:04:05.000Z")},
		dbx.HashExp{"id": req.Id}).Execute()
	if err != nil {
		t.Fatal(err)
	}

	done := w.finish(t, req.Id)
	if done.GetString("state") != core.DataRequestStateDone || !core.IsErasedAccount(w.fresh(t, gwen)) {
		t.Fatalf("state %s", done.GetString("state"))
	}

	// a running request cannot be cancelled any more
	if err := core.CancelErasure(app, w.ctx, done, w.su, "admin", ""); err == nil {
		t.Error("a finished erasure cannot be cancelled")
	}
}

func TestErasureResumesAfterAnInterruptedRunWithoutDoubleWork(t *testing.T) {
	w := newErWorld(t)
	app := w.app()
	gwen := w.users["gwen"]
	w.seedPerson(t, gwen)

	core.SetErasureStepHook(func(req *core.Record, step string) error {
		if step == "map:"+core.CollectionNameSessions {
			return errors.New("simulated crash")
		}

		return nil
	})
	t.Cleanup(func() { core.SetErasureStepHook(nil) })

	req := w.request(t, gwen, zero())

	// let the first attempt fail
	deadline := time.Now().Add(10 * time.Second)

	// (the job was started by the request itself; one pass makes one attempt)
	for time.Now().Before(deadline) {
		r, _ := app.FindRecordById(core.CollectionNameDataRequests, req.Id)
		if r.GetString("state") == core.DataRequestStateQueued && strings.Contains(r.GetString("progress"), `"attempts":1`) {
			break
		}

		time.Sleep(20 * time.Millisecond)
	}

	mid, _ := app.FindRecordById(core.CollectionNameDataRequests, req.Id)
	if mid.GetString("state") != core.DataRequestStateQueued {
		t.Fatalf("an interrupted run goes back in the queue, state %s", mid.GetString("state"))
	}

	// part of the work is done and kept in the progress
	if n := w.count(t, core.CollectionNameExternalAuths, "recordRef = {:id}", dbx.Params{"id": gwen.Id}); n != 0 {
		t.Fatalf("steps before the crash must have run (%d external auths)", n)
	}

	if w.count(t, core.CollectionNameSessions, "recordRef = {:id}", dbx.Params{"id": gwen.Id}) == 0 {
		t.Fatal("the step that crashed must not have run")
	}

	// the server restarts: the hook goes away, the running row is queued again
	var started []string

	core.SetErasureStepHook(func(_ *core.Record, step string) error {
		started = append(started, step)

		return nil
	})

	done := w.finish(t, req.Id)

	if done.GetString("state") != core.DataRequestStateDone {
		t.Fatalf("state %s err %s", done.GetString("state"), done.GetString("error"))
	}

	for _, s := range started {
		if s == "init" || s == "log" || s == "lock" || s == "map:"+core.CollectionNameExternalAuths {
			t.Errorf("step %s ran twice", s)
		}
	}

	if !core.IsErasedAccount(w.fresh(t, gwen)) {
		t.Error("the account must be anonymized after the resume")
	}

	// one result, counted once
	res := erResult(t, done)
	if res.Erased[core.CollectionNameExternalAuths] != 1 || res.Erased[core.CollectionNameSessions] != 1 {
		t.Errorf("counts: %+v", res.Erased)
	}

	if own := w.count(t, core.CollectionNameAuditLog, "action = 'gdpr_erase'"); own != 1 {
		t.Errorf("gdpr_erase entries: %d", own)
	}

	if w.noticeCount("result") != 1 {
		t.Errorf("the person must be told once, was told %d times", w.noticeCount("result"))
	}
}

func TestErasureRestoreReplayErasesAgainAndIsIdempotent(t *testing.T) {
	w := newErWorld(t)
	app := w.app()
	gwen, nedM := w.users["gwen"], w.nedM
	email := gwen.Email()

	// an erased person, and an erased household row of a person without login
	_, _ = w.erase(t, gwen)

	if !core.IsErasedAccount(w.fresh(t, gwen)) {
		t.Fatal("setup")
	}

	if n, err := core.ReplayErasureLog(app); err != nil || n != 0 {
		t.Fatalf("nothing is back yet: %d %v", n, err)
	}

	// a restore of an older backup: the account is back as it was, and so is
	// a session (the erasure log is not part of the backup's age: it is newer)
	back := w.fresh(t, gwen)
	back.SetEmail(email)
	back.Set("name", "Gwen Back")
	mustSave(t, app, back)

	w.seed(t, core.CollectionNameSessions, map[string]any{"collectionRef": gwen.Collection().Id, "recordRef": gwen.Id, "ip": "203.0.113.5"})

	if core.IsErasedAccount(w.fresh(t, gwen)) {
		t.Fatal("setup: the account is back")
	}

	n, err := core.ReplayErasureLog(app)
	if err != nil || n != 1 {
		t.Fatalf("replay found %d: %v", n, err)
	}

	// the replay is a request like another: it runs once
	var replay *core.Record

	rows, _ := app.FindRecordsByFilter(core.CollectionNameDataRequests, "type = 'erase' && state = 'queued'", "", 0, 0)
	if len(rows) != 1 {
		t.Fatalf("queued replays: %d", len(rows))
	}

	replay = rows[0]

	// a second replay before the job ran does not queue a second request
	if n, _ := core.ReplayErasureLog(app); n != 1 {
		t.Logf("second look found %d (still queued)", n)
	}

	if more, _ := app.FindRecordsByFilter(core.CollectionNameDataRequests, "type = 'erase' && state = 'queued'", "", 0, 0); len(more) != 1 {
		t.Fatalf("a replay must not be queued twice: %d", len(more))
	}

	done := w.finish(t, replay.Id)
	if done.GetString("state") != core.DataRequestStateDone {
		t.Fatalf("state %s err %s", done.GetString("state"), done.GetString("error"))
	}

	got := w.fresh(t, gwen)
	if !core.IsErasedAccount(got) || got.GetString("name") != "" {
		t.Errorf("the replay must erase the person again: %q %q", got.Email(), got.GetString("name"))
	}

	if w.count(t, core.CollectionNameSessions, "recordRef = {:id}", dbx.Params{"id": gwen.Id}) != 0 {
		t.Error("the restored session must go")
	}

	// idempotent: nothing is found now, and the person is not told again
	if n, err := core.ReplayErasureLog(app); err != nil || n != 0 {
		t.Fatalf("second replay found %d: %v", n, err)
	}

	logs, _ := app.FindAllRecords(core.CollectionNameErasureLog)
	if len(logs) != 1 || logs[0].GetInt("replays") != 1 {
		t.Errorf("the log counts the replay: %d rows, replays %d", len(logs), logs[0].GetInt("replays"))
	}

	if w.noticeCount("result") != 1 {
		t.Errorf("a replay sends no notice (the person was told once): %d", w.noticeCount("result"))
	}

	_ = nedM
}

func TestErasureSelfServiceNeedsTheEmailedConfirmation(t *testing.T) {
	w := newErWorld(t)
	app := w.app()
	gwen := w.users["gwen"]

	req, err := core.RequestErasure(app, core.ErasureRequestInput{Subject: gwen, Requester: gwen, Mode: core.ErasureModeSelf, Ctx: w.ctx})
	if err != nil {
		t.Fatal(err)
	}

	if req.GetString("state") != core.DataRequestStateAwaitingConfirmation {
		t.Fatalf("state %s", req.GetString("state"))
	}

	// nothing is locked or erased before the link is used
	if core.FindActiveBan(app, gwen.Collection().Id, gwen.Id) != nil {
		t.Error("the account must not be locked before the confirmation")
	}

	core.ProcessErasures(app)

	if core.IsErasedAccount(w.fresh(t, gwen)) {
		t.Fatal("an unconfirmed request must not run")
	}

	// the link went to the person's own address
	n := w.lastNotice("confirm")
	if n == nil || n.To != gwen.Email() || n.Token == "" {
		t.Fatalf("confirm notice %+v", n)
	}

	// the token is not stored: only its hash, and it is hidden
	if strings.Contains(string(erJSON(t, req.FieldsData())), n.Token) {
		t.Error("the token must not be stored")
	}

	// a wrong token does nothing
	if _, err := core.ConfirmErasure(app, w.ctx, strings.Repeat("x", 40), ""); err == nil {
		t.Error("a wrong token must be refused")
	}

	// a second request while one is open is refused
	if _, err := core.RequestErasure(app, core.ErasureRequestInput{Subject: gwen, Requester: gwen, Mode: core.ErasureModeSelf, Ctx: w.ctx}); err == nil {
		t.Error("only one open request per person")
	}

	confirmed, err := core.ConfirmErasure(app, w.ctx, n.Token, "203.0.113.2")
	if err != nil {
		t.Fatal(err)
	}

	if confirmed.GetString("state") != core.DataRequestStateGrace {
		t.Fatalf("state %s", confirmed.GetString("state"))
	}

	if ban := core.FindActiveBan(app, gwen.Collection().Id, gwen.Id); ban == nil || ban.GetString("reason") != core.BanReasonPendingErasure {
		t.Errorf("the account is locked for the grace period: %v", ban)
	}

	// the link works once
	if _, err := core.ConfirmErasure(app, w.ctx, n.Token, ""); err == nil {
		t.Error("the confirmation link must not work twice")
	}

	// signing in with a passkey cancels it (the apis layer calls this)
	acct := w.fresh(t, gwen)
	if !core.CancelPendingErasureForAccount(app, w.ctx, acct, "") {
		t.Fatal("a passkey sign-in must cancel the erasure")
	}

	if core.FindActiveBan(app, gwen.Collection().Id, gwen.Id) != nil {
		t.Error("cancelled: the account is unlocked")
	}

	if core.CancelPendingErasureForAccount(app, w.ctx, acct, "") {
		t.Error("nothing left to cancel")
	}
}

func TestErasureConfirmationLinkExpires(t *testing.T) {
	w := newErWorld(t)
	app := w.app()
	gwen := w.users["gwen"]

	req, err := core.RequestErasure(app, core.ErasureRequestInput{Subject: gwen, Requester: gwen, Mode: core.ErasureModeSelf, Ctx: w.ctx})
	if err != nil {
		t.Fatal(err)
	}

	_, err = app.NonconcurrentDB().Update(core.CollectionNameDataRequests,
		dbx.Params{"tokenExpires": time.Now().Add(-time.Hour).UTC().Format("2006-01-02 15:04:05.000Z")},
		dbx.HashExp{"id": req.Id}).Execute()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := core.ConfirmErasure(app, w.ctx, w.lastNotice("confirm").Token, ""); err == nil {
		t.Error("an expired link must be refused")
	}

	if n, err := core.ExpireErasureConfirmations(app); err != nil || n != 1 {
		t.Fatalf("expired %d: %v", n, err)
	}

	if r, _ := app.FindRecordById(core.CollectionNameDataRequests, req.Id); r.GetString("state") != core.DataRequestStateExpired {
		t.Errorf("state %s", r.GetString("state"))
	}
}

func TestErasureConfirmationNeedsAUsableEmailAndAWorkingMailer(t *testing.T) {
	w := newErWorld(t)
	app := w.app()
	gwen := w.users["gwen"]

	// a failing mailer cancels the request: nobody can confirm what nobody was told
	core.ErasureNotifier = func(core.App, *core.ErasureNotice) error { return errors.New("smtp down") }

	_, err := core.RequestErasure(app, core.ErasureRequestInput{Subject: gwen, Requester: gwen, Mode: core.ErasureModeSelf, Ctx: w.ctx})

	var ee *core.ErasureError
	if !errors.As(err, &ee) || ee.Code != core.ErasureCodeMail {
		t.Fatalf("want a mail_failed refusal, got %v", err)
	}

	if n := w.count(t, core.CollectionNameDataRequests, "type = 'erase' && state != 'cancelled'"); n != 0 {
		t.Errorf("%d open requests remain", n)
	}

	// an account without a usable address goes to the staff path
	gwen.SetEmail("")
	gwen2 := w.users["ann"]
	gwen2.SetEmail("erased-x@invalid")

	if _, err := core.RequestErasure(app, core.ErasureRequestInput{Subject: gwen2, Requester: gwen2, Mode: core.ErasureModeSelf, Ctx: w.ctx}); err == nil {
		t.Error("an account without a usable email cannot confirm by email")
	} else if !errors.As(err, &ee) || ee.Code != core.ErasureCodeNoEmail {
		t.Errorf("want no_email, got %v", err)
	}
}

func TestErasureAdminPathRecordsTheIdentityCheckAndNeedsOne(t *testing.T) {
	w := newErWorld(t)
	app := w.app()
	gwen := w.users["gwen"]

	// no confirmation and no identity check: refused
	if _, err := core.RequestErasure(app, core.ErasureRequestInput{Subject: gwen, Requester: w.su, Mode: core.ErasureModeAdmin, Ctx: w.ctx}); err == nil {
		t.Fatal("the staff path must say how the person was identified")
	}

	// a person cannot use the admin mode
	if _, err := core.RequestErasure(app, core.ErasureRequestInput{Subject: w.users["ann"], Requester: gwen, Mode: core.ErasureModeAdmin, IdentityCheck: "x", Ctx: w.ctx}); err == nil {
		t.Fatal("only a superuser may use the admin mode")
	}

	// a person cannot ask for somebody else
	if _, err := core.RequestErasure(app, core.ErasureRequestInput{Subject: w.users["ann"], Requester: gwen, Mode: core.ErasureModeSelf, Ctx: w.ctx}); err == nil {
		t.Fatal("self mode is for the person's own account")
	}

	// a superuser account is not erased by this tool
	if _, err := core.RequestErasure(app, core.ErasureRequestInput{Subject: w.su, Requester: w.su, Mode: core.ErasureModeAdmin, IdentityCheck: "x", Ctx: w.ctx}); err == nil {
		t.Fatal("operator accounts are not erased here")
	}

	days := 3
	req := w.request(t, gwen, &days)

	var meta map[string]any
	_ = req.UnmarshalJSONField("meta", &meta)

	if meta["identityCheck"] != "checked a photo ID at the office" || meta["mode"] != "admin" {
		t.Errorf("meta %v", meta)
	}

	// graceDays outside 0..30 are refused
	big := 31
	if _, err := core.RequestErasure(app, core.ErasureRequestInput{Subject: w.users["ann"], Requester: w.su, Mode: core.ErasureModeAdmin, IdentityCheck: "x", GraceDays: &big, Ctx: w.ctx}); err == nil {
		t.Error("graceDays above 30 must be refused")
	}
}

func TestErasureGraceDaysSetting(t *testing.T) {
	w := newErWorld(t)
	app := w.app()

	if core.ErasureGraceDays(app) != 14 {
		t.Fatalf("default %d", core.ErasureGraceDays(app))
	}

	app.Settings().Erasure.GraceDays = zero()

	if core.ErasureGraceDays(app) != 0 {
		t.Fatal("0 is a real value")
	}

	d := 31
	app.Settings().Erasure.GraceDays = &d

	if err := app.Settings().Erasure.Validate(); err == nil {
		t.Error("31 must be refused")
	}

	d = 30
	if err := app.Settings().Erasure.Validate(); err != nil {
		t.Error(err)
	}
}

func TestErasureGuardianForAChildAndTheMinorCannotAskForThemselves(t *testing.T) {
	w := newErWorld(t)
	app := w.app()
	mix, qed := w.users["mix"], w.users["qed"]

	// a minor cannot self-request: the household row has no login, so there is no session to ask from;
	// asking for it as the row's own subject is refused
	if _, err := core.RequestErasure(app, core.ErasureRequestInput{Subject: w.kid1, Requester: w.kid1, Mode: core.ErasureModeSelf, Ctx: w.ctx}); err == nil {
		t.Error("a minor cannot ask for their own erasure")
	}

	// a guardian of another household cannot
	if _, err := core.RequestErasure(app, core.ErasureRequestInput{Subject: w.kid1, Requester: qed, Mode: core.ErasureModeGuardian, Ctx: w.ctx}); err == nil {
		t.Error("only a guardian of the child's household may ask")
	}

	// an adult household member is not a child
	if _, err := core.RequestErasure(app, core.ErasureRequestInput{Subject: w.nedM, Requester: mix, Mode: core.ErasureModeGuardian, Ctx: w.ctx}); err == nil {
		t.Error("a guardian cannot ask for the erasure of an adult")
	}

	// yan is an adult with a login and not a guardian of H1
	if _, err := core.RequestErasure(app, core.ErasureRequestInput{Subject: w.kid1, Requester: w.users["yan"], Mode: core.ErasureModeGuardian, Ctx: w.ctx}); err == nil {
		t.Error("an adult who is not a guardian cannot ask")
	}

	kidId := w.kid1.Id
	consentPurpose(t, w.hhWorld, map[string]any{"key": "photos", "name": "Photos", "text": "Photos."})

	if _, err := core.ConsentSelf(app, w.ctx, w.fresh(t, mix), "photos", kidId, core.ConsentActionGrant); err != nil {
		t.Fatal(err)
	}

	req, err := core.RequestErasure(app, core.ErasureRequestInput{Subject: w.kid1, Requester: mix, Mode: core.ErasureModeGuardian, Ctx: w.ctx})
	if err != nil {
		t.Fatal(err)
	}

	// the guardian is emailed, not the child; the guardian's account is not locked
	n := w.lastNotice("confirm")
	if n == nil || n.To != mix.Email() {
		t.Fatalf("confirm notice %+v", n)
	}

	if _, err := core.ConfirmErasure(app, w.ctx, n.Token, ""); err != nil {
		t.Fatal(err)
	}

	if core.IsBanned(app, mix) {
		t.Error("the guardian must not be locked out")
	}

	// the grace period ends
	_, err = app.NonconcurrentDB().Update(core.CollectionNameDataRequests,
		dbx.Params{"graceUntil": time.Now().Add(-time.Minute).UTC().Format("2006-01-02 15:04:05.000Z")},
		dbx.HashExp{"id": req.Id}).Execute()
	if err != nil {
		t.Fatal(err)
	}

	done := w.finish(t, req.Id)
	if done.GetString("state") != core.DataRequestStateDone {
		t.Fatalf("state %s err %s %+v", done.GetString("state"), done.GetString("error"), erResult(t, done))
	}

	kid := w.fresh(t, w.kid1)
	if kid.GetString("name") != "" || kid.GetString("birthDate") != "" || kid.GetString("status") != core.HouseholdMemberLeft {
		t.Errorf("the child's row must be anonymized: %v", kid.FieldsData())
	}

	// the consent given for the child is kept as evidence, under the pseudonym
	p := core.ErasurePseudonym(kidId)
	if w.count(t, core.CollectionNameConsentEvents, "subject = {:p}", dbx.Params{"p": p}) != 1 || w.count(t, core.CollectionNameConsents, "subject = {:id}", dbx.Params{"id": kidId}) != 0 {
		t.Error("the child's consent evidence must be pseudonymized")
	}

	// the guardian was told; the other members of the household are untouched
	if r := w.lastNotice("result"); r == nil || r.To != mix.Email() {
		t.Errorf("result notice %+v", r)
	}

	if w.fresh(t, w.nedM).GetString("name") == "" || w.fresh(t, w.mixM).GetString("name") == "" {
		t.Error("other members must not be touched")
	}

	// the log has the child's hash
	if w.count(t, core.CollectionNameErasureLog, "subjectHash = {:h}", dbx.Params{"h": core.ErasureSubjectHash(core.CollectionNameHouseholdMembers, kidId)}) != 1 {
		t.Error("the erasure log must have the child")
	}
}

func TestErasureRefusesTheLastGuardianAndTheLastOwner(t *testing.T) {
	w := newErWorld(t)
	app := w.app()
	qed, mix, ned := w.users["qed"], w.users["mix"], w.users["ned"]

	// qed is the only guardian of H2, which has the minor kid2
	_, err := core.RequestErasure(app, core.ErasureRequestInput{Subject: qed, Requester: w.su, Mode: core.ErasureModeAdmin, IdentityCheck: "x", GraceDays: zero(), Ctx: w.ctx})

	var ee *core.ErasureError
	if !errors.As(err, &ee) || ee.Code != core.ErasureCodeLastGuardian || len(ee.Ids) != 1 || ee.Ids[0] != w.h2.Id || ee.Status != 409 {
		t.Fatalf("want last_guardian for H2, got %v", err)
	}

	if !strings.Contains(ee.Message, "guardian") {
		t.Errorf("the reason must be clear: %q", ee.Message)
	}

	if core.IsErasedAccount(w.fresh(t, qed)) {
		t.Fatal("nothing may change")
	}

	// H1 has two guardians: one can go, then the other cannot
	_, res := w.erase(t, ned)
	if !core.IsErasedAccount(w.fresh(t, ned)) {
		t.Fatalf("ned must be erased: %+v", res)
	}

	if _, err := core.RequestErasure(app, core.ErasureRequestInput{Subject: mix, Requester: w.su, Mode: core.ErasureModeAdmin, IdentityCheck: "x", GraceDays: zero(), Ctx: w.ctx}); !errors.As(err, &ee) || ee.Code != core.ErasureCodeLastGuardian {
		t.Fatalf("the last guardian of H1 is refused: %v", err)
	}

	// the same guard holds at run time: the household changed during the grace period
	days := 14
	pat := w.users["pat"]

	// pat is the only admin of P, not an owner: make pat the only owner of P
	members, _ := app.FindRecordsByFilter(core.CollectionNameOrgMembers, "user = {:u} && org = {:o}", "", 0, 0, dbx.Params{"u": pat.Id, "o": w.P.Id})
	members[0].Set("role", "owner")
	mustSave(t, app, members[0])

	if _, err := core.RequestErasure(app, core.ErasureRequestInput{Subject: pat, Requester: w.su, Mode: core.ErasureModeAdmin, IdentityCheck: "x", GraceDays: &days, Ctx: w.ctx}); !errors.As(err, &ee) || ee.Code != core.ErasureCodeLastOwner || ee.Ids[0] != w.P.Id {
		t.Fatalf("the last owner is refused: %v", err)
	}
}

func TestErasureRefusalAtRunTimeChangesNothingAndUnlocks(t *testing.T) {
	w := newErWorld(t)
	app := w.app()
	ned := w.users["ned"]
	days := 14

	req := w.request(t, ned, &days)

	// during the grace period mix (the other guardian of H1) leaves the household
	mixM := w.fresh(t, w.mixM)
	mixM.Set("status", core.HouseholdMemberLeft)
	mixM.Set("guardian", false)
	mustSave(t, app, mixM)

	_, err := app.NonconcurrentDB().Update(core.CollectionNameDataRequests,
		dbx.Params{"graceUntil": time.Now().Add(-time.Minute).UTC().Format("2006-01-02 15:04:05.000Z")},
		dbx.HashExp{"id": req.Id}).Execute()
	if err != nil {
		t.Fatal(err)
	}

	done := w.finish(t, req.Id)
	if done.GetString("state") != core.DataRequestStateFailed || !strings.Contains(done.GetString("error"), "guardian") {
		t.Fatalf("state %s error %q", done.GetString("state"), done.GetString("error"))
	}

	if core.IsErasedAccount(w.fresh(t, ned)) || w.fresh(t, w.nedM).GetString("name") == "" {
		t.Error("nothing may be erased")
	}

	if core.IsBanned(app, ned) {
		t.Error("a refused erasure must unlock the account")
	}

	if res := erResult(t, done); res.Refused != core.ErasureCodeLastGuardian {
		t.Errorf("result %+v", res)
	}
}

func TestErasureNoticeIsSentBeforeTheAddressIsAnonymized(t *testing.T) {
	w := newErWorld(t)
	gwen := w.users["gwen"]
	email := gwen.Email()

	w.erase(t, gwen)

	n := w.lastNotice("result")
	if n == nil {
		t.Fatal("no notice")
	}

	if n.To != email || n.AcctMail != email {
		t.Errorf("the notice went to %q and the stored address was %q when it was sent", n.To, n.AcctMail)
	}

	if w.fresh(t, gwen).Email() == email {
		t.Error("the address must be anonymized afterwards")
	}

	if res := n.Result; res == nil || res.Anonymized["users"] != 1 {
		t.Errorf("the notice says the account is anonymized: %+v", res)
	}
}

func TestErasureOfAChildWithoutLoginReplaysAfterARestore(t *testing.T) {
	w := newErWorld(t)
	app := w.app()
	kid := w.kid2

	// the staff path works for a person without a login
	req, res := w.erase(t, kid)
	if req.GetString("state") != core.DataRequestStateDone || res.Anonymized[core.CollectionNameHouseholdMembers] != 1 {
		t.Fatalf("state %s %+v", req.GetString("state"), res)
	}

	// the household still has its guardian and its shape
	if _, err := app.FindRecordById(core.CollectionNameHouseholds, w.h2.Id); err != nil {
		t.Fatal("the household must stay")
	}

	// a restore brings the child back
	back := w.fresh(t, kid)
	back.Set("name", "Kid Two Again")
	back.Set("status", core.HouseholdMemberActive)
	mustSave(t, app, back)

	n, err := core.ReplayErasureLog(app)
	if err != nil || n != 1 {
		t.Fatalf("replay found %d: %v", n, err)
	}

	rows, _ := app.FindRecordsByFilter(core.CollectionNameDataRequests, "type = 'erase' && state = 'queued'", "", 0, 0)
	if len(rows) != 1 {
		t.Fatalf("queued: %d", len(rows))
	}

	if done := w.finish(t, rows[0].Id); done.GetString("state") != core.DataRequestStateDone {
		t.Fatalf("state %s err %s", done.GetString("state"), done.GetString("error"))
	}

	if got := w.fresh(t, kid); got.GetString("name") != "" || got.GetString("status") != core.HouseholdMemberLeft {
		t.Errorf("the replay must erase the child again: %v", got.FieldsData())
	}

	if n, _ := core.ReplayErasureLog(app); n != 0 {
		t.Errorf("idempotent: %d", n)
	}

	// a person with a login is refused through the household row: use the account
	if _, err := core.RequestErasure(app, core.ErasureRequestInput{Subject: w.yanM, Requester: w.su, Mode: core.ErasureModeAdmin, IdentityCheck: "x", Ctx: w.ctx}); err == nil {
		t.Error("a row with a login is erased through the account")
	}
}

func TestErasureEntryIsDeliveredAsAWebhookWithIdsAndCountsOnly(t *testing.T) {
	w := newErWorld(t)
	app := w.app()
	gwen := w.users["gwen"]

	t.Setenv(core.WebhooksAllowLocalhostEnv, "true")
	core.WebhookTestSeams(app).SetServing(false) // nothing is sent; the deliveries are queued

	addEndpoint(t, app, "http://127.0.0.1:9/hook", []string{"*"}, true)

	_, _ = w.erase(t, gwen)

	rows, err := app.FindRecordsByFilter(core.CollectionNameWebhookDeliveries, "eventType = 'gdpr.erase'", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	if len(rows) != 1 {
		t.Fatalf("one gdpr.erase delivery, got %d", len(rows))
	}

	payload := string(erJSON(t, rows[0].GetRaw("payload")))
	for _, bad := range []string{gwen.Id, gwen.Email()} {
		if strings.Contains(payload, bad) {
			t.Errorf("the webhook payload names the person: %s", payload)
		}
	}

	if !strings.Contains(payload, core.ErasurePseudonym(gwen.Id)) {
		t.Errorf("the payload should carry the pseudonym: %s", payload)
	}
}
