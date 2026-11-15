package apis_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/apis"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// Fork-local (Claviger #88, 88-4): the erasure routes and the emailed links.

type erasureEnv struct {
	*exportEnv
	adminFresh string
	pageMux    http.Handler
}

func newErasureEnv(t *testing.T) *erasureEnv {
	t.Helper()

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(app.Cleanup)

	// one router, the hosted pages bound on serve like the real server does
	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}

	var mux http.Handler

	if err := app.OnServe().Trigger(&core.ServeEvent{App: app, Router: router}, func(e *core.ServeEvent) error {
		m, err := e.Router.BuildMux()
		mux = m

		return err
	}); err != nil {
		t.Fatal(err)
	}

	ee := &exportEnv{apiKeyEnv: &apiKeyEnv{t: t, app: app, mux: mux}}
	env := &erasureEnv{exportEnv: ee, pageMux: mux}

	users, _ := app.FindCollectionByNameOrId("users")
	mk := func(email string) *core.Record {
		u := core.NewRecord(users)
		u.SetEmail(email)
		u.SetPassword("1234567890")
		u.Set("name", "name of "+email)

		if err := app.Save(u); err != nil {
			t.Fatal(err)
		}

		return u
	}

	ee.user, ee.other = mk("export-a@example.com"), mk("export-b@example.com")
	ee.userTok, _ = ee.user.NewAuthToken()
	ee.otherTok, _ = ee.other.NewAuthToken()

	admin, err := app.FindAuthRecordByEmail(core.CollectionNameSuperusers, "test@example.com")
	if err != nil {
		t.Fatal(err)
	}

	ee.adminTok, _ = admin.NewAuthToken()

	env.adminFresh, err = admin.NewPasskeyAuthToken(time.Now(), true)
	if err != nil {
		t.Fatal(err)
	}

	return env
}

func (env *erasureEnv) wait(id string) map[string]any {
	env.t.Helper()

	deadline := time.Now().Add(20 * time.Second)

	for {
		core.ProcessErasures(env.app)

		code, body := env.do(http.MethodGet, "/api/gdpr/erasures/"+id, "", env.adminTok)
		if code != http.StatusOK {
			env.t.Fatalf("status %d %s", code, body)
		}

		var v map[string]any
		_ = json.Unmarshal([]byte(body), &v)

		switch v["state"] {
		case core.DataRequestStateDone, core.DataRequestStatePartial, core.DataRequestStateFailed, core.DataRequestStateCancelled:
			return v
		}

		if time.Now().After(deadline) {
			env.t.Fatalf("the erasure did not finish: %s", body)
		}

		time.Sleep(30 * time.Millisecond)
	}
}

var erasureLinkRe = regexp.MustCompile(`/erasure/(confirm|cancel)\?token=([A-Za-z0-9_-]+)`)

// linkFrom returns the path and the token of the last email's button.
func (env *erasureEnv) linkFrom(kind string) string {
	env.t.Helper()

	msg := env.app.TestMailer.LastMessage()

	for _, m := range erasureLinkRe.FindAllStringSubmatch(msg.HTML, -1) {
		if m[1] == kind {
			tok, _ := url.QueryUnescape(m[2])

			return tok
		}
	}

	env.t.Fatalf("no %s link in the email: %s", kind, msg.HTML)

	return ""
}

func (env *erasureEnv) page(method, path string, form url.Values) (int, string, *http.Response) {
	var req *http.Request

	if method == http.MethodPost {
		req = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		req.Header.Set("content-type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}

	rec := httptest.NewRecorder()
	env.pageMux.ServeHTTP(rec, req)

	res := rec.Result()

	return rec.Code, pageBody(env.t, res), res
}

func TestErasureAdminRoutesAreForSuperusersOnly(t *testing.T) {
	env := newErasureEnv(t)

	routes := []struct{ method, url string }{
		{http.MethodPost, "/api/gdpr/subjects/" + env.user.Id + "/erase"},
		{http.MethodGet, "/api/gdpr/erasures"},
		{http.MethodGet, "/api/gdpr/erasures/abc"},
		{http.MethodPost, "/api/gdpr/erasures/abc/cancel"},
	}

	key, _ := env.mint("users", env.user.Id, "*")

	for _, r := range routes {
		for who, tok := range map[string]string{"anonymous": "", "a user": env.userTok, "another user": env.otherTok, "an api key": key} {
			if code, _ := env.do(r.method, r.url, `{"identityCheck":"x"}`, tok); code != http.StatusUnauthorized && code != http.StatusForbidden {
				t.Errorf("%s %s as %s: %d", r.method, r.url, who, code)
			}
		}
	}

	// a superuser without a fresh passkey is asked for one, and nothing happens
	code, body := env.do(http.MethodPost, "/api/gdpr/subjects/"+env.user.Id+"/erase", `{"identityCheck":"x"}`, env.adminTok)
	if code != http.StatusForbidden || !strings.Contains(body, "passkey_step_up_required") {
		t.Fatalf("a destructive admin route needs a fresh passkey: %d %s", code, body)
	}

	if n, _ := env.app.CountRecords(core.CollectionNameDataRequests, dbx.HashExp{"type": "erase"}); n != 0 {
		t.Fatal("no request may exist")
	}

	// the records API cannot create or change an erasure request or the log either
	for _, c := range []string{core.CollectionNameErasureLog, core.CollectionNameDataRequests} {
		if code, _ := env.do(http.MethodPost, "/api/collections/"+c+"/records", `{"type":"erase","state":"queued","collectionRef":"x","recordRef":"y","subjectHash":"h"}`, env.adminFresh); code != http.StatusForbidden && code != http.StatusBadRequest {
			t.Errorf("create in %s through the records API: %d", c, code)
		}
	}
}

func TestErasureAdminRouteStaffPath(t *testing.T) {
	env := newErasureEnv(t)

	// the identity check is required without the emailed confirmation
	if code, body := env.do(http.MethodPost, "/api/gdpr/subjects/"+env.user.Id+"/erase", `{}`, env.adminFresh); code != http.StatusBadRequest || !strings.Contains(body, "identity_check_required") {
		t.Fatalf("identity check: %d %s", code, body)
	}

	if code, _ := env.do(http.MethodPost, "/api/gdpr/subjects/nope/erase", `{"identityCheck":"x"}`, env.adminFresh); code != http.StatusNotFound {
		t.Fatalf("unknown subject: %d", code)
	}

	code, body := env.do(http.MethodPost, "/api/gdpr/subjects/"+env.user.Id+"/erase", `{"identityCheck":"Passport seen at the office","graceDays":0}`, env.adminFresh)
	if code != http.StatusAccepted {
		t.Fatalf("start: %d %s", code, body)
	}

	var v map[string]any
	_ = json.Unmarshal([]byte(body), &v)

	id, _ := v["id"].(string)
	if v["identityCheck"] != "Passport seen at the office" || v["mode"] != "admin" {
		t.Errorf("view %s", body)
	}

	done := env.wait(id)
	if done["state"] != core.DataRequestStateDone {
		t.Fatalf("not done: %v", done)
	}

	// the stub: the pseudonym, not the id
	if done["subjectId"] == env.user.Id {
		t.Error("the finished request must not name the person")
	}

	acct, _ := env.app.FindRecordById("users", env.user.Id)
	if !core.IsErasedAccount(acct) {
		t.Error("the account must be anonymized")
	}

	// the old token of the erased account no longer works
	if code, _ := env.do(http.MethodGet, "/api/me/erasure", "", env.userTok); code != http.StatusUnauthorized && code != http.StatusForbidden {
		t.Errorf("a token of an erased account: %d", code)
	}

	// the person was told (the staff path sends the result when there is an address)
	if m := env.app.TestMailer.LastMessage(); !strings.Contains(m.HTML, "erasure request") || m.To[0].Address != "export-a@example.com" {
		t.Errorf("the result email: to %v %q", m.To, m.Subject)
	}

	// listing and viewing are for superusers, the listing filters by subject
	code, body = env.do(http.MethodGet, "/api/gdpr/erasures", "", env.adminTok)
	if code != http.StatusOK || !strings.Contains(body, id) {
		t.Fatalf("list: %d %s", code, body)
	}
}

func TestErasureSelfServiceFlow(t *testing.T) {
	env := newErasureEnv(t)

	if code, _ := env.do(http.MethodPost, "/api/me/erasure", `{}`, ""); code != http.StatusUnauthorized && code != http.StatusForbidden {
		t.Fatalf("anonymous: %d", code)
	}

	// step-up first: no password, wrong password
	if code, body := env.do(http.MethodPost, "/api/me/erasure", `{}`, env.userTok); code != http.StatusBadRequest || !strings.Contains(body, "password") {
		t.Fatalf("no step-up: %d %s", code, body)
	}

	if code, _ := env.do(http.MethodPost, "/api/me/erasure", `{"password":"wrong-password"}`, env.userTok); code != http.StatusBadRequest {
		t.Fatalf("wrong password: %d", code)
	}

	// superusers use the admin route; api keys are refused
	if code, _ := env.do(http.MethodPost, "/api/me/erasure", `{}`, env.adminTok); code != http.StatusForbidden {
		t.Fatalf("superuser: %d", code)
	}

	key, _ := env.mint("users", env.user.Id, "*")
	if code, _ := env.do(http.MethodPost, "/api/me/erasure", `{"password":"1234567890"}`, key); code != http.StatusForbidden {
		t.Fatalf("api key: %d", code)
	}

	// a person cannot ask for someone else's child
	code, body := env.do(http.MethodPost, "/api/me/erasure", `{"password":"1234567890","member":"nope"}`, env.userTok)
	if code != http.StatusNotFound {
		t.Fatalf("unknown member: %d %s", code, body)
	}

	code, body = env.do(http.MethodPost, "/api/me/erasure", `{"password":"1234567890"}`, env.userTok)
	if code != http.StatusAccepted {
		t.Fatalf("request: %d %s", code, body)
	}

	var v map[string]any
	_ = json.Unmarshal([]byte(body), &v)

	id, _ := v["id"].(string)
	if v["state"] != core.DataRequestStateAwaitingConfirmation || v["identityCheck"] != nil {
		t.Fatalf("view %s", body)
	}

	// nothing changed yet
	acct, _ := env.app.FindRecordById("users", env.user.Id)
	if core.IsErasedAccount(acct) || core.IsBanned(env.app, acct) {
		t.Fatal("nothing may change before the confirmation")
	}

	// a second request is refused while one is open
	if code, _ := env.do(http.MethodPost, "/api/me/erasure", `{"password":"1234567890"}`, env.userTok); code != http.StatusConflict {
		t.Fatalf("a second request: %d", code)
	}

	// the link opens a page with a button; opening it changes nothing (mail scanners)
	token := env.linkFrom("confirm")

	pcode, page, res := env.page(http.MethodGet, "/erasure/confirm?token="+url.QueryEscape(token), nil)
	if pcode != http.StatusOK || !strings.Contains(page, `method="post"`) || !strings.Contains(page, token) {
		t.Fatalf("page: %d %s", pcode, page)
	}

	strictPage(t, res, page)

	if r, _ := env.app.FindRecordById(core.CollectionNameDataRequests, id); r.GetString("state") != core.DataRequestStateAwaitingConfirmation {
		t.Fatal("a GET must not confirm")
	}

	// confirm
	pcode, page, _ = env.page(http.MethodPost, "/erasure/confirm", url.Values{"token": {token}})
	if pcode != http.StatusOK || !strings.Contains(page, "confirmed") {
		t.Fatalf("confirm: %d %s", pcode, page)
	}

	acct, _ = env.app.FindRecordById("users", env.user.Id)
	ban := core.FindActiveBan(env.app, acct.Collection().Id, acct.Id)

	if ban == nil || ban.GetString("reason") != core.BanReasonPendingErasure {
		t.Fatalf("the account is locked: %v", ban)
	}

	// the locked account cannot use the old token any more (the token key was rotated)
	if code, _ := env.do(http.MethodGet, "/api/me/erasure", "", env.userTok); code == http.StatusOK {
		t.Error("a locked account must lose its sessions")
	}

	// the grace email carries the cancel link; the confirm link cannot be reused
	if code, _, _ := env.page(http.MethodPost, "/erasure/confirm", url.Values{"token": {token}}); code != http.StatusBadRequest {
		t.Errorf("the link must work once: %d", code)
	}

	cancel := env.linkFrom("cancel")

	if code, _, _ := env.page(http.MethodGet, "/erasure/cancel?token="+url.QueryEscape(cancel), nil); code != http.StatusOK {
		t.Fatal("cancel page")
	}

	if r, _ := env.app.FindRecordById(core.CollectionNameDataRequests, id); r.GetString("state") != core.DataRequestStateGrace {
		t.Fatal("a GET must not cancel")
	}

	if code, page, _ := env.page(http.MethodPost, "/erasure/cancel", url.Values{"token": {cancel}}); code != http.StatusOK || !strings.Contains(page, "cancelled") {
		t.Fatalf("cancel: %d %s", code, page)
	}

	acct, _ = env.app.FindRecordById("users", env.user.Id)
	if core.IsBanned(env.app, acct) || core.IsErasedAccount(acct) {
		t.Error("cancelled: unlocked, nothing erased")
	}

	// a wrong or empty token is refused without saying which
	if code, _, _ := env.page(http.MethodPost, "/erasure/cancel", url.Values{"token": {"nope"}}); code != http.StatusBadRequest {
		t.Errorf("a wrong token: %d", code)
	}

	if code, page, _ := env.page(http.MethodGet, "/erasure/confirm", nil); code != http.StatusOK || strings.Contains(page, `method="post"`) {
		t.Errorf("a page without a token offers no button: %d", code)
	}
}

func TestErasureOtherPeopleCannotSeeOrCancel(t *testing.T) {
	env := newErasureEnv(t)

	code, body := env.do(http.MethodPost, "/api/me/erasure", `{"password":"1234567890"}`, env.userTok)
	if code != http.StatusAccepted {
		t.Fatalf("%d %s", code, body)
	}

	var v map[string]any
	_ = json.Unmarshal([]byte(body), &v)

	id, _ := v["id"].(string)

	// the other user sees nothing of it
	_, list := env.do(http.MethodGet, "/api/me/erasure", "", env.otherTok)
	if strings.Contains(list, id) {
		t.Error("another person's request must not be listed")
	}

	if code, _ := env.do(http.MethodPost, "/api/me/erasure/"+id+"/cancel", `{"password":"1234567890"}`, env.otherTok); code != http.StatusNotFound {
		t.Errorf("another person cancels: %d", code)
	}

	// the person cancels their own, with step-up
	if code, _ := env.do(http.MethodPost, "/api/me/erasure/"+id+"/cancel", `{}`, env.userTok); code != http.StatusBadRequest {
		t.Errorf("cancel without step-up: %d", code)
	}

	if code, body := env.do(http.MethodPost, "/api/me/erasure/"+id+"/cancel", `{"password":"1234567890"}`, env.userTok); code != http.StatusOK || !strings.Contains(body, `"cancelled"`) {
		t.Errorf("cancel: %d %s", code, body)
	}
}

func TestErasurePasskeySignInCancelsTheErasureOfALockedAccount(t *testing.T) {
	env := newErasureEnv(t)

	days := 14

	acct, _ := env.app.FindRecordById("users", env.user.Id)
	admin, _ := env.app.FindAuthRecordByEmail(core.CollectionNameSuperusers, "test@example.com")

	req, err := core.RequestErasure(env.app, core.ErasureRequestInput{
		Subject: acct, Requester: admin, Mode: core.ErasureModeAdmin, IdentityCheck: "id", GraceDays: &days,
	})
	if err != nil {
		t.Fatal(err)
	}

	if !core.IsBanned(env.app, acct) {
		t.Fatal("setup: locked")
	}

	// the function the passkey sign-in calls (apis.recordAuthResponse); the
	// ceremony itself is covered by the passkey tests
	if !core.CancelPendingErasureForAccount(env.app, nil, acct, "") {
		t.Fatal("not cancelled")
	}

	if core.IsBanned(env.app, acct) {
		t.Error("unlocked")
	}

	if r, _ := env.app.FindRecordById(core.CollectionNameDataRequests, req.Id); r.GetString("state") != core.DataRequestStateCancelled {
		t.Errorf("state %s", r.GetString("state"))
	}
}

func TestErasureSecretNeverLeavesThroughTheAPI(t *testing.T) {
	env := newErasureEnv(t)

	code, body := env.do(http.MethodPost, "/api/gdpr/subjects/"+env.user.Id+"/erase", `{"identityCheck":"id","graceDays":0}`, env.adminFresh)
	if code != http.StatusAccepted {
		t.Fatalf("%d %s", code, body)
	}

	var v map[string]any
	_ = json.Unmarshal([]byte(body), &v)
	env.wait(v["id"].(string))

	var secret string
	if err := env.app.DB().NewQuery("SELECT [[value]] FROM {{_params}} WHERE [[id]]='erasureSecret'").Row(&secret); err != nil {
		t.Fatal(err)
	}

	secret = strings.Trim(secret, `"`)
	if len(secret) < 40 {
		t.Fatalf("secret %q", secret)
	}

	for _, u := range []string{
		"/api/settings", "/api/collections?perPage=200", "/api/gdpr/erasures", "/api/gdpr/erasures/" + v["id"].(string),
		"/api/collections/" + core.CollectionNameErasureLog + "/records", "/api/collections/" + core.CollectionNameDataRequests + "/records",
		"/api/collections/" + core.CollectionNameAuditLog + "/records?perPage=500", "/api/collections/_params/records",
	} {
		_, b := env.do(http.MethodGet, u, "", env.adminTok)
		if strings.Contains(b, secret) {
			t.Errorf("%s returns the secret", u)
		}
	}
}
