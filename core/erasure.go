package core

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/hook"
	"github.com/doodlemania2/claviger/tools/router"
	"github.com/doodlemania2/claviger/tools/security"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #88, 88-4): erasure of a person (GDPR Article 17).
//
// A request is a row of _dataRequests with type "erase". Its life:
//
//	awaiting_confirmation  the person (or the guardian) was emailed a link
//	grace                  confirmed; the account is locked (a ban with the
//	                       reason pending_erasure) and the request can be
//	                       cancelled by the link, by the person signing in with
//	                       a passkey, or by a superuser
//	queued                 the grace period is over (or was 0)
//	running                the job is working through the data map
//	done | partial         finished; partial when something was kept
//	                       (legal hold, permanent register, ...) or failed
//	failed                 refused at run time or out of retries
//	cancelled
//
// The job (erasure_run.go) walks the data map. Who can start one:
//
//   - self service: a signed-in user, for themselves (fresh passkey, or the
//     password when they have no passkey; then the emailed confirmation);
//   - a guardian, for a minor of their household (the minor has no login and
//     cannot ask; same steps, the guardian is emailed);
//   - a superuser (admin route), either with the emailed confirmation of the
//     person, or without it, recording how the identity was checked in meta.
//
// Audit entries: gdpr_erase_request, gdpr_erase_confirm, gdpr_erase_cancel and
// gdpr_erase (the result: ids and counts only). "gdpr_erase" is delivered as
// the webhook event "gdpr.erase". See docs/erasure.md.

const (
	CollectionNameErasureLog = "_erasureLog"

	AuditActionErasureRequest = "gdpr_erase_request"
	AuditActionErasureConfirm = "gdpr_erase_confirm"
	AuditActionErasureCancel  = "gdpr_erase_cancel"
	AuditActionErase          = "gdpr_erase"

	// BanReasonPendingErasure is the reason of the ban that locks an account
	// during the grace period.
	BanReasonPendingErasure = "pending_erasure"

	// BanReasonErased is the reason of the permanent ban of an account that
	// was erased: the stub row stays (other rows point at it) and cannot sign in.
	BanReasonErased = "erased"

	ErasureModeSelf     = "self"
	ErasureModeGuardian = "guardian"
	ErasureModeAdmin    = "admin"
	ErasureModeReplay   = "replay"

	erasureTokenLifetime = 7 * 24 * time.Hour
	erasureCronId        = "__clvErasureJob__"
	erasureCronExpr      = "*/10 * * * *"
	erasureMaxAttempts   = 5

	erasureIdentityCheckMax = 1000
)

// Codes of an ErasureError.
const (
	ErasureCodeActive         = "erasure_active"
	ErasureCodeSubject        = "erasure_subject"
	ErasureCodeLastGuardian   = "last_guardian"
	ErasureCodeLastOwner      = "last_owner"
	ErasureCodeNoEmail        = "no_email"
	ErasureCodeForbidden      = "forbidden"
	ErasureCodeIdentityCheck  = "identity_check_required"
	ErasureCodeMail           = "mail_failed"
	ErasureCodeToken          = "invalid_token"
	ErasureCodeState          = "invalid_state"
	ErasureCodeNotAvailable   = "not_available"
	ErasureCodeSuperuser      = "superuser"
	ErasureCodeLoginMember    = "has_login"
	ErasureCodeNotMinorMember = "not_minor"
)

// ErasureError is a refusal with a code a client can act on.
type ErasureError struct {
	Status  int
	Code    string
	Message string

	// Ids names the households or orgs that cause a refusal (never a person).
	Ids []string
}

func (e *ErasureError) Error() string { return e.Message }

func erasureErr(status int, code, msg string, ids ...string) *ErasureError {
	return &ErasureError{Status: status, Code: code, Message: msg, Ids: ids}
}

// ErasureNotice is what the mails package is asked to send.
type ErasureNotice struct {
	// Kind is "confirm" (the link to confirm), "grace" (the erasure was
	// confirmed: the date, and the link to cancel) or "result" (Article 12(4)).
	Kind string

	// Recipient is the person to write to: the subject, or the guardian who
	// asked for a minor. Subject is the person being erased.
	Recipient *Record
	Subject   *Record

	// OnBehalf is true when Recipient is a guardian acting for the subject.
	OnBehalf bool

	// Token is the confirmation token (confirm) or the cancel token (grace).
	Token string

	Expires    time.Time
	GraceUntil time.Time
	GraceDays  int

	// Result is set for Kind "result".
	Result *ErasureResult
}

// ErasureNotifier emails a notice. The mails package sets it. For Kind
// "result" it is called before the address is anonymized.
var ErasureNotifier func(app App, n *ErasureNotice) error

var (
	erasureRunMu sync.Mutex

	// erasureStepHook is a test seam: it is called before each step of the job
	// and may fail it, to simulate a crash.
	erasureStepHook func(req *Record, step string) error
)

// ErasureAvailable reports whether erasure requests can be made at all.
func ErasureAvailable(app App) bool {
	if _, err := app.FindCachedCollectionByNameOrId(CollectionNameErasureLog); err != nil {
		return false
	}

	col, err := app.FindCachedCollectionByNameOrId(CollectionNameDataRequests)

	return err == nil && col.Fields.GetByName("graceUntil") != nil
}

func erasureHash(s string) string {
	sum := sha256.Sum256([]byte(s))

	return hex.EncodeToString(sum[:])
}

// erasureSecretKey is the _params key of the per-install secret.
const erasureSecretKey = "erasureSecret"

const erasureSecretStoreKey = "__clvErasureSecret__"

var erasureSecretMu sync.Mutex

// ErrErasureSecretMissing is returned when the secret does not exist and may
// not be created (the replay).
var ErrErasureSecretMissing = errors.New("the erasure secret does not exist")

// erasureSecret returns the secret that keys the pseudonyms and the erasure
// log hashes: 32 random bytes, generated once per install and kept in the
// _params table of data.db (a hidden, server-written row: no collection, no
// route, no export and no settings response ever returns it, and it is not
// derived from PB_ENCRYPTION_KEY or the KEK, which are optional). It travels
// with the database through a backup and a restore, which the replay needs.
// With create it makes the secret when it is missing; without, a missing
// secret is ErrErasureSecretMissing.
func erasureSecret(app App, create bool) ([]byte, error) {
	if v, ok := app.Store().Get(erasureSecretStoreKey).([]byte); ok && len(v) > 0 {
		return v, nil
	}

	erasureSecretMu.Lock()
	defer erasureSecretMu.Unlock()

	if v, ok := app.Store().Get(erasureSecretStoreKey).([]byte); ok && len(v) > 0 {
		return v, nil
	}

	param := &Param{}

	err := app.ModelQuery(param).Model(erasureSecretKey, param)
	if err == nil && param.Id != "" {
		var enc string
		if json.Unmarshal(param.Value, &enc) == nil {
			if raw, derr := base64.StdEncoding.DecodeString(enc); derr == nil && len(raw) >= 32 {
				app.Store().Set(erasureSecretStoreKey, raw)

				return raw, nil
			}
		}

		return nil, errors.New("the erasure secret is stored but unreadable")
	}

	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	if !create {
		return nil, ErrErasureSecretMissing
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}

	value, _ := json.Marshal(base64.StdEncoding.EncodeToString(raw))

	param = &Param{Value: types.JSONRaw(value)}
	param.Id = erasureSecretKey
	param.MarkAsNew()

	// without hooks: nothing may audit, broadcast or log the value
	if err := app.UnsafeWithoutHooks().Save(param); err != nil {
		// another replica may have made it first
		if again := app.ModelQuery(&Param{}).Model(erasureSecretKey, &Param{}); again == nil {
			erasureSecretMu.Unlock()
			defer erasureSecretMu.Lock()

			return erasureSecret(app, false)
		}

		return nil, err
	}

	app.Store().Set(erasureSecretStoreKey, raw)

	return raw, nil
}

// EnsureErasureSecret creates the secret when it is missing.
func EnsureErasureSecret(app App) error {
	_, err := erasureSecret(app, true)

	return err
}

func erasureHMAC(app App, label string) string {
	key, err := erasureSecret(app, true)
	if err != nil {
		// callers make sure of the secret first (EnsureErasureSecret); a
		// pseudonym made with another key would silently break the replay
		panic("erasure secret unavailable: " + err.Error())
	}

	m := hmac.New(sha256.New, key)
	m.Write([]byte(label))

	return hex.EncodeToString(m.Sum(nil))
}

// ErasureSubjectHash is the value stored in _erasureLog: HMAC-SHA-256, under
// the per-install secret, of the collection name and the id of the subject.
// Without the secret it cannot be recomputed from an id.
func ErasureSubjectHash(app App, collection, id string) string {
	return erasureHMAC(app, "claviger-erasure-log:"+collection+":"+id)
}

// ErasurePseudonym is the stand-in for the id of an erased person in the
// evidence that stays (audit entries, consents, break-glass grants, ...):
// "erased-" and 24 hex characters of an HMAC-SHA-256 of the id under the
// per-install secret. Whoever holds an old id (a webhook receiver, a relying
// party) cannot compute it.
func ErasurePseudonym(app App, id string) string {
	return "erased-" + erasureHMAC(app, "claviger-pseudonym:"+id)[:24]
}

// IsErasedAccount reports whether the auth record is an erased stub.
func IsErasedAccount(r *Record) bool {
	return r != nil && r.Collection().IsAuth() &&
		r.Email() == "erased-"+r.Id+"@invalid"
}

func erasedMember(m *Record) bool {
	return m != nil && m.GetString("name") == "" && m.GetString("birthDate") == "" &&
		m.GetString("status") == HouseholdMemberLeft && m.GetString("user") == ""
}

func erasureUsableEmail(r *Record) bool {
	if r == nil || !r.Collection().IsAuth() {
		return false
	}

	e := r.Email()

	return e != "" && !strings.HasSuffix(e, "@invalid")
}

// ---- subjects ----------------------------------------------------------------

// ErasureSubjectInfo is who is erased: an auth record (a user) or a household
// member who has no login (a minor).
type erasureSubject struct {
	rec     *Record
	member  bool     // rec is a household member row
	ids     []string // the subject's own ids, and the ids of its household member rows
	members []*Record
}

func erasureLoadSubject(app App, rec *Record) (*erasureSubject, error) {
	s := &erasureSubject{rec: rec, ids: []string{rec.Id}}

	switch {
	case rec.Collection().Name == CollectionNameHouseholdMembers:
		s.member = true
	case rec.Collection().IsAuth():
		if rec.Collection().Name == CollectionNameSuperusers {
			return nil, erasureErr(http.StatusConflict, ErasureCodeSuperuser,
				"Operator accounts are not erased through this tool.")
		}

		if _, err := app.FindCachedCollectionByNameOrId(CollectionNameHouseholdMembers); err == nil {
			ms, err := app.FindRecordsByFilter(CollectionNameHouseholdMembers, "user={:u}", "created", 0, 0, dbx.Params{"u": rec.Id})
			if err != nil {
				return nil, err
			}

			for _, m := range ms {
				s.members = append(s.members, m)
				s.ids = append(s.ids, m.Id)
			}
		}
	default:
		return nil, erasureErr(http.StatusBadRequest, ErasureCodeSubject, "The subject must be an account or a household member.")
	}

	return s, nil
}

// erasurePreflight refuses an erasure that would break something that must
// hold: the last active adult guardian of a household with a minor, and the
// last owner of an organization. Nothing is changed.
func erasurePreflight(app App, subject *Record) error {
	s, err := erasureLoadSubject(app, subject)
	if err != nil {
		return err
	}

	if s.member {
		if subject.GetString("user") != "" {
			return erasureErr(http.StatusConflict, ErasureCodeLoginMember,
				"This person has an account. Erase the account, which covers the household row.")
		}

		return nil
	}

	// the last guardian
	var households []string

	for _, m := range s.members {
		if !m.GetBool("guardian") || m.GetBool("isMinor") || m.GetString("status") != HouseholdMemberActive {
			continue
		}

		h, err := app.FindRecordById(CollectionNameHouseholds, m.GetString("household"))
		if err != nil {
			continue
		}

		all, err := HouseholdMembers(app, h)
		if err != nil {
			return err
		}

		rest := make([]*Record, 0, len(all))

		for _, o := range all {
			if !containsString(s.ids, o.Id) {
				rest = append(rest, o)
			}
		}

		if householdNeedsGuardian(rest) && !householdNeedsGuardian(all) {
			households = append(households, h.Id)
		}
	}

	if len(households) > 0 {
		return erasureErr(http.StatusConflict, ErasureCodeLastGuardian,
			"This person is the only active guardian of a household with a child. Add another adult guardian to the household first, or move the child.",
			households...)
	}

	// the last owner of an org
	if _, err := app.FindCachedCollectionByNameOrId(CollectionNameOrgMembers); err == nil {
		ms, err := app.FindRecordsByFilter(CollectionNameOrgMembers, "user={:u}", "", 0, 0, dbx.Params{"u": subject.Id})
		if err != nil {
			return err
		}

		var orgs []string

		for _, m := range ms {
			if isActiveOrgOwner(m) && countOtherActiveOwners(app, m.GetString("org"), m.Id) == 0 {
				orgs = append(orgs, m.GetString("org"))
			}
		}

		if len(orgs) > 0 {
			return erasureErr(http.StatusConflict, ErasureCodeLastOwner,
				"This person is the only owner of an organization. Make someone else an owner first, or archive the organization.",
				orgs...)
		}
	}

	return nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}

	return false
}

// ---- requests ----------------------------------------------------------------

// ErasureRequestInput describes an erasure request.
type ErasureRequestInput struct {
	// Subject is the person to erase: an auth record, or the household member
	// row of a minor (no login).
	Subject *Record

	// Requester is who asks: the subject (self), a guardian of the minor, or a
	// superuser.
	Requester *Record
	Mode      string
	IP        string

	// IdentityCheck (admin, no emailed confirmation) is how the person was
	// identified: kept in the request's meta.
	IdentityCheck string

	// RequireConfirmation (admin) sends the person the confirmation link
	// instead of taking the identity check on the staff member's word.
	RequireConfirmation bool

	// GraceDays overrides the setting (admin only), 0 to 30.
	GraceDays *int

	Ctx context.Context
}

// RequestErasure records an erasure request. Self service and guardian
// requests, and an admin request with RequireConfirmation, wait for the emailed
// confirmation. An admin request with an identity check starts the grace period
// (or the job, when it is 0) at once.
func RequestErasure(app App, in ErasureRequestInput) (*Record, error) {
	if !ErasureAvailable(app) {
		return nil, erasureErr(http.StatusNotFound, ErasureCodeNotAvailable, "Erasure is not available.")
	}

	ctx := in.Ctx
	if ctx == nil {
		ctx = context.Background()
	}

	subject, requester := in.Subject, in.Requester
	if subject == nil || requester == nil {
		return nil, erasureErr(http.StatusBadRequest, ErasureCodeSubject, "A subject and a requester are required.")
	}

	subjectIsMember := subject.Collection().Name == CollectionNameHouseholdMembers

	var recipient *Record

	switch in.Mode {
	case ErasureModeSelf:
		if subjectIsMember || requester.Id != subject.Id || requester.Collection().Id != subject.Collection().Id {
			return nil, erasureErr(http.StatusForbidden, ErasureCodeForbidden, "You can only ask for the erasure of your own account.")
		}

		recipient = subject
	case ErasureModeGuardian:
		if !subjectIsMember {
			return nil, erasureErr(http.StatusForbidden, ErasureCodeForbidden, "A guardian can only ask for the erasure of a child of the household.")
		}

		if !subject.GetBool("isMinor") || subject.GetString("user") != "" {
			return nil, erasureErr(http.StatusForbidden, ErasureCodeNotMinorMember, "Only the erasure of a child without an account can be asked for by a guardian.")
		}

		if requester.IsSuperuser() || activeGuardianOf(app, subject.GetString("household"), requester.Id) == nil {
			return nil, erasureErr(http.StatusForbidden, ErasureCodeForbidden, "Only an active guardian of the household can ask for this.")
		}

		recipient = requester
	case ErasureModeAdmin:
		if !requester.IsSuperuser() {
			return nil, erasureErr(http.StatusForbidden, ErasureCodeForbidden, "Only a superuser can start an erasure for another person.")
		}

		if in.RequireConfirmation && !subjectIsMember {
			recipient = subject
		}

		if in.RequireConfirmation && subjectIsMember {
			return nil, erasureErr(http.StatusBadRequest, ErasureCodeIdentityCheck,
				"A person without an account cannot confirm by email: record the identity check instead.")
		}

		if !in.RequireConfirmation && strings.TrimSpace(in.IdentityCheck) == "" {
			return nil, erasureErr(http.StatusBadRequest, ErasureCodeIdentityCheck,
				"Say how the person was identified (identityCheck), or ask for the emailed confirmation (requireConfirmation).")
		}

		if len(in.IdentityCheck) > erasureIdentityCheckMax {
			return nil, erasureErr(http.StatusBadRequest, ErasureCodeIdentityCheck, "The identity check text is too long.")
		}

		if in.GraceDays != nil && (*in.GraceDays < 0 || *in.GraceDays > ErasureMaxGraceDays) {
			return nil, erasureErr(http.StatusBadRequest, ErasureCodeState, "graceDays must be between 0 and 30.")
		}
	default:
		return nil, erasureErr(http.StatusBadRequest, ErasureCodeState, "Unknown mode.")
	}

	if recipient != nil && !erasureUsableEmail(recipient) {
		return nil, erasureErr(http.StatusConflict, ErasureCodeNoEmail,
			"There is no usable email address to send the confirmation to. Ask the church office, who can start the erasure after checking who you are.")
	}

	if err := erasurePreflight(app, subject); err != nil {
		return nil, err
	}

	col, err := app.FindCachedCollectionByNameOrId(CollectionNameDataRequests)
	if err != nil {
		return nil, err
	}

	grace := ErasureGraceDays(app)
	if in.GraceDays != nil {
		grace = *in.GraceDays
	}

	var (
		req   *Record
		token string
	)

	needsConfirm := recipient != nil

	err = app.RunInTransaction(func(txApp App) error {
		active, err := txApp.CountRecords(CollectionNameDataRequests, dbx.And(
			dbx.HashExp{
				"type":          DataRequestTypeErase,
				"collectionRef": subject.Collection().Id,
				"recordRef":     subject.Id,
			},
			dbx.In("state", DataRequestStateAwaitingConfirmation, DataRequestStateGrace, DataRequestStateQueued, DataRequestStateRunning),
		))
		if err != nil {
			return err
		}

		if active > 0 {
			return erasureErr(http.StatusConflict, ErasureCodeActive, "An erasure for this person is already in progress.")
		}

		req = NewRecord(col)
		req.Set("type", DataRequestTypeErase)
		req.Set("collectionRef", subject.Collection().Id)
		req.Set("recordRef", subject.Id)
		req.Set("requestedByCollection", requester.Collection().Id)
		req.Set("requestedBy", requester.Id)
		req.Set("selfService", in.Mode == ErasureModeSelf || in.Mode == ErasureModeGuardian)
		req.Set("dueAt", types.NowDateTime().Add(dataRequestDueDays*24*time.Hour))

		meta := map[string]any{"mode": in.Mode, "graceDays": grace}
		if in.IdentityCheck != "" {
			meta["identityCheck"] = strings.TrimSpace(in.IdentityCheck)
		}

		req.Set("meta", meta)

		if needsConfirm {
			token = security.RandomString(40)
			req.Set("state", DataRequestStateAwaitingConfirmation)
			req.Set("tokenHash", erasureHash(token))
			req.Set("tokenExpires", types.NowDateTime().Add(erasureTokenLifetime))
		} else {
			req.Set("state", DataRequestStateGrace) // startErasureGrace sets the real state
		}

		if err := txApp.SaveWithContext(ctx, req); err != nil {
			return err
		}

		return writeErasureAudit(txApp, ctx, AuditActionErasureRequest, req, requester, in.IP, map[string]any{
			"subjectCollection": subject.Collection().Name,
			"subjectId":         subject.Id,
			"mode":              in.Mode,
			"dueAt":             req.GetString("dueAt"),
			"confirmation":      needsConfirm,
		})
	})
	if err != nil {
		return nil, err
	}

	if !needsConfirm {
		if err := startErasureGrace(app, ctx, req, subject, requester, in.IP, grace); err != nil {
			return nil, err
		}

		return app.FindRecordById(CollectionNameDataRequests, req.Id)
	}

	notice := &ErasureNotice{
		Kind: "confirm", Recipient: recipient, Subject: subject, OnBehalf: in.Mode == ErasureModeGuardian,
		Token: token, Expires: req.GetDateTime("tokenExpires").Time(), GraceDays: grace,
	}

	if ErasureNotifier == nil || ErasureNotifier(app, notice) != nil {
		// nobody can confirm a request nobody was told about
		req.Set("state", DataRequestStateCancelled)
		req.Set("tokenHash", "")
		req.Set("error", "the confirmation email could not be sent")
		req.Set("cancelledAt", types.NowDateTime())

		if err := app.SaveNoValidateWithContext(ctx, req); err != nil {
			app.Logger().Error("Failed to cancel an erasure request whose email failed", "request", req.Id, "error", err)
		}

		return nil, erasureErr(http.StatusServiceUnavailable, ErasureCodeMail, "The confirmation email could not be sent. Try again later.")
	}

	return req, nil
}

func writeErasureAudit(app App, ctx context.Context, action string, req, actor *Record, ip string, meta map[string]any) error {
	if !auditAvailable(app) {
		return nil
	}

	entry := &AuditEntry{
		Action:         action,
		CollectionId:   req.Collection().Id,
		CollectionName: CollectionNameDataRequests,
		RecordId:       req.Id,
		ChangedFields:  []string{},
		IP:             ip,
		Meta:           meta,
	}
	entry.Actor = NewAuditActorFromRecord(actor)

	return WriteAuditEntry(app, ctx, entry, nil)
}

func erasureSubjectOf(app App, req *Record) (*Record, error) {
	return app.FindRecordById(req.GetString("collectionRef"), req.GetString("recordRef"))
}

func erasureMeta(req *Record) map[string]any {
	m := map[string]any{}
	_ = req.UnmarshalJSONField("meta", &m)

	return m
}

// erasureRecipient is who is told about the request: the subject, or the
// guardian who asked for a minor.
func erasureRecipient(app App, req, subject *Record) (*Record, bool) {
	if subject.Collection().Name == CollectionNameHouseholdMembers {
		who := dataRequestActor(app, req)
		if who == nil || who.IsSuperuser() {
			return nil, true
		}

		return who, true
	}

	return subject, false
}

// ConfirmErasure confirms a request by the token of the emailed link and
// starts the grace period (or the job when the grace period is 0).
func ConfirmErasure(app App, ctx context.Context, token, ip string) (*Record, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	req, err := findErasureByToken(app, "tokenHash", token, DataRequestStateAwaitingConfirmation)
	if err != nil {
		return nil, err
	}

	if exp := req.GetDateTime("tokenExpires"); !exp.IsZero() && exp.Time().Before(time.Now()) {
		return nil, erasureErr(http.StatusGone, ErasureCodeToken, "The link has expired. Ask again.")
	}

	subject, err := erasureSubjectOf(app, req)
	if err != nil {
		return nil, erasureErr(http.StatusNotFound, ErasureCodeSubject, "The person no longer exists.")
	}

	if err := erasurePreflight(app, subject); err != nil {
		return nil, err
	}

	meta := erasureMeta(req)
	grace := ErasureGraceDays(app)

	if g, ok := meta["graceDays"].(float64); ok {
		grace = int(g)
	}

	requester := dataRequestActor(app, req)

	if err := startErasureGrace(app, ctx, req, subject, requester, ip, grace); err != nil {
		return nil, err
	}

	return app.FindRecordById(CollectionNameDataRequests, req.Id)
}

func findErasureByToken(app App, field, token, state string) (*Record, error) {
	token = strings.TrimSpace(token)
	if len(token) < 20 {
		return nil, erasureErr(http.StatusNotFound, ErasureCodeToken, "This link is not valid.")
	}

	var reqs []*Record

	// the token fields are hidden: the filter language does not accept them
	err := app.RecordQuery(CollectionNameDataRequests).
		AndWhere(dbx.HashExp{"type": DataRequestTypeErase, field: erasureHash(token)}).
		AndWhere(dbx.In("state", stringsToAny(state)...)).
		Limit(1).
		All(&reqs)
	if err != nil || len(reqs) == 0 {
		return nil, erasureErr(http.StatusNotFound, ErasureCodeToken, "This link is not valid, or the request is no longer open.")
	}

	return reqs[0], nil
}

func stringsToAny(s ...string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}

	return out
}

// startErasureGrace confirms the request. With days > 0 it locks the account
// and waits; with 0 it queues the job.
func startErasureGrace(app App, ctx context.Context, req, subject, actor *Record, ip string, days int) error {
	cancelToken := security.RandomString(40)
	now := types.NowDateTime()

	req.Set("confirmedAt", now)
	req.Set("tokenHash", "")
	req.Set("tokenExpires", nil)
	req.Set("cancelHash", erasureHash(cancelToken))
	req.Set("graceUntil", now.Add(time.Duration(days)*24*time.Hour))

	meta := erasureMeta(req)
	meta["graceDays"] = days

	if days > 0 {
		req.Set("state", DataRequestStateGrace)
	} else {
		req.Set("state", DataRequestStateQueued)
	}

	err := app.RunInTransaction(func(txApp App) error {
		if days > 0 && subject.Collection().IsAuth() {
			created, err := applyErasureBan(txApp, ctx, req, subject)
			if err != nil {
				return err
			}

			meta["banCreated"] = created
		}

		req.Set("meta", meta)

		if err := txApp.SaveWithContext(ctx, req); err != nil {
			return err
		}

		return writeErasureAudit(txApp, ctx, AuditActionErasureConfirm, req, actor, ip, map[string]any{
			"subjectCollection": subject.Collection().Name,
			"subjectId":         subject.Id,
			"graceUntil":        req.GetString("graceUntil"),
			"graceDays":         days,
		})
	})
	if err != nil {
		return err
	}

	if days == 0 {
		StartErasures(app)

		return nil
	}

	// tell them when it happens and how to stop it
	if ErasureNotifier != nil {
		if rec, onBehalf := erasureRecipient(app, req, subject); rec != nil && erasureUsableEmail(rec) {
			err := ErasureNotifier(app, &ErasureNotice{
				Kind: "grace", Recipient: rec, Subject: subject, OnBehalf: onBehalf, Token: cancelToken,
				GraceUntil: req.GetDateTime("graceUntil").Time(), GraceDays: days,
			})
			if err != nil {
				app.Logger().Warn("Failed to send the erasure grace notice", "request", req.Id, "error", err.Error())
			}
		}
	}

	return nil
}

// applyErasureBan locks the account for the grace period. An existing active
// ban of another kind is left alone (and never lifted by a cancel). It returns
// whether this request created or reused the ban row.
func applyErasureBan(app App, ctx context.Context, req, subject *Record) (bool, error) {
	rows, err := app.FindAllRecords(CollectionNameBans, dbx.HashExp{
		"collectionRef": subject.Collection().Id, "recordRef": subject.Id,
	})
	if err != nil {
		return false, err
	}

	if len(rows) > 0 {
		b := rows[0]

		until := b.GetDateTime("until")
		if until.IsZero() || until.Time().After(time.Now()) {
			if b.GetString("reason") == BanReasonPendingErasure {
				return true, nil
			}

			return false, nil // another ban is in force: the account is locked already
		}

		// an expired ban row: reuse it (one row per account)
		b.Set("reason", BanReasonPendingErasure)
		b.Set("until", nil)
		b.Set("createdBy", "erasure:"+req.Id)

		return true, app.SaveNoValidateWithContext(ctx, b)
	}

	col, err := app.FindCachedCollectionByNameOrId(CollectionNameBans)
	if err != nil {
		return false, err
	}

	b := NewRecord(col)
	b.Set("collectionRef", subject.Collection().Id)
	b.Set("recordRef", subject.Id)
	b.Set("reason", BanReasonPendingErasure)
	b.Set("createdBy", "erasure:"+req.Id)

	return true, app.SaveNoValidateWithContext(ctx, b)
}

// erasureLockCtxKey marks the deletion of the lock an erasure placed itself.
type erasureLockCtxKey struct{}

// liftErasureBan removes the ban this request placed, never another one.
func liftErasureBan(app App, subjectCollectionId, subjectId string) error {
	rows, err := app.FindAllRecords(CollectionNameBans, dbx.HashExp{
		"collectionRef": subjectCollectionId, "recordRef": subjectId, "reason": BanReasonPendingErasure,
	})
	if err != nil {
		return err
	}

	ctx := context.WithValue(context.Background(), auditSuppressCtxKey{}, true)
	ctx = context.WithValue(ctx, erasureLockCtxKey{}, true)

	for _, b := range rows {
		if err := app.DeleteWithContext(ctx, b); err != nil {
			return err
		}
	}

	return nil
}

// CancelErasure cancels a request that has not started to run. by is who
// cancelled (nil for the link) and via says how ("link", "passkey", "session",
// "admin"). The account is unlocked.
func CancelErasure(app App, ctx context.Context, req, by *Record, via, ip string) error {
	if ctx == nil {
		ctx = context.Background()
	}

	subject, subjectErr := erasureSubjectOf(app, req)

	err := app.RunInTransaction(func(txApp App) error {
		// compare-and-swap: the job may claim it at this moment
		res, err := txApp.NonconcurrentDB().Update(CollectionNameDataRequests,
			dbx.Params{"state": DataRequestStateCancelled, "cancelledAt": types.NowDateTime().String(), "tokenHash": "", "cancelHash": ""},
			dbx.And(dbx.HashExp{"id": req.Id, "type": DataRequestTypeErase},
				dbx.In("state", DataRequestStateAwaitingConfirmation, DataRequestStateGrace, DataRequestStateQueued)),
		).Execute()
		if err != nil {
			return err
		}

		if n, _ := res.RowsAffected(); n != 1 {
			return erasureErr(http.StatusConflict, ErasureCodeState, "The erasure can no longer be cancelled.")
		}

		if subjectErr == nil && subject.Collection().IsAuth() && erasureMetaBool(req, "banCreated") {
			if err := liftErasureBan(txApp, subject.Collection().Id, subject.Id); err != nil {
				return err
			}
		}

		return writeErasureAudit(txApp, ctx, AuditActionErasureCancel, req, by, ip, map[string]any{
			"subjectCollection": req.GetString("collectionRef"),
			"subjectId":         req.GetString("recordRef"),
			"via":               via,
		})
	})

	return err
}

func erasureMetaBool(req *Record, key string) bool {
	v, _ := erasureMeta(req)[key].(bool)

	return v
}

// CancelErasureByToken cancels with the token of the link in the grace email.
func CancelErasureByToken(app App, ctx context.Context, token, ip string) (*Record, error) {
	req, err := findErasureByToken(app, "cancelHash", token, DataRequestStateGrace)
	if err != nil {
		// a request that was just promoted to queued can still be stopped
		var e2 error

		req, e2 = findErasureByToken(app, "cancelHash", token, DataRequestStateQueued)
		if e2 != nil {
			return nil, err
		}
	}

	if err := CancelErasure(app, ctx, req, nil, "link", ip); err != nil {
		return nil, err
	}

	return app.FindRecordById(CollectionNameDataRequests, req.Id)
}

// CancelPendingErasureForAccount cancels the erasure that locks the account,
// after the person proved who they are with a passkey (the sign-in of a locked
// account). It reports whether one was cancelled.
func CancelPendingErasureForAccount(app App, ctx context.Context, account *Record, ip string) bool {
	if account == nil || !ErasureAvailable(app) {
		return false
	}

	var reqs []*Record

	err := app.RecordQuery(CollectionNameDataRequests).
		AndWhere(dbx.HashExp{
			"type": DataRequestTypeErase, "state": DataRequestStateGrace,
			"collectionRef": account.Collection().Id, "recordRef": account.Id,
		}).
		Limit(1).
		All(&reqs)
	if err != nil || len(reqs) == 0 {
		return false
	}

	req := reqs[0]

	if err := CancelErasure(app, ctx, req, account, "passkey", ip); err != nil {
		app.Logger().Warn("Failed to cancel the erasure on a passkey sign-in", "request", req.Id, "error", err.Error())

		return false
	}

	return true
}

// ---- job loop ------------------------------------------------------------------

// StartErasures starts the job in the background unless it is running.
func StartErasures(app App) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				app.Logger().Error("Erasure job panicked", "panic", r)
			}
		}()

		ProcessErasures(app)
	}()
}

// ProcessErasures promotes the requests whose grace period is over and runs the
// queued ones, oldest first, one at a time. It returns how many it ran, and 0
// at once when another call is working through the queue.
func ProcessErasures(app App) int {
	if !ErasureAvailable(app) || !erasureRunMu.TryLock() {
		return 0
	}
	defer erasureRunMu.Unlock()

	ran := 0

	// a request that failed in this pass waits for the next one (the cron, or a
	// restart): a failing step is not retried in a tight loop
	failed := map[string]bool{}

	for {
		if !app.IsBootstrapped() || app.NonconcurrentDB() == nil {
			return ran // shutting down
		}

		if err := promoteDueErasures(app); err != nil {
			app.Logger().Warn("Failed to promote the erasure requests whose grace period ended", "error", err)
		}

		req, err := claimNextErasure(app, failed)
		if err != nil {
			app.Logger().Warn("Failed to load the next erasure request", "error", err)

			return ran
		}

		if req == nil {
			return ran
		}

		if !runErasure(app, req) {
			failed[req.Id] = true
		}

		ran++
	}
}

func promoteDueErasures(app App) error {
	n, err := app.CountRecords(CollectionNameDataRequests, dbx.And(
		dbx.HashExp{"type": DataRequestTypeErase, "state": DataRequestStateGrace},
		dbx.NewExp("[[graceUntil]] != '' AND [[graceUntil]] <= {:now}", dbx.Params{"now": types.NowDateTime().String()}),
	))
	if err != nil || n == 0 {
		return err
	}

	_, err = app.NonconcurrentDB().Update(CollectionNameDataRequests,
		dbx.Params{"state": DataRequestStateQueued},
		dbx.And(dbx.HashExp{"type": DataRequestTypeErase, "state": DataRequestStateGrace},
			dbx.NewExp("[[graceUntil]] != '' AND [[graceUntil]] <= {:now}", dbx.Params{"now": types.NowDateTime().String()})),
	).Execute()

	return err
}

func claimNextErasure(app App, skip map[string]bool) (*Record, error) {
	for range 5 {
		recs, err := app.FindRecordsByFilter(CollectionNameDataRequests,
			"type = 'erase' && state = 'queued'", "created", 0, 0)
		if err != nil {
			return nil, err
		}

		recs = slices.DeleteFunc(recs, func(r *Record) bool { return skip[r.Id] })

		if len(recs) == 0 {
			return nil, nil
		}

		res, err := app.NonconcurrentDB().Update(CollectionNameDataRequests,
			dbx.Params{"state": DataRequestStateRunning},
			dbx.HashExp{"id": recs[0].Id, "state": DataRequestStateQueued},
		).Execute()
		if err != nil {
			return nil, err
		}

		if n, _ := res.RowsAffected(); n == 1 {
			return app.FindRecordById(CollectionNameDataRequests, recs[0].Id)
		}
	}

	return nil, nil
}

// resumeErasures puts the requests a crash left running back in the queue; the
// progress stored on the row says where the job continues.
func resumeErasures(app App) error {
	n, err := app.CountRecords(CollectionNameDataRequests, dbx.HashExp{
		"type": DataRequestTypeErase, "state": DataRequestStateRunning,
	})
	if err != nil || n == 0 {
		return err
	}

	_, err = app.NonconcurrentDB().Update(CollectionNameDataRequests,
		dbx.Params{"state": DataRequestStateQueued},
		dbx.HashExp{"type": DataRequestTypeErase, "state": DataRequestStateRunning},
	).Execute()

	return err
}

// ExpireErasureConfirmations cancels the requests whose confirmation link was
// never used.
func ExpireErasureConfirmations(app App) (int, error) {
	if !ErasureAvailable(app) {
		return 0, nil
	}

	var recs []*Record

	err := app.RecordQuery(CollectionNameDataRequests).
		AndWhere(dbx.HashExp{"type": DataRequestTypeErase, "state": DataRequestStateAwaitingConfirmation}).
		AndWhere(dbx.NewExp("[[tokenExpires]] != '' AND [[tokenExpires]] < {:now}", dbx.Params{"now": types.NowDateTime().String()})).
		All(&recs)
	if err != nil {
		return 0, err
	}

	n := 0

	for _, r := range recs {
		r.Set("state", DataRequestStateExpired)
		r.Set("tokenHash", "")
		r.Set("cancelledAt", types.NowDateTime())

		if err := app.SaveNoValidate(r); err == nil {
			n++
		}
	}

	return n, nil
}

// ---- hooks -----------------------------------------------------------------------

func (app *BaseApp) registerErasureHooks() {
	// The records API never creates, changes or deletes an erasure request, or a
	// row of the erasure log: a superuser could otherwise skip the grace period
	// by editing a state. The functions above write them.
	forbidden := func(msg string) func(e *RecordRequestEvent) error {
		return func(e *RecordRequestEvent) error {
			return router.NewForbiddenError(msg, nil)
		}
	}

	for _, bindFn := range []func(tags ...string) *hook.TaggedHook[*RecordRequestEvent]{
		app.OnRecordCreateRequest, app.OnRecordUpdateRequest, app.OnRecordDeleteRequest,
	} {
		bindFn(CollectionNameErasureLog).Bind(&hook.Handler[*RecordRequestEvent]{
			Id: "__clvErasureLogGuard__", Priority: -100000,
			Func: forbidden("The erasure log is written by the erasure job only."),
		})

		bindFn(CollectionNameDataRequests).Bind(&hook.Handler[*RecordRequestEvent]{
			Id: "__clvErasureRequestGuard__", Priority: -100000,
			Func: func(e *RecordRequestEvent) error {
				if e.Record.GetString("type") == DataRequestTypeErase {
					return router.NewForbiddenError("Erasure requests are made through /api/gdpr and /api/me/erasure.", nil)
				}

				if orig := e.Record.Original(); orig != nil && orig.GetString("type") == DataRequestTypeErase {
					return router.NewForbiddenError("Erasure requests are made through /api/gdpr and /api/me/erasure.", nil)
				}

				return e.Next()
			},
		})
	}

	app.OnServe().Bind(&hook.Handler[*ServeEvent]{
		Id: "__clvErasureResume__",
		Func: func(e *ServeEvent) error {
			if ErasureAvailable(e.App) {
				if err := resumeErasures(e.App); err != nil {
					e.App.Logger().Warn("Failed to queue the interrupted erasures again", "error", err)
				}

				// a restore may have brought erased people back
				go func() {
					defer func() {
						if r := recover(); r != nil {
							e.App.Logger().Error("Erasure replay panicked", "panic", r)
						}
					}()

					if _, err := ReplayErasureLog(e.App); err != nil {
						e.App.Logger().Warn("Failed to replay the erasure log", "error", err)
					}

					ProcessErasures(e.App)
				}()
			}

			return e.Next()
		},
	})

	app.Cron().Add(erasureCronId, erasureCronExpr, func() {
		if !ErasureAvailable(app) {
			return
		}

		if _, err := ExpireErasureConfirmations(app); err != nil {
			app.Logger().Warn("Failed to expire erasure confirmations", "error", err)
		}

		ProcessErasures(app)
	})
}

var errErasureSubjectGone = errors.New("the subject no longer exists")
