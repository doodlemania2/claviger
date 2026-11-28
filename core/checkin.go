package core

import (
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #82): secure children's check-in and pickup.
//
// Core holds the primitives: the template collections and their rules, the
// pickup codes, guardian matching, the encrypted do-not-release flags, the
// kiosk principal and the append-only log. The kiosk screen and label printing
// are an app on top. See docs/checkin.md and docs/specs/80-88-congregations.md
// section 7.
//
// There is no `templates install` command in this codebase: like pastoral
// notes, the template is installed by a superuser through
// POST /api/checkin/install (CreateCheckinTemplates).

const (
	CollectionNameKiosks = "_kiosks"

	CheckinEventsCollection  = "checkin_events"
	CheckinSessionCollection = "checkin_sessions"
	PickupAuthCollection     = "pickup_authorizations"
	CheckinLogCollection     = "checkin_log"
	CheckinFlagsCollection   = "checkin_flags"

	// CheckinStaffTag lets an org member (role member) release a child by code
	// and read the sessions of the org; CheckinSupervisorTag adds the override,
	// the do-not-release flags and the incident hold. Owners and admins are
	// both without a tag.
	CheckinStaffTag      = "checkin"
	CheckinSupervisorTag = "checkin-supervisor"

	CheckinEventOpen   = "open"
	CheckinEventClosed = "closed"

	CheckinStateIn   = "in"
	CheckinStateOut  = "out"
	CheckinStateVoid = "void"

	CheckinMethodCode     = "code"
	CheckinMethodQR       = "qr"
	CheckinMethodPasskey  = "passkey"
	CheckinMethodOverride = "override"

	PickupStatusActive  = "active"
	PickupStatusRevoked = "revoked"

	KioskActive  = "active"
	KioskRevoked = "revoked"

	KioskTokenPrefix = "clvk_"

	// audit actions; AuditActionCheckinAlert is the webhook event checkin.alert
	// (ids only, never a name, a code or a reason). Check-ins and releases are
	// ordinary record audit entries of the session.
	AuditActionCheckinAlert = "checkin_alert"
	AuditActionKioskCreate  = "kiosk_register"
	AuditActionKioskRevoke  = "kiosk_revoke"

	// org settings (in _orgs.settings)
	CheckinRetentionSetting   = "checkinRetentionMonths"
	DefaultCheckinRetention   = 12
	CheckinCodeLengthSetting  = "checkinCodeLength"
	DefaultCheckinCodeLength  = 5
	KioskSessionSetting       = "kioskSessionMinutes"
	DefaultKioskSessionMinute = 5

	// CheckinMaxAttempts wrong codes lock a session.
	CheckinMaxAttempts = 5

	// CheckinCodeAlphabet has no 0/O, 1/I/L or other look-alikes.
	CheckinCodeAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

	// CheckinKeyEnv is the server key of the pickup codes (HMAC). Without it
	// the key is derived from the settings encryption key; with neither, check-in
	// refuses to run.
	CheckinKeyEnv = "CLAVIGER_CHECKIN_KEY"

	checkinReasonMax = 1000
)

// KioskStatuses are the values of the kiosk status field.
var KioskStatuses = []string{KioskActive, KioskRevoked}

var (
	CheckinEventStates = []string{CheckinEventOpen, CheckinEventClosed}
	CheckinStates      = []string{CheckinStateIn, CheckinStateOut, CheckinStateVoid}
	CheckinMethods     = []string{CheckinMethodCode, CheckinMethodQR, CheckinMethodPasskey, CheckinMethodOverride}
	PickupStatuses     = []string{PickupStatusActive, PickupStatusRevoked}

	// CheckinTags are the org tags the template uses.
	CheckinTags = []string{CheckinStaffTag, CheckinSupervisorTag}

	// ErrCheckinNoKey is returned when no server key is configured.
	ErrCheckinNoKey = errors.New("check-in needs a server key: set " + CheckinKeyEnv + " (or the settings encryption key)")

	// ErrCheckinDenied is the generic failure of a release: the same answer for
	// a wrong code, a locked session, an unknown session and a dead code.
	ErrCheckinDenied = orgErr(403, "The code could not be verified.")
)

// CheckinAvailable reports whether the check-in template is installed.
func CheckinAvailable(app App) bool {
	_, err := app.FindCachedCollectionByNameOrId(CheckinSessionCollection)

	return err == nil
}

// KiosksAvailable reports whether the _kiosks collection exists.
func KiosksAvailable(app App) bool {
	_, err := app.FindCachedCollectionByNameOrId(CollectionNameKiosks)

	return err == nil
}

// ---- server key and codes ---------------------------------------------------

func checkinKey(app App) ([]byte, error) {
	if v := strings.TrimSpace(os.Getenv(CheckinKeyEnv)); len(v) >= 16 {
		return hkdf.Key(sha256.New, []byte(v), nil, "claviger/checkin/v1", 32)
	}

	if name := app.EncryptionEnv(); name != "" {
		if v := os.Getenv(name); v != "" {
			return hkdf.Key(sha256.New, []byte(v), nil, "claviger/checkin/v1", 32)
		}
	}

	return nil, ErrCheckinNoKey
}

// checkinMAC is the keyed hash of a value in a domain.
func checkinMAC(key []byte, domain string, parts ...string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(domain))

	for _, p := range parts {
		mac.Write([]byte{0})
		mac.Write([]byte(p))
	}

	return hex.EncodeToString(mac.Sum(nil))
}

// NormalizeCheckinCode upper-cases the code and drops spaces and dashes (the
// label prints it in groups).
func NormalizeCheckinCode(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))

	return strings.NewReplacer(" ", "", "-", "", "_", "").Replace(code)
}

// deriveCheckinCode is the family code of the household at the event, attempt n.
// It is derived, not stored, so a second child of the family checked in later
// gets the same code, and only its HMAC is kept.
func deriveCheckinCode(key []byte, event, household string, length, n int) string {
	sum := checkinMAC(key, "code", event, household, strconv.Itoa(length), strconv.Itoa(n))
	raw, _ := hex.DecodeString(sum)

	out := make([]byte, length)
	for i := range out {
		out[i] = CheckinCodeAlphabet[int(raw[i])%len(CheckinCodeAlphabet)]
	}

	return string(out)
}

func checkinCodeHash(key []byte, event, code string) string {
	return checkinMAC(key, "codehash", event, NormalizeCheckinCode(code))
}

func checkinQRHash(key []byte, session, token string) string {
	return checkinMAC(key, "qrhash", session, token)
}

// CheckinCodeLength is the printed code length of the org (4 to 6).
func CheckinCodeLength(org *Record) int {
	return orgSettingInt(org, CheckinCodeLengthSetting, DefaultCheckinCodeLength, 4, 6)
}

// CheckinRetentionMonths is how long the org keeps check-in rows.
func CheckinRetentionMonths(org *Record) int {
	return orgSettingInt(org, CheckinRetentionSetting, DefaultCheckinRetention, 1, 240)
}

// KioskSessionTTL is the life of a kiosk session of the org.
func KioskSessionTTL(org *Record) time.Duration {
	return time.Duration(orgSettingInt(org, KioskSessionSetting, DefaultKioskSessionMinute, 1, 15)) * time.Minute
}

// ---- rules -------------------------------------------------------------------

// CheckinStaffRule is the condition "the user is staff of the org at field":
// an owner or admin, or a member who carries one of the tags.
func CheckinStaffRule(orgField string, tags ...string) string {
	admin := OrgMemberRule(orgField, OrgRoleOwner, OrgRoleAdmin)

	var parts []string

	for _, t := range tags {
		if r, err := OrgTagRule(orgField, t); err == nil {
			parts = append(parts, "("+r+")")
		}
	}

	if len(parts) == 0 {
		return "(" + admin + ")"
	}

	return fmt.Sprintf(`(%s) || ((%s) && %s._orgMembers_via_org.role ?= "%s")`,
		admin, strings.Join(parts, " || "), orgField, OrgRoleMember)
}

// CheckinRules are the rule presets of the five template collections, by name.
// Sessions, the log and the do-not-release flags are written by the routes
// only (null create, update and delete rules).
func CheckinRules() map[string]OrgRuleSet {
	str := func(s string) *string { return types.Pointer(s) }

	staff := CheckinStaffRule("org", CheckinStaffTag, CheckinSupervisorTag)
	supervisor := CheckinStaffRule("org", CheckinSupervisorTag)
	admins := OrgMemberRule("org", OrgRoleOwner, OrgRoleAdmin)
	householdRead := fmt.Sprintf(`@request.auth.id != "" && ((%s) || (%s))`, HouseholdMemberRule("household"), staff)
	pickupWrite := fmt.Sprintf(`@request.auth.id != "" && ((%s) || (%s))`, HouseholdGuardianRule("household"), admins)
	orgRead := OrgMemberRule("org")
	staffRead := `@request.auth.id != "" && (` + staff + `)`
	supervisorRule := `@request.auth.id != "" && (` + supervisor + `)`

	return map[string]OrgRuleSet{
		CheckinEventsCollection: {
			List: str(orgRead), View: str(orgRead),
			Create: str(admins), Update: str(admins), Delete: str(admins),
		},
		CheckinSessionCollection: {List: str(householdRead), View: str(householdRead)},
		PickupAuthCollection: {
			List: str(householdRead), View: str(householdRead),
			Create: str(pickupWrite), Update: str(pickupWrite), Delete: str(pickupWrite),
		},
		CheckinLogCollection:   {List: str(staffRead), View: str(staffRead)},
		CheckinFlagsCollection: {List: str(supervisorRule), View: str(supervisorRule)},
	}
}

// ---- install -------------------------------------------------------------------

// CheckinInstall is the result of CreateCheckinTemplates.
type CheckinInstall struct {
	Collections []string `json:"collections"`
	Policies    []string `json:"retentionPolicies"`
}

// CreateCheckinTemplates creates the check-in collections with their rule
// presets, registers them in the data map (#88) and creates the retention
// policies (disabled and as a dry run, like every policy). The do-not-release
// reason is an encryptedText field, so a key-encryption key must be configured.
func CreateCheckinTemplates(app App, ctx context.Context) (*CheckinInstall, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	var out *CheckinInstall

	err := app.RunInTransaction(func(txApp App) error {
		var err error

		out, err = createCheckinTemplates(txApp, ctx)

		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func createCheckinTemplates(app App, ctx context.Context) (*CheckinInstall, error) {
	if !FieldEncryptionAvailable(app) {
		return nil, errors.New("check-in needs field encryption for the do-not-release reasons: configure a key-encryption key (CLAVIGER_KEK) and restart first")
	}

	if _, err := checkinKey(app); err != nil {
		return nil, err
	}

	if !HouseholdsAvailable(app) {
		return nil, errors.New("households are not available")
	}

	if !KiosksAvailable(app) {
		return nil, errors.New("the " + CollectionNameKiosks + " collection is not available")
	}

	for _, name := range []string{CheckinEventsCollection, CheckinSessionCollection, PickupAuthCollection, CheckinLogCollection, CheckinFlagsCollection} {
		if _, err := app.FindCollectionByNameOrId(name); err == nil {
			return nil, fmt.Errorf("a collection named %q already exists", name)
		}
	}

	orgs, err := app.FindCachedCollectionByNameOrId(CollectionNameOrgs)
	if err != nil {
		return nil, err
	}

	households, err := app.FindCachedCollectionByNameOrId(CollectionNameHouseholds)
	if err != nil {
		return nil, err
	}

	members, err := app.FindCachedCollectionByNameOrId(CollectionNameHouseholdMembers)
	if err != nil {
		return nil, err
	}

	userCol, err := OrgUserCollection(app)
	if err != nil {
		return nil, errors.New("the user collection is not available")
	}

	rules := CheckinRules()
	auto := func() []Field {
		return []Field{
			&AutodateField{Name: "created", OnCreate: true},
			&AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
		}
	}
	orgRel := func() Field {
		return &RelationField{Name: "org", Required: true, CollectionId: orgs.Id, MaxSelect: 1, CascadeDelete: true}
	}

	events := NewBaseCollection(CheckinEventsCollection)
	events.Fields.Add(
		orgRel(),
		&TextField{Name: "name", Required: true, Max: 200},
		&DateField{Name: "starts"},
		&DateField{Name: "ends"},
		&SelectField{Name: "status", Required: true, MaxSelect: 1, Values: CheckinEventStates},
	)
	events.Fields.Add(auto()...)
	events.AddIndex("idx_checkin_events_org", false, "org, status", "")

	sessions := NewBaseCollection(CheckinSessionCollection)

	if err := saveCheckinCollection(app, events, rules[CheckinEventsCollection]); err != nil {
		return nil, err
	}

	sessions.Fields.Add(
		&RelationField{Name: "event", Required: true, CollectionId: events.Id, MaxSelect: 1, CascadeDelete: true},
		orgRel(),
		&RelationField{Name: "household", Required: true, CollectionId: households.Id, MaxSelect: 1},
		&RelationField{Name: "child", Required: true, CollectionId: members.Id, MaxSelect: 1},
		// the id of the guardian's household member row, as text: the evidence outlives the row, and the data export reaches it by "checkedInBy~_householdMembers.user"
		&TextField{Name: "checkedInBy", Max: 100},
		&DateField{Name: "checkedInAt"},
		// HMAC-SHA256 with the server key; hidden from every API response and redacted in the audit log
		&TextField{Name: "codeHash", Hidden: true, Max: 64},
		&TextField{Name: "qrHash", Hidden: true, Max: 64},
		&SelectField{Name: "state", Required: true, MaxSelect: 1, Values: CheckinStates},
		&TextField{Name: "releasedTo", Max: 200},
		&TextField{Name: "releasedBy", Max: 100},
		&DateField{Name: "releasedAt"},
		&SelectField{Name: "method", MaxSelect: 1, Values: CheckinMethods},
		// plain markers only ("allergy", "medical"): the details stay encrypted on the member
		&JSONField{Name: "flags", MaxSize: 4 << 10},
		&NumberField{Name: "failedAttempts", OnlyInt: true, Min: types.Pointer(0.0)},
		&DateField{Name: "lockedAt"},
		&BoolField{Name: "incident"},
		&DateField{Name: "purgeAfter"},
	)
	sessions.Fields.Add(auto()...)
	sessions.AddIndex("idx_checkin_sessions_event_household", false, "event, household", "")
	sessions.AddIndex("idx_checkin_sessions_event_code", false, "event, codeHash", "")
	sessions.AddIndex("idx_checkin_sessions_child", true, "event, child", "`state` = 'in'")
	sessions.AddIndex("idx_checkin_sessions_purge", false, "purgeAfter", "")

	if err := saveCheckinCollection(app, sessions, rules[CheckinSessionCollection]); err != nil {
		return nil, err
	}

	pickups := NewBaseCollection(PickupAuthCollection)
	pickups.Fields.Add(
		orgRel(),
		&RelationField{Name: "household", Required: true, CollectionId: households.Id, MaxSelect: 1, CascadeDelete: true},
		&RelationField{Name: "child", Required: true, CollectionId: members.Id, MaxSelect: 1, CascadeDelete: true},
		&RelationField{Name: "user", CollectionId: userCol.Id, MaxSelect: 1},
		&TextField{Name: "name", Required: true, Max: 200},
		&TextField{Name: "relationship", Max: 100},
		&FileField{Name: "photo", Protected: true, MaxSelect: 1, MaxSize: 5 << 20,
			MimeTypes: []string{"image/png", "image/jpeg", "image/webp"}},
		&DateField{Name: "validUntil"},
		&RelationField{Name: "approvedBy", CollectionId: members.Id, MaxSelect: 1},
		&SelectField{Name: "status", Required: true, MaxSelect: 1, Values: PickupStatuses},
	)
	pickups.Fields.Add(auto()...)
	pickups.AddIndex("idx_pickup_authorizations_child", false, "child, status", "")

	if err := saveCheckinCollection(app, pickups, rules[PickupAuthCollection]); err != nil {
		return nil, err
	}

	logCol := NewBaseCollection(CheckinLogCollection)
	logCol.Fields.Add(
		orgRel(),
		// ids as text: the log outlives the rows it describes
		&TextField{Name: "event", Max: 100},
		&TextField{Name: "session", Max: 100},
		&TextField{Name: "action", Required: true, Max: 50},
		&TextField{Name: "actorType", Max: 20},
		&TextField{Name: "actorId", Max: 100},
		&TextField{Name: "method", Max: 20},
		&TextField{Name: "result", Max: 20},
		&JSONField{Name: "detail", MaxSize: 16 << 10},
		&DateField{Name: "purgeAfter"},
		&AutodateField{Name: "created", OnCreate: true},
	)
	logCol.AddIndex("idx_checkin_log_session", false, "session", "")
	logCol.AddIndex("idx_checkin_log_org", false, "org, created", "")
	logCol.AddIndex("idx_checkin_log_purge", false, "purgeAfter", "")

	if err := saveCheckinCollection(app, logCol, rules[CheckinLogCollection]); err != nil {
		return nil, err
	}

	flags := NewBaseCollection(CheckinFlagsCollection)
	flags.Fields.Add(
		orgRel(),
		&RelationField{Name: "household", Required: true, CollectionId: households.Id, MaxSelect: 1, CascadeDelete: true},
		&RelationField{Name: "child", Required: true, CollectionId: members.Id, MaxSelect: 1, CascadeDelete: true},
		// who may not take the child: names, household member ids or user ids; empty blocks everyone
		&JSONField{Name: "people", MaxSize: 16 << 10},
		&EncryptedTextField{Name: "reason", Required: true, Max: checkinReasonMax},
		&BoolField{Name: "active"},
		&TextField{Name: "setBy", Max: 100},
	)
	flags.Fields.Add(auto()...)
	flags.AddIndex("idx_checkin_flags_child", false, "child, active", "")

	if err := saveCheckinCollection(app, flags, rules[CheckinFlagsCollection]); err != nil {
		return nil, err
	}

	if err := registerCheckinDataMap(app); err != nil {
		return nil, err
	}

	out := &CheckinInstall{Collections: []string{
		CheckinEventsCollection, CheckinSessionCollection, PickupAuthCollection, CheckinLogCollection, CheckinFlagsCollection,
	}}

	if RetentionAvailable(app) {
		for _, p := range []struct{ name, collection string }{
			{"Check-in sessions", CheckinSessionCollection},
			{"Check-in log", CheckinLogCollection},
		} {
			rec, err := CreateRetentionPolicy(app, ctx, RetentionPolicyInput{
				Name: types.Pointer(p.name), Collection: types.Pointer(p.collection),
				Basis: types.Pointer("purgeAfter"), After: types.Pointer(0), Unit: types.Pointer(RetentionUnitDays),
				Action: types.Pointer(RetentionActionDelete),
			})
			if err != nil {
				return nil, fmt.Errorf("retention policy for %s: %w", p.collection, err)
			}

			out.Policies = append(out.Policies, rec.Id)
		}
	}

	return out, nil
}

func saveCheckinCollection(app App, col *Collection, r OrgRuleSet) error {
	col.ListRule, col.ViewRule = r.List, r.View
	col.CreateRule, col.UpdateRule, col.DeleteRule = r.Create, r.Update, r.Delete

	return app.Save(col)
}

func registerCheckinDataMap(app App) error {
	const retainReason = "Children's safeguarding record of who checked a child in and who took them home. Kept for the retention period (12 months by default, set per org) and for longer under an incident hold."

	for _, entry := range []DataMapEntry{
		{Collection: CheckinEventsCollection, Treatment: DataMapTreatmentNone,
			Note: "Check-in events (#82): a name and a time window, no personal data."},
		{Collection: CheckinSessionCollection, Treatment: DataMapTreatmentRetain,
			SubjectPaths: []string{"checkedInBy~" + CollectionNameHouseholdMembers + ".user"}, PersonalFields: []string{"releasedTo", "releasedBy", "flags"},
			AgeOutBasis: "checkedInAt", RetainReason: retainReason,
			Note: "Check-in sessions (#82). The code is stored only as an HMAC. Deleted by the retention policy once purgeAfter has passed, unless an incident hold applies."},
		{Collection: PickupAuthCollection, Treatment: DataMapTreatmentErase,
			SubjectPaths: []string{"user"}, PersonalFields: []string{"name", "relationship", "photo", "user"},
			Note: "People a guardian approved to pick a child up (#82), with a photo. Deleted when the person is erased."},
		{Collection: CheckinLogCollection, Treatment: DataMapTreatmentRetain,
			SubjectPaths: []string{"actorId"}, PersonalFields: []string{"actorId", "detail"},
			AgeOutBasis: "created", RetainReason: retainReason,
			Note: "Append-only check-in log (#82). Rows are never changed; the retention policy deletes them after purgeAfter."},
		{Collection: CheckinFlagsCollection, Treatment: DataMapTreatmentRetain,
			SubjectPaths: []string{"setBy"}, PersonalFields: []string{"people", "reason", "setBy"},
			RetainReason: "A safeguarding restriction on who may take a child home. Kept while it applies and as evidence afterwards.",
			Note:         "Do-not-release flags (#82). The reason is encrypted text."},
	} {
		if err := registerCheckinDataMapEntry(app, entry); err != nil {
			return err
		}
	}

	return nil
}

// registerCheckinDataMapEntry writes the custom (non built-in) _dataMap row.
func registerCheckinDataMapEntry(app App, entry DataMapEntry) error {
	col, err := app.FindCachedCollectionByNameOrId(CollectionNameDataMap)
	if err != nil {
		return nil
	}

	rec, err := app.FindFirstRecordByData(col, "collection", entry.Collection)
	if err != nil {
		rec = NewRecord(col)
	} else if rec.GetBool("builtin") {
		return nil
	}

	rec.Set("collection", entry.Collection)
	rec.Set("treatment", entry.Treatment)
	rec.Set("subjectPaths", nonNilStrings(entry.SubjectPaths))
	rec.Set("personalFields", nonNilStrings(entry.PersonalFields))
	rec.Set("retainReason", entry.RetainReason)
	rec.Set("ageOutBasis", entry.AgeOutBasis)
	rec.Set("note", entry.Note)
	rec.Set("builtin", false)

	return app.Save(rec)
}

// EnsureCheckinTags creates the two tags in the org when they are missing and
// returns the names it created.
func EnsureCheckinTags(app App, ctx context.Context, org *Record) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	var created []string

	for _, name := range CheckinTags {
		if _, err := app.FindFirstRecordByFilter(CollectionNameOrgRoles, "org={:org} && name={:name}",
			dbx.Params{"org": org.Id, "name": name}); err == nil {
			continue
		}

		col, err := app.FindCachedCollectionByNameOrId(CollectionNameOrgRoles)
		if err != nil {
			return created, err
		}

		tag := NewRecord(col)
		tag.Set("org", org.Id)
		tag.Set("name", name)

		if err := saveOrgRecord(app, ctx, tag); err != nil {
			return created, err
		}

		created = append(created, name)
	}

	return created, nil
}
