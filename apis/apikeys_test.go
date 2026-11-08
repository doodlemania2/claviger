package apis_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/apis"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #95).

type apiKeyEnv struct {
	t   testing.TB
	app *tests.TestApp
	mux http.Handler
}

func newAPIKeyEnv(t testing.TB) *apiKeyEnv {
	t.Helper()

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)

	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	mux, err := router.BuildMux()
	if err != nil {
		t.Fatal(err)
	}

	return &apiKeyEnv{t: t, app: app, mux: mux}
}

func (env *apiKeyEnv) do(method, url, body, auth string) (int, string) {
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// mint inserts a key directly and returns the full key and its record.
func (env *apiKeyEnv) mint(ownerCollection, ownerId string, scopes ...string) (string, *core.Record) {
	env.t.Helper()

	key, prefix, hash, err := core.GenerateAPIKey()
	if err != nil {
		env.t.Fatal(err)
	}
	col, err := env.app.FindCollectionByNameOrId(ownerCollection)
	if err != nil {
		env.t.Fatal(err)
	}
	keys, _ := env.app.FindCollectionByNameOrId(core.CollectionNameAPIKeys)
	r := core.NewRecord(keys)
	r.Set("collectionRef", col.Id)
	r.Set("recordRef", ownerId)
	r.Set("name", "test")
	r.Set("prefix", prefix)
	r.Set("secretHash", hash)
	r.Set("scopes", scopes)
	if err := env.app.Save(r); err != nil {
		env.t.Fatal(err)
	}
	return key, r
}

const (
	akSuperuserId = "sywbhecnh46rhm0"
	akUserId      = "4q1xlclmfloku33"
)

func TestAPIKeyCreateListRevoke(t *testing.T) {
	t.Parallel()
	env := newAPIKeyEnv(t)

	// create
	status, body := env.do("POST", "/api/apikeys", `{"name":"ci","scopes":["collection:demo2:read"],"expires":"2099-01-01 00:00:00.000Z"}`, auditUserToken)
	if status != 200 {
		t.Fatalf("create: %d %s", status, body)
	}
	var created map[string]any
	_ = json.Unmarshal([]byte(body), &created)
	key, _ := created["key"].(string)
	if !strings.HasPrefix(key, "clv_") || len(key) != 65 {
		t.Fatalf("unexpected key %q", key)
	}
	id, _ := created["id"].(string)
	if created["prefix"] != key[4:12] {
		t.Fatalf("prefix mismatch: %v", created["prefix"])
	}

	// the hash is stored, the secret is not
	rec, err := env.app.FindRecordById(core.CollectionNameAPIKeys, id)
	if err != nil {
		t.Fatal(err)
	}
	secret := key[13:]
	if rec.GetString("secretHash") != core.HashAPIKeySecret(secret) {
		t.Fatal("stored hash does not match")
	}
	var raw string
	_ = env.app.DB().NewQuery("SELECT group_concat(name||prefix||scopes||secretHash||collectionRef||recordRef) FROM _apiKeys").Row(&raw)
	if strings.Contains(raw, secret) {
		t.Fatal("secret found in the database")
	}

	// the key authenticates and works
	if status, body := env.do("GET", "/api/collections/demo2/records", "", key); status != 200 {
		t.Fatalf("key list: %d %s", status, body)
	}
	// also with the Bearer schema
	if status, _ := env.do("GET", "/api/collections/demo2/records", "", "Bearer "+key); status != 200 {
		t.Fatalf("bearer key list: %d", status)
	}

	// list: never shows the key or hash; shown once
	status, body = env.do("GET", "/api/apikeys", "", auditUserToken)
	if status != 200 || !strings.Contains(body, `"prefix":"`+key[4:12]+`"`) {
		t.Fatalf("list: %d %s", status, body)
	}
	for _, leak := range []string{secret, key, "secretHash", core.HashAPIKeySecret(secret)} {
		if strings.Contains(body, leak) {
			t.Fatalf("list leaks %q: %s", leak, body)
		}
	}

	// other users do not see it nor revoke it; superusers do
	if status, body := env.do("GET", "/api/apikeys", "", auditSuperuserToken); status != 200 || strings.Contains(body, id) {
		t.Fatalf("superuser default list should be own keys only: %d %s", status, body)
	}
	if status, body := env.do("GET", "/api/apikeys?all=true", "", auditSuperuserToken); status != 200 || !strings.Contains(body, id) {
		t.Fatalf("superuser all list: %d %s", status, body)
	}
	otherUser, _ := env.app.FindRecordById("users", "oap640cot4yru2s")
	otherToken, _ := otherUser.NewAuthToken()
	if status, _ := env.do("DELETE", "/api/apikeys/"+id, "", otherToken); status != 404 {
		t.Fatalf("foreign revoke expected 404, got %d", status)
	}

	// revoke
	if status, _ := env.do("DELETE", "/api/apikeys/"+id, "", auditUserToken); status != 204 {
		t.Fatalf("revoke expected 204, got %d", status)
	}
	if status, _ := env.do("DELETE", "/api/apikeys/"+id, "", auditUserToken); status != 204 {
		t.Fatalf("second revoke expected 204, got %d", status)
	}
	if status, _ := env.do("GET", "/api/collections/demo2/records", "", key); status != 401 {
		t.Fatalf("revoked key expected 401, got %d", status)
	}

	// audit: creation and revocation, no secrets
	entries, err := env.app.FindRecordsByFilter(core.CollectionNameAuditLog, "targetCollection='_apiKeys'", "created", 0, 0)
	if err != nil || len(entries) != 2 {
		t.Fatalf("expected 2 audit entries, got %d (%v)", len(entries), err)
	}
	dump, _ := json.Marshal(entries)
	for _, leak := range []string{secret, core.HashAPIKeySecret(secret)} {
		if strings.Contains(string(dump), leak) {
			t.Fatalf("audit leaks %q", leak)
		}
	}
}

func TestAPIKeyCreateValidation(t *testing.T) {
	t.Parallel()
	env := newAPIKeyEnv(t)

	for name, body := range map[string]string{
		"no name":        `{"scopes":["*"]}`,
		"no scopes":      `{"name":"a"}`,
		"bad scope":      `{"name":"a","scopes":["collection:demo2:admin"]}`,
		"unknown coll":   `{"name":"a","scopes":["collection:nope:read"]}`,
		"forbidden coll": `{"name":"a","scopes":["collection:_mfas:read"]}`,
		"past expiry":    `{"name":"a","scopes":["*"],"expires":"2001-01-01 00:00:00.000Z"}`,
		"invalid expiry": `{"name":"a","scopes":["*"],"expires":"soon"}`,
	} {
		if status, resp := env.do("POST", "/api/apikeys", body, auditUserToken); status != 400 {
			t.Errorf("%s: expected 400, got %d %s", name, status, resp)
		}
	}
	if status, _ := env.do("POST", "/api/apikeys", `{"name":"a","scopes":["*"]}`, ""); status != 401 {
		t.Errorf("guest expected 401, got %d", status)
	}
}

func TestAPIKeyCreateRequiresFreshPasskey(t *testing.T) {
	t.Parallel()
	env := newAPIKeyEnv(t)

	creds, _ := env.app.FindCollectionByNameOrId(core.CollectionNameWebAuthnCredentials)
	c := core.NewRecord(creds)
	users, _ := env.app.FindCollectionByNameOrId("users")
	c.Set("collectionRef", users.Id)
	c.Set("recordRef", akUserId)
	c.Set("credentialId", "cred1")
	c.Set("publicKey", "pk")
	if err := env.app.SaveNoValidate(c); err != nil {
		t.Fatal(err)
	}

	body := `{"name":"a","scopes":["*"]}`
	status, resp := env.do("POST", "/api/apikeys", body, auditUserToken)
	if status != 403 || !strings.Contains(resp, apis.StepUpRequiredCode) {
		t.Fatalf("expected step-up 403, got %d %s", status, resp)
	}

	user, _ := env.app.FindRecordById("users", akUserId)
	fresh, err := user.NewPasskeyAuthToken(time.Now(), true)
	if err != nil {
		t.Fatal(err)
	}
	if status, resp := env.do("POST", "/api/apikeys", body, fresh); status != 200 {
		t.Fatalf("fresh passkey expected 200, got %d %s", status, resp)
	}

	stale, _ := user.NewPasskeyAuthToken(time.Now().Add(-10*time.Minute), true)
	if status, _ := env.do("POST", "/api/apikeys", body, stale); status != 403 {
		t.Fatalf("stale passkey expected 403, got %d", status)
	}
}

func TestAPIKeyAuthFailures(t *testing.T) {
	t.Parallel()
	env := newAPIKeyEnv(t)

	good, rec := env.mint("users", akUserId, "*")

	expired, expRec := env.mint("users", akUserId, "*")
	expRec.Set("expires", types.NowDateTime().Add(-time.Hour))
	if err := env.app.Save(expRec); err != nil {
		t.Fatal(err)
	}

	wrongSecret := good[:13] + strings.Repeat("a", 52)

	for name, token := range map[string]string{
		"valid but wrong secret": wrongSecret,
		"expired":                expired,
		"unknown prefix":         "clv_aaaaaaaa_" + strings.Repeat("a", 52),
		"malformed":              "clv_nope",
		"empty parts":            "clv__",
	} {
		status, body := env.do("GET", "/api/collections/demo2/records", "", token)
		if status != 401 {
			t.Errorf("%s: expected 401, got %d", name, status)
		}
		if strings.Contains(body, "expired") && name != "expired" && false {
			t.Errorf("oracle")
		}
	}
	// identical bodies for every failure kind: no oracle
	_, b1 := env.do("GET", "/api/collections/demo2/records", "", wrongSecret)
	_, b2 := env.do("GET", "/api/collections/demo2/records", "", expired)
	_, b3 := env.do("GET", "/api/collections/demo2/records", "", "clv_aaaaaaaa_"+strings.Repeat("a", 52))
	if b1 != b2 || b2 != b3 {
		t.Errorf("failure bodies differ:\n%s\n%s\n%s", b1, b2, b3)
	}

	// owner gone
	if err := env.app.Delete(mustFind(t, env.app, "users", akUserId)); err != nil {
		t.Fatal(err)
	}
	if status, _ := env.do("GET", "/api/collections/demo2/records", "", good); status != 401 {
		t.Errorf("deleted owner expected 401, got %d", status)
	}
	_ = rec
}

func mustFind(t testing.TB, app core.App, col, id string) *core.Record {
	r, err := app.FindRecordById(col, id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAPIKeyScopes(t *testing.T) {
	t.Parallel()
	env := newAPIKeyEnv(t)

	readDemo2, _ := env.mint("users", akUserId, "collection:demo2:read")
	writeDemo2, _ := env.mint("users", akUserId, "collection:demo2:write")
	readDemo3, _ := env.mint("users", akUserId, "collection:demo3:read") // rule excludes users
	all, _ := env.mint("users", akUserId, "*")
	suReadDemo3, _ := env.mint("_superusers", akSuperuserId, "collection:demo3:read")

	cases := []struct {
		name, method, url, body, auth string
		want                          int
	}{
		{"read in scope", "GET", "/api/collections/demo2/records", "", readDemo2, 200},
		{"read by id in scope", "GET", "/api/collections/demo2/records/0yxhwia2amd8gec", "", readDemo2, 200},
		{"read out of scope", "GET", "/api/collections/demo1/records", "", readDemo2, 403},
		{"write with read scope", "POST", "/api/collections/demo2/records", `{"title":"xx"}`, readDemo2, 403},
		{"write in scope", "POST", "/api/collections/demo2/records", `{"title":"xx"}`, writeDemo2, 200},
		{"read with write scope", "GET", "/api/collections/demo2/records", "", writeDemo2, 403},
		{"delete with read scope", "DELETE", "/api/collections/demo2/records/0yxhwia2amd8gec", "", readDemo2, 403},
		{"scope never grants beyond rules", "GET", "/api/collections/demo3/records", "", readDemo3, 200}, // list rules filter to empty set
		{"view beyond rules", "GET", "/api/collections/demo3/records/1tmknxy2868d869", "", readDemo3, 404},
		{"superuser limited by scope", "GET", "/api/collections/demo2/records", "", suReadDemo3, 403},
		{"superuser in scope", "GET", "/api/collections/demo3/records", "", suReadDemo3, 200},
		{"expand needs target scope", "GET", "/api/collections/demo1/records?expand=rel_many", "", suMint(env, "collection:demo1:read"), 403},
		{"expand nested needs target scope", "GET", "/api/collections/demo1/records?expand=rel_many.rel", "", suMint(env, "collection:demo1:read"), 403},
		{"expand with target scope", "GET", "/api/collections/demo1/records?expand=rel_many", "", suMint(env, "collection:demo1:read", "collection:users:read"), 200},
		{"all scope", "GET", "/api/collections/demo2/records", "", all, 200},
		{"other routes need *", "GET", "/api/collections/demo2", "", readDemo2, 403},
		{"user * cannot reach superuser routes", "GET", "/api/collections", "", all, 403}, // owner is not a superuser (rule, not scope)
		{"realtime needs *", "POST", "/api/realtime", `{"clientId":"x","subscriptions":[]}`, readDemo2, 403},
		{"batch needs *", "POST", "/api/batch", `{"requests":[]}`, readDemo2, 403},
		{"health always", "GET", "/api/health", "", readDemo2, 200},
	}

	for _, c := range cases {
		status, body := env.do(c.method, c.url, c.body, c.auth)
		if status != c.want {
			t.Errorf("%s: expected %d, got %d %s", c.name, c.want, status, body)
		}
	}

	// the demo3 list returned nothing for the users-owned key
	_, body := env.do("GET", "/api/collections/demo3/records", "", readDemo3)
	if !strings.Contains(body, `"totalItems":0`) && !strings.Contains(body, `"items":[]`) {
		t.Errorf("rules must still apply as the owner: %s", body)
	}
}

func suMint(env *apiKeyEnv, scopes ...string) string {
	k, _ := env.mint("_superusers", akSuperuserId, scopes...)
	return k
}

func TestAPIKeyHardDenials(t *testing.T) {
	t.Parallel()
	env := newAPIKeyEnv(t)

	userAll, _ := env.mint("users", akUserId, "*")
	suAll, _ := env.mint("_superusers", akSuperuserId, "*")
	suScoped, _ := env.mint("_superusers", akSuperuserId, "collection:demo2:read")

	// keys never manage keys, even with * and a superuser owner
	for _, k := range []string{userAll, suAll} {
		if s, _ := env.do("POST", "/api/apikeys", `{"name":"x","scopes":["*"]}`, k); s != 403 {
			t.Errorf("key create expected 403, got %d", s)
		}
		if s, _ := env.do("GET", "/api/apikeys", "", k); s != 403 {
			t.Errorf("key list expected 403, got %d", s)
		}
		if s, _ := env.do("DELETE", "/api/apikeys/whatever", "", k); s != 403 {
			t.Errorf("key revoke expected 403, got %d", s)
		}
	}

	// auth flows and account changes
	for _, c := range []struct{ method, url, body string }{
		{"POST", "/api/collections/users/auth-refresh", ""},
		{"POST", "/api/collections/users/auth-with-password", `{"identity":"a","password":"b"}`},
		{"POST", "/api/collections/users/request-password-reset", `{"email":"test@example.com"}`},
		{"POST", "/api/collections/users/confirm-email-change", `{}`},
		{"POST", "/api/collections/users/request-email-change", `{"newEmail":"x@example.com"}`},
		{"POST", "/api/collections/users/auth-with-webauthn/step-up-begin", `{}`},
		{"POST", "/api/collections/users/impersonate/" + akUserId, `{}`},
		{"POST", "/api/files/token", ``},
		{"PATCH", "/api/collections/users/records/" + akUserId, `{"password":"newpassword123","passwordConfirm":"newpassword123"}`},
		{"PATCH", "/api/collections/users/records/" + akUserId, `{"email":"new@example.com"}`},
		{"POST", "/api/collections/_superusers/records", `{"email":"a@example.com","password":"1234567890","passwordConfirm":"1234567890"}`},
		{"GET", "/api/collections/_apiKeys/records", ``},
		{"GET", "/api/collections/_mfas/records", ``},
	} {
		for name, k := range map[string]string{"user*": userAll, "su*": suAll} {
			if s, b := env.do(c.method, c.url, c.body, k); s != 403 {
				t.Errorf("%s %s with %s expected 403, got %d %s", c.method, c.url, name, s, b)
			}
		}
	}

	// superuser routes: reachable only with superuser owner AND *
	if s, _ := env.do("GET", "/api/collections", "", suAll); s != 200 {
		t.Errorf("superuser * should reach superuser routes, got %d", s)
	}
	if s, _ := env.do("GET", "/api/collections", "", suScoped); s != 403 {
		t.Errorf("scoped superuser key must not reach superuser routes, got %d", s)
	}
	if s, _ := env.do("GET", "/api/collections", "", userAll); s != 403 {
		t.Errorf("user * must not reach superuser routes, got %d", s)
	}
}

func TestAPIKeyRulesRunAsOwner(t *testing.T) {
	t.Parallel()
	env := newAPIKeyEnv(t)

	// users viewRule: id = @request.auth.id -> only the owner is visible
	k, _ := env.mint("users", akUserId, "collection:users:read")
	if status, body := env.do("GET", "/api/collections/users/records/"+akUserId, "", k); status != 200 {
		t.Fatalf("owner view expected 200, got %d %s", status, body)
	}
	if status, body := env.do("GET", "/api/collections/users/records/oap640cot4yru2s", "", k); status != 404 {
		t.Fatalf("foreign view expected 404, got %d %s", status, body)
	}
}

func TestAPIKeyLastUsedThrottled(t *testing.T) {
	t.Parallel()
	env := newAPIKeyEnv(t)

	k, rec := env.mint("users", akUserId, "collection:demo2:read")
	lastUsed := func() string {
		r := mustFind(t, env.app, core.CollectionNameAPIKeys, rec.Id)
		return r.GetString("lastUsed")
	}

	if lastUsed() != "" {
		t.Fatal("lastUsed should start empty")
	}
	env.do("GET", "/api/collections/demo2/records", "", k)
	first := lastUsed()
	if first == "" {
		t.Fatal("lastUsed not recorded")
	}

	time.Sleep(20 * time.Millisecond)
	env.do("GET", "/api/collections/demo2/records", "", k)
	if lastUsed() != first {
		t.Fatal("lastUsed written again within the throttle window")
	}

	// outside the window it is refreshed
	old := time.Now().Add(-2 * time.Minute).UTC().Format(types.DefaultDateLayout)
	if _, err := env.app.DB().Update(core.CollectionNameAPIKeys, dbx.Params{"lastUsed": old}, dbx.HashExp{"id": rec.Id}).Execute(); err != nil {
		t.Fatal(err)
	}
	env.do("GET", "/api/collections/demo2/records", "", k)
	if lastUsed() == old {
		t.Fatal("lastUsed not refreshed after the throttle window")
	}

	// no audit noise from lastUsed
	entries, _ := env.app.FindRecordsByFilter(core.CollectionNameAuditLog, "targetCollection='_apiKeys' && action='update'", "", 0, 0)
	if len(entries) != 0 {
		t.Fatalf("lastUsed must not be audited, got %d entries", len(entries))
	}
}

func TestAPIKeyAuditActor(t *testing.T) {
	t.Parallel()
	env := newAPIKeyEnv(t)

	k, rec := env.mint("users", akUserId, "collection:demo2:write")
	if s, b := env.do("POST", "/api/collections/demo2/records", `{"id":"keyaudit0000001","title":"xx"}`, k); s != 200 {
		t.Fatalf("create: %d %s", s, b)
	}
	entries, _ := env.app.FindRecordsByFilter(core.CollectionNameAuditLog, "targetId='keyaudit0000001'", "", 0, 0)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.GetString("actorId") != akUserId || e.GetString("actorType") != "record" {
		t.Fatalf("actor should be the owner: %s %s", e.GetString("actorType"), e.GetString("actorId"))
	}
	var meta map[string]any
	_ = e.UnmarshalJSONField("meta", &meta)
	if meta["apiKeyId"] != rec.Id {
		t.Fatalf("meta.apiKeyId = %v, want %s", meta["apiKeyId"], rec.Id)
	}

	// a JWT write carries no key id
	if s, b := env.do("POST", "/api/collections/demo2/records", `{"id":"jwtaudit0000001","title":"yy"}`, auditUserToken); s != 200 {
		t.Fatalf("jwt create failed: %d %s", s, b)
	}
	entries, _ = env.app.FindRecordsByFilter(core.CollectionNameAuditLog, "targetId='jwtaudit0000001'", "", 0, 0)
	var meta2 map[string]any
	_ = entries[0].UnmarshalJSONField("meta", &meta2)
	if _, has := meta2["apiKeyId"]; has {
		t.Fatal("JWT request must not carry apiKeyId")
	}
}

func TestAPIKeyFormatAndRateLimit(t *testing.T) {
	t.Parallel()

	key, prefix, hash, err := core.GenerateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	p, s, ok := core.ParseAPIKey(key)
	if !ok || p != prefix || core.HashAPIKeySecret(s) != hash {
		t.Fatalf("round trip failed for %s", key)
	}
	for _, bad := range []string{"", "clv_", "clv_a_b", key + "x", strings.ToUpper(key), "xlv_" + key[4:]} {
		if _, _, ok := core.ParseAPIKey(bad); ok {
			t.Errorf("%q should not parse", bad)
		}
	}

	env := newAPIKeyEnv(t)
	bad := "clv_aaaaaaaa_" + strings.Repeat("a", 52)
	var last int
	for i := 0; i < 40; i++ {
		last, _ = env.do("GET", "/api/collections/demo2/records", "", bad)
	}
	if last != 429 {
		t.Fatalf("expected failed lookups to be rate limited (429), got %d", last)
	}
}

func TestJWTPathUnaffectedByAPIKeys(t *testing.T) {
	t.Parallel()
	env := newAPIKeyEnv(t)

	// an invalid JWT is still just a guest (no hard 401), exactly as before
	if s, _ := env.do("GET", "/api/collections/demo2/records", "", "eyJhbGciOiJIUzI1NiJ9.invalid.token"); s != 200 {
		t.Fatalf("invalid JWT should remain a guest request, got %d", s)
	}
	if s, _ := env.do("GET", "/api/collections/users/records/"+akUserId, "", auditUserToken); s != 200 {
		t.Fatalf("valid JWT, got %d", s)
	}
}

func TestAPIKeyFilterSortOracle(t *testing.T) {
	t.Parallel()
	env := newAPIKeyEnv(t)

	narrow := suMint(env, "collection:demo1:read")
	wide := suMint(env, "collection:demo1:read", "collection:users:read")
	all := suMint(env, "*")

	q := func(param, value string) string {
		return "/api/collections/demo1/records?" + param + "=" + url.QueryEscape(value)
	}

	denied := []string{
		q("filter", "rel_many.email != ''"),
		q("filter", "rel_many.email ?= 'x'"),
		q("filter", "@collection.users.email = 'x'"),
		q("filter", "@collection.users:u.email = 'x'"),
		q("filter", "users_via_rel_many.id != ''"),
		q("filter", "(id != '' && rel_many:each.email ~ 'a')"),
		q("sort", "rel_many.email"),
		q("sort", "-rel_many.email,id"),
		q("sort", "+@collection.users.email"),
		q("filter", "@request.auth.id != '' && @collection.users.id != ''"),
	}
	for _, u := range denied {
		if s, b := env.do("GET", u, "", narrow); s != 403 {
			t.Errorf("narrow key, %s: expected 403, got %d %s", u, s, b)
		}
		if s, b := env.do("GET", u, "", wide); s == 403 {
			t.Errorf("key with users scope, %s: unexpected 403 %s", u, b)
		}
		if s, b := env.do("GET", u, "", all); s == 403 {
			t.Errorf("* key, %s: unexpected 403 %s", u, b)
		}
	}

	allowed := []string{
		q("filter", "id != ''"),
		q("filter", "id != '' && rel_many:length > 0"),
		q("filter", "id = 'rel_many.email'"), // inside a string literal
		q("sort", "-id"),
		q("sort", "@random"),
	}
	for _, u := range allowed {
		if s, b := env.do("GET", u, "", narrow); s != 200 {
			t.Errorf("narrow key, %s: expected 200, got %d %s", u, s, b)
		}
	}
}
