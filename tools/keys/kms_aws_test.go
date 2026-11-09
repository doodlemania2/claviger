//go:build kms_aws

package keys_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/tools/keys"
	"github.com/doodlemania2/claviger/tools/keys/awskmstest"
	"github.com/doodlemania2/claviger/tools/keys/kmshttp"
)

// Fork-local (Claviger #75): AWS KMS source; runs with -tags kms_aws.

func awsFake(t *testing.T) (*awskmstest.Fake, map[string]string) {
	t.Helper()

	f := awskmstest.New()
	t.Cleanup(f.Server.Close)

	restore := keys.SetAWSClientOptions(func() kmshttp.Options {
		return kmshttp.Options{
			Endpoint:    f.Server.URL,
			MaxAttempts: 2,
			Sleep:       func(context.Context, time.Duration) error { return nil },
		}
	})
	t.Cleanup(restore)

	return f, map[string]string{
		"AWS_ACCESS_KEY_ID":     awskmstest.AccessKey,
		"AWS_SECRET_ACCESS_KEY": awskmstest.SecretKey,
		"AWS_REGION":            awskmstest.Region,
	}
}

func TestAWSSourceRoundTripAndErrors(t *testing.T) {
	f, base := awsFake(t)
	arn := f.AddKey("kek")
	ctx := context.Background()

	src, err := keys.LoadFromEnv(env(merge(base, map[string]string{keys.EnvKEKAWSKeyID: arn})), "")
	if err != nil {
		t.Fatal(err)
	}
	if src.Name() != "aws-kms" || src.KeyID() != "awskms:"+arn {
		t.Fatalf("%s %s", src.Name(), src.KeyID())
	}

	dek := bytes.Repeat([]byte{5}, 32)
	w, err := src.Wrap(ctx, dek)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := src.Unwrap(ctx, src.KeyID(), w); err != nil || !bytes.Equal(got, dek) {
		t.Fatalf("%v", err)
	}

	// a key id from another source
	if _, err := src.Unwrap(ctx, "k0123456789abcdef", w); !errors.Is(err, keys.ErrKeyIDMismatch) {
		t.Fatalf("got %v", err)
	}

	// altered value: definite
	if _, err := src.Unwrap(ctx, src.KeyID(), []byte("garbage")); !errors.Is(err, keys.ErrUnwrap) || keys.IsIndeterminate(err) {
		t.Fatalf("got %v", err)
	}

	// wrong key: ciphertext from another KMS key under the same id
	other := f.AddKey("other")
	srcOther, _ := keys.LoadFromEnv(env(merge(base, map[string]string{keys.EnvKEKAWSKeyID: other})), "")
	wOther, _ := srcOther.Wrap(ctx, dek)
	if _, err := src.Unwrap(ctx, src.KeyID(), wOther); !errors.Is(err, keys.ErrUnwrap) || keys.IsIndeterminate(err) {
		t.Fatalf("got %v", err)
	}

	// an outage is indeterminate and not reported as a wrong key
	f.FailNext(10)
	if _, err := src.Unwrap(ctx, src.KeyID(), w); !keys.IsIndeterminate(err) || errors.Is(err, keys.ErrUnwrap) {
		t.Fatalf("got %v", err)
	}
}

func TestAWSLoadFailsClosed(t *testing.T) {
	f, base := awsFake(t)
	arn := f.AddKey("kek")
	cfg := merge(base, map[string]string{keys.EnvKEKAWSKeyID: arn})

	// bad credentials: definite, and the secret is not echoed
	_, err := keys.LoadFromEnv(env(merge(cfg, map[string]string{"AWS_SECRET_ACCESS_KEY": "wrong-secret"})), "")
	if err == nil || keys.IsIndeterminate(err) || strings.Contains(err.Error(), "wrong-secret") {
		t.Fatalf("got %v", err)
	}

	// unknown key: definite
	_, err = keys.LoadFromEnv(env(merge(base, map[string]string{keys.EnvKEKAWSKeyID: "arn:aws:kms:us-east-1:111122223333:key/missing"})), "")
	if err == nil || keys.IsIndeterminate(err) {
		t.Fatalf("got %v", err)
	}

	// permission denied: definite
	f.Deny = true
	if _, err = keys.LoadFromEnv(env(cfg), ""); err == nil || keys.IsIndeterminate(err) {
		t.Fatalf("got %v", err)
	}
	f.Deny = false

	// unreachable: indeterminate (the entrypoint pre-flight fails open, the app fails closed)
	f.FailNext(10)
	if _, err = keys.LoadFromEnv(env(cfg), ""); !keys.IsIndeterminate(err) {
		t.Fatalf("got %v", err)
	}
	f.Server.Close()
	if _, err = keys.LoadFromEnv(env(cfg), ""); !keys.IsIndeterminate(err) {
		t.Fatalf("got %v", err)
	}
}

func TestAWSSourceExcludesOtherSources(t *testing.T) {
	f, base := awsFake(t)
	arn := f.AddKey("kek")

	for _, other := range []string{keys.EnvKEK, keys.EnvKEKFile, keys.EnvKEKAzureKeyID, keys.EnvKEKGCPKeyID} {
		_, err := keys.LoadFromEnv(env(merge(base, map[string]string{keys.EnvKEKAWSKeyID: arn, other: "x"})), "")
		if err == nil || !strings.Contains(err.Error(), keys.EnvKEKAWSKeyID) {
			t.Fatalf("%s: got %v", other, err)
		}
	}

	_, err := keys.LoadOldFromEnv(env(merge(base, map[string]string{keys.EnvKEKOld: b64Key(t), keys.EnvKEKAWSKeyIDOld: arn})), "")
	if err == nil || !strings.Contains(err.Error(), keys.EnvKEKAWSKeyIDOld) {
		t.Fatalf("got %v", err)
	}

	// the app-key fallback does not conflict: KMS takes precedence
	src, err := keys.LoadFromEnv(env(merge(base, map[string]string{keys.EnvKEKAWSKeyID: arn, "PB_ENCRYPTION_KEY": strings.Repeat("x", 32)})), "PB_ENCRYPTION_KEY")
	if err != nil || src.Name() != "aws-kms" {
		t.Fatalf("%v", err)
	}
}

func TestAWSRotationWithOld(t *testing.T) {
	f, base := awsFake(t)
	a, b := f.AddKey("a"), f.AddKey("b")
	ctx := context.Background()
	dek := bytes.Repeat([]byte{3}, 32)
	envKEK := b64Key(t)

	// KMS key A -> KMS key B
	srcA, _ := keys.LoadWithOld(env(merge(base, map[string]string{keys.EnvKEKAWSKeyID: a})), "")
	wA, _ := srcA.Wrap(ctx, dek)

	srcB, err := keys.LoadWithOld(env(merge(base, map[string]string{keys.EnvKEKAWSKeyID: b, keys.EnvKEKAWSKeyIDOld: a})), "")
	if err != nil {
		t.Fatal(err)
	}
	m := srcB.(*keys.Multi)
	if !m.HasOld() || !m.Has(srcA.KeyID()) || m.KeyID() == srcA.KeyID() {
		t.Fatal("old key not available")
	}
	got, err := srcB.Unwrap(ctx, srcA.KeyID(), wA)
	if err != nil || !bytes.Equal(got, dek) {
		t.Fatalf("%v", err)
	}
	wB, err := srcB.Wrap(ctx, got)
	if err != nil {
		t.Fatal(err)
	}

	// with only B configured, A's data is reported as a different key, not as corruption
	onlyB, _ := keys.LoadWithOld(env(merge(base, map[string]string{keys.EnvKEKAWSKeyID: b})), "")
	if _, err := onlyB.Unwrap(ctx, srcA.KeyID(), wA); !errors.Is(err, keys.ErrKeyIDMismatch) {
		t.Fatalf("got %v", err)
	}
	if got, err := onlyB.Unwrap(ctx, srcB.KeyID(), wB); err != nil || !bytes.Equal(got, dek) {
		t.Fatalf("%v", err)
	}

	// local -> KMS and back
	local, _ := keys.LoadFromEnv(env(map[string]string{keys.EnvKEK: envKEK}), "")
	wLocal, _ := local.Wrap(ctx, dek)
	toKMS, err := keys.LoadWithOld(env(merge(base, map[string]string{keys.EnvKEKAWSKeyID: a, keys.EnvKEKOld: envKEK})), "")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := toKMS.Unwrap(ctx, local.KeyID(), wLocal); err != nil || !bytes.Equal(got, dek) {
		t.Fatalf("%v", err)
	}
	back, err := keys.LoadWithOld(env(merge(base, map[string]string{keys.EnvKEK: envKEK, keys.EnvKEKAWSKeyIDOld: a})), "")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := back.Unwrap(ctx, srcA.KeyID(), wA); err != nil || !bytes.Equal(got, dek) {
		t.Fatalf("%v", err)
	}
}
