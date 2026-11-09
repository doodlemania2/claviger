package oidc

import (
	"testing"
	"time"

	"github.com/doodlemania2/claviger/tests"
)

// ready() holds m.mu while it calls cryptoKey(). Without an encryption env
// (dev mode) cryptoKey() must not take m.mu again, or the first OIDC request
// deadlocks.
func TestCryptoKeyDoesNotTakeTheModuleLock(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	m := &module{app: app}
	if encryptionKey(app) != "" {
		t.Skip("the test app has an encryption env; the dev-key path is not taken")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	done := make(chan [32]byte, 1)
	go func() { done <- m.cryptoKey() }()

	select {
	case k := <-done:
		if k == ([32]byte{}) {
			t.Fatal("expected a random dev key")
		}
		if again := m.cryptoKey(); again != k {
			t.Fatal("the dev key must stay the same within a process")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cryptoKey blocked on the module lock")
	}
}
