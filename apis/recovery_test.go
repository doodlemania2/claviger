package apis_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #83): staff-vouched recovery codes.
//
// Fixture: the org fixture (alice owns P1, bob is admin of P1, carol is a
// member of P1 and admin of P2, dave is a viewer of P1, sam is suspended in
// P1, erin belongs to P2 only) with passkeys enabled on users. carol and eve
// are tagged "greeter" in P1; eve is a plain member added here.

type recoveryEnv struct {
	*ceremonyEnv
}

func newRecoveryEnv(t testing.TB) *recoveryEnv {
	t.Helper()

	env := newCeremonyEnv(t)
	seedOrgs(t, env.app)

	roles, err := env.app.FindCollectionByNameOrId(core.CollectionNameOrgRoles)
	orgMust(t, err)

	tag := core.NewRecord(roles)
	tag.Id = "taggreeterp1aaa"
	tag.Set("org", orgP1)
	tag.Set("name", core.RecoveryTag)
	orgMust(t, env.app.Save(tag))

	m, err := core.FindOrgMember(env.app, orgP1, orgUserID("carol"))
	orgMust(t, err)
	m.Set("tags", append(m.GetStringSlice("tags"), tag.Id))
	orgMust(t, env.app.Save(m))

	// a second greeter: frank joins P1 as a member with the tag
	members, _ := env.app.FindCollectionByNameOrId(core.CollectionNameOrgMembers)
	f := core.NewRecord(members)
	f.Set("org", orgP1)
	f.Set("user", orgUserID("frank"))
	f.Set("role", "member")
	f.Set("status", "active")
	f.Set("tags", []string{tag.Id})
	orgMust(t, env.app.Save(f))

	return &recoveryEnv{env}
}

// fresh is a token of the user with a passkey assertion made now.
func (env *recoveryEnv) fresh(who string) string {
	env.t.Helper()

	u, err := env.app.FindRecordById("users", orgUserID(who))
	orgMust(env.t, err)

	tok, err := u.NewPasskeyAuthToken(time.Now(), true)
	orgMust(env.t, err)

	return tok
}

func (env *recoveryEnv) plain(who string) string {
	env.t.Helper()

	u, err := env.app.FindRecordById("users", orgUserID(who))
	orgMust(env.t, err)

	tok, err := u.NewAuthToken()
	orgMust(env.t, err)

	return tok
}

func (env *recoveryEnv) issue(token, target, verification string, extra map[string]any) (int, map[string]any) {
	body := map[string]any{"user": orgUserID(target), "verification": verification}
	for k, v := range extra {
		body[k] = v
	}

	return env.do(http.MethodPost, "/api/orgs/"+orgP1+"/recovery", body, token)
}

// rawRedeem returns the status and the exact response body, for comparisons.
func (env *recoveryEnv) rawRedeem(code, ip string) (int, string) {
	b, _ := json.Marshal(map[string]any{"code": code})
	req := httptest.NewRequest(http.MethodPost, "/api/recovery/redeem", strings.NewReader(string(b)))
	req.Header.Set("content-type", "application/json")

	if ip != "" {
		req.RemoteAddr = ip + ":4444"
	}

	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)

	return rec.Code, rec.Body.String()
}

func (env *recoveryEnv) redeem(code string) (int, map[string]any) {
	return env.do(http.MethodPost, "/api/recovery/redeem", map[string]any{"code": code}, "")
}

func (env *recoveryEnv) auditRows(action string) []*core.Record {
	env.t.Helper()

	rows, err := env.app.FindAllRecords(core.CollectionNameAuditLog, dbx.HashExp{"action": action})
	orgMust(env.t, err)

	return rows
}

func TestRecoveryIssueRedeemAndEnroll(t *testing.T) {
	t.Parallel()

	env := newRecoveryEnv(t)

	status, body := env.issue(env.fresh("carol"), "dave", "known_by_sight", map[string]any{"note": "front desk"})
	if status != http.StatusCreated {
		t.Fatalf("issue: %d %v", status, body)
	}

	code, _ := body["code"].(string)
	if !regexp.MustCompile(`^[A-Z2-9]{5} [A-Z2-9]{5}$`).MatchString(code) {
		t.Fatalf("code %q is not two groups of five", code)
	}

	if body["pending"] != false || body["emailSent"] != true || body["smsSent"] != false {
		t.Fatalf("unexpected response %v", body)
	}

	// only a hash is stored, and the code is nowhere in the table
	rec, err := env.app.FindRecordById(core.CollectionNameRecoveryCodes, body["id"].(string))
	orgMust(t, err)

	plain := strings.ReplaceAll(code, " ", "")
	if rec.GetString("codeHash") == "" || strings.Contains(rec.GetString("codeHash"), plain) || len(rec.GetString("codeHash")) != 64 {
		t.Fatalf("hash %q", rec.GetString("codeHash"))
	}

	if rec.GetString("ref") != plain[:5] || rec.GetString("issuedBy") != orgUserID("carol") || rec.GetString("verification") != "known_by_sight" {
		t.Fatalf("row %v", rec.FieldsData())
	}

	// the notice reaches the person, names the time, and never carries the code
	if env.app.TestMailer.TotalSend() != 1 {
		t.Fatalf("expected 1 notice, got %d", env.app.TestMailer.TotalSend())
	}

	msg := env.app.TestMailer.FirstMessage()
	if msg.To[0].Address != "dave@orgs.test" || strings.Contains(msg.HTML, plain) || !strings.Contains(msg.HTML, "recovery code") ||
		!strings.Contains(msg.HTML, "/recovery#report=") {
		t.Fatalf("notice: %s %s", msg.To, msg.HTML)
	}

	// the person types it in lower case with a dash: it still works
	status, res := env.redeem(strings.ToLower(plain[:5]) + "-" + strings.ToLower(plain[5:]))
	if status != http.StatusOK || res["collection"] != "users" {
		t.Fatalf("redeem: %d %v", status, res)
	}

	token, _ := res["token"].(string)

	// ... and the existing passkey reset routes enroll the passkey
	auth := newSoftAuthenticator(t, "example.com")
	status, enrolled := resetFinish(t, env.ceremonyEnv, token, auth, "dave phone")
	if status != http.StatusCreated {
		t.Fatalf("enroll: %d %v", status, enrolled)
	}

	if rows := passkeyCredentialRows(t, env.ceremonyEnv, orgUserID("dave")); len(rows) != 1 {
		t.Fatalf("expected the new passkey, got %d rows", len(rows))
	}

	// single use
	if status, _ := env.redeem(plain); status != http.StatusBadRequest {
		t.Fatalf("second redeem: %d", status)
	}

	// the audit trail: issue by carol, redeem attributed to carol, with org, channel, verification
	for _, action := range []string{"recovery_issue", "recovery_redeem"} {
		rows := env.auditRows(action)
		if len(rows) != 1 {
			t.Fatalf("%s: %d entries", action, len(rows))
		}

		meta := map[string]any{}
		_ = rows[0].UnmarshalJSONField("meta", &meta)

		if rows[0].GetString("actorId") != orgUserID("carol") || meta["orgId"] != orgP1 ||
			meta["verification"] != "known_by_sight" || meta["channel"] != "display" {
			t.Fatalf("%s entry: actor %s meta %v", action, rows[0].GetString("actorId"), meta)
		}
	}

	// neither the code nor its hash appear in any audit entry
	all, _ := env.app.FindAllRecords(core.CollectionNameAuditLog)
	for _, r := range all {
		raw, _ := json.Marshal(r.FieldsData())
		if strings.Contains(string(raw), plain) || strings.Contains(string(raw), rec.GetString("codeHash")) {
			t.Fatalf("audit entry %s holds the code or the hash: %s", r.GetString("action"), raw)
		}
	}
}

func TestRecoveryIssueGuards(t *testing.T) {
	t.Parallel()

	env := newRecoveryEnv(t)
	url := "/api/orgs/" + orgP1 + "/recovery"

	// banned target: indistinguishable from a person who is not a member
	bans, err := env.app.FindCollectionByNameOrId(core.CollectionNameBans)
	orgMust(t, err)

	ban := core.NewRecord(bans)
	ban.Set("collectionRef", "_pb_users_auth_")
	ban.Set("recordRef", orgUserID("dave"))
	ban.Set("reason", "x")
	orgMust(t, env.app.Save(ban))

	banStatus, banBody := env.issue(env.fresh("carol"), "dave", "id_checked", nil)
	noStatus, noBody := env.issue(env.fresh("carol"), "erin", "id_checked", nil) // not a member of P1

	if banStatus != http.StatusNotFound || banStatus != noStatus || banBody["message"] != noBody["message"] {
		t.Fatalf("banned %d %v differs from non-member %d %v", banStatus, banBody, noStatus, noBody)
	}

	cases := []struct {
		name   string
		token  string
		target string
		verif  string
		want   int
	}{
		{"no step up", env.plain("carol"), "alice", "id_checked", http.StatusForbidden},
		{"anonymous", "", "alice", "id_checked", http.StatusUnauthorized},
		{"viewer is not staff", env.fresh("dave"), "sam", "id_checked", http.StatusForbidden},
		{"not a member of the org", env.fresh("erin"), "dave", "id_checked", http.StatusNotFound},
		{"self", env.fresh("carol"), "carol", "id_checked", http.StatusBadRequest},
		{"suspended target", env.fresh("carol"), "sam", "id_checked", http.StatusNotFound},
		{"unknown verification", env.fresh("carol"), "dave", "nope", http.StatusBadRequest},
		{"missing verification", env.fresh("carol"), "dave", "", http.StatusBadRequest},
	}

	for _, c := range cases {
		if status, body := env.issue(c.token, c.target, c.verif, nil); status != c.want {
			t.Errorf("%s: expected %d, got %d %v", c.name, c.want, status, body)
		}
	}

	// the step-up refusal carries the machine-readable code
	_, body := env.issue(env.plain("carol"), "dave", "id_checked", nil)
	if !strings.Contains(toJSON(body), "passkey_step_up_required") {
		t.Errorf("no step-up code: %v", body)
	}

	if status, _ := env.do(http.MethodPost, url, map[string]any{"user": orgUserID("dave"), "verification": "id_checked", "channel": "fax"}, env.fresh("carol")); status != http.StatusBadRequest {
		t.Errorf("unknown channel: %d", status)
	}

	if n, _ := env.app.CountRecords(core.CollectionNameRecoveryCodes); n != 0 {
		t.Fatalf("a refused request left %d rows", n)
	}
}

func TestRecoveryIssueRateLimit(t *testing.T) {
	t.Parallel()

	env := newRecoveryEnv(t)

	for i := range core.DefaultRecoveryIssuePerHour {
		if status, body := env.issue(env.fresh("carol"), "dave", "id_checked", nil); status != http.StatusCreated {
			t.Fatalf("issue %d: %d %v", i, status, body)
		}
	}

	if status, _ := env.issue(env.fresh("carol"), "dave", "id_checked", nil); status != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", status)
	}

	// the limit is per staff member
	if status, body := env.issue(env.fresh("frank"), "dave", "id_checked", nil); status != http.StatusCreated {
		t.Fatalf("another greeter: %d %v", status, body)
	}
}

func TestRecoveryNewCodeClosesTheOldOne(t *testing.T) {
	t.Parallel()

	env := newRecoveryEnv(t)

	_, first := env.issue(env.fresh("carol"), "dave", "id_checked", nil)
	_, second := env.issue(env.fresh("frank"), "dave", "id_checked", nil)

	if status, _ := env.redeem(first["code"].(string)); status != http.StatusBadRequest {
		t.Fatalf("the first code still works: %d", status)
	}

	if status, body := env.redeem(second["code"].(string)); status != http.StatusOK {
		t.Fatalf("the second code: %d %v", status, body)
	}
}

func TestRecoveryElevatedTargetNeedsTwoStaff(t *testing.T) {
	t.Parallel()

	env := newRecoveryEnv(t)
	confirm := func(token, id string) (int, map[string]any) {
		return env.do(http.MethodPost, "/api/orgs/"+orgP1+"/recovery/"+id+"/confirm", nil, token)
	}

	// bob is an admin of P1: the first greeter only creates a pending request
	status, body := env.issue(env.fresh("carol"), "bob", "id_checked", nil)
	if status != http.StatusCreated || body["pending"] != true || body["code"] != nil || body["token"] != nil {
		t.Fatalf("issue for an admin: %d %v", status, body)
	}

	id := body["id"].(string)

	// no code exists yet, so nothing can be redeemed and nobody got a notice
	if env.app.TestMailer.TotalSend() != 0 {
		t.Fatal("a notice was sent before the confirmation")
	}

	rec, _ := env.app.FindRecordById(core.CollectionNameRecoveryCodes, id)
	if rec.GetString("codeHash") != "" || rec.GetString("ref") != "" {
		t.Fatal("a pending request holds a code")
	}

	// the issuer cannot confirm her own request, the target cannot confirm, a viewer is not staff
	for name, c := range map[string]struct {
		token string
		want  int
	}{
		"the issuer":      {env.fresh("carol"), http.StatusForbidden},
		"the target":      {env.fresh("bob"), http.StatusForbidden},
		"a viewer":        {env.fresh("dave"), http.StatusForbidden},
		"no step up":      {env.plain("frank"), http.StatusForbidden},
		"outside the org": {env.fresh("erin"), http.StatusNotFound},
		"anonymous":       {"", http.StatusUnauthorized},
	} {
		if status, res := confirm(c.token, id); status != c.want {
			t.Errorf("%s: expected %d, got %d %v", name, c.want, status, res)
		}
	}

	// a different greeter confirms; the code is shown to the confirmer only now
	status, res := confirm(env.fresh("frank"), id)
	if status != http.StatusOK || res["pending"] != false {
		t.Fatalf("confirm: %d %v", status, res)
	}

	code, _ := res["code"].(string)
	if code == "" {
		t.Fatalf("no code after the confirmation: %v", res)
	}

	if status, _ := confirm(env.fresh("alice"), id); status != http.StatusConflict {
		t.Fatalf("a second confirmation: %d", status)
	}

	if env.app.TestMailer.TotalSend() != 1 || env.app.TestMailer.FirstMessage().To[0].Address != "bob@orgs.test" {
		t.Fatalf("expected one notice to bob after the confirmation, got %d", env.app.TestMailer.TotalSend())
	}

	if status, body := env.redeem(code); status != http.StatusOK {
		t.Fatalf("redeem: %d %v", status, body)
	}

	rec, _ = env.app.FindRecordById(core.CollectionNameRecoveryCodes, id)
	if rec.GetString("issuedBy") != orgUserID("carol") || rec.GetString("confirmedBy") != orgUserID("frank") {
		t.Fatalf("who vouched: %s %s", rec.GetString("issuedBy"), rec.GetString("confirmedBy"))
	}

	if len(env.auditRows("recovery_issue")) != 1 || len(env.auditRows("recovery_confirm")) != 1 || len(env.auditRows("recovery_redeem")) != 1 {
		t.Fatal("expected one issue, confirm and redeem entry")
	}

	// the same holds for an owner, and for a member who is an admin of another org
	for _, target := range []string{"alice", "carol"} {
		if status, body := env.issue(env.fresh("frank"), target, "id_checked", nil); status != http.StatusCreated || body["pending"] != true {
			t.Errorf("%s: %d %v", target, status, body)
		}
	}
}

func TestRecoveryWrongGuessesKillTheCode(t *testing.T) {
	t.Parallel()

	env := newRecoveryEnv(t)

	_, body := env.issue(env.fresh("carol"), "dave", "id_checked", nil)
	good := strings.ReplaceAll(body["code"].(string), " ", "")
	ref := good[:5]

	wrong := ref + "AAAAA"
	if wrong == good {
		wrong = ref + "BBBBB"
	}

	for i := range core.RecoveryMaxAttempts {
		if status, _ := env.redeem(wrong); status != http.StatusBadRequest {
			t.Fatalf("guess %d: %d", i, status)
		}
	}

	rec, _ := env.app.FindRecordById(core.CollectionNameRecoveryCodes, body["id"].(string))
	if rec.GetInt("attempts") != core.RecoveryMaxAttempts {
		t.Fatalf("attempts %d", rec.GetInt("attempts"))
	}

	// now even the right code is dead
	if status, _ := env.redeem(good); status != http.StatusBadRequest {
		t.Fatalf("the right code after 3 wrong guesses: %d", status)
	}

	if len(env.auditRows("recovery_code_locked")) != 1 || len(env.auditRows("recovery_redeem_failed")) < 3 {
		t.Fatalf("audit: locked %d failed %d", len(env.auditRows("recovery_code_locked")), len(env.auditRows("recovery_redeem_failed")))
	}
}

func TestRecoveryRedeemFailuresLookAlike(t *testing.T) {
	t.Parallel()

	env := newRecoveryEnv(t)

	_, a := env.issue(env.fresh("carol"), "dave", "id_checked", nil)
	good := strings.ReplaceAll(a["code"].(string), " ", "")

	// expired
	rec, _ := env.app.FindRecordById(core.CollectionNameRecoveryCodes, a["id"].(string))
	rec.Set("expires", types.NowDateTime().Add(-time.Minute))
	orgMust(t, env.app.Save(rec))

	expiredStatus, expiredBody := env.rawRedeem(good, "")
	unknownStatus, unknownBody := env.rawRedeem("ZZZZZZZZZZ", "")
	wrongStatus, wrongBody := env.rawRedeem(good[:5]+"ZZZZZ", "")
	junkStatus, junkBody := env.rawRedeem("hello", "")

	for name, got := range map[string]struct {
		status int
		body   string
	}{
		"unknown": {unknownStatus, unknownBody}, "wrong": {wrongStatus, wrongBody}, "junk": {junkStatus, junkBody},
	} {
		if got.status != expiredStatus || got.body != expiredBody {
			t.Errorf("%s answers differently from expired: %d %s vs %d %s", name, got.status, got.body, expiredStatus, expiredBody)
		}
	}

	if expiredStatus != http.StatusBadRequest {
		t.Fatalf("status %d", expiredStatus)
	}

	// a banned person: the code stops working, same answer
	_, b := env.issue(env.fresh("frank"), "dave", "id_checked", nil)

	bans, _ := env.app.FindCollectionByNameOrId(core.CollectionNameBans)
	ban := core.NewRecord(bans)
	ban.Set("collectionRef", "_pb_users_auth_")
	ban.Set("recordRef", orgUserID("dave"))
	orgMust(t, env.app.Save(ban))

	st, bd := env.rawRedeem(b["code"].(string), "")
	if st != expiredStatus || bd != expiredBody {
		t.Errorf("banned answers differently: %d %s", st, bd)
	}
}

func TestRecoveryRedeemIsRateLimitedPerIP(t *testing.T) {
	t.Parallel()

	env := newRecoveryEnv(t)

	for i := range 15 {
		if status, _ := env.rawRedeem("ZZZZZZZZZZ", "10.9.9.9"); status != http.StatusBadRequest {
			t.Fatalf("attempt %d: %d", i, status)
		}
	}

	if status, _ := env.rawRedeem("ZZZZZZZZZZ", "10.9.9.9"); status != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", status)
	}

	// another address has its own budget
	if status, _ := env.rawRedeem("ZZZZZZZZZZ", "10.8.8.8"); status != http.StatusBadRequest {
		t.Fatalf("other IP: %d", status)
	}
}

func TestRecoveryQRChannelReturnsATokenAndALink(t *testing.T) {
	t.Parallel()

	env := newRecoveryEnv(t)

	status, body := env.issue(env.fresh("carol"), "dave", "other", map[string]any{"channel": "qr"})
	if status != http.StatusCreated {
		t.Fatalf("%d %v", status, body)
	}

	token, _ := body["token"].(string)
	if len(token) != 26 || body["code"] != nil || !strings.HasSuffix(body["url"].(string), "/recovery#code="+token) {
		t.Fatalf("qr response %v", body)
	}

	if status, res := env.redeem(token); status != http.StatusOK || res["token"] == nil {
		t.Fatalf("redeem the token: %d %v", status, res)
	}
}

func TestRecoverySMSChannelSendsNothingYet(t *testing.T) {
	t.Parallel()

	env := newRecoveryEnv(t)

	status, body := env.issue(env.fresh("carol"), "dave", "id_checked", map[string]any{"channel": "sms"})
	if status != http.StatusCreated || body["smsSent"] != false || body["code"] == nil {
		t.Fatalf("%d %v", status, body)
	}
}

func TestRecoveryReportClosesTheCode(t *testing.T) {
	t.Parallel()

	env := newRecoveryEnv(t)

	_, body := env.issue(env.fresh("carol"), "dave", "id_checked", nil)
	code := body["code"].(string)

	m := regexp.MustCompile(`/recovery#report=([A-Za-z0-9_\-.]+)`).FindStringSubmatch(env.app.TestMailer.FirstMessage().HTML)
	if m == nil {
		t.Fatal("no report link in the notice")
	}

	if status, _ := env.do(http.MethodPost, "/api/recovery/report", map[string]any{"token": "garbage"}, ""); status != http.StatusBadRequest {
		t.Fatalf("garbage report: %d", status)
	}

	if status, res := env.do(http.MethodPost, "/api/recovery/report", map[string]any{"token": m[1]}, ""); status != http.StatusNoContent {
		t.Fatalf("report: %d %v", status, res)
	}

	if status, _ := env.redeem(code); status != http.StatusBadRequest {
		t.Fatalf("a reported code still works: %d", status)
	}

	rows := env.auditRows("recovery_reported")
	if len(rows) != 1 || rows[0].GetString("actorId") != orgUserID("dave") {
		t.Fatalf("report audit: %d", len(rows))
	}

	// reporting twice is harmless
	if status, _ := env.do(http.MethodPost, "/api/recovery/report", map[string]any{"token": m[1]}, ""); status != http.StatusNoContent {
		t.Fatalf("second report: %d", status)
	}
}

func TestRecoveryDemotedGreeterCodeDies(t *testing.T) {
	t.Parallel()

	env := newRecoveryEnv(t)

	_, body := env.issue(env.fresh("frank"), "dave", "id_checked", nil)

	m, _ := core.FindOrgMember(env.app, orgP1, orgUserID("frank"))
	m.Set("tags", []string{})
	orgMust(t, env.app.Save(m))

	if status, _ := env.redeem(body["code"].(string)); status != http.StatusBadRequest {
		t.Fatalf("a code vouched by someone who lost the right: %d", status)
	}
}

func TestRecoveryList(t *testing.T) {
	t.Parallel()

	env := newRecoveryEnv(t)

	_, issued := env.issue(env.fresh("carol"), "dave", "id_checked", nil)
	_, pending := env.issue(env.fresh("carol"), "bob", "id_checked", nil)

	url := "/api/orgs/" + orgP1 + "/recovery"

	status, body := env.do(http.MethodGet, url, nil, env.plain("frank"))
	if status != http.StatusOK {
		t.Fatalf("%d %v", status, body)
	}

	raw := toJSON(body)
	for _, secret := range []string{issued["code"].(string), strings.ReplaceAll(issued["code"].(string), " ", ""), "codeHash", `"ref"`} {
		if strings.Contains(raw, secret) {
			t.Fatalf("the list leaks %q: %s", secret, raw)
		}
	}

	items, _ := body["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items %v", items)
	}

	for _, it := range items {
		m := it.(map[string]any)
		if m["id"] == pending["id"] && (m["state"] != "pending" || m["canConfirm"] != true) {
			t.Errorf("pending item %v", m)
		}
	}

	if status, _ := env.do(http.MethodGet, url, nil, env.plain("dave")); status != http.StatusForbidden {
		t.Errorf("viewer list: %d", status)
	}

	if status, _ := env.do(http.MethodGet, url, nil, env.plain("erin")); status != http.StatusNotFound {
		t.Errorf("outsider list: %d", status)
	}
}

// the collection is closed to the records API for everyone but superusers
func TestRecoveryCollectionRulesAreClosed(t *testing.T) {
	t.Parallel()

	env := newRecoveryEnv(t)
	col, err := env.app.FindCollectionByNameOrId(core.CollectionNameRecoveryCodes)
	orgMust(t, err)

	for name, r := range map[string]*string{"list": col.ListRule, "view": col.ViewRule, "create": col.CreateRule, "update": col.UpdateRule, "delete": col.DeleteRule} {
		if r != nil {
			t.Errorf("%s rule is %q, expected null", name, *r)
		}
	}

	_, issued := env.issue(env.fresh("carol"), "dave", "id_checked", nil)

	status, body := env.do(http.MethodGet, "/api/collections/_recoveryCodes/records", nil, env.plain("dave"))
	if status == http.StatusOK && len(body["items"].([]any)) != 0 {
		t.Fatalf("a user lists the codes: %v", body)
	}

	status, body = env.do(http.MethodGet, "/api/collections/_recoveryCodes/records", nil, auditSuperuserToken)
	// (hidden fields are visible to superusers, who can read the database anyway)
	if status != http.StatusOK || !strings.Contains(toJSON(body), issued["id"].(string)) {
		t.Fatalf("superuser list: %d %v", status, body)
	}
}
