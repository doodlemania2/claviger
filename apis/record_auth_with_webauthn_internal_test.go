package apis

import (
	"reflect"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"
)

func newWebAuthnInitTestApp(t *testing.T) *core.BaseApp {
	t.Helper()

	app := core.NewBaseApp(core.BaseAppConfig{})

	app.Settings().Meta.AppName = "PocketBase Test"
	app.Settings().Meta.AppURL = "https://auth.stfoafrisco.org"

	return app
}

func TestInitWebAuthnFallbackToAppURL(t *testing.T) {
	app := newWebAuthnInitTestApp(t)

	t.Setenv("WEBAUTHN_RP_ID", "")
	t.Setenv("WEBAUTHN_RP_ORIGINS", "")

	wa, err := initWebAuthn(app)
	if err != nil {
		t.Fatal(err)
	}

	if wa.Config.RPID != "auth.stfoafrisco.org" {
		t.Fatalf("expected fallback RPID %q, got %q", "auth.stfoafrisco.org", wa.Config.RPID)
	}

	if !reflect.DeepEqual(wa.Config.RPOrigins, []string{"https://auth.stfoafrisco.org"}) {
		t.Fatalf("expected fallback RPOrigins %v, got %v", []string{"https://auth.stfoafrisco.org"}, wa.Config.RPOrigins)
	}
}

func TestInitWebAuthnUsesEnvOverrides(t *testing.T) {
	app := newWebAuthnInitTestApp(t)

	t.Setenv("WEBAUTHN_RP_ID", "stfoafrisco.org")
	t.Setenv("WEBAUTHN_RP_ORIGINS", "https://app.stfoafrisco.org")

	wa, err := initWebAuthn(app)
	if err != nil {
		t.Fatal(err)
	}

	if wa.Config.RPID != "stfoafrisco.org" {
		t.Fatalf("expected override RPID %q, got %q", "stfoafrisco.org", wa.Config.RPID)
	}

	if !reflect.DeepEqual(wa.Config.RPOrigins, []string{"https://app.stfoafrisco.org"}) {
		t.Fatalf("expected override RPOrigins %v, got %v", []string{"https://app.stfoafrisco.org"}, wa.Config.RPOrigins)
	}
}

func TestInitWebAuthnParsesCommaSeparatedOrigins(t *testing.T) {
	app := newWebAuthnInitTestApp(t)

	t.Setenv("WEBAUTHN_RP_ID", "stfoafrisco.org")
	t.Setenv("WEBAUTHN_RP_ORIGINS", " https://app.stfoafrisco.org, , https://auth.stfoafrisco.org/ , ")

	wa, err := initWebAuthn(app)
	if err != nil {
		t.Fatal(err)
	}

	expected := []string{"https://app.stfoafrisco.org", "https://auth.stfoafrisco.org"}
	if !reflect.DeepEqual(wa.Config.RPOrigins, expected) {
		t.Fatalf("expected parsed RPOrigins %v, got %v", expected, wa.Config.RPOrigins)
	}
}

func TestInitWebAuthnCacheBustsOnOverrideChanges(t *testing.T) {
	app := newWebAuthnInitTestApp(t)

	t.Setenv("WEBAUTHN_RP_ID", "stfoafrisco.org")
	t.Setenv("WEBAUTHN_RP_ORIGINS", "https://app.stfoafrisco.org")

	wa1, err := initWebAuthn(app)
	if err != nil {
		t.Fatal(err)
	}

	entryRaw1, ok := app.Store().GetOk(webauthnRPCacheKey)
	if !ok {
		t.Fatal("expected cache entry after first initWebAuthn call")
	}
	entry1, ok := entryRaw1.(*webauthnRPCacheEntry)
	if !ok || entry1 == nil {
		t.Fatal("expected cache entry of type *webauthnRPCacheEntry")
	}
	cacheKey1 := entry1.cacheKey

	t.Setenv("WEBAUTHN_RP_ORIGINS", "https://auth.stfoafrisco.org")

	wa2, err := initWebAuthn(app)
	if err != nil {
		t.Fatal(err)
	}

	entryRaw2, ok := app.Store().GetOk(webauthnRPCacheKey)
	if !ok {
		t.Fatal("expected cache entry after second initWebAuthn call")
	}
	entry2, ok := entryRaw2.(*webauthnRPCacheEntry)
	if !ok || entry2 == nil {
		t.Fatal("expected cache entry of type *webauthnRPCacheEntry")
	}

	if wa1 == wa2 {
		t.Fatal("expected a rebuilt webauthn instance when overrides change")
	}

	if cacheKey1 == entry2.cacheKey {
		t.Fatal("expected cache key to change when override values change")
	}

	if !reflect.DeepEqual(wa2.Config.RPOrigins, []string{"https://auth.stfoafrisco.org"}) {
		t.Fatalf("expected updated RPOrigins %v, got %v", []string{"https://auth.stfoafrisco.org"}, wa2.Config.RPOrigins)
	}
}

// TestInitWebAuthnRejectsIPRelyingPartyID pins the RP-ID domain requirement.
//
// go-webauthn v0.18.0 started rejecting IP-based RP-IDs, which v0.17.4 had
// accepted. An IP RP-ID could never complete a real ceremony anyway (browsers
// refuse it), so the fork fails fast with an actionable message rather than
// surfacing the library error. This test keeps that behaviour stable across
// future library bumps.
func TestInitWebAuthnRejectsIPRelyingPartyID(t *testing.T) {
	scenarios := []struct {
		name   string
		appURL string
	}{
		{"ipv4 loopback", "http://127.0.0.1:8090"},
		{"ipv6 loopback", "http://[::1]:8090"},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			app := newWebAuthnInitTestApp(t)
			app.Settings().Meta.AppURL = s.appURL

			t.Setenv("WEBAUTHN_RP_ID", "")
			t.Setenv("WEBAUTHN_RP_ORIGINS", "")

			wa, err := initWebAuthn(app)
			if err == nil {
				t.Fatalf("expected an error for RP-ID %q, got %v", s.appURL, wa)
			}
			if !strings.Contains(err.Error(), "must be a domain") {
				t.Fatalf("expected a domain-requirement error, got %v", err)
			}
		})
	}
}

// TestInitWebAuthnLocalhostStillSupported guards the supported local-dev path:
// plain http on localhost must keep working after the IP restriction above.
func TestInitWebAuthnLocalhostStillSupported(t *testing.T) {
	app := newWebAuthnInitTestApp(t)
	app.Settings().Meta.AppURL = "http://localhost:8090"

	t.Setenv("WEBAUTHN_RP_ID", "")
	t.Setenv("WEBAUTHN_RP_ORIGINS", "")

	wa, err := initWebAuthn(app)
	if err != nil {
		t.Fatalf("localhost must remain a valid local-dev RP-ID: %v", err)
	}
	if got := wa.Config.RPID; got != "localhost" {
		t.Fatalf("RPID = %q, want %q", got, "localhost")
	}
}

// TestInitWebAuthnIPAppURLWithDomainOverride documents the escape hatch: an
// IP-bound app URL is still usable for local dev by naming a domain RP-ID.
func TestInitWebAuthnIPAppURLWithDomainOverride(t *testing.T) {
	app := newWebAuthnInitTestApp(t)
	app.Settings().Meta.AppURL = "http://127.0.0.1:8090"

	t.Setenv("WEBAUTHN_RP_ID", "localhost")
	t.Setenv("WEBAUTHN_RP_ORIGINS", "http://localhost:8090")

	wa, err := initWebAuthn(app)
	if err != nil {
		t.Fatalf("WEBAUTHN_RP_ID override must rescue an IP app URL: %v", err)
	}
	if got := wa.Config.RPID; got != "localhost" {
		t.Fatalf("RPID = %q, want %q", got, "localhost")
	}
}
