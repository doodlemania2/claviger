package core

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/mail"
	"strings"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/hook"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #79b): email invitations to organizations.
//
// An invitation is a row of the hidden _invitations system collection. The
// link that is emailed carries a random 32-byte token; only its SHA-256 is
// stored, so a database read cannot be turned into a working link, and the
// token is single use (the status moves from pending to accepted, revoked or
// expired and never back, except that a resend renews a pending or expired
// invitation with a new token).
//
// Like every org write, the operations here are plain functions that the
// /api/orgs routes call, so SCIM (79c) can reuse them. Accepting writes the
// membership with AddOrgMember (source "invitation") and marks the
// invitation accepted inside one transaction.
//
// With no invitation created, none of this runs.

const (
	// CollectionNameInvitations is the hidden system collection of invitations.
	CollectionNameInvitations = "_invitations"

	InvitationStatusPending  = "pending"
	InvitationStatusAccepted = "accepted"
	InvitationStatusRevoked  = "revoked"
	InvitationStatusExpired  = "expired"

	// InvitationLifetimeDays is the default life of an invitation. An org can
	// change it with settings.invitationExpiryDays (1 to 30).
	InvitationLifetimeDays = 7

	// InvitationsPerHour is the default cap of invitations created per org and
	// hour. An org can change it with settings.invitationsPerHour (1 to 200).
	InvitationsPerHour = 20

	// InvitationResendInterval is the shortest time between two sends of the
	// same invitation.
	InvitationResendInterval = time.Minute

	// InvitationRetentionDays is how long finished invitations are kept.
	InvitationRetentionDays = 90

	invitationCleanupCronId   = "__pbInvitationCleanup__"
	invitationCleanupCronExpr = "13 5 * * *" // daily at 05:13 (off :00, :02, :07, :37, 03:23, 03:41, 04:47)

	// InvitationInvalidMessage is the one answer for an unknown, revoked,
	// accepted or expired token, so a token cannot be probed.
	InvitationInvalidMessage = "This invitation is invalid or has expired."
)

// InvitationEmailNotVerified and InvitationEmailMismatch are the codes of the
// accept refusals that the hosted page turns into guidance.
const (
	InvitationCodeNotVerified = "email_not_verified"
	InvitationCodeMismatch    = "email_mismatch"
	InvitationCodeAccount     = "account_exists"
)

// InvitationError is an OrgError with a stable machine readable code.
type InvitationError struct {
	OrgError
	Code string
}

func invErr(status int, code, msg string) error {
	return &InvitationError{OrgError: OrgError{Status: status, Message: msg}, Code: code}
}

// InvitationErrorCode returns the code of an InvitationError ("" otherwise).
func InvitationErrorCode(err error) string {
	var ie *InvitationError
	if errors.As(err, &ie) {
		return ie.Code
	}

	return ""
}

// AsOrgError returns the OrgError carried by err (also inside an InvitationError).
func AsOrgError(err error) (*OrgError, bool) {
	var ie *InvitationError
	if errors.As(err, &ie) {
		return &ie.OrgError, true
	}

	var oe *OrgError
	if errors.As(err, &oe) {
		return oe, true
	}

	return nil, false
}

// NormalizeInvitationEmail trims and lower-cases an email address and checks
// that it is a plain address (no display name).
func NormalizeInvitationEmail(raw string) (string, error) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" || len(raw) > 254 {
		return "", orgErr(400, "A valid email address is required.")
	}

	addr, err := mail.ParseAddress(raw)
	if err != nil || addr.Address != raw || !strings.Contains(raw[strings.LastIndex(raw, "@"):], ".") {
		return "", orgErr(400, "A valid email address is required.")
	}

	return raw, nil
}

// HashInvitationToken returns the stored form of a token.
func HashInvitationToken(token string) string {
	sum := sha256.Sum256([]byte(token))

	return hex.EncodeToString(sum[:])
}

// newInvitationToken returns a fresh token (32 random bytes, base64url).
func newInvitationToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString(b), nil
}

// orgSettings parses the settings JSON of an org (empty on any problem).
func orgSettings(org *Record) map[string]any {
	out := map[string]any{}

	raw := org.GetString("settings")
	if raw == "" || raw == "null" {
		return out
	}

	_ = json.Unmarshal([]byte(raw), &out)

	return out
}

func orgSettingInt(org *Record, key string, def, lo, hi int) int {
	v, ok := orgSettings(org)[key].(float64)
	if !ok || int(v) < lo || int(v) > hi {
		return def
	}

	return int(v)
}

// InvitationLifetime is the life of an invitation of the org.
func InvitationLifetime(org *Record) time.Duration {
	return time.Duration(orgSettingInt(org, "invitationExpiryDays", InvitationLifetimeDays, 1, 30)) * 24 * time.Hour
}

// InvitationEmailOverride returns the org's own subject and body (either may
// be empty, meaning the default).
func InvitationEmailOverride(org *Record) (subject, body string) {
	m, _ := orgSettings(org)["invitationEmail"].(map[string]any)
	subject, _ = m["subject"].(string)
	body, _ = m["body"].(string)

	return subject, body
}

func invitationStamp(t time.Time) string { return t.UTC().Format(types.DefaultDateLayout) }

// IsInvitationExpired reports whether a pending invitation is past its expiry.
func IsInvitationExpired(inv *Record) bool {
	return inv.GetDateTime("expires").Time().Before(time.Now())
}

// EffectiveInvitationStatus is the status, with a pending invitation that is
// past its expiry reported as expired (before the cron marks it).
func EffectiveInvitationStatus(inv *Record) string {
	st := inv.GetString("status")
	if st == InvitationStatusPending && IsInvitationExpired(inv) {
		return InvitationStatusExpired
	}

	return st
}

// InvitationInput describes a new invitation.
type InvitationInput struct {
	Email string
	Role  string
	Tags  []string
}

// FindInvitationByToken loads the invitation of a token (any status).
func FindInvitationByToken(app App, token string) (*Record, error) {
	token = strings.TrimSpace(token)
	if token == "" || len(token) > 200 {
		return nil, orgErr(400, InvitationInvalidMessage)
	}

	hash := HashInvitationToken(token)

	inv, err := app.FindFirstRecordByData(CollectionNameInvitations, "tokenHash", hash)
	if err != nil || subtle.ConstantTimeCompare([]byte(inv.GetString("tokenHash")), []byte(hash)) != 1 {
		return nil, orgErr(400, InvitationInvalidMessage)
	}

	return inv, nil
}

// LookupInvitation resolves a token to a pending, unexpired invitation of a
// live org, or fails with the one generic error.
func LookupInvitation(app App, ctx context.Context, token string) (inv, org *Record, err error) {
	inv, err = FindInvitationByToken(app, token)
	if err != nil {
		return nil, nil, err
	}

	if inv.GetString("status") == InvitationStatusPending && IsInvitationExpired(inv) {
		inv.Set("status", InvitationStatusExpired)
		_ = app.SaveWithContext(ctx, inv) // lazy expiry; a failure only delays the cron
	}

	if inv.GetString("status") != InvitationStatusPending {
		return nil, nil, orgErr(400, InvitationInvalidMessage)
	}

	org, err = app.FindRecordById(CollectionNameOrgs, inv.GetString("org"))
	if err != nil || org.GetBool("archived") {
		return nil, nil, orgErr(400, InvitationInvalidMessage)
	}

	return inv, org, nil
}

func invitationActorMayHandle(actor *OrgActor, role string) error {
	if !actor.CanManage() {
		return orgErr(403, "Only owners and admins can manage invitations.")
	}

	if OrgRoleRank(role) > actor.rank() {
		return orgErr(403, "You cannot manage an invitation for a role above your own.")
	}

	return nil
}

// expireStaleInvitations marks the pending invitations of the org (optionally
// of one email) that are past their expiry as expired.
func expireStaleInvitations(app App, ctx context.Context, orgId, email string) error {
	filter := "org={:org} && status='pending' && expires<{:now}"
	params := dbx.Params{"org": orgId, "now": invitationStamp(time.Now())}

	if email != "" {
		filter += " && email={:email}"
		params["email"] = email
	}

	recs, err := app.FindRecordsByFilter(CollectionNameInvitations, filter, "", 0, 0, params)
	if err != nil {
		return err
	}

	for _, r := range recs {
		r.Set("status", InvitationStatusExpired)

		if err := app.SaveWithContext(ctx, r); err != nil {
			return err
		}
	}

	return nil
}

// ExpireOrgInvitations marks the pending invitations of one org that are past
// their expiry as expired.
func ExpireOrgInvitations(app App, ctx context.Context, orgId string) error {
	return expireStaleInvitations(app, ctx, orgId, "")
}

// invitationInviter returns the id to store in invitedBy: only a record of the
// member collection (superusers are in a different collection and are named
// by the audit entry).
func invitationInviter(app App, actor *OrgActor) string {
	if actor.Record == nil || actor.Superuser || actor.System {
		return ""
	}

	col, err := OrgUserCollection(app)
	if err != nil || actor.Record.Collection().Id != col.Id {
		return ""
	}

	return actor.Record.Id
}

// CreateInvitation creates a pending invitation and returns it together with
// the plain token (shown only once: it goes into the email).
//
// Guards: owners and admins (or a superuser) only; nobody invites above their
// own role; the org is live; at most InvitationsPerHour new invitations per
// org and hour; one pending invitation per email and org; the email is not
// already a member.
func CreateInvitation(app App, ctx context.Context, actor *OrgActor, org *Record, in InvitationInput) (*Record, string, error) {
	if in.Role == "" {
		in.Role = OrgRoleMember
	}

	if !validRole(in.Role) {
		return nil, "", orgErr(400, "Invalid role.")
	}

	if err := invitationActorMayHandle(actor, in.Role); err != nil {
		return nil, "", err
	}

	if err := requireLiveOrg(org, actor); err != nil {
		return nil, "", err
	}

	email, err := NormalizeInvitationEmail(in.Email)
	if err != nil {
		return nil, "", err
	}

	col, err := app.FindCachedCollectionByNameOrId(CollectionNameInvitations)
	if err != nil {
		return nil, "", orgErr(500, "Invitations are not available.")
	}

	token, err := newInvitationToken()
	if err != nil {
		return nil, "", err
	}

	var inv *Record

	txErr := app.RunInTransaction(func(txApp App) error {
		if err := expireStaleInvitations(txApp, ctx, org.Id, ""); err != nil {
			return err
		}

		limit := orgSettingInt(org, "invitationsPerHour", InvitationsPerHour, 1, 200)

		recent, err := txApp.FindRecordsByFilter(CollectionNameInvitations, "org={:org} && created>{:since}", "", limit, 0,
			dbx.Params{"org": org.Id, "since": invitationStamp(time.Now().Add(-time.Hour))})
		if err != nil {
			return err
		}

		if len(recent) >= limit {
			return orgErr(429, "Too many invitations were created for this organization in the last hour. Try again later.")
		}

		if usersCol, uerr := OrgUserCollection(txApp); uerr == nil {
			if u, uerr := txApp.FindAuthRecordByEmail(usersCol, email); uerr == nil {
				if _, merr := FindOrgMember(txApp, org.Id, u.Id); merr == nil {
					return orgErr(409, "That person is already a member of the organization.")
				}
			}
		}

		if _, err := txApp.FindFirstRecordByFilter(CollectionNameInvitations, "org={:org} && email={:email} && status='pending'",
			dbx.Params{"org": org.Id, "email": email}); err == nil {
			return orgErr(409, "An invitation for that email is already pending. Resend it instead.")
		}

		tags, err := ResolveOrgTags(txApp, ctx, org, in.Tags)
		if err != nil {
			return err
		}

		now := time.Now()

		inv = NewRecord(col)
		inv.Set("org", org.Id)
		inv.Set("email", email)
		inv.Set("role", in.Role)
		inv.Set("tags", tags)
		inv.Set("invitedBy", invitationInviter(txApp, actor))
		inv.Set("tokenHash", HashInvitationToken(token))
		inv.Set("status", InvitationStatusPending)
		inv.Set("expires", now.Add(InvitationLifetime(org)))
		inv.Set("lastSentAt", now)
		inv.Set("sendCount", 1)

		return saveOrgRecord(txApp, ctx, inv)
	})
	if txErr != nil {
		return nil, "", txErr
	}

	return inv, token, nil
}

// ResendInvitation rotates the token of a pending (or expired) invitation,
// restarts its expiry and returns the new plain token. At most one resend per
// InvitationResendInterval.
func ResendInvitation(app App, ctx context.Context, actor *OrgActor, org, inv *Record) (string, error) {
	if inv.GetString("org") != org.Id {
		return "", orgErr(404, "Invitation not found.")
	}

	if err := invitationActorMayHandle(actor, inv.GetString("role")); err != nil {
		return "", err
	}

	if err := requireLiveOrg(org, actor); err != nil {
		return "", err
	}

	token, err := newInvitationToken()
	if err != nil {
		return "", err
	}

	txErr := app.RunInTransaction(func(txApp App) error {
		fresh, err := txApp.FindRecordById(CollectionNameInvitations, inv.Id)
		if err != nil {
			return orgErr(404, "Invitation not found.")
		}

		switch fresh.GetString("status") {
		case InvitationStatusPending, InvitationStatusExpired:
		default:
			return orgErr(409, "Only pending invitations can be resent. Create a new invitation instead.")
		}

		if time.Since(fresh.GetDateTime("lastSentAt").Time()) < InvitationResendInterval {
			return orgErr(429, "This invitation was just sent. Wait a minute before resending it.")
		}

		if fresh.GetString("status") == InvitationStatusExpired {
			if _, err := txApp.FindFirstRecordByFilter(CollectionNameInvitations,
				"org={:org} && email={:email} && status='pending' && id!={:id}",
				dbx.Params{"org": org.Id, "email": fresh.GetString("email"), "id": fresh.Id}); err == nil {
				return orgErr(409, "Another invitation for that email is pending.")
			}
		}

		now := time.Now()

		fresh.Set("status", InvitationStatusPending)
		fresh.Set("tokenHash", HashInvitationToken(token))
		fresh.Set("expires", now.Add(InvitationLifetime(org)))
		fresh.Set("lastSentAt", now)
		fresh.Set("sendCount", fresh.GetInt("sendCount")+1)

		if err := saveOrgRecord(txApp, ctx, fresh); err != nil {
			return err
		}

		inv.Load(fresh.FieldsData())

		return nil
	})
	if txErr != nil {
		return "", txErr
	}

	return token, nil
}

// RevokeInvitation ends a pending invitation; its link stops working.
func RevokeInvitation(app App, ctx context.Context, actor *OrgActor, org, inv *Record) error {
	if inv.GetString("org") != org.Id {
		return orgErr(404, "Invitation not found.")
	}

	if err := invitationActorMayHandle(actor, inv.GetString("role")); err != nil {
		return err
	}

	return app.RunInTransaction(func(txApp App) error {
		fresh, err := txApp.FindRecordById(CollectionNameInvitations, inv.Id)
		if err != nil {
			return orgErr(404, "Invitation not found.")
		}

		if fresh.GetString("status") != InvitationStatusPending {
			return orgErr(409, "Only pending invitations can be revoked.")
		}

		fresh.Set("status", InvitationStatusRevoked)

		if err := saveOrgRecord(txApp, ctx, fresh); err != nil {
			return err
		}

		inv.Load(fresh.FieldsData())

		return nil
	})
}

// InvitationEmailMatches reports whether the signed-in account may use the
// invitation: same email (case-insensitive) and a verified address. Accounts
// outside the member collection (superusers) never match.
func InvitationEmailMatches(app App, inv, user *Record) error {
	col, err := OrgUserCollection(app)
	if err != nil || user == nil || user.Collection().Id != col.Id {
		return invErr(403, InvitationCodeMismatch, "Sign in with the account that received the invitation.")
	}

	if !strings.EqualFold(strings.TrimSpace(user.Email()), inv.GetString("email")) {
		return invErr(403, InvitationCodeMismatch,
			"This invitation was sent to a different email address. Sign in with the invited address, or ask for a new invitation.")
	}

	if !user.Verified() {
		return invErr(403, InvitationCodeNotVerified,
			"Verify your email address first, then accept the invitation.")
	}

	return nil
}

// AcceptInvitation adds the signed-in user to the org and marks the
// invitation accepted, in one transaction. The user's email must match the
// invitation and be verified (an unverified account is never linked).
func AcceptInvitation(app App, ctx context.Context, token string, user *Record) (member, inv *Record, err error) {
	inv, org, err := LookupInvitation(app, ctx, token)
	if err != nil {
		return nil, nil, err
	}

	if err := InvitationEmailMatches(app, inv, user); err != nil {
		return nil, nil, err
	}

	txErr := app.RunInTransaction(func(txApp App) error {
		// re-read inside the transaction: the single-use check
		fresh, err := txApp.FindRecordById(CollectionNameInvitations, inv.Id)
		if err != nil || fresh.GetString("status") != InvitationStatusPending || IsInvitationExpired(fresh) {
			return orgErr(400, InvitationInvalidMessage)
		}

		tagNames := OrgTagNames(txApp, fresh)

		member, err = AddOrgMember(txApp, ctx, &OrgActor{System: true}, org, OrgMemberInput{
			User:   user.Id,
			Role:   fresh.GetString("role"),
			Tags:   tagNames,
			Source: OrgSourceInvitation,
		})
		if err != nil {
			var oe *OrgError
			if errors.As(err, &oe) && oe.Status == 409 {
				return orgErr(409, "You are already a member of this organization.")
			}

			return err
		}

		fresh.Set("status", InvitationStatusAccepted)
		fresh.Set("acceptedBy", user.Id)
		fresh.Set("acceptedAt", time.Now())

		if err := saveOrgRecord(txApp, ctx, fresh); err != nil {
			return err
		}

		inv = fresh

		return nil
	})
	if txErr != nil {
		return nil, nil, txErr
	}

	return member, inv, nil
}

// ExpireInvitations marks every pending invitation past its expiry as expired.
func ExpireInvitations(app App, ctx context.Context) (int, error) {
	if _, err := app.FindCachedCollectionByNameOrId(CollectionNameInvitations); err != nil {
		return 0, nil
	}

	recs, err := app.FindRecordsByFilter(CollectionNameInvitations, "status='pending' && expires<{:now}", "", 0, 0,
		dbx.Params{"now": invitationStamp(time.Now())})
	if err != nil {
		return 0, err
	}

	n := 0

	for _, r := range recs {
		r.Set("status", InvitationStatusExpired)

		if err := app.SaveWithContext(ctx, r); err != nil {
			return n, err
		}

		n++
	}

	return n, nil
}

// DeleteOldInvitations deletes accepted, revoked and expired invitations that
// have not changed for InvitationRetentionDays.
func DeleteOldInvitations(app App, ctx context.Context) (int, error) {
	if _, err := app.FindCachedCollectionByNameOrId(CollectionNameInvitations); err != nil {
		return 0, nil
	}

	cutoff := invitationStamp(time.Now().AddDate(0, 0, -InvitationRetentionDays))
	total := 0

	for {
		recs, err := app.FindRecordsByFilter(CollectionNameInvitations, "status!='pending' && updated<{:cutoff}", "", 200, 0,
			dbx.Params{"cutoff": cutoff})
		if err != nil {
			return total, err
		}

		if len(recs) == 0 {
			return total, nil
		}

		for _, r := range recs {
			if err := app.DeleteWithContext(ctx, r); err != nil {
				return total, err
			}

			total++
		}
	}
}

// ---- hooks ---------------------------------------------------------------

func (app *BaseApp) registerInvitationHooks() {
	app.OnRecordValidate(CollectionNameInvitations).Bind(&hook.Handler[*RecordEvent]{
		Id: "__pbInvitationValidate__",
		Func: func(e *RecordEvent) error {
			if !validRole(e.Record.GetString("role")) {
				return orgErr(400, "Invalid role.")
			}

			if err := validateOrgMemberTags(e.App, e.Record); err != nil {
				return err
			}

			return e.Next()
		},
	})

	app.Cron().Add(invitationCleanupCronId, invitationCleanupCronExpr, func() {
		ctx := WithAuditContext(context.Background(), AuditActor{Type: AuditActorTypeSystem}, "")

		if _, err := ExpireInvitations(app, ctx); err != nil {
			app.Logger().Warn("Failed to expire invitations", "error", err)
		}

		if _, err := DeleteOldInvitations(app, ctx); err != nil {
			app.Logger().Warn("Failed to delete old invitations", "error", err)
		}
	})
}
