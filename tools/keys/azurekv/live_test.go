//go:build azurekv_live

package azurekv_test

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/tools/keys/azurekv"
)

// Live test against a real Key Vault. Not run in CI:
//
//	CLAVIGER_LIVE_KEY_URL=https://<vault>.vault.azure.net/keys/<name> \
//	  go test -tags azurekv_live -run Live ./tools/keys/azurekv
//
// plus whatever credentials CredentialFromEnv needs (AZURE_* or a managed identity).
func TestLiveWrapUnwrap(t *testing.T) {
	url := os.Getenv("CLAVIGER_LIVE_KEY_URL")
	if url == "" {
		t.Skip("CLAVIGER_LIVE_KEY_URL is not set")
	}

	cred, err := azurekv.CredentialFromEnv(os.Getenv, azurekv.Options{})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	c, err := azurekv.New(ctx, url, cred, azurekv.Options{})
	if err != nil {
		t.Fatal(err)
	}

	dek := bytes.Repeat([]byte{0x42}, 32)
	w, err := c.Wrap(ctx, dek)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Unwrap(ctx, c.KeyID(), w)
	if err != nil || !bytes.Equal(got, dek) {
		t.Fatalf("round trip failed: %v", err)
	}
	t.Logf("ok with key id %s, algorithm %s", c.KeyID(), c.Algorithm())
}
