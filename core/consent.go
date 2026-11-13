package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/hook"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #88, 88-2): consent records.
//
// Three hidden system collections hold the model:
//
//   - _consentPurposes: what a person can consent to (a key, the text shown,
//     its version, the lawful basis, a minimum age, whether a guardian must
//     give it, and whether it is direct marketing).
//   - _consents: the state row, one per (subject, purpose). It says whether
//     the consent is given now and on which version of the text.
//   - _consentEvents: append-only evidence, one row per grant or withdrawal,
//     with the text that was shown. A model hook refuses every update and
//     delete (so it holds for app.Save too), except the pseudonymization
//     helper that erasure will use.
//
// The subject is a text id: a _householdMembers row (a person without a login,
// such as a child) or a user. The purpose is stored as its KEY, not an id, so
// that a rule can read it as a plain value:
//
//	@collection._consents:c.subject ?= member && @collection._consents:c.purpose ?= "newsletter" && @collection._consents:c.withdrawnAt ?= ""
//
// All write rules are null. Rows are written by the functions below (and the
// routes built on them), which enforce who may consent for whom.

const (
	CollectionNameConsentPurposes = "_consentPurposes"
	CollectionNameConsents        = "_consents"
	CollectionNameConsentEvents   = "_consentEvents"

	ConsentSubjectMember = "member"
	ConsentSubjectUser   = "user"

	ConsentMethodWeb   = "web"
	ConsentMethodPaper = "paper"
	ConsentMethodStaff = "staff"

	ConsentActionGrant    = "grant"
	ConsentActionWithdraw = "withdraw"

	ConsentPurposeActive   = "active"
	ConsentPurposeArchived = "archived"

	// the status of one subject for one purpose, as the routes report it
	ConsentStatusGranted   = "granted"
	ConsentStatusWithdrawn = "withdrawn"
	ConsentStatusNone      = "none"

	ConsentLawfulConsent            = "consent"
	ConsentLawfulLegitimateInterest = "legitimate_interest"
	ConsentLawfulLegalObligation    = "legal_obligation"
	ConsentLawfulContract           = "contract"
	ConsentLawfulVitalInterest      = "vital_interest"
	ConsentLawfulPublicTask         = "public_task"

	// DefaultMarketingMinAge is the minimum age a marketing purpose gets when
	// none is set (spec 10.4).
	DefaultMarketingMinAge = 18
	MaxConsentMinAge       = 21

	// the reason codes of a purpose that cannot be granted
	ConsentReasonMarketingMinor = "marketing_minor"
	ConsentReasonNeedsGuardian  = "needs_guardian"
)

var (
	ConsentSubjectTypes  = []string{ConsentSubjectMember, ConsentSubjectUser}
	ConsentMethods       = []string{ConsentMethodWeb, ConsentMethodPaper, ConsentMethodStaff}
	ConsentActions       = []string{ConsentActionGrant, ConsentActionWithdraw}
	ConsentPurposeStates = []string{ConsentPurposeActive, ConsentPurposeArchived}
	ConsentLawfulBases   = []string{
		ConsentLawfulConsent, ConsentLawfulLegitimateInterest, ConsentLawfulLegalObligation,
		ConsentLawfulContract, ConsentLawfulVitalInterest, ConsentLawfulPublicTask,
	}

	// consentNow is the clock of the consent functions (replaced in tests).
	consentNow = time.Now
)

type consentEventRewriteCtxKey struct{}

// ConsentsAvailable reports whether the consent model exists.
func ConsentsAvailable(app App) bool {
	_, err := app.FindCachedCollectionByNameOrId(CollectionNameConsents)

	return err == nil
}

// ---- subjects -----------------------------------------------------------------

// ConsentSubject is the person a consent is about.
type ConsentSubject struct {
	Type string // ConsentSubjectMember or ConsentSubjectUser
	Id   string
	Name string

	Member    *Record // member subjects
	Household *Record // member subjects
	Org       *Record // member subjects: the org of the household
	User      *Record // user subjects
}

// Minor reports whether the subject is a minor (a member row flagged isMinor).
func (s *ConsentSubject) Minor() bool {
	return s.Member != nil && s.Member.GetBool("isMinor")
}

// FindConsentMember resolves a household member as a consent subject. The
// member must be active in an active household of a live org.
func FindConsentMember(app App, id string) (*ConsentSubject, error) {
	m, err := app.FindRecordById(CollectionNameHouseholdMembers, strings.TrimSpace(id))
	if err != nil || m.GetString("status") != HouseholdMemberActive {
		return nil, orgErr(404, "Household member not found.")
	}

	h, org, err := FindHousehold(app, m.GetString("household"))
	if err != nil || h.GetString("status") != HouseholdStatusActive {
		return nil, orgErr(404, "Household member not found.")
	}

	return &ConsentSubject{Type: ConsentSubjectMember, Id: m.Id, Name: m.GetString("name"), Member: m, Household: h, Org: org}, nil
}

// FindConsentUser resolves a user as a consent subject.
func FindConsentUser(app App, id string) (*ConsentSubject, error) {
	col, err := OrgUserCollection(app)
	if err != nil {
		return nil, orgErr(500, "The organizations model is not initialized.")
	}

	u, err := app.FindRecordById(col, strings.TrimSpace(id))
	if err != nil {
		return nil, orgErr(404, "User not found.")
	}

	return consentUserSubject(u), nil
}

func consentUserSubject(u *Record) *ConsentSubject {
	name := strings.TrimSpace(u.GetString("name"))
	if name == "" {
		name = u.Email()
	}

	return &ConsentSubject{Type: ConsentSubjectUser, Id: u.Id, Name: name, User: u}
}

// consentUserInOrg reports whether the user belongs to the org: an active
// member, or an active person of a household of the org.
func consentUserInOrg(app App, orgId, userId string) bool {
	if m, err := FindOrgMember(app, orgId, userId); err == nil && m.GetString("status") == OrgStatusActive {
		return true
	}

	_, err := app.FindFirstRecordByFilter(CollectionNameHouseholdMembers,
		"user={:u} && status={:s} && household.org={:o}",
		dbx.Params{"u": userId, "s": HouseholdMemberActive, "o": orgId})

	return err == nil
}

// consentAge returns the age in whole years, or -1 when the birth date is
// unknown.
func consentAge(m *Record, now time.Time) int {
	b, err := ParseBirthDate(m.GetString("birthDate"))
	if err != nil {
		return -1
	}

	n := now.UTC()
	years := n.Year() - b.Year()

	if n.Month() < b.Month() || (n.Month() == b.Month() && n.Day() < b.Day()) {
		years--
	}

	return years
}

// consentBelowMinAge reports whether the person is younger than the purpose's
// minimum age. An unknown birth date counts as below when the person is a minor.
func consentBelowMinAge(m *Record, purpose *Record, now time.Time) bool {
	minAge := purpose.GetInt("minAge")
	if m == nil || minAge <= 0 {
		return false
	}

	age := consentAge(m, now)
	if age < 0 {
		return m.GetBool("isMinor")
	}

	return age < minAge
}

// consentNeedsGuardian reports whether a guardian must give the consent for
// this person and purpose: a minor always, and a person below the purpose's
// minimum age.
func consentNeedsGuardian(s *ConsentSubject, purpose *Record, now time.Time) bool {
	if s.Member == nil {
		return false
	}

	return s.Minor() || consentBelowMinAge(s.Member, purpose, now)
}

// ---- purposes -----------------------------------------------------------------

// FindConsentPurpose loads a purpose by key or id.
func FindConsentPurpose(app App, ref string) (*Record, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, orgErr(404, "Consent purpose not found.")
	}

	if p, err := app.FindFirstRecordByData(CollectionNameConsentPurposes, "key", ref); err == nil {
		return p, nil
	}

	if p, err := app.FindRecordById(CollectionNameConsentPurposes, ref); err == nil {
		return p, nil
	}

	return nil, orgErr(404, "Consent purpose not found.")
}

// ConsentPurposeApplies reports whether the purpose is offered to the subject:
// a global purpose to everyone, an org's purpose to the people of that org.
func ConsentPurposeApplies(app App, purpose *Record, s *ConsentSubject) bool {
	org := purpose.GetString("org")
	if org == "" {
		return true
	}

	if s.Type == ConsentSubjectMember {
		return s.Org != nil && s.Org.Id == org
	}

	return consentUserInOrg(app, org, s.Id)
}

// ConsentPurposeInput is the writable part of a purpose.
type ConsentPurposeInput map[string]any

func consentStr(in map[string]any, k string) (string, bool) {
	v, ok := in[k]
	if !ok {
		return "", false
	}

	s, _ := v.(string)

	return strings.TrimSpace(s), true
}

func consentInt(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		return int(t), t == float64(int(t))
	case int:
		return t, true
	case int64:
		return int(t), true
	}

	return 0, false
}

func applyConsentPurposeInput(app App, rec *Record, in map[string]any) error {
	if v, ok := consentStr(in, "key"); ok {
		if !rec.IsNew() && v != rec.GetString("key") {
			return orgErr(400, "The key of a purpose cannot be changed.")
		}

		rec.Set("key", v)
	}

	if v, ok := consentStr(in, "name"); ok {
		rec.Set("name", v)
	}

	if v, ok := in["text"].(string); ok {
		rec.Set("text", strings.TrimSpace(v))
	}

	if v, ok := consentStr(in, "lawfulBasis"); ok {
		rec.Set("lawfulBasis", v)
	}

	if v, ok := in["minAge"]; ok {
		n, ok := consentInt(v)
		if !ok || n < 0 || n > MaxConsentMinAge {
			return orgErr(400, fmt.Sprintf("minAge must be a whole number from 0 to %d.", MaxConsentMinAge))
		}

		rec.Set("minAge", n)
	}

	if v, ok := in["requiresGuardian"].(bool); ok {
		rec.Set("requiresGuardian", v)
	}

	if v, ok := in["marketing"].(bool); ok {
		rec.Set("marketing", v)
	}

	if v, ok := consentStr(in, "status"); ok {
		rec.Set("status", v)
	}

	if v, ok := consentStr(in, "org"); ok {
		if !rec.IsNew() && v != rec.GetString("org") {
			// moving a purpose to another org would change who is asked
			return orgErr(400, "The org of a purpose cannot be changed.")
		}

		if v != "" {
			org, err := FindOrg(app, v)
			if err != nil {
				return orgErr(404, "Organization not found.")
			}

			v = org.Id
		}

		rec.Set("org", v)
	}

	return nil
}

// CreateConsentPurpose creates a purpose (version 1). A marketing purpose with
// no minimum age gets 18.
func CreateConsentPurpose(app App, ctx context.Context, in map[string]any) (*Record, error) {
	col, err := app.FindCachedCollectionByNameOrId(CollectionNameConsentPurposes)
	if err != nil {
		return nil, orgErr(404, "Consents are not available.")
	}

	rec := NewRecord(col)
	rec.Set("status", ConsentPurposeActive)
	rec.Set("lawfulBasis", ConsentLawfulConsent)

	if err := applyConsentPurposeInput(app, rec, in); err != nil {
		return nil, err
	}

	if err := saveOrgRecord(app, ctx, rec); err != nil {
		return nil, err
	}

	return rec, nil
}

// UpdateConsentPurpose changes a purpose. A change of the text raises the
// version (the model hook does it), so existing consents show as given on an
// older version.
func UpdateConsentPurpose(app App, ctx context.Context, rec *Record, in map[string]any) (*Record, error) {
	if err := applyConsentPurposeInput(app, rec, in); err != nil {
		return nil, err
	}

	if err := saveOrgRecord(app, ctx, rec); err != nil {
		return nil, err
	}

	return rec, nil
}

// DeleteConsentPurpose deletes a purpose that nobody has consented to. A used
// purpose is the evidence of its consents: it is archived instead (409).
func DeleteConsentPurpose(app App, ctx context.Context, rec *Record) error {
	for _, name := range []string{CollectionNameConsents, CollectionNameConsentEvents} {
		if _, err := app.FindFirstRecordByData(name, "purpose", rec.GetString("key")); err == nil {
			return orgErr(409, "The purpose has consent records. Archive it instead.")
		}
	}

	return deleteOrgRecord(app, ctx, rec)
}

// ---- grant and withdraw -------------------------------------------------------

type consentChange struct {
	Purpose     *Record
	Subject     *ConsentSubject
	Action      string
	Method      string
	EvidenceRef string
	GivenBy     string // the subject, or the guardian member, or ""
	Actor       *Record
	OrgId       string
}

// ConsentResult is what a grant or withdrawal did.
type ConsentResult struct {
	State   *Record
	Event   *Record // nil when nothing changed
	Changed bool
}

func consentOrgId(s *ConsentSubject, purpose *Record, fallback string) string {
	switch {
	case s.Org != nil:
		return s.Org.Id
	case purpose.GetString("org") != "":
		return purpose.GetString("org")
	}

	return fallback
}

func findConsentState(app App, subject, purpose string) *Record {
	r, err := app.FindFirstRecordByFilter(CollectionNameConsents, "subject={:s} && purpose={:p}",
		dbx.Params{"s": subject, "p": purpose})
	if err != nil {
		return nil
	}

	return r
}

// ConsentStatus returns the state of the row: granted, withdrawn or none.
func ConsentStatus(state *Record) string {
	switch {
	case state == nil || state.GetDateTime("grantedAt").IsZero():
		return ConsentStatusNone
	case !state.GetDateTime("withdrawnAt").IsZero():
		return ConsentStatusWithdrawn
	}

	return ConsentStatusGranted
}

// ConsentOutdated reports whether the consent was given on an older version
// of the purpose's text.
func ConsentOutdated(state, purpose *Record) bool {
	return ConsentStatus(state) == ConsentStatusGranted && state.GetInt("policyVersion") < purpose.GetInt("version")
}

func applyConsentChange(app App, ctx context.Context, c consentChange) (*ConsentResult, error) {
	if c.Purpose.GetString("status") != ConsentPurposeActive {
		return nil, orgErr(409, "This purpose is archived.")
	}

	if !ConsentPurposeApplies(app, c.Purpose, c.Subject) {
		return nil, orgErr(404, "Consent purpose not found.")
	}

	key := c.Purpose.GetString("key")
	version := c.Purpose.GetInt("version")
	now := consentNow().UTC()

	var res *ConsentResult

	err := app.RunInTransaction(func(txApp App) error {
		state := findConsentState(txApp, c.Subject.Id, key)

		switch c.Action {
		case ConsentActionGrant:
			if ConsentStatus(state) == ConsentStatusGranted && state.GetInt("policyVersion") == version {
				res = &ConsentResult{State: state}
				return nil
			}
		case ConsentActionWithdraw:
			if ConsentStatus(state) != ConsentStatusGranted {
				return orgErr(409, "There is no consent to withdraw.")
			}
		default:
			return orgErr(400, "The action must be grant or withdraw.")
		}

		if state == nil {
			col, err := txApp.FindCachedCollectionByNameOrId(CollectionNameConsents)
			if err != nil {
				return err
			}

			state = NewRecord(col)
			state.Set("subject", c.Subject.Id)
			state.Set("subjectType", c.Subject.Type)
			state.Set("purpose", key)
		}

		state.Set("method", c.Method)
		state.Set("evidenceRef", c.EvidenceRef)

		if c.OrgId != "" {
			state.Set("org", c.OrgId)
		}

		if c.Action == ConsentActionGrant {
			state.Set("policyVersion", version)
			state.Set("grantedAt", mustDateTime(now))
			state.Set("withdrawnAt", "")
			state.Set("givenBy", c.GivenBy)
		} else {
			state.Set("withdrawnAt", mustDateTime(now))
		}

		if err := saveOrgRecord(txApp, ctx, state); err != nil {
			return err
		}

		evCol, err := txApp.FindCachedCollectionByNameOrId(CollectionNameConsentEvents)
		if err != nil {
			return err
		}

		ev := NewRecord(evCol)
		ev.Set("subject", c.Subject.Id)
		ev.Set("subjectType", c.Subject.Type)
		ev.Set("purpose", key)
		ev.Set("action", c.Action)
		ev.Set("policyVersion", version)
		ev.Set("text", c.Purpose.GetString("text"))
		ev.Set("givenBy", c.GivenBy)
		ev.Set("method", c.Method)
		ev.Set("evidenceRef", c.EvidenceRef)
		ev.Set("org", c.OrgId)

		if c.Actor != nil {
			ev.Set("actor", c.Actor.Id)
		}

		if err := saveOrgRecord(txApp, ctx, ev); err != nil {
			return err
		}

		res = &ConsentResult{State: state, Event: ev, Changed: true}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return res, nil
}

func mustDateTime(t time.Time) types.DateTime {
	d, _ := types.ParseDateTime(t)

	return d
}

// activeGuardianOf returns the active adult guardian row of the household for
// the user, or nil.
func activeGuardianOf(app App, household string, userId string) *Record {
	g, err := app.FindFirstRecordByFilter(CollectionNameHouseholdMembers,
		"household={:h} && user={:u} && status={:s} && guardian=true && isMinor=false",
		dbx.Params{"h": household, "u": userId, "s": HouseholdMemberActive})
	if err != nil {
		return nil
	}

	return g
}

func checkConsentGrant(app App, purpose *Record, s *ConsentSubject) error {
	if purpose.GetBool("marketing") && s.Minor() {
		return orgErr(403, "Direct marketing cannot be consented to for a minor.")
	}

	return nil
}

// ConsentSelf grants or withdraws the consent of the signed-in user for
// themselves (memberId empty) or for a minor of a household where they are an
// active guardian (memberId set). A person who is a minor, or below the
// purpose's minimum age, or under a purpose that requires a guardian, can only
// be consented for by a guardian.
func ConsentSelf(app App, ctx context.Context, auth *Record, purposeRef, memberId, action string) (*ConsentResult, error) {
	if auth == nil || auth.IsSuperuser() || auth.Collection().Name != OrgUserCollectionName() {
		return nil, orgErr(403, "Sign in with a user account to manage consents.")
	}

	purpose, err := FindConsentPurpose(app, purposeRef)
	if err != nil {
		return nil, err
	}

	now := consentNow()
	change := consentChange{Purpose: purpose, Action: action, Method: ConsentMethodWeb, Actor: auth}

	var subject *ConsentSubject

	if memberId != "" {
		subject, err = FindConsentMember(app, memberId)
		if err != nil {
			return nil, err
		}
	}

	switch {
	case subject != nil && subject.Member.GetString("user") != auth.Id:
		// for someone else: only for a minor, only as an active guardian of the household
		guardian := activeGuardianOf(app, subject.Household.Id, auth.Id)
		if guardian == nil {
			return nil, orgErr(404, "Household member not found.")
		}

		if !consentNeedsGuardian(subject, purpose, now) {
			return nil, orgErr(403, "Only this person can give their consent.")
		}

		change.GivenBy = guardian.Id
	default:
		// for themselves (an own member row is the same person as the user)
		subject = consentUserSubject(auth)

		// a person with a login who is below the purpose's minimum age
		rows, _ := app.FindRecordsByFilter(CollectionNameHouseholdMembers,
			"user={:u} && status={:s}", "", 0, 0, dbx.Params{"u": auth.Id, "s": HouseholdMemberActive})
		for _, m := range rows {
			if consentBelowMinAge(m, purpose, now) || (purpose.GetBool("requiresGuardian") && m.GetBool("isMinor")) {
				return nil, orgErr(403, "A guardian must give this consent.")
			}
		}

		change.GivenBy = auth.Id
	}

	if action == ConsentActionGrant {
		if err := checkConsentGrant(app, purpose, subject); err != nil {
			return nil, err
		}
	}

	change.Subject = subject
	change.OrgId = consentOrgId(subject, purpose, "")

	return applyConsentChange(app, ctx, change)
}

// ConsentStaffInput is a consent recorded by staff on paper or in person.
type ConsentStaffInput struct {
	Purpose     string
	Member      string // a household member of the org, or
	User        string // a user who belongs to the org
	Action      string
	Method      string // paper or staff (default staff)
	EvidenceRef string // required for a grant: where the evidence is kept
	GivenBy     string // a minor's consent needs the guardian member who gave it
}

// ConsentStaff records a consent that was given on paper or told to staff.
// Staff are the owners and admins of the org and the members tagged
// "registrar" (the same people who manage the households).
func ConsentStaff(app App, ctx context.Context, auth *Record, org *Record, in ConsentStaffInput) (*ConsentResult, error) {
	actor := NewHouseholdActor(app, org, nil, auth)
	if !actor.IsMember() && !actor.Superuser {
		return nil, orgErr(404, "Organization not found.")
	}

	if !actor.Staff {
		return nil, orgErr(403, "Only the staff of the organization can record a consent.")
	}

	if err := requireLiveOrg(org, actor.OrgActor); err != nil {
		return nil, err
	}

	purpose, err := FindConsentPurpose(app, in.Purpose)
	if err != nil {
		return nil, err
	}

	if po := purpose.GetString("org"); po != "" && po != org.Id {
		return nil, orgErr(404, "Consent purpose not found.")
	}

	method := in.Method
	if method == "" {
		method = ConsentMethodStaff
	}

	if method != ConsentMethodPaper && method != ConsentMethodStaff {
		return nil, orgErr(400, "The method must be paper or staff.")
	}

	if in.Action == ConsentActionGrant && strings.TrimSpace(in.EvidenceRef) == "" {
		return nil, orgErr(400, "evidenceRef is required: say where the evidence is kept.")
	}

	if (in.Member == "") == (in.User == "") {
		return nil, orgErr(400, "Give either member or user.")
	}

	var subject *ConsentSubject

	if in.Member != "" {
		subject, err = FindConsentMember(app, in.Member)
		if err != nil {
			return nil, err
		}

		if subject.Org.Id != org.Id {
			return nil, orgErr(404, "Household member not found.")
		}
	} else {
		subject, err = FindConsentUser(app, in.User)
		if err != nil {
			return nil, err
		}

		if !consentUserInOrg(app, org.Id, subject.Id) {
			return nil, orgErr(404, "User not found.")
		}
	}

	change := consentChange{
		Purpose: purpose, Subject: subject, Action: in.Action, Method: method,
		EvidenceRef: strings.TrimSpace(in.EvidenceRef), Actor: auth, OrgId: org.Id,
	}

	switch {
	case consentNeedsGuardian(subject, purpose, consentNow()):
		if in.GivenBy != "" || in.Action == ConsentActionGrant {
			g, err := app.FindRecordById(CollectionNameHouseholdMembers, in.GivenBy)
			if err != nil || g.GetString("household") != subject.Household.Id ||
				g.GetString("status") != HouseholdMemberActive || !g.GetBool("guardian") || g.GetBool("isMinor") {
				return nil, orgErr(400, "givenBy must be an active guardian of the person's household.")
			}

			change.GivenBy = g.Id
		}
	default:
		change.GivenBy = subject.Id
	}

	if in.Action == ConsentActionGrant {
		if err := checkConsentGrant(app, purpose, subject); err != nil {
			return nil, err
		}
	}

	return applyConsentChange(app, ctx, change)
}

// ---- reading ------------------------------------------------------------------

// ConsentPurposeStatus is one purpose for one subject.
type ConsentPurposeStatus struct {
	Purpose  *Record
	State    *Record
	Status   string
	Outdated bool
	CanGrant bool
	Reason   string
}

// ConsentOverviewSubject is a person the signed-in user can manage consents for.
type ConsentOverviewSubject struct {
	*ConsentSubject
	Purposes []ConsentPurposeStatus
}

// ConsentOverview lists the purposes offered to the user and to the minors of
// the households where they are an active guardian, with the current state.
func ConsentOverview(app App, auth *Record) ([]*ConsentOverviewSubject, error) {
	if auth == nil || auth.IsSuperuser() || auth.Collection().Name != OrgUserCollectionName() {
		return []*ConsentOverviewSubject{}, nil
	}

	purposes, err := app.FindRecordsByFilter(CollectionNameConsentPurposes, "status={:s}", "name", 0, 0,
		dbx.Params{"s": ConsentPurposeActive})
	if err != nil {
		return nil, err
	}

	now := consentNow()
	self := consentUserSubject(auth)

	subjects := []*ConsentSubject{self}

	households, err := MyHouseholds(app, auth.Id)
	if err != nil {
		return nil, err
	}

	for _, h := range households {
		if activeGuardianOf(app, h.Id, auth.Id) == nil {
			continue
		}

		members, err := HouseholdMembers(app, h)
		if err != nil {
			return nil, err
		}

		org, _ := app.FindRecordById(CollectionNameOrgs, h.GetString("org"))

		for _, m := range members {
			if m.GetString("status") != HouseholdMemberActive || !m.GetBool("isMinor") || m.GetString("user") == auth.Id {
				continue
			}

			subjects = append(subjects, &ConsentSubject{
				Type: ConsentSubjectMember, Id: m.Id, Name: m.GetString("name"), Member: m, Household: h, Org: org,
			})
		}
	}

	out := make([]*ConsentOverviewSubject, 0, len(subjects))

	for _, s := range subjects {
		item := &ConsentOverviewSubject{ConsentSubject: s, Purposes: []ConsentPurposeStatus{}}

		for _, p := range purposes {
			if !ConsentPurposeApplies(app, p, s) {
				continue
			}

			state := findConsentState(app, s.Id, p.GetString("key"))
			ps := ConsentPurposeStatus{
				Purpose: p, State: state, Status: ConsentStatus(state),
				Outdated: state != nil && ConsentOutdated(state, p), CanGrant: true,
			}

			switch {
			case p.GetBool("marketing") && s.Minor():
				ps.CanGrant, ps.Reason = false, ConsentReasonMarketingMinor
			case s.Type == ConsentSubjectUser && consentUserNeedsGuardian(app, auth, p, now):
				ps.CanGrant, ps.Reason = false, ConsentReasonNeedsGuardian
			}

			item.Purposes = append(item.Purposes, ps)
		}

		out = append(out, item)
	}

	return out, nil
}

func consentUserNeedsGuardian(app App, auth *Record, purpose *Record, now time.Time) bool {
	rows, _ := app.FindRecordsByFilter(CollectionNameHouseholdMembers,
		"user={:u} && status={:s}", "", 0, 0, dbx.Params{"u": auth.Id, "s": HouseholdMemberActive})

	for _, m := range rows {
		if consentBelowMinAge(m, purpose, now) || (purpose.GetBool("requiresGuardian") && m.GetBool("isMinor")) {
			return true
		}
	}

	return false
}

// ConsentHistory returns the state rows and the events of the subjects, oldest
// event first.
func ConsentHistory(app App, subjectIds ...string) (states, events []*Record, err error) {
	if len(subjectIds) == 0 {
		return []*Record{}, []*Record{}, nil
	}

	exps := make([]dbx.Expression, 0, len(subjectIds))
	for _, id := range subjectIds {
		exps = append(exps, dbx.HashExp{"subject": id})
	}

	col, err := app.FindCachedCollectionByNameOrId(CollectionNameConsents)
	if err != nil {
		return nil, nil, err
	}

	states = []*Record{}
	if err = app.RecordQuery(col).AndWhere(dbx.Or(exps...)).OrderBy("created ASC", "id ASC").All(&states); err != nil {
		return nil, nil, err
	}

	evCol, err := app.FindCachedCollectionByNameOrId(CollectionNameConsentEvents)
	if err != nil {
		return nil, nil, err
	}

	events = []*Record{}
	if err = app.RecordQuery(evCol).AndWhere(dbx.Or(exps...)).OrderBy("created ASC", "id ASC").All(&events); err != nil {
		return nil, nil, err
	}

	return states, events, nil
}

// ---- pseudonymization (for erasure, 88-4) ------------------------------------------

// ConsentPseudonym is the stand-in for a subject id in kept evidence.
func ConsentPseudonym(id string) string {
	sum := sha256.Sum256([]byte("claviger-consent:" + id))

	return "erased-" + hex.EncodeToString(sum[:])[:24]
}

// PseudonymizeConsents replaces the ids of the given people (a member id or a
// user id) in the consent rows with a pseudonym, and clears the evidence
// reference. It is the one operation allowed to change a consent event: the
// action, purpose, text version and time stay as the evidence of what was
// agreed. It returns the number of events changed. Erasure (88-4) calls it;
// nothing else does.
func PseudonymizeConsents(app App, ctx context.Context, ids ...string) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	changed := 0
	rewrite := context.WithValue(ctx, consentEventRewriteCtxKey{}, true)

	err := app.RunInTransaction(func(txApp App) error {
		for _, id := range ids {
			if id == "" {
				continue
			}

			p := ConsentPseudonym(id)

			for _, name := range []string{CollectionNameConsentEvents, CollectionNameConsents} {
				filter := "subject={:i} || givenBy={:i}"
				if name == CollectionNameConsentEvents {
					filter += " || actor={:i}"
				}

				rows, err := txApp.FindRecordsByFilter(name, filter, "", 0, 0, dbx.Params{"i": id})
				if err != nil {
					return err
				}

				for _, r := range rows {
					if r.GetString("subject") == id {
						r.Set("subject", p)
						r.Set("evidenceRef", "")
					}

					if r.GetString("givenBy") == id {
						r.Set("givenBy", p)
					}

					if r.Collection().Fields.GetByName("actor") != nil && r.GetString("actor") == id {
						r.Set("actor", p)
					}

					if r.Collection().Fields.GetByName("pseudonymizedAt") != nil {
						r.Set("pseudonymizedAt", mustDateTime(consentNow().UTC()))
					}

					if err := txApp.SaveWithContext(rewrite, r); err != nil {
						return err
					}

					if name == CollectionNameConsentEvents {
						changed++
					}
				}
			}
		}

		return nil
	})
	if err != nil {
		return 0, err
	}

	return changed, nil
}

// ---- hooks --------------------------------------------------------------------

// validateConsentPurpose keeps the key fixed, defaults the minimum age of a
// marketing purpose to 18, and raises the version when the text changes.
func validateConsentPurpose(e *RecordEvent) error {
	r := e.Record

	if r.IsNew() {
		r.Set("version", 1)

		if r.GetBool("marketing") && r.GetInt("minAge") == 0 {
			r.Set("minAge", DefaultMarketingMinAge)
		}

		return e.Next()
	}

	orig := r.Original()
	if orig == nil {
		return e.Next()
	}

	if r.GetString("key") != orig.GetString("key") {
		r.Set("key", orig.GetString("key"))
	}

	if r.GetString("text") != orig.GetString("text") {
		r.Set("version", orig.GetInt("version")+1)
	} else {
		r.Set("version", orig.GetInt("version"))
	}

	return e.Next()
}

func (app *BaseApp) registerConsentHooks() {
	app.OnRecordValidate(CollectionNameConsentPurposes).Bind(&hook.Handler[*RecordEvent]{
		Id:   "__pbConsentPurposeValidate__",
		Func: validateConsentPurpose,
	})

	// _consentEvents is append-only for the records API, Go hooks and JS hooks
	// (not raw SQL), the same way _auditLog is. Creating is open to code: the
	// collection has null write rules, so only the functions above create.
	guard := func(me *ModelEvent) error {
		re, ok := newRecordEventFromModelEvent(me)
		if !ok || re.Record.Collection().Name != CollectionNameConsentEvents {
			return me.Next()
		}

		if me.Type == ModelEventTypeUpdate && me.Context != nil && me.Context.Value(consentEventRewriteCtxKey{}) != nil {
			return me.Next()
		}

		return fmt.Errorf("the %s collection is append-only: %s is not allowed", CollectionNameConsentEvents, me.Type)
	}

	app.OnModelUpdate().Bind(&hook.Handler[*ModelEvent]{Id: "__pbConsentEventGuard__", Func: guard, Priority: -100000})
	app.OnModelDelete().Bind(&hook.Handler[*ModelEvent]{Id: "__pbConsentEventGuard__", Func: guard, Priority: -100000})
}
