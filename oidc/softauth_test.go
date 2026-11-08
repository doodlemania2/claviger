package oidc_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
)

// Copied from apis/record_auth_with_webauthn_ceremony_test.go (test helpers
// of another package cannot be imported): a minimal in-process WebAuthn
// authenticator with a real ECDSA P-256 key.

// authenticator data flag bits (WebAuthn section 6.1).
const (
	flagUserPresent            = 0x01
	flagUserVerified           = 0x04
	flagBackupEligible         = 0x08
	flagBackupState            = 0x10
	flagAttestedCredentialData = 0x40
)

// softAuthenticator is a minimal in-process WebAuthn authenticator. It holds a
// real ECDSA P-256 key and produces attestation/assertion payloads in exactly
// the wire format the browser would send (see encodeAttestation/encodeAssertion
// in apis/passkeys_page.go).
type softAuthenticator struct {
	key       *ecdsa.PrivateKey
	credID    []byte
	aaguid    []byte
	signCount uint32
	rpID      string
}

func newSoftAuthenticator(t testing.TB, rpID string) *softAuthenticator {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate authenticator key: %v", err)
	}

	credID := make([]byte, 32)
	if _, err := rand.Read(credID); err != nil {
		t.Fatalf("failed to generate credential id: %v", err)
	}

	return &softAuthenticator{
		key:       key,
		credID:    credID,
		aaguid:    make([]byte, 16), // all-zero aaguid, as "none" attestation requires
		signCount: 1,
		rpID:      rpID,
	}
}

// coseKey returns the credential public key in COSE_Key form. The integer map
// keys mirror webauthncose.EC2PublicKeyData's `cbor:"N,keyasint"` tags so the
// library decodes it into an EC2 key: 1=kty, 3=alg, -1=crv, -2=x, -3=y.
func (a *softAuthenticator) coseKey(t testing.TB) []byte {
	t.Helper()

	// Take the coordinates from the encoded uncompressed point rather than the
	// big.Int X/Y fields, which are deprecated as of Go 1.26.
	// Bytes() returns SEC 1 uncompressed form: 0x04 || X(32) || Y(32).
	pub, err := a.key.PublicKey.Bytes()
	if err != nil {
		t.Fatalf("failed to encode public key: %v", err)
	}
	if len(pub) != 65 || pub[0] != 0x04 {
		t.Fatalf("unexpected P-256 public key encoding: len=%d prefix=%#x", len(pub), pub[0])
	}

	x := pub[1:33]
	y := pub[33:65]

	raw, err := webauthncbor.Marshal(map[int]any{
		1:  2,  // kty: EC2
		3:  -7, // alg: ES256
		-1: 1,  // crv: P-256
		-2: x,
		-3: y,
	})
	if err != nil {
		t.Fatalf("failed to encode COSE key: %v", err)
	}

	return raw
}

// authData builds the authenticator data structure (WebAuthn §6.1).
func (a *softAuthenticator) authData(t testing.TB, includeAttestedCredential bool) []byte {
	t.Helper()

	rpIDHash := sha256.Sum256([]byte(a.rpID))

	// Backup eligible/state are set so the credential looks like a modern
	// multi-device passkey; the fork stores these flags on the credential.
	flags := byte(flagUserPresent | flagUserVerified | flagBackupEligible | flagBackupState)
	if includeAttestedCredential {
		flags |= flagAttestedCredentialData
	}

	buf := new(bytes.Buffer)
	buf.Write(rpIDHash[:])
	buf.WriteByte(flags)

	counter := make([]byte, 4)
	binary.BigEndian.PutUint32(counter, a.signCount)
	buf.Write(counter)

	if includeAttestedCredential {
		buf.Write(a.aaguid)

		credIDLen := make([]byte, 2)
		binary.BigEndian.PutUint16(credIDLen, uint16(len(a.credID)))
		buf.Write(credIDLen)
		buf.Write(a.credID)
		buf.Write(a.coseKey(t))
	}

	return buf.Bytes()
}

func (a *softAuthenticator) clientDataJSON(t testing.TB, ceremonyType, challenge, origin string) []byte {
	t.Helper()

	raw, err := json.Marshal(map[string]any{
		"type":        ceremonyType,
		"challenge":   challenge,
		"origin":      origin,
		"crossOrigin": false,
	})
	if err != nil {
		t.Fatalf("failed to encode clientDataJSON: %v", err)
	}

	return raw
}

// attestation produces a registration response using the "none" attestation
// format, which is what a browser returns when the RP does not request
// attestation conveyance (the fork leaves AttestationPreference unset).
func (a *softAuthenticator) attestation(t testing.TB, challenge, origin string) map[string]any {
	t.Helper()

	clientData := a.clientDataJSON(t, "webauthn.create", challenge, origin)
	rawAuthData := a.authData(t, true)

	attObj, err := webauthncbor.Marshal(map[string]any{
		"fmt":      "none",
		"attStmt":  map[string]any{},
		"authData": rawAuthData,
	})
	if err != nil {
		t.Fatalf("failed to encode attestation object: %v", err)
	}

	credIDb64 := base64.RawURLEncoding.EncodeToString(a.credID)

	return map[string]any{
		"id":                     credIDb64,
		"rawId":                  credIDb64,
		"type":                   "public-key",
		"clientExtensionResults": map[string]any{},
		"response": map[string]any{
			"attestationObject": base64.RawURLEncoding.EncodeToString(attObj),
			"clientDataJSON":    base64.RawURLEncoding.EncodeToString(clientData),
		},
	}
}

// assertion produces a login response, signing over
// authenticatorData || SHA256(clientDataJSON) as the spec requires.
func (a *softAuthenticator) assertion(t testing.TB, challenge, origin, userHandle string) map[string]any {
	t.Helper()

	a.signCount++

	clientData := a.clientDataJSON(t, "webauthn.get", challenge, origin)
	rawAuthData := a.authData(t, false)

	clientDataHash := sha256.Sum256(clientData)
	signed := append(append([]byte{}, rawAuthData...), clientDataHash[:]...)
	digest := sha256.Sum256(signed)

	sig, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		t.Fatalf("failed to sign assertion: %v", err)
	}

	credIDb64 := base64.RawURLEncoding.EncodeToString(a.credID)

	return map[string]any{
		"id":                     credIDb64,
		"rawId":                  credIDb64,
		"type":                   "public-key",
		"clientExtensionResults": map[string]any{},
		"response": map[string]any{
			"authenticatorData": base64.RawURLEncoding.EncodeToString(rawAuthData),
			"clientDataJSON":    base64.RawURLEncoding.EncodeToString(clientData),
			"signature":         base64.RawURLEncoding.EncodeToString(sig),
			"userHandle":        base64.RawURLEncoding.EncodeToString([]byte(userHandle)),
		},
	}
}

// ceremonyEnv wires a test app + router so a sequence of dependent requests can
