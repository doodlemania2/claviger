package core

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #82, 82-2, 82-3, 82-6): check in, release, alerts.

const AuditActorTypeKiosk = "kiosk" // the actor id is "kiosk:<kioskId>"

// CheckinActor is who calls a check-in route: a signed-in user, or a kiosk.
type CheckinActor struct {
	User  *Record
	Kiosk *Record
}

func (a CheckinActor) typ() string {
	if a.Kiosk != nil {
		return "kiosk"
	}

	return "user"
}

func (a CheckinActor) id() string {
	switch {
	case a.Kiosk != nil:
		return a.Kiosk.Id
	case a.User != nil:
		return a.User.Id
	}

	return ""
}

// CheckinNotice is what the notifier needs to tell people about an alert.
type CheckinNotice struct {
	// Kind is locked, do_not_release or override.
	Kind string
	// Audience is "staff" (the org's coordinators) or "guardians" (the
	// household's guardians, after an override).
	Audience   string
	Org        *Record
	Session    *Record
	Recipients []*Record
}

// CheckinNotifier emails the notice and returns how many it reached. The mails
// package sets it (core cannot import it). Nil means nobody is told by email;
// the log and the audit entry are written either way.
var CheckinNotifier func(app App, n *CheckinNotice) (int, error)

// CheckinStanding is the standing of a user in the org of a session.
type CheckinStanding struct{ Staff, Supervisor bool }

// CheckinStandingOf works out whether the user is check-in staff in the org:
// owners and admins are both; a member with the tag is staff, and a member with
// the supervisor tag is also a supervisor.
func CheckinStandingOf(app App, org, user *Record) CheckinStanding {
	if org == nil || user == nil {
		return CheckinStanding{}
	}

	m := ActiveOrgMember(app, org, user.Id)
	if m == nil {
		return CheckinStanding{}
	}

	switch m.GetString("role") {
	case OrgRoleOwner, OrgRoleAdmin:
		return CheckinStanding{Staff: true, Supervisor: true}
	case OrgRoleMember:
		tags := OrgTagNames(app, m)

		sup := slices.Contains(tags, CheckinSupervisorTag)

		return CheckinStanding{Staff: sup || slices.Contains(tags, CheckinStaffTag), Supervisor: sup}
	}

	return CheckinStanding{}
}

// CheckinGuardianMember returns the active guardian row of the user in the household.
func CheckinGuardianMember(app App, householdId, userId string) *Record {
	if householdId == "" || userId == "" {
		return nil
	}

	m, err := app.FindFirstRecordByFilter(CollectionNameHouseholdMembers,
		"household={:h} && user={:u} && guardian=true && status='active'", dbx.Params{"h": householdId, "u": userId})
	if err != nil {
		return nil
	}

	return m
}

// CheckinAuthorizedPickup returns the active pickup authorization of the user
// for the child (an approved person with an account), or nil.
func CheckinAuthorizedPickup(app App, childId, userId string, now time.Time) *Record {
	if childId == "" || userId == "" {
		return nil
	}

	rows, err := app.FindRecordsByFilter(PickupAuthCollection, "child={:c} && user={:u} && status='active'", "", 0, 0,
		dbx.Params{"c": childId, "u": userId})
	if err != nil {
		return nil
	}

	for _, r := range rows {
		if until := r.GetDateTime("validUntil"); until.IsZero() || until.Time().After(now) {
			return r
		}
	}

	return nil
}

// checkinHasValue reports whether an allergy or medical value holds anything.
func checkinHasValue(v any) bool {
	var s string

	switch t := v.(type) {
	case nil:
		return false
	case string:
		s = t
	default:
		raw, err := json.Marshal(t)
		if err != nil {
			return false
		}

		s = string(raw)
	}

	switch strings.TrimSpace(s) {
	case "", "null", "{}", "[]", `""`:
		return false
	}

	return true
}

// CheckinFlagsOf returns the plain markers of a child: "allergy" and "medical".
// The details stay encrypted on the member; a label shows the marker only.
func CheckinFlagsOf(child *Record) []string {
	flags := []string{}

	if checkinHasValue(child.Get("allergies")) {
		flags = append(flags, "allergy")
	}

	if checkinHasValue(child.Get("medical")) {
		flags = append(flags, "medical")
	}

	return flags
}

func checkinEventLive(event *Record, now time.Time) bool {
	if event.GetString("status") != CheckinEventOpen {
		return false
	}

	ends := event.GetDateTime("ends")

	return ends.IsZero() || ends.Time().After(now)
}

func checkinPurgeAfter(org *Record, from time.Time) types.DateTime {
	dt, _ := types.ParseDateTime(from.UTC().AddDate(0, CheckinRetentionMonths(org), 0))

	return dt
}

func checkinAuditContext(ctx context.Context, actor CheckinActor, org *Record) context.Context {
	if actor.Kiosk != nil {
		return WithAuditContext(ctx, AuditActor{Type: AuditActorTypeKiosk, Id: "kiosk:" + actor.Kiosk.Id, OrgId: org.Id}, "")
	}

	return ctx
}

// ---- log -------------------------------------------------------------------

type checkinLogEntry struct {
	Org     *Record
	Event   string
	Session string
	Action  string
	Method  string
	Result  string
	Actor   CheckinActor
	Detail  map[string]any
}

// writeCheckinLog appends a row. The collection has null write rules and a
// guard that refuses update and delete, so this is the only way in.
func writeCheckinLog(app App, ctx context.Context, e checkinLogEntry) error {
	col, err := app.FindCachedCollectionByNameOrId(CheckinLogCollection)
	if err != nil {
		return nil
	}

	rec := NewRecord(col)
	rec.Set("org", e.Org.Id)
	rec.Set("event", e.Event)
	rec.Set("session", e.Session)
	rec.Set("action", e.Action)
	rec.Set("actorType", e.Actor.typ())
	rec.Set("actorId", e.Actor.id())
	rec.Set("method", e.Method)
	rec.Set("result", e.Result)

	if e.Detail != nil {
		rec.Set("detail", e.Detail)
	}

	rec.Set("purgeAfter", checkinPurgeAfter(e.Org, time.Now()))

	if err := app.SaveWithContext(ctx, rec); err != nil {
		return err
	}

	// a row written after an incident hold on its session is held as well
	if e.Session != "" {
		if s, err := app.FindRecordById(CheckinSessionCollection, e.Session); err == nil && s.GetBool("incident") {
			holdNewCheckinLog(app, ctx, s, rec)
		}
	}

	return nil
}

// ---- check in -------------------------------------------------------------------

// CheckinInput is a check-in request.
type CheckinInput struct {
	Event     *Record
	Household string
	Children  []string
	// Guardian is the user acting as the household's guardian.
	Guardian *Record
	// Actor is the principal that made the call (the guardian, or a kiosk).
	Actor CheckinActor
}

// CheckinLabel is the label data of one child. Claviger returns it; printing
// is up to the app on top.
type CheckinLabel struct {
	Session     string   `json:"session"`
	Child       string   `json:"child"`
	ChildName   string   `json:"childName"`
	Code        string   `json:"code"`
	QR          string   `json:"qr"`
	Flags       []string `json:"flags"`
	CheckedInAt string   `json:"checkedInAt"`
}

// CheckinResult is the answer of a check-in: one code for the family and the
// label of each child. The code and the QR tokens are shown only here.
type CheckinResult struct {
	Event     string         `json:"event"`
	Household string         `json:"household"`
	Code      string         `json:"code"`
	Labels    []CheckinLabel `json:"labels"`
}

var checkinQREncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

func newCheckinQRToken() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	return strings.ToLower(checkinQREncoding.EncodeToString(b)), nil
}

// CheckIn creates one session per child and returns the family code and the
// labels. The caller must be an active guardian of the household, the event
// must be open and belong to the household's org, and each child must be an
// active minor of the household who is not already in.
func CheckIn(app App, ctx context.Context, in CheckinInput) (*CheckinResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	if !CheckinAvailable(app) {
		return nil, orgErr(404, "Check-in is not available.")
	}

	key, err := checkinKey(app)
	if err != nil {
		return nil, orgErr(503, err.Error())
	}

	now := time.Now()

	if in.Event == nil || !checkinEventLive(in.Event, now) {
		return nil, orgErr(409, "The event is not open for check-in.")
	}

	household, err := app.FindRecordById(CollectionNameHouseholds, in.Household)
	if err != nil || household.GetString("org") != in.Event.GetString("org") || household.GetString("status") != HouseholdStatusActive {
		return nil, orgErr(403, "You may not check this household in.")
	}

	org, err := app.FindRecordById(CollectionNameOrgs, household.GetString("org"))
	if err != nil || org.GetBool("archived") {
		return nil, orgErr(403, "You may not check this household in.")
	}

	if in.Actor.Kiosk != nil && !KioskMayServe(in.Actor.Kiosk, in.Event) {
		return nil, orgErr(403, "This kiosk may not serve this event.")
	}

	guardian := CheckinGuardianMember(app, household.Id, userID(in.Guardian))
	if guardian == nil {
		return nil, orgErr(403, "Only a guardian of the household can check a child in.")
	}

	if len(in.Children) == 0 {
		return nil, orgErr(400, "Choose at least one child.")
	}

	seen := map[string]bool{}

	var children []*Record

	for _, id := range in.Children {
		if seen[id] {
			continue
		}

		seen[id] = true

		c, err := app.FindRecordById(CollectionNameHouseholdMembers, id)
		if err != nil || c.GetString("household") != household.Id || c.GetString("status") != HouseholdMemberActive || !c.GetBool("isMinor") {
			return nil, orgErr(400, "Only an active child of the household can be checked in.")
		}

		children = append(children, c)
	}

	code, codeHash, err := familyCheckinCode(app, key, org, in.Event.Id, household.Id)
	if err != nil {
		return nil, err
	}

	col, err := app.FindCachedCollectionByNameOrId(CheckinSessionCollection)
	if err != nil {
		return nil, err
	}

	out := &CheckinResult{Event: in.Event.Id, Household: household.Id, Code: code}
	auditCtx := checkinAuditContext(ctx, in.Actor, org)
	purge := checkinPurgeAfter(org, now)

	err = app.RunInTransaction(func(txApp App) error {
		for _, c := range children {
			if n, _ := txApp.CountRecords(CheckinSessionCollection,
				dbx.HashExp{"event": in.Event.Id, "child": c.Id, "state": CheckinStateIn}); n > 0 {
				return orgErr(409, "A child is already checked in to this event.")
			}

			token, err := newCheckinQRToken()
			if err != nil {
				return err
			}

			s := NewRecord(col)
			s.Id = GenerateDefaultRandomId()
			s.Set("event", in.Event.Id)
			s.Set("org", org.Id)
			s.Set("household", household.Id)
			s.Set("child", c.Id)
			s.Set("checkedInBy", guardian.Id)
			s.Set("checkedInAt", types.NowDateTime())
			s.Set("codeHash", codeHash)
			s.Set("qrHash", checkinQRHash(key, s.Id, token))
			s.Set("state", CheckinStateIn)
			flags := CheckinFlagsOf(c)
			s.Set("flags", flags)
			s.Set("failedAttempts", 0)
			s.Set("purgeAfter", purge)

			if err := txApp.SaveWithContext(auditCtx, s); err != nil {
				return wrapOrgSaveErr(err)
			}

			if err := writeCheckinLog(txApp, auditCtx, checkinLogEntry{
				Org: org, Event: in.Event.Id, Session: s.Id, Action: "check_in", Result: "ok", Actor: in.Actor,
				Detail: map[string]any{"household": household.Id, "child": c.Id, "guardian": guardian.Id},
			}); err != nil {
				return err
			}

			out.Labels = append(out.Labels, CheckinLabel{
				Session: s.Id, Child: c.Id, ChildName: c.GetString("name"), Code: code,
				QR: s.Id + "." + token, Flags: flags, CheckedInAt: s.GetDateTime("checkedInAt").String(),
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func userID(r *Record) string {
	if r == nil {
		return ""
	}

	return r.Id
}

// familyCheckinCode returns the one code of the household at the event and its
// HMAC. A family that already has a session at the event keeps its code; a new
// family gets a code no other household of the event uses.
func familyCheckinCode(app App, key []byte, org *Record, event, household string) (string, string, error) {
	length := CheckinCodeLength(org)

	if existing, err := app.FindFirstRecordByFilter(CheckinSessionCollection, "event={:e} && household={:h}",
		dbx.Params{"e": event, "h": household}); err == nil {
		want := existing.GetString("codeHash")

		for l := 4; l <= 6; l++ {
			for n := 0; n < 64; n++ {
				code := deriveCheckinCode(key, event, household, l, n)
				if subtle.ConstantTimeCompare([]byte(checkinCodeHash(key, event, code)), []byte(want)) == 1 {
					return code, want, nil
				}
			}
		}
	}

	for n := 0; n < 256; n++ {
		code := deriveCheckinCode(key, event, household, length, n)
		hash := checkinCodeHash(key, event, code)

		if c, _ := app.CountRecords(CheckinSessionCollection,
			dbx.NewExp("[[event]] = {:e} AND [[codeHash]] = {:c} AND [[household]] != {:h}",
				dbx.Params{"e": event, "c": hash, "h": household})); c == 0 {
			return code, hash, nil
		}
	}

	return "", "", orgErr(409, "No free pickup code is left for this event.")
}

// ---- release -------------------------------------------------------------------

// ReleaseInput is a release request. With neither Code nor QR nor Override, the
// release is the guardian's own, by a fresh passkey (Assertion).
type ReleaseInput struct {
	SessionId  string
	Code       string
	QR         string
	ReleasedTo string
	Override   bool
	Reason     string
	// Assertion is set by the route when the caller's token carries a fresh
	// passkey assertion (the step-up ceremony, 5 minutes).
	Assertion bool
	Actor     CheckinActor
}

// ErrCheckinBlocked is returned when a do-not-release flag stops the release.
var ErrCheckinBlocked = orgErr(403, "This child cannot be released now. Ask a coordinator.")

// ReleaseSession hands a child over. Every refusal of the code check is the
// same ErrCheckinDenied, whether the session is unknown, locked, dead or the
// code is wrong.
func ReleaseSession(app App, ctx context.Context, in ReleaseInput) (*Record, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	key, err := checkinKey(app)
	if err != nil {
		return nil, orgErr(503, err.Error())
	}

	s, err := app.FindRecordById(CheckinSessionCollection, in.SessionId)
	if err != nil {
		return nil, ErrCheckinDenied
	}

	org, err := app.FindRecordById(CollectionNameOrgs, s.GetString("org"))
	if err != nil || org.GetBool("archived") {
		return nil, ErrCheckinDenied
	}

	event, err := app.FindRecordById(CheckinEventsCollection, s.GetString("event"))
	if err != nil {
		return nil, ErrCheckinDenied
	}

	auditCtx := checkinAuditContext(ctx, in.Actor, org)
	now := time.Now()
	logEntry := func(action, method, result string, detail map[string]any) {
		if err := writeCheckinLog(app, auditCtx, checkinLogEntry{
			Org: org, Event: event.Id, Session: s.Id, Action: action, Method: method, Result: result, Actor: in.Actor, Detail: detail,
		}); err != nil {
			app.Logger().Warn("Failed to write the check-in log", "error", err.Error())
		}
	}

	// who is calling, and what they may do in this org
	standing := CheckinStanding{}

	switch {
	case in.Actor.Kiosk != nil:
		if in.Actor.Kiosk.GetString("org") != org.Id || !KioskMayServe(in.Actor.Kiosk, event) {
			return nil, ErrCheckinDenied
		}

		standing.Staff = true // a registered kiosk verifies codes; it cannot override
	case in.Actor.User != nil:
		standing = CheckinStandingOf(app, org, in.Actor.User)
	default:
		return nil, ErrCheckinDenied
	}

	if s.GetString("state") != CheckinStateIn {
		return nil, ErrCheckinDenied
	}

	method := ""
	releasedTo := strings.TrimSpace(in.ReleasedTo)

	if len(releasedTo) > 200 {
		releasedTo = releasedTo[:200]
	}

	var identities []string

	switch {
	case in.Override:
		if in.Actor.User == nil || !standing.Supervisor {
			return nil, ErrCheckinDenied
		}

		reason := strings.TrimSpace(in.Reason)
		if len(reason) < 10 || len(reason) > checkinReasonMax {
			return nil, orgErr(400, "An override needs a typed reason of at least 10 characters.")
		}

		if releasedTo == "" {
			return nil, orgErr(400, "Say who the child is released to.")
		}

		method = CheckinMethodOverride
	case in.Code != "" || in.QR != "":
		if !standing.Staff {
			return nil, ErrCheckinDenied
		}

		if !checkinEventLive(event, now) {
			logEntry("release_denied", CheckinMethodCode, "denied", map[string]any{"reason": "event_over"})

			return nil, ErrCheckinDenied
		}

		method = CheckinMethodCode

		if err := verifyCheckinCode(app, auditCtx, key, org, event, s, in, logEntry); err != nil {
			return nil, err
		}

		if in.QR != "" {
			method = CheckinMethodQR
		}
	default:
		// the guardian's own release, by a fresh passkey
		if in.Actor.User == nil || !in.Assertion {
			return nil, ErrCheckinDenied
		}

		if !checkinEventLive(event, now) {
			return nil, ErrCheckinDenied
		}

		method = CheckinMethodPasskey

		if g := CheckinGuardianMember(app, s.GetString("household"), in.Actor.User.Id); g != nil {
			releasedTo = g.Id
			identities = append(identities, g.Id, g.GetString("name"))
		} else if a := CheckinAuthorizedPickup(app, s.GetString("child"), in.Actor.User.Id, now); a != nil {
			releasedTo = a.GetString("name")
			identities = append(identities, a.Id, a.GetString("name"))
		} else {
			logEntry("release_denied", method, "denied", map[string]any{"reason": "not_authorized"})

			return nil, ErrCheckinDenied
		}

		identities = append(identities, in.Actor.User.Id)
	}

	// do-not-release: a route check, not a rule; it stops every method, an override included
	identities = append(identities, releasedTo)

	if m, err := app.FindRecordById(CollectionNameHouseholdMembers, releasedTo); err == nil {
		identities = append(identities, m.Id, m.GetString("name"), m.GetString("user"))
	}

	if blocked, flag := checkinReleaseBlocked(app, s, identities); blocked {
		logEntry("release_blocked", method, "denied", map[string]any{"flag": flag})
		raiseCheckinAlert(app, auditCtx, "do_not_release", org, s, in.Actor)

		return nil, ErrCheckinBlocked
	}

	fresh, err := app.FindRecordById(CheckinSessionCollection, s.Id)
	if err != nil || fresh.GetString("state") != CheckinStateIn {
		return nil, ErrCheckinDenied
	}

	fresh.Set("state", CheckinStateOut)
	fresh.Set("releasedTo", releasedTo)
	fresh.Set("releasedBy", checkinReleaser(in.Actor))
	fresh.Set("releasedAt", types.NowDateTime())
	fresh.Set("method", method)
	fresh.Set("failedAttempts", 0)
	fresh.Set("lockedAt", "")

	err = app.RunInTransaction(func(txApp App) error {
		if err := txApp.SaveWithContext(auditCtx, fresh); err != nil {
			return wrapOrgSaveErr(err)
		}

		detail := map[string]any{"releasedTo": releasedTo}
		if method == CheckinMethodOverride {
			detail["override"] = true
			detail["reason"] = strings.TrimSpace(in.Reason)
		}

		return writeCheckinLog(txApp, auditCtx, checkinLogEntry{
			Org: org, Event: event.Id, Session: s.Id, Action: "release", Method: method, Result: "ok", Actor: in.Actor, Detail: detail,
		})
	})
	if err != nil {
		return nil, err
	}

	if method == CheckinMethodOverride {
		raiseCheckinAlert(app, auditCtx, "override", org, fresh, in.Actor)
		notifyCheckinGuardians(app, org, fresh, "override")
	}

	return fresh, nil
}

func checkinReleaser(a CheckinActor) string {
	if a.User != nil {
		return a.User.Id
	}

	return ""
}

// verifyCheckinCode reserves one of the five tries, then compares in constant
// time. The reservation comes first (an atomic conditional update), so parallel
// guesses cannot get more than five comparisons.
func verifyCheckinCode(app App, ctx context.Context, key []byte, org, event, s *Record, in ReleaseInput,
	logEntry func(action, method, result string, detail map[string]any)) error {
	res, err := app.DB().NewQuery("UPDATE {{" + CheckinSessionCollection + "}} SET [[failedAttempts]] = COALESCE([[failedAttempts]], 0) + 1 " +
		"WHERE [[id]] = {:id} AND COALESCE([[failedAttempts]], 0) < {:max}").
		Bind(dbx.Params{"id": s.Id, "max": CheckinMaxAttempts}).Execute()
	if err != nil {
		return err
	}

	if n, _ := res.RowsAffected(); n == 0 {
		logEntry("release_denied", CheckinMethodCode, "denied", map[string]any{"reason": "locked"})

		return ErrCheckinDenied
	}

	var ok bool

	if in.QR != "" {
		token := in.QR
		if i := strings.LastIndex(token, "."); i >= 0 {
			token = token[i+1:]
		}

		given := checkinQRHash(key, s.Id, strings.ToLower(strings.TrimSpace(token)))
		ok = subtle.ConstantTimeCompare([]byte(given), []byte(s.GetString("qrHash"))) == 1
	} else {
		given := checkinCodeHash(key, event.Id, in.Code)
		ok = subtle.ConstantTimeCompare([]byte(given), []byte(s.GetString("codeHash"))) == 1
	}

	if ok {
		return nil
	}

	fresh, err := app.FindRecordById(CheckinSessionCollection, s.Id)
	if err == nil && fresh.GetInt("failedAttempts") >= CheckinMaxAttempts && fresh.GetDateTime("lockedAt").IsZero() {
		if _, err := app.DB().NewQuery("UPDATE {{" + CheckinSessionCollection + "}} SET [[lockedAt]] = {:now} WHERE [[id]] = {:id}").
			Bind(dbx.Params{"now": types.NowDateTime().String(), "id": s.Id}).Execute(); err != nil {
			app.Logger().Warn("Failed to lock a check-in session", "error", err.Error())
		}

		logEntry("locked", CheckinMethodCode, "denied", map[string]any{"attempts": CheckinMaxAttempts})
		raiseCheckinAlert(app, ctx, "locked", org, s, in.Actor)

		return ErrCheckinDenied
	}

	logEntry("release_denied", CheckinMethodCode, "denied", map[string]any{"reason": "mismatch"})

	return ErrCheckinDenied
}

// ---- do-not-release -------------------------------------------------------------

// SetCheckinFlag creates a do-not-release flag on a child. people names who may
// not take the child (a name, a household member id or a user id); none means
// nobody may until the flag is lifted. The reason is encrypted text.
func SetCheckinFlag(app App, ctx context.Context, setter *Record, childId string, people []string, reason string) (*Record, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	child, err := app.FindRecordById(CollectionNameHouseholdMembers, childId)
	if err != nil {
		return nil, orgErr(404, "No such child.")
	}

	h, err := app.FindRecordById(CollectionNameHouseholds, child.GetString("household"))
	if err != nil {
		return nil, orgErr(404, "No such child.")
	}

	org, err := app.FindRecordById(CollectionNameOrgs, h.GetString("org"))
	if err != nil {
		return nil, orgErr(404, "No such child.")
	}

	if !CheckinStandingOf(app, org, setter).Supervisor {
		return nil, orgErr(403, "Only a check-in coordinator can set a do-not-release flag.")
	}

	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > checkinReasonMax {
		return nil, orgErr(400, "A reason is required (up to 1000 characters).")
	}

	clean := []string{}

	for _, p := range people {
		if p = strings.TrimSpace(p); p != "" {
			clean = append(clean, p)
		}
	}

	col, err := app.FindCachedCollectionByNameOrId(CheckinFlagsCollection)
	if err != nil {
		return nil, orgErr(404, "Check-in is not available.")
	}

	rec := NewRecord(col)
	rec.Set("org", org.Id)
	rec.Set("household", h.Id)
	rec.Set("child", child.Id)
	rec.Set("people", clean)
	rec.Set("reason", reason)
	rec.Set("active", true)
	rec.Set("setBy", setter.Id)

	if err := app.SaveWithContext(ctx, rec); err != nil {
		return nil, wrapOrgSaveErr(err)
	}

	return rec, nil
}

// LiftCheckinFlag ends a flag (it stays as evidence).
func LiftCheckinFlag(app App, ctx context.Context, by *Record, id string) (*Record, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	rec, err := app.FindRecordById(CheckinFlagsCollection, id)
	if err != nil {
		return nil, orgErr(404, "No such flag.")
	}

	org, err := app.FindRecordById(CollectionNameOrgs, rec.GetString("org"))
	if err != nil || !CheckinStandingOf(app, org, by).Supervisor {
		return nil, orgErr(404, "No such flag.")
	}

	rec.Set("active", false)

	if err := app.SaveWithContext(ctx, rec); err != nil {
		return nil, wrapOrgSaveErr(err)
	}

	return rec, nil
}

// checkinReleaseBlocked reports whether an active flag of the child names one
// of the identities, or names nobody (a flag with no names blocks everyone).
// When the flag names people and the releaser is not known at all, the release
// is blocked too: the check cannot be answered.
func checkinReleaseBlocked(app App, s *Record, identities []string) (bool, string) {
	flags, err := app.FindRecordsByFilter(CheckinFlagsCollection, "child={:c} && active=true", "", 0, 0,
		dbx.Params{"c": s.GetString("child")})
	if err != nil || len(flags) == 0 {
		return false, ""
	}

	var have []string

	for _, id := range identities {
		if id = strings.ToLower(strings.TrimSpace(id)); id != "" {
			have = append(have, id)
		}
	}

	for _, f := range flags {
		var people []string

		_ = f.UnmarshalJSONField("people", &people)

		if len(people) == 0 || len(have) == 0 {
			return true, f.Id
		}

		for _, p := range people {
			if slices.Contains(have, strings.ToLower(strings.TrimSpace(p))) {
				return true, f.Id
			}
		}
	}

	return false, ""
}

// ---- incident hold -------------------------------------------------------------

// PlaceCheckinIncidentHold marks the session as an incident and puts an active
// legal hold (#88) on it and on every log row of it, so that neither the
// retention job nor anything else deletes them. Lifting is done through the
// legal holds (/api/gdpr/holds).
func PlaceCheckinIncidentHold(app App, ctx context.Context, by *Record, sessionId, reason string) (*Record, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	if !LegalHoldsAvailable(app) {
		return nil, orgErr(409, "Legal holds are not available.")
	}

	s, err := app.FindRecordById(CheckinSessionCollection, sessionId)
	if err != nil {
		return nil, orgErr(404, "No such session.")
	}

	org, err := app.FindRecordById(CollectionNameOrgs, s.GetString("org"))
	if err != nil || !CheckinStandingOf(app, org, by).Supervisor {
		return nil, orgErr(404, "No such session.")
	}

	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, orgErr(400, "A reason is required.")
	}

	logs, _ := app.FindRecordsByFilter(CheckinLogCollection, "session={:s}", "", 0, 0, dbx.Params{"s": s.Id})

	err = app.RunInTransaction(func(txApp App) error {
		s.Set("incident", true)

		if err := txApp.SaveWithContext(ctx, s); err != nil {
			return wrapOrgSaveErr(err)
		}

		targets := []struct{ col, id string }{{CheckinSessionCollection, s.Id}}
		for _, l := range logs {
			targets = append(targets, struct{ col, id string }{CheckinLogCollection, l.Id})
		}

		for _, t := range targets {
			if _, err := PlaceLegalHold(txApp, ctx, by, LegalHoldInput{
				Scope: LegalHoldScopeRecord, Collection: t.col, RecordId: t.id,
				Reason: reason, Reference: "checkin-incident:" + s.Id,
			}); err != nil {
				return err
			}
		}

		return writeCheckinLog(txApp, ctx, checkinLogEntry{
			Org: org, Event: s.GetString("event"), Session: s.Id, Action: "incident_hold", Result: "ok",
			Actor: CheckinActor{User: by}, Detail: map[string]any{"rows": len(logs) + 1},
		})
	})
	if err != nil {
		return nil, err
	}

	return s, nil
}

// holdNewCheckinLog puts the hold on a log row written after an incident hold
// on its session. Best effort: the guard on the table already forbids edits.
func holdNewCheckinLog(app App, ctx context.Context, s *Record, row *Record) {
	if !s.GetBool("incident") || !LegalHoldsAvailable(app) {
		return
	}

	if _, err := PlaceLegalHold(app, ctx, nil, LegalHoldInput{
		Scope: LegalHoldScopeRecord, Collection: CheckinLogCollection, RecordId: row.Id,
		Reason: "incident hold of session " + s.Id, Reference: "checkin-incident:" + s.Id,
	}); err != nil {
		app.Logger().Warn("Failed to hold a check-in log row", "error", err.Error())
	}
}

// ---- alerts -------------------------------------------------------------------

// raiseCheckinAlert writes the audit entry (a webhook event, checkin.alert,
// with ids only), the log row, and emails the org's coordinators. Best effort:
// an alert that cannot be sent never undoes the refusal that caused it.
func raiseCheckinAlert(app App, ctx context.Context, kind string, org, s *Record, actor CheckinActor) {
	entry := &AuditEntry{
		Action:         AuditActionCheckinAlert,
		CollectionId:   s.Collection().Id,
		CollectionName: CheckinSessionCollection,
		RecordId:       s.Id,
		ChangedFields:  []string{},
		Meta:           map[string]any{"kind": kind, "orgId": org.Id, "event": s.GetString("event"), "session": s.Id},
	}
	applyAuditContext(ctx, entry)

	if err := WriteAuditEntry(app, ctx, entry, nil); err != nil {
		app.Logger().Warn("Failed to write a check-in alert", "error", err.Error())
	}

	if err := writeCheckinLog(app, ctx, checkinLogEntry{
		Org: org, Event: s.GetString("event"), Session: s.Id, Action: "alert", Result: kind, Actor: actor,
	}); err != nil {
		app.Logger().Warn("Failed to log a check-in alert", "error", err.Error())
	}

	if CheckinNotifier == nil {
		return
	}

	to := checkinCoordinators(app, org)
	if len(to) == 0 {
		return
	}

	if _, err := CheckinNotifier(app, &CheckinNotice{Kind: kind, Audience: "staff", Org: org, Session: s, Recipients: to}); err != nil {
		app.Logger().Warn("Failed to email a check-in alert", "error", err.Error())
	}
}

// checkinCoordinators are the org's owners, admins and supervisor-tagged
// members who have a usable address.
func checkinCoordinators(app App, org *Record) []*Record {
	members, err := app.FindRecordsByFilter(CollectionNameOrgMembers, "org={:o} && status='active'", "created", 0, 0, dbx.Params{"o": org.Id})
	if err != nil {
		return nil
	}

	userCol, err := OrgUserCollection(app)
	if err != nil {
		return nil
	}

	var out []*Record

	for _, m := range members {
		role := m.GetString("role")
		if role != OrgRoleOwner && role != OrgRoleAdmin &&
			!(role == OrgRoleMember && slices.Contains(OrgTagNames(app, m), CheckinSupervisorTag)) {
			continue
		}

		if u, err := app.FindRecordById(userCol, m.GetString("user")); err == nil && usableEmail(u) {
			out = append(out, u)
		}
	}

	return out
}

// notifyCheckinGuardians tells the household's guardians with an account.
func notifyCheckinGuardians(app App, org, s *Record, kind string) {
	if CheckinNotifier == nil {
		return
	}

	rows, err := app.FindRecordsByFilter(CollectionNameHouseholdMembers, "household={:h} && guardian=true && status='active' && user!=''",
		"", 0, 0, dbx.Params{"h": s.GetString("household")})
	if err != nil {
		return
	}

	userCol, err := OrgUserCollection(app)
	if err != nil {
		return
	}

	var to []*Record

	for _, m := range rows {
		if u, err := app.FindRecordById(userCol, m.GetString("user")); err == nil && usableEmail(u) {
			to = append(to, u)
		}
	}

	if len(to) == 0 {
		return
	}

	if _, err := CheckinNotifier(app, &CheckinNotice{Kind: kind, Audience: "guardians", Org: org, Session: s, Recipients: to}); err != nil {
		app.Logger().Warn("Failed to tell the guardians about an override", "error", err.Error())
	}
}
