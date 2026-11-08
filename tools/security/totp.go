package security

import (
	"crypto/hmac"
	crand "crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP (RFC 6238) parameters used by Claviger. They are the values that every
// mainstream authenticator app supports (SHA1, 6 digits, 30s), so the
// otpauth:// URI does not need to carry them.
const (
	TOTPPeriod = 30 * time.Second
	TOTPDigits = 6

	// TOTPSkew is the number of steps accepted on each side of the current one.
	TOTPSkew = 1

	totpSecretBytes = 20 // 160 bits, the RFC 4226 recommended length
)

var totpBase32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateTOTPSecret returns a new random base32 (unpadded) TOTP shared secret.
func GenerateTOTPSecret() string {
	b := make([]byte, totpSecretBytes)
	if _, err := crand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return totpBase32.EncodeToString(b)
}

// HOTP computes an RFC 4226 HMAC-SHA1 one-time password with the given
// number of digits.
func HOTP(key []byte, counter uint64, digits int) string {
	mac := hmac.New(sha1.New, key)

	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	mac.Write(buf[:])
	sum := mac.Sum(nil)

	offset := sum[len(sum)-1] & 0x0f
	code := (uint32(sum[offset]&0x7f) << 24) |
		(uint32(sum[offset+1]) << 16) |
		(uint32(sum[offset+2]) << 8) |
		uint32(sum[offset+3])

	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}

	return fmt.Sprintf("%0*d", digits, code%mod)
}

// TOTPStep returns the RFC 6238 time step (counter) for the provided time.
func TOTPStep(t time.Time) int64 {
	return t.Unix() / int64(TOTPPeriod/time.Second)
}

// TOTPCodeAtStep returns the 6 digit code for the base32 secret and time step.
func TOTPCodeAtStep(secret string, step int64) (string, error) {
	key, err := decodeTOTPSecret(secret)
	if err != nil {
		return "", err
	}

	return HOTP(key, uint64(step), TOTPDigits), nil
}

// ValidateTOTP checks the code against the current time step and TOTPSkew
// steps on each side.
//
// A code is only accepted for a step strictly greater than lastStep
// (the step of the last accepted code), which makes every code single-use.
// It returns the matched step, to be stored as the new lastStep by the caller.
func ValidateTOTP(secret, code string, now time.Time, lastStep int64) (int64, bool, error) {
	key, err := decodeTOTPSecret(secret)
	if err != nil {
		return 0, false, err
	}

	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != TOTPDigits {
		return 0, false, nil
	}

	current := TOTPStep(now)

	var matched int64
	var found bool

	// no early exit: every candidate step is always evaluated
	for s := current - TOTPSkew; s <= current+TOTPSkew; s++ {
		expected := HOTP(key, uint64(s), TOTPDigits)
		if subtle.ConstantTimeCompare([]byte(expected), []byte(code)) == 1 && s > lastStep {
			if !found || s > matched {
				matched = s
				found = true
			}
		}
	}

	return matched, found, nil
}

// TOTPProvisioningURI builds the otpauth:// URI (Key URI Format) that
// authenticator apps import (usually from a QR code).
func TOTPProvisioningURI(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)

	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(TOTPDigits))
	q.Set("period", fmt.Sprint(int(TOTPPeriod/time.Second)))

	return "otpauth://totp/" + label + "?" + q.Encode()
}

func decodeTOTPSecret(secret string) ([]byte, error) {
	secret = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(secret), " ", ""))
	secret = strings.TrimRight(secret, "=")

	return totpBase32.DecodeString(secret)
}
