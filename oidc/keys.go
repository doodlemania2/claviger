package oidc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/security"
	"github.com/doodlemania2/claviger/tools/types"
	"github.com/go-jose/go-jose/v4"
	"github.com/zitadel/oidc/v3/pkg/op"
)

const (
	// AlgRS256 is the default signing algorithm.
	AlgRS256 = "RS256"

	// AlgES256 is the selectable alternative.
	AlgES256 = "ES256"

	keyEncryptedPrefix = "enc:"

	// DefaultKeyGrace is how long a rotated-out key stays in the JWKS.
	DefaultKeyGrace = 7 * 24 * time.Hour
)

// ErrPlaintextKeys is returned when a signing key would have to be stored
// unencrypted outside dev mode.
var ErrPlaintextKeys = errors.New("refusing to store signing keys unencrypted: set the settings encryption env (PB_ENCRYPTION_KEY)")

func validAlg(alg string) bool { return alg == AlgRS256 || alg == AlgES256 }

// encryptionKey returns the 32 character settings encryption key, or "".
func encryptionKey(app core.App) string {
	name := app.EncryptionEnv()
	if name == "" {
		return ""
	}

	key := envGet(name)
	if len(key) != 32 {
		return ""
	}

	return key
}

// enabled reports whether the provider may run. It runs when the encryption
// key is configured, or in dev mode (plaintext keys, with a warning).
func enabled(app core.App) (bool, string) {
	if encryptionKey(app) != "" {
		return true, ""
	}

	if app.IsDev() {
		return true, ""
	}

	return false, "the settings encryption env is not set (PB_ENCRYPTION_KEY, 32 characters), so signing keys cannot be stored encrypted"
}

// keyRow is a _signingKeys row.
type keyRow struct {
	Id         string         `db:"id"`
	Kid        string         `db:"kid"`
	Alg        string         `db:"alg"`
	PrivateKey string         `db:"privateKey"`
	PublicJwk  string         `db:"publicJwk"`
	Encrypted  bool           `db:"encrypted"`
	RetiresAt  types.DateTime `db:"retiresAt"`
	Created    types.DateTime `db:"created"`
}

func (k keyRow) active() bool { return k.RetiresAt.IsZero() }

func (k keyRow) usable(now time.Time) bool {
	return k.RetiresAt.IsZero() || k.RetiresAt.Time().After(now)
}

func (s *Store) listKeys() ([]keyRow, error) {
	var rows []keyRow

	err := s.app.ConcurrentDB().
		Select("id", "kid", "alg", "privateKey", "publicJwk", "encrypted", "retiresAt", "created").
		From(core.CollectionNameSigningKeys).
		OrderBy("created DESC").
		All(&rows)

	return rows, err
}

func generateKey(alg string) (any, error) {
	switch alg {
	case AlgRS256:
		return rsa.GenerateKey(rand.Reader, 2048)
	case AlgES256:
		return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}

	return nil, fmt.Errorf("unsupported signing algorithm %q", alg)
}

// newKeyRecord generates a key and returns the record to store (not saved).
func (s *Store) newKeyRecord(alg string) (*core.Record, error) {
	col, err := s.app.FindCachedCollectionByNameOrId(core.CollectionNameSigningKeys)
	if err != nil {
		return nil, err
	}

	priv, err := generateKey(alg)
	if err != nil {
		return nil, err
	}

	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	stored, encrypted, err := s.protect(string(pemBytes))
	if err != nil {
		return nil, err
	}

	kid := security.RandomString(16)

	var pub any
	switch k := priv.(type) {
	case *rsa.PrivateKey:
		pub = &k.PublicKey
	case *ecdsa.PrivateKey:
		pub = &k.PublicKey
	}

	jwk, err := json.Marshal(jose.JSONWebKey{Key: pub, KeyID: kid, Algorithm: alg, Use: "sig"})
	if err != nil {
		return nil, err
	}

	rec := core.NewRecord(col)
	rec.Set("kid", kid)
	rec.Set("alg", alg)
	rec.Set("privateKey", stored)
	rec.Set("publicJwk", string(jwk))
	rec.Set("encrypted", encrypted)

	return rec, nil
}

// protect encrypts the PEM with the encryption env key. Without the key it
// only passes the PEM through in dev mode.
func (s *Store) protect(pemText string) (string, bool, error) {
	if key := encryptionKey(s.app); key != "" {
		enc, err := security.Encrypt([]byte(pemText), key)
		if err != nil {
			return "", false, err
		}

		return keyEncryptedPrefix + enc, true, nil
	}

	if s.app.IsDev() {
		s.app.Logger().Warn("OIDC signing key stored unencrypted (dev mode); set PB_ENCRYPTION_KEY for any real deployment")

		return pemText, false, nil
	}

	return "", false, ErrPlaintextKeys
}

func (s *Store) unprotect(stored string) (string, error) {
	enc, ok := strings.CutPrefix(stored, keyEncryptedPrefix)
	if !ok {
		return stored, nil
	}

	key := encryptionKey(s.app)
	if key == "" {
		return "", fmt.Errorf("the signing key is encrypted but the encryption key env %q is not set", s.app.EncryptionEnv())
	}

	plain, err := security.Decrypt(enc, key)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt the signing key: %w", err)
	}

	return string(plain), nil
}

// ActiveKey returns the active key of the algorithm, creating it on first use.
func (s *Store) activeKeyRow(ctx context.Context, alg string) (*keyRow, error) {
	for attempt := 0; attempt < 2; attempt++ {
		rows, err := s.listKeys()
		if err != nil {
			return nil, err
		}

		for _, r := range rows {
			if r.Alg == alg && r.active() {
				r := r
				return &r, nil
			}
		}

		rec, err := s.newKeyRecord(alg)
		if err != nil {
			return nil, err
		}

		// a concurrent request may win the unique "one active key per alg"
		// index; the loop then picks its key up
		if err := s.app.SaveWithContext(detach(ctx), rec); err != nil {
			s.app.Logger().Debug("Signing key creation raced", "alg", alg, "error", err)
		}
	}

	return nil, errors.New("failed to create a signing key")
}

// RotateKeys makes a fresh key active for each algorithm and keeps the
// previous one in the JWKS until grace has passed. With no algorithms it
// rotates every algorithm that has an active key, or creates an RS256 key on
// a fresh install. It returns the new key ids.
func (s *Store) RotateKeys(ctx context.Context, algs []string, grace time.Duration) ([]string, error) {
	if grace <= 0 {
		grace = DefaultKeyGrace
	}

	rows, err := s.listKeys()
	if err != nil {
		return nil, err
	}

	if len(algs) == 0 {
		seen := map[string]bool{}
		for _, r := range rows {
			if r.active() && !seen[r.Alg] {
				seen[r.Alg] = true
				algs = append(algs, r.Alg)
			}
		}
		if len(algs) == 0 {
			algs = []string{AlgRS256}
		}
	}

	fresh := make([]*core.Record, 0, len(algs))
	for _, alg := range algs {
		if !validAlg(alg) {
			return nil, fmt.Errorf("unsupported signing algorithm %q", alg)
		}

		rec, err := s.newKeyRecord(alg)
		if err != nil {
			return nil, err
		}

		fresh = append(fresh, rec)
	}

	retire, _ := types.ParseDateTime(time.Now().Add(grace).UTC())
	ids := make([]string, 0, len(fresh))

	err = s.app.RunInTransaction(func(txApp core.App) error {
		for _, rec := range fresh {
			for _, r := range rows {
				if r.Alg != rec.GetString("alg") || !r.active() {
					continue
				}

				old, err := txApp.FindRecordById(core.CollectionNameSigningKeys, r.Id)
				if err != nil {
					return err
				}

				old.Set("retiresAt", retire)
				if err := txApp.SaveWithContext(detach(ctx), old); err != nil {
					return err
				}
			}

			if err := txApp.SaveWithContext(detach(ctx), rec); err != nil {
				return err
			}

			ids = append(ids, rec.GetString("kid"))
		}

		// forget keys that left the JWKS long ago
		_, _ = txApp.NonconcurrentDB().Delete(core.CollectionNameSigningKeys,
			dbx.NewExp("[[retiresAt]] != '' AND [[retiresAt]] < {:cutoff}", dbx.Params{
				"cutoff": time.Now().Add(-30 * 24 * time.Hour).UTC().Format(types.DefaultDateLayout),
			}),
		).Execute()

		return nil
	})
	if err != nil {
		return nil, err
	}

	return ids, nil
}

// ----- op.SigningKey / op.Key -------------------------------------------

type signingKey struct {
	kid string
	alg jose.SignatureAlgorithm
	key any
}

func (k *signingKey) SignatureAlgorithm() jose.SignatureAlgorithm { return k.alg }
func (k *signingKey) Key() any                                    { return k.key }
func (k *signingKey) ID() string                                  { return k.kid }

type publicKey struct{ jwk jose.JSONWebKey }

func (p publicKey) ID() string { return p.jwk.KeyID }
func (p publicKey) Algorithm() jose.SignatureAlgorithm {
	return jose.SignatureAlgorithm(p.jwk.Algorithm)
}
func (p publicKey) Use() string { return "sig" }
func (p publicKey) Key() any    { return p.jwk.Key }

func (s *Store) parsePrivate(r *keyRow) (*signingKey, error) {
	pemText, err := s.unprotect(r.PrivateKey)
	if err != nil {
		return nil, err
	}

	blk, _ := pem.Decode([]byte(pemText))
	if blk == nil {
		return nil, errors.New("invalid signing key")
	}

	priv, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}

	return &signingKey{kid: r.Kid, alg: jose.SignatureAlgorithm(r.Alg), key: priv}, nil
}

// SigningKey implements op.Storage. The algorithm is the one of the client
// whose token request is being served (see algContext), RS256 otherwise.
func (s *Store) SigningKey(ctx context.Context) (op.SigningKey, error) {
	alg := algFromContext(ctx)

	row, err := s.activeKeyRow(ctx, alg)
	if err != nil {
		return nil, err
	}

	return s.parsePrivate(row)
}

// SignatureAlgorithms implements op.Storage (discovery).
func (s *Store) SignatureAlgorithms(context.Context) ([]jose.SignatureAlgorithm, error) {
	return []jose.SignatureAlgorithm{jose.RS256, jose.ES256}, nil
}

// KeySet implements op.Storage: the active keys plus the ones still inside
// their grace period. It never creates a key.
func (s *Store) KeySet(context.Context) ([]op.Key, error) {
	rows, err := s.listKeys()
	if err != nil {
		return nil, err
	}

	now := time.Now()
	out := make([]op.Key, 0, len(rows))

	for _, r := range rows {
		if !r.usable(now) {
			continue
		}

		var jwk jose.JSONWebKey
		if err := json.Unmarshal([]byte(r.PublicJwk), &jwk); err != nil {
			continue
		}

		out = append(out, publicKey{jwk})
	}

	return out, nil
}

// KeyInfo is the public description of a signing key (never the private part).
type KeyInfo struct {
	Kid       string `json:"kid"`
	Alg       string `json:"alg"`
	Created   string `json:"created"`
	RetiresAt string `json:"retiresAt"`
	Status    string `json:"status"` // active, retiring, retired
	Encrypted bool   `json:"encrypted"`
}

// Keys lists the signing keys without any secret material.
func (s *Store) Keys() ([]KeyInfo, error) {
	rows, err := s.listKeys()
	if err != nil {
		return nil, err
	}

	now := time.Now()
	out := make([]KeyInfo, 0, len(rows))

	for _, r := range rows {
		status := "active"
		switch {
		case !r.RetiresAt.IsZero() && !r.RetiresAt.Time().After(now):
			status = "retired"
		case !r.RetiresAt.IsZero():
			status = "retiring"
		}

		info := KeyInfo{Kid: r.Kid, Alg: r.Alg, Created: r.Created.String(), Status: status, Encrypted: r.Encrypted}
		if !r.RetiresAt.IsZero() {
			info.RetiresAt = r.RetiresAt.String()
		}

		out = append(out, info)
	}

	return out, nil
}
