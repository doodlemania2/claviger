package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	validation "ganigeorgiev.com/pocketbase/ozzo-validation/v4"
	"github.com/doodlemania2/claviger/tools/hook"
)

// Fork-local (Claviger #80): households and guardian-managed minors.
//
// Two hidden system collections hold the model:
//
//   - _households: a group of people in one home org, with shared contact
//     data (address, homePhone; encrypted when a key is configured).
//   - _householdMembers: one row per PERSON. The row is the person; a login
//     (`user`) is optional, and a minor never has one.
//
// Both have minimal list and view rules and null write rules. Writes go
// through the functions in this file (and the /api/households routes built
// on them) and through the model hook below, which enforces the guards:
//
//   - a minor cannot be a guardian and cannot have a login;
//   - a household with an active minor keeps at least one active adult
//     guardian (a change that would break it is refused);
//   - isMinor follows birthDate and the org's graduation age.
//
// The daily job (RunHouseholdGraduation) graduates minors at the org's age.

const (
	CollectionNameHouseholds       = "_households"
	CollectionNameHouseholdMembers = "_householdMembers"

	// HouseholdRegistrarTag is the org tag (an _orgRoles row of the
	// household's org, attached through _orgMembers.tags) that lets a member
	// manage the households of the org, besides owners and admins.
	HouseholdRegistrarTag = "registrar"

	HouseholdStatusActive   = "active"
	HouseholdStatusArchived = "archived"

	HouseholdRoleHead   = "head"
	HouseholdRoleSpouse = "spouse"
	HouseholdRoleAdult  = "adult"
	HouseholdRoleChild  = "child"
	HouseholdRoleOther  = "other"

	HouseholdMemberActive   = "active"
	HouseholdMemberLeft     = "left"
	HouseholdMemberDeceased = "deceased"

	HouseholdSourceManual = "manual"
	HouseholdSourceImport = "import"
	HouseholdSourceSCIM   = "scim"

	// the org setting (in _orgs.settings) that holds the graduation age
	HouseholdGraduationAgeSetting = "graduationAge"
	DefaultHouseholdGraduationAge = 18
	MinHouseholdGraduationAge     = 14
	MaxHouseholdGraduationAge     = 21

	// AuditActionHouseholdGraduated is written once per graduation by the
	// daily job. A graduation is a passage of time, not a record write the
	// audit hooks would see. It is deliberately not a webhook event: the
	// household collections never produce webhooks (children's data).
	AuditActionHouseholdGraduated = "household_graduated"

	householdCronId   = "__pbHouseholdGraduation__"
	householdCronExpr = "41 6 * * *" // daily at 06:41 (off the other jobs)
)

// HouseholdRoles and the other lists feed the migration and the validators.
var (
	HouseholdRoles          = []string{HouseholdRoleHead, HouseholdRoleSpouse, HouseholdRoleAdult, HouseholdRoleChild, HouseholdRoleOther}
	HouseholdMemberStatuses = []string{HouseholdMemberActive, HouseholdMemberLeft, HouseholdMemberDeceased}
	HouseholdSources        = []string{HouseholdSourceManual, HouseholdSourceImport, HouseholdSourceSCIM}
	HouseholdStatuses       = []string{HouseholdStatusActive, HouseholdStatusArchived}

	// householdNow is the clock of the model hook (replaced in tests).
	householdNow = time.Now
)

func householdErr(status int, msg string) error { return orgErr(status, msg) }

// HouseholdsAvailable reports whether the households model exists.
func HouseholdsAvailable(app App) bool {
	_, err := app.FindCachedCollectionByNameOrId(CollectionNameHouseholdMembers)

	return err == nil
}

// ---- age and graduation -------------------------------------------------------

// HouseholdGraduationAge returns the age at which a minor of the org becomes
// independent (setting graduationAge in _orgs.settings, an integer from 14 to
// 21; anything else means the default, 18).
func HouseholdGraduationAge(org *Record) int {
	if org == nil {
		return DefaultHouseholdGraduationAge
	}

	if v, ok := credentialOrgSettings(org)[HouseholdGraduationAgeSetting].(float64); ok &&
		v == float64(int(v)) && int(v) >= MinHouseholdGraduationAge && int(v) <= MaxHouseholdGraduationAge {
		return int(v)
	}

	return DefaultHouseholdGraduationAge
}

// ParseBirthDate reads a birth date: a plain date (2010-05-17) or a date-time
// (only the date part counts).
func ParseBirthDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if len(s) >= 10 {
		if t, err := time.Parse("2006-01-02", s[:10]); err == nil {
			return t, nil
		}
	}

	return time.Time{}, errors.New("the birth date must be a date such as 2010-05-17")
}

// IsMinorAt reports whether a person born on birth is under the graduation
// age on the day now (the birthday itself counts as the new age). It is a
// plain function of its arguments, so it is tested with a fixed clock.
func IsMinorAt(birth, now time.Time, graduationAge int) bool {
	b := time.Date(birth.Year(), birth.Month(), birth.Day(), 0, 0, 0, 0, time.UTC)
	n := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)

	return n.Before(b.AddDate(graduationAge, 0, 0))
}

// ---- who may act --------------------------------------------------------------

// HouseholdActor is who performs a household operation.
type HouseholdActor struct {
	*OrgActor
	// Staff: a superuser, an owner or admin of the household's org, or a
	// member of the org tagged "registrar".
	Staff bool
	// Member is the caller's active row in this household (nil if none).
	Member *Record
}

// NewHouseholdActor resolves the standing of auth for the household (which
// may be nil, to resolve the org standing only).
func NewHouseholdActor(app App, org *Record, household *Record, auth *Record) *HouseholdActor {
	a := &HouseholdActor{OrgActor: NewOrgActor(app, org, auth)}
	if auth == nil {
		return a
	}

	switch {
	case a.Superuser:
		a.Staff = true
	case a.Role == OrgRoleOwner || a.Role == OrgRoleAdmin:
		a.Staff = true
	case a.Role == OrgRoleMember:
		if m := ActiveOrgMember(app, org, auth.Id); m != nil {
			a.Staff = slices.Contains(OrgTagNames(app, m), HouseholdRegistrarTag)
		}
	}

	if household != nil && !a.Superuser {
		m, err := app.FindFirstRecordByFilter(CollectionNameHouseholdMembers,
			"household={:h} && user={:u} && status={:s}",
			dbx.Params{"h": household.Id, "u": auth.Id, "s": HouseholdMemberActive})
		if err == nil {
			a.Member = m
		}
	}

	return a
}

// Guardian reports whether the caller is an active adult guardian of the
// household.
func (a *HouseholdActor) Guardian() bool {
	return a.Member != nil && a.Member.GetBool("guardian") && !a.Member.GetBool("isMinor")
}

// CanView reports whether the caller may read the household.
func (a *HouseholdActor) CanView() bool { return a.Staff || a.Member != nil }

// FindHousehold loads a household and its org.
func FindHousehold(app App, id string) (*Record, *Record, error) {
	h, err := app.FindRecordById(CollectionNameHouseholds, id)
	if err != nil {
		return nil, nil, householdErr(404, "Household not found.")
	}

	org, err := app.FindRecordById(CollectionNameOrgs, h.GetString("org"))
	if err != nil {
		return nil, nil, householdErr(404, "Household not found.")
	}

	return h, org, nil
}

// FindHouseholdMember loads a member row of the household.
func FindHouseholdMember(app App, h *Record, id string) (*Record, error) {
	m, err := app.FindRecordById(CollectionNameHouseholdMembers, id)
	if err != nil || m.GetString("household") != h.Id {
		return nil, householdErr(404, "Household member not found.")
	}

	return m, nil
}

// HouseholdMembers lists the rows of the household (every status), oldest first.
func HouseholdMembers(app App, h *Record) ([]*Record, error) {
	return app.FindRecordsByFilter(CollectionNameHouseholdMembers, "household={:h}", "created", 0, 0, dbx.Params{"h": h.Id})
}

// ---- sealed fields ------------------------------------------------------------

var (
	householdSensitiveMemberFields = []string{"birthDate", "allergies", "medical"}
	householdSensitiveFields       = []string{"address", "homePhone"}
)

// requireSealed refuses a write of the named sensitive fields while the field
// is not encrypted (no key-encryption key was configured when the collection
// was created and the fields have not been upgraded). Children's data must
// not be written in the clear by accident.
func requireSealed(col *Collection, names ...string) error {
	for _, n := range names {
		f := col.Fields.GetByName(n)
		if f == nil {
			continue
		}

		if t := f.Type(); t != FieldTypeEncryptedText && t != FieldTypeEncryptedJSON {
			return householdErr(409, "The field "+n+" is not encrypted: configure a key-encryption key (CLAVIGER_KEK) and restart before storing it.")
		}
	}

	return nil
}

func hasKeys(m map[string]any, names ...string) []string {
	var out []string

	for _, n := range names {
		if _, ok := m[n]; ok {
			out = append(out, n)
		}
	}

	return out
}

// ---- households ---------------------------------------------------------------

// HouseholdInput creates a household. First, when set, is the first member.
type HouseholdInput struct {
	Org       string
	Name      string
	Address   any
	HomePhone string
	First     map[string]any
}

// CreateHousehold creates a household in the org with its first member in one
// transaction. Org staff may create an empty household or name any first
// member. Any other active member of the org creates a household for
// themselves: they become its head and first guardian.
func CreateHousehold(app App, ctx context.Context, auth *Record, org *Record, in HouseholdInput) (*Record, error) {
	actor := NewHouseholdActor(app, org, nil, auth)
	if !actor.IsMember() {
		return nil, householdErr(404, "Organization not found.")
	}

	if err := requireLiveOrg(org, actor.OrgActor); err != nil {
		return nil, err
	}

	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 200 {
		return nil, householdErr(400, "A name of up to 200 characters is required.")
	}

	hCol, err := app.FindCachedCollectionByNameOrId(CollectionNameHouseholds)
	if err != nil {
		return nil, err
	}

	if in.Address != nil {
		if err := requireSealed(hCol, "address"); err != nil {
			return nil, err
		}
	}

	if in.HomePhone != "" {
		if err := requireSealed(hCol, "homePhone"); err != nil {
			return nil, err
		}
	}

	first := in.First
	if !actor.Staff {
		// self service: the caller is the first member, whatever the body says
		name := ""
		if first != nil {
			name, _ = first["name"].(string)
		}

		if strings.TrimSpace(name) == "" {
			name = auth.GetString("name")
		}

		if strings.TrimSpace(name) == "" {
			name = auth.Email()
		}

		first = map[string]any{"name": name, "role": HouseholdRoleHead, "guardian": true, "user": auth.Id}
	}

	var out *Record

	err = app.RunInTransaction(func(txApp App) error {
		rec := NewRecord(hCol)
		rec.Set("org", org.Id)
		rec.Set("name", in.Name)
		rec.Set("status", HouseholdStatusActive)

		if in.Address != nil {
			rec.Set("address", in.Address)
		}

		if in.HomePhone != "" {
			rec.Set("homePhone", in.HomePhone)
		}

		if err := saveOrgRecord(txApp, ctx, rec); err != nil {
			return err
		}

		out = rec

		if first != nil {
			f := &HouseholdActor{OrgActor: &OrgActor{System: true}, Staff: true}

			if _, err := addHouseholdMember(txApp, ctx, f, rec, org, first); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// UpdateHousehold changes name, address and homePhone (an active guardian or
// staff) and status (staff only).
func UpdateHousehold(app App, ctx context.Context, actor *HouseholdActor, org, h *Record, patch map[string]any) (*Record, error) {
	if !actor.Staff && !actor.Guardian() {
		return nil, householdErr(403, "Only a guardian of the household or the staff of the organization can do this.")
	}

	if err := requireLiveOrg(org, actor.OrgActor); err != nil {
		return nil, err
	}

	col, err := app.FindCachedCollectionByNameOrId(CollectionNameHouseholds)
	if err != nil {
		return nil, err
	}

	if err := requireSealed(col, hasKeys(patch, householdSensitiveFields...)...); err != nil {
		return nil, err
	}

	if v, ok := patch["name"]; ok {
		s, _ := v.(string)

		s = strings.TrimSpace(s)
		if s == "" || len(s) > 200 {
			return nil, householdErr(400, "A name of up to 200 characters is required.")
		}

		h.Set("name", s)
	}

	if v, ok := patch["address"]; ok {
		h.Set("address", v)
	}

	if v, ok := patch["homePhone"]; ok {
		s, _ := v.(string)
		h.Set("homePhone", s)
	}

	if v, ok := patch["status"]; ok {
		if !actor.Staff {
			return nil, householdErr(403, "Only the staff of the organization can archive a household.")
		}

		s, _ := v.(string)
		if !slices.Contains(HouseholdStatuses, s) {
			return nil, householdErr(400, "Invalid status.")
		}

		h.Set("status", s)
	}

	if err := saveOrgRecord(app, ctx, h); err != nil {
		return nil, err
	}

	return h, nil
}

// MyHouseholds lists the households in which the user is an active member.
func MyHouseholds(app App, userId string) ([]*Record, error) {
	rows, err := app.FindRecordsByFilter(CollectionNameHouseholdMembers,
		"user={:u} && status={:s} && household.status={:hs} && household.org.archived=false", "created", 0, 0,
		dbx.Params{"u": userId, "s": HouseholdMemberActive, "hs": HouseholdStatusActive})
	if err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(rows))

	for _, r := range rows {
		if id := r.GetString("household"); !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}

	if len(ids) == 0 {
		return []*Record{}, nil
	}

	return app.FindRecordsByIds(CollectionNameHouseholds, ids)
}

// OrgHouseholds lists the households of an org (staff view), by name.
func OrgHouseholds(app App, orgId string, limit, offset int) ([]*Record, error) {
	return app.FindRecordsByFilter(CollectionNameHouseholds, "org={:o}", "name", limit, offset, dbx.Params{"o": orgId})
}

// ---- members ------------------------------------------------------------------

// memberFields are the keys a body may carry.
var householdMemberKeys = []string{"name", "role", "guardian", "isMinor", "user", "birthDate", "allergies", "medical", "status"}

func applyMemberFields(m *Record, fields map[string]any) error {
	for k, v := range fields {
		switch k {
		case "name":
			s, _ := v.(string)

			s = strings.TrimSpace(s)
			if s == "" || len(s) > 200 {
				return householdErr(400, "A name of up to 200 characters is required.")
			}

			m.Set("name", s)
		case "role":
			s, _ := v.(string)
			if !slices.Contains(HouseholdRoles, s) {
				return householdErr(400, "Invalid role.")
			}

			m.Set("role", s)
		case "status":
			s, _ := v.(string)
			if !slices.Contains(HouseholdMemberStatuses, s) {
				return householdErr(400, "Invalid status.")
			}

			m.Set("status", s)
		case "guardian", "isMinor":
			b, ok := v.(bool)
			if !ok {
				return householdErr(400, k+" must be true or false.")
			}

			m.Set(k, b)
		case "user":
			s, _ := v.(string)
			m.Set("user", strings.TrimSpace(s))
		case "birthDate":
			s, _ := v.(string)

			s = strings.TrimSpace(s)
			if s != "" {
				t, err := ParseBirthDate(s)
				if err != nil {
					return householdErr(400, err.Error())
				}

				s = t.Format("2006-01-02")
			}

			m.Set("birthDate", s)
		case "allergies", "medical":
			m.Set(k, v)
		}
	}

	return nil
}

func filterMemberKeys(in map[string]any) map[string]any {
	out := map[string]any{}

	for _, k := range householdMemberKeys {
		if v, ok := in[k]; ok {
			out[k] = v
		}
	}

	return out
}

// AddHouseholdMember adds a person to the household. A guardian of the
// household or the org staff may do it. Only staff may attach a login (`user`)
// to someone else; a guardian may only attach their own. A minor never gets a
// login (the model hook refuses it).
func AddHouseholdMember(app App, ctx context.Context, actor *HouseholdActor, org, h *Record, fields map[string]any) (*Record, error) {
	return addHouseholdMember(app, ctx, actor, h, org, fields)
}

func addHouseholdMember(app App, ctx context.Context, actor *HouseholdActor, h, org *Record, fields map[string]any) (*Record, error) {
	if !actor.Staff && !actor.Guardian() {
		return nil, householdErr(403, "Only a guardian of the household or the staff of the organization can add people.")
	}

	if err := requireLiveOrg(org, actor.OrgActor); err != nil {
		return nil, err
	}

	if h.GetString("status") != HouseholdStatusActive {
		return nil, householdErr(409, "The household is archived.")
	}

	fields = filterMemberKeys(fields)

	col, err := app.FindCachedCollectionByNameOrId(CollectionNameHouseholdMembers)
	if err != nil {
		return nil, err
	}

	if err := requireSealed(col, hasKeys(fields, householdSensitiveMemberFields...)...); err != nil {
		return nil, err
	}

	if u, _ := fields["user"].(string); u != "" && !actor.Staff && (actor.Record == nil || u != actor.Record.Id) {
		return nil, householdErr(403, "Only the staff of the organization can give a person a login.")
	}

	if u, _ := fields["user"].(string); u != "" {
		if _, err := FindOrgUser(app, u); err != nil {
			return nil, householdErr(400, "That user does not exist.")
		}
	}

	m := NewRecord(col)
	m.Set("household", h.Id)
	m.Set("role", HouseholdRoleAdult)
	m.Set("status", HouseholdMemberActive)
	m.Set("source", HouseholdSourceManual)

	if err := applyMemberFields(m, fields); err != nil {
		return nil, err
	}

	if m.GetString("name") == "" {
		return nil, householdErr(400, "A name is required.")
	}

	if err := saveOrgRecord(app, ctx, m); err != nil {
		return nil, err
	}

	return m, nil
}

// UpdateHouseholdMember changes a person of the household.
//
//   - Staff change everything.
//   - A guardian changes any person, except: attaching a login (staff only),
//     and demoting or removing another guardian (the person themselves or the
//     staff).
//   - A person with a login changes their own name, birth date, allergies and
//     medical notes, and may step down as guardian or leave.
func UpdateHouseholdMember(app App, ctx context.Context, actor *HouseholdActor, org, h, m *Record, fields map[string]any) (*Record, error) {
	if m.GetString("household") != h.Id {
		return nil, householdErr(404, "Household member not found.")
	}

	if err := requireLiveOrg(org, actor.OrgActor); err != nil {
		return nil, err
	}

	fields = filterMemberKeys(fields)
	self := actor.Member != nil && actor.Member.Id == m.Id

	switch {
	case actor.Staff:
	case actor.Guardian():
		if _, ok := fields["user"]; ok {
			return nil, householdErr(403, "Only the staff of the organization can change a login.")
		}

		if !self && m.GetBool("guardian") && !m.GetBool("isMinor") {
			if g, ok := fields["guardian"].(bool); (ok && !g) || fields["status"] != nil {
				return nil, householdErr(403, "Only the person themselves or the staff of the organization can change another guardian.")
			}
		}
	case self:
		for k, v := range fields {
			switch k {
			case "name", "birthDate", "allergies", "medical":
			case "guardian":
				if b, _ := v.(bool); b {
					return nil, householdErr(403, "You cannot make yourself a guardian.")
				}
			case "status":
				if s, _ := v.(string); s != HouseholdMemberLeft {
					return nil, householdErr(403, "You can only leave the household.")
				}
			default:
				return nil, householdErr(403, "You cannot change "+k+".")
			}
		}
	default:
		return nil, householdErr(403, "You cannot change this person.")
	}

	mcol, err := app.FindCachedCollectionByNameOrId(CollectionNameHouseholdMembers)
	if err != nil {
		return nil, err
	}

	if err := requireSealed(mcol, hasKeys(fields, householdSensitiveMemberFields...)...); err != nil {
		return nil, err
	}

	if u, _ := fields["user"].(string); u != "" {
		if _, err := FindOrgUser(app, u); err != nil {
			return nil, householdErr(400, "That user does not exist.")
		}
	}

	if err := applyMemberFields(m, fields); err != nil {
		return nil, err
	}

	if err := saveOrgRecord(app, ctx, m); err != nil {
		return nil, err
	}

	return m, nil
}

// SetHouseholdGuardian sets or clears the guardian flag. Who may do it is
// decided by UpdateHouseholdMember; the model hook keeps at least one adult
// guardian while there is an active minor.
func SetHouseholdGuardian(app App, ctx context.Context, actor *HouseholdActor, org, h, m *Record, guardian bool) (*Record, error) {
	return UpdateHouseholdMember(app, ctx, actor, org, h, m, map[string]any{"guardian": guardian})
}

// RemoveHouseholdMember deletes a person's row. Staff may remove anyone; a
// guardian may remove anyone who is not a guardian; a person may remove
// themselves. A removal that would leave an active minor without an adult
// guardian is refused.
func RemoveHouseholdMember(app App, ctx context.Context, actor *HouseholdActor, org, h, m *Record) error {
	if m.GetString("household") != h.Id {
		return householdErr(404, "Household member not found.")
	}

	if err := requireLiveOrg(org, actor.OrgActor); err != nil {
		return err
	}

	self := actor.Member != nil && actor.Member.Id == m.Id
	isGuardian := m.GetBool("guardian") && !m.GetBool("isMinor")

	switch {
	case actor.Staff, self:
	case actor.Guardian() && !isGuardian:
	case actor.Guardian():
		return householdErr(403, "Only the person themselves or the staff of the organization can remove a guardian.")
	default:
		return householdErr(403, "You cannot remove this person.")
	}

	return app.RunInTransaction(func(txApp App) error {
		members, err := HouseholdMembers(txApp, h)
		if err != nil {
			return err
		}

		rest := make([]*Record, 0, len(members))

		for _, o := range members {
			if o.Id != m.Id {
				rest = append(rest, o)
			}
		}

		if householdNeedsGuardian(rest) && !householdNeedsGuardian(members) {
			return householdErr(409, "A household with a minor needs at least one active adult guardian.")
		}

		return deleteOrgRecord(txApp, ctx, m)
	})
}

// ---- the guard ----------------------------------------------------------------

// householdNeedsGuardian reports whether the rows break the guardian rule: an
// active minor and no active adult guardian.
func householdNeedsGuardian(members []*Record) bool {
	minor, guardian := false, false

	for _, m := range members {
		if m.GetString("status") != HouseholdMemberActive {
			continue
		}

		if m.GetBool("isMinor") {
			minor = true
		} else if m.GetBool("guardian") {
			guardian = true
		}
	}

	return minor && !guardian
}

// validateHouseholdMember is the model hook (OnRecordValidate) of
// _householdMembers. It derives isMinor from birthDate and enforces the guards.
func validateHouseholdMember(e *RecordEvent) error {
	m := e.Record
	app := e.App

	h, err := app.FindRecordById(CollectionNameHouseholds, m.GetString("household"))
	if err != nil {
		return e.Next() // the relation validator reports the missing household
	}

	if bd := m.GetString("birthDate"); bd != "" {
		t, err := ParseBirthDate(bd)
		if err != nil {
			return validation.Errors{"birthDate": validation.NewError("validation_invalid_birth_date", err.Error())}
		}

		org, _ := app.FindRecordById(CollectionNameOrgs, h.GetString("org"))
		m.Set("isMinor", IsMinorAt(t, householdNow(), HouseholdGraduationAge(org)))
	}

	if m.GetBool("isMinor") {
		if m.GetString("user") != "" {
			return validation.Errors{"user": validation.NewError("validation_minor_login", "A minor cannot have a login.")}
		}

		if m.GetBool("guardian") {
			return validation.Errors{"guardian": validation.NewError("validation_minor_guardian", "A minor cannot be a guardian.")}
		}
	}

	members, err := HouseholdMembers(app, h)
	if err != nil {
		return err
	}

	before := members
	after := make([]*Record, 0, len(members)+1)

	for _, o := range members {
		if o.Id != m.Id {
			after = append(after, o)
		}
	}

	after = append(after, m)

	if !m.IsNew() && householdNeedsGuardian(after) && !householdNeedsGuardian(before) {
		return householdErr(409, "A household with a minor needs at least one active adult guardian.")
	}

	if m.IsNew() && householdNeedsGuardian(after) {
		return householdErr(409, "A household with a minor needs at least one active adult guardian.")
	}

	return e.Next()
}

// validateOrgGraduationAge refuses a graduation age outside the bounds.
func validateOrgGraduationAge(e *RecordEvent) error {
	if v, ok := credentialOrgSettings(e.Record)[HouseholdGraduationAgeSetting]; ok {
		f, isNum := v.(float64)
		if !isNum || f != float64(int(f)) || int(f) < MinHouseholdGraduationAge || int(f) > MaxHouseholdGraduationAge {
			return validation.Errors{"settings": validation.NewError("validation_graduation_age",
				fmt.Sprintf("graduationAge must be a whole number from %d to %d.", MinHouseholdGraduationAge, MaxHouseholdGraduationAge))}
		}
	}

	return e.Next()
}

// ---- graduation job -----------------------------------------------------------

// HouseholdGraduationReport is what a run of the job did.
type HouseholdGraduationReport struct {
	Graduated int `json:"graduated"`
	// Skipped counts minors without a usable birth date (they stay as set by hand).
	Skipped int `json:"skipped"`
}

// RunHouseholdGraduation sets isMinor to false for every active minor whose
// birth date and org graduation age say they are an adult on the day now. It
// never makes an adult a minor again (that could break the guardian rule),
// and a minor without a birth date is left alone.
//
// It is idempotent: the update is a compare-and-swap and the audit entry
// (household_graduated, one per person, no name and no birth date) is only
// written by the run that changed the row.
func RunHouseholdGraduation(app App, ctx context.Context, now time.Time) (*HouseholdGraduationReport, error) {
	report := &HouseholdGraduationReport{}

	if !HouseholdsAvailable(app) {
		return report, nil
	}

	minors, err := app.FindRecordsByFilter(CollectionNameHouseholdMembers,
		"isMinor=true && status={:s}", "created", 0, 0, dbx.Params{"s": HouseholdMemberActive})
	if err != nil {
		return report, err
	}

	ages := map[string]int{}      // household id -> graduation age
	orgIds := map[string]string{} // household id -> org id

	stamp := credentialDateStamp(now)

	for _, m := range minors {
		bd := m.GetString("birthDate")
		if bd == "" {
			report.Skipped++

			continue
		}

		birth, err := ParseBirthDate(bd)
		if err != nil {
			report.Skipped++

			continue
		}

		hid := m.GetString("household")
		if _, ok := ages[hid]; !ok {
			ages[hid] = DefaultHouseholdGraduationAge

			if h, err := app.FindRecordById(CollectionNameHouseholds, hid); err == nil {
				orgIds[hid] = h.GetString("org")

				if org, err := app.FindRecordById(CollectionNameOrgs, h.GetString("org")); err == nil {
					ages[hid] = HouseholdGraduationAge(org)
				}
			}
		}

		if IsMinorAt(birth, now, ages[hid]) {
			continue
		}

		changed := false

		err = app.RunInTransaction(func(txApp App) error {
			res, err := txApp.NonconcurrentDB().Update(CollectionNameHouseholdMembers,
				dbx.Params{"isMinor": false, "updated": stamp},
				dbx.NewExp("[[id]]={:id} AND [[isMinor]]={:minor}", dbx.Params{"id": m.Id, "minor": true})).Execute()
			if err != nil {
				return err
			}

			if n, _ := res.RowsAffected(); n == 0 {
				return nil
			}

			changed = true

			return writeHouseholdGraduationAudit(txApp, ctx, m, orgIds[hid], ages[hid])
		})
		if err != nil {
			return report, err
		}

		if changed {
			report.Graduated++
		}
	}

	return report, nil
}

func writeHouseholdGraduationAudit(app App, ctx context.Context, m *Record, orgId string, age int) error {
	if !auditAvailable(app) {
		return nil
	}

	meta := map[string]any{"household": m.GetString("household"), "graduationAge": age}
	if orgId != "" {
		meta["orgId"] = orgId
	}

	entry := &AuditEntry{
		Action:         AuditActionHouseholdGraduated,
		CollectionId:   m.Collection().Id,
		CollectionName: CollectionNameHouseholdMembers,
		RecordId:       m.Id,
		ChangedFields:  []string{"isMinor"},
		Before:         map[string]any{"isMinor": true},
		After:          map[string]any{"isMinor": false},
		Meta:           meta,
	}
	applyAuditContext(ctx, entry)

	return WriteAuditEntry(app, ctx, entry, m)
}

// auditHouseholdMeta adds meta.orgId to the entries of household members (the
// row has no org of its own; the org is the household's).
func auditHouseholdMeta(app App, entry *AuditEntry, record *Record) {
	if record.Collection().Name != CollectionNameHouseholdMembers {
		return
	}

	h, err := app.FindRecordById(CollectionNameHouseholds, record.GetString("household"))
	if err != nil {
		return
	}

	if id := h.GetString("org"); id != "" {
		entry.Meta["orgId"] = id
	}

	entry.Meta["household"] = h.Id
}

type householdUpgrade struct {
	collection string
	field      string
	json       bool
}

// ---- encryption upgrade -------------------------------------------------------

// FieldEncryptionAvailable reports whether a key-encryption key is configured.
func FieldEncryptionAvailable(app App) bool {
	_, err := fieldKeySource(app)

	return err == nil
}

// UpgradeHouseholdEncryption turns the sensitive fields of the household
// collections into encrypted fields, and encrypts the values already stored.
// The migration creates them as plain fields (it must not depend on a key);
// the sealed check of the routes keeps sensitive data out of them until this
// has run. It runs at start-up and does nothing when there is no key or
// nothing to upgrade.
func UpgradeHouseholdEncryption(app App) error {
	if !HouseholdsAvailable(app) || !FieldEncryptionAvailable(app) {
		return nil
	}

	todo := map[string][]householdUpgrade{}

	for _, w := range []householdUpgrade{
		{CollectionNameHouseholds, "address", true},
		{CollectionNameHouseholds, "homePhone", false},
		{CollectionNameHouseholdMembers, "birthDate", false},
		{CollectionNameHouseholdMembers, "allergies", true},
		{CollectionNameHouseholdMembers, "medical", true},
	} {
		col, err := app.FindCachedCollectionByNameOrId(w.collection)
		if err != nil {
			return err
		}

		if f := col.Fields.GetByName(w.field); f != nil && f.Type() != FieldTypeEncryptedText && f.Type() != FieldTypeEncryptedJSON {
			todo[w.collection] = append(todo[w.collection], w)
		}
	}

	for _, name := range []string{CollectionNameHouseholds, CollectionNameHouseholdMembers} {
		wants := todo[name]
		if len(wants) == 0 {
			continue
		}

		if err := app.RunInTransaction(func(txApp App) error {
			return upgradeHouseholdCollection(txApp, name, wants)
		}); err != nil {
			return fmt.Errorf("failed to encrypt the fields of %s: %w", name, err)
		}
	}

	return nil
}

// upgradeHouseholdCollection replaces the plain fields with encrypted fields
// of the same name (a new field id: the type of a field cannot change under
// the same id) and writes back the values that were stored in the plain ones.
func upgradeHouseholdCollection(app App, name string, wants []householdUpgrade) error {
	col, err := app.FindCollectionByNameOrId(name)
	if err != nil {
		return err
	}

	// the plain values written around the routes (the admin API, SQL)
	saved := map[string]map[string]string{} // record id -> field -> value

	for _, w := range wants {
		var rows []struct {
			Id    string `db:"id"`
			Value string `db:"v"`
		}

		if err := app.DB().NewQuery("SELECT [[id]] AS id, COALESCE(CAST([[" + w.field + "]] AS TEXT), '') AS v FROM {{" + name + "}} WHERE [[" + w.field + "]] IS NOT NULL").All(&rows); err != nil {
			return err
		}

		for _, r := range rows {
			if r.Value == "" || r.Value == "null" {
				continue
			}

			if saved[r.Id] == nil {
				saved[r.Id] = map[string]string{}
			}

			saved[r.Id][w.field] = r.Value
		}
	}

	for _, w := range wants {
		col.Fields.RemoveByName(w.field)

		if w.json {
			col.Fields.Add(&EncryptedJSONField{Name: w.field, MaxSize: 64 << 10})
		} else {
			col.Fields.Add(&EncryptedTextField{Name: w.field, Max: 500})
		}
	}

	if err := app.Save(col); err != nil {
		return err
	}

	// written with UPDATE statements and not through a record load: on
	// PostgreSQL a "SELECT *" prepared before the change fails after it
	for id, fields := range saved {
		rec := NewRecord(col)
		rec.Id = id

		for f, v := range fields {
			rec.Set(f, v)

			dv, ok := col.Fields.GetByName(f).(DriverValuer)
			if !ok {
				return fmt.Errorf("field %s cannot be written", f)
			}

			val, err := dv.DriverValue(rec)
			if err != nil {
				return err
			}

			if _, err := app.DB().Update(name, dbx.Params{f: val}, dbx.HashExp{"id": id}).Execute(); err != nil {
				return err
			}
		}
	}

	return nil
}

// PrepareHouseholdImport drops, from the household collections of app, the
// plain sensitive fields that the import payload carries as encrypted fields,
// so that importing the collections of a database whose fields were already
// upgraded does not leave two fields of the same name (an import keeps the
// existing fields it does not find by id). It is for a freshly bootstrapped
// target (migrate-db) and refuses to drop a field of a table that holds rows.
func PrepareHouseholdImport(app App, payload []map[string]any) error {
	for _, item := range payload {
		name, _ := item["name"].(string)
		if name != CollectionNameHouseholds && name != CollectionNameHouseholdMembers {
			continue
		}

		col, err := app.FindCollectionByNameOrId(name)
		if err != nil {
			continue
		}

		fields, _ := item["fields"].([]any)
		changed := false

		for _, f := range fields {
			fm, _ := f.(map[string]any)

			ty, _ := fm["type"].(string)
			if ty != FieldTypeEncryptedText && ty != FieldTypeEncryptedJSON {
				continue
			}

			fname, _ := fm["name"].(string)

			cur := col.Fields.GetByName(fname)
			if cur == nil || cur.Type() == FieldTypeEncryptedText || cur.Type() == FieldTypeEncryptedJSON {
				continue
			}

			var n int
			if err := app.DB().Select("count(*)").From(name).Row(&n); err != nil {
				return err
			}

			if n > 0 {
				return fmt.Errorf("%s already holds rows: cannot replace the plain field %s", name, fname)
			}

			col.Fields.RemoveByName(fname)

			changed = true
		}

		if changed {
			if err := app.Save(col); err != nil {
				return err
			}
		}
	}

	return nil
}

// ---- hooks --------------------------------------------------------------------

func (app *BaseApp) registerHouseholdHooks() {
	app.OnRecordValidate(CollectionNameHouseholdMembers).Bind(&hook.Handler[*RecordEvent]{
		Id:   "__pbHouseholdMemberValidate__",
		Func: validateHouseholdMember,
	})

	app.OnRecordValidate(CollectionNameOrgs).Bind(&hook.Handler[*RecordEvent]{
		Id:   "__pbHouseholdOrgSettings__",
		Func: validateOrgGraduationAge,
	})

	app.OnServe().Bind(&hook.Handler[*ServeEvent]{
		Id: "__pbHouseholdEncryption__",
		Func: func(e *ServeEvent) error {
			if !FieldEncryptionAvailable(e.App) {
				if col, err := e.App.FindCachedCollectionByNameOrId(CollectionNameHouseholdMembers); err == nil {
					if f := col.Fields.GetByName("birthDate"); f != nil && f.Type() == FieldTypeText {
						e.App.Logger().Warn("No key-encryption key is configured: the household fields for address, phone, birth date, allergies and medical notes are not encrypted and cannot be written. Set CLAVIGER_KEK and restart.")
					}
				}
			} else if err := UpgradeHouseholdEncryption(e.App); err != nil {
				e.App.Logger().Warn("Failed to encrypt the household fields", "error", fmt.Sprint(err))
			}

			return e.Next()
		},
	})

	app.Cron().Add(householdCronId, householdCronExpr, func() {
		ctx := WithAuditContext(context.Background(), AuditActor{Type: AuditActorTypeSystem}, "")

		if _, err := RunHouseholdGraduation(app, ctx, time.Now()); err != nil && !errors.Is(err, context.Canceled) {
			app.Logger().Warn("Failed to run the household graduation job", "error", fmt.Sprint(err))
		}
	})
}
