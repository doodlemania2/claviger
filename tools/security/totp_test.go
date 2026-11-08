package security_test

import (
	"net/url"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/tools/security"
)

// RFC 6238 appendix B (SHA1, 8 digits, secret "12345678901234567890").
func TestHOTPRFC6238Vectors(t *testing.T) {
	key := []byte("12345678901234567890")

	scenarios := []struct {
		unix     int64
		expected string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	}

	for _, s := range scenarios {
		got := security.HOTP(key, uint64(security.TOTPStep(time.Unix(s.unix, 0))), 8)
		if got != s.expected {
			t.Fatalf("t=%d: expected %s, got %s", s.unix, s.expected, got)
		}
	}
}

func TestValidateTOTP(t *testing.T) {
	secret := security.GenerateTOTPSecret()
	now := time.Unix(1700000000, 0)
	step := security.TOTPStep(now)

	codeAt := func(s int64) string {
		c, err := security.TOTPCodeAtStep(secret, s)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	scenarios := []struct {
		name     string
		code     string
		lastStep int64
		ok       bool
		step     int64
	}{
		{"current", codeAt(step), 0, true, step},
		{"previous (skew -1)", codeAt(step - 1), 0, true, step - 1},
		{"next (skew +1)", codeAt(step + 1), 0, true, step + 1},
		{"-2 rejected", codeAt(step - 2), 0, false, 0},
		{"+2 rejected", codeAt(step + 2), 0, false, 0},
		{"replay of the same step", codeAt(step), step, false, 0},
		{"older step than last used", codeAt(step - 1), step, false, 0},
		{"newer step than last used", codeAt(step + 1), step, true, step + 1},
		{"wrong length", "12345", 0, false, 0},
		{"spaces tolerated", codeAt(step)[:3] + " " + codeAt(step)[3:], 0, true, step},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			gotStep, ok, err := security.ValidateTOTP(secret, s.code, now, s.lastStep)
			if err != nil {
				t.Fatal(err)
			}
			if ok != s.ok || (ok && gotStep != s.step) {
				t.Fatalf("expected ok=%v step=%d, got ok=%v step=%d", s.ok, s.step, ok, gotStep)
			}
		})
	}

	if _, _, err := security.ValidateTOTP("not base32!!", "123456", now, 0); err == nil {
		t.Fatal("expected invalid secret error")
	}
}

func TestTOTPProvisioningURI(t *testing.T) {
	u, err := url.Parse(security.TOTPProvisioningURI("Claviger", "a@example.com", "ABCDEF"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "otpauth" || u.Host != "totp" {
		t.Fatalf("unexpected uri %s", u)
	}
	if u.Query().Get("secret") != "ABCDEF" || u.Query().Get("issuer") != "Claviger" || u.Query().Get("digits") != "6" {
		t.Fatalf("unexpected query %s", u.RawQuery)
	}
}

func TestGenerateTOTPSecret(t *testing.T) {
	a, b := security.GenerateTOTPSecret(), security.GenerateTOTPSecret()
	if a == b || len(a) != 32 {
		t.Fatalf("unexpected secrets %q %q", a, b)
	}
}
