package oidc_test

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/oidc"
)

func TestPasskeySignInSetsACRAndAMRAndSurvivesRefresh(t *testing.T) {
	e := newEnv(t, envOpts{webauthn: true})
	clientID, _ := e.createClient(map[string]any{"public": true, "firstParty": true})

	user := e.user()
	userToken, err := user.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}

	auth := newSoftAuthenticator(t, "example.com")
	b := e.browser()

	// register the passkey
	status, begin := b.json("POST", "/api/collections/users/auth-with-webauthn/register-begin", map[string]any{}, map[string]string{"Authorization": userToken})
	if status != 200 {
		t.Fatalf("register-begin: %d %v", status, begin)
	}
	payload := auth.attestation(t, challengeOf(t, begin), appURL)
	payload["sessionToken"] = begin["sessionToken"]
	payload["name"] = "laptop"
	if status, out := b.json("POST", "/api/collections/users/auth-with-webauthn/register-finish", payload, map[string]string{"Authorization": userToken}); status != 201 {
		t.Fatalf("register-finish: %d %v", status, out)
	}

	// authorize, then sign in with the passkey instead of the password
	reqID, p := b.startAuthorize(clientID, authOpts{})
	b.do("GET", "/oidc/login?authRequestID="+reqID, nil, nil)

	status, begin = b.json("POST", "/api/collections/users/auth-with-webauthn/login-begin", map[string]any{"identity": testEmail}, nil)
	if status != 200 {
		t.Fatalf("login-begin: %d %v", status, begin)
	}
	payload = auth.assertion(t, challengeOf(t, begin), appURL, user.Id)
	payload["sessionToken"] = begin["sessionToken"]
	payload["identity"] = testEmail
	status, login := b.json("POST", "/api/collections/users/auth-with-webauthn/login-finish", payload, nil)
	if status != 200 {
		t.Fatalf("login-finish: %d %v", status, login)
	}

	status, out := b.finish(reqID, login["token"].(string))
	if status != 200 {
		t.Fatalf("finish: %d %v", status, out)
	}

	u := b.resume(out["redirect"].(string))
	tok := e.mustExchange(clientID, "", loginResult{code: u.Query().Get("code"), pkce: p})

	idClaims, err := e.verifyJWT(tok["id_token"].(string))
	if err != nil {
		t.Fatal(err)
	}

	if idClaims["acr"] != "urn:claviger:acr:passkey" {
		t.Fatalf("acr: %v", idClaims["acr"])
	}
	amr, _ := idClaims["amr"].([]any)
	if len(amr) != 2 || amr[0] != "hwk" || amr[1] != "user" {
		t.Fatalf("amr: %v", amr)
	}

	// refresh keeps both (the library alone loses acr here)
	status, tok2 := e.refresh(clientID, "", tok["refresh_token"].(string))
	if status != 200 {
		t.Fatalf("refresh: %d %v", status, tok2)
	}

	idClaims2, err := e.verifyJWT(tok2["id_token"].(string))
	if err != nil {
		t.Fatal(err)
	}
	amr2, _ := idClaims2["amr"].([]any)
	if idClaims2["acr"] != "urn:claviger:acr:passkey" || len(amr2) != 2 || amr2[0] != "hwk" {
		t.Fatalf("acr/amr lost on refresh: %v", idClaims2)
	}
	if idClaims2["sid"] != idClaims["sid"] {
		t.Fatalf("sid changed on refresh: %v vs %v", idClaims2["sid"], idClaims["sid"])
	}
}

func challengeOf(t testing.TB, body map[string]any) string {
	t.Helper()

	options, _ := body["options"].(map[string]any)
	pk, _ := options["publicKey"].(map[string]any)
	ch, _ := pk["challenge"].(string)
	if ch == "" {
		t.Fatalf("no challenge in %v", body)
	}

	return ch
}

func TestKeyRotationKeepsOldTokensVerifiable(t *testing.T) {
	e := newEnv(t)
	clientID, _ := e.createClient(map[string]any{"public": true, "firstParty": true})

	before := e.mustExchange(clientID, "", e.browser().login(clientID, authOpts{}))
	oldHeader, _ := decodeJWT(t, before["id_token"].(string))

	if n := len(e.jwks().Keys); n != 1 {
		t.Fatalf("expected 1 key, got %d", n)
	}

	// rotate through the admin API
	status, out := e.browser().json("POST", "/api/oidc/keys/rotate", map[string]any{"graceHours": 1}, map[string]string{"Authorization": e.super})
	if status != 200 {
		t.Fatalf("rotate: %d %v", status, out)
	}

	if n := len(e.jwks().Keys); n != 2 {
		t.Fatalf("expected the old and the new key in the JWKS, got %d", n)
	}

	// the token signed before the rotation still verifies
	if _, err := e.verifyJWT(before["id_token"].(string)); err != nil {
		t.Fatalf("an old token stopped verifying after the rotation: %v", err)
	}

	// new tokens use the new key
	after := e.mustExchange(clientID, "", e.browser().login(clientID, authOpts{}))
	newHeader, _ := decodeJWT(t, after["id_token"].(string))
	if newHeader["kid"] == oldHeader["kid"] {
		t.Fatal("new tokens are still signed with the old key")
	}

	// once the grace period is over, the old key leaves the JWKS
	past := time.Now().Add(-time.Minute).UTC().Format("2006-01-02 15:04:05.000Z")
	if _, err := e.app.NonconcurrentDB().NewQuery("UPDATE {{_signingKeys}} SET [[retiresAt]] = '" + past + "' WHERE [[retiresAt]] != ''").Execute(); err != nil {
		t.Fatal(err)
	}
	if n := len(e.jwks().Keys); n != 1 {
		t.Fatalf("expected the retired key to leave the JWKS, got %d keys", n)
	}
	if _, err := e.verifyJWT(before["id_token"].(string)); err == nil {
		t.Fatal("a token of a retired key still verifies against the JWKS")
	}

	status, keys := e.browser().json("GET", "/api/oidc/keys", nil, map[string]string{"Authorization": e.super})
	_ = keys
	if status != 200 {
		t.Fatalf("keys: %d", status)
	}
}

func TestRotateKeysCommand(t *testing.T) {
	e := newEnv(t)
	e.createClient(map[string]any{"public": true, "firstParty": true})

	store := oidc.NewStore(e.app)
	if _, err := store.RotateKeys(context.Background(), nil, 0); err != nil { // fresh install: creates RS256
		t.Fatal(err)
	}

	cmd := oidc.NewCommand(e.app)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"rotate-keys", "--alg", "ES256", "--grace", "2h"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "created signing key") {
		t.Fatalf("output: %q", out.String())
	}

	keys, err := store.Keys()
	if err != nil || len(keys) != 2 {
		t.Fatalf("keys: %v %v", err, keys)
	}

	// rotating RS256 again retires the first key but keeps it published
	if _, err := store.RotateKeys(context.Background(), []string{"RS256"}, time.Hour); err != nil {
		t.Fatal(err)
	}

	statuses := map[string]int{}
	keys, _ = store.Keys()
	for _, k := range keys {
		statuses[k.Alg+"/"+k.Status]++
	}
	if statuses["RS256/active"] != 1 || statuses["RS256/retiring"] != 1 || statuses["ES256/active"] != 1 {
		t.Fatalf("key statuses: %v", statuses)
	}

	if err := func() error { _, err := store.RotateKeys(context.Background(), []string{"HS256"}, 0); return err }(); err == nil {
		t.Fatal("an unsupported algorithm was accepted")
	}
}

func TestES256Client(t *testing.T) {
	e := newEnv(t)
	clientID, _ := e.createClient(map[string]any{"public": true, "firstParty": true, "signingAlg": "ES256"})
	rsaClient, _ := e.createClient(map[string]any{"public": true, "firstParty": true})

	tok := e.mustExchange(clientID, "", e.browser().login(clientID, authOpts{}))
	hdr, _ := decodeJWT(t, tok["id_token"].(string))
	if hdr["alg"] != "ES256" {
		t.Fatalf("id token alg: %v", hdr["alg"])
	}
	if _, err := e.verifyJWT(tok["id_token"].(string)); err != nil {
		t.Fatalf("ES256 token does not verify: %v", err)
	}
	if hdr, _ = decodeJWT(t, tok["access_token"].(string)); hdr["alg"] != "ES256" {
		t.Fatalf("access token alg: %v", hdr["alg"])
	}

	// another client of the same server keeps RS256
	tok2 := e.mustExchange(rsaClient, "", e.browser().login(rsaClient, authOpts{}))
	if hdr, _ = decodeJWT(t, tok2["id_token"].(string)); hdr["alg"] != "RS256" {
		t.Fatalf("default alg: %v", hdr["alg"])
	}

	algs := map[string]bool{}
	for _, k := range e.jwks().Keys {
		algs[k.Algorithm] = true
	}
	if !algs["ES256"] || !algs["RS256"] {
		t.Fatalf("JWKS algorithms: %v", algs)
	}
}

func TestDisabledWithoutEncryptionOutsideDevMode(t *testing.T) {
	e := newEnv(t, envOpts{noEncryption: true})

	for _, path := range []string{
		"/oidc/.well-known/openid-configuration", "/oidc/authorize", "/oidc/keys", "/oidc/login", "/.well-known/openid-configuration",
	} {
		rec := e.browser().do("GET", path, nil, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s should not exist without the encryption env: %d", path, rec.Code)
		}
	}

	// no key was (or can be) created in the clear
	status, out := e.browser().json("POST", "/api/oidc/keys/rotate", map[string]any{}, map[string]string{"Authorization": e.super})
	if status != 400 {
		t.Fatalf("key rotation without the encryption env: %d %v", status, out)
	}
	if _, err := oidc.NewStore(e.app).RotateKeys(context.Background(), nil, 0); err == nil || !strings.Contains(err.Error(), "unencrypted") {
		t.Fatalf("a plaintext key was created: %v", err)
	}

	var n int
	_ = e.app.ConcurrentDB().NewQuery("SELECT COUNT(*) FROM {{_signingKeys}}").Row(&n)
	if n != 0 {
		t.Fatalf("%d signing keys exist", n)
	}

	status, st := e.browser().json("GET", "/api/oidc/status", nil, map[string]string{"Authorization": e.super})
	if status != 200 || st["enabled"] != false || st["reason"] == "" {
		t.Fatalf("status: %d %v", status, st)
	}

	// the command refuses too
	cmd := oidc.NewCommand(e.app)
	cmd.SetArgs([]string{"rotate-keys"})
	cmd.SilenceErrors = true
	if err := cmd.Execute(); err == nil {
		t.Fatal("rotate-keys ran without the encryption env")
	}
}

func TestStatusAndNoWorkWithoutClients(t *testing.T) {
	e := newEnv(t)

	status, st := e.browser().json("GET", "/api/oidc/status", nil, map[string]string{"Authorization": e.super})
	if status != 200 || st["enabled"] != true || st["issuer"] != issuerURL {
		t.Fatalf("status: %d %v", status, st)
	}

	// the admin routes are for superusers only
	for _, path := range []string{"/api/oidc/status", "/api/oidc/keys"} {
		if rec := e.browser().do("GET", path, nil, nil); rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
			t.Fatalf("%s is open to guests: %d", path, rec.Code)
		}

		userToken, _ := e.user().NewAuthToken()
		if rec := e.browser().do("GET", path, map[string]string{"Authorization": userToken}, nil); rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s is open to users: %d", path, rec.Code)
		}
	}

	// idle: browsing the public endpoints wrote nothing
	for _, path := range []string{"/oidc/.well-known/openid-configuration", "/oidc/keys"} {
		e.browser().do("GET", path, nil, nil)
	}
	for _, table := range []string{"_signingKeys", "_oauthRequests", "_oauthTokens", "_oauthRefreshTokens", "_oauthConsents"} {
		var n int
		_ = e.app.ConcurrentDB().NewQuery("SELECT COUNT(*) FROM " + table).Row(&n)
		if n != 0 {
			t.Fatalf("%s has %d rows without any client", table, n)
		}
	}
}

func TestClientValidation(t *testing.T) {
	e := newEnv(t)

	bad := map[string]map[string]any{
		"http redirect":     {"redirectUris": []string{"http://app.example.net/cb"}},
		"wildcard redirect": {"redirectUris": []string{"https://*.example.net/cb"}},
		"fragment":          {"redirectUris": []string{"https://app.example.net/cb#x"}},
		"javascript":        {"redirectUris": []string{"javascript:alert(1)"}},
		"no redirect":       {"redirectUris": []string{}},
		"superusers":        {"authCollection": "_superusers"},
		"no collection":     {"authCollection": "nothing"},
		"short client id":   {"clientId": "x"},
		"bad scope":         {"scopes": []string{"a b"}},
	}

	for name, body := range bad {
		body["public"] = true
		if _, ok := body["redirectUris"]; !ok {
			body["redirectUris"] = []string{redirectURI}
		}

		if _, ok := body["authCollection"]; !ok {
			body["authCollection"] = "users"
		}

		body["name"] = "x"

		if status, out := e.browser().json("POST", "/api/oidc/clients", body, map[string]string{"Authorization": e.super}); status == 200 {
			t.Errorf("%s was accepted: %v", name, out)
		}
	}

	// loopback http is fine for development
	if status, out := e.browser().json("POST", "/api/oidc/clients", map[string]any{
		"name": "dev", "public": true, "authCollection": "users", "redirectUris": []string{"http://localhost:3000/cb"},
	}, map[string]string{"Authorization": e.super}); status != 200 {
		t.Fatalf("loopback redirect rejected: %d %v", status, out)
	}

	// a confidential client cannot be created without the endpoint that makes its secret
	col, _ := e.app.FindCollectionByNameOrId(core.CollectionNameOAuthClients)
	rec := core.NewRecord(col)
	rec.Set("clientId", "handmade-client")
	rec.Set("name", "x")
	rec.Set("authCollection", "users")
	rec.Set("redirectUris", []string{redirectURI})
	if err := e.app.Save(rec); err == nil {
		t.Fatal("a confidential client without a secret was saved")
	}
}

func TestNoSecretsInAPIResponsesOrAudit(t *testing.T) {
	e := newEnv(t)
	clientID, secret := e.createClient(map[string]any{"firstParty": true})

	tok := e.mustExchange(clientID, secret, e.browser().login(clientID, authOpts{}))

	// rotate once so a second key, and an update of the first, are audited too
	if status, out := e.browser().json("POST", "/api/oidc/keys/rotate", map[string]any{}, map[string]string{"Authorization": e.super}); status != 200 {
		t.Fatalf("rotate: %d %v", status, out)
	}

	var pems []string
	rows, err := e.app.FindAllRecords(core.CollectionNameSigningKeys)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		pems = append(pems, r.GetString("privateKey"))
		if strings.Contains(r.GetString("privateKey"), "BEGIN") || !strings.HasPrefix(r.GetString("privateKey"), "enc:") {
			t.Fatal("a signing key is stored in clear text")
		}
	}

	hashes := []string{}
	refs, _ := e.app.FindAllRecords(core.CollectionNameOAuthRefreshTokens)
	for _, r := range refs {
		hashes = append(hashes, r.GetString("tokenHash"))
	}
	clientRec, _ := e.app.FindFirstRecordByData(core.CollectionNameOAuthClients, "clientId", clientID)
	hashes = append(hashes, clientRec.GetString("secretHash"))

	secrets := append(append([]string{}, pems...), hashes...)
	secrets = append(secrets, secret, tok["refresh_token"].(string))

	check := func(where, body string) {
		t.Helper()

		for _, s := range secrets {
			if s != "" && strings.Contains(body, s) {
				t.Fatalf("%s leaks a secret (%.12s...)", where, s)
			}
		}

		for _, marker := range []string{"PRIVATE KEY", "enc:"} {
			if strings.Contains(body, marker) {
				t.Fatalf("%s contains %q", where, marker)
			}
		}

		// a sensitive field may appear only as hidden or redacted, never with a value
		for _, name := range []string{"secretHash", "privateKey", "tokenHash"} {
			for _, part := range strings.Split(body, "\""+name+"\":")[1:] {
				if !strings.HasPrefix(part, "\"[redacted]\"") && !strings.HasPrefix(part, "null") {
					t.Fatalf("%s exposes %s: %.60s", where, name, part)
				}
			}
		}
	}

	h := map[string]string{"Authorization": e.super}
	for _, c := range []string{
		core.CollectionNameSigningKeys, core.CollectionNameOAuthClients, core.CollectionNameOAuthRefreshTokens,
		core.CollectionNameOAuthRequests, core.CollectionNameOAuthTokens, core.CollectionNameAuditLog,
	} {
		rec := e.browser().do("GET", "/api/collections/"+c+"/records?perPage=200", h, nil)
		if rec.Code != 200 {
			t.Fatalf("list %s: %d %s", c, rec.Code, rec.Body.String())
		}

		check("the records API of "+c, rec.Body.String())
	}

	rec := e.browser().do("GET", "/api/oidc/keys", h, nil)
	check("/api/oidc/keys", rec.Body.String())

	// the audit log recorded the key and client changes, with the secrets redacted
	audit, err := e.app.FindRecordsByFilter(core.CollectionNameAuditLog, "targetCollection = '_signingKeys' || targetCollection = '_oauthClients'", "", 20, 0)
	if err != nil || len(audit) < 3 {
		t.Fatalf("expected audit entries for the client and the keys: %v %d", err, len(audit))
	}
	for _, a := range audit {
		raw, _ := a.MarshalJSON()
		check("an audit entry", string(raw))
	}
}

func TestAuthnContextMapping(t *testing.T) {
	cases := []struct {
		method  string
		passkey bool
		amr     string
		acr     string
	}{
		{"password", false, "pwd", oidc.ACRPassword},
		{"totp", false, "pwd otp mfa", oidc.ACRMFA},
		{"mfa", false, "pwd mfa", oidc.ACRMFA},
		{"otp", false, "otp", oidc.ACRPassword},
		{"webauthn", true, "hwk user", oidc.ACRPasskey},
		{"discoverable", true, "hwk user", oidc.ACRPasskey},
		{"password", true, "pwd hwk user", oidc.ACRPasskey}, // a fresh passkey step-up on top of a password sign-in
		{"mfa", true, "pwd mfa hwk user", oidc.ACRPasskey},
		{"", false, "pwd", oidc.ACRPassword},
	}

	for _, c := range cases {
		amr, acr := oidc.AuthnContext(c.method, c.passkey)
		if strings.Join(amr, " ") != c.amr || acr != c.acr {
			t.Errorf("%q passkey=%v: got %v %s, want %s %s", c.method, c.passkey, amr, acr, c.amr, c.acr)
		}
	}
}
