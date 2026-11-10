package core

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/hook"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #81): safeguarding credentials.
//
// Two hidden system collections hold the model:
//
//   - _credentialTypes: what an org recognizes (key, name, validity, whether a
//     second verifier or a document is needed). A type defined at an org also
//     applies to its descendants.
//   - _credentials: one attestation per person and type: dates, issuer, who
//     recorded and verified it. Claviger stores the attestation, never the
//     result of a background check.
//
// Writes go through the functions in this file (and the /api/orgs/{id}/...
// routes built on them); the collections have null write rules. Access is
// gated by plain rule strings (credential_rules.go) that read `expires`, so
// access lapses at the expiry with no job. The daily job (RunCredentialLifecycle)
// only keeps the plain `status` for the dashboard and emits the audit entries
// and webhook events for upcoming expiry and lapse.

const (
	CollectionNameCredentialTypes = "_credentialTypes"
	CollectionNameCredentials     = "_credentials"

	// CredentialCoordinatorTag marks members who may record and verify
	// credentials besides owners and admins.
	CredentialCoordinatorTag = "safeguarding"

	CredentialTypeActive  = "active"
	CredentialTypeRetired = "retired"

	// the stored (dashboard) status of a credential
	CredentialStatusPending = "pending"
	CredentialStatusValid   = "valid"
	CredentialStatusLapsed  = "lapsed"
	CredentialStatusRevoked = "revoked"

	// the standing of a person for one type (access-status route)
	CredentialStandingCleared  = "cleared"
	CredentialStandingExpiring = "expiring"
	CredentialStandingPending  = "pending"
	CredentialStandingLapsed   = "lapsed"
	CredentialStandingRevoked  = "revoked"
	CredentialStandingMissing  = "missing"

	// audit actions (and the webhook event types) of the lifecycle job
	AuditActionCredentialExpiring = "credential_expiring"
	AuditActionCredentialLapsed   = "credential_lapsed"

	// DefaultCredentialExpiringDays is the "expiring soon" window of the
	// access-status route, and the largest default reminder.
	DefaultCredentialExpiringDays = 60

	credentialCronId   = "__pbCredentialLifecycle__"
	credentialCronExpr = "17 6 * * *" // daily at 06:17 (off the other jobs)
)

// DefaultCredentialReminderDays are the reminder thresholds in days before
// the expiry (org setting credentialReminderDays overrides them).
var DefaultCredentialReminderDays = []int{60, 30, 7}

var credentialKeyRegex = regexp.MustCompile(`^[a-z0-9_]{2,40}$`)

func credErr(status int, msg string) error { return orgErr(status, msg) }

// CredentialsAvailable reports whether the credentials model exists.
func CredentialsAvailable(app App) bool {
	_, err := app.FindCachedCollectionByNameOrId(CollectionNameCredentials)

	return err == nil
}

// ValidCredentialKey reports whether s is usable as a credential type key.
func ValidCredentialKey(s string) bool { return credentialKeyRegex.MatchString(s) }

// ---- org settings ------------------------------------------------------------

func credentialOrgSettings(org *Record) map[string]any {
	out := map[string]any{}

	raw := org.GetString("settings")
	if raw == "" {
		return out
	}

	_ = json.Unmarshal([]byte(raw), &out)

	return out
}

// CredentialDocumentsEnabled reports whether the org allows document uploads
// (org setting credentialDocuments, off by default).
func CredentialDocumentsEnabled(org *Record) bool {
	v, _ := credentialOrgSettings(org)["credentialDocuments"].(bool)

	return v
}

// CredentialReminderDays returns the reminder thresholds of an org, largest
// first (setting credentialReminderDays, 1 to 365 each, at most 6).
func CredentialReminderDays(org *Record) []int {
	days := slices.Clone(DefaultCredentialReminderDays)

	if list, ok := credentialOrgSettings(org)["credentialReminderDays"].([]any); ok && len(list) > 0 {
		parsed := []int{}

		for _, v := range list {
			if f, ok := v.(float64); ok && f >= 1 && f <= 365 && f == float64(int(f)) && !slices.Contains(parsed, int(f)) {
				parsed = append(parsed, int(f))
			}
		}

		if len(parsed) > 0 && len(parsed) <= 6 {
			days = parsed
		}
	}

	sort.Sort(sort.Reverse(sort.IntSlice(days)))

	return days
}

// ---- who may coordinate ------------------------------------------------------

// CredentialActor is who performs a credential operation.
type CredentialActor struct {
	*OrgActor
	// Coordinator may record, verify and revoke credentials of the org.
	Coordinator bool
	// Overseer may read the credentials of the org: a coordinator of the org
	// or of any ancestor.
	Overseer bool
}

// isCredentialCoordinator: owner or admin, or a member (not a viewer) with
// the safeguarding tag, at exactly this org.
func isCredentialCoordinator(app App, org *Record, userId string) bool {
	m := ActiveOrgMember(app, org, userId)
	if m == nil {
		return false
	}

	switch m.GetString("role") {
	case OrgRoleOwner, OrgRoleAdmin:
		return true
	case OrgRoleMember:
		return slices.Contains(OrgTagNames(app, m), CredentialCoordinatorTag)
	}

	return false
}

// NewCredentialActor resolves the standing of auth in the org.
func NewCredentialActor(app App, org *Record, auth *Record) *CredentialActor {
	a := &CredentialActor{OrgActor: NewOrgActor(app, org, auth)}

	if auth == nil {
		return a
	}

	if a.Superuser {
		a.Coordinator, a.Overseer = true, true

		return a
	}

	a.Coordinator = isCredentialCoordinator(app, org, auth.Id)
	a.Overseer = a.Coordinator

	if !a.Overseer && orgClosureAvailable(app) {
		rows, err := orgAncestorClosure(app, org.Id)
		if err == nil {
			for _, r := range rows {
				if r.Depth == 0 {
					continue
				}

				if anc, err := app.FindRecordById(CollectionNameOrgs, r.Ancestor); err == nil &&
					isCredentialCoordinator(app, anc, auth.Id) {
					a.Overseer = true

					break
				}
			}
		}
	}

	return a
}

// ---- credential types --------------------------------------------------------

// CredentialTypeInput describes a credential type.
type CredentialTypeInput struct {
	Key              string
	Name             string
	ValidMonths      int
	RequiresDocument bool
	RequiresVerifier bool
}

// CredentialTypePatch changes a type (nil = unchanged). The key never changes.
type CredentialTypePatch struct {
	Name             *string
	ValidMonths      *int
	RequiresDocument *bool
	RequiresVerifier *bool
	Status           *string
}

func requireCredentialAdmin(actor *CredentialActor, org *Record) error {
	if !actor.CanManage() {
		return credErr(403, "Only owners and admins can manage credential types.")
	}

	return requireLiveOrg(org, actor.OrgActor)
}

// CreateCredentialType defines a type at the org.
func CreateCredentialType(app App, ctx context.Context, actor *CredentialActor, org *Record, in CredentialTypeInput) (*Record, error) {
	if err := requireCredentialAdmin(actor, org); err != nil {
		return nil, err
	}

	in.Key = strings.TrimSpace(in.Key)
	in.Name = strings.TrimSpace(in.Name)

	if !ValidCredentialKey(in.Key) {
		return nil, credErr(400, "The key must be 2 to 40 characters of a-z, 0-9 and _.")
	}

	if in.Name == "" || len(in.Name) > 200 {
		return nil, credErr(400, "A name of up to 200 characters is required.")
	}

	if in.ValidMonths < 0 || in.ValidMonths > 120 {
		return nil, credErr(400, "validMonths must be between 0 and 120.")
	}

	if in.RequiresDocument && !CredentialDocumentsEnabled(org) {
		return nil, credErr(400, "Document uploads are off for this organization (setting credentialDocuments).")
	}

	if _, err := app.FindFirstRecordByFilter(CollectionNameCredentialTypes, "org={:org} && key={:key}",
		dbx.Params{"org": org.Id, "key": in.Key}); err == nil {
		return nil, credErr(409, "That credential type already exists.")
	}

	col, err := app.FindCachedCollectionByNameOrId(CollectionNameCredentialTypes)
	if err != nil {
		return nil, err
	}

	rec := NewRecord(col)
	rec.Set("org", org.Id)
	rec.Set("key", in.Key)
	rec.Set("name", in.Name)
	rec.Set("validMonths", in.ValidMonths)
	rec.Set("requiresDocument", in.RequiresDocument)
	rec.Set("requiresVerifier", in.RequiresVerifier)
	rec.Set("status", CredentialTypeActive)

	if err := saveOrgRecord(app, ctx, rec); err != nil {
		return nil, err
	}

	return rec, nil
}

// UpdateCredentialType changes a type of the org.
func UpdateCredentialType(app App, ctx context.Context, actor *CredentialActor, org, typ *Record, p CredentialTypePatch) (*Record, error) {
	if typ.GetString("org") != org.Id {
		return nil, credErr(404, "Credential type not found.")
	}

	if err := requireCredentialAdmin(actor, org); err != nil {
		return nil, err
	}

	if p.Name != nil {
		n := strings.TrimSpace(*p.Name)
		if n == "" || len(n) > 200 {
			return nil, credErr(400, "A name of up to 200 characters is required.")
		}

		typ.Set("name", n)
	}

	if p.ValidMonths != nil {
		if *p.ValidMonths < 0 || *p.ValidMonths > 120 {
			return nil, credErr(400, "validMonths must be between 0 and 120.")
		}

		typ.Set("validMonths", *p.ValidMonths)
	}

	if p.RequiresDocument != nil {
		if *p.RequiresDocument && !CredentialDocumentsEnabled(org) {
			return nil, credErr(400, "Document uploads are off for this organization (setting credentialDocuments).")
		}

		typ.Set("requiresDocument", *p.RequiresDocument)
	}

	if p.RequiresVerifier != nil {
		typ.Set("requiresVerifier", *p.RequiresVerifier)
	}

	if p.Status != nil {
		if *p.Status != CredentialTypeActive && *p.Status != CredentialTypeRetired {
			return nil, credErr(400, "Invalid status.")
		}

		typ.Set("status", *p.Status)
	}

	if err := saveOrgRecord(app, ctx, typ); err != nil {
		return nil, err
	}

	return typ, nil
}

// DeleteCredentialType deletes a type that no credential of the org uses;
// retire it instead otherwise.
func DeleteCredentialType(app App, ctx context.Context, actor *CredentialActor, org, typ *Record) error {
	if typ.GetString("org") != org.Id {
		return credErr(404, "Credential type not found.")
	}

	if err := requireCredentialAdmin(actor, org); err != nil {
		return err
	}

	if used, _ := app.FindFirstRecordByFilter(CollectionNameCredentials, "org={:org} && type={:key}",
		dbx.Params{"org": org.Id, "key": typ.GetString("key")}); used != nil {
		return credErr(409, "Credentials of this type exist. Retire the type instead.")
	}

	return deleteOrgRecord(app, ctx, typ)
}

// orgChain returns the org id followed by its ancestors, nearest first.
func orgChain(app App, orgId string) []string {
	ids := []string{orgId}

	if !orgClosureAvailable(app) {
		return ids
	}

	rows, err := orgAncestorClosure(app, orgId)
	if err != nil {
		return ids
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].Depth < rows[j].Depth })

	ids = ids[:0]
	for _, r := range rows {
		ids = append(ids, r.Ancestor)
	}

	if len(ids) == 0 {
		return []string{orgId}
	}

	return ids
}

// FindCredentialType returns the nearest type with the key, defined at the
// org or at one of its ancestors.
func FindCredentialType(app App, orgId, key string) (*Record, error) {
	for _, id := range orgChain(app, orgId) {
		if t, err := app.FindFirstRecordByFilter(CollectionNameCredentialTypes, "org={:org} && key={:key}",
			dbx.Params{"org": id, "key": key}); err == nil {
			return t, nil
		}
	}

	return nil, credErr(404, "Credential type not found.")
}

// CredentialTypesFor lists the types that apply to the org: its own and its
// ancestors', nearest definition of a key winning.
func CredentialTypesFor(app App, orgId string) ([]*Record, error) {
	out := []*Record{}
	seen := map[string]bool{}

	for _, id := range orgChain(app, orgId) {
		recs, err := app.FindRecordsByFilter(CollectionNameCredentialTypes, "org={:org}", "key", 0, 0, dbx.Params{"org": id})
		if err != nil {
			return nil, err
		}

		for _, t := range recs {
			if !seen[t.GetString("key")] {
				seen[t.GetString("key")] = true
				out = append(out, t)
			}
		}
	}

	return out, nil
}

// ---- credentials -------------------------------------------------------------

// CredentialInput describes a credential to record.
type CredentialInput struct {
	// User is a record id or an email of the member collection.
	User     string
	Type     string
	IssuedAt time.Time // zero = now
	Expires  time.Time // zero = issuedAt + the type's validMonths
	Issuer   string
	// Document is an optional file (a *filesystem.File); only when the org
	// allows documents.
	Document any
}

// CredentialPatch changes a credential (nil/zero = unchanged).
type CredentialPatch struct {
	IssuedAt time.Time
	Expires  time.Time
	Issuer   *string
	Document any
}

// CredentialState is the standing of a single credential at the time.
func CredentialState(c *Record, now time.Time) string {
	switch {
	case c.GetBool("revoked"):
		return CredentialStatusRevoked
	case !c.GetDateTime("expires").Time().After(now):
		return CredentialStatusLapsed
	case !c.GetBool("verified"):
		return CredentialStatusPending
	}

	return CredentialStatusValid
}

func credentialDateStamp(t time.Time) types.DateTime {
	dt, _ := types.ParseDateTime(t.UTC())

	return dt
}

func holderInSubtree(app App, org *Record, userId string) bool {
	subtree := map[string]bool{org.Id: true}

	if orgClosureAvailable(app) {
		if rows, err := orgSubtreeClosure(app, org.Id); err == nil {
			for _, r := range rows {
				subtree[r.Descendant] = true
			}
		}
	}

	ms, err := app.FindRecordsByFilter(CollectionNameOrgMembers, "user={:user}", "", 0, 0, dbx.Params{"user": userId})
	if err != nil {
		return false
	}

	for _, m := range ms {
		if subtree[m.GetString("org")] {
			return true
		}
	}

	return false
}

func requireCoordinator(actor *CredentialActor, org *Record) error {
	if !actor.Coordinator {
		return credErr(403, "Only safeguarding coordinators (owners, admins, or members tagged \""+CredentialCoordinatorTag+"\") can do this.")
	}

	return requireLiveOrg(org, actor.OrgActor)
}

// refuseSelf stops a coordinator from approving their own credential.
func refuseSelf(actor *CredentialActor, holderId string) error {
	if actor.Record != nil && !actor.Superuser && !actor.System && actor.Record.Id == holderId {
		return credErr(403, "You cannot record or verify your own credential. Ask another coordinator.")
	}

	return nil
}

func actorUserId(actor *CredentialActor) string {
	if actor.Record == nil || actor.Superuser || actor.System {
		return ""
	}

	return actor.Record.Id
}

// RecordCredential records an attestation for a person.
//
// With a type that requires a verifier the credential starts as pending and
// grants nothing until a second coordinator verifies it; otherwise it is
// verified by the recorder at once. Nobody records or verifies their own.
func RecordCredential(app App, ctx context.Context, actor *CredentialActor, org *Record, in CredentialInput) (*Record, error) {
	if err := requireCoordinator(actor, org); err != nil {
		return nil, err
	}

	user, err := FindOrgUser(app, in.User)
	if err != nil {
		return nil, err
	}

	if err := refuseSelf(actor, user.Id); err != nil {
		return nil, err
	}

	if !holderInSubtree(app, org, user.Id) {
		return nil, credErr(400, "The person is not a member of this organization or of one below it.")
	}

	typ, err := FindCredentialType(app, org.Id, strings.TrimSpace(in.Type))
	if err != nil {
		return nil, credErr(400, "Unknown credential type.")
	}

	if typ.GetString("status") != CredentialTypeActive {
		return nil, credErr(400, "That credential type is retired.")
	}

	now := time.Now()

	issued := in.IssuedAt
	if issued.IsZero() {
		issued = now
	}

	expires := in.Expires
	if expires.IsZero() {
		if m := typ.GetInt("validMonths"); m > 0 {
			expires = issued.AddDate(0, m, 0)
		} else {
			return nil, credErr(400, "An expiry date is required.")
		}
	}

	if !expires.After(issued) {
		return nil, credErr(400, "The expiry must be after the issue date.")
	}

	if !expires.After(now) {
		return nil, credErr(400, "The expiry must be in the future.")
	}

	if len(in.Issuer) > 200 {
		return nil, credErr(400, "The issuer is too long.")
	}

	if in.Document != nil && !CredentialDocumentsEnabled(org) {
		return nil, credErr(400, "Document uploads are off for this organization.")
	}

	if typ.GetBool("requiresDocument") && in.Document == nil {
		return nil, credErr(400, "This credential type requires a document.")
	}

	col, err := app.FindCachedCollectionByNameOrId(CollectionNameCredentials)
	if err != nil {
		return nil, err
	}

	rec := NewRecord(col)
	rec.Set("user", user.Id)
	rec.Set("org", org.Id)
	rec.Set("type", typ.GetString("key"))
	rec.Set("issuedAt", credentialDateStamp(issued))
	rec.Set("expires", credentialDateStamp(expires))
	rec.Set("issuer", strings.TrimSpace(in.Issuer))
	rec.Set("recordedBy", actorUserId(actor))
	rec.Set("revoked", false)

	if in.Document != nil {
		rec.Set("document", in.Document)
	}

	if typ.GetBool("requiresVerifier") {
		rec.Set("verified", false)
		rec.Set("status", CredentialStatusPending)
	} else {
		rec.Set("verified", true)
		rec.Set("verifiedBy", actorUserId(actor))
		rec.Set("status", CredentialStatusValid)
	}

	if err := saveOrgRecord(app, ctx, rec); err != nil {
		return nil, err
	}

	return rec, nil
}

// FindOrgCredential loads a credential issued by the org.
func FindOrgCredential(app App, org *Record, id string) (*Record, error) {
	c, err := app.FindRecordById(CollectionNameCredentials, id)
	if err != nil || c.GetString("org") != org.Id {
		return nil, credErr(404, "Credential not found.")
	}

	return c, nil
}

// UpdateCredential renews or corrects a credential. A change of the dates
// starts the reminders again; for a type that requires a verifier it also
// needs a new verification.
func UpdateCredential(app App, ctx context.Context, actor *CredentialActor, org, c *Record, p CredentialPatch) (*Record, error) {
	if c.GetString("org") != org.Id {
		return nil, credErr(404, "Credential not found.")
	}

	if err := requireCoordinator(actor, org); err != nil {
		return nil, err
	}

	if err := refuseSelf(actor, c.GetString("user")); err != nil {
		return nil, err
	}

	if c.GetBool("revoked") {
		return nil, credErr(409, "The credential is revoked. Record a new one.")
	}

	now := time.Now()
	dates := false

	if !p.IssuedAt.IsZero() {
		c.Set("issuedAt", credentialDateStamp(p.IssuedAt))
		dates = true
	}

	if !p.Expires.IsZero() {
		if !p.Expires.After(now) {
			return nil, credErr(400, "The expiry must be in the future.")
		}

		c.Set("expires", credentialDateStamp(p.Expires))
		dates = true
	}

	if dates {
		if issued := c.GetDateTime("issuedAt"); !issued.IsZero() && !c.GetDateTime("expires").Time().After(issued.Time()) {
			return nil, credErr(400, "The expiry must be after the issue date.")
		}
	}

	if p.Issuer != nil {
		if len(*p.Issuer) > 200 {
			return nil, credErr(400, "The issuer is too long.")
		}

		c.Set("issuer", strings.TrimSpace(*p.Issuer))
	}

	if p.Document != nil {
		if !CredentialDocumentsEnabled(org) {
			return nil, credErr(400, "Document uploads are off for this organization.")
		}

		c.Set("document", p.Document)
	}

	if dates {
		typ, _ := FindCredentialType(app, org.Id, c.GetString("type"))

		if typ != nil && typ.GetBool("requiresVerifier") {
			c.Set("verified", false)
			c.Set("verifiedBy", "")
			c.Set("status", CredentialStatusPending)
		} else {
			c.Set("verified", true)
			c.Set("verifiedBy", actorUserId(actor))
			c.Set("status", CredentialStatusValid)
		}

		c.Set("recordedBy", actorUserId(actor))
		c.Set("reminderStage", 0)
		c.Set("lapsedAt", "")
	}

	if err := saveOrgRecord(app, ctx, c); err != nil {
		return nil, err
	}

	return c, nil
}

// VerifyCredential is the second pair of eyes: a coordinator other than the
// recorder and the holder confirms a pending credential.
func VerifyCredential(app App, ctx context.Context, actor *CredentialActor, org, c *Record) (*Record, error) {
	if c.GetString("org") != org.Id {
		return nil, credErr(404, "Credential not found.")
	}

	if err := requireCoordinator(actor, org); err != nil {
		return nil, err
	}

	if err := refuseSelf(actor, c.GetString("user")); err != nil {
		return nil, err
	}

	if c.GetBool("revoked") {
		return nil, credErr(409, "The credential is revoked.")
	}

	if c.GetBool("verified") {
		return nil, credErr(409, "The credential is already verified.")
	}

	if id := actorUserId(actor); id != "" && id == c.GetString("recordedBy") {
		return nil, credErr(403, "A second coordinator must verify this credential.")
	}

	c.Set("verified", true)
	c.Set("verifiedBy", actorUserId(actor))

	if CredentialState(c, time.Now()) == CredentialStatusValid {
		c.Set("status", CredentialStatusValid)
	}

	if err := saveOrgRecord(app, ctx, c); err != nil {
		return nil, err
	}

	return c, nil
}

// RevokeCredential withdraws a credential at once.
func RevokeCredential(app App, ctx context.Context, actor *CredentialActor, org, c *Record, reason string) (*Record, error) {
	if c.GetString("org") != org.Id {
		return nil, credErr(404, "Credential not found.")
	}

	if err := requireCoordinator(actor, org); err != nil {
		return nil, err
	}

	if c.GetBool("revoked") {
		return nil, credErr(409, "The credential is already revoked.")
	}

	reason = strings.TrimSpace(reason)
	if len(reason) > 500 {
		return nil, credErr(400, "The reason is too long.")
	}

	c.Set("revoked", true)
	c.Set("revokedReason", reason)
	c.Set("revokedAt", credentialDateStamp(time.Now()))
	c.Set("status", CredentialStatusRevoked)

	if err := saveOrgRecord(app, ctx, c); err != nil {
		return nil, err
	}

	return c, nil
}

// ---- access status -----------------------------------------------------------

// CredentialStanding computes the standing of one person from the credentials
// of one type: the best credential wins.
func CredentialStanding(creds []*Record, now time.Time, expiringDays int) (standing string, best *Record) {
	rank := map[string]int{
		CredentialStandingCleared: 6, CredentialStandingExpiring: 5, CredentialStandingPending: 4,
		CredentialStandingLapsed: 3, CredentialStandingRevoked: 2, CredentialStandingMissing: 1,
	}

	standing = CredentialStandingMissing
	horizon := now.AddDate(0, 0, expiringDays)

	for _, c := range creds {
		var s string

		switch CredentialState(c, now) {
		case CredentialStatusValid:
			s = CredentialStandingCleared
			if !c.GetDateTime("expires").Time().After(horizon) {
				s = CredentialStandingExpiring
			}
		case CredentialStatusPending:
			s = CredentialStandingPending
		case CredentialStatusLapsed:
			s = CredentialStandingLapsed
		default:
			s = CredentialStandingRevoked
		}

		if best == nil || rank[s] > rank[standing] ||
			(rank[s] == rank[standing] && c.GetDateTime("expires").Time().After(best.GetDateTime("expires").Time())) {
			standing, best = s, c
		}
	}

	return standing, best
}

// AccessStatusItem is one person's standing for one type.
type AccessStatusItem struct {
	User       string     `json:"user"`
	Name       string     `json:"name,omitempty"`
	Email      string     `json:"email,omitempty"`
	Standing   string     `json:"standing"`
	Credential string     `json:"credential,omitempty"`
	IssuedBy   string     `json:"issuedByOrg,omitempty"`
	Expires    *time.Time `json:"expires,omitempty"`
	DaysLeft   *int       `json:"daysLeft,omitempty"`
}

// AccessStatusType is the result for one credential type.
type AccessStatusType struct {
	Key        string             `json:"key"`
	Name       string             `json:"name"`
	Counts     map[string]int     `json:"counts"`
	TotalItems int                `json:"totalItems"`
	Items      []AccessStatusItem `json:"items"`
}

// AccessStatusOptions narrows CredentialAccessStatus.
type AccessStatusOptions struct {
	// Type limits the result to one type key (empty = every active type).
	Type string
	// Standing limits the items (not the counts) to one standing.
	Standing string
	// ExpiringDays is the "expiring" window (0 = DefaultCredentialExpiringDays).
	ExpiringDays int
	// Limit caps the items per type (0 = 200).
	Limit int
	Now   time.Time
}

// CredentialAccessStatus tells, for every active member of the org, whether
// they are cleared, expiring, lapsed, pending, revoked or without a credential
// of each type. Credentials issued by the org or by any ancestor count, the
// same as the ancestor rule form.
func CredentialAccessStatus(app App, org *Record, opts AccessStatusOptions) ([]AccessStatusType, error) {
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}

	if opts.ExpiringDays <= 0 {
		opts.ExpiringDays = DefaultCredentialExpiringDays
	}

	if opts.Limit <= 0 {
		opts.Limit = 200
	}

	typesList, err := CredentialTypesFor(app, org.Id)
	if err != nil {
		return nil, err
	}

	members, err := app.FindRecordsByFilter(CollectionNameOrgMembers, "org={:org} && status='active'", "created", 0, 0, dbx.Params{"org": org.Id})
	if err != nil {
		return nil, err
	}

	userCol, _ := OrgUserCollection(app)
	people := map[string]*Record{}

	if userCol != nil {
		ids := make([]string, 0, len(members))
		for _, m := range members {
			ids = append(ids, m.GetString("user"))
		}

		for chunk := range slices.Chunk(ids, 500) {
			recs, err := app.FindRecordsByIds(userCol, chunk)
			if err != nil {
				return nil, err
			}

			for _, r := range recs {
				people[r.Id] = r
			}
		}
	}

	chain := []any{}
	for _, id := range orgChain(app, org.Id) {
		chain = append(chain, id)
	}

	out := []AccessStatusType{}

	for _, typ := range typesList {
		if typ.GetString("status") != CredentialTypeActive {
			continue
		}

		key := typ.GetString("key")
		if opts.Type != "" && opts.Type != key {
			continue
		}

		creds, err := app.FindAllRecords(CollectionNameCredentials, dbx.HashExp{"type": key, "org": chain})
		if err != nil {
			return nil, err
		}

		byUser := map[string][]*Record{}
		for _, c := range creds {
			byUser[c.GetString("user")] = append(byUser[c.GetString("user")], c)
		}

		res := AccessStatusType{
			Key: key, Name: typ.GetString("name"), Items: []AccessStatusItem{},
			Counts: map[string]int{
				CredentialStandingCleared: 0, CredentialStandingExpiring: 0, CredentialStandingPending: 0,
				CredentialStandingLapsed: 0, CredentialStandingRevoked: 0, CredentialStandingMissing: 0,
			},
		}

		for _, m := range members {
			uid := m.GetString("user")
			standing, best := CredentialStanding(byUser[uid], opts.Now, opts.ExpiringDays)
			res.Counts[standing]++

			if opts.Standing != "" && opts.Standing != standing {
				continue
			}

			res.TotalItems++

			if len(res.Items) >= opts.Limit {
				continue
			}

			item := AccessStatusItem{User: uid, Standing: standing}

			if u := people[uid]; u != nil {
				if userCol.Fields.GetByName("name") != nil {
					item.Name = u.GetString("name")
				}

				item.Email = u.Email()
			}

			if best != nil {
				exp := best.GetDateTime("expires").Time()
				days := int(exp.Sub(opts.Now).Hours() / 24)
				item.Credential, item.IssuedBy, item.Expires, item.DaysLeft = best.Id, best.GetString("org"), &exp, &days
			}

			res.Items = append(res.Items, item)
		}

		out = append(out, res)
	}

	return out, nil
}

// ---- lifecycle job -----------------------------------------------------------

// CredentialLifecycleReport counts what one run of the job did.
type CredentialLifecycleReport struct {
	Lapsed   int `json:"lapsed"`
	Expiring int `json:"expiring"`
}

// RunCredentialLifecycle marks newly lapsed credentials and emits the
// reminders, as of now.
//
// A lapse is a passage of time and not a record change, so the audit log
// would never see it: each newly lapsed credential gets one audit entry
// (credential_lapsed) and each reminder threshold crossed one entry
// (credential_expiring), which webhooks deliver as credential.lapsed and
// credential.expiring. The rules read `expires` and never wait for this job.
//
// It is idempotent. Each step is a compare-and-swap UPDATE and the entry is
// only written by the run that changed the row, so a second run (or a second
// replica) writes nothing.
func RunCredentialLifecycle(app App, ctx context.Context, now time.Time) (*CredentialLifecycleReport, error) {
	report := &CredentialLifecycleReport{}

	if !CredentialsAvailable(app) {
		return report, nil
	}

	nowStr := credentialDateStamp(now).String()

	// 1. lapse
	due, err := app.FindRecordsByFilter(CollectionNameCredentials,
		"revoked=false && status!={:lapsed} && expires<={:now}", "expires", 0, 0,
		dbx.Params{"lapsed": CredentialStatusLapsed, "now": nowStr})
	if err != nil {
		return report, err
	}

	for _, c := range due {
		changed := false

		err := app.RunInTransaction(func(txApp App) error {
			res, err := txApp.NonconcurrentDB().Update(CollectionNameCredentials,
				dbx.Params{"status": CredentialStatusLapsed, "lapsedAt": nowStr, "updated": nowStr},
				dbx.NewExp("[[id]]={:id} AND [[revoked]]={:rev} AND [[status]]!={:lapsed}",
					dbx.Params{"id": c.Id, "rev": false, "lapsed": CredentialStatusLapsed})).Execute()
			if err != nil {
				return err
			}

			n, _ := res.RowsAffected()
			if n == 0 {
				return nil
			}

			changed = true

			return writeCredentialAudit(txApp, ctx, AuditActionCredentialLapsed, c, map[string]any{"expires": c.GetDateTime("expires").String()})
		})
		if err != nil {
			return report, err
		}

		if changed {
			report.Lapsed++
		}
	}

	// 2. reminders
	orgDays := map[string][]int{}
	maxDays := 0

	orgs, _ := app.FindAllRecords(CollectionNameOrgs)
	for _, o := range orgs {
		d := CredentialReminderDays(o)
		orgDays[o.Id] = d

		if len(d) > 0 {
			maxDays = max(maxDays, d[0])
		}
	}

	if maxDays == 0 {
		return report, nil
	}

	soon, err := app.FindRecordsByFilter(CollectionNameCredentials,
		"revoked=false && verified=true && status={:valid} && expires>{:now} && expires<={:horizon}", "expires", 0, 0,
		dbx.Params{"valid": CredentialStatusValid, "now": nowStr, "horizon": credentialDateStamp(now.AddDate(0, 0, maxDays+1)).String()})
	if err != nil {
		return report, err
	}

	for _, c := range soon {
		exp := c.GetDateTime("expires").Time()
		daysLeft := int((exp.Sub(now) + 24*time.Hour - 1) / (24 * time.Hour)) // rounded up

		// the tightest threshold already crossed
		threshold := 0

		for _, t := range orgDays[c.GetString("org")] {
			if daysLeft <= t && (threshold == 0 || t < threshold) {
				threshold = t
			}
		}

		if threshold == 0 {
			continue
		}

		changed := false

		err := app.RunInTransaction(func(txApp App) error {
			res, err := txApp.NonconcurrentDB().Update(CollectionNameCredentials,
				dbx.Params{"reminderStage": threshold, "notifiedAt": nowStr, "updated": nowStr},
				dbx.NewExp("[[id]]={:id} AND [[status]]={:valid} AND (COALESCE([[reminderStage]], 0)=0 OR [[reminderStage]]>{:t})",
					dbx.Params{"id": c.Id, "valid": CredentialStatusValid, "t": threshold})).Execute()
			if err != nil {
				return err
			}

			n, _ := res.RowsAffected()
			if n == 0 {
				return nil
			}

			changed = true

			return writeCredentialAudit(txApp, ctx, AuditActionCredentialExpiring, c, map[string]any{
				"expires": c.GetDateTime("expires").String(), "daysLeft": daysLeft, "threshold": threshold,
			})
		})
		if err != nil {
			return report, err
		}

		if changed {
			report.Expiring++
		}
	}

	return report, nil
}

func writeCredentialAudit(app App, ctx context.Context, action string, c *Record, extra map[string]any) error {
	if !auditAvailable(app) {
		return nil
	}

	after := map[string]any{
		"user": c.GetString("user"), "org": c.GetString("org"), "type": c.GetString("type"),
		"status": CredentialStatusLapsed,
	}
	for k, v := range extra {
		after[k] = v
	}

	if action == AuditActionCredentialExpiring {
		after["status"] = CredentialStatusValid
	}

	meta := map[string]any{"orgId": c.GetString("org"), "type": c.GetString("type"), "user": c.GetString("user")}

	entry := &AuditEntry{
		Action:         action,
		CollectionId:   c.Collection().Id,
		CollectionName: CollectionNameCredentials,
		RecordId:       c.Id,
		ChangedFields:  []string{"status"},
		After:          after,
		Meta:           meta,
	}
	applyAuditContext(ctx, entry)

	return WriteAuditEntry(app, ctx, entry, c)
}

// ---- hooks ---------------------------------------------------------------

func (app *BaseApp) registerCredentialHooks() {
	app.OnRecordValidate(CollectionNameCredentials).Bind(&hook.Handler[*RecordEvent]{
		Id: "__pbCredentialValidate__",
		Func: func(e *RecordEvent) error {
			if !ValidCredentialKey(e.Record.GetString("type")) {
				return credErr(400, "Invalid credential type key.")
			}

			if v := e.Record.GetString("verifiedBy"); v != "" && v == e.Record.GetString("user") {
				return credErr(400, "A credential cannot be verified by its holder.")
			}

			return e.Next()
		},
	})

	app.OnRecordValidate(CollectionNameCredentialTypes).Bind(&hook.Handler[*RecordEvent]{
		Id: "__pbCredentialTypeValidate__",
		Func: func(e *RecordEvent) error {
			if !ValidCredentialKey(e.Record.GetString("key")) {
				return credErr(400, "Invalid credential type key.")
			}

			return e.Next()
		},
	})

	app.Cron().Add(credentialCronId, credentialCronExpr, func() {
		ctx := WithAuditContext(context.Background(), AuditActor{Type: AuditActorTypeSystem}, "")

		if _, err := RunCredentialLifecycle(app, ctx, time.Now()); err != nil && !errors.Is(err, context.Canceled) {
			app.Logger().Warn("Failed to run the credential lifecycle job", "error", fmt.Sprint(err))
		}
	})
}
