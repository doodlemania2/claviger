package apis_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
	"github.com/doodlemania2/claviger/tools/security"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #93).

const (
	sessUserId  = "4q1xlclmfloku33"
	sessSuperId = "sywbhecnh46rhm0"
)

// newSessionEnv is a test app with MFA switched off (the fixture enables it
// for users); the MFA test turns it back on.
func newSessionEnv(t testing.TB) *apiKeyEnv {
	t.Helper()
	env := newAPIKeyEnv(t)
	for _, name := range []string{"users", "clients", core.CollectionNameSuperusers} {
		c, err := env.app.FindCollectionByNameOrId(name)
		if err != nil {
			t.Fatal(err)
		}
		c.MFA.Enabled = false
		if err := env.app.Save(c); err != nil {
			t.Fatal(err)
		}
	}
	return env
}

type sessResp struct {
	Token  string         `json:"token"`
	Record map[string]any `json:"record"`
	MfaId  string         `json:"mfaId"`
}

func (env *apiKeyEnv) login(collection, identity, password string) (int, sessResp, string) {
	env.t.Helper()
	status, body := env.do("POST", "/api/collections/"+collection+"/auth-with-password",
		fmt.Sprintf(`{"identity":%q,"password":%q}`, identity, password), "")
	var r sessResp
	_ = json.Unmarshal([]byte(body), &r)
	return status, r, body
}

func (env *apiKeyEnv) mustLogin(collection, identity, password string) string {
	env.t.Helper()
	status, r, body := env.login(collection, identity, password)
	if status != 200 || r.Token == "" {
		env.t.Fatalf("login failed: %d %s", status, body)
	}
	return r.Token
}

func claimsOf(t testing.TB, token string) map[string]any {
	t.Helper()
	c, err := security.ParseUnverifiedJWT(token)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func sidOf(t testing.TB, token string) string {
	t.Helper()
	sid, _ := claimsOf(t, token)["sid"].(string)
	return sid
}

func (env *apiKeyEnv) session(sid string) *core.Record {
	env.t.Helper()
	r, err := env.app.FindRecordById(core.CollectionNameSessions, sid)
	if err != nil {
		env.t.Fatalf("session %q: %v", sid, err)
	}
	return r
}

func (env *apiKeyEnv) status(method, url, body, token string) int {
	s, _ := env.do(method, url, body, token)
	return s
}

func (env *apiKeyEnv) sessionCount(userId string) int {
	n, err := env.app.CountRecords(core.CollectionNameSessions, dbx.HashExp{"recordRef": userId})
	if err != nil {
		env.t.Fatal(err)
	}
	return int(n)
}

func TestSessionCreatedOnPasswordLogin(t *testing.T) {
	t.Parallel()
	env := newSessionEnv(t)

	token := env.mustLogin("users", "test@example.com", "1234567890")
	sid := sidOf(t, token)
	if sid == "" {
		t.Fatal("the token has no sid claim")
	}

	s := env.session(sid)
	if s.GetString("authMethod") != "password" || s.GetString("recordRef") != sessUserId || s.GetBool("revoked") {
		t.Fatalf("unexpected session: %v", s.PublicExport())
	}
	if !s.GetDateTime("expires").Time().After(time.Now().Add(time.Hour)) {
		t.Fatalf("session expires too early: %v", s.GetString("expires"))
	}
	if exp, _ := claimsOf(t, token)["exp"].(float64); int64(exp) != s.GetDateTime("expires").Time().Unix() {
		t.Fatalf("session expiry %v differs from the token exp %v", s.GetDateTime("expires").Time().Unix(), exp)
	}

	// superusers get sessions too
	suToken := env.mustLogin("_superusers", "test@example.com", "1234567890")
	su := env.session(sidOf(t, suToken))
	if su.GetString("authMethod") != "password" {
		t.Fatalf("superuser session: %v", su.PublicExport())
	}

	// the token works
	if s := env.status("POST", "/api/collections/users/auth-refresh", "", token); s != 200 {
		t.Fatalf("auth-refresh: %d", s)
	}
}

func TestSessionCreatedOnOTPAndMFA(t *testing.T) {
	t.Parallel()
	env := newSessionEnv(t)

	users, _ := env.app.FindCollectionByNameOrId("users")
	user := mustFind(t, env.app, "users", sessUserId)

	mkOTP := func(id string) {
		otp := core.NewOTP(env.app)
		otp.Id = id
		otp.SetCollectionRef(users.Id)
		otp.SetRecordRef(user.Id)
		otp.SetPassword("123456")
		if err := env.app.Save(otp); err != nil {
			t.Fatal(err)
		}
	}

	// plain OTP sign-in
	users.MFA.Enabled = false
	if err := env.app.Save(users); err != nil {
		t.Fatal(err)
	}
	mkOTP(strings.Repeat("a", 15))
	status, body := env.do("POST", "/api/collections/users/auth-with-otp", `{"otpId":"`+strings.Repeat("a", 15)+`","password":"123456"}`, "")
	var r sessResp
	_ = json.Unmarshal([]byte(body), &r)
	if status != 200 || sidOf(t, r.Token) == "" {
		t.Fatalf("otp: %d %s", status, body)
	}
	if m := env.session(sidOf(t, r.Token)).GetString("authMethod"); m != "otp" {
		t.Fatalf("expected otp, got %q", m)
	}

	// MFA: password first, then OTP with the mfaId
	users, _ = env.app.FindCollectionByNameOrId("users")
	users.MFA.Enabled = true
	users.MFA.Rule = ""
	if err := env.app.Save(users); err != nil {
		t.Fatal(err)
	}
	status, first, body := env.login("users", "test@example.com", "1234567890")
	if status != 401 || first.MfaId == "" || first.Token != "" {
		t.Fatalf("expected an mfa challenge, got %d %s", status, body)
	}
	if env.sessionCount(sessUserId) != 1 { // only the OTP one: no session before MFA completes
		t.Fatalf("a session was created before the MFA completed: %d", env.sessionCount(sessUserId))
	}

	mkOTP(strings.Repeat("b", 15))
	status, body = env.do("POST", "/api/collections/users/auth-with-otp",
		`{"otpId":"`+strings.Repeat("b", 15)+`","password":"123456","mfaId":"`+first.MfaId+`"}`, "")
	r = sessResp{}
	_ = json.Unmarshal([]byte(body), &r)
	if status != 200 || sidOf(t, r.Token) == "" {
		t.Fatalf("mfa completion: %d %s", status, body)
	}
	if m := env.session(sidOf(t, r.Token)).GetString("authMethod"); m != "mfa" {
		t.Fatalf("expected mfa, got %q", m)
	}
}

func TestSessionRevokeTakesEffectImmediately(t *testing.T) {
	t.Parallel()
	env := newSessionEnv(t)

	token := env.mustLogin("users", "test@example.com", "1234567890")
	sid := sidOf(t, token)

	// warm the cache
	for range 3 {
		if s := env.status("GET", "/api/sessions", "", token); s != 200 {
			t.Fatalf("list: %d", s)
		}
	}

	// the list marks the current session
	_, body := env.do("GET", "/api/sessions", "", token)
	var list struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.Unmarshal([]byte(body), &list)
	if len(list.Items) != 1 || list.Items[0]["id"] != sid || list.Items[0]["current"] != true {
		t.Fatalf("unexpected list: %s", body)
	}

	// revoke through the endpoint (own session)
	if s := env.status("DELETE", "/api/sessions/"+sid, "", token); s != 204 {
		t.Fatalf("revoke: %d", s)
	}
	if s := env.status("GET", "/api/sessions", "", token); s != 401 {
		t.Fatalf("a revoked session must be refused at once, got %d", s)
	}
	if s := env.status("POST", "/api/collections/users/auth-refresh", "", token); s != 401 {
		t.Fatalf("a revoked session must not refresh, got %d", s)
	}
	if env.session(sid).GetString("revokedReason") != core.SessionRevokedByUser {
		t.Fatalf("reason: %q", env.session(sid).GetString("revokedReason"))
	}
}

func TestSessionRevokedThroughRecordsAPIAndExpiry(t *testing.T) {
	t.Parallel()
	env := newSessionEnv(t)

	token := env.mustLogin("users", "test@example.com", "1234567890")
	sid := sidOf(t, token)
	if s := env.status("GET", "/api/sessions", "", token); s != 200 { // cache it
		t.Fatalf("%d", s)
	}

	// a plain record save (what the admin UI records page does) invalidates the cache
	rec := env.session(sid)
	rec.Set("revoked", true)
	if err := env.app.Save(rec); err != nil {
		t.Fatal(err)
	}
	if s := env.status("GET", "/api/sessions", "", token); s != 401 {
		t.Fatalf("got %d", s)
	}

	// an expired session is refused too (the cache entry is dropped by the
	// helper the refresh uses, here by saving)
	token2 := env.mustLogin("users", "test@example.com", "1234567890")
	sid2 := sidOf(t, token2)
	if s := env.status("GET", "/api/sessions", "", token2); s != 200 {
		t.Fatalf("%d", s)
	}
	rec2 := env.session(sid2)
	rec2.Set("expires", types.NowDateTime().Add(-time.Minute))
	if err := env.app.Save(rec2); err != nil {
		t.Fatal(err)
	}
	if s := env.status("GET", "/api/sessions", "", token2); s != 401 {
		t.Fatalf("an expired session must be refused, got %d", s)
	}
}

func TestSessionRefreshKeepsSid(t *testing.T) {
	t.Parallel()
	env := newSessionEnv(t)

	token := env.mustLogin("users", "test@example.com", "1234567890")
	sid := sidOf(t, token)

	// make the stored expiry visibly stale, then refresh
	if _, err := env.app.NonconcurrentDB().NewQuery("UPDATE _sessions SET expires={:e} WHERE id={:id}").
		Bind(dbx.Params{"e": types.NowDateTime().Add(time.Minute).String(), "id": sid}).Execute(); err != nil {
		t.Fatal(err)
	}
	core.InvalidateSessionCache(env.app)

	status, body := env.do("POST", "/api/collections/users/auth-refresh", "", token)
	var r sessResp
	_ = json.Unmarshal([]byte(body), &r)
	if status != 200 || r.Token == "" {
		t.Fatalf("refresh: %d %s", status, body)
	}
	if sidOf(t, r.Token) != sid {
		t.Fatalf("the refresh changed the sid: %q -> %q", sid, sidOf(t, r.Token))
	}
	if _, ok := claimsOf(t, r.Token)["passkeyAt"]; ok {
		t.Fatal("refresh must not carry passkeyAt")
	}
	if got := env.session(sid).GetDateTime("expires").Time(); got.Before(time.Now().Add(24 * time.Hour)) {
		t.Fatalf("the session expiry was not extended: %v", got)
	}
	if env.sessionCount(sessUserId) != 1 {
		t.Fatal("the refresh created another session")
	}
	if s := env.status("GET", "/api/sessions", "", r.Token); s != 200 {
		t.Fatalf("the refreshed token: %d", s)
	}
}

func TestLegacyTokensKeepWorking(t *testing.T) {
	t.Parallel()
	env := newSessionEnv(t)

	// fixtures: tokens minted before sessions existed (no sid), valid until 2050
	for name, token := range map[string]string{"user": auditUserToken, "superuser": auditSuperuserToken} {
		if sidOf(t, token) != "" {
			t.Fatalf("%s fixture has a sid", name)
		}
		if s := env.status("GET", "/api/sessions", "", token); s != 200 {
			t.Fatalf("legacy %s token: %d", name, s)
		}
	}

	// a refresh moves a refreshable legacy token onto a session
	status, body := env.do("POST", "/api/collections/users/auth-refresh", "", auditUserToken)
	var r sessResp
	_ = json.Unmarshal([]byte(body), &r)
	if status != 200 || sidOf(t, r.Token) == "" {
		t.Fatalf("refresh of a legacy token: %d %s", status, body)
	}
	if m := env.session(sidOf(t, r.Token)).GetString("authMethod"); m != "other" {
		t.Fatalf("method %q", m)
	}

	// and the legacy token itself still works afterwards
	if s := env.status("GET", "/api/sessions", "", auditUserToken); s != 200 {
		t.Fatalf("%d", s)
	}
}

func TestSessionRevokeOthers(t *testing.T) {
	t.Parallel()
	env := newSessionEnv(t)

	a := env.mustLogin("users", "test@example.com", "1234567890")
	b := env.mustLogin("users", "test@example.com", "1234567890")
	c := env.mustLogin("users", "test@example.com", "1234567890")

	status, body := env.do("POST", "/api/sessions/revoke-others", "", b)
	if status != 200 || !strings.Contains(body, `"revoked":2`) {
		t.Fatalf("revoke-others: %d %s", status, body)
	}

	if env.status("GET", "/api/sessions", "", a) != 401 || env.status("GET", "/api/sessions", "", c) != 401 {
		t.Fatal("the other sessions must be dead")
	}
	_, body = env.do("GET", "/api/sessions", "", b)
	if !strings.Contains(body, `"current":true`) || strings.Count(body, `"id":"`) != 1 {
		t.Fatalf("only the current session must remain: %s", body)
	}

	// it is audited
	n, _ := env.app.CountRecords(core.CollectionNameAuditLog, dbx.HashExp{"action": "revoke_sessions", "actorId": sessUserId})
	if n != 1 {
		t.Fatalf("expected one revoke_sessions audit entry, got %d", n)
	}
}

func TestSessionsCannotBeTouchedByOthersOrKeys(t *testing.T) {
	t.Parallel()
	env := newSessionEnv(t)

	su := env.mustLogin("_superusers", "test@example.com", "1234567890")
	user := env.mustLogin("users", "test@example.com", "1234567890")
	userSid := sidOf(t, user)

	// another user's session id is not revealed
	other := env.mustLogin("clients", "test@example.com", "1234567890")
	if s := env.status("DELETE", "/api/sessions/"+userSid, "", other); s != 404 {
		t.Fatalf("got %d", s)
	}
	// listing someone else's sessions needs a superuser
	if s := env.status("GET", "/api/sessions?collection=users&record="+sessUserId, "", other); s != 403 {
		t.Fatalf("got %d", s)
	}

	// the superuser lists them and revokes all
	status, body := env.do("GET", "/api/sessions?collection=users&record="+sessUserId, "", su)
	if status != 200 || !strings.Contains(body, userSid) {
		t.Fatalf("%d %s", status, body)
	}
	status, body = env.do("POST", "/api/sessions/revoke-all", `{"collection":"users","record":"`+sessUserId+`","reason":"stolen laptop"}`, su)
	if status != 200 || !strings.Contains(body, `"revoked":1`) {
		t.Fatalf("%d %s", status, body)
	}
	if env.status("GET", "/api/sessions", "", user) != 401 {
		t.Fatal("the session must be dead")
	}
	if r := env.session(userSid).GetString("revokedReason"); !strings.Contains(r, "stolen laptop") {
		t.Fatalf("reason %q", r)
	}
	if s := env.status("POST", "/api/sessions/revoke-all", `{"collection":"users","record":"`+sessUserId+`"}`, other); s != 403 {
		t.Fatalf("non-superuser revoke-all: %d", s)
	}

	// an API key (even a "*" key of a superuser) cannot manage sessions or bans
	key, _ := env.mint("_superusers", sessSuperId, "*")
	for _, c := range [][2]string{
		{"GET", "/api/sessions"},
		{"POST", "/api/sessions/revoke-others"},
		{"POST", "/api/sessions/revoke-all"},
		{"DELETE", "/api/sessions/" + userSid},
		{"GET", "/api/bans"},
		{"POST", "/api/bans"},
	} {
		if s := env.status(c[0], c[1], `{}`, key); s != 403 {
			t.Fatalf("%s %s with an API key: %d", c[0], c[1], s)
		}
	}
}

func TestPasswordChangeRevokesSessions(t *testing.T) {
	t.Parallel()
	env := newSessionEnv(t)

	token := env.mustLogin("users", "test@example.com", "1234567890")
	sid := sidOf(t, token)

	user := mustFind(t, env.app, "users", sessUserId)
	user.SetPassword("brand-new-pass-1")
	if err := env.app.Save(user); err != nil {
		t.Fatal(err)
	}

	s := env.session(sid)
	if !s.GetBool("revoked") || s.GetString("revokedReason") != core.SessionRevokedPasswordChanged {
		t.Fatalf("session after a password change: %v", s.PublicExport())
	}
	if env.status("GET", "/api/sessions", "", token) != 401 {
		t.Fatal("the token must be dead")
	}

	// a bare token key rotation is marked differently
	token2 := env.mustLogin("users", "test@example.com", "brand-new-pass-1")
	user = mustFind(t, env.app, "users", sessUserId)
	user.RefreshTokenKey()
	if err := env.app.Save(user); err != nil {
		t.Fatal(err)
	}
	if r := env.session(sidOf(t, token2)).GetString("revokedReason"); r != core.SessionRevokedTokenKeyRotated {
		t.Fatalf("reason %q", r)
	}
}

func TestSessionLastSeenThrottled(t *testing.T) {
	t.Parallel()
	env := newSessionEnv(t)

	token := env.mustLogin("users", "test@example.com", "1234567890")
	sid := sidOf(t, token)

	setLastSeen := func(d time.Duration) {
		if _, err := env.app.NonconcurrentDB().NewQuery("UPDATE {{_sessions}} SET [[lastSeen]]={:t} WHERE id={:id}").
			Bind(dbx.Params{"t": types.NowDateTime().Add(d).String(), "id": sid}).Execute(); err != nil {
			t.Fatal(err)
		}
		core.InvalidateSessionCache(env.app)
	}

	// recent: not rewritten
	setLastSeen(-time.Minute)
	before := env.session(sid).GetString("lastSeen")
	env.status("GET", "/api/sessions", "", token)
	if after := env.session(sid).GetString("lastSeen"); after != before {
		t.Fatalf("lastSeen was written within the throttle window: %s -> %s", before, after)
	}

	// stale: rewritten once, then left alone
	setLastSeen(-time.Hour)
	stale := env.session(sid).GetString("lastSeen")
	env.status("GET", "/api/sessions", "", token)
	fresh := env.session(sid).GetString("lastSeen")
	if fresh == stale {
		t.Fatal("a stale lastSeen was not updated")
	}
	env.status("GET", "/api/sessions", "", token)
	if again := env.session(sid).GetString("lastSeen"); again != fresh {
		t.Fatalf("lastSeen written twice in the window: %s -> %s", fresh, again)
	}
}

func TestSessionCleanupCron(t *testing.T) {
	t.Parallel()
	env := newSessionEnv(t)

	token := env.mustLogin("users", "test@example.com", "1234567890")
	live := sidOf(t, token)

	old := time.Now().AddDate(0, 0, -45)
	insert := func(id string, revoked bool, expires, lastSeen time.Time) {
		_, err := env.app.NonconcurrentDB().Insert(core.CollectionNameSessions, dbx.Params{
			"id": id, "collectionRef": "_pb_users_auth_", "recordRef": sessUserId,
			"created": types.NowDateTime().String(), "lastSeen": types.NowDateTime().Add(time.Until(lastSeen)).String(),
			"expires": types.NowDateTime().Add(time.Until(expires)).String(), "revoked": revoked,
		}).Execute()
		if err != nil {
			t.Fatal(err)
		}
	}
	insert("expiredold000001", false, old, old)                               // expired long ago: deleted
	insert("revokedold000001", true, time.Now().AddDate(1, 0, 0), old)        // revoked, idle for 45 days: deleted
	insert("revokedrecent001", true, time.Now().AddDate(1, 0, 0), time.Now()) // revoked recently: kept
	insert("expiredrecent001", false, time.Now().Add(-time.Hour), time.Now()) // expired an hour ago: kept

	var job interface{ Run() }
	for _, j := range env.app.Cron().Jobs() {
		if j.Id() == "__pbSessionCleanup__" {
			job = j
		}
	}
	if job == nil {
		t.Fatal("cleanup job not registered")
	}
	job.Run()

	for id, want := range map[string]bool{
		"expiredold000001": false, "revokedold000001": false,
		"revokedrecent001": true, "expiredrecent001": true, live: true,
	} {
		_, err := env.app.FindRecordById(core.CollectionNameSessions, id)
		if (err == nil) != want {
			t.Fatalf("session %s: kept=%v, want %v", id, err == nil, want)
		}
	}
}

func TestSessionForCustomAuthResponse(t *testing.T) {
	t.Parallel()
	env := newSessionEnv(t)

	// the superuser impersonates; the target gets a dedicated session
	su := env.mustLogin("_superusers", "test@example.com", "1234567890")

	// a reason is required
	if s := env.status("POST", "/api/collections/users/impersonate/"+sessUserId, `{}`, su); s != 400 {
		t.Fatalf("missing reason: %d", s)
	}
	// superusers cannot be impersonated
	if s := env.status("POST", "/api/collections/_superusers/impersonate/"+sessSuperId, `{"reason":"x"}`, su); s != 403 {
		t.Fatalf("superuser target: %d", s)
	}

	status, body := env.do("POST", "/api/collections/users/impersonate/"+sessUserId, `{"reason":"checking ticket 42","duration":600}`, su)
	var r sessResp
	_ = json.Unmarshal([]byte(body), &r)
	if status != 200 || r.Token == "" {
		t.Fatalf("impersonate: %d %s", status, body)
	}
	claims := claimsOf(t, r.Token)
	if claims["impersonatedBy"] != sessSuperId || claims["refreshable"] != false {
		t.Fatalf("claims: %v", claims)
	}
	s := env.session(sidOf(t, r.Token))
	if s.GetString("authMethod") != "impersonate" {
		t.Fatalf("method %q", s.GetString("authMethod"))
	}
	if d := time.Until(s.GetDateTime("expires").Time()); d > 11*time.Minute || d < 5*time.Minute {
		t.Fatalf("impersonation session lifetime %v", d)
	}

	// the token works as the user, and cannot be refreshed into a longer one
	if s := env.status("GET", "/api/sessions", "", r.Token); s != 200 {
		t.Fatalf("%d", s)
	}

	// audit entry with the impersonator, target and reason
	entries, err := env.app.FindAllRecords(core.CollectionNameAuditLog, dbx.HashExp{"action": "impersonate"})
	if err != nil || len(entries) != 1 {
		t.Fatalf("audit entries: %v %d", err, len(entries))
	}
	e := entries[0]
	if e.GetString("actorId") != sessSuperId || e.GetString("targetId") != sessUserId || e.GetString("actorType") != "superuser" {
		t.Fatalf("entry: %v", e.PublicExport())
	}
	if !strings.Contains(tests.CompactJSON(e.GetString("meta")), "checking ticket 42") {
		t.Fatalf("meta: %s", tests.CompactJSON(e.GetString("meta")))
	}
}

// -------------------------------------------------------------------

func TestBanFlow(t *testing.T) {
	t.Parallel()
	env := newSessionEnv(t)

	su := env.mustLogin("_superusers", "test@example.com", "1234567890")
	user := env.mustLogin("users", "test@example.com", "1234567890")
	userSid := sidOf(t, user)

	key, _ := env.mint("users", sessUserId, "*")
	if s := env.status("GET", "/api/health", "", key); s != 200 {
		t.Fatalf("key before the ban: %d", s)
	}
	if s := env.status("GET", "/api/collections/demo2/records", "", key); s == 401 {
		t.Fatalf("key before the ban: %d", s)
	}

	// the baseline: a wrong password
	badStatus, _, badBody := env.login("users", "test@example.com", "wrong-password")

	// non-superusers cannot ban
	if s := env.status("POST", "/api/bans", `{"collection":"users","record":"`+sessUserId+`"}`, user); s != 403 {
		t.Fatalf("non-superuser ban: %d", s)
	}

	status, body := env.do("POST", "/api/bans", `{"collection":"users","record":"`+sessUserId+`","reason":"abusive"}`, su)
	if status != 200 || !strings.Contains(body, "abusive") {
		t.Fatalf("ban: %d %s", status, body)
	}

	// the live session dies on the very next request, and so does the legacy token
	if s := env.status("GET", "/api/sessions", "", user); s != 401 {
		t.Fatalf("session after the ban: %d", s)
	}
	if r := env.session(userSid).GetString("revokedReason"); r != core.SessionRevokedBanned {
		t.Fatalf("reason %q", r)
	}
	if s := env.status("GET", "/api/sessions", "", auditUserToken); s != 401 {
		t.Fatalf("legacy token after the ban: %d", s)
	}

	// sign-in is refused with exactly the body of a wrong password
	status, _, banBody := env.login("users", "test@example.com", "1234567890")
	if status != badStatus || banBody != badBody {
		t.Fatalf("a banned sign-in differs from a failed one:\n%d %s\n%d %s", status, banBody, badStatus, badBody)
	}
	if strings.Contains(strings.ToLower(banBody), "ban") {
		t.Fatalf("the response reveals the ban: %s", banBody)
	}

	// the API key stops working
	if s := env.status("GET", "/api/collections/demo2/records", "", key); s != 401 {
		t.Fatalf("api key of a banned owner: %d", s)
	}

	// no reset emails, and no hint that the account is banned
	uc, _ := env.app.FindCollectionByNameOrId("users")
	uc.WebAuthn.Enabled = true
	if err := env.app.Save(uc); err != nil {
		t.Fatal(err)
	}
	env.app.TestMailer.Reset()
	for _, u := range []string{"/api/collections/users/request-password-reset", "/api/collections/users/request-passkey-reset"} {
		if s := env.status("POST", u, `{"email":"test@example.com"}`, ""); s != 204 {
			t.Fatalf("%s: %d", u, s)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if n := env.app.TestMailer.TotalSend(); n != 0 {
		t.Fatalf("%d reset emails were sent to a banned account", n)
	}

	// superusers can read the reason
	_, body = env.do("GET", "/api/bans?collection=users&record="+sessUserId, "", su)
	if !strings.Contains(body, "abusive") {
		t.Fatalf("%s", body)
	}
	if env.status("GET", "/api/bans", "", user) != 401 && env.status("GET", "/api/bans", "", user) != 403 {
		t.Fatal("a user can list bans")
	}

	// the ban is in the audit log
	n, _ := env.app.CountRecords(core.CollectionNameAuditLog, dbx.HashExp{"targetCollection": core.CollectionNameBans, "action": "create"})
	if n != 1 {
		t.Fatalf("ban audit entries: %d", n)
	}

	// unban restores sign-in
	ban := mustFindBan(t, env, sessUserId)
	if s := env.status("DELETE", "/api/bans/"+ban.Id, "", su); s != 204 {
		t.Fatalf("unban: %d", s)
	}
	if st, r, b := env.login("users", "test@example.com", "1234567890"); st != 200 || sidOf(t, r.Token) == "" {
		t.Fatalf("sign-in after the unban: %d %s", st, b)
	}

	// (and the reset email flows again, so the zero above was the ban's doing)
	env.status("POST", "/api/collections/users/request-password-reset", `{"email":"test@example.com"}`, "")
	time.Sleep(100 * time.Millisecond)
	if env.app.TestMailer.TotalSend() == 0 {
		t.Fatal("no reset email after the unban")
	}
}

func mustFindBan(t testing.TB, env *apiKeyEnv, userId string) *core.Record {
	t.Helper()
	b, err := env.app.FindFirstRecordByData(core.CollectionNameBans, "recordRef", userId)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBanPolicy(t *testing.T) {
	t.Parallel()
	env := newSessionEnv(t)

	su := env.mustLogin("_superusers", "test@example.com", "1234567890")

	// a superuser cannot ban themselves
	if s := env.status("POST", "/api/bans", `{"collection":"_superusers","record":"`+sessSuperId+`"}`, su); s != 403 {
		t.Fatalf("self-ban: %d", s)
	}
	// ... nor through the records API
	if s := env.status("POST", "/api/collections/_bans/records", `{"collectionRef":"pbc_3142635823","recordRef":"`+sessSuperId+`"}`, su); s != 403 {
		t.Fatalf("self-ban through the records API: %d", s)
	}

	// invalid / past expiry is rejected
	if s := env.status("POST", "/api/bans", `{"collection":"users","record":"`+sessUserId+`","until":"2000-01-01 00:00:00.000Z"}`, su); s != 400 {
		t.Fatalf("past expiry: %d", s)
	}

	// superusers can ban other superusers
	other, err := env.app.FindCollectionByNameOrId(core.CollectionNameSuperusers)
	if err != nil {
		t.Fatal(err)
	}
	second := core.NewRecord(other)
	second.SetEmail("second@example.com")
	second.SetPassword("1234567890")
	if err := env.app.Save(second); err != nil {
		t.Fatal(err)
	}
	if s := env.status("POST", "/api/bans", `{"collection":"_superusers","record":"`+second.Id+`","reason":"left"}`, su); s != 200 {
		t.Fatalf("ban another superuser: %d", s)
	}
	if st, _, _ := env.login("_superusers", "second@example.com", "1234567890"); st != 400 {
		t.Fatalf("banned superuser sign-in: %d", st)
	}
}

func TestBanExpires(t *testing.T) {
	t.Parallel()
	env := newSessionEnv(t)

	su := env.mustLogin("_superusers", "test@example.com", "1234567890")
	until := time.Now().Add(time.Hour).UTC().Format(types.DefaultDateLayout)
	if s := env.status("POST", "/api/bans", `{"collection":"users","record":"`+sessUserId+`","until":"`+until+`"}`, su); s != 200 {
		t.Fatalf("%d", s)
	}
	if st, _, _ := env.login("users", "test@example.com", "1234567890"); st != 400 {
		t.Fatalf("during the ban: %d", st)
	}

	// once "until" has passed the account can sign in again
	ban := mustFindBan(t, env, sessUserId)
	if _, err := env.app.NonconcurrentDB().NewQuery("UPDATE _bans SET until={:u} WHERE id={:id}").
		Bind(dbx.Params{"u": types.NowDateTime().Add(-time.Minute).String(), "id": ban.Id}).Execute(); err != nil {
		t.Fatal(err)
	}
	if st, _, b := env.login("users", "test@example.com", "1234567890"); st != 200 {
		t.Fatalf("after the ban expired: %d %s", st, b)
	}

	// and the cleanup removes the stale row
	if n, err := core.DeleteExpiredBans(env.app, time.Now()); err != nil || n != 1 {
		t.Fatalf("cleanup: %d %v", n, err)
	}
}

func TestSessionsDeletedWithRecord(t *testing.T) {
	t.Parallel()
	env := newSessionEnv(t)

	env.mustLogin("users", "test@example.com", "1234567890")
	user := mustFind(t, env.app, "users", sessUserId)
	if err := env.app.Delete(user); err != nil {
		// the fixture user may be referenced by other records; that is not what is tested here
		t.Skipf("cannot delete the fixture user: %v", err)
	}
	if env.sessionCount(sessUserId) != 0 {
		t.Fatal("the sessions outlived the record")
	}
}

// -------------------------------------------------------------------

// BenchmarkAuthRequestWithSession measures GET /api/health-like authenticated
// requests: no sid (legacy), sid with a warm cache, and sid with the cache
// bypassed (every request reads the session row).
func BenchmarkAuthRequest(b *testing.B) {
	env := newSessionEnv(b)
	token := env.mustLogin("users", "test@example.com", "1234567890")

	run := func(b *testing.B, tok string, bypass bool) {
		for b.Loop() {
			if bypass {
				core.InvalidateSessionCache(env.app)
			}
			if s := env.status("GET", "/api/collections/users/records/"+sessUserId, "", tok); s != 200 {
				b.Fatalf("status %d", s)
			}
		}
	}

	b.Run("legacy-no-sid", func(b *testing.B) { run(b, auditUserToken, false) })
	b.Run("sid-cached", func(b *testing.B) { run(b, token, false) })
	b.Run("sid-uncached", func(b *testing.B) { run(b, token, true) })
}
