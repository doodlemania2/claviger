// Package awskmstest is a fake AWS KMS (JSON API, SigV4-verified) for tests.
// It wraps with real AES-256-GCM, so a ciphertext only opens under its own key.
package awskmstest

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
)

const (
	Region    = "us-east-1"
	AccessKey = "AKIAFAKEFAKEFAKEFAKE"
	SecretKey = "fake-secret-access-key-for-tests"
)

// Fake is the test double. Create it with [New].
type Fake struct {
	Server *httptest.Server

	mu       sync.Mutex
	keys     map[string]*key // arn -> key
	aliases  map[string]string
	calls    []string
	failNext int // answer 503 this many times

	// Deny makes every KMS action answer AccessDeniedException.
	Deny bool
	// KeyState / KeyUsage override the DescribeKey answer ("" = Enabled / ENCRYPT_DECRYPT).
	KeyState string
	KeyUsage string
	// SessionToken, when set, must be sent in X-Amz-Security-Token.
	SessionToken string
}

type key struct {
	aead cipher.AEAD
	arn  string
}

// New starts a fake. Close it with Fake.Server.Close.
func New() *Fake {
	f := &Fake{keys: map[string]*key{}, aliases: map[string]string{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

// AddKey creates a key and returns its ARN.
func (f *Fake) AddKey(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	blk, _ := aes.NewCipher(b)
	g, _ := cipher.NewGCM(blk)
	arn := "arn:aws:kms:" + Region + ":111122223333:key/" + name
	f.keys[arn] = &key{aead: g, arn: arn}
	return arn
}

// AddAlias points alias/<name> at an existing key.
func (f *Fake) AddAlias(name, arn string) {
	f.mu.Lock()
	f.aliases["alias/"+name] = arn
	f.mu.Unlock()
}

// FailNext makes the next n requests answer 503.
func (f *Fake) FailNext(n int) { f.mu.Lock(); f.failNext = n; f.mu.Unlock() }

// Calls returns the KMS actions received, in order.
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func jsonErr(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"__type": typ, "message": msg})
}

func (f *Fake) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	f.mu.Lock()
	if f.failNext > 0 {
		f.failNext--
		f.mu.Unlock()
		jsonErr(w, 503, "ServiceUnavailableException", "try later")
		return
	}
	f.mu.Unlock()

	if err := f.verify(r, body); err != "" {
		jsonErr(w, 400, "InvalidSignatureException", err)
		return
	}

	action := strings.TrimPrefix(r.Header.Get("X-Amz-Target"), "TrentService.")
	f.mu.Lock()
	f.calls = append(f.calls, action)
	deny := f.Deny
	f.mu.Unlock()

	if deny {
		jsonErr(w, 400, "AccessDeniedException", "User is not authorized to perform kms:"+action)
		return
	}

	var in struct {
		KeyId             string
		Plaintext         string
		CiphertextBlob    string
		EncryptionContext map[string]string
	}
	_ = json.Unmarshal(body, &in)

	k := f.lookup(in.KeyId)
	if k == nil {
		jsonErr(w, 400, "NotFoundException", "Key '"+in.KeyId+"' does not exist")
		return
	}

	aad := func() []byte {
		keys := make([]string, 0, len(in.EncryptionContext))
		for n := range in.EncryptionContext {
			keys = append(keys, n)
		}
		sort.Strings(keys)
		var sb strings.Builder
		for _, n := range keys {
			sb.WriteString(n + "=" + in.EncryptionContext[n] + ";")
		}
		return []byte(sb.String())
	}()

	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	switch action {
	case "DescribeKey":
		state, usage := f.KeyState, f.KeyUsage
		if state == "" {
			state = "Enabled"
		}
		if usage == "" {
			usage = "ENCRYPT_DECRYPT"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"KeyMetadata": map[string]any{
			"Arn": k.arn, "Enabled": state == "Enabled", "KeyState": state, "KeyUsage": usage, "KeySpec": "SYMMETRIC_DEFAULT",
		}})
	case "Encrypt":
		pt, err := base64.StdEncoding.DecodeString(in.Plaintext)
		if err != nil {
			jsonErr(w, 400, "ValidationException", "bad plaintext")
			return
		}
		n := make([]byte, k.aead.NonceSize())
		_, _ = rand.Read(n)
		ct := k.aead.Seal(n, n, pt, aad)
		_ = json.NewEncoder(w).Encode(map[string]string{"CiphertextBlob": base64.StdEncoding.EncodeToString(ct), "KeyId": k.arn})
	case "Decrypt":
		ct, err := base64.StdEncoding.DecodeString(in.CiphertextBlob)
		ns := k.aead.NonceSize()
		if err != nil || len(ct) < ns {
			jsonErr(w, 400, "InvalidCiphertextException", "")
			return
		}
		pt, err := k.aead.Open(nil, ct[:ns], ct[ns:], aad)
		if err != nil {
			jsonErr(w, 400, "IncorrectKeyException", "The key ID in the request does not identify a CMK that can perform this operation.")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"Plaintext": base64.StdEncoding.EncodeToString(pt), "KeyId": k.arn})
	default:
		jsonErr(w, 400, "UnknownOperationException", action)
	}
}

func (f *Fake) lookup(id string) *key {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.aliases[id]; ok {
		id = a
	}
	if k, ok := f.keys[id]; ok {
		return k
	}
	// bare key id
	return f.keys["arn:aws:kms:"+Region+":111122223333:key/"+id]
}

func mac(key []byte, s string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(s))
	return m.Sum(nil)
}

// verify re-computes the SigV4 signature independently of the client code.
func (f *Fake) verify(r *http.Request, body []byte) string {
	auth := r.Header.Get("Authorization")
	rest, ok := strings.CutPrefix(auth, "AWS4-HMAC-SHA256 ")
	if !ok {
		return "missing signature"
	}
	fields := map[string]string{}
	for _, p := range strings.Split(rest, ", ") {
		k, v, _ := strings.Cut(p, "=")
		fields[k] = v
	}
	cred := strings.Split(fields["Credential"], "/")
	if len(cred) != 5 || cred[0] != AccessKey {
		return "unknown access key"
	}
	if f.SessionToken != "" && r.Header.Get("X-Amz-Security-Token") != f.SessionToken {
		return "bad security token"
	}
	signed := strings.Split(fields["SignedHeaders"], ";")
	var ch strings.Builder
	for _, h := range signed {
		v := r.Header.Get(h)
		if h == "host" {
			v = r.Host
		}
		ch.WriteString(h + ":" + strings.Join(strings.Fields(v), " ") + "\n")
	}
	bh := sha256.Sum256(body)
	canon := strings.Join([]string{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, ch.String(), fields["SignedHeaders"], hex.EncodeToString(bh[:])}, "\n")
	ch2 := sha256.Sum256([]byte(canon))
	scope := strings.Join(cred[1:], "/")
	sts := "AWS4-HMAC-SHA256\n" + r.Header.Get("X-Amz-Date") + "\n" + scope + "\n" + hex.EncodeToString(ch2[:])
	k := mac([]byte("AWS4"+SecretKey), cred[1])
	k = mac(k, cred[2])
	k = mac(k, cred[3])
	k = mac(k, "aws4_request")
	if cred[3] != "kms" || cred[2] != Region {
		return fmt.Sprintf("bad scope %s", scope)
	}
	if hex.EncodeToString(mac(k, sts)) != fields["Signature"] {
		return "signature mismatch"
	}
	return ""
}
