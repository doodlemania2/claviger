// Package gcpkmstest is a fake Google Cloud KMS REST API, metadata server and
// OAuth2 token endpoint for tests. It wraps with real AES-256-GCM and verifies
// service account JWT signatures.
package gcpkmstest

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Fake is the test double. Create it with [New].
type Fake struct {
	Server *httptest.Server

	// AccessToken is the token the KMS accepts.
	AccessToken string
	// IssuedToken, when set, is what the token endpoints hand out instead of
	// AccessToken (so the KMS rejects it with 401).
	IssuedToken string
	// Deny makes the KMS answer 403.
	Deny bool
	// Purpose and PrimaryState override the key metadata ("" = ENCRYPT_DECRYPT / ENABLED).
	Purpose      string
	PrimaryState string

	mu       sync.Mutex
	keys     map[string]cipher.AEAD
	calls    []string
	failNext int
	saKey    *rsa.PrivateKey
	tokens   int
}

// New starts a fake. Close it with Fake.Server.Close.
func New() *Fake {
	f := &Fake{AccessToken: "fake-access-token", keys: map[string]cipher.AEAD{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

// KeyName returns the resource name of a test key.
func KeyName(key string) string {
	return "projects/p/locations/global/keyRings/r/cryptoKeys/" + key
}

// AddKey creates a key and returns its resource name.
func (f *Fake) AddKey(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	blk, _ := aes.NewCipher(b)
	g, _ := cipher.NewGCM(blk)
	f.keys[KeyName(key)] = g
	return KeyName(key)
}

// FailNext makes the next n KMS requests answer 503.
func (f *Fake) FailNext(n int) { f.mu.Lock(); f.failNext = n; f.mu.Unlock() }

// Calls returns the KMS requests received ("GET name", "POST name:encrypt", ...).
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// TokenRequests is the number of OAuth token requests served.
func (f *Fake) TokenRequests() int { f.mu.Lock(); defer f.mu.Unlock(); return f.tokens }

// WriteServiceAccount writes a service account JSON key file whose token_uri
// is this fake, and returns its path.
func (f *Fake) WriteServiceAccount(t testing.TB) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.saKey = k
	f.mu.Unlock()

	der, _ := x509.MarshalPKCS8PrivateKey(k)
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	j, _ := json.Marshal(map[string]string{
		"type": "service_account", "client_email": "claviger@p.iam.gserviceaccount.com",
		"private_key": string(pemKey), "private_key_id": "kid1", "token_uri": f.Server.URL + "/token",
	})
	p := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(p, j, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func errJSON(w http.ResponseWriter, code int, status, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "status": status, "message": msg}})
}

func (f *Fake) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/computeMetadata/v1/instance/service-accounts/default/token":
		if r.Header.Get("Metadata-Flavor") != "Google" {
			errJSON(w, 403, "FORBIDDEN", "missing Metadata-Flavor")
			return
		}
		f.mu.Lock()
		f.tokens++
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": f.issued(), "expires_in": 3600, "token_type": "Bearer"})
		return
	case "/token":
		f.token(w, r)
		return
	}

	f.mu.Lock()
	if f.failNext > 0 {
		f.failNext--
		f.mu.Unlock()
		errJSON(w, 503, "UNAVAILABLE", "try later")
		return
	}
	f.mu.Unlock()

	if r.Header.Get("Authorization") != "Bearer "+f.AccessToken {
		errJSON(w, 401, "UNAUTHENTICATED", "bad token")
		return
	}

	rest := strings.TrimPrefix(r.URL.Path, "/v1/")
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+rest)
	deny := f.Deny
	f.mu.Unlock()
	if deny {
		errJSON(w, 403, "PERMISSION_DENIED", "Permission denied on resource")
		return
	}

	name, op, _ := strings.Cut(rest, ":")
	f.mu.Lock()
	aead := f.keys[name]
	f.mu.Unlock()
	if aead == nil {
		errJSON(w, 404, "NOT_FOUND", "key not found")
		return
	}

	body, _ := io.ReadAll(r.Body)
	var in struct{ Plaintext, Ciphertext, AdditionalAuthenticatedData string }
	_ = json.Unmarshal(body, &in)
	aad, _ := base64.StdEncoding.DecodeString(in.AdditionalAuthenticatedData)

	w.Header().Set("Content-Type", "application/json")
	switch op {
	case "":
		purpose, state := f.Purpose, f.PrimaryState
		if purpose == "" {
			purpose = "ENCRYPT_DECRYPT"
		}
		if state == "" {
			state = "ENABLED"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"name": name, "purpose": purpose, "primary": map[string]string{"state": state}})
	case "encrypt":
		pt, err := base64.StdEncoding.DecodeString(in.Plaintext)
		if err != nil {
			errJSON(w, 400, "INVALID_ARGUMENT", "bad plaintext")
			return
		}
		n := make([]byte, aead.NonceSize())
		_, _ = rand.Read(n)
		_ = json.NewEncoder(w).Encode(map[string]string{"name": name + "/cryptoKeyVersions/1", "ciphertext": base64.StdEncoding.EncodeToString(aead.Seal(n, n, pt, aad))})
	case "decrypt":
		ct, err := base64.StdEncoding.DecodeString(in.Ciphertext)
		ns := aead.NonceSize()
		if err != nil || len(ct) < ns {
			errJSON(w, 400, "INVALID_ARGUMENT", "Decryption failed: the ciphertext is invalid.")
			return
		}
		pt, err := aead.Open(nil, ct[:ns], ct[ns:], aad)
		if err != nil {
			errJSON(w, 400, "INVALID_ARGUMENT", "Decryption failed: the ciphertext is invalid.")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"plaintext": base64.StdEncoding.EncodeToString(pt)})
	default:
		errJSON(w, 404, "NOT_FOUND", "unknown method")
	}
}

func (f *Fake) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	parts := strings.Split(r.PostForm.Get("assertion"), ".")
	f.mu.Lock()
	k := f.saKey
	f.mu.Unlock()

	bad := func(msg string) {
		w.WriteHeader(400)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": msg})
	}
	if k == nil || len(parts) != 3 {
		bad("Invalid JWT")
		return
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	h := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err != nil || rsa.VerifyPKCS1v15(&k.PublicKey, crypto.SHA256, h[:], sig) != nil {
		bad("Invalid JWT Signature.")
		return
	}
	claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct{ Iss, Scope, Aud string }
	_ = json.Unmarshal(claims, &c)
	if c.Iss == "" || !strings.Contains(c.Scope, "cloudkms") {
		bad("bad claims")
		return
	}
	f.mu.Lock()
	f.tokens++
	f.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": f.issued(), "expires_in": 3600})
}

func (f *Fake) issued() string {
	if f.IssuedToken != "" {
		return f.IssuedToken
	}
	return f.AccessToken
}
