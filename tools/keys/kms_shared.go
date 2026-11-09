//go:build kms_aws || kms_gcp

package keys

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Shared glue of the AWS and GCP KEK sources; only built with one of their tags.

const kmsLoadTimeout = 30 * time.Second

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
