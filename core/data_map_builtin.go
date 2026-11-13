package core

// Fork-local (Claviger #88, 88-0): the built-in data map entries.
//
// ADDING A SYSTEM COLLECTION: add one DataMapEntry line to builtinDataMap
// below, in the same change that creates the collection. If the collection's
// migration is numbered AFTER 1780001500_add_data_map.go, the list is not
// enough (the seed has already run): call
// core.RegisterDataMapEntry(txApp, entry) at the end of that migration
// instead. TestDataMapCoversSystemCollections fails until one of the two is
// done. A collection that holds no personal data still needs an entry, with
// Treatment "none" and a Note that says why.
//
// Subject paths use the field names of the collection. "recordRef" together
// with "collectionRef" points at an auth record; "id" means the row is the
// person.
var builtinDataMap = []DataMapEntry{
	// ---- auth records ----
	{Collection: "users", Treatment: DataMapTreatmentAnonymize, SubjectPaths: []string{"id"},
		PersonalFields: []string{"email", "name", "avatar"},
		Note:           "Default auth collection (an app may rename or replace it). Export omits the password hash and tokenKey; erasure sets email to erased-<id>@invalid, blanks the name and rotates tokens."},
	{Collection: "_superusers", Treatment: DataMapTreatmentAnonymize, SubjectPaths: []string{"id"},
		PersonalFields: []string{"email"},
		Note:           "Operator accounts. Not exported by self-service; handled by an administrator."},

	// ---- credentials and sign-in state ----
	{Collection: CollectionNameExternalAuths, Treatment: DataMapTreatmentErase, SubjectPaths: []string{"recordRef"},
		PersonalFields: []string{"provider", "providerId"}, Note: "Export: provider and provider id."},
	{Collection: CollectionNameWebAuthnCredentials, Treatment: DataMapTreatmentErase, SubjectPaths: []string{"recordRef"},
		PersonalFields: []string{"name", "created", "lastUsed"},
		Note:           "Export: name, created, last used. Never the credential id or public key."},
	{Collection: CollectionNameTOTPs, Treatment: DataMapTreatmentErase, SubjectPaths: []string{"recordRef"},
		Note: "Export: existence only. The secret and backup codes are never exported."},
	{Collection: CollectionNameMFAs, Treatment: DataMapTreatmentErase, SubjectPaths: []string{"recordRef"},
		Note: "Export: existence only."},
	{Collection: CollectionNameOTPs, Treatment: DataMapTreatmentErase, SubjectPaths: []string{"recordRef"},
		PersonalFields: []string{"sentTo"}, Note: "Export: existence only. The password hash is never exported."},
	{Collection: CollectionNameSessions, Treatment: DataMapTreatmentErase, SubjectPaths: []string{"recordRef"},
		PersonalFields: []string{"userAgent", "ip"}, Note: "Export: device label, IP, times."},
	{Collection: CollectionNameAuthOrigins, Treatment: DataMapTreatmentErase, SubjectPaths: []string{"recordRef"},
		PersonalFields: []string{"fingerprint"}, Note: "Export: existence and dates."},
	{Collection: CollectionNameAPIKeys, Treatment: DataMapTreatmentErase, SubjectPaths: []string{"recordRef"},
		PersonalFields: []string{"name"}, Note: "Export: name, prefix, scopes, last used. Never the secret hash."},
	{Collection: CollectionNameBans, Treatment: DataMapTreatmentRetain, SubjectPaths: []string{"recordRef"},
		PersonalFields: []string{"reason"}, AgeOutBasis: "until",
		RetainReason: "Security and legal interest: kept for the ban period, then erased.",
		Note:         "Export: reason and dates."},

	// ---- organizations ----
	{Collection: CollectionNameOrgMembers, Treatment: DataMapTreatmentErase, SubjectPaths: []string{"user"},
		PersonalFields: []string{"externalId"}, Note: "Export: roles, tags, source."},
	{Collection: CollectionNameInvitations, Treatment: DataMapTreatmentErase, SubjectPaths: []string{"email", "invitedBy", "acceptedBy"},
		PersonalFields: []string{"email"}, Note: "Matched by invited email address as well as by user."},
	{Collection: CollectionNameOrgs, Treatment: DataMapTreatmentNone,
		Note: "Organization names and settings. Not about a person."},
	{Collection: CollectionNameOrgRoles, Treatment: DataMapTreatmentNone,
		Note: "Role definitions. Not about a person."},
	{Collection: CollectionNameOrgClosure, Treatment: DataMapTreatmentNone,
		Note: "Org hierarchy closure table (#84), derived from _orgs. Not about a person."},
	{Collection: CollectionNameSCIMTokens, Treatment: DataMapTreatmentNone,
		Note: "Provisioning tokens of an organization (hash only). Not about a person."},
	{Collection: CollectionNameSCIMGroups, Treatment: DataMapTreatmentNone,
		Note: "Group names and member links as sent by the identity provider; the members are already covered by " + CollectionNameOrgMembers + "."},

	// ---- safeguarding credentials (#81) ----
	{Collection: CollectionNameCredentials, Treatment: DataMapTreatmentErase, SubjectPaths: []string{"user"},
		PersonalFields: []string{"issuer", "document", "revokedReason", "recordedBy", "verifiedBy"}, AgeOutBasis: "expires",
		Note: "Safeguarding credential attestations (#81). Export: all fields, including the revoke reason. Erased after the credential retention period set by the congregation."},
	{Collection: CollectionNameCredentialTypes, Treatment: DataMapTreatmentNone,
		Note: "Credential type definitions (#81). Not about a person."},

	// ---- households (#80) ----
	{Collection: CollectionNameHouseholdMembers, Treatment: DataMapTreatmentErase, SubjectPaths: []string{"user"},
		PersonalFields: []string{"name", "birthDate", "photo", "allergies", "medical"},
		Note:           "A person of a household (#80). Export: the person's own row only, never another member's details. A minor has no login, so a minor's row is not reached through a user."},
	{Collection: CollectionNameHouseholds, Treatment: DataMapTreatmentExport, SubjectPaths: []string{CollectionNameHouseholdMembers + "_via_household.user"},
		PersonalFields: []string{"address", "homePhone"},
		Note:           "The shared contact data of the household (#80), exported to each member with a login. Kept on erasure of one member: it belongs to the others too."},
	// ---- staff-vouched recovery (#83) ----
	{Collection: CollectionNameRecoveryCodes, Treatment: DataMapTreatmentErase, SubjectPaths: []string{"user"},
		PersonalFields: []string{"issuedBy", "confirmedBy", "note"}, AgeOutBasis: "expires",
		Note: "Staff-vouched recovery codes (#83). Export: who vouched, how, when. Never the reference or the hash. Rows are deleted 30 days after they expire."},

	// ---- member transfers between orgs (#84) ----
	{Collection: CollectionNameOrgTransfers, Treatment: DataMapTreatmentErase, SubjectPaths: []string{"person"},
		PersonalFields: []string{"startedBy", "decidedBy"},
		Note:           "Consent records of a move between orgs (#84). Export: both orgs, state, who started and decided. Pending rows expire after the transfer lifetime. A transfer of a member without a login (a minor) is about the household member row and goes with it."},
	// ---- consent (#88, 88-2) ----
	{Collection: CollectionNameConsentPurposes, Treatment: DataMapTreatmentNone,
		Note: "What a person can consent to (key, text, version, lawful basis). Not about a person."},
	{Collection: CollectionNameConsents, Treatment: DataMapTreatmentRetain,
		SubjectPaths:   []string{"subject", "subject~" + CollectionNameHouseholdMembers + ".user"},
		PersonalFields: []string{"subject", "givenBy", "evidenceRef"}, AgeOutBasis: "grantedAt",
		RetainReason: "Evidence that consent was given or withdrawn, kept for the limitation period. On erasure the ids are pseudonymized (PseudonymizeConsents), not deleted.",
		Note:         "Consent state per person and purpose (88-2). Export: all columns, including consents given for the person by a guardian."},
	{Collection: CollectionNameConsentEvents, Treatment: DataMapTreatmentRetain,
		SubjectPaths:   []string{"subject", "subject~" + CollectionNameHouseholdMembers + ".user"},
		PersonalFields: []string{"subject", "givenBy", "actor", "evidenceRef"}, AgeOutBasis: "created",
		RetainReason: "Evidence that consent was given or withdrawn, with the text shown. Append-only; kept for the limitation period and pseudonymized on erasure (PseudonymizeConsents).",
		Note:         "Append-only consent history (88-2). Export: every grant and withdrawal with the text version and the text shown."},
	// ---- permanent registers (#87) ----
	{Collection: CollectionNamePermanentCollections, Treatment: DataMapTreatmentNone,
		Note: "The list of permanent registers (#87). Configuration, not about a person."},

	// ---- OpenID Connect provider ----
	{Collection: CollectionNameOAuthConsents, Treatment: DataMapTreatmentErase, SubjectPaths: []string{"recordRef"},
		Note: "Export: client, scopes, dates."},
	{Collection: CollectionNameOAuthTokens, Treatment: DataMapTreatmentErase, SubjectPaths: []string{"subject"},
		Note: "Export: client, scopes, dates."},
	{Collection: CollectionNameOAuthRefreshTokens, Treatment: DataMapTreatmentErase, SubjectPaths: []string{"subject"},
		Note: "Export: client, scopes, dates. Never the token hash."},
	{Collection: CollectionNameOAuthRequests, Treatment: DataMapTreatmentErase, SubjectPaths: []string{"subject"},
		Note: "Short-lived authorize requests."},
	{Collection: CollectionNameOAuthSso, Treatment: DataMapTreatmentErase, SubjectPaths: []string{"subject"},
		Note: "Single sign-on sessions of the hosted sign-in page."},
	{Collection: CollectionNameOAuthDeviceCodes, Treatment: DataMapTreatmentErase, SubjectPaths: []string{"subject"},
		Note: "Pending device authorizations (short-lived)."},
	{Collection: CollectionNameOAuthClients, Treatment: DataMapTreatmentNone,
		Note: "Registered relying parties (applications). Not about a person."},
	{Collection: CollectionNameSigningKeys, Treatment: DataMapTreatmentNone,
		Note: "Token signing keys (encrypted). Not about a person."},

	// ---- operations ----
	{Collection: CollectionNameAuditLog, Treatment: DataMapTreatmentAnonymize, SubjectPaths: []string{"actorId", "targetId"},
		PersonalFields: []string{"before", "after", "ip", "changedFields", "meta"},
		Note:           "Append-only. Export: entries by or about the subject. Erasure scrubs before, after and ip of those entries and keeps action, time, collection and a pseudonymous record id (spec 10.6)."},
	{Collection: CollectionNameWebhooks, Treatment: DataMapTreatmentNone,
		Note: "Webhook endpoints configured by operators. Not about a person."},
	{Collection: CollectionNameWebhookDeliveries, Treatment: DataMapTreatmentErase, SubjectPaths: []string{"payload"},
		PersonalFields: []string{"payload", "attemptLog"},
		Note:           "Subject is found inside the payload (delivery events name the record). Deliveries whose payload names the subject are deleted."},
	{Collection: CollectionNameFieldKeys, Treatment: DataMapTreatmentNone,
		Note: "Wrapped data keys of encrypted fields. Key material, not personal data."},

	{Collection: CollectionNameDataRequests, Treatment: DataMapTreatmentErase, SubjectPaths: []string{"recordRef"},
		Note: "Export: the request, its state and dates (never the export file). The export ZIP is deleted with the row."},

	// The auxiliary database table _logs (request logs, IP and user id) is
	// not a collection. It is covered by the log retention setting
	// (logs.maxDays), not by a row here.
}

// BuiltinDataMapEntry returns the built-in entry of a collection, for the
// migrations that register their collections with RegisterDataMapEntry.
func BuiltinDataMapEntry(collection string) DataMapEntry {
	for _, e := range builtinDataMap {
		if e.Collection == collection {
			return e
		}
	}

	return DataMapEntry{Collection: collection, Treatment: DataMapTreatmentNone}
}
