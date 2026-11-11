package core

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/security"
	"github.com/doodlemania2/claviger/tools/types"
	"github.com/golang-jwt/jwt/v5"
)

// Fork-local (Claviger #83): staff-vouched recovery codes.
//
// A staff member who has verified a person in the flesh issues a short-lived,
// single-use code. The person types it (or follows a link made from it) on
// the hosted recovery page and receives the ordinary passkey reset token, so
// the existing confirm-passkey-reset routes enroll the new passkey and rotate
// the token key. There is no other enrollment path.
//
// The hidden _recoveryCodes collection holds the codes, as SHA-256 hashes. It
// has null rules: the records API shows it to superusers only, and every write
// goes through the functions below (and the /api/orgs/{id}/recovery routes).
//
// Security decisions, in one place:
//
//   - Who may issue: a superuser, an owner or admin of the org, or a member
//     tagged "greeter". The target must be an active member of that org. No
//     self issue. A banned target is refused with the same answer as a
//     non-member.
//   - A target who is an owner or admin of any org (or has the email of a
//     superuser) needs two different staff members. The first creates a
//     pending request that holds NO code; a different eligible staff member
//     confirms it and the code is generated at that moment and shown to the
//     confirmer only. Because only a hash is stored, a code can never be shown
//     again. Both steps need a fresh passkey assertion (apis layer).
//   - Code: 10 characters from a 32-letter alphabet (50 bits), shown as two
//     groups of five. The first group is a public reference (the lookup key),
//     the whole code is hashed. That is what lets the server count wrong
//     guesses per code: a guess whose reference matches a live code and whose
//     hash does not counts against that code, and the third one kills it.
//     The QR variant is 26 characters (130 bits) of the same alphabet.
//   - Redeem never says why it failed (unknown, wrong, expired, used, locked,
//     pending and banned all look the same).
//   - Neither the code nor its hash is ever written to the audit log or to
//     the application log. The collection is excluded from the automatic
//     record audit for that reason; explicit entries are written instead.

const (
	CollectionNameRecoveryCodes = "_recoveryCodes"

	// RecoveryTag is the org tag (an _orgRoles row of the org) that lets a
	// member issue recovery codes. Owners and admins may always issue.
	RecoveryTag = "greeter"

	RecoveryChannelDisplay = "display"
	RecoveryChannelQR      = "qr"
	RecoveryChannelSMS     = "sms"

	RecoveryVerificationKnownBySight = "known_by_sight"
	RecoveryVerificationIDChecked    = "id_checked"
	RecoveryVerificationOther        = "other"

	// audit actions
	AuditActionRecoveryIssue        = "recovery_issue"
	AuditActionRecoveryConfirm      = "recovery_confirm"
	AuditActionRecoveryRedeem       = "recovery_redeem"
	AuditActionRecoveryRedeemFailed = "recovery_redeem_failed"
	AuditActionRecoveryLocked       = "recovery_code_locked"
	AuditActionRecoveryReported     = "recovery_reported"

	// RecoveryCodeMinutesEnv sets the lifetime of a code (5 to 60, default 15).
	RecoveryCodeMinutesEnv = "CLAVIGER_RECOVERY_CODE_MINUTES"
	// RecoveryIssuePerHourEnv sets how many codes one staff member may issue
	// per hour (1 to 100, default 5).
	RecoveryIssuePerHourEnv = "CLAVIGER_RECOVERY_ISSUE_PER_HOUR"

	DefaultRecoveryCodeMinutes  = 15
	DefaultRecoveryIssuePerHour = 5

	// RecoveryMaxAttempts is how many wrong guesses a code survives.
	RecoveryMaxAttempts = 3

	// 32 characters; no 0, 1, I or O, which are easy to mistake for each other
	recoveryAlphabet   = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	recoveryCodeLength = 10
	recoveryTokenLen   = 26
	recoveryRefLength  = 5

	// how long an expired row is kept before the next issue deletes it
	recoveryKeepExpired = 30 * 24 * time.Hour
)

// RecoveryChannels and RecoveryVerifications are the select values.
var (
	RecoveryChannels      = []string{RecoveryChannelDisplay, RecoveryChannelQR, RecoveryChannelSMS}
	RecoveryVerifications = []string{RecoveryVerificationKnownBySight, RecoveryVerificationIDChecked, RecoveryVerificationOther}
)

// ErrRecoveryInvalid is the only error RedeemRecovery returns for a code that
// cannot be redeemed, whatever the reason.
var ErrRecoveryInvalid = errors.New("invalid or expired code")

// RecoveryAvailable reports whether the recovery codes collection exists.
func RecoveryAvailable(app App) bool {
	_, err := app.FindCachedCollectionByNameOrId(CollectionNameRecoveryCodes)

	return err == nil
}

// RecoveryLifetime is the lifetime of a code, from CLAVIGER_RECOVERY_CODE_MINUTES.
func RecoveryLifetime() time.Duration {
	return time.Duration(envInt(RecoveryCodeMinutesEnv, DefaultRecoveryCodeMinutes, 5, 60)) * time.Minute
}

// RecoveryIssueLimit is the number of codes one staff member may issue per hour.
func RecoveryIssueLimit() int {
	return envInt(RecoveryIssuePerHourEnv, DefaultRecoveryIssuePerHour, 1, 100)
}

func envInt(name string, def, lo, hi int) int {
	v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil || v < lo || v > hi {
		return def
	}

	return v
}

// ---- code generation ---------------------------------------------------------

func randomRecoveryString(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}

	out := make([]byte, n)
	for i, b := range buf {
		out[i] = recoveryAlphabet[b&31] // 256 is a multiple of 32: no modulo bias
	}

	return string(out), nil
}

// NormalizeRecoveryCode upper-cases the input and drops spaces, dashes and
// anything else outside the alphabet's character classes. It returns "" for a
// value of the wrong length (neither a typed code nor a token).
func NormalizeRecoveryCode(s string) string {
	var sb strings.Builder

	for _, r := range strings.ToUpper(s) {
		if (r >= 'A' && r <= 'Z') || (r >= '2' && r <= '9') {
			sb.WriteRune(r)
		}
	}

	out := sb.String()
	if len(out) != recoveryCodeLength && len(out) != recoveryTokenLen {
		return ""
	}

	return out
}

// FormatRecoveryCode shows a 10 character code as two groups of five.
func FormatRecoveryCode(code string) string {
	if len(code) != recoveryCodeLength {
		return code
	}

	return code[:5] + " " + code[5:]
}

func recoveryHash(normalized string) string {
	sum := sha256.Sum256([]byte(normalized))

	return hex.EncodeToString(sum[:])
}

// ---- standing ----------------------------------------------------------------

// RecoveryStaff reports whether the actor may issue and confirm recovery codes
// for the org.
func RecoveryStaff(app App, org *Record, actor *OrgActor) bool {
	if actor == nil || actor.Record == nil {
		return false
	}

	if actor.Superuser {
		return true
	}

	m := ActiveOrgMember(app, org, actor.Record.Id)
	if m == nil {
		return false
	}

	switch m.GetString("role") {
	case OrgRoleOwner, OrgRoleAdmin:
		return true
	case OrgRoleMember:
		for _, name := range OrgTagNames(app, m) {
			if name == RecoveryTag {
				return true
			}
		}
	}

	return false
}

// RecoveryTargetIsElevated reports whether recovering the user needs two staff
// members: an owner or admin of any org, or an account with the email of a
// superuser.
func RecoveryTargetIsElevated(app App, user *Record) bool {
	n, err := app.CountRecords(CollectionNameOrgMembers, dbx.NewExp(
		"[[user]]={:u} AND [[status]]='active' AND ([[role]]='owner' OR [[role]]='admin')", dbx.Params{"u": user.Id}))
	if err != nil || n > 0 {
		return true // when in doubt ask for the second person
	}

	if email := user.Email(); email != "" {
		if _, err := app.FindAuthRecordByEmail(CollectionNameSuperusers, email); err == nil {
			return true
		}
	}

	return false
}

// ---- issue -------------------------------------------------------------------

// RecoveryInput is the request to issue a code.
type RecoveryInput struct {
	User         string // id of the member
	Channel      string
	Verification string
	Note         string
}

// RecoveryIssued is the result of IssueRecovery and ConfirmRecovery.
type RecoveryIssued struct {
	Record *Record
	// Code is the 10 character code ("" for the qr channel and while pending),
	// Token the 26 character QR token ("" otherwise). Shown once.
	Code, Token string
	// Pending is true when a second staff member must confirm first.
	Pending bool
	Target  *Record
}

// DisplayCode is the code as two groups of five.
func (r *RecoveryIssued) DisplayCode() string { return FormatRecoveryCode(r.Code) }

func recoveryTarget(app App, org *Record, userId string) (*Record, error) {
	notFound := orgErr(404, "Member not found.")

	col, err := OrgUserCollection(app)
	if err != nil {
		return nil, notFound
	}

	user, err := app.FindRecordById(col, userId)
	if err != nil || ActiveOrgMember(app, org, user.Id) == nil {
		return nil, notFound
	}

	// a banned account gets the answer of a non-member: no code, no hint why
	if IsBanned(app, user) {
		return nil, notFound
	}

	if !col.WebAuthn.Enabled {
		return nil, orgErr(400, "Passkeys are not enabled for these accounts.")
	}

	return user, nil
}

func requireRecoveryStaff(app App, org *Record, actor *OrgActor) error {
	if !RecoveryAvailable(app) {
		return orgErr(404, "Recovery codes are not available.")
	}

	if org.GetBool("archived") {
		return orgErr(404, "Organization not found.")
	}

	if !RecoveryStaff(app, org, actor) {
		return orgErr(403, "Only greeters, owners and admins can do this.")
	}

	return nil
}

// IssueRecovery creates a recovery code for a member of the org. For an
// elevated target it creates a pending request without a code instead.
func IssueRecovery(app App, ctx context.Context, actor *OrgActor, org *Record, in RecoveryInput) (*RecoveryIssued, error) {
	if err := requireRecoveryStaff(app, org, actor); err != nil {
		return nil, err
	}

	if in.Channel == "" {
		in.Channel = RecoveryChannelDisplay
	}

	if !slices.Contains(RecoveryChannels, in.Channel) {
		return nil, orgErr(400, "Invalid channel.")
	}

	if !slices.Contains(RecoveryVerifications, in.Verification) {
		return nil, orgErr(400, "Say how the person was verified.")
	}

	in.Note = strings.TrimSpace(in.Note)
	if len([]rune(in.Note)) > RecoveryNoteMax {
		return nil, orgErr(400, "The note is too long.")
	}

	if in.User == actor.Record.Id {
		return nil, orgErr(400, "You cannot issue a recovery code for yourself.")
	}

	target, err := recoveryTarget(app, org, in.User)
	if err != nil {
		return nil, err
	}

	// per staff member rate limit, counted in the table (it survives restarts)
	since := types.NowDateTime().Add(-time.Hour).String()

	issued, err := app.CountRecords(CollectionNameRecoveryCodes, dbx.NewExp(
		"[[issuedBy]]={:s} AND [[created]]>={:since}", dbx.Params{"s": actor.Record.Id, "since": since}))
	if err != nil {
		return nil, err
	}

	if int(issued) >= RecoveryIssueLimit() {
		return nil, orgErr(429, "You have issued too many codes in the last hour. Try again later.")
	}

	elevated := RecoveryTargetIsElevated(app, target)

	col, err := app.FindCachedCollectionByNameOrId(CollectionNameRecoveryCodes)
	if err != nil {
		return nil, err
	}

	rec := NewRecord(col)
	rec.Set("user", target.Id)
	rec.Set("org", org.Id)
	rec.Set("issuedBy", actor.Record.Id)
	rec.Set("channel", in.Channel)
	rec.Set("verification", in.Verification)
	rec.Set("note", in.Note)
	rec.Set("elevated", elevated)
	rec.Set("expires", types.NowDateTime().Add(RecoveryLifetime()))
	rec.Set("attempts", 0)

	res := &RecoveryIssued{Record: rec, Pending: elevated, Target: target}

	if !elevated {
		if err := fillRecoveryCode(app, rec, res); err != nil {
			return nil, err
		}
	}

	txErr := app.RunInTransaction(func(txApp App) error {
		// one live code per person: an older one is closed
		if err := closeLiveRecoveryCodes(txApp, target.Id, org.Id); err != nil {
			return err
		}

		_ = purgeOldRecoveryCodes(txApp)

		if err := txApp.Save(rec); err != nil {
			return err
		}

		return writeRecoveryAudit(txApp, ctx, AuditActionRecoveryIssue, rec, map[string]any{"pending": elevated})
	})
	if txErr != nil {
		return nil, txErr
	}

	return res, nil
}

// RecoveryNoteMax is the longest note, in characters.
const RecoveryNoteMax = 200

// fillRecoveryCode generates a code or token for the channel, sets the hash and
// reference on the row and puts the plaintext in res.
func fillRecoveryCode(app App, rec *Record, res *RecoveryIssued) error {
	for range 5 {
		length := recoveryCodeLength
		if rec.GetString("channel") == RecoveryChannelQR {
			length = recoveryTokenLen
		}

		secret, err := randomRecoveryString(length)
		if err != nil {
			return err
		}

		ref := secret[:recoveryRefLength]

		// the reference is the lookup key: keep it unique among live codes
		n, err := app.CountRecords(CollectionNameRecoveryCodes, dbx.HashExp{"ref": ref, "usedAt": ""})
		if err != nil {
			return err
		}

		if n > 0 {
			continue
		}

		rec.Set("ref", ref)
		rec.Set("codeHash", recoveryHash(secret))

		if length == recoveryCodeLength {
			res.Code = secret
		} else {
			res.Token = secret
		}

		return nil
	}

	return errors.New("could not generate a unique recovery code")
}

func closeLiveRecoveryCodes(app App, userId, orgId string) error {
	now := types.NowDateTime()

	_, err := app.NonconcurrentDB().Update(CollectionNameRecoveryCodes,
		dbx.Params{"expires": now.String(), "updated": now.String()},
		dbx.NewExp("[[user]]={:u} AND [[org]]={:o} AND [[usedAt]]='' AND [[expires]]>{:now}",
			dbx.Params{"u": userId, "o": orgId, "now": now.String()}),
	).Execute()

	return err
}

func purgeOldRecoveryCodes(app App) error {
	_, err := app.NonconcurrentDB().Delete(CollectionNameRecoveryCodes,
		dbx.NewExp("[[expires]]<{:cut}", dbx.Params{"cut": types.NowDateTime().Add(-recoveryKeepExpired).String()}),
	).Execute()

	return err
}

// ---- confirm -----------------------------------------------------------------

// RecoveryByID returns an open request of the org.
func recoveryOfOrg(app App, org *Record, id string) (*Record, error) {
	rec, err := app.FindRecordById(CollectionNameRecoveryCodes, id)
	if err != nil || rec.GetString("org") != org.Id {
		return nil, orgErr(404, "Recovery request not found.")
	}

	return rec, nil
}

// ConfirmRecovery is the second staff member's step for an elevated target:
// it generates the code and returns it to the confirmer. The confirmer must be
// eligible, must differ from the issuer and from the target.
func ConfirmRecovery(app App, ctx context.Context, actor *OrgActor, org *Record, id string) (*RecoveryIssued, error) {
	if err := requireRecoveryStaff(app, org, actor); err != nil {
		return nil, err
	}

	rec, err := recoveryOfOrg(app, org, id)
	if err != nil {
		return nil, err
	}

	if !rec.GetBool("elevated") || rec.GetString("confirmedBy") != "" ||
		!rec.GetDateTime("usedAt").IsZero() || !rec.GetDateTime("expires").Time().After(time.Now()) {
		return nil, orgErr(409, "This request cannot be confirmed (it is already confirmed, used or expired).")
	}

	if rec.GetString("issuedBy") == actor.Record.Id {
		return nil, orgErr(403, "A different staff member must confirm.")
	}

	if rec.GetString("user") == actor.Record.Id {
		return nil, orgErr(403, "You cannot confirm a recovery code for yourself.")
	}

	target, err := recoveryTarget(app, org, rec.GetString("user"))
	if err != nil {
		return nil, orgErr(409, "This request cannot be confirmed (it is already confirmed, used or expired).")
	}

	res := &RecoveryIssued{Record: rec, Target: target}

	if err := fillRecoveryCode(app, rec, res); err != nil {
		return nil, err
	}

	now := types.NowDateTime()
	expires := now.Add(RecoveryLifetime())

	txErr := app.RunInTransaction(func(txApp App) error {
		// compare and swap: two confirmations at once cannot both win
		r, err := txApp.NonconcurrentDB().Update(CollectionNameRecoveryCodes,
			dbx.Params{
				"confirmedBy": actor.Record.Id,
				"ref":         rec.GetString("ref"),
				"codeHash":    rec.GetString("codeHash"),
				"expires":     expires.String(),
				"updated":     now.String(),
			},
			dbx.HashExp{"id": rec.Id, "confirmedBy": "", "usedAt": ""},
		).Execute()
		if err != nil {
			return err
		}

		if n, _ := r.RowsAffected(); n != 1 {
			return orgErr(409, "This request cannot be confirmed (it is already confirmed, used or expired).")
		}

		rec.Set("confirmedBy", actor.Record.Id)
		rec.Set("expires", expires)

		return writeRecoveryAudit(txApp, ctx, AuditActionRecoveryConfirm, rec, nil)
	})
	if txErr != nil {
		return nil, txErr
	}

	return res, nil
}

// RecoveryState is the state of a row for the staff list.
func RecoveryState(rec *Record, now time.Time) string {
	switch {
	case !rec.GetDateTime("usedAt").IsZero():
		return "used"
	case !rec.GetDateTime("reportedAt").IsZero():
		return "reported"
	case !rec.GetDateTime("expires").Time().After(now):
		return "expired"
	case rec.GetBool("elevated") && rec.GetString("confirmedBy") == "":
		return "pending"
	default:
		return "active"
	}
}

// ---- redeem ------------------------------------------------------------------

// RedeemRecovery exchanges a code for the passkey reset token of the person it
// was issued to. Every failure returns ErrRecoveryInvalid. ip is only logged.
func RedeemRecovery(app App, code, ip string) (token string, collection *Collection, err error) {
	if !RecoveryAvailable(app) {
		return "", nil, ErrRecoveryInvalid
	}

	norm := NormalizeRecoveryCode(code)
	if norm == "" {
		app.Logger().Warn("recovery redeem failed", "reason", "malformed", "ip", ip)

		return "", nil, ErrRecoveryInvalid
	}

	ctx := context.Background()
	hash := recoveryHash(norm)

	rows, err := app.FindAllRecords(CollectionNameRecoveryCodes, dbx.HashExp{"ref": norm[:recoveryRefLength], "usedAt": ""})
	if err != nil || len(rows) == 0 {
		app.Logger().Warn("recovery redeem failed", "reason", "no_match", "ip", ip)

		return "", nil, ErrRecoveryInvalid
	}

	for _, rec := range rows {
		if rec.GetString("codeHash") == "" { // pending, or locked
			continue
		}

		if subtle.ConstantTimeCompare([]byte(rec.GetString("codeHash")), []byte(hash)) != 1 {
			recordWrongRecoveryGuess(app, ctx, rec)

			continue
		}

		tok, user, ok := redeemMatchingRecovery(app, ctx, rec)
		if ok {
			return tok, user, nil
		}
	}

	return "", nil, ErrRecoveryInvalid
}

func recordWrongRecoveryGuess(app App, ctx context.Context, rec *Record) {
	if !rec.GetDateTime("expires").Time().After(time.Now()) {
		return
	}

	attempts := rec.GetInt("attempts") + 1
	now := types.NowDateTime()
	params := dbx.Params{"attempts": attempts, "updated": now.String()}
	locked := attempts >= RecoveryMaxAttempts

	if locked {
		params["expires"] = now.String()
	}

	if _, err := app.NonconcurrentDB().Update(CollectionNameRecoveryCodes, params, dbx.HashExp{"id": rec.Id, "usedAt": ""}).Execute(); err != nil {
		app.Logger().Error("recovery attempt counter failed", "error", err)

		return
	}

	_ = writeRecoveryAudit(app, ctx, AuditActionRecoveryRedeemFailed, rec, map[string]any{"reason": "wrong_code", "attempts": attempts})

	if locked {
		_ = writeRecoveryAudit(app, ctx, AuditActionRecoveryLocked, rec, nil)
	}
}

func redeemMatchingRecovery(app App, ctx context.Context, rec *Record) (string, *Collection, bool) {
	fail := func(reason string) (string, *Collection, bool) {
		_ = writeRecoveryAudit(app, ctx, AuditActionRecoveryRedeemFailed, rec, map[string]any{"reason": reason})

		return "", nil, false
	}

	if !rec.GetDateTime("expires").Time().After(time.Now()) {
		return fail("expired")
	}

	if !rec.GetDateTime("reportedAt").IsZero() {
		return fail("reported")
	}

	org, err := app.FindRecordById(CollectionNameOrgs, rec.GetString("org"))
	if err != nil || org.GetBool("archived") {
		return fail("org")
	}

	user, err := recoveryTarget(app, org, rec.GetString("user"))
	if err != nil {
		return fail("target")
	}

	// whoever vouched must still be allowed to: a demoted or removed
	// greeter's open code dies with the right
	for _, id := range []string{rec.GetString("issuedBy"), rec.GetString("confirmedBy")} {
		if id != "" && !recoveryStaffStillValid(app, org, id) {
			return fail("issuer")
		}
	}

	token, err := user.NewPasskeyResetToken()
	if err != nil {
		return fail("token")
	}

	now := types.NowDateTime()

	txErr := app.RunInTransaction(func(txApp App) error {
		// single use: only one redeemer can flip usedAt
		r, err := txApp.NonconcurrentDB().Update(CollectionNameRecoveryCodes,
			dbx.Params{"usedAt": now.String(), "updated": now.String()},
			dbx.HashExp{"id": rec.Id, "usedAt": ""},
		).Execute()
		if err != nil {
			return err
		}

		if n, _ := r.RowsAffected(); n != 1 {
			return ErrRecoveryInvalid
		}

		return writeRecoveryAudit(txApp, ctx, AuditActionRecoveryRedeem, rec, nil)
	})
	if txErr != nil {
		return "", nil, false
	}

	return token, user.Collection(), true
}

func recoveryStaffStillValid(app App, org *Record, id string) bool {
	if _, err := app.FindRecordById(CollectionNameSuperusers, id); err == nil {
		return true
	}

	col, err := OrgUserCollection(app)
	if err != nil {
		return false
	}

	u, err := app.FindRecordById(col, id)
	if err != nil {
		return false
	}

	return RecoveryStaff(app, org, NewOrgActor(app, org, u))
}

// ---- report ------------------------------------------------------------------

const recoveryReportTokenType = "recoveryReport"

// NewRecoveryReportToken signs the link of the notice email: it lets the
// person report an unexpected code for a week, without signing in. It is
// signed with the person's token key, so a passkey reset or a ban voids it.
func NewRecoveryReportToken(user *Record, recordId string) (string, error) {
	key := user.TokenKey() + user.Collection().PasskeyResetToken.Secret

	return security.NewJWT(jwt.MapClaims{
		TokenClaimType: recoveryReportTokenType,
		TokenClaimId:   user.Id,
		"rid":          recordId,
	}, key, 7*24*time.Hour)
}

// ParseRecoveryReportToken verifies the token and returns the user id and the
// record id.
func ParseRecoveryReportToken(app App, token string) (userId, recordId string, err error) {
	claims, err := security.ParseUnverifiedJWT(token)
	if err != nil {
		return "", "", ErrRecoveryInvalid
	}

	id, _ := claims[TokenClaimId].(string)

	col, err := OrgUserCollection(app)
	if err != nil {
		return "", "", ErrRecoveryInvalid
	}

	user, err := app.FindRecordById(col, id)
	if err != nil {
		return "", "", ErrRecoveryInvalid
	}

	verified, err := security.ParseJWT(token, user.TokenKey()+col.PasskeyResetToken.Secret)
	if err != nil {
		return "", "", ErrRecoveryInvalid
	}

	if typ, _ := verified[TokenClaimType].(string); typ != recoveryReportTokenType {
		return "", "", ErrRecoveryInvalid
	}

	rid, _ := verified["rid"].(string)

	return user.Id, rid, nil
}

// ReportRecovery closes a code the person did not expect (the link in the
// notice email). It is idempotent. The audit entry is attributed to the person.
func ReportRecovery(app App, ctx context.Context, recordId, userId string) error {
	rec, err := app.FindRecordById(CollectionNameRecoveryCodes, recordId)
	if err != nil || rec.GetString("user") != userId {
		return ErrRecoveryInvalid
	}

	if !rec.GetDateTime("reportedAt").IsZero() {
		return nil
	}

	now := types.NowDateTime()

	return app.RunInTransaction(func(txApp App) error {
		params := dbx.Params{"reportedAt": now.String(), "updated": now.String()}
		if rec.GetDateTime("usedAt").IsZero() {
			params["expires"] = now.String()
		}

		if _, err := txApp.NonconcurrentDB().Update(CollectionNameRecoveryCodes, params, dbx.HashExp{"id": rec.Id}).Execute(); err != nil {
			return err
		}

		return writeRecoveryAudit(txApp, ctx, AuditActionRecoveryReported, rec, nil)
	})
}

// ---- audit -------------------------------------------------------------------

// writeRecoveryAudit writes one entry for a code. The actor is whatever the
// context carries (the staff member for issue and confirm); for redeem, which
// runs unauthenticated, it is the staff member who vouched. Neither the code
// nor the hash is ever included.
func writeRecoveryAudit(app App, ctx context.Context, action string, rec *Record, extra map[string]any) error {
	if !auditAvailable(app) {
		return nil
	}

	meta := map[string]any{
		"orgId":        rec.GetString("org"),
		"user":         rec.GetString("user"),
		"verification": rec.GetString("verification"),
		"channel":      rec.GetString("channel"),
	}

	for k, v := range extra {
		meta[k] = v
	}

	entry := &AuditEntry{
		Action:         action,
		CollectionName: CollectionNameRecoveryCodes,
		RecordId:       rec.Id,
		Meta:           meta,
	}

	if c, err := app.FindCachedCollectionByNameOrId(CollectionNameRecoveryCodes); err == nil {
		entry.CollectionId = c.Id
	}

	applyAuditContext(ctx, entry)

	// redeem and report run without a staff session: attribute them to the
	// staff member who vouched, as the spec asks, with the person in meta
	switch action {
	case AuditActionRecoveryRedeem, AuditActionRecoveryRedeemFailed, AuditActionRecoveryLocked:
		if entry.Actor.Type == AuditActorTypeSystem || entry.Actor.Id == "" {
			vouch := rec.GetString("confirmedBy")
			if vouch == "" {
				vouch = rec.GetString("issuedBy")
			}

			entry.Actor = recoveryStaffActor(app, vouch)
		}

		meta["via"] = "recovery_code"
	case AuditActionRecoveryReported:
		if u, err := app.FindRecordById(OrgUserCollectionName(), rec.GetString("user")); err == nil {
			entry.Actor = NewSelfAuditActor(u)
			meta["self"] = true
		}
	}

	return WriteAuditEntry(app, ctx, entry, nil)
}

func recoveryStaffActor(app App, id string) AuditActor {
	if id == "" {
		return AuditActor{Type: AuditActorTypeSystem}
	}

	if col, err := OrgUserCollection(app); err == nil {
		if u, err := app.FindRecordById(col, id); err == nil {
			return NewAuditActorFromRecord(u)
		}
	}

	if su, err := app.FindRecordById(CollectionNameSuperusers, id); err == nil {
		return NewAuditActorFromRecord(su)
	}

	return AuditActor{Type: AuditActorTypeSystem}
}
