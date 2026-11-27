package sso

import (
	"net/netip"
	"testing"

	"github.com/doodlemania2/claviger/core"
)

func TestSafeReturnTo(t *testing.T) {
	for in, want := range map[string]string{
		"/oidc/login?request=1":         "/oidc/login?request=1",
		"https://app.example/passkeys":  "https://app.example/passkeys",
		"":                              "",
		"//evil.example":                "",
		"/\\evil.example":               "",
		"https://evil.example/":         "",
		"http://app.example/x":          "", // another scheme is another origin
		"javascript:alert(1)":           "",
		"relative/path":                 "",
		"https://user@app.example/x":    "",
		"https://app.example.evil.test": "",
	} {
		if got := SafeReturnTo("https://app.example", in); got != want {
			t.Errorf("SafeReturnTo(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMapRoleTakesTheHighestRank(t *testing.T) {
	c := &Connection{RoleMapping: map[string]string{"a": "viewer", "b": "admin", "c": "member"}}

	for _, tc := range []struct {
		groups []string
		want   string
	}{
		{nil, ""},
		{[]string{"zzz"}, ""},
		{[]string{"a"}, "viewer"},
		{[]string{"a", "c", "b"}, "admin"},
		{[]string{"A"}, ""}, // exact match
	} {
		if got := MapRole(c, tc.groups); got != tc.want {
			t.Errorf("MapRole(%v) = %q, want %q", tc.groups, got, tc.want)
		}
	}
}

func TestDomainMatches(t *testing.T) {
	for _, tc := range []struct {
		values []string
		want   bool
	}{
		{[]string{"claviger-verification=tok"}, true},
		{[]string{`"claviger-verification=tok"`}, true},
		{[]string{" claviger-verification=tok "}, true},
		{[]string{"v=spf1 -all", "claviger-verification=tok"}, true},
		{[]string{"claviger-verification=tok2"}, false},
		{[]string{"claviger-verification=tok extra"}, false},
		{[]string{"xclaviger-verification=tok"}, false},
		{nil, false},
	} {
		if got := DomainMatches(tc.values, "tok"); got != tc.want {
			t.Errorf("DomainMatches(%q) = %v, want %v", tc.values, got, tc.want)
		}
	}
}

func TestNormalizeIssuer(t *testing.T) {
	for in, want := range map[string]string{
		"https://idp.example/oauth2/default/":                                          "https://idp.example/oauth2/default",
		"https://idp.example/.well-known/openid-configuration":                         "https://idp.example",
		"  https://login.microsoftonline.com/t/v2.0/.well-known/openid-configuration ": "https://login.microsoftonline.com/t/v2.0",
	} {
		if got := NormalizeIssuer(in); got != want {
			t.Errorf("NormalizeIssuer(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBlockedIP(t *testing.T) {
	for ip, want := range map[string]bool{
		"8.8.8.8": false, "1.1.1.1": false, "2606:4700::1111": false,
		"127.0.0.1": true, "::1": true, "10.0.0.5": true, "192.168.1.1": true, "172.16.0.1": true,
		"169.254.169.254": true, "100.64.0.1": true, "0.0.0.0": true, "::ffff:10.0.0.1": true, "fd00:ec2::254": true,
	} {
		if got := blockedIP(netip.MustParseAddr(ip)); got != want {
			t.Errorf("blockedIP(%s) = %v, want %v", ip, got, want)
		}
	}
}

func TestValidateConnection(t *testing.T) {
	good := func() *Connection {
		return &Connection{
			Type: "oidc", Issuer: "https://idp.example.org", ClientID: "id", ClientSecret: "s",
			Scopes: []string{"openid"}, RoleMapping: map[string]string{"g": "admin"},
		}
	}

	if err := ValidateConnection(good()); err != nil {
		t.Fatalf("a valid connection: %v", err)
	}

	for name, mut := range map[string]func(c *Connection){
		"unknown type":       func(c *Connection) { c.Type = "ldap" },
		"saml not built yet": func(c *Connection) { c.Type = "saml" },
		"http issuer":        func(c *Connection) { c.Issuer = "http://idp.example.org" },
		"issuer with creds":  func(c *Connection) { c.Issuer = "https://u:p@idp.example.org" },
		"no issuer":          func(c *Connection) { c.Issuer = "" },
		"no client id":       func(c *Connection) { c.ClientID = "" },
		"no secret":          func(c *Connection) { c.ClientSecret = "" },
		"no openid":          func(c *Connection) { c.Scopes = []string{"email"} },
		"owner default":      func(c *Connection) { c.DefaultRole = core.OrgRoleOwner },
		"owner mapping":      func(c *Connection) { c.RoleMapping = map[string]string{"g": "owner"} },
		"bad role":           func(c *Connection) { c.RoleMapping = map[string]string{"g": "root"} },
		"empty mapping key":  func(c *Connection) { c.RoleMapping = map[string]string{" ": "member"} },
	} {
		c := good()
		mut(c)

		if err := ValidateConnection(c); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}

	if err := (oidcProvider{}).Validate(&Connection{Issuer: "http://127.0.0.1:9999", ClientID: "a", ClientSecret: "b", Scopes: []string{"openid"}}); err != nil {
		t.Errorf("a loopback http issuer is allowed for development: %v", err)
	}
}

func TestOIDCIsRegisteredThroughTheSeam(t *testing.T) {
	if _, ok := ProviderFor("oidc"); !ok {
		t.Fatal("oidc must be registered")
	}
}
