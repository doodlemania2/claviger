package core_test

import (
	"testing"

	"github.com/doodlemania2/claviger/core"
)

func TestNormalizePhoneRegion(t *testing.T) {
	t.Parallel()

	scenarios := []struct {
		in, region, want string
	}{
		{"+44 7700 900123", "", "+447700900123"},
		{"+44 7700 900123", "US", "+447700900123"}, // a + wins over the region
		{"(415) 555-0123", "", ""},
		{"(415) 555-0123", "US", "+14155550123"},
		{"415.555.0123", "us", "+14155550123"},
		{"1-415-555-0123", "CA", "+14155550123"},
		{"1 876 555 0123", "JM", "+18765550123"},
		{"555-0123", "US", ""},       // 7 digits
		{"2 415 555 0123", "US", ""}, // 11 digits not starting with 1
		{"015 555 0123", "US", ""},   // area code starts with 0
		{"415 155 0123", "US", ""},   // exchange starts with 1
		{"07700 900123", "GB", "+447700900123"},
		{"7700 900123", "GB", "+447700900123"},
		{"087 123 4567", "IE", "+353871234567"},
		{"0151 1234567", "DE", "+491511234567"},
		{"06 12 34 56 78", "FR", "+33612345678"},
		{"612 345 678", "ES", "+34612345678"},
		{"312 345 6789", "IT", "+393123456789"},
		{"912 345 678", "PT", "+351912345678"},
		{"06 12345678", "NL", "+31612345678"},
		{"0470 12 34 56", "BE", "+32470123456"},
		{"55 1234 5678", "MX", "+525512345678"},
		{"(11) 91234-5678", "BR", "+5511912345678"},
		{"0917 123 4567", "PH", "+639171234567"},
		{"010-1234-5678", "KR", "+821012345678"},
		{"091 234 5678", "VN", "+84912345678"},
		{"138 1234 5678", "CN", "+8613812345678"},
		{"0412 345 678", "AU", "+61412345678"},
		{"021 123 4567", "NZ", "+64211234567"},
		{"0044 7700 900123", "GB", ""}, // international prefix is not rewritten
		{"7700 900123 x5", "GB", ""},
		{"call the office", "US", ""},
		{"", "US", ""},
		{"12", "GB", ""},               // too short
		{"0123456789012345", "GB", ""}, // too long
		{"415 555 0123", "ZZ", ""},     // unknown region
		{"415 555 0123", "", ""},
	}

	for _, s := range scenarios {
		if got := core.NormalizePhoneRegion(s.in, s.region); got != s.want {
			t.Errorf("NormalizePhoneRegion(%q, %q) = %q, want %q", s.in, s.region, got, s.want)
		}
	}
}

func TestPhoneRegionsAndSettingsValidation(t *testing.T) {
	t.Parallel()

	for _, r := range []string{"US", "CA", "GB", "IE", "DE", "FR", "ES", "IT", "PT", "NL", "BE", "MX", "BR", "PH", "KR", "VN", "CN", "AU", "NZ"} {
		if !core.IsPhoneRegion(r) {
			t.Errorf("%s not supported", r)
		}
	}

	for _, ok := range []string{"", "US", "gb"} {
		if err := (core.SMSConfig{DefaultRegion: ok}).Validate(); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}

	for _, bad := range []string{"ZZ", "USA", "1"} {
		if err := (core.SMSConfig{DefaultRegion: bad}).Validate(); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
