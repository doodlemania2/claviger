package keys_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/tools/keys"
	"github.com/doodlemania2/claviger/tools/keys/azurekv"
	"github.com/doodlemania2/claviger/tools/keys/azurekvtest"
)

// Fork-local (Claviger #75, M4).

func azureFake(t *testing.T) (*azurekvtest.Fake, map[string]string) {
	t.Helper()

	f := azurekvtest.New()
	t.Cleanup(f.Server.Close)
	f.Secret = "dev-client-secret"

	restore := keys.SetAzureClientOptions(func() azurekv.Options {
		return azurekv.Options{
			HTTPClient:  f.Server.Client(),
			MaxAttempts: 2,
			Sleep:       func(context.Context, time.Duration) error { return nil },
		}
	})
	t.Cleanup(restore)

	return f, map[string]string{
		"AZURE_CLIENT_ID":      "client",
		"AZURE_TENANT_ID":      "tenant",
		"AZURE_CLIENT_SECRET":  f.Secret,
		"AZURE_AUTHORITY_HOST": f.AuthorityHost(),
	}
}

func merge(ms ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range ms {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func TestAzureSourceRoundTripAndErrors(t *testing.T) {
	f, base := azureFake(t)
	f.AddKey("kek")

	src, err := keys.LoadFromEnv(env(merge(base, map[string]string{keys.EnvKEKAzureKeyID: f.KeyURL("kek", "")})), "")
	if err != nil {
		t.Fatal(err)
	}
	if src.Name() != "azure-key-vault" || !strings.HasPrefix(src.KeyID(), "azkv:") {
		t.Fatalf("%s %s", src.Name(), src.KeyID())
	}

	dek := bytes.Repeat([]byte{5}, 32)
	w, err := src.Wrap(context.Background(), dek)
	if err != nil {
		t.Fatal(err)
	}
	got, err := src.Unwrap(context.Background(), src.KeyID(), w)
	if err != nil || !bytes.Equal(got, dek) {
		t.Fatalf("%v", err)
	}

	// a key id from another source
	if _, err := src.Unwrap(context.Background(), "k0123456789abcdef", w); !errors.Is(err, keys.ErrKeyIDMismatch) {
		t.Fatalf("got %v", err)
	}

	// an altered value is a definite unwrap failure
	_, err = src.Unwrap(context.Background(), src.KeyID(), []byte("garbage"))
	if !errors.Is(err, keys.ErrUnwrap) || keys.IsIndeterminate(err) {
		t.Fatalf("got %v", err)
	}

	// an outage is indeterminate and is not reported as a wrong key
	f.FailNext("/unwrapkey", 503, 10, nil)
	_, err = src.Unwrap(context.Background(), src.KeyID(), w)
	if !keys.IsIndeterminate(err) || errors.Is(err, keys.ErrUnwrap) {
		t.Fatalf("got %v", err)
	}
}

func TestAzureSourceExcludesOtherSources(t *testing.T) {
	f, base := azureFake(t)
	f.AddKey("kek")
	url := f.KeyURL("kek", "")

	for _, other := range []string{keys.EnvKEK, keys.EnvKEKFile} {
		_, err := keys.LoadFromEnv(env(merge(base, map[string]string{keys.EnvKEKAzureKeyID: url, other: b64Key(t)})), "")
		if err == nil || !strings.Contains(err.Error(), keys.EnvKEKAzureKeyID) {
			t.Fatalf("%s: got %v", other, err)
		}
	}

	// the old-key variables are mutually exclusive too
	_, err := keys.LoadOldFromEnv(env(map[string]string{keys.EnvKEKOld: b64Key(t), keys.EnvKEKAzureKeyIDOld: url}), "")
	if err == nil || !strings.Contains(err.Error(), keys.EnvKEKAzureKeyIDOld) {
		t.Fatalf("got %v", err)
	}

	// the app-key fallback does not conflict: Key Vault takes precedence
	src, err := keys.LoadFromEnv(env(merge(base, map[string]string{keys.EnvKEKAzureKeyID: url, "PB_ENCRYPTION_KEY": strings.Repeat("x", 32)})), "PB_ENCRYPTION_KEY")
	if err != nil || src.Name() != "azure-key-vault" {
		t.Fatalf("%v", err)
	}
}

func TestAzureLoadErrors(t *testing.T) {
	f, base := azureFake(t)
	f.AddKey("kek")

	// bad credentials: definite
	_, err := keys.LoadFromEnv(env(merge(base, map[string]string{keys.EnvKEKAzureKeyID: f.KeyURL("kek", ""), "AZURE_CLIENT_SECRET": "wrong"})), "")
	if err == nil || keys.IsIndeterminate(err) || strings.Contains(err.Error(), "wrong") {
		t.Fatalf("got %v", err)
	}

	// unknown key: definite
	_, err = keys.LoadFromEnv(env(merge(base, map[string]string{keys.EnvKEKAzureKeyID: f.KeyURL("missing", "")})), "")
	if err == nil || keys.IsIndeterminate(err) {
		t.Fatalf("got %v", err)
	}

	// outage: indeterminate
	f.FailNext("/keys/", 503, 10, nil)
	_, err = keys.LoadFromEnv(env(merge(base, map[string]string{keys.EnvKEKAzureKeyID: f.KeyURL("kek", "")})), "")
	if !keys.IsIndeterminate(err) {
		t.Fatalf("got %v", err)
	}

	// malformed URL
	_, err = keys.LoadFromEnv(env(merge(base, map[string]string{keys.EnvKEKAzureKeyID: "https://x/secrets/a"})), "")
	if err == nil || keys.IsIndeterminate(err) {
		t.Fatalf("got %v", err)
	}
}

func TestMultiRewrapBetweenEnvAndAzure(t *testing.T) {
	f, base := azureFake(t)
	f.AddKey("kek")
	url := f.KeyURL("kek", "")
	envKEK := b64Key(t)
	ctx := context.Background()
	dek := bytes.Repeat([]byte{1}, 32)

	// env KEK -> Key Vault
	envSrc, _ := keys.LoadFromEnv(env(map[string]string{keys.EnvKEK: envKEK}), "")
	wrappedEnv, _ := envSrc.Wrap(ctx, dek)

	toAzure, err := keys.LoadWithOld(env(merge(base, map[string]string{keys.EnvKEKAzureKeyID: url, keys.EnvKEKOld: envKEK})), "")
	if err != nil {
		t.Fatal(err)
	}
	m := toAzure.(*keys.Multi)
	if !m.HasOld() || !m.Has(envSrc.KeyID()) {
		t.Fatal("old env KEK not available")
	}
	got, err := toAzure.Unwrap(ctx, envSrc.KeyID(), wrappedEnv)
	if err != nil || !bytes.Equal(got, dek) {
		t.Fatalf("%v", err)
	}
	wrappedAz, err := toAzure.Wrap(ctx, got)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := toAzure.Unwrap(ctx, toAzure.KeyID(), wrappedAz); err != nil || !bytes.Equal(got, dek) {
		t.Fatalf("%v", err)
	}

	// Key Vault -> env KEK (back)
	back, err := keys.LoadWithOld(env(merge(base, map[string]string{keys.EnvKEK: envKEK, keys.EnvKEKAzureKeyIDOld: url})), "")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := back.Unwrap(ctx, toAzure.KeyID(), wrappedAz); err != nil || !bytes.Equal(got, dek) {
		t.Fatalf("%v", err)
	}

	// the app cannot read Key Vault data with only the env KEK
	only, _ := keys.LoadWithOld(env(map[string]string{keys.EnvKEK: envKEK}), "")
	if _, err := only.Unwrap(ctx, toAzure.KeyID(), wrappedAz); !errors.Is(err, keys.ErrKeyIDMismatch) {
		t.Fatalf("got %v", err)
	}
}

func TestAzureKeyVaultRotationIsAKEKRotation(t *testing.T) {
	f, base := azureFake(t)
	f.AddKey("kek")
	cfg := merge(base, map[string]string{keys.EnvKEKAzureKeyID: f.KeyURL("kek", "")})
	ctx := context.Background()
	dek := bytes.Repeat([]byte{8}, 32)

	v1, _ := keys.LoadWithOld(env(cfg), "")
	w1, _ := v1.Wrap(ctx, dek)

	f.AddKey("kek") // rotated in Key Vault

	v2, err := keys.LoadWithOld(env(cfg), "")
	if err != nil {
		t.Fatal(err)
	}
	if v2.KeyID() == v1.KeyID() {
		t.Fatal("the new version must have a new key id (so that rewrap sees it)")
	}
	if got, err := v2.Unwrap(ctx, v1.KeyID(), w1); err != nil || !bytes.Equal(got, dek) {
		t.Fatalf("old data must stay readable: %v", err)
	}
}
