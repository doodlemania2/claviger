package core_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
	"github.com/doodlemania2/claviger/tools/keys"
	"github.com/doodlemania2/claviger/tools/keys/azurekv"
	"github.com/doodlemania2/claviger/tools/keys/azurekvtest"
)

// Fork-local (Claviger #75, M4): the Azure Key Vault KEK source end to end,
// against a fake Key Vault. Not parallel: the keys come from the environment.

func useFakeKeyVault(t *testing.T) *azurekvtest.Fake {
	t.Helper()

	f := azurekvtest.New()
	t.Cleanup(f.Server.Close)
	f.Secret = "dev-client-secret"
	f.AddKey("claviger-kek")

	restore := keys.SetAzureClientOptions(func() azurekv.Options {
		return azurekv.Options{
			HTTPClient:  f.Server.Client(),
			MaxAttempts: 2,
			Sleep:       func(context.Context, time.Duration) error { return nil },
		}
	})
	t.Cleanup(restore)

	t.Setenv("AZURE_CLIENT_ID", "client")
	t.Setenv("AZURE_TENANT_ID", "tenant")
	t.Setenv("AZURE_CLIENT_SECRET", f.Secret)
	t.Setenv("AZURE_AUTHORITY_HOST", f.AuthorityHost())

	return f
}

func TestAzureKeyVaultRewrapRestartAndFailClosed(t *testing.T) {
	f := useFakeKeyVault(t)
	envKEK := setTestKEK(t)

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()
	col := newEncryptedCollection(t, app)
	want := seedPeople(t, app, col, 10)

	// env KEK -> Key Vault: start with Key Vault as primary and the env KEK as previous
	t.Setenv("CLAVIGER_KEK", "")
	t.Setenv("CLAVIGER_KEK_OLD", envKEK)
	t.Setenv(keys.EnvKEKAzureKeyID, f.KeyURL("claviger-kek", ""))
	core.ResetFieldKeyring()
	app2, err := restartApp(t, app)
	if err != nil {
		t.Fatal(err)
	}
	defer app2.Cleanup()
	assertAllReadable(t, app2, col, want)

	res, err := core.RewrapFieldKeys(context.Background(), app2)
	if err != nil || res.Rewrapped != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if rows := fieldKeyRowsOf(t, app2, col, "phone"); !strings.HasPrefix(rows[0].KekId, "azkv:") {
		t.Fatalf("kekId = %q", rows[0].KekId)
	}

	// steady state: reads and writes do not call Key Vault
	f.Reset()
	assertAllReadable(t, app2, col, want)
	saveEncRecord(t, app2, col, "Z", "999-9999")
	if n := f.Count("/keys/"); n != 0 {
		t.Fatalf("%d Key Vault calls in steady state", n)
	}

	// Key Vault alone is enough now
	t.Setenv("CLAVIGER_KEK_OLD", "")
	core.ResetFieldKeyring()
	app3, err := restartApp(t, app2)
	if err != nil {
		t.Fatal(err)
	}
	defer app3.Cleanup()
	assertAllReadable(t, app3, col, want)

	// Key Vault unreachable at bootstrap with encrypted fields: refuse to start
	f.FailNext("/keys/", 503, 1000, nil)
	core.ResetFieldKeyring()
	_, err = restartApp(t, app3)
	if err == nil || !strings.Contains(err.Error(), "key-encryption key is unavailable") || !keys.IsIndeterminate(err) {
		t.Fatalf("expected a fail-closed start with an indeterminate cause, got %v", err)
	}

	// wrong credentials: also refused
	t.Setenv("AZURE_CLIENT_SECRET", "wrong-secret")
	core.ResetFieldKeyring()
	_, err = restartApp(t, app3)
	if err == nil || keys.IsIndeterminate(err) || strings.Contains(err.Error(), "wrong-secret") {
		t.Fatalf("got %v", err)
	}

	// back to Key Vault, rewrap back to the env KEK
	t.Setenv("AZURE_CLIENT_SECRET", f.Secret)
	f.RevokeTokens()
	f.ClearFailures()
	t.Setenv(keys.EnvKEKAzureKeyID, "")
	t.Setenv(keys.EnvKEKAzureKeyIDOld, f.KeyURL("claviger-kek", ""))
	t.Setenv("CLAVIGER_KEK", envKEK)
	core.ResetFieldKeyring()
	app4, err := restartApp(t, app3)
	if err != nil {
		t.Fatal(err)
	}
	defer app4.Cleanup()
	assertAllReadable(t, app4, col, want)
	if res, err := core.RewrapFieldKeys(context.Background(), app4); err != nil || res.Rewrapped != 1 {
		t.Fatalf("%+v %v", res, err)
	}
}
