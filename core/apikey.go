package core

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #95): API keys.
//
// A key authenticates a request as its owner (an auth record, superuser
// included), restricted by the key's scopes. Scopes can only narrow what the
// owner is already allowed to do; every collection rule still runs as the owner.
//
// Format: clv_<prefix>_<secret>
//
//   - prefix: 8 random base32 characters. It is the lookup handle (unique
//     index) and is shown in the UI so a key can be recognised later.
//   - secret: 32 random bytes (256 bits) as 52 base32 characters.
//
// The alphabet is lowercase a-z2-7, so neither part can contain the "_"
// separator. Only the SHA-256 of the secret is stored. A fast hash is correct
// here: the secret is 256 bits of CSPRNG output, so there is nothing to
// brute-force and a slow KDF (bcrypt, argon2) would only add CPU cost to every
// request. The digest is compared in constant time.
//
// The full key is returned exactly once, by the create endpoint.

const (
	// CollectionNameAPIKeys is the hidden system collection holding the keys.
	CollectionNameAPIKeys = "_apiKeys"

	// APIKeyTokenPrefix starts every API key. JWTs never start with it, so it
	// cleanly separates the two credential kinds.
	APIKeyTokenPrefix = "clv_"

	APIKeyScopeAll   = "*"
	APIKeyScopeRead  = "read"
	APIKeyScopeWrite = "write"

	// APIKeyLastUsedInterval is the minimum gap between lastUsed writes.
	APIKeyLastUsedInterval = time.Minute

	// APIKeyMaxPerOwner is the maximum number of active keys per owner.
	APIKeyMaxPerOwner = 50

	apiKeyMaxScopes = 100
	apiKeyPrefixLen = 8
	apiKeySecretLen = 52 // base32 of 32 bytes, unpadded
)

// ErrInvalidAPIKey is returned for every kind of key failure (malformed,
// unknown, wrong secret, revoked, expired, owner gone) so callers cannot turn
// the difference into an oracle.
var ErrInvalidAPIKey = errors.New("invalid api key")

var (
	apiKeyEncoding   = base32.StdEncoding.WithPadding(base32.NoPadding)
	apiKeyScopeRegex = regexp.MustCompile(`^collection:([^:\s]+):(read|write)$`)
	apiKeyPartRegex  = regexp.MustCompile(`^[a-z2-7]+$`)
)

// APIKeyForbiddenCollections can never be reached with an API key, whatever
// its scopes: they hold credential material of other mechanisms.
var APIKeyForbiddenCollections = []string{
	CollectionNameAPIKeys,
	CollectionNameMFAs,
	CollectionNameOTPs,
	CollectionNameAuthOrigins,
	CollectionNameExternalAuths,
	CollectionNameWebAuthnCredentials,
	// #79a: membership changes must not be possible with a stolen key
	CollectionNameOrgs,
	CollectionNameOrgMembers,
	CollectionNameOrgRoles,
	CollectionNameInvitations, // #79b: hashed invitation tokens
	CollectionNameSCIMTokens,  // #79c: hashed SCIM tokens
	CollectionNameSCIMGroups,  // #79c: provisioning state
}

// APIKeyInfo describes the key that authenticated a request.
type APIKeyInfo struct {
	Id     string
	Prefix string
	Scopes []string
}

// HasAll reports whether the key carries the "*" scope.
func (k *APIKeyInfo) HasAll() bool {
	return slices.Contains(k.Scopes, APIKeyScopeAll)
}

// Allows reports whether the key's scopes cover the access (read or write,
// see APIKeyScopeRead/Write) to the named collection.
func (k *APIKeyInfo) Allows(collectionName string, access string) bool {
	if slices.Contains(APIKeyForbiddenCollections, collectionName) {
		return false
	}

	if k.HasAll() {
		return true
	}

	return slices.Contains(k.Scopes, "collection:"+collectionName+":"+access)
}

// GenerateAPIKey returns a new full key together with its prefix and the
// hash to store.
func GenerateAPIKey() (key, prefix, secretHash string, err error) {
	pb := make([]byte, 5) // 5 bytes -> exactly 8 base32 chars
	sb := make([]byte, 32)
	if _, err = rand.Read(pb); err != nil {
		return "", "", "", err
	}
	if _, err = rand.Read(sb); err != nil {
		return "", "", "", err
	}

	prefix = strings.ToLower(apiKeyEncoding.EncodeToString(pb))
	secret := strings.ToLower(apiKeyEncoding.EncodeToString(sb))

	return APIKeyTokenPrefix + prefix + "_" + secret, prefix, HashAPIKeySecret(secret), nil
}

// HashAPIKeySecret returns the hex SHA-256 of the key secret.
func HashAPIKeySecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// IsAPIKeyToken reports whether the token looks like an API key
// (as opposed to a JWT).
func IsAPIKeyToken(token string) bool {
	return strings.HasPrefix(token, APIKeyTokenPrefix)
}

// ParseAPIKey splits a key into its prefix and secret.
func ParseAPIKey(token string) (prefix, secret string, ok bool) {
	rest, found := strings.CutPrefix(token, APIKeyTokenPrefix)
	if !found {
		return "", "", false
	}

	prefix, secret, found = strings.Cut(rest, "_")
	if !found ||
		len(prefix) != apiKeyPrefixLen || len(secret) != apiKeySecretLen ||
		!apiKeyPartRegex.MatchString(prefix) || !apiKeyPartRegex.MatchString(secret) {
		return "", "", false
	}

	return prefix, secret, true
}

// ValidateAPIKeyScopes checks and normalizes (de-duplicates) a scope list:
// "*" or "collection:<name>:read|write" for an existing, reachable collection.
func ValidateAPIKeyScopes(app App, scopes []string) ([]string, error) {
	if len(scopes) == 0 {
		return nil, errors.New("at least one scope is required")
	}
	if len(scopes) > apiKeyMaxScopes {
		return nil, fmt.Errorf("at most %d scopes are allowed", apiKeyMaxScopes)
	}

	result := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)

		if scope != APIKeyScopeAll {
			m := apiKeyScopeRegex.FindStringSubmatch(scope)
			if m == nil {
				return nil, fmt.Errorf("invalid scope %q", scope)
			}
			if slices.Contains(APIKeyForbiddenCollections, m[1]) {
				return nil, fmt.Errorf("the collection %q cannot be accessed with an API key", m[1])
			}
			if _, err := app.FindCachedCollectionByNameOrId(m[1]); err != nil {
				return nil, fmt.Errorf("unknown collection in scope %q", scope)
			}
		}

		if !slices.Contains(result, scope) {
			result = append(result, scope)
		}
	}

	return result, nil
}

// AuthenticateAPIKey resolves an API key to its owner record.
//
// Every failure returns ErrInvalidAPIKey. The secret comparison is constant
// time and a dummy comparison runs for unknown prefixes, so neither the error
// nor the timing says which part was wrong.
func AuthenticateAPIKey(app App, token string) (*Record, *APIKeyInfo, error) {
	prefix, secret, ok := ParseAPIKey(token)
	if !ok {
		return nil, nil, ErrInvalidAPIKey
	}

	given := HashAPIKeySecret(secret)

	keyRecord, err := app.FindFirstRecordByData(CollectionNameAPIKeys, "prefix", prefix)
	if err != nil {
		subtle.ConstantTimeCompare([]byte(given), []byte(given))
		return nil, nil, ErrInvalidAPIKey
	}

	if subtle.ConstantTimeCompare([]byte(given), []byte(keyRecord.GetString("secretHash"))) != 1 {
		return nil, nil, ErrInvalidAPIKey
	}

	if keyRecord.GetBool("revoked") {
		return nil, nil, ErrInvalidAPIKey
	}

	if exp := keyRecord.GetDateTime("expires"); !exp.IsZero() && exp.Time().Before(time.Now()) {
		return nil, nil, ErrInvalidAPIKey
	}

	ownerCollection, err := app.FindCachedCollectionByNameOrId(keyRecord.GetString("collectionRef"))
	if err != nil || !ownerCollection.IsAuth() {
		return nil, nil, ErrInvalidAPIKey
	}

	owner, err := app.FindRecordById(ownerCollection, keyRecord.GetString("recordRef"))
	if err != nil {
		return nil, nil, ErrInvalidAPIKey
	}

	// a banned owner's keys stop working (#93)
	if IsBanned(app, owner) {
		return nil, nil, ErrInvalidAPIKey
	}

	touchAPIKeyLastUsed(app, keyRecord)

	var scopes []string
	_ = keyRecord.UnmarshalJSONField("scopes", &scopes)

	return owner, &APIKeyInfo{Id: keyRecord.Id, Prefix: prefix, Scopes: scopes}, nil
}

// touchAPIKeyLastUsed records the use at most once per APIKeyLastUsedInterval.
//
// The throttle is evaluated by the database (a conditional UPDATE), so
// concurrent requests cannot both write, and the write bypasses the record
// hooks on purpose: it must not produce an audit entry per minute.
func touchAPIKeyLastUsed(app App, keyRecord *Record) {
	now := time.Now().UTC()
	cutoff := now.Add(-APIKeyLastUsedInterval)

	if last := keyRecord.GetDateTime("lastUsed"); !last.IsZero() && last.Time().After(cutoff) {
		return
	}

	_, err := app.NonconcurrentDB().NewQuery(
		"UPDATE {{" + CollectionNameAPIKeys + "}} SET [[lastUsed]] = {:now} " +
			"WHERE [[id]] = {:id} AND ([[lastUsed]] = '' OR [[lastUsed]] IS NULL OR [[lastUsed]] < {:cutoff})",
	).Bind(dbx.Params{
		"now":    now.Format(types.DefaultDateLayout),
		"cutoff": cutoff.Format(types.DefaultDateLayout),
		"id":     keyRecord.Id,
	}).Execute()
	if err != nil {
		app.Logger().Debug("Failed to update the API key lastUsed", "id", keyRecord.Id, "error", err)
	}
}
