package core

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #84, 84-4): member transfers between orgs, with consent.
//
// A transfer moves one person from a source org to a target org. It is a row
// of the system collection _orgTransfers (pending, accepted, declined,
// cancelled, expired). An admin of the source org, or the person, starts it.
// Only the person can accept it, while signed in. An admin (or a superuser)
// can never complete it alone. When the person is a minor with a login (a
// household member flagged isMinor), an adult guardian of their household
// decides in their place.
//
// Accepting runs in one transaction: the target membership is created, the
// source membership is suspended or removed (chosen when the transfer started),
// the transfer is closed and one org_transfer_accept audit entry names both
// orgs. The records the person wrote stay in the source org; OrgTransferStays
// counts them per collection for the admins.
//
// The collection is excluded from the automatic record audit: the explicit
// org_transfer_* entries describe each step once.

const (
	CollectionNameOrgTransfers = "_orgTransfers"

	OrgTransferPending   = "pending"
	OrgTransferAccepted  = "accepted"
	OrgTransferDeclined  = "declined"
	OrgTransferCancelled = "cancelled"
	OrgTransferExpired   = "expired"

	// what happens to the source membership when the transfer is accepted
	OrgTransferSuspend = "suspend"
	OrgTransferRemove  = "remove"

	AuditActionOrgTransferStart   = "org_transfer_start"
	AuditActionOrgTransferAccept  = "org_transfer_accept"
	AuditActionOrgTransferDecline = "org_transfer_decline"
	AuditActionOrgTransferCancel  = "org_transfer_cancel"
	AuditActionOrgTransferExpire  = "org_transfer_expire"

	// OrgTransferDaysEnv sets the default lifetime of a pending transfer in
	// days (1 to 90, default 14). An org can override it with the "transferDays"
	// key of its settings.
	OrgTransferDaysEnv     = "CLAVIGER_ORG_TRANSFER_DAYS"
	DefaultOrgTransferDays = 14

	orgTransferCronId   = "__pbOrgTransferExpiry__"
	orgTransferCronExpr = "41 5 * * *" // daily at 05:41
)

// OrgTransferStates and OrgTransferOutcomes are the select values.
var (
	OrgTransferStates   = []string{OrgTransferPending, OrgTransferAccepted, OrgTransferDeclined, OrgTransferCancelled, OrgTransferExpired}
	OrgTransferOutcomes = []string{OrgTransferSuspend, OrgTransferRemove}
)

// OrgTransferNotice is what the notifier needs to tell people about a transfer.
type OrgTransferNotice struct {
	Transfer *Record
	FromOrg  *Record
	ToOrg    *Record
	// Person is the person who would move; Recipients are who receives the
	// notice (the person, or the guardians of a minor).
	Person     *Record
	Recipients []*Record
	// Starter is a display name of whoever started it ("" when unknown).
	Starter string
}

// OrgTransferNotifier delivers the notice by email. The mails package sets it
// (core cannot import it); nil means no email is sent. It must skip
// recipients without an address.
var OrgTransferNotifier func(app App, n *OrgTransferNotice) error

// OrgTransfersAvailable reports whether the transfers collection exists.
func OrgTransfersAvailable(app App) bool {
	_, err := app.FindCachedCollectionByNameOrId(CollectionNameOrgTransfers)

	return err == nil
}

// OrgTransferLifetime is how long a transfer started from org stays pending:
// the org's "transferDays" setting, else CLAVIGER_ORG_TRANSFER_DAYS, else 14.
func OrgTransferLifetime(org *Record) time.Duration {
	days := envInt(OrgTransferDaysEnv, DefaultOrgTransferDays, 1, 90)

	if org != nil {
		switch v := orgSettings(org)["transferDays"].(type) {
		case float64:
			if v >= 1 && v <= 90 && v == float64(int(v)) {
				days = int(v)
			}
		}
	}

	return time.Duration(days) * 24 * time.Hour
}

// OrgTransferInput describes a new transfer.
type OrgTransferInput struct {
	// User is a record id or email of the member collection.
	User string
	// To is the id or slug of the target org.
	To string
	// Outcome is "suspend" (default) or "remove".
	Outcome string
}

// ---- who decides -----------------------------------------------------------

// minorGuardianRow returns the household row that makes the person a minor
// with a login in the org, or nil.
func minorHouseholdRow(app App, orgId, userId string) *Record {
	if !HouseholdsAvailable(app) {
		return nil
	}

	m, err := app.FindFirstRecordByFilter(CollectionNameHouseholdMembers,
		"user={:u} && isMinor=true && status={:s} && household.org={:o}",
		dbx.Params{"u": userId, "s": HouseholdMemberActive, "o": orgId})
	if err != nil {
		return nil
	}

	return m
}

// isGuardianOf reports whether auth is an active adult guardian in the
// household of the minor's row.
func isGuardianOf(app App, minorRow *Record, authId string) bool {
	if minorRow == nil || authId == "" {
		return false
	}

	_, err := app.FindFirstRecordByFilter(CollectionNameHouseholdMembers,
		"household={:h} && user={:g} && guardian=true && isMinor=false && status={:s}",
		dbx.Params{"h": minorRow.GetString("household"), "g": authId, "s": HouseholdMemberActive})

	return err == nil
}

// OrgTransferDecider reports whether auth may accept or decline the transfer:
// the person themself, or for a minor with a login an adult guardian of their
// household (the minor cannot decide alone). A superuser or an org admin never
// can.
func OrgTransferDecider(app App, t *Record, auth *Record) bool {
	if auth == nil || auth.IsSuperuser() {
		return false
	}

	col, err := OrgUserCollection(app)
	if err != nil || auth.Collection().Id != col.Id {
		return false
	}

	person := t.GetString("person")

	if minor := minorHouseholdRow(app, t.GetString("fromOrg"), person); minor != nil {
		return isGuardianOf(app, minor, auth.Id)
	}

	return auth.Id == person
}

// GuardedUserIds lists the users with a login whose transfers the user may
// decide as a guardian.
func GuardedUserIds(app App, userId string) []string {
	if !HouseholdsAvailable(app) {
		return nil
	}

	mine, err := app.FindRecordsByFilter(CollectionNameHouseholdMembers,
		"user={:g} && guardian=true && isMinor=false && status={:s}", "", 0, 0,
		dbx.Params{"g": userId, "s": HouseholdMemberActive})
	if err != nil || len(mine) == 0 {
		return nil
	}

	hs := make([]any, 0, len(mine))
	for _, m := range mine {
		hs = append(hs, m.GetString("household"))
	}

	minors, err := app.FindAllRecords(CollectionNameHouseholdMembers,
		dbx.In("household", hs...), dbx.HashExp{"isMinor": true, "status": HouseholdMemberActive}, dbx.NewExp("[[user]] != ''"))
	if err != nil {
		return nil
	}

	out := []string{}

	for _, m := range minors {
		if u := m.GetString("user"); u != "" && !slices.Contains(out, u) {
			out = append(out, u)
		}
	}

	return out
}

// ---- start -------------------------------------------------------------------

// StartOrgTransfer opens a pending transfer of a member of org to another org.
//
// Who may start: an owner or admin of the source org (or a superuser), the
// person themself, or a guardian of a minor. Guards: not to the same org; the
// target exists and is not archived; the person is an active member of the
// source and not yet a member of the target; no other pending transfer of the
// person out of the source; an admin cannot move someone ranked above them;
// the last active owner cannot be moved.
func StartOrgTransfer(app App, ctx context.Context, actor *OrgActor, org *Record, in OrgTransferInput) (*Record, error) {
	if !OrgTransfersAvailable(app) {
		return nil, orgErr(404, "Transfers are not available.")
	}

	if actor.Record == nil {
		return nil, orgErr(401, "Authentication required.")
	}

	user, err := FindOrgUser(app, in.User)
	if err != nil {
		return nil, err
	}

	self := !actor.Superuser && !actor.System && actor.Record.Id == user.Id
	guardian := !self && !actor.Superuser && !actor.System && isGuardianOf(app, minorHouseholdRow(app, org.Id, user.Id), actor.Record.Id)

	if !actor.CanManage() && !self && !guardian {
		return nil, orgErr(403, "Only owners and admins, the person or a guardian can start a transfer.")
	}

	// a minor with a login does not start or decide alone
	if self && minorHouseholdRow(app, org.Id, user.Id) != nil {
		return nil, orgErr(403, "A guardian must start the transfer of a minor.")
	}

	if err := requireLiveOrg(org, actor); err != nil {
		return nil, err
	}

	outcome := in.Outcome
	if outcome == "" {
		outcome = OrgTransferSuspend
	}

	if !slices.Contains(OrgTransferOutcomes, outcome) {
		return nil, orgErr(400, "The outcome must be suspend or remove.")
	}

	target, err := FindOrg(app, strings.TrimSpace(in.To))
	if err != nil {
		return nil, orgErr(404, "The target organization was not found.")
	}

	if target.Id == org.Id {
		return nil, orgErr(400, "The target must be another organization.")
	}

	if target.GetBool("archived") {
		return nil, orgErr(409, "The target organization is archived.")
	}

	member, err := FindOrgMember(app, org.Id, user.Id)
	if err != nil || member.GetString("status") != OrgStatusActive {
		return nil, orgErr(404, "The person is not an active member of the organization.")
	}

	if !self && !guardian && OrgRoleRank(member.GetString("role")) > actor.rank() {
		return nil, orgErr(403, "You cannot transfer a member ranked above you.")
	}

	if member.GetString("role") == OrgRoleOwner && countOtherActiveOwners(app, org.Id, member.Id) == 0 {
		return nil, orgErr(409, "The last owner of an organization cannot be transferred.")
	}

	if _, err := FindOrgMember(app, target.Id, user.Id); err == nil {
		return nil, orgErr(409, "The person is already a member of the target organization.")
	}

	_ = ExpireOrgTransfers(app, ctx, time.Now())

	if n, _ := app.CountRecords(CollectionNameOrgTransfers,
		dbx.HashExp{"person": user.Id, "fromOrg": org.Id, "state": OrgTransferPending}); n > 0 {
		return nil, orgErr(409, "A transfer of this person out of the organization is already pending.")
	}

	col, err := app.FindCachedCollectionByNameOrId(CollectionNameOrgTransfers)
	if err != nil {
		return nil, err
	}

	t := NewRecord(col)
	t.Set("person", user.Id)
	t.Set("fromOrg", org.Id)
	t.Set("toOrg", target.Id)
	t.Set("state", OrgTransferPending)
	t.Set("sourceOutcome", outcome)
	t.Set("startedBy", actor.Record.Id)
	t.Set("expiresAt", types.NowDateTime().Add(OrgTransferLifetime(org)))

	err = app.RunInTransaction(func(txApp App) error {
		if err := txApp.SaveWithContext(ctx, t); err != nil {
			return wrapOrgSaveErr(err)
		}

		return writeOrgTransferAudit(txApp, ctx, AuditActionOrgTransferStart, t, org.Id, nil)
	})
	if err != nil {
		return nil, err
	}

	notifyOrgTransfer(app, t, org, target, user, actor.Record)

	return t, nil
}

func notifyOrgTransfer(app App, t *Record, from, to, person, starter *Record) {
	if OrgTransferNotifier == nil || starter.Id == person.Id {
		return
	}

	recipients := []*Record{person}

	if minor := minorHouseholdRow(app, from.Id, person.Id); minor != nil {
		recipients = recipients[:0]

		gs, err := app.FindRecordsByFilter(CollectionNameHouseholdMembers,
			"household={:h} && guardian=true && isMinor=false && status={:s} && user!=''", "", 0, 0,
			dbx.Params{"h": minor.GetString("household"), "s": HouseholdMemberActive})
		if err == nil {
			for _, g := range gs {
				if u, err := app.FindRecordById(OrgUserCollectionName(), g.GetString("user")); err == nil {
					recipients = append(recipients, u)
				}
			}
		}
	}

	name := starter.GetString("name")
	if name == "" {
		name = starter.Email()
	}

	n := &OrgTransferNotice{Transfer: t, FromOrg: from, ToOrg: to, Person: person, Recipients: recipients, Starter: name}

	if err := OrgTransferNotifier(app, n); err != nil {
		app.Logger().Warn("Failed to send the transfer notice", "transfer", t.Id, "error", err.Error())
	}
}

// ---- lifecycle -----------------------------------------------------------------

// loadPendingTransfer re-reads the transfer and refuses one that is not
// pending. A pending transfer past its expiry is closed first.
func loadPendingTransfer(app App, ctx context.Context, t *Record) (*Record, error) {
	fresh, err := app.FindRecordById(CollectionNameOrgTransfers, t.Id)
	if err != nil {
		return nil, orgErr(404, "Transfer not found.")
	}

	if fresh.GetString("state") == OrgTransferPending && !fresh.GetDateTime("expiresAt").Time().After(time.Now()) {
		_ = expireOrgTransfer(app, ctx, fresh)

		return nil, orgErr(409, "The transfer has expired.")
	}

	if fresh.GetString("state") != OrgTransferPending {
		return nil, orgErr(409, "The transfer is no longer pending ("+fresh.GetString("state")+").")
	}

	return fresh, nil
}

func closeOrgTransfer(app App, ctx context.Context, t *Record, state, action, decidedBy string, meta map[string]any) error {
	return app.RunInTransaction(func(txApp App) error {
		fresh, err := txApp.FindRecordById(CollectionNameOrgTransfers, t.Id)
		if err != nil {
			return orgErr(404, "Transfer not found.")
		}

		if fresh.GetString("state") != OrgTransferPending {
			return orgErr(409, "The transfer is no longer pending.")
		}

		fresh.Set("state", state)
		fresh.Set("decidedAt", types.NowDateTime())
		fresh.Set("decidedBy", decidedBy)

		if err := txApp.SaveWithContext(ctx, fresh); err != nil {
			return wrapOrgSaveErr(err)
		}

		return writeOrgTransferAudit(txApp, ctx, action, fresh, fresh.GetString("fromOrg"), meta)
	})
}

// Sides of a decline, recorded in the audit entry as meta.side.
const (
	OrgTransferSidePerson = "person"
	OrgTransferSideTarget = "target"
)

// DeclineOrgTransfer closes the transfer at the request of the person (or the
// guardian of a minor), or as a veto by an owner or admin of the target org.
// The target side can only decline: accepting stays with the person. The audit
// entry records who declined (meta.decidedBy) and which side (meta.side).
func DeclineOrgTransfer(app App, ctx context.Context, auth *Record, t *Record) (*Record, error) {
	t, err := loadPendingTransfer(app, ctx, t)
	if err != nil {
		return nil, err
	}

	side := OrgTransferSidePerson

	if !OrgTransferDecider(app, t, auth) {
		if !isTargetStaff(app, t, auth) {
			return nil, orgErr(403, "Only the person or an admin of the receiving organization can decline this transfer.")
		}

		side = OrgTransferSideTarget
	}

	meta := map[string]any{"side": side, "decidedBy": auth.Id}

	if err := closeOrgTransfer(app, ctx, t, OrgTransferDeclined, AuditActionOrgTransferDecline, auth.Id, meta); err != nil {
		return nil, err
	}

	return app.FindRecordById(CollectionNameOrgTransfers, t.Id)
}

// isTargetStaff reports whether auth is an active owner or admin of the
// receiving org. A superuser does not count: the veto belongs to the org.
func isTargetStaff(app App, t *Record, auth *Record) bool {
	if auth == nil || auth.IsSuperuser() {
		return false
	}

	to, err := app.FindRecordById(CollectionNameOrgs, t.GetString("toOrg"))
	if err != nil {
		return false
	}

	return NewOrgActor(app, to, auth).CanManage()
}

// CancelOrgTransfer withdraws a pending transfer: whoever started it, an owner
// or admin of the source org (or a superuser), or the person.
func CancelOrgTransfer(app App, ctx context.Context, auth *Record, t *Record) (*Record, error) {
	t, err := loadPendingTransfer(app, ctx, t)
	if err != nil {
		return nil, err
	}

	allowed := OrgTransferDecider(app, t, auth) || (auth != nil && auth.Id == t.GetString("startedBy"))

	if !allowed && auth != nil {
		if from, err := app.FindRecordById(CollectionNameOrgs, t.GetString("fromOrg")); err == nil {
			allowed = NewOrgActor(app, from, auth).CanManage()
		}
	}

	if !allowed {
		return nil, orgErr(403, "You cannot cancel this transfer.")
	}

	if err := closeOrgTransfer(app, ctx, t, OrgTransferCancelled, AuditActionOrgTransferCancel, auth.Id, nil); err != nil {
		return nil, err
	}

	return app.FindRecordById(CollectionNameOrgTransfers, t.Id)
}

// OrgTransferResult is the outcome of an accepted transfer.
type OrgTransferResult struct {
	Transfer *Record
	// Membership is the new membership in the target org.
	Membership *Record
	// Stays counts the records of the person that stay in the source org.
	Stays []OrgTransferStay
}

// AcceptOrgTransfer completes the transfer. Only the person (or the guardian
// of a minor) can; the call is refused for everyone else, whatever their role.
// One transaction creates the target membership, suspends or removes the
// source membership, closes the transfer and writes the audit entry.
func AcceptOrgTransfer(app App, ctx context.Context, auth *Record, t *Record) (*OrgTransferResult, error) {
	t, err := loadPendingTransfer(app, ctx, t)
	if err != nil {
		return nil, err
	}

	if !OrgTransferDecider(app, t, auth) {
		return nil, orgErr(403, "Only the person can accept their transfer. An admin cannot accept it for them.")
	}

	members, err := app.FindCachedCollectionByNameOrId(CollectionNameOrgMembers)
	if err != nil {
		return nil, err
	}

	res := &OrgTransferResult{}

	err = app.RunInTransaction(func(txApp App) error {
		fresh, err := txApp.FindRecordById(CollectionNameOrgTransfers, t.Id)
		if err != nil || fresh.GetString("state") != OrgTransferPending {
			return orgErr(409, "The transfer is no longer pending.")
		}

		from, err := txApp.FindRecordById(CollectionNameOrgs, fresh.GetString("fromOrg"))
		if err != nil || from.GetBool("archived") {
			return orgErr(409, "The source organization is not available.")
		}

		to, err := txApp.FindRecordById(CollectionNameOrgs, fresh.GetString("toOrg"))
		if err != nil || to.GetBool("archived") {
			return orgErr(409, "The target organization is not available.")
		}

		person := fresh.GetString("person")

		src, err := FindOrgMember(txApp, from.Id, person)
		if err != nil || src.GetString("status") != OrgStatusActive {
			return orgErr(409, "The person is no longer an active member of the source organization.")
		}

		if _, err := FindOrgMember(txApp, to.Id, person); err == nil {
			return orgErr(409, "The person is already a member of the target organization.")
		}

		dst := NewRecord(members)
		dst.Set("org", to.Id)
		dst.Set("user", person)
		dst.Set("role", OrgRoleMember)
		dst.Set("status", OrgStatusActive)
		dst.Set("source", OrgSourceManual)

		if err := saveOrgRecord(txApp, ctx, dst); err != nil {
			return err
		}

		outcome := fresh.GetString("sourceOutcome")

		if outcome == OrgTransferRemove {
			err = deleteOrgRecord(txApp, ctx, src)
		} else {
			src.Set("status", OrgStatusSuspended)
			err = saveOrgRecord(txApp, ctx, src)
		}

		if err != nil {
			return err
		}

		fresh.Set("state", OrgTransferAccepted)
		fresh.Set("decidedAt", types.NowDateTime())
		fresh.Set("decidedBy", auth.Id)

		if err := txApp.SaveWithContext(ctx, fresh); err != nil {
			return wrapOrgSaveErr(err)
		}

		res.Transfer = fresh
		res.Membership = dst

		return writeOrgTransferAudit(txApp, ctx, AuditActionOrgTransferAccept, fresh, from.Id, map[string]any{
			"fromOrg": from.Id,
			"toOrg":   to.Id,
			"outcome": outcome,
		})
	})
	if err != nil {
		return nil, err
	}

	res.Stays, _ = OrgTransferStays(app, t.GetString("fromOrg"), t.GetString("person"))

	return res, nil
}

// ExpireOrgTransfers closes every pending transfer past its expiry.
func ExpireOrgTransfers(app App, ctx context.Context, now time.Time) error {
	if !OrgTransfersAvailable(app) {
		return nil
	}

	due, err := app.FindRecordsByFilter(CollectionNameOrgTransfers, "state='pending' && expiresAt<{:now}", "", 0, 0,
		dbx.Params{"now": now.UTC().Format(types.DefaultDateLayout)})
	if err != nil {
		return err
	}

	var errs []error

	for _, t := range due {
		if err := expireOrgTransfer(app, ctx, t); err != nil && !errors.Is(err, context.Canceled) {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func expireOrgTransfer(app App, ctx context.Context, t *Record) error {
	if ctx == nil || ctx == context.Background() {
		ctx = WithAuditContext(context.Background(), AuditActor{Type: AuditActorTypeSystem}, "")
	}

	return closeOrgTransfer(app, ctx, t, OrgTransferExpired, AuditActionOrgTransferExpire, "", nil)
}

// ---- what stays behind ---------------------------------------------------------

// OrgTransferStay is the number of rows of one collection that are about or by
// the person in the source org and stay there.
type OrgTransferStay struct {
	Collection string `json:"collection"`
	Count      int64  `json:"count"`
}

// orgTransferStayExcluded are the collections that describe the move itself.
var orgTransferStayExcluded = []string{CollectionNameOrgMembers, CollectionNameOrgTransfers, CollectionNameOrgs, CollectionNameOrgRoles}

// OrgTransferStays counts, per collection of the data map, the rows that link
// the person to the org: collections with a single `org` relation and a
// direct relation to the user collection among their subject paths. The rows
// themselves are never returned. Collections whose link to the org is another
// field, or is reached through a relation, are not counted.
func OrgTransferStays(app App, orgId, userId string) ([]OrgTransferStay, error) {
	rows, err := loadDataMapRows(app)
	if err != nil {
		return nil, err
	}

	userCol, err := OrgUserCollection(app)
	if err != nil {
		return nil, err
	}

	out := []OrgTransferStay{}

	for _, r := range rows {
		if r.Treatment == DataMapTreatmentNone || slices.Contains(orgTransferStayExcluded, r.Collection) {
			continue
		}

		col, err := app.FindCachedCollectionByNameOrId(r.Collection)
		if err != nil {
			continue
		}

		orgField, _ := col.Fields.GetByName("org").(*RelationField)
		if orgField == nil || orgField.IsMultiple() {
			continue
		}

		for _, p := range r.SubjectPaths {
			f, _ := col.Fields.GetByName(p).(*RelationField)
			if f == nil || f.IsMultiple() || f.CollectionId != userCol.Id {
				continue
			}

			n, err := app.CountRecords(col, dbx.HashExp{"org": orgId, p: userId})
			if err != nil {
				return nil, err
			}

			if n > 0 {
				out = append(out, OrgTransferStay{Collection: r.Collection, Count: n})
			}

			break
		}
	}

	slices.SortFunc(out, func(a, b OrgTransferStay) int { return strings.Compare(a.Collection, b.Collection) })

	return out, nil
}

// ---- audit ---------------------------------------------------------------------

func writeOrgTransferAudit(app App, ctx context.Context, action string, t *Record, orgId string, extra map[string]any) error {
	if !auditAvailable(app) {
		return nil
	}

	meta := map[string]any{
		"orgId":   orgId,
		"fromOrg": t.GetString("fromOrg"),
		"toOrg":   t.GetString("toOrg"),
		"user":    t.GetString("person"),
		"outcome": t.GetString("sourceOutcome"),
	}

	for k, v := range extra {
		meta[k] = v
	}

	entry := &AuditEntry{
		Action:         action,
		CollectionName: CollectionNameOrgTransfers,
		RecordId:       t.Id,
		Meta:           meta,
	}

	if c, err := app.FindCachedCollectionByNameOrId(CollectionNameOrgTransfers); err == nil {
		entry.CollectionId = c.Id
	}

	applyAuditContext(ctx, entry)

	return WriteAuditEntry(app, ctx, entry, nil)
}

// ---- cron ------------------------------------------------------------------------

func (app *BaseApp) registerOrgTransferHooks() {
	app.Cron().Add(orgTransferCronId, orgTransferCronExpr, func() {
		ctx := WithAuditContext(context.Background(), AuditActor{Type: AuditActorTypeSystem}, "")

		if err := ExpireOrgTransfers(app, ctx, time.Now()); err != nil && !errors.Is(err, context.Canceled) {
			app.Logger().Warn("Failed to expire the pending transfers", "error", err.Error())
		}
	})
}
