//go:build kms_gcp

package keys

import (
	"context"
	"fmt"

	"github.com/doodlemania2/claviger/tools/keys/gcpkms"
	"github.com/doodlemania2/claviger/tools/keys/kmshttp"
)

// Fork-local (Claviger #75): Google Cloud KMS as a KEK source (build tag kms_gcp).

// GCPKMSBuilt reports whether this binary was built with Google Cloud KMS support.
const GCPKMSBuilt = true

var gcpOptions = func() kmshttp.Options { return kmshttp.Options{} }

// SetGCPClientOptions replaces the HTTP client, clock and endpoints used to
// reach Google Cloud. It exists for tests and returns a function that
// restores the previous setting.
func SetGCPClientOptions(f func() kmshttp.Options) (restore func()) {
	prev := gcpOptions
	gcpOptions = f
	return func() { gcpOptions = prev }
}

func newGCPSource(name, keyName string, getenv func(string) string) (KeySource, error) {
	opts := gcpOptions()

	ts, err := gcpkms.TokenSourceFromEnv(getenv, opts)
	if err != nil {
		return nil, fmt.Errorf("gcp kms key source: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), kmsLoadTimeout)
	defer cancel()

	c, err := gcpkms.New(ctx, keyName, ts, opts)
	if err != nil {
		return nil, fmt.Errorf("gcp kms key source: %w", err)
	}

	return &kmsSource{name: name, c: c, mismatch: gcpkms.ErrKeyMismatch, indet: kmshttp.IsIndeterminate}, nil
}
