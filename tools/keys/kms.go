package keys

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Fork-local (Claviger #75): AWS KMS and Google Cloud KMS as KEK sources.
//
// The clients (tools/keys/awskms, tools/keys/gcpkms) are plain standard-library
// code and always compile. Only the glue that links them into the KEK loader
// sits behind the build tags kms_aws and kms_gcp, so a default build does not
// carry them. Without the tag, configuring the variable fails closed.

// Environment variable names of the cloud KMS sources. Credentials use the
// providers' standard variables (AWS_*, GOOGLE_APPLICATION_CREDENTIALS).
const (
	// EnvKEKAWSKeyID selects AWS KMS as the KEK source: a key ARN (preferred),
	// key id or alias. It excludes every other key source. Needs the kms_aws build tag.
	EnvKEKAWSKeyID = "CLAVIGER_KEK_AWS_KEY_ID"

	// EnvKEKAWSKeyIDOld names the previous AWS KMS key during a rotation.
	EnvKEKAWSKeyIDOld = "CLAVIGER_KEK_AWS_KEY_ID_OLD"

	// EnvKEKGCPKeyID selects Google Cloud KMS as the KEK source:
	// projects/<p>/locations/<l>/keyRings/<r>/cryptoKeys/<k>. It excludes every
	// other key source. Needs the kms_gcp build tag.
	EnvKEKGCPKeyID = "CLAVIGER_KEK_GCP_KEY_ID"

	// EnvKEKGCPKeyIDOld names the previous Cloud KMS key during a rotation.
	EnvKEKGCPKeyIDOld = "CLAVIGER_KEK_GCP_KEY_ID_OLD"

	kmsLoadTimeout = 30 * time.Second
)

// kmsClient is what the AWS and GCP clients have in common.
type kmsClient interface {
	KeyID() string
	Wrap(ctx context.Context, dek []byte) ([]byte, error)
	Unwrap(ctx context.Context, keyID string, wrapped []byte) ([]byte, error)
}

// kmsSource adapts a cloud KMS client to [KeySource], mapping its errors the
// way the Azure source does.
type kmsSource struct {
	name     string
	c        kmsClient
	mismatch error // the client's key-mismatch sentinel
	indet    func(error) bool
}

func (s *kmsSource) Name() string  { return s.name }
func (s *kmsSource) KeyID() string { return s.c.KeyID() }

func (s *kmsSource) Wrap(ctx context.Context, dek []byte) ([]byte, error) {
	return s.c.Wrap(ctx, dek)
}

func (s *kmsSource) Unwrap(ctx context.Context, kekID string, wrapped []byte) ([]byte, error) {
	dek, err := s.c.Unwrap(ctx, kekID, wrapped)
	switch {
	case err == nil:
		return dek, nil
	case errors.Is(err, s.mismatch):
		return nil, fmt.Errorf("%w (stored key id %q, configured key id %q)", ErrKeyIDMismatch, kekID, s.KeyID())
	case s.indet(err):
		return nil, err
	}

	// the service answered and refused: wrong key, no permission, altered value
	return nil, fmt.Errorf("%w: %w", ErrUnwrap, err)
}

// cloudRefs returns the names (and values) of the cloud KMS variables that are
// set among names, in order.
func cloudRefs(getenv func(string) string, names ...string) map[string]string {
	out := map[string]string{}
	for _, n := range names {
		if v := strings.TrimSpace(getenv(n)); v != "" {
			out[n] = v
		}
	}
	return out
}

func notBuilt(envName, tag string) error {
	return fmt.Errorf("%s is set but this build has no support for it: rebuild with -tags %s (the release image does not include it unless built with that tag)", envName, tag)
}
