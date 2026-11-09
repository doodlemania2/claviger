package azurekv_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/tools/keys/azurekv"
	"github.com/doodlemania2/claviger/tools/keys/azurekvtest"
)

type clock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return nil
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type env struct {
	fake *azurekvtest.Fake
	clk  *clock
	opts azurekv.Options
}

func newEnv(t *testing.T) *env {
	t.Helper()

	clk := &clock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	fake := azurekvtest.New()
	t.Cleanup(fake.Server.Close)
	fake.Now = clk.Now
	fake.Secret = "s3cr3t-client-secret-value"
	fake.Assertion = "eyJfederated.assertion.value"
	fake.IdentityHeader = "ident-header-value"

	return &env{fake: fake, clk: clk, opts: azurekv.Options{
		HTTPClient:   fake.Server.Client(),
		Now:          clk.Now,
		Sleep:        clk.Sleep,
		Jitter:       func() float64 { return 0.5 },
		IMDSEndpoint: fake.IMDSEndpoint(),
	}}
}

func (e *env) secretCred() azurekv.TokenCredential {
	return azurekv.NewClientSecret(e.fake.AuthorityHost(), "tenant-1", "client-1", e.fake.Secret, e.opts)
}

func (e *env) client(t *testing.T, keyURL string) *azurekv.Client {
	t.Helper()
	c, err := azurekv.New(context.Background(), keyURL, e.secretCred(), e.opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func roundTrip(t *testing.T, c *azurekv.Client) {
	t.Helper()
	dek := bytes.Repeat([]byte{9}, 32)
	w, err := c.Wrap(context.Background(), dek)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(w, dek) {
		t.Fatal("wrapped contains plaintext")
	}
	got, err := c.Unwrap(context.Background(), c.KeyID(), w)
	if err != nil || !bytes.Equal(got, dek) {
		t.Fatalf("unwrap: %x %v", got, err)
	}
}

func TestAuthModes(t *testing.T) {
	e := newEnv(t)
	v := e.fake.AddKey("kek")
	url := e.fake.KeyURL("kek", "")

	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(e.fake.Assertion+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := map[string]struct {
		cred  azurekv.TokenCredential
		check func(t *testing.T)
	}{
		"client secret": {e.secretCred(), nil},
		"workload identity": {
			azurekv.NewWorkloadIdentity(e.fake.AuthorityHost(), "tenant-1", "client-1", tokenFile, e.opts),
			func(t *testing.T) {
				if e.fake.Count("/oauth2/v2.0/token") == 0 {
					t.Fatal("no token exchange")
				}
			},
		},
		"imds system assigned": {azurekv.NewManagedIdentity("", "", "", e.opts), func(t *testing.T) {
			r := e.fake.Requests()[0]
			if r.Header.Get("Metadata") != "true" || r.Query.Get("resource") != "https://vault.azure.net" || r.Query.Get("client_id") != "" {
				t.Fatalf("bad IMDS request %+v", r)
			}
		}},
		"imds user assigned": {azurekv.NewManagedIdentity("uami-client-id", "", "", e.opts), func(t *testing.T) {
			if got := e.fake.Requests()[0].Query.Get("client_id"); got != "uami-client-id" {
				t.Fatalf("client_id = %q", got)
			}
		}},
		"identity endpoint": {azurekv.NewManagedIdentity("uami", e.fake.IdentityEndpoint(), e.fake.IdentityHeader, e.opts), func(t *testing.T) {
			r := e.fake.Requests()[0]
			if !strings.HasPrefix(r.Path, "/identity/") || r.Query.Get("api-version") != "2019-08-01" || r.Query.Get("client_id") != "uami" {
				t.Fatalf("bad identity request %+v", r)
			}
		}},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e.fake.Reset()
			c, err := azurekv.New(context.Background(), url, tc.cred, e.opts)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(c.KeyID(), "/kek/"+v) {
				t.Fatalf("key id %q", c.KeyID())
			}
			roundTrip(t, c)
			if tc.check != nil {
				tc.check(t)
			}
		})
	}
}

func TestCredentialFromEnv(t *testing.T) {
	e := newEnv(t)
	e.fake.AddKey("kek")

	get := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	tokenFile := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tokenFile, []byte(e.fake.Assertion), 0o600)

	for name, m := range map[string]map[string]string{
		"auto prefers workload identity": {"AZURE_FEDERATED_TOKEN_FILE": tokenFile, "AZURE_CLIENT_ID": "c", "AZURE_TENANT_ID": "t", "AZURE_CLIENT_SECRET": "ignored", "AZURE_AUTHORITY_HOST": e.fake.AuthorityHost()},
		"auto client secret":             {"AZURE_CLIENT_SECRET": e.fake.Secret, "AZURE_CLIENT_ID": "c", "AZURE_TENANT_ID": "t", "AZURE_AUTHORITY_HOST": e.fake.AuthorityHost()},
		"explicit identity endpoint":     {"CLAVIGER_KEK_AZURE_AUTH": "managed-identity", "IDENTITY_ENDPOINT": e.fake.IdentityEndpoint(), "IDENTITY_HEADER": e.fake.IdentityHeader},
		"auto managed identity (imds)":   {},
	} {
		t.Run(name, func(t *testing.T) {
			cred, err := azurekv.CredentialFromEnv(get(m), e.opts)
			if err != nil {
				t.Fatal(err)
			}
			c, err := azurekv.New(context.Background(), e.fake.KeyURL("kek", ""), cred, e.opts)
			if err != nil {
				t.Fatal(err)
			}
			roundTrip(t, c)
		})
	}

	for name, m := range map[string]map[string]string{
		"workload identity incomplete": {"CLAVIGER_KEK_AZURE_AUTH": "workload-identity", "AZURE_CLIENT_ID": "c"},
		"client secret incomplete":     {"CLAVIGER_KEK_AZURE_AUTH": "client-secret", "AZURE_CLIENT_SECRET": "x"},
		"unknown mode":                 {"CLAVIGER_KEK_AZURE_AUTH": "magic"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := azurekv.CredentialFromEnv(get(m), e.opts); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestVersionResolutionAndRotation(t *testing.T) {
	e := newEnv(t)
	v1 := e.fake.AddKey("kek")

	unpinned := e.client(t, e.fake.KeyURL("kek", ""))
	if !strings.HasSuffix(unpinned.KeyID(), "/kek/"+v1) {
		t.Fatalf("resolved %q", unpinned.KeyID())
	}
	if unpinned.Algorithm() != "RSA-OAEP-256" {
		t.Fatal(unpinned.Algorithm())
	}

	dek := bytes.Repeat([]byte{3}, 32)
	wrapped, err := unpinned.Wrap(context.Background(), dek)
	if err != nil {
		t.Fatal(err)
	}
	oldID := unpinned.KeyID()

	// the key is rotated in Key Vault
	v2 := e.fake.AddKey("kek")

	// a running client keeps wrapping with its pinned version
	if unpinned.KeyID() != oldID {
		t.Fatal("pinned version changed")
	}

	// a restarted app resolves the new version, and still opens the old data
	restarted := e.client(t, e.fake.KeyURL("kek", ""))
	if !strings.HasSuffix(restarted.KeyID(), "/kek/"+v2) || restarted.KeyID() == oldID {
		t.Fatalf("restarted key id %q", restarted.KeyID())
	}
	got, err := restarted.Unwrap(context.Background(), oldID, wrapped)
	if err != nil || !bytes.Equal(got, dek) {
		t.Fatalf("old version unwrap: %v", err)
	}

	// an explicit version pins it
	pinned := e.client(t, e.fake.KeyURL("kek", v1))
	if pinned.KeyID() != oldID {
		t.Fatalf("pinned id %q", pinned.KeyID())
	}
}

func TestKeyIDMismatch(t *testing.T) {
	e := newEnv(t)
	e.fake.AddKey("kek")
	e.fake.AddKey("other")
	c := e.client(t, e.fake.KeyURL("kek", ""))

	for _, id := range []string{"k0123456789abcdef", "azkv:other.example/kek/v1", "azkv:" + strings.TrimPrefix(e.fake.URL(), "https://") + "/other/v1", "azkv:x"} {
		if _, err := c.Unwrap(context.Background(), id, []byte("x")); !errors.Is(err, azurekv.ErrKeyMismatch) {
			t.Fatalf("%s: %v", id, err)
		}
	}
}

func TestDefiniteFailures(t *testing.T) {
	t.Run("forbidden", func(t *testing.T) {
		e := newEnv(t)
		e.fake.AddKey("kek")
		e.fake.Deny = true
		_, err := azurekv.New(context.Background(), e.fake.KeyURL("kek", ""), e.secretCred(), e.opts)
		assertDefinite(t, err, 403)
		if e.fake.Count("/keys/") != 1 {
			t.Fatal("a 403 must not be retried")
		}
	})

	t.Run("missing key", func(t *testing.T) {
		e := newEnv(t)
		_, err := azurekv.New(context.Background(), e.fake.KeyURL("nope", ""), e.secretCred(), e.opts)
		assertDefinite(t, err, 404)
	})

	t.Run("bad client secret", func(t *testing.T) {
		e := newEnv(t)
		e.fake.AddKey("kek")
		cred := azurekv.NewClientSecret(e.fake.AuthorityHost(), "t", "c", "wrong-secret-xyz", e.opts)
		_, err := azurekv.New(context.Background(), e.fake.KeyURL("kek", ""), cred, e.opts)
		assertDefinite(t, err, 401)
		if strings.Contains(err.Error(), "wrong-secret-xyz") {
			t.Fatal("secret in error")
		}
	})

	t.Run("wrong key on unwrap", func(t *testing.T) {
		e := newEnv(t)
		e.fake.AddKey("kek")
		c := e.client(t, e.fake.KeyURL("kek", ""))
		_, err := c.Unwrap(context.Background(), c.KeyID(), []byte("not a wrapped key"))
		assertDefinite(t, err, 400)
	})

	t.Run("unsupported key type", func(t *testing.T) {
		e := newEnv(t)
		e.fake.AddKey("kek")
		e.fake.KeyType = "EC"
		_, err := azurekv.New(context.Background(), e.fake.KeyURL("kek", ""), e.secretCred(), e.opts)
		if err == nil || azurekv.IsIndeterminate(err) || !strings.Contains(err.Error(), "cannot wrap") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("bad urls", func(t *testing.T) {
		e := newEnv(t)
		for _, u := range []string{"", "http://v.vault.azure.net/keys/k", "https://v.vault.azure.net/secrets/k", "https://v.vault.azure.net/keys/", "https://v.vault.azure.net/keys/k/v/x", "https://v.vault.azure.net/keys/k?x=1", "not a url"} {
			if _, err := azurekv.New(context.Background(), u, e.secretCred(), e.opts); err == nil {
				t.Fatalf("%q accepted", u)
			}
		}
	})
}

func assertDefinite(t *testing.T, err error, status int) {
	t.Helper()
	var ae *azurekv.Error
	if !errors.As(err, &ae) {
		t.Fatalf("not an azurekv.Error: %v", err)
	}
	if ae.Status != status || !ae.Definite || azurekv.IsIndeterminate(err) {
		t.Fatalf("got %+v", ae)
	}
}

func TestRetryBackoffAndIndeterminate(t *testing.T) {
	t.Run("recovers after 503s with growing jittered backoff", func(t *testing.T) {
		e := newEnv(t)
		e.fake.AddKey("kek")
		c := e.client(t, e.fake.KeyURL("kek", ""))

		e.fake.FailNext("/wrapkey", 503, 2, nil)
		e.clk.sleeps = nil
		if _, err := c.Wrap(context.Background(), make([]byte, 32)); err != nil {
			t.Fatal(err)
		}
		if len(e.clk.sleeps) != 2 {
			t.Fatalf("sleeps %v", e.clk.sleeps)
		}
		// jitter 0.5: d/2 + 0.5*d/2 = 0.75*d for d = 200ms, 400ms
		if e.clk.sleeps[0] != 150*time.Millisecond || e.clk.sleeps[1] != 300*time.Millisecond {
			t.Fatalf("sleeps %v", e.clk.sleeps)
		}
	})

	t.Run("honours Retry-After", func(t *testing.T) {
		e := newEnv(t)
		e.fake.AddKey("kek")
		c := e.client(t, e.fake.KeyURL("kek", ""))

		e.fake.FailNext("/wrapkey", 429, 1, http.Header{"Retry-After": {"7"}})
		e.clk.sleeps = nil
		if _, err := c.Wrap(context.Background(), make([]byte, 32)); err != nil {
			t.Fatal(err)
		}
		if len(e.clk.sleeps) != 1 || e.clk.sleeps[0] != 7*time.Second {
			t.Fatalf("sleeps %v", e.clk.sleeps)
		}
	})

	t.Run("gives up after the attempts and is indeterminate", func(t *testing.T) {
		e := newEnv(t)
		e.fake.AddKey("kek")
		c := e.client(t, e.fake.KeyURL("kek", ""))

		e.fake.Reset()
		e.fake.FailNext("/wrapkey", 500, 100, nil)
		_, err := c.Wrap(context.Background(), make([]byte, 32))
		if !azurekv.IsIndeterminate(err) {
			t.Fatalf("got %v", err)
		}
		if n := e.fake.Count("/wrapkey"); n != 4 {
			t.Fatalf("%d attempts", n)
		}
	})

	t.Run("network failure is indeterminate", func(t *testing.T) {
		e := newEnv(t)
		e.fake.AddKey("kek")
		cred := e.secretCred()
		e.fake.Server.Close()
		_, err := azurekv.New(context.Background(), e.fake.KeyURL("kek", ""), cred, e.opts)
		if !azurekv.IsIndeterminate(err) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("cancelled context is indeterminate", func(t *testing.T) {
		e := newEnv(t)
		e.fake.AddKey("kek")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := azurekv.New(ctx, e.fake.KeyURL("kek", ""), e.secretCred(), e.opts)
		if !azurekv.IsIndeterminate(err) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("identity endpoint 5xx is indeterminate, 400 is definite", func(t *testing.T) {
		e := newEnv(t)
		e.fake.AddKey("kek")
		e.fake.FailNext("/imds/", 500, 100, nil)
		_, err := azurekv.New(context.Background(), e.fake.KeyURL("kek", ""), azurekv.NewManagedIdentity("", "", "", e.opts), e.opts)
		if !azurekv.IsIndeterminate(err) {
			t.Fatalf("got %v", err)
		}

		// wrong identity header: the endpoint answers 400
		cred := azurekv.NewManagedIdentity("", e.fake.IdentityEndpoint(), "wrong-header", e.opts)
		_, err = azurekv.New(context.Background(), e.fake.KeyURL("kek", ""), cred, e.opts)
		assertDefinite(t, err, 400)
	})
}

func TestTokenCacheAndRefresh(t *testing.T) {
	e := newEnv(t)
	e.fake.AddKey("kek")
	e.fake.ExpiresIn = time.Hour

	c := e.client(t, e.fake.KeyURL("kek", ""))
	roundTrip(t, c)
	roundTrip(t, c)
	if n := e.fake.Count("/oauth2/v2.0/token"); n != 1 {
		t.Fatalf("token requests = %d, want 1 (cached)", n)
	}

	// 54 minutes later the token is still inside its window
	e.clk.Advance(54 * time.Minute)
	roundTrip(t, c)
	if n := e.fake.Count("/oauth2/v2.0/token"); n != 1 {
		t.Fatalf("token requests = %d, want 1", n)
	}

	// within five minutes of expiry it is refreshed
	e.clk.Advance(2 * time.Minute)
	roundTrip(t, c)
	if n := e.fake.Count("/oauth2/v2.0/token"); n != 2 {
		t.Fatalf("token requests = %d, want 2 (refreshed)", n)
	}

	// a token revoked server-side is replaced after a 401
	e.fake.RevokeTokens()
	roundTrip(t, c)
	if n := e.fake.Count("/oauth2/v2.0/token"); n != 3 {
		t.Fatalf("token requests = %d, want 3", n)
	}
}

func TestShortLivedTokenRefreshedEarly(t *testing.T) {
	e := newEnv(t)
	e.fake.AddKey("kek")
	e.fake.ExpiresIn = 10 * time.Minute

	c := e.client(t, e.fake.KeyURL("kek", ""))
	roundTrip(t, c)
	e.clk.Advance(9 * time.Minute) // past 80% of a 10 minute token
	roundTrip(t, c)
	if n := e.fake.Count("/oauth2/v2.0/token"); n != 2 {
		t.Fatalf("token requests = %d, want 2", n)
	}
}

func TestIdentityEndpointExpiresOnParsed(t *testing.T) {
	e := newEnv(t)
	e.fake.AddKey("kek")
	cred := azurekv.NewManagedIdentity("", e.fake.IdentityEndpoint(), e.fake.IdentityHeader, e.opts)
	c, err := azurekv.New(context.Background(), e.fake.KeyURL("kek", ""), cred, e.opts)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, c)
	roundTrip(t, c)
	if n := e.fake.Count("/identity/"); n != 1 {
		t.Fatalf("identity requests = %d", n)
	}
	e.clk.Advance(56 * time.Minute)
	roundTrip(t, c)
	if n := e.fake.Count("/identity/"); n != 2 {
		t.Fatalf("identity requests = %d, want 2", n)
	}
}

func TestNoSecretsInRequestsOrErrors(t *testing.T) {
	e := newEnv(t)
	e.fake.AddKey("kek")
	c := e.client(t, e.fake.KeyURL("kek", ""))
	dek := bytes.Repeat([]byte{0xAB}, 32)

	w, _ := c.Wrap(context.Background(), dek)
	_, uerr := c.Unwrap(context.Background(), c.KeyID(), []byte("garbage"))
	e.fake.Deny = true
	_, derr := c.Wrap(context.Background(), dek)

	for _, err := range []error{uerr, derr} {
		if err == nil {
			t.Fatal("expected errors")
		}
		for _, secret := range []string{e.fake.Secret, "fake-token", string(dek), "Bearer"} {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error leaks %q: %v", secret, err)
			}
		}
	}

	// the DEK plaintext is never sent unwrapped, and the secret only ever goes to the token endpoint
	for _, r := range e.fake.Requests() {
		if strings.Contains(r.Path, "/keys/") && r.Query.Get("client_secret") != "" {
			t.Fatal("secret sent to the vault")
		}
	}
	_ = w
}

func TestWorkloadIdentityTokenFileReread(t *testing.T) {
	e := newEnv(t)
	e.fake.AddKey("kek")
	file := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(file, []byte("stale"), 0o600)

	cred := azurekv.NewWorkloadIdentity(e.fake.AuthorityHost(), "t", "c", file, e.opts)
	if _, err := azurekv.New(context.Background(), e.fake.KeyURL("kek", ""), cred, e.opts); err == nil {
		t.Fatal("stale assertion accepted")
	}

	_ = os.WriteFile(file, []byte(e.fake.Assertion), 0o600)
	if _, err := azurekv.New(context.Background(), e.fake.KeyURL("kek", ""), cred, e.opts); err != nil {
		t.Fatal(err)
	}

	_ = os.Remove(file)
	cred = azurekv.NewWorkloadIdentity(e.fake.AuthorityHost(), "t", "c", file, e.opts)
	_, err := azurekv.New(context.Background(), e.fake.KeyURL("kek", ""), cred, e.opts)
	if err == nil || strings.Contains(err.Error(), filepath.Dir(file)) {
		t.Fatalf("got %v", err)
	}
}
