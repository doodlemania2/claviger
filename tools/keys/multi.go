package keys

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Environment variable names that configure the previous KEK during a
// KEK rotation (see [LoadWithOld]).
const (
	// EnvKEKOld holds the previous KEK: base64 of exactly 32 bytes.
	EnvKEKOld = "CLAVIGER_KEK_OLD"

	// EnvKEKFileOld names a file holding the previous KEK.
	EnvKEKFileOld = "CLAVIGER_KEK_FILE_OLD"

	// EnvKEKOldFromAppKey ("1", "true" or "yes") declares that the previous
	// KEK is the one derived from the app encryption key (PB_ENCRYPTION_KEY),
	// i.e. the rotation moves from the fallback to a dedicated CLAVIGER_KEK.
	EnvKEKOldFromAppKey = "CLAVIGER_KEK_OLD_FROM_APP_KEY"
)

// ErrNoOldKey is returned by [LoadOldFromEnv] when no previous KEK is configured.
var ErrNoOldKey = errors.New("no previous key-encryption key is configured")

// Multi is a [KeySource] that wraps with a primary KEK and can unwrap with the
// primary or with any previous KEK. It is what makes a KEK rotation possible
// without downtime: the app starts with the new KEK as primary and the old one
// as a fallback, can read every data key, and `keys rewrap` then moves the
// wrapped data keys onto the primary.
type Multi struct {
	primary KeySource
	old     []KeySource
}

// NewMulti returns a source that wraps with primary and unwraps with primary
// or any of the old sources (sources whose id equals the primary's are ignored).
func NewMulti(primary KeySource, old ...KeySource) *Multi {
	m := &Multi{primary: primary}
	for _, o := range old {
		if o != nil && o.KeyID() != primary.KeyID() {
			m.old = append(m.old, o)
		}
	}
	return m
}

// Primary returns the KEK used to wrap.
func (m *Multi) Primary() KeySource { return m.primary }

// HasOld reports whether previous KEKs are configured.
func (m *Multi) HasOld() bool { return len(m.old) > 0 }

// Name implements [KeySource].
func (m *Multi) Name() string {
	if len(m.old) == 0 {
		return m.primary.Name()
	}
	names := make([]string, 0, len(m.old))
	for _, o := range m.old {
		names = append(names, o.Name())
	}
	return m.primary.Name() + " (previous: " + strings.Join(names, ", ") + ")"
}

// KeyID implements [KeySource]; it is the id of the primary KEK.
func (m *Multi) KeyID() string { return m.primary.KeyID() }

// Wrap implements [KeySource]; it always wraps under the primary KEK.
func (m *Multi) Wrap(ctx context.Context, dek []byte) ([]byte, error) {
	return m.primary.Wrap(ctx, dek)
}

// Unwrap implements [KeySource].
func (m *Multi) Unwrap(ctx context.Context, kekID string, wrapped []byte) ([]byte, error) {
	if kekID != m.primary.KeyID() {
		for _, o := range m.old {
			if o.KeyID() == kekID {
				return o.Unwrap(ctx, kekID, wrapped)
			}
		}
	}

	return m.primary.Unwrap(ctx, kekID, wrapped)
}

// Has reports whether the KEK with the given id is available.
func (m *Multi) Has(kekID string) bool {
	if kekID == m.primary.KeyID() {
		return true
	}
	for _, o := range m.old {
		if o.KeyID() == kekID {
			return true
		}
	}
	return false
}

// LoadOldFromEnv builds the previous KEK source from CLAVIGER_KEK_OLD,
// CLAVIGER_KEK_FILE_OLD or CLAVIGER_KEK_OLD_FROM_APP_KEY. It returns
// [ErrNoOldKey] when none is set, and an error naming the variable (never its
// value) when one is set but unusable or several are set.
func LoadOldFromEnv(getenv func(string) string, appKeyEnv string) (KeySource, error) {
	direct := strings.TrimSpace(getenv(EnvKEKOld))
	file := strings.TrimSpace(getenv(EnvKEKFileOld))

	azure := azureKeyIDFromEnv(getenv, EnvKEKAzureKeyIDOld)
	aws := strings.TrimSpace(getenv(EnvKEKAWSKeyIDOld))
	gcp := strings.TrimSpace(getenv(EnvKEKGCPKeyIDOld))

	fromApp := false
	switch strings.ToLower(strings.TrimSpace(getenv(EnvKEKOldFromAppKey))) {
	case "1", "true", "yes":
		fromApp = true
	}

	set := 0
	for _, b := range []bool{direct != "", file != "", fromApp, azure != "", aws != "", gcp != ""} {
		if b {
			set++
		}
	}

	switch {
	case set == 0:
		return nil, ErrNoOldKey
	case set > 1:
		return nil, fmt.Errorf("set only one of %s, %s, %s, %s, %s and %s", EnvKEKOld, EnvKEKFileOld, EnvKEKOldFromAppKey, EnvKEKAzureKeyIDOld, EnvKEKAWSKeyIDOld, EnvKEKGCPKeyIDOld)
	case azure != "":
		return newAzureSource("azure-key-vault (previous)", azure, getenv)
	case aws != "":
		return newAWSSource("aws-kms (previous)", aws, getenv)
	case gcp != "":
		return newGCPSource("gcp-kms (previous)", gcp, getenv)
	case direct != "":
		kek, err := decodeKEK(direct)
		if err != nil {
			return nil, fmt.Errorf("%s is invalid: %v (expected base64 of %d random bytes)", EnvKEKOld, err, KEKLength)
		}
		return newLocalSource("env (previous)", kek)
	case file != "":
		return loadFromFile(EnvKEKFileOld, file)
	}

	src, err := deriveFromAppKey(getenv, appKeyEnv)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", EnvKEKOldFromAppKey, err)
	}
	if src == nil {
		return nil, fmt.Errorf("%s is set but the app encryption key (%s) is not", EnvKEKOldFromAppKey, appKeyEnv)
	}

	return src, nil
}

// LoadWithOld is [LoadFromEnv] plus the optional previous KEK of a rotation in
// progress. The result is a [*Multi]. A previous KEK that is configured but
// invalid is an error (fail closed), so a typo cannot silently drop it.
func LoadWithOld(getenv func(string) string, appKeyEnv string) (KeySource, error) {
	primary, err := loadPrimary(getenv, appKeyEnv)
	if err != nil {
		return nil, err
	}

	old, err := LoadOldFromEnv(getenv, appKeyEnv)
	if errors.Is(err, ErrNoOldKey) {
		return NewMulti(primary), nil
	}
	if err != nil {
		return nil, err
	}

	return NewMulti(primary, old), nil
}
