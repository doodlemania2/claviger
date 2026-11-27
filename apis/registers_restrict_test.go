package apis_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #88, 88-7): restriction of processing and anonymization
// of register entries through the routes. Fixture: see registers_test.go.

func (env *registerEnv) finalDraft(surname string) string {
	env.t.Helper()

	id := env.draft(surname)
	if status, body := env.finalize(env.fresh("carol"), id); status != http.StatusOK {
		env.t.Fatalf("finalize: %d %v", status, body)
	}

	return id
}

func TestRegisterApiRestrictionRoutes(t *testing.T) {
	t.Parallel()

	env := newRegisterEnv(t, 1)
	id := env.finalDraft("Smith")
	draft := env.draft("Draft")
	base := "/api/registers/baptisms/"
	reason := map[string]any{"reason": "The person objects to the accuracy"}

	type sc struct {
		name   string
		path   string
		token  string
		body   map[string]any
		status int
	}

	for _, c := range []sc{
		{"anonymous", id + "/restrict", "", reason, 401},
		{"no step-up", id + "/restrict", env.plain("carol"), reason, 403},
		{"superuser", id + "/restrict", auditSuperuserToken, reason, 403},
		{"admin is not a registrar", id + "/restrict", env.fresh("bob"), reason, 403},
		{"viewer", id + "/restrict", env.fresh("dave"), reason, 403},
		{"other org", id + "/restrict", env.fresh("erin"), reason, 404},
		{"reason required", id + "/restrict", env.fresh("carol"), map[string]any{}, 400},
		{"a draft", draft + "/restrict", env.fresh("carol"), reason, 409},
		{"lift a restriction that is not set", id + "/unrestrict", env.fresh("carol"), reason, 409},
		{"restrict", id + "/restrict", env.fresh("carol"), reason, 200},
		{"restrict twice", id + "/restrict", env.fresh("frank"), reason, 409},
		{"admin cannot lift", id + "/unrestrict", env.fresh("bob"), reason, 403},
		{"lift needs a reason", id + "/unrestrict", env.fresh("frank"), map[string]any{}, 400},
		{"lift", id + "/unrestrict", env.fresh("frank"), reason, 200},
		{"restrict again after a lift", id + "/restrict", env.fresh("carol"), reason, 200},
	} {
		if status, body := env.do(http.MethodPost, base+c.path, c.body, c.token); status != c.status {
			t.Errorf("%s: want %d, got %d %v", c.name, c.status, status, body)
		}
	}

	// three notes: restrict, lift, restrict; the audit log has them all
	for action, want := range map[string]int{core.AuditActionRegisterRestrict: 2, core.AuditActionRegisterUnrestrict: 1} {
		rows, err := env.app.FindAllRecords(core.CollectionNameAuditLog, dbx.HashExp{"action": action})
		orgMust(t, err)

		if len(rows) != want {
			t.Errorf("%s: %d audit entries, want %d", action, len(rows), want)
		}
	}

	if res, err := core.VerifyRegisters(env.app); err != nil || !res[0].OK() {
		t.Fatalf("the chain holds: %v %+v", err, res)
	}
}

func TestRegisterApiRestrictedEntryIsHidden(t *testing.T) {
	t.Parallel()

	env := newRegisterEnv(t, 1)
	id := env.finalDraft("Smith")
	other := env.finalDraft("Jones")
	url := "/api/registers/baptisms/" + id

	if status, body := env.do(http.MethodPost, url+"/restrict", map[string]any{"reason": "Art 18 request"}, env.fresh("carol")); status != 200 {
		t.Fatalf("restrict: %d %v", status, body)
	}

	// the records API redacts it for everyone but a superuser
	for _, who := range []string{"carol", "bob"} {
		status, body := env.do(http.MethodGet, "/api/collections/baptisms/records/"+id, nil, env.plain(who))
		if status != http.StatusOK || body["surname"] != nil || body["restricted"] != true || body["id"] != id {
			t.Errorf("%s records view: %d %v", who, status, body)
		}
	}

	if status, body := env.do(http.MethodGet, "/api/collections/baptisms/records/"+other, nil, env.plain("carol")); status != 200 || body["surname"] != "Jones" {
		t.Fatalf("an unrestricted entry is shown: %d %v", status, body)
	}

	if status, body := env.do(http.MethodGet, "/api/collections/baptisms/records/"+id, nil, auditSuperuserToken); status != 200 || body["surname"] != "Smith" {
		t.Fatalf("the superuser's admin UI sees it: %d %v", status, body)
	}

	// the list shows the entry without its content
	status, items := env.do(http.MethodGet, "/api/registers/baptisms?org="+orgP1+"&state=final", nil, env.plain("carol"))
	if status != 200 {
		t.Fatalf("list: %d", status)
	}

	for _, it := range items["items"].([]any) {
		m := it.(map[string]any)
		rec := m["record"].(map[string]any)

		if m["id"] == id && (m["redacted"] != true || rec["surname"] != nil || rec["hash"] != nil) {
			t.Errorf("list leaks the restricted entry: %v", m)
		}

		if m["id"] == other && rec["surname"] != "Jones" {
			t.Errorf("list hides an unrestricted entry: %v", m)
		}
	}

	// the view: redacted without a reason, and for anyone who cannot reveal it
	type sc struct {
		name     string
		query    string
		token    string
		revealed bool
	}

	for _, c := range []sc{
		{"registrar without a reason", "", env.plain("carol"), false},
		{"registrar with a blank reason", "?reason=%20", env.plain("carol"), false},
		{"admin with a reason", "?reason=curious", env.plain("bob"), false},
		{"superuser without a reason", "", auditSuperuserToken, false},
		{"registrar with a reason", "?reason=Certificate+for+the+person", env.plain("carol"), true},
		{"superuser with a reason", "?reason=Support+case+12", auditSuperuserToken, true},
	} {
		status, body := env.do(http.MethodGet, url+c.query, nil, c.token)
		if status != 200 {
			t.Errorf("%s: %d %v", c.name, status, body)

			continue
		}

		rec, _ := body["record"].(map[string]any)
		got := rec["surname"] == "Smith"
		notes, _ := body["annotations"].([]any)

		if got != c.revealed || (body["redacted"] == true) == c.revealed || (c.revealed && len(notes) == 0) || (!c.revealed && len(notes) != 0) {
			t.Errorf("%s: revealed=%v, got %v", c.name, got, body)
		}
	}

	rows, err := env.app.FindAllRecords(core.CollectionNameAuditLog, dbx.HashExp{"action": core.AuditActionRegisterRestrictedRead})
	orgMust(t, err)

	if len(rows) != 2 || !strings.Contains(rows[0].GetString("meta"), "reason") {
		t.Fatalf("revealing is audited with the reason: %d rows", len(rows))
	}

	// the certificate needs the same reason
	if status, _ := env.do(http.MethodGet, url+"/certificate", nil, env.plain("carol")); status != http.StatusForbidden {
		t.Fatalf("certificate of a restricted entry without a reason: %d", status)
	}

	if rec := env.raw(http.MethodGet, url+"/certificate?reason=Person+asked+for+it", nil, env.plain("carol")); rec.Code != 200 || !strings.Contains(rec.Body.String(), "Smith") {
		t.Fatalf("certificate with a reason: %d", rec.Code)
	}

	// lifting makes it readable again
	if status, body := env.do(http.MethodPost, url+"/unrestrict", map[string]any{"reason": "Resolved"}, env.fresh("carol")); status != 200 {
		t.Fatalf("unrestrict: %d %v", status, body)
	}

	if status, body := env.do(http.MethodGet, "/api/collections/baptisms/records/"+id, nil, env.plain("bob")); status != 200 || body["surname"] != "Smith" {
		t.Fatalf("lifted: %d %v", status, body)
	}
}

func TestRegisterApiErasureRestrictsFinalEntries(t *testing.T) {
	t.Parallel()

	env := newRegisterEnv(t, 1)
	id := env.finalDraft("Smith")

	e, err := env.app.FindRecordById("baptisms", id)
	orgMust(t, err)

	if _, err := core.AnnotateErasureRequest(env.app, context.Background(), e, "Data subject erasure request"); err != nil {
		t.Fatal(err)
	}

	status, body := env.do(http.MethodGet, "/api/registers/baptisms/"+id, nil, env.plain("bob"))
	if status != 200 || body["redacted"] != true || body["restricted"] != true {
		t.Fatalf("an erased-and-retained entry is restricted: %d %v", status, body)
	}

	// and a registrar can still lift it, with a note
	if status, body := env.do(http.MethodPost, "/api/registers/baptisms/"+id+"/unrestrict", map[string]any{"reason": "Request withdrawn"}, env.fresh("carol")); status != 200 {
		t.Fatalf("lift: %d %v", status, body)
	}
}

func TestRegisterApiAnonymization(t *testing.T) {
	t.Parallel()

	env := newRegisterEnv(t, 1)

	// the default register has it off
	id := env.finalDraft("Smith")
	if status, body := env.do(http.MethodPost, "/api/registers/baptisms/"+id+"/anonymize", map[string]any{"reason": "r"}, env.fresh("carol")); status != 409 {
		t.Fatalf("off by default: %d %v", status, body)
	}

	status, body := env.do(http.MethodPost, "/api/registers", map[string]any{
		"template": "sacramental", "name": "marriages", "anonymize": true, "anonymizeAfterDays": 30,
	}, auditSuperuserToken)
	if status != http.StatusCreated {
		t.Fatalf("create: %d %v", status, body)
	}

	if status, _ := env.do(http.MethodPost, "/api/registers", map[string]any{"template": "sacramental", "name": "deaths", "anonymize": true}, auditSuperuserToken); status != http.StatusBadRequest {
		t.Fatalf("anonymization needs a period: %d", status)
	}

	col, _ := env.app.FindCollectionByNameOrId("marriages")

	var ids []string

	for _, name := range []string{"Smith", "Jones", "Brown"} {
		r := core.NewRecord(col)
		r.Set("org", orgP1)
		r.Set("sacrament", "marriage")
		r.Set("surname", name)
		r.Set("givenNames", "Ann")
		orgMust(t, env.app.Save(r))

		if status, body := env.do(http.MethodPost, "/api/registers/marriages/"+r.Id+"/finalize", nil, env.fresh("carol")); status != 200 {
			t.Fatalf("finalize: %d %v", status, body)
		}

		ids = append(ids, r.Id)
	}

	url := "/api/registers/marriages/" + ids[1] + "/anonymize"
	ok := map[string]any{"reason": "Retention period over"}

	if status, body := env.do(http.MethodPost, url, ok, env.fresh("carol")); status != 409 || !strings.Contains(fmt.Sprint(body), "retention period") {
		t.Fatalf("not due: %d %v", status, body)
	}

	later := time.Now().AddDate(0, 0, 31)
	defer core.SetRegisterClock(func() time.Time { return later })()

	for _, c := range []struct {
		name, token string
		status      int
	}{
		{"no step-up", env.plain("carol"), 403},
		{"superuser", auditSuperuserToken, 403},
		{"admin", env.fresh("bob"), 403},
		{"other org", env.fresh("erin"), 404},
	} {
		if status, _ := env.do(http.MethodPost, url, ok, c.token); status != c.status {
			t.Errorf("%s: %d, want %d", c.name, status, c.status)
		}
	}

	if status, _ := env.do(http.MethodPost, url, map[string]any{}, env.fresh("carol")); status != 400 {
		t.Fatalf("reason required: %d", status)
	}

	status, body = env.do(http.MethodPost, url, ok, env.fresh("carol"))
	if status != 200 || body["anonymized"] != true {
		t.Fatalf("anonymize: %d %v", status, body)
	}

	rec, _ := body["record"].(map[string]any)
	if s, _ := rec["surname"].(string); !strings.HasPrefix(s, "anon-") {
		t.Fatalf("pseudonym expected: %v", rec)
	}

	// the batch route does the remaining two
	status, body = env.do(http.MethodPost, "/api/registers/marriages/anonymize", map[string]any{"org": orgP1, "reason": "Retention"}, env.fresh("frank"))
	if status != 200 || body["count"] != float64(2) {
		t.Fatalf("batch: %d %v", status, body)
	}

	if status, _ := env.do(http.MethodPost, "/api/registers/marriages/anonymize", map[string]any{"org": orgP1}, env.fresh("frank")); status != 400 {
		t.Fatalf("batch needs a reason: %d", status)
	}

	for _, v := range mustVerifyAll(t, env) {
		if !v.OK() {
			t.Fatalf("verification after anonymization: %+v", v)
		}
	}
}

func mustVerifyAll(t testing.TB, env *registerEnv) []*core.RegisterVerification {
	t.Helper()

	res, err := core.VerifyRegisters(env.app)
	orgMust(t, err)

	return res
}
