package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #88, 88-4): erasure.
//
// World: the household world of the #80 tests (H1: mix and ned guardians, yan
// an adult with a login, kid1 a minor; H2: qed and kid2), a superuser, a
// pastoral notes collection and a sacramental register. The notifier is a
// fake that records what it was asked to send, and what the account looked
// like at that moment.

type erWorld struct {
	*hhWorld
	su *core.Record

	mu      sync.Mutex
	notices []*erNotice
}

type erNotice struct {
	Kind     string
	To       string // the address the notice went to
	Token    string
	Result   *core.ErasureResult
	AcctMail string // the stored email of the subject at the time of sending
}

func newErWorld(t *testing.T) *erWorld {
	t.Helper()

	hw := newHHWorld(t)
	w := &erWorld{hhWorld: hw}
	app := hw.f.app

	col, _ := app.FindCollectionByNameOrId(core.CollectionNameSuperusers)
	w.su = core.NewRecord(col)
	w.su.SetEmail("root@erasure.test")
	w.su.SetPassword("password123")
	mustSave(t, app, w.su)

	old := core.ErasureNotifier
	core.ErasureNotifier = func(a core.App, n *core.ErasureNotice) error {
		w.mu.Lock()
		defer w.mu.Unlock()

		stored := ""
		if r, err := a.FindRecordById(n.Subject.Collection(), n.Subject.Id); err == nil && n.Subject.Collection().IsAuth() {
			stored = r.Email()
		}

		w.notices = append(w.notices, &erNotice{Kind: n.Kind, To: n.Recipient.Email(), Token: n.Token, Result: n.Result, AcctMail: stored})

		return nil
	}

	t.Cleanup(func() { core.ErasureNotifier = old })

	return w
}

func (w *erWorld) lastNotice(kind string) *erNotice {
	w.mu.Lock()
	defer w.mu.Unlock()

	for i := len(w.notices) - 1; i >= 0; i-- {
		if w.notices[i].Kind == kind {
			return w.notices[i]
		}
	}

	return nil
}

func (w *erWorld) noticeCount(kind string) int {
	w.mu.Lock()
	defer w.mu.Unlock()

	n := 0

	for _, x := range w.notices {
		if x.Kind == kind {
			n++
		}
	}

	return n
}

func (w *erWorld) app() core.App { return w.f.app }

// seed saves a row without validation (the system collections have required
// fields the test does not care about).
func (w *erWorld) seed(t *testing.T, collection string, vals map[string]any) *core.Record {
	t.Helper()

	col, err := w.app().FindCollectionByNameOrId(collection)
	if err != nil {
		t.Fatalf("%s: %v", collection, err)
	}

	r := core.NewRecord(col)
	for k, v := range vals {
		r.Set(k, v)
	}

	if err := w.app().SaveNoValidate(r); err != nil {
		t.Fatalf("seed %s: %v", collection, err)
	}

	return r
}

func zero() *int { z := 0; return &z }

// request starts an admin erasure with the identity check and no grace period.
func (w *erWorld) request(t *testing.T, subject *core.Record, grace *int) *core.Record {
	t.Helper()

	req, err := core.RequestErasure(w.app(), core.ErasureRequestInput{
		Subject: subject, Requester: w.su, Mode: core.ErasureModeAdmin, IdentityCheck: "checked a photo ID at the office",
		GraceDays: grace, Ctx: w.ctx,
	})
	if err != nil {
		t.Fatalf("request: %v", err)
	}

	return req
}

// finish waits for the request to end and returns it.
func (w *erWorld) finish(t *testing.T, id string) *core.Record {
	t.Helper()

	deadline := time.Now().Add(20 * time.Second)

	for time.Now().Before(deadline) {
		core.ProcessErasures(w.app())

		r, err := w.app().FindRecordById(core.CollectionNameDataRequests, id)
		if err != nil {
			t.Fatal(err)
		}

		switch r.GetString("state") {
		case core.DataRequestStateDone, core.DataRequestStatePartial, core.DataRequestStateFailed, core.DataRequestStateCancelled:
			return r
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatal("the erasure did not finish")

	return nil
}

func (w *erWorld) erase(t *testing.T, subject *core.Record) (*core.Record, *core.ErasureResult) {
	t.Helper()

	req := w.finish(t, w.request(t, subject, zero()).Id)

	return req, erResult(t, req)
}

func erResult(t *testing.T, req *core.Record) *core.ErasureResult {
	t.Helper()

	res := &core.ErasureResult{}
	if err := req.UnmarshalJSONField("result", res); err != nil {
		t.Fatal(err)
	}

	return res
}

func (w *erWorld) count(t *testing.T, collection, filter string, params ...dbx.Params) int {
	t.Helper()

	p := dbx.Params{}
	if len(params) > 0 {
		p = params[0]
	}

	rows, err := w.app().FindRecordsByFilter(collection, filter, "", 0, 0, p)
	if err != nil {
		t.Fatalf("%s: %v", collection, err)
	}

	return len(rows)
}

func (w *erWorld) fresh(t *testing.T, r *core.Record) *core.Record {
	t.Helper()

	got, err := w.app().FindRecordById(r.Collection(), r.Id)
	if err != nil {
		t.Fatal(err)
	}

	return got
}

// seedPerson gives the user a row in every collection of the data map table
// (spec 10.3) and returns what was made.
func (w *erWorld) seedPerson(t *testing.T, u *core.Record) {
	t.Helper()

	uc := u.Collection().Id
	ref := map[string]any{"collectionRef": uc, "recordRef": u.Id}

	with := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range ref {
			out[k] = v
		}

		for k, v := range extra {
			out[k] = v
		}

		return out
	}

	w.seed(t, core.CollectionNameExternalAuths, with(map[string]any{"provider": "google", "providerId": "g-1"}))
	w.seed(t, core.CollectionNameWebAuthnCredentials, with(map[string]any{"credentialId": "cred-" + u.Id, "name": "phone"}))
	w.seed(t, core.CollectionNameTOTPs, with(map[string]any{"secret": "s", "enabled": true}))
	w.seed(t, core.CollectionNameMFAs, with(map[string]any{"method": "otp"}))
	w.seed(t, core.CollectionNameOTPs, with(map[string]any{"sentTo": u.Email()}))
	w.seed(t, core.CollectionNameSessions, with(map[string]any{"userAgent": "UA", "ip": "203.0.113.9"}))
	w.seed(t, core.CollectionNameAuthOrigins, with(map[string]any{"fingerprint": "fp"}))
	w.seed(t, core.CollectionNameAPIKeys, with(map[string]any{"name": "key", "prefix": "ck_1"}))
	w.seed(t, core.CollectionNameOAuthConsents, with(map[string]any{"clientId": "c1"}))

	oauth := map[string]any{"collectionRef": uc, "subject": u.Id, "clientId": "c1"}
	for _, c := range []string{core.CollectionNameOAuthTokens, core.CollectionNameOAuthRefreshTokens, core.CollectionNameOAuthRequests, core.CollectionNameOAuthSso, core.CollectionNameOAuthDeviceCodes} {
		w.seed(t, c, oauth)
	}

	w.seed(t, core.CollectionNameRecoveryCodes, map[string]any{"user": u.Id, "note": "vouched"})
	w.seed(t, core.CollectionNameCredentials, map[string]any{"user": u.Id, "type": "background", "issuer": "state"})
	w.seed(t, core.CollectionNameInvitations, map[string]any{"email": u.Email(), "org": w.P.Id, "status": "pending"})
	w.seed(t, core.CollectionNameWebhookDeliveries, map[string]any{"eventType": "record.created", "payload": map[string]any{"record": map[string]any{"id": u.Id, "email": u.Email()}}})
}

func TestErasureAdultAcrossTheDataMap(t *testing.T) {
	w := newErWorld(t)
	app := w.app()

	ned, nedM := w.users["ned"], w.nedM
	email, nedId := ned.Email(), ned.Id

	ned.Set("name", "Ned Person")
	ned.Set("username", "nedperson")
	mustSave(t, app, ned)
	w.seedPerson(t, ned)

	// an org membership, an org transfer, a household row (nedM), consents
	// under the user id and under the household row
	transfer := w.seed(t, core.CollectionNameOrgTransfers, map[string]any{"person": ned.Id, "fromOrg": w.P.Id, "toOrg": w.P.Id, "state": "pending", "startedBy": ned.Id})

	purpose := consentPurpose(t, w.hhWorld, map[string]any{"key": "newsletter", "name": "Newsletter", "text": "We email you."})
	_ = purpose

	_, err := core.ConsentSelf(app, w.ctx, w.fresh(t, ned), "newsletter", "", core.ConsentActionGrant)
	if err != nil {
		t.Fatal(err)
	}

	bg := w.seed(t, core.CollectionNameBreakGlass, map[string]any{"collection": "pastoral_notes", "recordId": "n1", "org": w.P.Id, "grantee": ned.Id, "reason": "an emergency", "approvedBy": w.users["pat"].Id})

	// a lifted hold on the person: kept as evidence, pseudonymized
	hold, err := core.PlaceLegalHold(app, w.ctx, w.su, core.LegalHoldInput{Scope: core.LegalHoldScopeSubject, Subject: ned.Id, SubjectCollection: "users", Reason: "a dispute that ended", Reference: "R-1"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := core.LiftLegalHold(app, w.ctx, hold.Id, w.su); err != nil {
		t.Fatal(err)
	}

	// an export request with a result
	exp := w.seed(t, core.CollectionNameDataRequests, map[string]any{"type": "export", "state": "done", "collectionRef": ned.Collection().Id, "recordRef": ned.Id})

	// the audit log already holds entries about ned: the creation of the
	// account, with the email in "after"
	before := w.count(t, core.CollectionNameAuditLog, "targetId = {:id}", dbx.Params{"id": nedId})
	if before == 0 {
		t.Fatal("expected audit entries about the account")
	}

	req, res := w.erase(t, ned)

	t.Logf("%s", erJSON(t, res))
	if req.GetString("state") != core.DataRequestStateDone {
		t.Fatalf("state %s err %q result %+v", req.GetString("state"), req.GetString("error"), res)
	}

	// erase: every row of the table is gone
	gone := map[string]string{
		core.CollectionNameExternalAuths:       "recordRef",
		core.CollectionNameWebAuthnCredentials: "recordRef",
		core.CollectionNameTOTPs:               "recordRef",
		core.CollectionNameMFAs:                "recordRef",
		core.CollectionNameOTPs:                "recordRef",
		core.CollectionNameSessions:            "recordRef",
		core.CollectionNameAuthOrigins:         "recordRef",
		core.CollectionNameAPIKeys:             "recordRef",
		core.CollectionNameOAuthConsents:       "recordRef",
		core.CollectionNameOAuthTokens:         "subject",
		core.CollectionNameOAuthRefreshTokens:  "subject",
		core.CollectionNameOAuthRequests:       "subject",
		core.CollectionNameOAuthSso:            "subject",
		core.CollectionNameOAuthDeviceCodes:    "subject",
		core.CollectionNameRecoveryCodes:       "user",
		core.CollectionNameCredentials:         "user",
		core.CollectionNameOrgMembers:          "user",
		core.CollectionNameOrgTransfers:        "person",
		core.CollectionNameInvitations:         "email",
		core.CollectionNameWebhookDeliveries:   "id",
		core.CollectionNameDataRequests:        "id",
	}

	for c, field := range gone {
		switch c {
		case core.CollectionNameWebhookDeliveries:
			if n := w.count(t, c, "payload ~ {:id}", dbx.Params{"id": nedId}); n != 0 {
				t.Errorf("%s: %d rows about the person remain", c, n)
			}
		case core.CollectionNameDataRequests:
			if _, err := app.FindRecordById(c, exp.Id); err == nil {
				t.Errorf("the export request remains")
			}
		case core.CollectionNameInvitations:
			if n := w.count(t, c, "email = {:e}", dbx.Params{"e": email}); n != 0 {
				t.Errorf("%s: %d invitations to the address remain", c, n)
			}
		default:
			if n := w.count(t, c, field+" = {:id}", dbx.Params{"id": nedId}); n != 0 {
				t.Errorf("%s: %d rows remain", c, n)
			}
		}
	}

	if _, err := app.FindRecordById(core.CollectionNameOrgTransfers, transfer.Id); err == nil {
		t.Error("the transfer remains")
	}

	// anonymize: the account and the household row
	acct := w.fresh(t, ned)
	if acct.Email() != "erased-"+nedId+"@invalid" || !core.IsErasedAccount(acct) {
		t.Errorf("email %q", acct.Email())
	}

	if acct.GetString("name") != "" || acct.GetString("username") != "erased-"+nedId {
		t.Errorf("name %q username %q", acct.GetString("name"), acct.GetString("username"))
	}

	if acct.ValidatePassword("password123") || acct.TokenKey() == ned.TokenKey() {
		t.Error("the password and the token key must be replaced")
	}

	m := w.fresh(t, nedM)
	if m.GetString("name") != "" || m.GetString("birthDate") != "" || m.GetString("user") != "" || m.GetBool("guardian") || m.GetString("status") != core.HouseholdMemberLeft {
		t.Errorf("the household row was not anonymized: %v", m.FieldsData())
	}

	// export treatment: the household stays for the others
	if _, err := app.FindRecordById(core.CollectionNameHouseholds, w.h1.Id); err != nil {
		t.Error("the household must stay")
	}

	// the account is locked for good
	if ban := core.FindActiveBan(app, acct.Collection().Id, nedId); ban == nil || ban.GetString("reason") != core.BanReasonErased {
		t.Errorf("ban %v", ban)
	}

	// retain, pseudonymized: consents and events
	pseudo := core.ErasurePseudonym(app, nedId)

	if n := w.count(t, core.CollectionNameConsents, "subject = {:p}", dbx.Params{"p": pseudo}); n != 1 {
		t.Errorf("consent states under the pseudonym: %d", n)
	}

	if n := w.count(t, core.CollectionNameConsentEvents, "subject = {:p} && action = 'grant'", dbx.Params{"p": pseudo}); n != 1 {
		t.Errorf("consent events under the pseudonym: %d", n)
	}

	if n := w.count(t, core.CollectionNameConsents, "subject = {:id}", dbx.Params{"id": nedId}); n != 0 {
		t.Errorf("consent states still under the id: %d", n)
	}

	// break-glass: kept, the person replaced
	got := w.fresh(t, bg)
	if got.GetString("grantee") != pseudo || got.GetString("approvedBy") != w.users["pat"].Id || got.GetString("reason") != "an emergency" {
		t.Errorf("break-glass: grantee %q approvedBy %q", got.GetString("grantee"), got.GetString("approvedBy"))
	}

	// the lifted hold: kept, pseudonymized
	if h := w.fresh(t, hold); h.GetString("subject") != pseudo {
		t.Errorf("hold subject %q", h.GetString("subject"))
	}

	// the audit log: entries about the person stay, without personal values
	if n := w.count(t, core.CollectionNameAuditLog, "targetId = {:id}", dbx.Params{"id": nedId}); n != 0 {
		t.Errorf("%d audit entries still carry the account id", n)
	}

	scrubbed, err := app.FindRecordsByFilter(core.CollectionNameAuditLog, "targetId = {:p}", "created", 0, 0, dbx.Params{"p": pseudo})
	if err != nil {
		t.Fatal(err)
	}

	if len(scrubbed) < before {
		t.Errorf("the audit entries about the person were lost: %d of %d", len(scrubbed), before)
	}

	for _, e := range scrubbed {
		if e.GetString("action") == "" || e.GetString("targetCollection") == "" || e.GetDateTime("created").IsZero() {
			t.Errorf("action, collection and time must stay: %v", e.FieldsData())
		}

		for _, f := range []string{"before", "after", "meta"} {
			raw := string(erJSON(t, e.GetRaw(f)))
			if strings.Contains(raw, email) || strings.Contains(raw, nedId) {
				t.Errorf("entry %s still holds the person in %s: %s", e.Id, f, raw)
			}
		}

		if e.GetString("ip") != "" {
			t.Errorf("entry %s keeps an IP", e.Id)
		}
	}

	// nothing anywhere in the audit log names the address any more
	if n := w.count(t, core.CollectionNameAuditLog, "after ~ {:e} || before ~ {:e} || meta ~ {:e}", dbx.Params{"e": email}); n != 0 {
		t.Errorf("%d entries still contain the email address", n)
	}

	// its own entry: pseudonymous, ids and counts only
	own, err := app.FindRecordsByFilter(core.CollectionNameAuditLog, "action = 'gdpr_erase'", "", 0, 0)
	if err != nil || len(own) != 1 {
		t.Fatalf("gdpr_erase entries: %d %v", len(own), err)
	}

	meta := string(erJSON(t, own[0].GetRaw("meta")))
	if strings.Contains(meta, nedId) || strings.Contains(meta, email) || !strings.Contains(meta, pseudo) {
		t.Errorf("gdpr_erase meta: %s", meta)
	}

	// the request keeps the pseudonym, never the id
	if req.GetString("recordRef") != pseudo {
		t.Errorf("recordRef %q", req.GetString("recordRef"))
	}

	if req.GetString("progress") != "" && req.GetString("progress") != "null" {
		t.Errorf("progress must be cleared: %s", req.GetString("progress"))
	}

	// the erasure log: a hash, nothing reversible
	logs, _ := app.FindAllRecords(core.CollectionNameErasureLog)
	if len(logs) != 1 || logs[0].GetString("subjectHash") != core.ErasureSubjectHash(app, "users", nedId) {
		t.Fatalf("erasure log: %v", logs)
	}

	if strings.Contains(string(erJSON(t, logs[0].FieldsData())), nedId) {
		t.Error("the erasure log holds the id")
	}

	if !slices.Contains(mustStrings(t, logs[0], "collections"), core.CollectionNameSessions) {
		t.Errorf("the log lists the collections touched: %v", logs[0].GetRaw("collections"))
	}
}

func erJSON(t *testing.T, v any) []byte {
	t.Helper()

	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}

	return b
}

func mustStrings(t *testing.T, r *core.Record, field string) []string {
	t.Helper()

	var out []string
	if err := r.UnmarshalJSONField(field, &out); err != nil {
		t.Fatal(err)
	}

	return out
}

var _ = errors.New
var _ = context.Background
