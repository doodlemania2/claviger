package gcpkms_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/tools/keys/gcpkms"
	"github.com/doodlemania2/claviger/tools/keys/gcpkmstest"
	"github.com/doodlemania2/claviger/tools/keys/kmshttp"
)

func opts(f *gcpkmstest.Fake) kmshttp.Options {
	return kmshttp.Options{
		MaxAttempts:      3,
		Sleep:            func(context.Context, time.Duration) error { return nil },
		Endpoint:         f.Server.URL,
		MetadataEndpoint: f.Server.URL,
	}
}

func newClient(t *testing.T, f *gcpkmstest.Fake, name string, env map[string]string) (*gcpkms.Client, error) {
	t.Helper()
	o := opts(f)
	ts, err := gcpkms.TokenSourceFromEnv(func(k string) string { return env[k] }, o)
	if err != nil {
		return nil, err
	}
	return gcpkms.New(context.Background(), name, ts, o)
}

func TestRoundTripWithMetadataAndServiceAccount(t *testing.T) {
	f := gcpkmstest.New()
	defer f.Server.Close()
	name := f.AddKey("kek1")
	sa := f.WriteServiceAccount(t)

	for label, env := range map[string]map[string]string{
		"metadata":        {},
		"service account": {"GOOGLE_APPLICATION_CREDENTIALS": sa},
	} {
		c, err := newClient(t, f, name, env)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if c.KeyID() != "gcpkms:"+name {
			t.Fatalf("key id %q", c.KeyID())
		}

		dek := bytes.Repeat([]byte{9}, 32)
		w, err := c.Wrap(context.Background(), dek)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		got, err := c.Unwrap(context.Background(), c.KeyID(), w)
		if err != nil || !bytes.Equal(got, dek) {
			t.Fatalf("%s: unwrap: %v", label, err)
		}
	}
	if f.TokenRequests() < 2 {
		t.Fatalf("token requests: %d", f.TokenRequests())
	}
}

func TestWrongKeyAndAlteredValue(t *testing.T) {
	f := gcpkmstest.New()
	defer f.Server.Close()
	a, b := f.AddKey("a"), f.AddKey("b")

	ca, _ := newClient(t, f, a, nil)
	cb, _ := newClient(t, f, b, nil)
	w, _ := ca.Wrap(context.Background(), []byte("0123456789abcdef0123456789abcdef"))

	if _, err := cb.Unwrap(context.Background(), ca.KeyID(), w); !errors.Is(err, gcpkms.ErrKeyMismatch) {
		t.Fatalf("got %v", err)
	}
	// right id, ciphertext of another key (also binds the AAD): definite refusal
	for _, in := range [][]byte{w, []byte("garbage")} {
		_, err := cb.Unwrap(context.Background(), cb.KeyID(), in)
		if err == nil || kmshttp.IsIndeterminate(err) {
			t.Fatalf("expected a definite error, got %v", err)
		}
	}
}

func TestKeyChecksAndDenied(t *testing.T) {
	f := gcpkmstest.New()
	defer f.Server.Close()
	name := f.AddKey("k")

	if _, err := newClient(t, f, "projects/p/locations/global/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1", nil); err == nil || !strings.Contains(err.Error(), "must look like") {
		t.Fatalf("got %v", err)
	}
	if _, err := newClient(t, f, gcpkmstest.KeyName("missing"), nil); err == nil || kmshttp.IsIndeterminate(err) || !strings.Contains(err.Error(), "NOT_FOUND") {
		t.Fatalf("got %v", err)
	}

	f.Purpose = "ASYMMETRIC_SIGN"
	if _, err := newClient(t, f, name, nil); err == nil || !strings.Contains(err.Error(), "ENCRYPT_DECRYPT") {
		t.Fatalf("got %v", err)
	}
	f.Purpose, f.PrimaryState = "", "DISABLED"
	if _, err := newClient(t, f, name, nil); err == nil || !strings.Contains(err.Error(), "not ENABLED") {
		t.Fatalf("got %v", err)
	}
	f.PrimaryState, f.Deny = "", true
	if _, err := newClient(t, f, name, nil); err == nil || kmshttp.IsIndeterminate(err) || !strings.Contains(err.Error(), "PERMISSION_DENIED") {
		t.Fatalf("got %v", err)
	}
}

func TestUnreachableIsIndeterminateAndRetries(t *testing.T) {
	f := gcpkmstest.New()
	name := f.AddKey("k")
	c, err := newClient(t, f, name, nil)
	if err != nil {
		t.Fatal(err)
	}

	f.FailNext(2)
	if _, err := c.Wrap(context.Background(), []byte("0123456789abcdef0123456789abcdef")); err != nil {
		t.Fatalf("retry failed: %v", err)
	}

	f.FailNext(100)
	if _, err := c.Wrap(context.Background(), []byte("x")); err == nil || !kmshttp.IsIndeterminate(err) {
		t.Fatalf("got %v", err)
	}

	f.Server.Close()
	if _, err := newClient(t, f, name, nil); err == nil || !kmshttp.IsIndeterminate(err) {
		t.Fatalf("got %v", err)
	}
}

func TestExpiredTokenIsRefreshedOnce(t *testing.T) {
	f := gcpkmstest.New()
	defer f.Server.Close()
	name := f.AddKey("k")
	c, err := newClient(t, f, name, nil)
	if err != nil {
		t.Fatal(err)
	}

	before := f.TokenRequests()
	f.AccessToken = "rotated" // the cached token is now rejected with 401
	if _, err := c.Wrap(context.Background(), []byte("0123456789abcdef0123456789abcdef")); err != nil {
		t.Fatal(err)
	}
	if f.TokenRequests() != before+1 {
		t.Fatalf("token requests %d -> %d", before, f.TokenRequests())
	}
}

func TestBadCredentialFiles(t *testing.T) {
	f := gcpkmstest.New()
	defer f.Server.Close()
	o := opts(f)
	load := func(content string) error {
		p := filepath.Join(t.TempDir(), "c.json")
		_ = os.WriteFile(p, []byte(content), 0o600)
		_, err := gcpkms.TokenSourceFromEnv(func(k string) string {
			if k == "GOOGLE_APPLICATION_CREDENTIALS" {
				return p
			}
			return ""
		}, o)
		return err
	}

	if err := load(`{"type":"authorized_user"}`); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("got %v", err)
	}
	if err := load(`{"type":"external_account"}`); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("got %v", err)
	}
	if err := load(`not json`); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := gcpkms.TokenSourceFromEnv(func(k string) string {
		if k == "GOOGLE_APPLICATION_CREDENTIALS" {
			return "/nonexistent/sa.json"
		}
		return ""
	}, o); err == nil {
		t.Fatal("expected an error for a missing file")
	}

	// tokens from an issuer the KMS does not trust: definite 401
	other := gcpkmstest.New()
	other.AccessToken = "other-token"
	defer other.Server.Close()
	sa := other.WriteServiceAccount(t)
	name := f.AddKey("k")
	_, err := newClient(t, f, name, map[string]string{"GOOGLE_APPLICATION_CREDENTIALS": sa})
	if err == nil || kmshttp.IsIndeterminate(err) {
		t.Fatalf("got %v", err)
	}
}
