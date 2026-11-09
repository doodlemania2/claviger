package awskms_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/tools/keys/awskms"
	"github.com/doodlemania2/claviger/tools/keys/awskmstest"
	"github.com/doodlemania2/claviger/tools/keys/kmshttp"
)

func opts(f *awskmstest.Fake) kmshttp.Options {
	return kmshttp.Options{
		MaxAttempts: 3,
		Sleep:       func(context.Context, time.Duration) error { return nil },
		Endpoint:    f.Server.URL,
	}
}

func envCreds(extra map[string]string) func(string) string {
	m := map[string]string{"AWS_ACCESS_KEY_ID": awskmstest.AccessKey, "AWS_SECRET_ACCESS_KEY": awskmstest.SecretKey}
	for k, v := range extra {
		m[k] = v
	}
	return func(k string) string { return m[k] }
}

func newClient(t *testing.T, f *awskmstest.Fake, ref string) (*awskms.Client, error) {
	t.Helper()
	o := opts(f)
	creds, err := awskms.CredentialsFromEnv(envCreds(nil), awskmstest.Region, o)
	if err != nil {
		t.Fatal(err)
	}
	return awskms.New(context.Background(), ref, awskmstest.Region, creds, f.Server.URL, o)
}

func TestRoundTripSignedAndAlias(t *testing.T) {
	f := awskmstest.New()
	defer f.Server.Close()
	arn := f.AddKey("kek1")
	f.AddAlias("claviger", arn)

	for _, ref := range []string{arn, "kek1", "alias/claviger"} {
		c, err := newClient(t, f, ref)
		if err != nil {
			t.Fatalf("%s: %v", ref, err)
		}
		if c.KeyID() != "awskms:"+arn {
			t.Fatalf("key id %q", c.KeyID())
		}

		dek := bytes.Repeat([]byte{7}, 32)
		w, err := c.Wrap(context.Background(), dek)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(w, dek) {
			t.Fatal("wrapped value contains the key")
		}
		got, err := c.Unwrap(context.Background(), c.KeyID(), w)
		if err != nil || !bytes.Equal(got, dek) {
			t.Fatalf("unwrap: %v", err)
		}
	}
}

func TestWrongKeyAndAlteredValue(t *testing.T) {
	f := awskmstest.New()
	defer f.Server.Close()
	a := f.AddKey("a")
	f.AddKey("b")

	ca, _ := newClient(t, f, a)
	cb, err := newClient(t, f, "b")
	if err != nil {
		t.Fatal(err)
	}

	w, _ := ca.Wrap(context.Background(), []byte("0123456789abcdef0123456789abcdef"))

	// another key's id: refused locally
	if _, err := cb.Unwrap(context.Background(), ca.KeyID(), w); !errors.Is(err, awskms.ErrKeyMismatch) {
		t.Fatalf("got %v", err)
	}
	// the right id with a ciphertext of another key: definite service refusal
	_, err = cb.Unwrap(context.Background(), cb.KeyID(), w)
	if err == nil || kmshttp.IsIndeterminate(err) {
		t.Fatalf("expected a definite error, got %v", err)
	}
	// garbage
	_, err = ca.Unwrap(context.Background(), ca.KeyID(), []byte("x"))
	if err == nil || kmshttp.IsIndeterminate(err) {
		t.Fatalf("expected a definite error, got %v", err)
	}
}

func TestMissingKeyDeniedAndUnusableKeys(t *testing.T) {
	f := awskmstest.New()
	defer f.Server.Close()
	arn := f.AddKey("k")

	if _, err := newClient(t, f, "nope"); err == nil || kmshttp.IsIndeterminate(err) || !strings.Contains(err.Error(), "NotFoundException") {
		t.Fatalf("got %v", err)
	}

	f.KeyState = "Disabled"
	if _, err := newClient(t, f, arn); err == nil || !strings.Contains(err.Error(), "not Enabled") {
		t.Fatalf("got %v", err)
	}
	f.KeyState, f.KeyUsage = "", "SIGN_VERIFY"
	if _, err := newClient(t, f, arn); err == nil || !strings.Contains(err.Error(), "ENCRYPT_DECRYPT") {
		t.Fatalf("got %v", err)
	}
	f.KeyUsage = ""

	f.Deny = true
	if _, err := newClient(t, f, arn); err == nil || kmshttp.IsIndeterminate(err) || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("got %v", err)
	}
}

func TestBadSecretIsDefinite(t *testing.T) {
	f := awskmstest.New()
	defer f.Server.Close()
	arn := f.AddKey("k")

	o := opts(f)
	creds, _ := awskms.CredentialsFromEnv(envCreds(map[string]string{"AWS_SECRET_ACCESS_KEY": "wrong"}), awskmstest.Region, o)
	_, err := awskms.New(context.Background(), arn, awskmstest.Region, creds, f.Server.URL, o)
	if err == nil || kmshttp.IsIndeterminate(err) || !strings.Contains(err.Error(), "InvalidSignature") {
		t.Fatalf("got %v", err)
	}
}

func TestUnreachableIsIndeterminateAndRetries(t *testing.T) {
	f := awskmstest.New()
	arn := f.AddKey("k")
	c, err := newClient(t, f, arn)
	if err != nil {
		t.Fatal(err)
	}

	// 503s are retried, then succeed
	f.FailNext(2)
	if _, err := c.Wrap(context.Background(), []byte("0123456789abcdef0123456789abcdef")); err != nil {
		t.Fatalf("retry failed: %v", err)
	}

	// persistent 503: indeterminate
	f.FailNext(100)
	_, err = c.Wrap(context.Background(), []byte("x"))
	if err == nil || !kmshttp.IsIndeterminate(err) {
		t.Fatalf("got %v", err)
	}

	// connection refused: indeterminate, and New fails closed
	f.Server.Close()
	if _, err := c.Wrap(context.Background(), []byte("x")); err == nil || !kmshttp.IsIndeterminate(err) {
		t.Fatalf("got %v", err)
	}
	if _, err := newClient(t, f, arn); err == nil || !kmshttp.IsIndeterminate(err) {
		t.Fatalf("got %v", err)
	}
}

func TestRegionAndARNValidation(t *testing.T) {
	f := awskmstest.New()
	defer f.Server.Close()
	o := opts(f)
	creds, _ := awskms.CredentialsFromEnv(envCreds(nil), "", o)

	if _, err := awskms.New(context.Background(), "alias/x", "", creds, f.Server.URL, o); err == nil || !strings.Contains(err.Error(), "region") {
		t.Fatalf("got %v", err)
	}
	if _, err := awskms.New(context.Background(), "arn:aws:s3:::bucket", "us-east-1", creds, f.Server.URL, o); err == nil {
		t.Fatal("expected an error for a non-KMS ARN")
	}
}

func TestSessionTokenIsSigned(t *testing.T) {
	f := awskmstest.New()
	defer f.Server.Close()
	f.SessionToken = "tok"
	arn := f.AddKey("k")

	o := opts(f)
	creds, _ := awskms.CredentialsFromEnv(envCreds(map[string]string{"AWS_SESSION_TOKEN": "tok"}), awskmstest.Region, o)
	if _, err := awskms.New(context.Background(), arn, awskmstest.Region, creds, f.Server.URL, o); err != nil {
		t.Fatal(err)
	}
}

func TestSharedCredentialsFile(t *testing.T) {
	f := awskmstest.New()
	defer f.Server.Close()
	arn := f.AddKey("k")

	p := filepath.Join(t.TempDir(), "credentials")
	content := "[other]\naws_access_key_id = nope\naws_secret_access_key = nope\n\n[prod]\n# comment\naws_access_key_id = " +
		awskmstest.AccessKey + "\naws_secret_access_key=" + awskmstest.SecretKey + "\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	env := map[string]string{"AWS_SHARED_CREDENTIALS_FILE": p, "AWS_PROFILE": "prod"}
	o := opts(f)
	creds, err := awskms.CredentialsFromEnv(func(k string) string { return env[k] }, awskmstest.Region, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := awskms.New(context.Background(), arn, awskmstest.Region, creds, f.Server.URL, o); err != nil {
		t.Fatal(err)
	}

	env["AWS_PROFILE"] = "missing"
	if _, err := awskms.CredentialsFromEnv(func(k string) string { return env[k] }, awskmstest.Region, o); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("got %v", err)
	}
}

func TestContainerAndIMDSAndWebIdentity(t *testing.T) {
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	credJSON := `{"AccessKeyId":"` + awskmstest.AccessKey + `","SecretAccessKey":"` + awskmstest.SecretKey + `","Token":"t","Expiration":"` + exp + `"}`

	meta := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/creds" && r.Header.Get("Authorization") == "secret-token":
			_, _ = w.Write([]byte(credJSON))
		case r.URL.Path == "/latest/api/token" && r.Method == http.MethodPut:
			_, _ = w.Write([]byte("imds-token"))
		case r.Method == http.MethodPost && r.URL.Path == "/":
			_ = r.ParseForm()
			if r.PostForm.Get("Action") != "AssumeRoleWithWebIdentity" || r.PostForm.Get("WebIdentityToken") != "jwt" {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`<ErrorResponse><Error><Code>InvalidIdentityToken</Code><Message>bad</Message></Error></ErrorResponse>`))
				return
			}
			_, _ = w.Write([]byte(`<AssumeRoleWithWebIdentityResponse><AssumeRoleWithWebIdentityResult><Credentials><AccessKeyId>` +
				awskmstest.AccessKey + `</AccessKeyId><SecretAccessKey>` + awskmstest.SecretKey + `</SecretAccessKey><SessionToken>t</SessionToken><Expiration>` +
				exp + `</Expiration></Credentials></AssumeRoleWithWebIdentityResult></AssumeRoleWithWebIdentityResponse>`))
		case r.Header.Get("X-aws-ec2-metadata-token") != "imds-token":
			w.WriteHeader(401)
		case r.URL.Path == "/latest/meta-data/iam/security-credentials/":
			_, _ = w.Write([]byte("role-name\n"))
		case r.URL.Path == "/latest/meta-data/iam/security-credentials/role-name":
			_, _ = w.Write([]byte(credJSON))
		default:
			w.WriteHeader(404)
		}
	}))
	defer meta.Close()

	o := kmshttp.Options{MetadataEndpoint: meta.URL, MaxAttempts: 1}
	check := func(name string, env map[string]string) {
		t.Helper()
		p, err := awskms.CredentialsFromEnv(func(k string) string { return env[k] }, "us-east-1", o)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		c, err := p.Credentials(context.Background())
		if err != nil || c.AccessKeyID != awskmstest.AccessKey || c.SessionToken != "t" {
			t.Fatalf("%s: %+v %v", name, c, err)
		}
	}

	check("ecs", map[string]string{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/creds", "AWS_CONTAINER_AUTHORIZATION_TOKEN": "secret-token"})
	t.Setenv("HOME", t.TempDir()) // no ~/.aws/credentials
	check("imds", map[string]string{})

	tokFile := filepath.Join(t.TempDir(), "tok")
	_ = os.WriteFile(tokFile, []byte("jwt\n"), 0o600)
	check("web identity", map[string]string{
		"AWS_WEB_IDENTITY_TOKEN_FILE": tokFile, "AWS_ROLE_ARN": "arn:aws:iam::111122223333:role/r", "AWS_ENDPOINT_URL_STS": meta.URL,
	})
}
