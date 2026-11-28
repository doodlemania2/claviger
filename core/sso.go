package core

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/doodlemania2/claviger/tools/security"
)

// Fork-local (Claviger #92): inbound enterprise single sign-on.
//
// Three hidden system collections hold the model (migrations 17800035NN):
//
//   - _ssoConnections: one upstream identity provider of an organization. The
//     "type" field selects the protocol implementation registered in package
//     sso ("oidc" today, "saml" later); type-specific settings live in the
//     "config" JSON field, so a new type needs no migration.
//   - _ssoDomains: an email domain an organization claims, proven by a DNS TXT
//     record. An unverified claim is never routed.
//   - _ssoFlows: the short-lived state of a sign-in in progress (state, nonce,
//     PKCE verifier, the one-time code handed back to the page).
//
// The logic is in package sso and apis/sso*.go; see docs/inbound-sso.md.
const (
	CollectionNameSSOConnections = "_ssoConnections"
	CollectionNameSSODomains     = "_ssoDomains"
	CollectionNameSSOFlows       = "_ssoFlows"

	SSOTypeOIDC = "oidc"
	SSOTypeSAML = "saml"

	SSODomainPending  = "pending"
	SSODomainVerified = "verified"

	// SSOVerificationPrefix starts the value of the DNS TXT record that proves
	// control of a domain: "claviger-verification=<token>".
	SSOVerificationPrefix = "claviger-verification="

	AuditActionSSODomainVerify = "sso_domain_verify"
	AuditActionSSOLogin        = "sso_login"
	AuditActionSSOJIT          = "sso_jit_create"
	AuditActionSSOLink         = "sso_link"
	AuditActionSSOLoginFailed  = "sso_login_failed"
	AuditActionSSORoleSync     = "sso_role_sync"
	AuditActionSSOBlocked      = "sso_password_blocked"

	ssoSecretPrefix = "sso:enc:v1:"
)

// SSOTypes lists the connection types the collection accepts.
var SSOTypes = []string{SSOTypeOIDC, SSOTypeSAML}

// SetSSOSecret returns the stored form of a connection secret: encrypted with
// the app encryption key (the one of the encrypted settings) when it is
// configured, plain otherwise, like webhook secrets.
func SetSSOSecret(app App, secret string) (string, error) {
	if secret == "" {
		return "", nil
	}

	key := os.Getenv(app.EncryptionEnv())
	if key == "" {
		return secret, nil
	}

	enc, err := security.Encrypt([]byte(secret), key)
	if err != nil {
		return "", err
	}

	return ssoSecretPrefix + enc, nil
}

// OpenSSOSecret reverses SetSSOSecret.
func OpenSSOSecret(app App, stored string) (string, error) {
	if !strings.HasPrefix(stored, ssoSecretPrefix) {
		return stored, nil
	}

	key := os.Getenv(app.EncryptionEnv())
	if key == "" {
		return "", fmt.Errorf("the SSO secret is encrypted but %q is not set", app.EncryptionEnv())
	}

	plain, err := security.Decrypt(strings.TrimPrefix(stored, ssoSecretPrefix), key)
	if err != nil {
		return "", err
	}

	return string(plain), nil
}

// NormalizeSSODomain lowercases and trims a domain and reports whether it is
// a plausible registrable DNS name (labels of letters, digits and hyphens,
// at least two labels, at most 253 characters, no wildcard).
func NormalizeSSODomain(d string) (string, bool) {
	d = strings.ToLower(strings.TrimSpace(d))
	d = strings.TrimSuffix(d, ".")

	if len(d) < 3 || len(d) > 253 || !strings.Contains(d, ".") {
		return "", false
	}

	for _, label := range strings.Split(d, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", false
		}

		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return "", false
			}
		}
	}

	return d, true
}

// SSOEmailDomain returns the normalized domain of an email address ("" when
// the value is not an address).
func SSOEmailDomain(email string) string {
	email = strings.TrimSpace(email)

	at := strings.LastIndex(email, "@")
	if at <= 0 || at == len(email)-1 {
		return ""
	}

	d, ok := NormalizeSSODomain(email[at+1:])
	if !ok {
		return ""
	}

	return d
}

// WriteSSOAudit writes one explicit audit entry for an SSO event (sign-in,
// account creation or link, domain verification, role sync). collection is
// the collection the entry is about, recordId the row (may be empty). The
// actor and IP come from ctx (core.WithAuditContext). A failure to write is
// returned: callers inside a transaction thereby roll the change back.
func WriteSSOAudit(app App, ctx context.Context, action, collection, recordId string, meta map[string]any) error {
	if !auditAvailable(app) {
		return nil
	}

	entry := &AuditEntry{Action: action, CollectionName: collection, RecordId: recordId, Meta: meta}

	if c, err := app.FindCachedCollectionByNameOrId(collection); err == nil {
		entry.CollectionId = c.Id
	}

	applyAuditContext(ctx, entry)

	return WriteAuditEntry(app, ctx, entry, nil)
}
