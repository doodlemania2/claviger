package apis_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
)

// Fork-local tests for issue #43: the per-user ownership boundary on
// _webauthnCredentials, exercised against a credential that really exists
// (the older suite only ever reached these checks with zero rows).

const credentialsPath = "/api/collections/users/auth-with-webauthn/credentials"

// hiddenCredentialFields are the fields that must never appear in generic
// records API responses (publicKey from the original collection migration, the
// rest from migrations/1779672000_lock_webauthn_credentials.go).
var hiddenCredentialFields = []string{
	"publicKey", "credentialId", "aaguid", "flags", "signCount", "transport", "attestationType",
}

// ownershipFixture seeds a passkey for user A and returns a token for a
// different user B.
type ownershipFixture struct {
	env    *ceremonyEnv
	credID string // _webauthnCredentials record id owned by A
	tokenA string
	tokenB string
	userB  *core.Record
}

func newOwnershipFixture(t *testing.T) *ownershipFixture {
	t.Helper()

	env := newCeremonyEnv(t)
	credID := registerPasskey(t, env, newSoftAuthenticator(t, ceremonyRPID), "user A laptop")
	if credID == "" {
		t.Fatal("registerPasskey returned no credential record id")
	}

	users, err := env.app.FindAllRecords("users")
	if err != nil {
		t.Fatal(err)
	}
	var userB *core.Record
	for _, u := range users {
		if u.Id != ceremonyUserID {
			userB = u
			break
		}
	}
	if userB == nil {
		t.Fatal("no second user in the fixture")
	}

	tokenB, err := userB.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}

	return &ownershipFixture{env: env, credID: credID, tokenA: ceremonyUserToken, tokenB: tokenB, userB: userB}
}

func (f *ownershipFixture) credentialExists(t *testing.T) bool {
	t.Helper()

	_, err := f.env.app.FindRecordById(core.CollectionNameWebAuthnCredentials, f.credID)
	return err == nil
}

func (f *ownershipFixture) credentialName(t *testing.T) string {
	t.Helper()

	r, err := f.env.app.FindRecordById(core.CollectionNameWebAuthnCredentials, f.credID)
	if err != nil {
		t.Fatal(err)
	}
	return r.GetString("name")
}

func TestWebAuthnCredentialOwnershipList(t *testing.T) {
	t.Parallel()
	f := newOwnershipFixture(t)

	// the owner sees it (guards the test against a vacuous pass)
	if ownIDs := f.listCredentialIDs(t, f.tokenA); !contains(ownIDs, f.credID) {
		t.Fatalf("owner list = %v, expected to contain %s", ownIDs, f.credID)
	}

	ids := f.listCredentialIDs(t, f.tokenB)
	if contains(ids, f.credID) {
		t.Fatalf("user B's list leaked user A's credential: %v", ids)
	}
}

// listCredentialIDs calls the list endpoint, which returns a bare JSON array
// (ceremonyEnv.do decodes into a map, so this issues the request itself).
func (f *ownershipFixture) listCredentialIDs(t *testing.T, token string) []string {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, credentialsPath, strings.NewReader(""))
	req.Header.Set("Authorization", token)
	rec := httptest.NewRecorder()
	f.env.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list credentials: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("list credentials: unexpected body %s: %v", rec.Body.String(), err)
	}

	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		id, _ := r["id"].(string)
		ids = append(ids, id)
	}

	return ids
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func TestWebAuthnCredentialOwnershipPatch(t *testing.T) {
	t.Parallel()
	f := newOwnershipFixture(t)

	status, body := f.env.do(http.MethodPatch, credentialsPath+"/"+f.credID, map[string]any{"name": "hijacked"}, f.tokenB)
	if status != http.StatusNotFound {
		t.Fatalf("user B PATCH of A's credential: expected 404, got %d (%v)", status, body)
	}
	if got := f.credentialName(t); got != "user A laptop" {
		t.Fatalf("credential was renamed by a non-owner: %q", got)
	}

	// the owner can (guards against the endpoint simply always 404ing)
	status, body = f.env.do(http.MethodPatch, credentialsPath+"/"+f.credID, map[string]any{"name": "renamed"}, f.tokenA)
	if status != http.StatusOK {
		t.Fatalf("owner PATCH: expected 200, got %d (%v)", status, body)
	}
	if got := f.credentialName(t); got != "renamed" {
		t.Fatalf("owner rename not applied: %q", got)
	}
}

func TestWebAuthnCredentialOwnershipDelete(t *testing.T) {
	t.Parallel()
	f := newOwnershipFixture(t)

	status, body := f.env.do(http.MethodDelete, credentialsPath+"/"+f.credID, nil, f.tokenB)
	if status != http.StatusNotFound {
		t.Fatalf("user B DELETE of A's credential: expected 404, got %d (%v)", status, body)
	}
	if !f.credentialExists(t) {
		t.Fatal("credential was deleted by a non-owner")
	}

	status, body = f.env.do(http.MethodDelete, credentialsPath+"/"+f.credID, nil, f.tokenA)
	if status != http.StatusNoContent {
		t.Fatalf("owner DELETE: expected 204, got %d (%v)", status, body)
	}
	if f.credentialExists(t) {
		t.Fatal("owner delete did not remove the credential")
	}
}

// The collection is also reachable through the generic records API; its owner
// rule must hide A's row from B and the hardened fields from everyone.
func TestWebAuthnCredentialsGenericRecordsAPI(t *testing.T) {
	t.Parallel()
	f := newOwnershipFixture(t)

	const listURL = "/api/collections/_webauthnCredentials/records"

	// as B: no rows, view by id is 404, delete by id is 404 and the row survives
	status, body := f.env.do(http.MethodGet, listURL, nil, f.tokenB)
	if status != http.StatusOK {
		t.Fatalf("B list: expected 200, got %d (%v)", status, body)
	}
	if items, _ := body["items"].([]any); len(items) != 0 {
		t.Fatalf("B sees %d rows via the generic API: %v", len(items), items)
	}
	if status, _ = f.env.do(http.MethodGet, listURL+"/"+f.credID, nil, f.tokenB); status != http.StatusNotFound {
		t.Fatalf("B view of A's row: expected 404, got %d", status)
	}
	if status, _ = f.env.do(http.MethodDelete, listURL+"/"+f.credID, nil, f.tokenB); status != http.StatusNotFound {
		t.Fatalf("B delete of A's row: expected 404, got %d", status)
	}
	if !f.credentialExists(t) {
		t.Fatal("credential was deleted by a non-owner via the generic API")
	}

	// unauthenticated sees nothing at all
	if status, body = f.env.do(http.MethodGet, listURL, nil, ""); status == http.StatusOK {
		if items, _ := body["items"].([]any); len(items) != 0 {
			t.Fatalf("guest sees rows via the generic API: %v", items)
		}
	}

	// as A: the row is visible, but without any hidden field
	status, body = f.env.do(http.MethodGet, listURL, nil, f.tokenA)
	if status != http.StatusOK {
		t.Fatalf("A list: expected 200, got %d (%v)", status, body)
	}
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("A expected exactly 1 row, got %v", items)
	}
	row, _ := items[0].(map[string]any)
	if row["id"] != f.credID {
		t.Fatalf("unexpected row %v", row)
	}
	for _, field := range hiddenCredentialFields {
		if _, present := row[field]; present {
			t.Errorf("hidden field %q is exposed by the generic records API: %v", field, row[field])
		}
	}

	// ... and the same for the single-record view
	status, body = f.env.do(http.MethodGet, listURL+"/"+f.credID, nil, f.tokenA)
	if status != http.StatusOK {
		t.Fatalf("A view: expected 200, got %d (%v)", status, body)
	}
	for _, field := range hiddenCredentialFields {
		if _, present := body[field]; present {
			t.Errorf("hidden field %q is exposed by the generic view endpoint", field)
		}
	}
}

// Proves migrations/1779672000_lock_webauthn_credentials.go really took effect
// on a freshly bootstrapped instance. The migration returns nil without doing
// anything when _webauthnCredentials does not exist yet, so a regression in
// migration ordering would silently skip the hardening; this fails instead.
func TestWebAuthnCredentialsLockMigrationApplied(t *testing.T) {
	t.Parallel()

	app := core.NewBaseApp(core.BaseAppConfig{DataDir: t.TempDir(), EncryptionEnv: "pb_test_env"})
	if err := app.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.ResetBootstrapState() })
	if err := app.RunAllMigrations(); err != nil {
		t.Fatal(err)
	}

	col, err := app.FindCollectionByNameOrId(core.CollectionNameWebAuthnCredentials)
	if err != nil {
		t.Fatalf("collection missing after migrations: %v", err)
	}

	for _, name := range hiddenCredentialFields {
		f := col.Fields.GetByName(name)
		if f == nil {
			t.Errorf("field %q missing", name)
			continue
		}
		if !f.GetHidden() {
			t.Errorf("field %q is not hidden after migrations", name)
		}
	}

	sc, ok := col.Fields.GetByName("signCount").(*core.NumberField)
	if !ok || sc.Min == nil || *sc.Min != 0 {
		t.Errorf("signCount minimum not enforced: %+v", sc)
	}
}
