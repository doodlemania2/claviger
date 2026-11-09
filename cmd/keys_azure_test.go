package cmd_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/cmd"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
	"github.com/doodlemania2/claviger/tools/keys"
	"github.com/doodlemania2/claviger/tools/keys/azurekv"
	"github.com/doodlemania2/claviger/tools/keys/azurekvtest"
)

// Fork-local (Claviger #75, M4): `keys check` against Azure Key Vault. A
// definite refusal is FAILED; an outage is "skipped" so entrypoint.sh fails open.
func TestKeysCheckAzureKeyVaultVerdicts(t *testing.T) {
	f := azurekvtest.New()
	defer f.Server.Close()
	f.Secret = "dev-client-secret"
	f.AddKey("kek")

	restore := keys.SetAzureClientOptions(func() azurekv.Options {
		return azurekv.Options{HTTPClient: f.Server.Client(), MaxAttempts: 2, Sleep: func(context.Context, time.Duration) error { return nil }}
	})
	defer restore()

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()
	usePostgresOfApp(t, app)

	envKEK := kekB64(t)
	t.Setenv("CLAVIGER_KEK", envKEK)

	col := core.NewBaseCollection("checked")
	col.Fields.Add(&core.EncryptedTextField{Name: "secret"})
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}

	// move the field key under Key Vault: rewrap with Key Vault primary, env KEK previous
	azEnv := map[string]string{
		"AZURE_CLIENT_ID": "c", "AZURE_TENANT_ID": "t", "AZURE_CLIENT_SECRET": f.Secret, "AZURE_AUTHORITY_HOST": f.AuthorityHost(),
		keys.EnvKEKAzureKeyID: f.KeyURL("kek", ""),
	}
	for k, v := range azEnv {
		t.Setenv(k, v)
	}
	t.Setenv("CLAVIGER_KEK", "")
	t.Setenv("CLAVIGER_KEK_OLD", envKEK)
	app.Store().Remove("__clavigerFieldKeySource__")
	if _, err := core.RewrapFieldKeys(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	if err := core.DialectOf(app).Checkpoint(app.DB()); err != nil {
		t.Fatal(err)
	}

	with := func(over map[string]string) func(string) string {
		m := map[string]string{}
		for k, v := range azEnv {
			m[k] = v
		}
		for k, v := range over {
			m[k] = v
		}
		return dbEnv(app, m)
	}
	check := func(getenv func(string) string) (string, error) {
		return cmd.CheckFieldKeys(context.Background(), app.DataDir(), "", getenv)
	}

	t.Run("healthy", func(t *testing.T) {
		msg, err := check(with(nil))
		if err != nil || !strings.Contains(msg, "OK (1 encrypted field key(s)") {
			t.Fatalf("%q %v", msg, err)
		}
	})

	t.Run("bad credentials are FAILED", func(t *testing.T) {
		_, err := check(with(map[string]string{"AZURE_CLIENT_SECRET": "wrong"}))
		if err == nil || !strings.HasPrefix(err.Error(), cmd.KeysCheckFailedPrefix) || strings.Contains(err.Error(), "wrong") {
			t.Fatalf("%v", err)
		}
	})

	t.Run("permission denied is FAILED", func(t *testing.T) {
		f.Deny = true
		defer func() { f.Deny = false }()
		_, err := check(with(nil))
		if err == nil || !strings.HasPrefix(err.Error(), cmd.KeysCheckFailedPrefix) {
			t.Fatalf("%v", err)
		}
	})

	t.Run("key not found is FAILED", func(t *testing.T) {
		_, err := check(with(map[string]string{keys.EnvKEKAzureKeyID: f.KeyURL("missing", "")}))
		if err == nil || !strings.HasPrefix(err.Error(), cmd.KeysCheckFailedPrefix) {
			t.Fatalf("%v", err)
		}
	})

	t.Run("wrong key (data wrapped by another key) is FAILED", func(t *testing.T) {
		f.AddKey("other")
		_, err := check(with(map[string]string{keys.EnvKEKAzureKeyID: f.KeyURL("other", "")}))
		if err == nil || !strings.HasPrefix(err.Error(), cmd.KeysCheckFailedPrefix) {
			t.Fatalf("%v", err)
		}
	})

	t.Run("outage while resolving the key is skipped", func(t *testing.T) {
		f.FailNext("/keys/", 503, 1000, nil)
		defer f.ClearFailures()
		msg, err := check(with(nil))
		if err != nil || !strings.HasPrefix(msg, "skipped") || strings.Contains(msg, cmd.KeysCheckFailedPrefix) {
			t.Fatalf("%q %v", msg, err)
		}
	})

	t.Run("outage while unwrapping is skipped", func(t *testing.T) {
		f.FailNext("/unwrapkey", 500, 1000, nil)
		defer f.ClearFailures()
		msg, err := check(with(nil))
		if err != nil || !strings.HasPrefix(msg, "skipped") {
			t.Fatalf("%q %v", msg, err)
		}
	})

	t.Run("token endpoint outage is skipped", func(t *testing.T) {
		f.FailNext("/oauth2/v2.0/token", 503, 1000, nil)
		defer f.ClearFailures()
		msg, err := check(with(nil))
		if err != nil || !strings.HasPrefix(msg, "skipped") {
			t.Fatalf("%q %v", msg, err)
		}
	})
}
