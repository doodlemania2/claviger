package keys_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/tools/keys"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func b64Key(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func TestLoadFromEnvNoSource(t *testing.T) {
	_, err := keys.LoadFromEnv(env(nil), "PB_ENCRYPTION_KEY")
	if !errors.Is(err, keys.ErrNoKeySource) {
		t.Fatalf("expected ErrNoKeySource, got %v", err)
	}
	if !strings.Contains(err.Error(), "CLAVIGER_KEK") {
		t.Fatalf("error should name the variables: %v", err)
	}
}

func TestWrapUnwrapRoundTrip(t *testing.T) {
	src, err := keys.LoadFromEnv(env(map[string]string{keys.EnvKEK: b64Key(t)}), "")
	if err != nil {
		t.Fatal(err)
	}

	dek := bytes.Repeat([]byte{7}, 32)
	wrapped, err := src.Wrap(context.Background(), dek)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wrapped, dek) {
		t.Fatal("wrapped key contains the plaintext")
	}

	got, err := src.Unwrap(context.Background(), src.KeyID(), wrapped)
	if err != nil || !bytes.Equal(got, dek) {
		t.Fatalf("unwrap = %x, %v", got, err)
	}

	// tampering is detected
	wrapped[len(wrapped)-1] ^= 1
	if _, err := src.Unwrap(context.Background(), src.KeyID(), wrapped); !errors.Is(err, keys.ErrUnwrap) {
		t.Fatalf("expected ErrUnwrap, got %v", err)
	}
}

func TestWrongKEK(t *testing.T) {
	a, _ := keys.LoadFromEnv(env(map[string]string{keys.EnvKEK: b64Key(t)}), "")
	b, _ := keys.LoadFromEnv(env(map[string]string{keys.EnvKEK: b64Key(t)}), "")

	if a.KeyID() == b.KeyID() {
		t.Fatal("different KEKs must have different ids")
	}

	wrapped, _ := a.Wrap(context.Background(), make([]byte, 32))

	if _, err := b.Unwrap(context.Background(), a.KeyID(), wrapped); !errors.Is(err, keys.ErrKeyIDMismatch) {
		t.Fatalf("expected ErrKeyIDMismatch, got %v", err)
	}
}

func TestHKDFFromAppKey(t *testing.T) {
	appKey := strings.Repeat("a", 32)

	src, err := keys.LoadFromEnv(env(map[string]string{"PB_ENCRYPTION_KEY": appKey}), "PB_ENCRYPTION_KEY")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := keys.LoadFromEnv(env(map[string]string{"PB_ENCRYPTION_KEY": appKey}), "PB_ENCRYPTION_KEY")
	if src.KeyID() != again.KeyID() {
		t.Fatal("derivation must be deterministic")
	}

	// the derived KEK is not the raw app key
	raw := base64.StdEncoding.EncodeToString([]byte(appKey))
	direct, _ := keys.LoadFromEnv(env(map[string]string{keys.EnvKEK: raw}), "")
	if direct.KeyID() == src.KeyID() {
		t.Fatal("derived KEK must differ from the raw app key")
	}

	// wrong length
	if _, err := keys.LoadFromEnv(env(map[string]string{"PB_ENCRYPTION_KEY": "short"}), "PB_ENCRYPTION_KEY"); err == nil || errors.Is(err, keys.ErrNoKeySource) {
		t.Fatalf("expected a length error, got %v", err)
	}
}

func TestDedicatedKEKWinsOverAppKey(t *testing.T) {
	kek := b64Key(t)
	only, _ := keys.LoadFromEnv(env(map[string]string{keys.EnvKEK: kek}), "")
	both, err := keys.LoadFromEnv(env(map[string]string{keys.EnvKEK: kek, "PB_ENCRYPTION_KEY": strings.Repeat("x", 32)}), "PB_ENCRYPTION_KEY")
	if err != nil {
		t.Fatal(err)
	}
	if only.KeyID() != both.KeyID() {
		t.Fatal("CLAVIGER_KEK must be preferred")
	}
}

func TestInvalidKEK(t *testing.T) {
	for name, v := range map[string]string{
		"not base64": "!!!not-base64!!!",
		"too short":  base64.StdEncoding.EncodeToString([]byte("short")),
	} {
		_, err := keys.LoadFromEnv(env(map[string]string{keys.EnvKEK: v}), "")
		if err == nil || errors.Is(err, keys.ErrNoKeySource) {
			t.Fatalf("%s: expected an error, got %v", name, err)
		}
		if strings.Contains(err.Error(), v) {
			t.Fatalf("%s: the error must not echo the value", name)
		}
	}
}

func TestFileSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kek")
	kek := b64Key(t)

	if err := os.WriteFile(path, []byte(kek+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	fromFile, err := keys.LoadFromEnv(env(map[string]string{keys.EnvKEKFile: path}), "")
	if err != nil {
		t.Fatal(err)
	}
	if fromFile.Name() != "file" {
		t.Fatalf("name = %q", fromFile.Name())
	}
	direct, _ := keys.LoadFromEnv(env(map[string]string{keys.EnvKEK: kek}), "")
	if fromFile.KeyID() != direct.KeyID() {
		t.Fatal("same KEK must give the same id")
	}

	// both set is ambiguous
	if _, err := keys.LoadFromEnv(env(map[string]string{keys.EnvKEKFile: path, keys.EnvKEK: kek}), ""); err == nil {
		t.Fatal("expected an error when both are set")
	}

	// loose permissions are refused
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.LoadFromEnv(env(map[string]string{keys.EnvKEKFile: path}), ""); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("expected a permissions error, got %v", err)
	}

	// missing file
	if _, err := keys.LoadFromEnv(env(map[string]string{keys.EnvKEKFile: filepath.Join(dir, "nope")}), ""); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}
