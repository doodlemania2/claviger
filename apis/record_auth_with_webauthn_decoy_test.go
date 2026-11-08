package apis_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Fork-local tests for issue #40: the login-begin decoy must be
// indistinguishable from a genuine response, must not be able to evict real
// ceremonies, and login-begin must be throttled even with Settings.RateLimits
// off (its default).

const loginBeginPath = "/api/collections/users/auth-with-webauthn/login-begin"

// loginBeginFrom posts a login-begin request as the given client address and
// returns the status plus the raw response body.
func loginBeginFrom(env *ceremonyEnv, remoteAddr, identity string) (int, []byte) {
	env.t.Helper()

	raw, err := json.Marshal(map[string]any{"identity": identity})
	if err != nil {
		env.t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, loginBeginPath, strings.NewReader(string(raw)))
	req.Header.Set("content-type", "application/json")
	req.RemoteAddr = remoteAddr + ":4444"

	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)

	return rec.Code, rec.Body.Bytes()
}

func mustLoginBegin(env *ceremonyEnv, remoteAddr, identity string) map[string]any {
	env.t.Helper()

	status, raw := loginBeginFrom(env, remoteAddr, identity)
	if status != http.StatusOK {
		env.t.Fatalf("login-begin(%q from %s): expected 200, got %d (%s)", identity, remoteAddr, status, raw)
	}

	decoded := map[string]any{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		env.t.Fatal(err)
	}

	return decoded
}

// flattenJSON turns a decoded JSON value into sorted "path=value" lines, with
// the values of the listed random/derived paths replaced by their kind and
// length so everything else is compared exactly.
func flattenJSON(prefix string, v any, volatile map[string]bool, out *[]string) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			flattenJSON(prefix+"."+k, child, volatile, out)
		}
	case []any:
		for i, child := range t {
			flattenJSON(fmt.Sprintf("%s[%d]", prefix, i), child, volatile, out)
		}
	default:
		if volatile[prefix] {
			*out = append(*out, fmt.Sprintf("%s=<%T len %d>", prefix, t, len(fmt.Sprint(t))))
		} else {
			*out = append(*out, fmt.Sprintf("%s=%v", prefix, t))
		}
	}
	sort.Strings(*out)
}

func shapeOf(body map[string]any) []string {
	volatile := map[string]bool{
		".options.publicKey.challenge":              false,
		".options.publicKey.allowCredentials[0].id": false,
		".sessionToken":                             false,
	}
	// challenge, credential id and session token are the only legitimately
	// per-response values; compare their kind and length, not content
	for k := range volatile {
		volatile[k] = true
	}

	var out []string
	flattenJSON("", body, volatile, &out)
	return out
}

func nested(t testing.TB, body map[string]any, path ...string) any {
	t.Helper()

	var cur any = body
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("path %v: %q is not an object", path, p)
		}
		cur = m[p]
	}

	return cur
}

func credID(t testing.TB, body map[string]any) string {
	t.Helper()

	list, ok := nested(t, body, "options", "publicKey", "allowCredentials").([]any)
	if !ok || len(list) == 0 {
		t.Fatalf("no allowCredentials in %v", body)
	}

	return list[0].(map[string]any)["id"].(string)
}

// TestWebAuthnLoginBeginDecoyIndistinguishable pins that a genuine response and
// a decoy carry the same JSON keys and the same non-random values. The original
// leak (issue #40) was a go-webauthn default (timeout) that the hand-rolled
// decoy did not share; this test keeps that from re-opening on a library bump.
func TestWebAuthnLoginBeginDecoyIndistinguishable(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	// a known account that has no passkey yet takes the decoy path through a
	// different branch of the handler; it must look the same too
	noPasskey := mustLoginBegin(env, "192.0.2.10", ceremonyIdentity)

	registerPasskey(t, env, newSoftAuthenticator(t, ceremonyRPID), "laptop")

	real1 := mustLoginBegin(env, "192.0.2.10", ceremonyIdentity)
	real2 := mustLoginBegin(env, "192.0.2.10", ceremonyIdentity)
	decoy1 := mustLoginBegin(env, "192.0.2.10", "nobody@example.com")
	decoy2 := mustLoginBegin(env, "192.0.2.10", "nobody@example.com")

	if got, want := shapeOf(decoy1), shapeOf(real1); !reflect.DeepEqual(got, want) {
		t.Fatalf("decoy and genuine responses differ\ndecoy: %v\nreal:  %v", got, want)
	}

	if got, want := shapeOf(noPasskey), shapeOf(real1); !reflect.DeepEqual(got, want) {
		t.Fatalf("no-passkey decoy and genuine responses differ\ndecoy: %v\nreal:  %v", got, want)
	}

	// the challenge is random on both paths (a deterministic one would be a
	// signal of its own) ...
	if challengeFrom(t, real1) == challengeFrom(t, real2) {
		t.Fatal("genuine challenge repeated")
	}
	if challengeFrom(t, decoy1) == challengeFrom(t, decoy2) {
		t.Fatal("decoy challenge is deterministic; it must be random like a genuine one")
	}
	if len(challengeFrom(t, decoy1)) != len(challengeFrom(t, real1)) {
		t.Fatal("decoy and genuine challenge lengths differ")
	}

	// ... while the credential id is stable per identity on both paths
	if credID(t, real1) != credID(t, real2) {
		t.Fatal("genuine credential id changed between probes")
	}
	if credID(t, decoy1) != credID(t, decoy2) {
		t.Fatal("decoy credential id changed between probes; a genuine one is stable")
	}
	if len(credID(t, decoy1)) != len(credID(t, real1)) {
		t.Logf("note: decoy credential id length %d vs soft authenticator %d", len(credID(t, decoy1)), len(credID(t, real1)))
	}
}

// Decoys must not occupy session slots, otherwise unauthenticated callers can
// fill the capped store and evict genuine in-flight ceremonies.
func TestWebAuthnLoginBeginDecoyIsNotStored(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)

	countSessions := func() int {
		n := 0
		for k := range env.app.Store().GetAll() {
			if strings.HasPrefix(k, "webauthn:session:") {
				n++
			}
		}
		return n
	}

	var decoyToken string
	for i := 0; i < 15; i++ {
		body := mustLoginBegin(env, "192.0.2.20", fmt.Sprintf("ghost%d@example.com", i))
		decoyToken, _ = body["sessionToken"].(string)
	}
	if n := countSessions(); n != 0 {
		t.Fatalf("expected no stored sessions after decoy requests, got %d", n)
	}

	// a decoy token must fail login-finish exactly like a wrong credential:
	// same status and body as a made-up token
	finish := func(token string) (int, map[string]any) {
		return env.do(http.MethodPost, "/api/collections/users/auth-with-webauthn/login-finish",
			map[string]any{"identity": "ghost0@example.com", "sessionToken": token}, "")
	}
	s1, b1 := finish(decoyToken)
	s2, b2 := finish("not-a-real-token-at-all-0123456789")
	if s1 != http.StatusBadRequest || !reflect.DeepEqual(b1, b2) || s1 != s2 {
		t.Fatalf("decoy finish = %d %v, bogus finish = %d %v", s1, b1, s2, b2)
	}
	if b1["message"] != "Failed to authenticate." {
		t.Fatalf("unexpected finish message: %v", b1)
	}

	registerPasskey(t, env, newSoftAuthenticator(t, ceremonyRPID), "laptop")
	mustLoginBegin(env, "192.0.2.20", ceremonyIdentity)
	if n := countSessions(); n != 1 {
		t.Fatalf("expected 1 stored session for the genuine login-begin, got %d", n)
	}
}

func TestWebAuthnLoginBeginRateLimitIsAlwaysOn(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)
	if env.app.Settings().RateLimits.Enabled {
		t.Fatal("precondition: Settings.RateLimits must be disabled (the default)")
	}

	// per-IP budget: 20 requests across distinct identities, the 21st is refused
	for i := 0; i < 20; i++ {
		if status, raw := loginBeginFrom(env, "192.0.2.30", fmt.Sprintf("ip%d@example.com", i)); status != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d (%s)", i+1, status, raw)
		}
	}
	status, raw := loginBeginFrom(env, "192.0.2.30", "ip20@example.com")
	if status != http.StatusTooManyRequests {
		t.Fatalf("expected 429 once the IP budget is spent, got %d (%s)", status, raw)
	}
	// same body shape as upstream's rate limiter
	if want := `{"data":{},"message":"Too Many Requests.","status":429}`; strings.TrimSpace(string(raw)) != want {
		t.Fatalf("unexpected 429 body %s, want %s", raw, want)
	}

	// another IP is unaffected by the first one's spent budget
	if status, raw := loginBeginFrom(env, "192.0.2.31", "ip20@example.com"); status != http.StatusOK {
		t.Fatalf("other IP: expected 200, got %d (%s)", status, raw)
	}
}

func TestWebAuthnLoginBeginRateLimitPerIdentity(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)

	// 10 requests for one identity, each from a different IP (so the IP budget
	// is never the limiting factor), normalized: case and padding do not mint
	// a new budget
	variants := []string{
		"victim@example.com", "VICTIM@example.com", " victim@example.com", "victim@example.com ",
		"Victim@Example.com", "victim@example.com", "VICTIM@EXAMPLE.COM", "victim@example.com",
		"victim@example.com", "victim@example.com",
	}
	for i, id := range variants {
		if status, raw := loginBeginFrom(env, fmt.Sprintf("192.0.2.%d", 40+i), id); status != http.StatusOK {
			t.Fatalf("request %d (%q): expected 200, got %d (%s)", i+1, id, status, raw)
		}
	}
	status, raw := loginBeginFrom(env, "192.0.2.60", "victim@example.com")
	if status != http.StatusTooManyRequests {
		t.Fatalf("expected 429 once the identity budget is spent, got %d (%s)", status, raw)
	}

	// a different identity, even from an IP that was just refused, is fine
	if status, raw := loginBeginFrom(env, "192.0.2.60", "bystander@example.com"); status != http.StatusOK {
		t.Fatalf("other identity: expected 200, got %d (%s)", status, raw)
	}
}

// Requests refused by the IP budget must not spend the identity budget, so one
// noisy IP cannot lock a victim out beyond the IP budget's own reach.
func TestWebAuthnLoginBeginIPRefusalsDoNotChargeIdentity(t *testing.T) {
	t.Parallel()

	env := newCeremonyEnv(t)

	for i := 0; i < 20; i++ {
		loginBeginFrom(env, "192.0.2.70", fmt.Sprintf("filler%d@example.com", i))
	}
	for i := 0; i < 15; i++ {
		if status, _ := loginBeginFrom(env, "192.0.2.70", "victim@example.com"); status != http.StatusTooManyRequests {
			t.Fatalf("expected IP refusal, got %d", status)
		}
	}

	if status, raw := loginBeginFrom(env, "192.0.2.71", "victim@example.com"); status != http.StatusOK {
		t.Fatalf("victim identity budget was charged by IP refusals: %d (%s)", status, raw)
	}
}
