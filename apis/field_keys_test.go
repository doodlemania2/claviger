package apis_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
)

// Fork-local (Claviger #75, M3). Not parallel: the key is set through the environment.

func waitForJob(t testing.TB, env *apiKeyEnv, op string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		status, body := env.do("GET", "/api/field-keys/job", "", auditSuperuserToken)
		if status != 200 {
			t.Fatalf("job: %d %s", status, body)
		}
		var res struct {
			Job map[string]any `json:"job"`
		}
		_ = json.Unmarshal([]byte(body), &res)
		if res.Job != nil && res.Job["op"] == op && res.Job["state"] != "running" {
			return res.Job
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the job did not finish")
	return nil
}

func TestFieldKeysAPI(t *testing.T) {
	setEncKEK(t)
	env := newAPIKeyEnv(t)

	encSetup(t, env.app)
	col, err := env.app.FindCollectionByNameOrId("apienc")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		r := core.NewRecord(col)
		r.Set("phone", "+1 555 0"+strings.Repeat("1", i%5+1))
		r.Set("ssn", "000-00-0000")
		if err := env.app.Save(r); err != nil {
			t.Fatal(err)
		}
	}

	// ---- access: superusers only, never an API key
	userKey, _ := env.mint("users", akUserId, "*")
	suKey, _ := env.mint("_superusers", akSuperuserId, "*")
	for _, ep := range [][2]string{
		{"GET", "/api/field-keys"}, {"GET", "/api/field-keys/job"},
		{"POST", "/api/field-keys/rotate"}, {"POST", "/api/field-keys/rewrap"},
		{"POST", "/api/field-keys/convert"}, {"POST", "/api/field-keys/prune"}, {"POST", "/api/field-keys/strict"},
	} {
		if status, _ := env.do(ep[0], ep[1], `{}`, ""); status != 401 {
			t.Errorf("%v as guest: expected 401, got %d", ep, status)
		}
		if status, _ := env.do(ep[0], ep[1], `{}`, auditUserToken); status != 403 {
			t.Errorf("%v as user: expected 403, got %d", ep, status)
		}
		for _, k := range []string{userKey, suKey} {
			if status, _ := env.do(ep[0], ep[1], `{}`, k); status != 403 && status != 401 {
				t.Errorf("%v with an API key: expected 401/403, got %d", ep, status)
			}
		}
	}

	// ---- status
	status, body := env.do("GET", "/api/field-keys", "", auditSuperuserToken)
	if status != 200 || !strings.Contains(body, `"collection":"apienc"`) || !strings.Contains(body, `"field":"ssn"`) {
		t.Fatalf("status: %d %s", status, body)
	}
	if strings.Contains(body, "wrapped") {
		t.Fatalf("the status must not carry key material: %s", body)
	}

	// ---- validation
	if status, _ := env.do("POST", "/api/field-keys/convert", `{}`, auditSuperuserToken); status != 400 {
		t.Errorf("convert without a collection: %d", status)
	}
	if status, _ := env.do("POST", "/api/field-keys/rotate", `{"field":"phone"}`, auditSuperuserToken); status != 400 {
		t.Errorf("a field without a collection: %d", status)
	}

	// ---- rotate in the background
	before := keyIdOf(t, env, "apienc", "phone")
	status, body = env.do("POST", "/api/field-keys/rotate", `{"collection":"apienc","field":"phone","batchSize":7}`, auditSuperuserToken)
	if status != 202 || !strings.Contains(body, `"op":"rotate"`) {
		t.Fatalf("rotate: %d %s", status, body)
	}
	job := waitForJob(t, env, "rotate")
	if job["state"] != "done" {
		t.Fatalf("job failed: %v", job)
	}

	after := keyIdOf(t, env, "apienc", "phone")
	if after == before {
		t.Fatal("the key must advance")
	}
	if untouched := keyIdOf(t, env, "apienc", "ssn"); untouched == after {
		t.Fatal("ssn must not share a key")
	}
	var n int
	_ = env.app.DB().NewQuery("SELECT COUNT(*) FROM apienc WHERE phone LIKE {:p}").Bind(dbx.Params{"p": "enc:v1:" + before + ":%"}).Row(&n)
	if n != 0 {
		t.Fatalf("%d rows are still on the old key", n)
	}
	if got, err := env.app.FindRecordById("apienc", "encrecord000001"); err != nil || got.GetString("phone") != "+1 555 0100" {
		t.Fatalf("record unreadable after rotation: %v", err)
	}

	// one audit entry, attributed to the superuser
	entries, _ := env.app.FindAllRecords(core.CollectionNameAuditLog, dbx.HashExp{"action": core.AuditActionFieldKeyRotate})
	if len(entries) != 1 || entries[0].GetString("actorType") != core.AuditActorTypeSuperuser || entries[0].GetString("actorId") != akSuperuserId {
		t.Fatalf("expected one rotation entry by the superuser: %v", entries)
	}

	// ---- prune (the old key is unused now)
	status, _ = env.do("POST", "/api/field-keys/prune", `{"collection":"apienc"}`, auditSuperuserToken)
	if status != 202 {
		t.Fatalf("prune: %d", status)
	}
	job = waitForJob(t, env, "prune")
	if job["state"] != "done" || !strings.Contains(toJSON(job["result"]), before) {
		t.Fatalf("prune job: %v", job)
	}

	// ---- rewrap with nothing to do is fine
	status, _ = env.do("POST", "/api/field-keys/rewrap", `{}`, auditSuperuserToken)
	if status != 202 {
		t.Fatalf("rewrap: %d", status)
	}
	if job = waitForJob(t, env, "rewrap"); job["state"] != "done" {
		t.Fatalf("rewrap job: %v", job)
	}

	// ---- strict
	if status, body = env.do("POST", "/api/field-keys/strict", `{"collection":"apienc"}`, auditSuperuserToken); status != 400 {
		t.Fatalf("strict without a field: %d %s", status, body)
	}
	if status, body = env.do("POST", "/api/field-keys/strict", `{"collection":"apienc","field":"phone","strict":true}`, auditSuperuserToken); status != 200 {
		t.Fatalf("strict: %d %s", status, body)
	}
	if status, body = env.do("POST", "/api/field-keys/strict", `{"collection":"apienc","field":"phone","strict":false}`, auditSuperuserToken); status != 200 {
		t.Fatalf("strict off: %d %s", status, body)
	}

	// ---- convert: legacy values appear, the job encrypts them
	if _, err := env.app.DB().NewQuery("UPDATE apienc SET phone='legacy' WHERE id='encrecord000001'").Execute(); err != nil {
		t.Fatal(err)
	}
	if status, body = env.do("POST", "/api/field-keys/strict", `{"collection":"apienc","field":"phone","strict":true}`, auditSuperuserToken); status != 400 || !strings.Contains(body, "keys convert") {
		t.Fatalf("strict must be refused while legacy values exist: %d %s", status, body)
	}
	if status, _ = env.do("POST", "/api/field-keys/convert", `{"collection":"apienc","field":"phone"}`, auditSuperuserToken); status != 202 {
		t.Fatalf("convert: %d", status)
	}
	if job = waitForJob(t, env, "convert"); job["state"] != "done" {
		t.Fatalf("convert job: %v", job)
	}
	if got, err := env.app.FindRecordById("apienc", "encrecord000001"); err != nil || got.GetString("phone") != "legacy" {
		t.Fatalf("converted record: %v", err)
	}
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func keyIdOf(t testing.TB, env *apiKeyEnv, collection, field string) string {
	t.Helper()
	col, err := env.app.FindCollectionByNameOrId(collection)
	if err != nil {
		t.Fatal(err)
	}
	var keyId string
	err = env.app.DB().Select("keyId").From(core.CollectionNameFieldKeys).
		Where(dbx.HashExp{"collectionRef": col.Id, "fieldRef": col.Fields.GetByName(field).GetId(), "retired": ""}).Row(&keyId)
	if err != nil {
		t.Fatal(err)
	}
	return keyId
}
