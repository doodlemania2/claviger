// Package keys implements the key-encryption-key (KEK) sources used by the
// fork-only field encryption (#75).
//
// A KeySource wraps and unwraps data-encryption keys (DEKs). It never sees
// record data. The env and file sources hold the KEK in process memory and wrap
// locally with AES-256-GCM; later sources (a cloud KMS) can implement the same
// interface and call the service instead.
package keys

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

// Environment variable names read by [LoadFromEnv].
const (
	// EnvKEK holds the KEK itself: base64 of exactly 32 random bytes.
	EnvKEK = "CLAVIGER_KEK"

	// EnvKEKFile names a file holding the KEK (same base64 text).
	EnvKEKFile = "CLAVIGER_KEK_FILE"
)

// KEKLength is the size in bytes of a key-encryption key.
const KEKLength = 32

var (
	// ErrNoKeySource is returned when no KEK is configured at all.
	ErrNoKeySource = errors.New("no key-encryption key is configured")

	// ErrKeyIDMismatch is returned when a wrapped key was produced by a
	// different KEK than the one that is configured.
	ErrKeyIDMismatch = errors.New("the data was wrapped with a different key-encryption key")

	// ErrUnwrap is returned when a wrapped key fails authentication, which for
	// a correct key id means the KEK is wrong or the stored value was altered.
	ErrUnwrap = errors.New("failed to unwrap the data key")
)

// KeySource wraps and unwraps data keys with a key-encryption key.
type KeySource interface {
	// Name is the short name of the source ("env", "file", ...). It is used
	// in logs and error messages and is never secret.
	Name() string

	// KeyID is the stable, non-secret id of the KEK currently used to wrap.
	// It is stored next to every wrapped DEK, so that a wrong or rotated KEK
	// is reported as such instead of as a generic decryption failure.
	KeyID() string

	// Wrap encrypts a data key.
	Wrap(ctx context.Context, dek []byte) ([]byte, error)

	// Unwrap decrypts a data key that was wrapped under kekID.
	Unwrap(ctx context.Context, kekID string, wrapped []byte) ([]byte, error)
}

// localSource is the shared implementation of the sources that hold the KEK
// in memory.
type localSource struct {
	name string
	id   string
	aead cipher.AEAD
}

func newLocalSource(name string, kek []byte) (*localSource, error) {
	if len(kek) != KEKLength {
		return nil, fmt.Errorf("the key-encryption key must be %d bytes (got %d)", KEKLength, len(kek))
	}

	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, err
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	// the id is a one-way fingerprint, safe to store and to print
	mac := hmac.New(sha256.New, kek)
	mac.Write([]byte("claviger/kek-id/v1"))

	return &localSource{
		name: name,
		id:   "k" + hex.EncodeToString(mac.Sum(nil)[:8]),
		aead: aead,
	}, nil
}

func (s *localSource) Name() string  { return s.name }
func (s *localSource) KeyID() string { return s.id }

func (s *localSource) wrapAAD(kekID string) []byte {
	return []byte("claviger/dek-wrap/v1|" + kekID)
}

func (s *localSource) Wrap(_ context.Context, dek []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := crand.Read(nonce); err != nil {
		return nil, err
	}

	return s.aead.Seal(nonce, nonce, dek, s.wrapAAD(s.id)), nil
}

func (s *localSource) Unwrap(_ context.Context, kekID string, wrapped []byte) ([]byte, error) {
	if kekID != s.id {
		return nil, fmt.Errorf("%w (stored key id %q, configured key id %q)", ErrKeyIDMismatch, kekID, s.id)
	}

	n := s.aead.NonceSize()
	if len(wrapped) < n+s.aead.Overhead() {
		return nil, ErrUnwrap
	}

	dek, err := s.aead.Open(nil, wrapped[:n], wrapped[n:], s.wrapAAD(kekID))
	if err != nil {
		return nil, ErrUnwrap
	}

	return dek, nil
}

// decodeKEK decodes the base64 text form of a KEK. It accepts the standard
// and URL-safe alphabets, with or without padding.
func decodeKEK(text string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(text); err == nil {
			if len(b) != KEKLength {
				return nil, fmt.Errorf("it decodes to %d bytes, expected %d", len(b), KEKLength)
			}
			return b, nil
		}
	}

	return nil, errors.New("it is not valid base64")
}
