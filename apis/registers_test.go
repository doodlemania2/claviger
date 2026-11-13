package apis_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #87): the register routes. Fixture: the org fixture
// with passkeys enabled. carol and frank are registrars of P1, bob is an
// admin of P1, dave is a viewer of P1, erin belongs to P2 only.

type registerEnv struct {
	*ceremonyEnv
	cfg *core.PermanentConfig
}

func newRegisterEnv(t testing.TB, signers int) *registerEnv {
	t.Helper()

	env := newCeremonyEnv(t)
	seedOrgs(t, env.app)

	roles, err := env.app.FindCollectionByNameOrId(core.CollectionNameOrgRoles)
	orgMust(t, err)

	tag := core.NewRecord(roles)
	tag.Id = "tagregistrarp1b"
	tag.Set("org", orgP1)
	tag.Set("name", core.RegistrarTag)
	orgMust(t, env.app.Save(tag))

	members, _ := env.app.FindCollectionByNameOrId(core.CollectionNameOrgMembers)

	m, err := core.FindOrgMember(env.app, orgP1, orgUserID("carol"))
	orgMust(t, err)
	m.Set("tags", append(m.GetStringSlice("tags"), tag.Id))
	orgMust(t, env.app.Save(m))

	f := core.NewRecord(members)
	f.Set("org", orgP1)
	f.Set("user", orgUserID("frank"))
	f.Set("role", "member")
	f.Set("status", "active")
	f.Set("tags", []string{tag.Id})
	orgMust(t, env.app.Save(f))

	cfg, err := core.CreateSacramentalRegister(env.app, core.SacramentalRegisterOpts{Name: "baptisms", Signers: signers})
	orgMust(t, err)

	return &registerEnv{ceremonyEnv: env, cfg: cfg}
}

func (env *registerEnv) fresh(who string) string {
	env.t.Helper()

	u, err := env.app.FindRecordById("users", orgUserID(who))
	orgMust(env.t, err)

	tok, err := u.NewPasskeyAuthToken(time.Now(), true)
	orgMust(env.t, err)

	return tok
}

func (env *registerEnv) plain(who string) string {
	env.t.Helper()

	u, err := env.app.FindRecordById("users", orgUserID(who))
	orgMust(env.t, err)

	tok, err := u.NewAuthToken()
	orgMust(env.t, err)

	return tok
}

func (env *registerEnv) draft(surname string) string {
	env.t.Helper()

	col, _ := env.app.FindCollectionByNameOrId("baptisms")
	r := core.NewRecord(col)
	r.Set("org", orgP1)
	r.Set("sacrament", "baptism")
	r.Set("surname", surname)
	orgMust(env.t, env.app.Save(r))

	return r.Id
}

func (env *registerEnv) raw(method, url string, body any, token string) *httptest.ResponseRecorder {
	env.t.Helper()

	var rd *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	} else {
		rd = strings.NewReader("")
	}

	req := httptest.NewRequest(method, url, rd)
	req.Header.Set("content-type", "application/json")

	if token != "" {
		req.Header.Set("Authorization", token)
	}

	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)

	return rec
}

func (env *registerEnv) finalize(token, id string) (int, map[string]any) {
	return env.do(http.MethodPost, "/api/registers/baptisms/"+id+"/finalize", nil, token)
}

func TestRegisterApiFinalizeNeedsStepUpAndLocksTheEntry(t *testing.T) {
	t.Parallel()

	env := newRegisterEnv(t, 1)
	id := env.draft("Smith")
	base := "/api/collections/baptisms/records/" + id

	// a draft is edited freely through the records API
	if status, body := env.do(http.MethodPatch, base, map[string]any{"givenNames": "Ann"}, env.plain("carol")); status != http.StatusOK {
		t.Fatalf("editing a draft: %d %v", status, body)
	}

	// no fresh passkey, no auth, a viewer, an outsider
	status, body := env.finalize(env.plain("carol"), id)
	if status != http.StatusForbidden || !strings.Contains(toJSON(body), "passkey_step_up_required") {
		t.Fatalf("without step-up: %d %v", status, body)
	}

	if status, _ := env.finalize("", id); status != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", status)
	}

	if status, body := env.finalize(auditSuperuserToken, id); status != http.StatusForbidden || !strings.Contains(fmt.Sprint(body), "Superusers cannot sign") {
		t.Fatalf("superuser requesting: %d %v", status, body)
	}

	for who, want := range map[string]int{"dave": 403, "bob": 403, "erin": 404} {
		if status, body := env.finalize(env.fresh(who), id); status != want {
			t.Errorf("%s: want %d, got %d %v", who, want, status, body)
		}
	}

	status, body = env.finalize(env.fresh("carol"), id)
	if status != http.StatusOK || body["final"] != true || body["state"] != "final" {
		t.Fatalf("finalize: %d %v", status, body)
	}

	// now the records API cannot change or delete it
	if status, body := env.do(http.MethodPatch, base, map[string]any{"givenNames": "Anna"}, env.plain("carol")); status != http.StatusBadRequest {
		t.Fatalf("editing a final entry: %d %v", status, body)
	}

	if status, body := env.do(http.MethodPatch, base, map[string]any{"finalizedAt": ""}, env.plain("carol")); status != http.StatusBadRequest {
		t.Fatalf("clearing finalizedAt: %d %v", status, body)
	}

	if status, body := env.do(http.MethodDelete, base, nil, env.plain("carol")); status != http.StatusBadRequest {
		t.Fatalf("deleting a final entry: %d %v", status, body)
	}

	if status, body := env.do(http.MethodPatch, base, map[string]any{"restricted": true}, env.plain("carol")); status != http.StatusOK {
		t.Fatalf("restricting a final entry: %d %v", status, body)
	}

	// not even a superuser
	if status, _ := env.do(http.MethodPatch, base, map[string]any{"surname": "X"}, auditSuperuserToken); status != http.StatusBadRequest {
		t.Fatalf("superuser edit: %d", status)
	}

	if status, _ := env.do(http.MethodDelete, base, nil, auditSuperuserToken); status != http.StatusBadRequest {
		t.Fatalf("superuser delete: %d", status)
	}
}

func TestRegisterApiTwoSigners(t *testing.T) {
	t.Parallel()

	env := newRegisterEnv(t, 2)
	id := env.draft("Smith")

	status, body := env.finalize(env.fresh("carol"), id)
	if status != http.StatusOK || body["pending"] != true || body["state"] != "pending" {
		t.Fatalf("request: %d %v", status, body)
	}

	if status, _ := env.finalize(env.fresh("carol"), id); status != http.StatusConflict {
		t.Fatalf("the same person confirming: %d", status)
	}

	// a superuser is refused at the request, and an org owner without the tag at the confirmation
	if status, body := env.finalize(auditSuperuserToken, id); status != http.StatusForbidden || !strings.Contains(fmt.Sprint(body), "Superusers cannot sign") {
		t.Fatalf("superuser confirming: %d %v", status, body)
	}

	if status, _ := env.finalize(env.fresh("alice"), id); status != http.StatusForbidden {
		t.Fatalf("owner without the tag confirming: %d", status)
	}

	// still editable
	if status, body := env.do(http.MethodPatch, "/api/collections/baptisms/records/"+id, map[string]any{"officiant": "Fr. B"}, env.plain("carol")); status != http.StatusOK {
		t.Fatalf("editing a pending entry: %d %v", status, body)
	}

	// edited after the request: the confirmation has to be asked for again
	if status, _ := env.finalize(env.fresh("frank"), id); status != http.StatusConflict {
		t.Fatalf("confirming a changed entry: %d", status)
	}

	if status, body := env.do(http.MethodDelete, "/api/registers/baptisms/"+id+"/finalize", nil, env.plain("carol")); status != http.StatusOK || body["state"] != "draft" {
		t.Fatalf("withdraw: %d %v", status, body)
	}

	env.finalize(env.fresh("carol"), id)

	status, body = env.finalize(env.fresh("frank"), id)
	if status != http.StatusOK || body["final"] != true {
		t.Fatalf("confirm: %d %v", status, body)
	}

	rec, err := env.app.FindRecordById("baptisms", id)
	orgMust(t, err)

	if rec.GetString("finalizedBy") != orgUserID("carol") || rec.GetString("secondSigner") != orgUserID("frank") {
		t.Fatalf("signers: %q %q", rec.GetString("finalizedBy"), rec.GetString("secondSigner"))
	}
}

func TestRegisterApiAnnotationsAndView(t *testing.T) {
	t.Parallel()

	env := newRegisterEnv(t, 1)
	id := env.draft("Smith")
	url := "/api/registers/baptisms/" + id

	note := map[string]any{"kind": "correction", "text": "Surname is Smyth", "reason": "Parish record seen"}

	if status, _ := env.do(http.MethodPost, url+"/annotations", note, env.fresh("carol")); status != http.StatusConflict {
		t.Fatalf("annotating a draft: %d", status)
	}

	env.finalize(env.fresh("carol"), id)

	if status, _ := env.do(http.MethodPost, url+"/annotations", note, env.plain("carol")); status != http.StatusForbidden {
		t.Fatalf("annotating without step-up: %d", status)
	}

	if status, body := env.do(http.MethodPost, url+"/annotations", note, auditSuperuserToken); status != http.StatusForbidden || !strings.Contains(fmt.Sprint(body), "Superusers cannot sign or annotate") {
		t.Fatalf("superuser annotating: %d %v", status, body)
	}

	for who, want := range map[string]int{"dave": 403, "bob": 403, "erin": 404} {
		if status, _ := env.do(http.MethodPost, url+"/annotations", note, env.fresh(who)); status != want {
			t.Errorf("%s annotating: want %d got %d", who, want, status)
		}
	}

	status, first := env.do(http.MethodPost, url+"/annotations", note, env.fresh("carol"))
	if status != http.StatusCreated || first["author"] != orgUserID("carol") {
		t.Fatalf("annotate: %d %v", status, first)
	}

	if status, body := env.do(http.MethodPost, url+"/annotations", map[string]any{"kind": "correction", "text": "x"}, env.fresh("carol")); status != http.StatusBadRequest {
		t.Fatalf("annotation without a reason: %d %v", status, body)
	}

	status, _ = env.do(http.MethodPost, url+"/annotations", map[string]any{
		"kind": "retraction", "text": "Mistaken", "reason": "Wrong book", "supersedes": first["id"],
	}, env.fresh("frank"))
	if status != http.StatusCreated {
		t.Fatalf("retraction: %d", status)
	}

	// the view: entry with annotations, oldest first
	status, view := env.do(http.MethodGet, url, nil, env.plain("bob"))
	if status != http.StatusOK || view["state"] != "final" {
		t.Fatalf("view: %d %v", status, view)
	}

	items, _ := view["annotations"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["id"] != first["id"] || items[1].(map[string]any)["kind"] != "retraction" {
		t.Fatalf("annotations: %v", items)
	}

	for who, want := range map[string]int{"dave": 403, "erin": 404} {
		if status, _ := env.do(http.MethodGet, url, nil, env.plain(who)); status != want {
			t.Errorf("%s view: want %d got %d", who, want, status)
		}
	}

	if status, _ := env.do(http.MethodGet, url, nil, ""); status != http.StatusUnauthorized {
		t.Fatalf("anonymous view: %d", status)
	}

	// annotations are not writable through the records API, not even by a superuser
	ann := "/api/collections/baptisms_annotations/records"
	if status, _ := env.do(http.MethodPost, ann, map[string]any{"entry": id, "kind": "correction", "text": "x", "reason": "y"}, env.plain("carol")); status != http.StatusBadRequest && status != http.StatusForbidden {
		t.Fatalf("member creating an annotation through the records API: %d", status)
	}

	if status, _ := env.do(http.MethodPatch, ann+"/"+first["id"].(string), map[string]any{"text": "edited"}, auditSuperuserToken); status != http.StatusBadRequest {
		t.Fatalf("superuser editing an annotation: %d", status)
	}

	if status, _ := env.do(http.MethodDelete, ann+"/"+first["id"].(string), nil, auditSuperuserToken); status != http.StatusBadRequest {
		t.Fatalf("superuser deleting an annotation: %d", status)
	}
}

func TestRegisterApiCertificate(t *testing.T) {
	t.Parallel()

	env := newRegisterEnv(t, 1)
	id := env.draft("Smith <b>")
	url := "/api/registers/baptisms/" + id + "/certificate"

	if rec := env.raw(http.MethodGet, url, nil, env.plain("carol")); rec.Code != http.StatusConflict {
		t.Fatalf("certificate of a draft: %d", rec.Code)
	}

	env.finalize(env.fresh("carol"), id)

	env.do(http.MethodPost, "/api/registers/baptisms/"+id+"/annotations", map[string]any{
		"kind": "later_event", "text": "Married in 2031", "reason": "Notice received",
	}, env.fresh("carol"))

	rec := env.raw(http.MethodGet, url, nil, env.plain("bob"))
	if rec.Code != http.StatusOK {
		t.Fatalf("certificate: %d %s", rec.Code, rec.Body.String())
	}

	page := rec.Body.String()

	for _, want := range []string{"Smith &lt;b&gt;", "Married in 2031", "Notice received", "Parish One", "Later event", "baptisms", "<code>"} {
		if !strings.Contains(page, want) {
			t.Errorf("the certificate lacks %q", want)
		}
	}

	if strings.Contains(page, "<b>") {
		t.Error("field values must be escaped")
	}

	stored, _ := env.app.FindRecordById("baptisms", id)
	if h := stored.GetString("hash"); h == "" || !strings.Contains(page, h) {
		t.Error("the certificate should show the chain hash")
	}

	// CSP-safe: nothing inline, strict policy, one stylesheet of our own
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "script-src") {
		t.Errorf("unexpected CSP %q", csp)
	}

	for _, bad := range []string{"<script", "style=", "onclick=", "<style"} {
		if strings.Contains(page, bad) {
			t.Errorf("the page contains %q", bad)
		}
	}

	css := env.raw(http.MethodGet, "/orgs/assets/certificate.css", nil, "")
	if css.Code != http.StatusOK || !strings.Contains(css.Body.String(), "@media print") {
		t.Errorf("print stylesheet: %d", css.Code)
	}

	// who may issue
	for who, want := range map[string]int{"dave": 403, "erin": 404} {
		if rec := env.raw(http.MethodGet, url, nil, env.plain(who)); rec.Code != want {
			t.Errorf("%s certificate: want %d got %d", who, want, rec.Code)
		}
	}

	if rec := env.raw(http.MethodGet, url, nil, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous certificate: %d", rec.Code)
	}

	// one audit entry for each certificate issued (bob's only; refusals write none)
	rows, err := env.app.FindAllRecords(core.CollectionNameAuditLog, dbx.HashExp{"action": core.AuditActionRegisterCertificate})
	orgMust(t, err)

	if len(rows) != 1 || rows[0].GetString("actorId") != orgUserID("bob") || !strings.Contains(rows[0].GetString("meta"), orgP1) {
		t.Fatalf("certificate audit: %d rows", len(rows))
	}

	env.raw(http.MethodGet, url, nil, env.plain("carol"))

	rows, _ = env.app.FindAllRecords(core.CollectionNameAuditLog, dbx.HashExp{"action": core.AuditActionRegisterCertificate})
	if len(rows) != 2 {
		t.Fatalf("a second issue should write a second entry, got %d", len(rows))
	}
}

func TestRegisterApiRefusesAPIKeys(t *testing.T) {
	t.Parallel()

	env := newRegisterEnv(t, 1)
	id := env.draft("Smith")

	status, _ := env.do(http.MethodGet, "/api/registers/baptisms/"+id, nil, "clv_nonexistent")
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		t.Fatalf("a bogus key: %d", status)
	}
}

func TestRegisterApiCreateFromTemplate(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	seedOrgs(t, env.app)

	body := map[string]any{"template": "sacramental", "name": "marriages", "signers": 2}

	if status, _ := env.do(http.MethodPost, "/api/registers", body, ""); status != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", status)
	}

	user, _ := env.app.FindRecordById("users", orgUserID("alice"))
	tok, _ := user.NewAuthToken()

	if status, _ := env.do(http.MethodPost, "/api/registers", body, tok); status != http.StatusForbidden {
		t.Fatalf("a user: %d", status)
	}

	status, out := env.do(http.MethodPost, "/api/registers", body, auditSuperuserToken)
	if status != http.StatusCreated || out["annotationCollection"] != "marriages_annotations" || out["chain"] != true || out["signers"] != float64(2) {
		t.Fatalf("create: %d %v", status, out)
	}

	if status, _ := env.do(http.MethodPost, "/api/registers", body, auditSuperuserToken); status != http.StatusConflict {
		t.Fatalf("duplicate: %d", status)
	}

	for _, b := range []map[string]any{
		{"template": "nope", "name": "x"},
		{"template": "sacramental", "name": "bad name"},
		{"template": "sacramental", "name": "ok", "signers": 5},
	} {
		if status, _ := env.do(http.MethodPost, "/api/registers", b, auditSuperuserToken); status != http.StatusBadRequest {
			t.Errorf("%v: %d", b, status)
		}
	}
}
