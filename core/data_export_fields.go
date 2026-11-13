package core

import (
	"slices"
	"strings"
)

// Fork-local (Claviger #88, 88-1): what each built-in collection exports.
//
// This is the allowlist the export uses for built-in data map entries. The
// data map says which rows belong to a person; this table says which of their
// columns may leave the system. It is deliberately an allowlist: a column that
// is not named here is not exported, so a secret added to a system collection
// later does not leak by default. A test fails when a built-in entry that is
// not "none" has no row here.
//
// Secrets that must never appear (see the test TestDataExportNoSecrets):
// password hashes, tokenKey, TOTP secrets and backup codes, passkey credential
// ids and public keys, API key and SCIM token hashes, OAuth token hashes,
// invitation token hashes, wrapped data keys and signing keys.

type dataExportCtx struct {
	subject *Record
}

type dataExportSpec struct {
	// Fields are the exported columns (an allowlist).
	Fields []string

	// AllFields exports every column except the secrets that are never
	// exported (password fields, tokenKey, names that look like secrets).
	// Used for auth records, which are the person's own profile.
	AllFields bool

	// Redact adjusts one row before it is written: it removes what would
	// reveal other people's data. rec is the stored record.
	Redact func(c *dataExportCtx, rec *Record, row map[string]any)
}

// existence is the allowlist of collections whose export is "existence only".
var dataExportExistence = []string{"id", "created", "updated"}

var builtinDataExport = map[string]dataExportSpec{
	"users":       {AllFields: true},
	"_superusers": {AllFields: true},

	CollectionNameExternalAuths:       {Fields: []string{"id", "provider", "providerId", "created", "updated"}},
	CollectionNameWebAuthnCredentials: {Fields: []string{"id", "name", "created", "updated", "lastUsed"}},
	CollectionNameTOTPs:               {Fields: dataExportExistence},
	CollectionNameMFAs:                {Fields: dataExportExistence},
	CollectionNameOTPs:                {Fields: dataExportExistence},
	CollectionNameSessions: {Fields: []string{
		"id", "userAgent", "ip", "authMethod", "created", "lastSeen", "expires", "revoked", "revokedReason",
	}},
	CollectionNameAuthOrigins: {Fields: dataExportExistence},
	CollectionNameAPIKeys: {Fields: []string{
		"id", "name", "prefix", "scopes", "expires", "lastUsed", "revoked", "created",
	}},
	CollectionNameBans: {Fields: []string{"id", "reason", "until", "created"}},
	// #81: spec 10.3 exports the whole credential record to its holder,
	// including why it was revoked; recordedBy/verifiedBy are user ids only
	CollectionNameCredentials: {Fields: []string{
		"id", "org", "type", "issuedAt", "expires", "issuer", "recordedBy", "verifiedBy", "verified",
		"document", "revoked", "revokedReason", "revokedAt", "status", "lapsedAt", "created", "updated",
	}},

	// #80: a person sees their own member row and the shared contact data of
	// their household, never another member's details (the subject path of
	// each entry only reaches the person's own row)
	CollectionNameHouseholdMembers: {Fields: []string{
		"id", "household", "name", "role", "guardian", "isMinor", "birthDate", "photo",
		"allergies", "medical", "status", "source", "created", "updated",
	}},
	CollectionNameHouseholds: {Fields: []string{"id", "org", "name", "address", "homePhone", "status", "created", "updated"}},
	// #83: who vouched for the person and how; never the reference or the hash
	CollectionNameRecoveryCodes: {Fields: []string{
		"id", "org", "issuedBy", "confirmedBy", "channel", "verification", "note", "expires", "usedAt",
		"reportedAt", "attempts", "created", "updated",
	}},

	// #84: where the person was moved from and to, and who started and decided it
	CollectionNameOrgTransfers: {Fields: []string{
		"id", "fromOrg", "toOrg", "member", "state", "sourceOutcome", "startedBy", "expiresAt", "decidedAt", "decidedBy",
		"created", "updated",
	}},
	// #88 88-2: the whole evidence trail of the person, including the text
	// they were shown; no secrets. Purposes are not about a person (none).
	CollectionNameConsents: {Fields: []string{
		"id", "subject", "subjectType", "purpose", "policyVersion", "grantedAt", "withdrawnAt",
		"givenBy", "method", "evidenceRef", "org", "created", "updated",
	}},
	CollectionNameConsentEvents: {Fields: []string{
		"id", "subject", "subjectType", "purpose", "action", "policyVersion", "text", "givenBy",
		"actor", "method", "evidenceRef", "org", "pseudonymizedAt", "created",
	}},

	CollectionNameOrgMembers: {Fields: []string{
		"id", "org", "role", "status", "tags", "source", "externalId", "created", "updated",
	}},
	CollectionNameInvitations: {
		Fields: []string{
			"id", "org", "email", "role", "tags", "status", "expires", "acceptedAt", "created", "updated",
		},
		Redact: redactInvitation,
	},

	CollectionNameOAuthConsents:      {Fields: []string{"id", "clientId", "scopes", "created", "updated"}},
	CollectionNameOAuthTokens:        {Fields: []string{"id", "clientId", "scopes", "audience", "expires", "revoked", "created"}},
	CollectionNameOAuthRefreshTokens: {Fields: []string{"id", "clientId", "scopes", "audience", "expires", "used", "revoked", "created"}},
	CollectionNameOAuthRequests:      {Fields: []string{"id", "clientId", "status", "expires", "created"}},
	CollectionNameOAuthSso:           {Fields: []string{"id", "expires", "created"}},
	CollectionNameOAuthDeviceCodes:   {Fields: []string{"id", "clientId", "scopes", "status", "expires", "created"}},

	CollectionNameAuditLog: {
		Fields: []string{
			"id", "action", "actorType", "actorCollection", "actorId", "targetCollection", "targetId",
			"changedFields", "before", "after", "ip", "meta", "created",
		},
		Redact: redactAuditEntry,
	},
	CollectionNameWebhookDeliveries: {
		Fields: []string{"id", "eventType", "payload", "status", "attempts", "created"},
		Redact: redactWebhookDelivery,
	},

	CollectionNameDataRequests: {Fields: []string{
		"id", "type", "state", "selfService", "dueAt", "completedAt", "expires", "downloadedAt", "created",
	}},
}

// dataExportFieldsFor returns the columns to export for an entry, in the order
// of the collection, and the spec in force.
func dataExportFieldsFor(col *Collection, entry dataMapRow) ([]string, dataExportSpec) {
	spec := dataExportSpec{AllFields: true}

	if entry.Builtin {
		s, ok := builtinDataExport[col.Name]
		if !ok {
			s = dataExportSpec{Fields: dataExportExistence} // fail safe
		}
		spec = s
	}

	var out []string
	for _, f := range col.Fields {
		name := f.GetName()
		if spec.AllFields {
			if f.Type() == FieldTypePassword || name == FieldNameTokenKey || auditSensitiveNameRegex.MatchString(name) {
				continue
			}
			out = append(out, name)
			continue
		}
		if slices.Contains(spec.Fields, name) {
			out = append(out, name)
		}
	}

	return out, spec
}

// redactInvitation drops the invited address from invitations the subject
// sent to somebody else (that address is another person's data).
func redactInvitation(c *dataExportCtx, rec *Record, row map[string]any) {
	own := strings.EqualFold(rec.GetString("email"), c.subject.Email()) ||
		rec.GetString("acceptedBy") == c.subject.Id
	if !own {
		delete(row, "email")
	}
}

// redactAuditEntry: for entries about a record that is not the subject, only
// the action, time, target and IP stay. The before and after values describe
// somebody else's record.
func redactAuditEntry(c *dataExportCtx, rec *Record, row map[string]any) {
	about := rec.GetString("targetId") == c.subject.Id &&
		rec.GetString("targetCollection") == c.subject.Collection().Name
	if about {
		return
	}
	delete(row, "before")
	delete(row, "after")
	delete(row, "changedFields")
}

// redactWebhookDelivery keeps the payload only when it describes a record
// about the subject; otherwise the delivery is listed without its content.
func redactWebhookDelivery(c *dataExportCtx, rec *Record, row map[string]any) {
	var payload struct {
		Data struct {
			Collection string         `json:"collection"`
			RecordId   string         `json:"recordId"`
			Record     map[string]any `json:"record"`
		} `json:"data"`
	}
	if err := rec.UnmarshalJSONField("payload", &payload); err == nil {
		d := payload.Data
		if d.RecordId == c.subject.Id && d.Collection == c.subject.Collection().Name {
			return
		}
		for _, v := range d.Record {
			if s, ok := v.(string); ok && s == c.subject.Id {
				return
			}
		}
	}
	delete(row, "payload")
}
