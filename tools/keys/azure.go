package keys

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/doodlemania2/claviger/tools/keys/azurekv"
)

// Fork-local (Claviger #75, M4): Azure Key Vault as a KEK source.

// Environment variable names of the Azure Key Vault source. The credentials
// are configured with the standard AZURE_* variables (see [azurekv.CredentialFromEnv]).
const (
	// EnvKEKAzureKeyID selects Azure Key Vault as the KEK source:
	// https://<vault>.vault.azure.net/keys/<name>[/<version>]. It excludes
	// CLAVIGER_KEK and CLAVIGER_KEK_FILE.
	EnvKEKAzureKeyID = "CLAVIGER_KEK_AZURE_KEY_ID"

	// EnvKEKAzureKeyIDOld names the previous Key Vault key during a rotation.
	EnvKEKAzureKeyIDOld = "CLAVIGER_KEK_AZURE_KEY_ID_OLD"

	azureLoadTimeout = 30 * time.Second
)

// azureOptions returns the client options; tests replace it.
var azureOptions = func() azurekv.Options { return azurekv.Options{} }

// SetAzureClientOptions replaces the HTTP client, clock and endpoints used to
// reach Azure. It exists for tests (to point the source at a fake service) and
// returns a function that restores the previous setting.
func SetAzureClientOptions(f func() azurekv.Options) (restore func()) {
	prev := azureOptions
	azureOptions = f
	return func() { azureOptions = prev }
}

// IsIndeterminate reports whether err says nothing definite about the key
// source: a timeout, a network error, throttling or a 5xx from a key service.
// `keys check` treats it as "could not run" and the entrypoint pre-flight
// fails open. A wrong credential, a missing permission or a wrong key are
// definite and are not indeterminate.
func IsIndeterminate(err error) bool { return azurekv.IsIndeterminate(err) }

type azureSource struct {
	name string
	c    *azurekv.Client
}

func newAzureSource(name, keyURL string, getenv func(string) string) (KeySource, error) {
	opts := azureOptions()

	cred, err := azurekv.CredentialFromEnv(getenv, opts)
	if err != nil {
		return nil, fmt.Errorf("azure key vault key source: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), azureLoadTimeout)
	defer cancel()

	c, err := azurekv.New(ctx, keyURL, cred, opts)
	if err != nil {
		return nil, fmt.Errorf("azure key vault key source: %w", err)
	}

	return &azureSource{name: name, c: c}, nil
}

func (s *azureSource) Name() string  { return s.name }
func (s *azureSource) KeyID() string { return s.c.KeyID() }

func (s *azureSource) Wrap(ctx context.Context, dek []byte) ([]byte, error) {
	return s.c.Wrap(ctx, dek)
}

func (s *azureSource) Unwrap(ctx context.Context, kekID string, wrapped []byte) ([]byte, error) {
	dek, err := s.c.Unwrap(ctx, kekID, wrapped)
	switch {
	case err == nil:
		return dek, nil
	case errors.Is(err, azurekv.ErrKeyMismatch):
		return nil, fmt.Errorf("%w (stored key id %q, configured key id %q)", ErrKeyIDMismatch, kekID, s.KeyID())
	case azurekv.IsIndeterminate(err):
		return nil, err
	}

	// the service answered and refused: wrong key, no permission, altered value
	return nil, fmt.Errorf("%w: %w", ErrUnwrap, err)
}

func azureKeyIDFromEnv(getenv func(string) string, name string) string {
	return strings.TrimSpace(getenv(name))
}
