// Package azurekvtest is a fake Azure Key Vault, token endpoint, instance
// metadata service and App Service identity endpoint for tests. It performs
// real RSA-OAEP-256 wrapping.
package azurekvtest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Request is one recorded call.
type Request struct {
	Method string
	Path   string
	Query  url.Values
	Header http.Header
	Form   url.Values
	Body   string
}

type keyVersion struct {
	version string
	priv    *rsa.PrivateKey
}

// Fake is the test double. Create it with [New].
type Fake struct {
	Server *httptest.Server

	mu       sync.Mutex
	versions map[string][]keyVersion // key name -> versions, oldest first
	tokens   map[string]time.Time
	requests []Request
	failures []*failure
	seq      int

	// Secret is the accepted client secret ("" = not accepted).
	Secret string
	// Assertion is the accepted federated assertion.
	Assertion string
	// IdentityHeader is the accepted IDENTITY_HEADER value.
	IdentityHeader string
	// ExpiresIn is the lifetime of issued tokens (default one hour).
	ExpiresIn time.Duration
	// Now is the clock for token expiry (default time.Now).
	Now func() time.Time
	// Deny makes the vault answer 403 to key calls.
	Deny bool
	// KeyType is the kty reported for keys ("RSA" by default).
	KeyType string
}

type failure struct {
	contains string
	status   int
	left     int
	header   http.Header
}

// New starts a fake. Close it with Fake.Server.Close.
func New() *Fake {
	f := &Fake{
		versions:  map[string][]keyVersion{},
		tokens:    map[string]time.Time{},
		ExpiresIn: time.Hour,
		Now:       time.Now,
		KeyType:   "RSA",
	}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(f.handle))
	return f
}

// URL is the base URL (https://127.0.0.1:port).
func (f *Fake) URL() string { return f.Server.URL }

// KeyURL returns the key URL for name (and version when not empty).
func (f *Fake) KeyURL(name, version string) string {
	u := f.Server.URL + "/keys/" + name
	if version != "" {
		u += "/" + version
	}
	return u
}

// IMDSEndpoint, IdentityEndpoint and AuthorityHost are the endpoints to hand to the code under test.
func (f *Fake) IMDSEndpoint() string     { return f.Server.URL + "/imds/token" }
func (f *Fake) IdentityEndpoint() string { return f.Server.URL + "/identity/token" }
func (f *Fake) AuthorityHost() string    { return f.Server.URL }

// AddKey creates a key (or a new version of it) and returns the version.
func (f *Fake) AddKey(name string) string {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.seq++
	v := fmt.Sprintf("v%032d", f.seq)
	f.versions[name] = append(f.versions[name], keyVersion{version: v, priv: priv})
	return v
}

// FailNext makes the next n requests whose path contains substr answer status.
func (f *Fake) FailNext(substr string, status, n int, header http.Header) {
	f.mu.Lock()
	f.failures = append(f.failures, &failure{contains: substr, status: status, left: n, header: header})
	f.mu.Unlock()
}

// Requests returns a copy of the recorded requests.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.requests...)
}

// Count returns the number of recorded requests whose path contains substr.
func (f *Fake) Count(substr string) int {
	n := 0
	for _, r := range f.Requests() {
		if strings.Contains(r.Path, substr) {
			n++
		}
	}
	return n
}

// Reset forgets the recorded requests.
func (f *Fake) Reset() {
	f.mu.Lock()
	f.requests = nil
	f.mu.Unlock()
}

// RevokeTokens makes every issued token invalid.
func (f *Fake) RevokeTokens() {
	f.mu.Lock()
	f.tokens = map[string]time.Time{}
	f.mu.Unlock()
}

func (f *Fake) issue() (token string, exp time.Time) {
	f.seq++
	token = fmt.Sprintf("fake-token-%d", f.seq)
	exp = f.Now().Add(f.ExpiresIn)
	f.tokens[token] = exp
	return
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *Fake) handle(w http.ResponseWriter, r *http.Request) {
	var body string
	if r.Method == http.MethodPost && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		body = string(b)
	} else {
		_ = r.ParseForm()
	}

	f.mu.Lock()
	f.requests = append(f.requests, Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Header: r.Header.Clone(), Form: r.PostForm, Body: body})

	for _, fl := range f.failures {
		if fl.left > 0 && strings.Contains(r.URL.Path, fl.contains) {
			fl.left--
			f.mu.Unlock()
			for k, v := range fl.header {
				w.Header()[k] = v
			}
			writeJSON(w, fl.status, map[string]any{"error": map[string]string{"code": "Injected", "message": "injected failure"}})
			return
		}
	}
	f.mu.Unlock()

	switch {
	case strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token"):
		f.aad(w, r)
	case strings.HasPrefix(r.URL.Path, "/imds/"):
		f.metadata(w, r, "Metadata", "true")
	case strings.HasPrefix(r.URL.Path, "/identity/"):
		f.metadata(w, r, "X-Identity-Header", f.IdentityHeader)
	case strings.HasPrefix(r.URL.Path, "/keys/"):
		f.vault(w, r, body)
	default:
		http.NotFound(w, r)
	}
}

func (f *Fake) aad(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	denied := func() {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client", "error_description": "AADSTS7000215: Invalid client secret provided."})
	}

	if r.PostForm.Get("grant_type") != "client_credentials" || r.PostForm.Get("scope") == "" || r.PostForm.Get("client_id") == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}

	switch {
	case r.PostForm.Get("client_assertion") != "":
		if r.PostForm.Get("client_assertion_type") != "urn:ietf:params:oauth:client-assertion-type:jwt-bearer" || f.Assertion == "" || r.PostForm.Get("client_assertion") != f.Assertion {
			denied()
			return
		}
	case r.PostForm.Get("client_secret") != "":
		if f.Secret == "" || r.PostForm.Get("client_secret") != f.Secret {
			denied()
			return
		}
	default:
		denied()
		return
	}

	tok, _ := f.issue()
	writeJSON(w, http.StatusOK, map[string]any{"access_token": tok, "token_type": "Bearer", "expires_in": int(f.ExpiresIn.Seconds())})
}

func (f *Fake) metadata(w http.ResponseWriter, r *http.Request, header, want string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if r.Header.Get(header) == "" || (want != "" && r.Header.Get(header) != want) || r.URL.Query().Get("resource") == "" || r.URL.Query().Get("api-version") == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}

	tok, exp := f.issue()
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": tok,
		"expires_on":   fmt.Sprint(exp.Unix()),
		"resource":     r.URL.Query().Get("resource"),
		"token_type":   "Bearer",
		"client_id":    r.URL.Query().Get("client_id"),
	})
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (f *Fake) vault(w http.ResponseWriter, r *http.Request, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	errJSON := func(status int, code, msg string) {
		writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
	}

	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	exp, ok := f.tokens[auth]
	if !ok || !f.Now().Before(exp) {
		errJSON(http.StatusUnauthorized, "Unauthorized", "AKV10000: Request is missing a Bearer or PoP token.")
		return
	}
	if r.URL.Query().Get("api-version") != "7.5" {
		errJSON(http.StatusBadRequest, "BadParameter", "unsupported api-version")
		return
	}
	if f.Deny {
		errJSON(http.StatusForbidden, "Forbidden", "The user, group or application does not have the required permission")
		return
	}

	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/") // keys, name, [version], [op]
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	name := parts[1]
	vs := f.versions[name]

	find := func(version string) *keyVersion {
		if version == "" && len(vs) > 0 {
			return &vs[len(vs)-1]
		}
		for i := range vs {
			if vs[i].version == version {
				return &vs[i]
			}
		}
		return nil
	}

	notFound := func() {
		errJSON(http.StatusNotFound, "KeyNotFound", "A key with this name was not found in this key vault.")
	}

	switch {
	case r.Method == http.MethodGet && (len(parts) == 2 || len(parts) == 3):
		version := ""
		if len(parts) == 3 {
			version = parts[2]
		}
		kv := find(version)
		if kv == nil {
			notFound()
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"key": map[string]any{
				"kid":     f.Server.URL + "/keys/" + name + "/" + kv.version,
				"kty":     f.KeyType,
				"key_ops": []string{"wrapKey", "unwrapKey", "encrypt", "decrypt"},
			},
			"attributes": map[string]any{"enabled": true},
		})

	case r.Method == http.MethodPost && len(parts) == 4 && (parts[3] == "wrapkey" || parts[3] == "unwrapkey"):
		kv := find(parts[2])
		if kv == nil {
			notFound()
			return
		}

		var req struct{ Alg, Value string }
		if err := json.Unmarshal([]byte(body), &req); err != nil || req.Alg != "RSA-OAEP-256" {
			errJSON(http.StatusBadRequest, "BadParameter", "bad request body")
			return
		}
		in, err := base64.RawURLEncoding.DecodeString(req.Value)
		if err != nil {
			errJSON(http.StatusBadRequest, "BadParameter", "bad value")
			return
		}

		var out []byte
		if parts[3] == "wrapkey" {
			out, err = rsa.EncryptOAEP(sha256.New(), rand.Reader, &kv.priv.PublicKey, in, nil)
		} else {
			out, err = rsa.DecryptOAEP(sha256.New(), nil, kv.priv, in, nil)
		}
		if err != nil {
			errJSON(http.StatusBadRequest, "BadParameter", "The parameter is incorrect; the operation failed")
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{"kid": f.Server.URL + "/keys/" + name + "/" + kv.version, "value": b64(out)})

	default:
		http.NotFound(w, r)
	}
}
