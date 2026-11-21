package oidc_test

import (
	"testing"
)

// A client with idTokenSeconds gets short ID tokens, on the code exchange and on
// every refresh; a client without it keeps the one-hour default.
func TestClientIDTokenLifetime(t *testing.T) {
	e := newEnv(t)

	life := func(tok map[string]any) int {
		t.Helper()

		_, c := decodeJWT(t, tok["id_token"].(string))

		return int(c["exp"].(float64) - c["iat"].(float64))
	}

	shortID, _ := e.createClient(map[string]any{"public": true, "firstParty": true, "idTokenSeconds": 600})
	tok := e.mustExchange(shortID, "", e.browser().login(shortID, authOpts{scope: "openid email offline_access"}))

	if got := life(tok); got != 600 {
		t.Fatalf("code exchange: id token lifetime %ds, want 600", got)
	}

	status, out := e.refresh(shortID, "", tok["refresh_token"].(string))
	if status != 200 {
		t.Fatalf("refresh: %d %v", status, out)
	}

	if out["id_token"] == nil || out["id_token"] == "" {
		t.Fatal("a refresh must return a new id_token")
	}

	if got := life(out); got != 600 {
		t.Fatalf("refresh: id token lifetime %ds, want 600", got)
	}

	defID, _ := e.createClient(map[string]any{"public": true, "firstParty": true})
	tok = e.mustExchange(defID, "", e.browser().login(defID, authOpts{scope: "openid email"}))

	if got := life(tok); got != 3600 {
		t.Fatalf("default: id token lifetime %ds, want 3600", got)
	}
}
