package gcpkms

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/doodlemania2/claviger/tools/keys/kmshttp"
)

// Scope is the OAuth2 scope requested for Cloud KMS.
const Scope = "https://www.googleapis.com/auth/cloudkms"

const defaultMetadata = "http://metadata.google.internal"

// ErrNoCredentials is returned when neither a service account file nor the metadata server is usable.
var ErrNoCredentials = errors.New("no Google credentials found (set GOOGLE_APPLICATION_CREDENTIALS to a service account JSON key file, or run on Google Cloud with a service account attached)")

// TokenSource yields OAuth2 access tokens.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
	Invalidate()
}

type cachedToken struct {
	mu      sync.Mutex
	tok     string
	exp     time.Time
	now     func() time.Time
	refresh func(ctx context.Context) (string, time.Time, error)
}

func (c *cachedToken) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tok != "" && c.now().Add(time.Minute).Before(c.exp) {
		return c.tok, nil
	}
	t, exp, err := c.refresh(ctx)
	if err != nil {
		return "", err
	}
	c.tok, c.exp = t, exp
	return t, nil
}

func (c *cachedToken) Invalidate() { c.mu.Lock(); c.tok = ""; c.mu.Unlock() }

// TokenSourceFromEnv selects the credential: the service account key file in
// GOOGLE_APPLICATION_CREDENTIALS when set, else the metadata server (GCE, GKE
// Workload Identity, Cloud Run, ...). Authorized-user and external-account
// (workload identity federation) files are not supported.
func TokenSourceFromEnv(getenv func(string) string, opts kmshttp.Options) (TokenSource, error) {
	tr := kmshttp.NewTransport("gcp auth", opts)

	if path := strings.TrimSpace(getenv("GOOGLE_APPLICATION_CREDENTIALS")); path != "" {
		sa, err := loadServiceAccount(path)
		if err != nil {
			return nil, err
		}
		return &cachedToken{now: tr.Now, refresh: func(ctx context.Context) (string, time.Time, error) {
			return sa.token(ctx, tr)
		}}, nil
	}

	base := opts.MetadataEndpoint
	if base == "" {
		base = defaultMetadata
		if h := getenv("GCE_METADATA_HOST"); h != "" {
			base = "http://" + h
		}
	}
	return &cachedToken{now: tr.Now, refresh: func(ctx context.Context) (string, time.Time, error) {
		return metadataToken(ctx, tr, base)
	}}, nil
}

type tokenResp struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

func (r tokenResp) result(now time.Time) (string, time.Time, error) {
	if r.AccessToken == "" {
		return "", time.Time{}, errors.New("the token response has no access token")
	}
	if r.ExpiresIn <= 0 {
		r.ExpiresIn = 300
	}
	return r.AccessToken, now.Add(time.Duration(r.ExpiresIn) * time.Second), nil
}

func authErr(op string, status int, err error) error {
	return &kmshttp.Error{Service: "gcp auth", Op: op, Status: status, Definite: true, Err: err}
}

func metadataToken(ctx context.Context, tr *kmshttp.Transport, base string) (string, time.Time, error) {
	status, body, err := tr.Do(ctx, "metadata token", nil, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			base+"/computeMetadata/v1/instance/service-accounts/default/token?scopes="+url.QueryEscape(Scope), nil)
		if err == nil {
			req.Header.Set("Metadata-Flavor", "Google")
		}
		return req, err
	})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%w: %w", ErrNoCredentials, err)
	}
	if status != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("%w (the metadata server answered HTTP %d)", ErrNoCredentials, status)
	}
	var r tokenResp
	if json.Unmarshal(body, &r) != nil {
		return "", time.Time{}, authErr("metadata token", status, errors.New("the response is not valid JSON"))
	}
	t, exp, err := r.result(tr.Now())
	if err != nil {
		return "", time.Time{}, authErr("metadata token", status, err)
	}
	return t, exp, nil
}

type serviceAccount struct {
	email    string
	keyID    string
	tokenURI string
	key      *rsa.PrivateKey
}

func loadServiceAccount(path string) (*serviceAccount, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("GOOGLE_APPLICATION_CREDENTIALS: cannot read the file: %v", err)
	}

	var f struct {
		Type         string `json:"type"`
		ClientEmail  string `json:"client_email"`
		PrivateKey   string `json:"private_key"`
		PrivateKeyID string `json:"private_key_id"`
		TokenURI     string `json:"token_uri"`
	}
	if json.Unmarshal(raw, &f) != nil {
		return nil, errors.New("GOOGLE_APPLICATION_CREDENTIALS: the file is not valid JSON")
	}
	if f.Type != "service_account" {
		return nil, fmt.Errorf("GOOGLE_APPLICATION_CREDENTIALS: credential type %q is not supported; use a service account key file, or run on Google Cloud with an attached service account", f.Type)
	}
	if f.ClientEmail == "" || f.PrivateKey == "" {
		return nil, errors.New("GOOGLE_APPLICATION_CREDENTIALS: the service account file has no client_email or private_key")
	}
	if f.TokenURI == "" {
		f.TokenURI = "https://oauth2.googleapis.com/token"
	}

	blk, _ := pem.Decode([]byte(f.PrivateKey))
	if blk == nil {
		return nil, errors.New("GOOGLE_APPLICATION_CREDENTIALS: the private key is not PEM")
	}
	var key *rsa.PrivateKey
	if k, err := x509.ParsePKCS8PrivateKey(blk.Bytes); err == nil {
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("GOOGLE_APPLICATION_CREDENTIALS: the private key is not RSA")
		}
		key = rk
	} else if k, err := x509.ParsePKCS1PrivateKey(blk.Bytes); err == nil {
		key = k
	} else {
		return nil, errors.New("GOOGLE_APPLICATION_CREDENTIALS: the private key cannot be parsed")
	}

	return &serviceAccount{email: f.ClientEmail, keyID: f.PrivateKeyID, tokenURI: f.TokenURI, key: key}, nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (s *serviceAccount) assertion(now time.Time) (string, error) {
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": s.keyID})
	claims, _ := json.Marshal(map[string]any{
		"iss": s.email, "scope": Scope, "aud": s.tokenURI,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})
	signing := b64(hdr) + "." + b64(claims)
	h := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, h[:])
	if err != nil {
		return "", err
	}
	return signing + "." + b64(sig), nil
}

func (s *serviceAccount) token(ctx context.Context, tr *kmshttp.Transport) (string, time.Time, error) {
	status, body, err := tr.Do(ctx, "token", nil, func(ctx context.Context) (*http.Request, error) {
		a, err := s.assertion(tr.Now())
		if err != nil {
			return nil, err
		}
		form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {a}}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURI, strings.NewReader(form.Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		return req, err
	})
	if err != nil {
		return "", time.Time{}, err
	}
	if status != http.StatusOK {
		var e struct {
			Error string `json:"error"`
			Desc  string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &e)
		return "", time.Time{}, &kmshttp.Error{Service: "gcp auth", Op: "token", Status: status, Definite: true,
			Code: kmshttp.Shorten(e.Error), Message: kmshttp.Shorten(e.Desc)}
	}
	var r tokenResp
	if json.Unmarshal(body, &r) != nil {
		return "", time.Time{}, authErr("token", status, errors.New("the response is not valid JSON"))
	}
	t, exp, err := r.result(tr.Now())
	if err != nil {
		return "", time.Time{}, authErr("token", status, err)
	}
	return t, exp, nil
}
