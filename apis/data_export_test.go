package apis_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #88, 88-1): the personal data export routes.

type exportEnv struct {
	*apiKeyEnv
	user, other *core.Record
	userTok     string
	otherTok    string
	adminTok    string
}

func newExportEnv(t *testing.T) *exportEnv {
	t.Helper()
	env := &exportEnv{apiKeyEnv: newAPIKeyEnv(t)}

	users, _ := env.app.FindCollectionByNameOrId("users")
	mk := func(email string) *core.Record {
		u := core.NewRecord(users)
		u.SetEmail(email)
		u.SetPassword("1234567890")
		u.Set("name", "name of "+email)
		if err := env.app.Save(u); err != nil {
			t.Fatal(err)
		}
		return u
	}
	env.user, env.other = mk("export-a@example.com"), mk("export-b@example.com")
	env.userTok, _ = env.user.NewAuthToken()
	env.otherTok, _ = env.other.NewAuthToken()

	admin, err := env.app.FindAuthRecordByEmail(core.CollectionNameSuperusers, "test@example.com")
	if err != nil {
		t.Fatal(err)
	}
	env.adminTok, _ = admin.NewAuthToken()

	return env
}

func (env *exportEnv) waitDone(id, tok string) map[string]any {
	env.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		code, body := env.do(http.MethodGet, "/api/gdpr/exports/"+id, "", tok)
		if code != http.StatusOK {
			env.t.Fatalf("status %d %s", code, body)
		}
		var v map[string]any
		_ = json.Unmarshal([]byte(body), &v)
		switch v["state"] {
		case core.DataRequestStateDone, core.DataRequestStatePartial, core.DataRequestStateFailed:
			return v
		}
		if time.Now().After(deadline) {
			env.t.Fatalf("export did not finish: %s", body)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (env *exportEnv) get(url, auth string) (int, []byte, http.Header) {
	req := httptest.NewRequest(http.MethodGet, url, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes(), rec.Header()
}

func TestSelfServiceExportFlow(t *testing.T) {
	env := newExportEnv(t)

	if code, _ := env.do(http.MethodPost, "/api/me/export", `{}`, ""); code != http.StatusUnauthorized && code != http.StatusForbidden {
		t.Fatalf("anonymous: %d", code)
	}

	// step-up: without a passkey the password is required
	if code, body := env.do(http.MethodPost, "/api/me/export", `{}`, env.userTok); code != http.StatusBadRequest || !strings.Contains(body, "password") {
		t.Fatalf("no password: %d %s", code, body)
	}
	if code, _ := env.do(http.MethodPost, "/api/me/export", `{"password":"wrong-password"}`, env.userTok); code != http.StatusBadRequest {
		t.Fatalf("wrong password: %d", code)
	}

	// API keys never request exports
	key, _ := env.mint("users", env.user.Id, "*")
	if code, _ := env.do(http.MethodPost, "/api/me/export", `{"password":"1234567890"}`, key); code != http.StatusForbidden {
		t.Fatalf("api key: %d", code)
	}

	code, body := env.do(http.MethodPost, "/api/me/export", `{"password":"1234567890"}`, env.userTok)
	if code != http.StatusAccepted {
		t.Fatalf("request: %d %s", code, body)
	}
	var created map[string]any
	_ = json.Unmarshal([]byte(body), &created)
	id, _ := created["id"].(string)
	if id == "" || created["selfService"] != true || created["subjectId"] != env.user.Id {
		t.Fatalf("unexpected view %s", body)
	}

	done := env.waitDone(id, env.userTok)
	if done["state"] != core.DataRequestStateDone || done["downloadable"] != true {
		t.Fatalf("not done: %v", done)
	}

	// somebody else cannot see it
	if code, _ := env.do(http.MethodGet, "/api/gdpr/exports/"+id, "", env.otherTok); code != http.StatusNotFound {
		t.Fatalf("other user status: %d", code)
	}
	if code, _, _ := env.get("/api/gdpr/exports/"+id+"/download", env.otherTok); code != http.StatusNotFound {
		t.Fatalf("other user download: %d", code)
	}
	if code, _, _ := env.get("/api/gdpr/exports/"+id+"/download", ""); code != http.StatusNotFound {
		t.Fatalf("anonymous download: %d", code)
	}

	// own list
	code, body = env.do(http.MethodGet, "/api/me/exports", "", env.userTok)
	if code != http.StatusOK || !strings.Contains(body, id) {
		t.Fatalf("list: %d %s", code, body)
	}
	if _, body := env.do(http.MethodGet, "/api/me/exports", "", env.otherTok); strings.Contains(body, id) {
		t.Fatal("another user's list shows the request")
	}

	// download once, with a file token as a browser link would
	ft, _ := env.user.NewFileToken()
	code, data, hdr := env.get("/api/gdpr/exports/"+id+"/download?token="+ft, "")
	if code != http.StatusOK || hdr.Get("Content-Type") != "application/zip" {
		t.Fatalf("download: %d %v", code, hdr)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var profile []byte
	for _, f := range zr.File {
		if f.Name == "data/users.json" {
			r, _ := f.Open()
			profile, _ = io.ReadAll(r)
			r.Close()
		}
	}
	if !strings.Contains(string(profile), "export-a@example.com") || strings.Contains(string(profile), "export-b@example.com") {
		t.Fatalf("unexpected profile: %s", profile)
	}

	if code, _, _ := env.get("/api/gdpr/exports/"+id+"/download", env.userTok); code != http.StatusNotFound {
		t.Fatalf("second download must fail: %d", code)
	}
	if n, _ := env.app.CountRecords(core.CollectionNameAuditLog, nil); n == 0 {
		t.Fatal("no audit entries")
	}
	dl, _ := env.app.FindAllRecords(core.CollectionNameAuditLog)
	seen := map[string]int{}
	for _, e := range dl {
		seen[e.GetString("action")]++
	}
	if seen[core.AuditActionDataExportRequest] != 1 || seen[core.AuditActionDataExport] != 1 || seen[core.AuditActionDataExportDownload] != 1 {
		t.Fatalf("audit entries: %v", seen)
	}
}

func TestSelfServiceExportNeedsPasskeyWhenEnrolled(t *testing.T) {
	env := newExportEnv(t)

	creds, _ := env.app.FindCollectionByNameOrId(core.CollectionNameWebAuthnCredentials)
	c := core.NewRecord(creds)
	c.Id = core.GenerateDefaultRandomId()
	c.Set("collectionRef", env.user.Collection().Id)
	c.Set("recordRef", env.user.Id)
	c.Set("name", "laptop")
	if err := env.app.UnsafeWithoutHooks().SaveNoValidate(c); err != nil {
		t.Fatal(err)
	}

	code, body := env.do(http.MethodPost, "/api/me/export", `{"password":"1234567890"}`, env.userTok)
	if code != http.StatusForbidden || !strings.Contains(body, "passkey_step_up_required") {
		t.Fatalf("expected a step-up requirement, got %d %s", code, body)
	}
}

func TestSuperuserExportForAnySubject(t *testing.T) {
	env := newExportEnv(t)

	if code, _ := env.do(http.MethodPost, "/api/gdpr/subjects/"+env.other.Id+"/export", `{}`, env.userTok); code != http.StatusForbidden && code != http.StatusUnauthorized {
		t.Fatalf("a regular user must not use the admin route: %d", code)
	}
	if code, _ := env.do(http.MethodPost, "/api/gdpr/subjects/nonexistent0000/export", `{}`, env.adminTok); code != http.StatusNotFound {
		t.Fatalf("unknown subject: %d", code)
	}
	if code, _ := env.do(http.MethodPost, "/api/gdpr/subjects/"+env.other.Id+"/export", `{"collection":"_superusers"}`, env.adminTok); code != http.StatusNotFound {
		t.Fatalf("wrong collection: %d", code)
	}

	code, body := env.do(http.MethodPost, "/api/gdpr/subjects/"+env.other.Id+"/export", `{}`, env.adminTok)
	if code != http.StatusAccepted {
		t.Fatalf("%d %s", code, body)
	}
	var v map[string]any
	_ = json.Unmarshal([]byte(body), &v)
	id := v["id"].(string)
	if v["selfService"] != false || v["subjectId"] != env.other.Id {
		t.Fatalf("unexpected view %s", body)
	}

	done := env.waitDone(id, env.adminTok)
	if done["state"] != core.DataRequestStateDone {
		t.Fatalf("%v", done)
	}

	// the subject did not ask and cannot fetch it
	if code, _ := env.do(http.MethodGet, "/api/gdpr/exports/"+id, "", env.otherTok); code != http.StatusNotFound {
		t.Fatalf("subject status: %d", code)
	}

	code, body = env.do(http.MethodGet, "/api/gdpr/exports?subject="+env.other.Id, "", env.adminTok)
	if code != http.StatusOK || !strings.Contains(body, id) {
		t.Fatalf("admin list: %d %s", code, body)
	}

	// discard instead of downloading
	code, body = env.do(http.MethodDelete, "/api/gdpr/exports/"+id, "", env.adminTok)
	if code != http.StatusOK || !strings.Contains(body, `"expired"`) {
		t.Fatalf("discard: %d %s", code, body)
	}
	if code, _, _ := env.get("/api/gdpr/exports/"+id+"/download", env.adminTok); code != http.StatusNotFound {
		t.Fatalf("download after discard: %d", code)
	}
}
