package core

import (
	"context"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #86, 86-2 and 86-4): break-glass access to a pastoral
// note.
//
// A holder of the "breakglass" tag asks for time-boxed access to one note, with
// a reason of at least 20 characters and a basis (fresh passkey required, in
// the route). Access is never instant: a DIFFERENT holder of the tag in the
// note's org must approve (also with a fresh passkey); approving one's own
// request is refused. On approval the note's author and the org owners are told
// by email (when the author has no usable address, the other holders of the
// tag are told as well). The notice cannot be turned off, and if nobody can
// be reached the grant is not given. There is no deferred notice mode.
//
// A grant is a row of the system collection _breakGlass. The read rules of the
// template (PastoralBreakGlassClause) look at activeFrom, expires, revoked and
// approvedBy, so the clock closes a grant with no job. A grant is also revoked
// at once when the grantee is suspended or removed from the org
// (registerPastoralHooks), and by the org owner, the note's author or the
// grantee.
//
// The grant length is at most 24 hours. An org can shorten it with the
// "breakglassHours" key of its settings; it can never lengthen it.
//
// The collection is excluded from the automatic record audit: the explicit
// breakglass_request, breakglass_open and breakglass_revoke entries describe
// each step once. The reason is in the grant and in the entry; it is readable
// by org owners only (and by the grantee about their own grant).

const (
	AuditActionBreakGlassRequest = "breakglass_request"
	AuditActionBreakGlassOpen    = "breakglass_open"
	AuditActionBreakGlassRevoke  = "breakglass_revoke"

	BreakGlassBasisSafety           = "safety"
	BreakGlassBasisLegal            = "legal"
	BreakGlassBasisOwnerUnavailable = "owner_unavailable"
	BreakGlassBasisOther            = "other"

	// BreakGlassMaxHours is the longest grant. An org setting can only shorten it.
	BreakGlassMaxHours = 24
	// BreakGlassPendingHours is how long a request waits for its approval.
	BreakGlassPendingHours = 24
	// BreakGlassMinReason and BreakGlassMaxReason bound the reason (characters).
	BreakGlassMinReason = 20
	BreakGlassMaxReason = 2000

	BreakGlassPending = "pending"
	BreakGlassActive  = "active"
	BreakGlassExpired = "expired"
	BreakGlassRevoked = "revoked"
	// BreakGlassLapsed is a request nobody approved in time.
	BreakGlassLapsed = "lapsed"
)

// BreakGlassBases are the values of the basis field.
var BreakGlassBases = []string{
	BreakGlassBasisSafety, BreakGlassBasisLegal, BreakGlassBasisOwnerUnavailable, BreakGlassBasisOther,
}

// BreakGlassNotice is what the notifier needs to tell people that access to a
// note was approved.
type BreakGlassNotice struct {
	Grant *Record
	Note  *Record
	Org   *Record
	// Grantee and Approver are the two people.
	Grantee  *Record
	Approver *Record
	Expires  time.Time
	// Recipients are who is told: the author and the org owners, plus the other
	// holders of the tag when the author has no usable address.
	Recipients []*Record
	// AuthorReachable is false when the author could not be told by email.
	AuthorReachable bool
}

// BreakGlassNotifier delivers the notice by email and returns how many
// recipients it reached. The mails package sets it (core cannot import it).
// Nil means nobody can be reached, so no grant is ever approved.
var BreakGlassNotifier func(app App, n *BreakGlassNotice) (int, error)

// BreakGlassAvailable reports whether the break-glass collection exists.
func BreakGlassAvailable(app App) bool {
	_, err := app.FindCachedCollectionByNameOrId(CollectionNameBreakGlass)

	return err == nil
}

// BreakGlassMaxFor is the longest grant of the org: 24 hours, or the shorter
// "breakglassHours" setting (1 to 24; a larger or invalid value is ignored).
func BreakGlassMaxFor(org *Record) time.Duration {
	return time.Duration(orgSettingInt(org, "breakglassHours", BreakGlassMaxHours, 1, BreakGlassMaxHours)) * time.Hour
}

// BreakGlassState derives the state of a grant at the moment.
func BreakGlassState(g *Record, now time.Time) string {
	switch {
	case g.GetBool("revoked"):
		return BreakGlassRevoked
	case g.GetDateTime("activeFrom").IsZero():
		if g.GetDateTime("requestedAt").Time().Add(BreakGlassPendingHours * time.Hour).Before(now) {
			return BreakGlassLapsed
		}

		return BreakGlassPending
	case !g.GetDateTime("expires").Time().After(now):
		return BreakGlassExpired
	default:
		return BreakGlassActive
	}
}

// IsBreakGlassHolder reports whether the user is an active member of the org
// who carries the breakglass tag.
func IsBreakGlassHolder(app App, org *Record, userId string) bool {
	m := ActiveOrgMember(app, org, userId)

	return m != nil && slices.Contains(OrgTagNames(app, m), PastoralTagBreakGlass)
}

func isOrgOwner(app App, org *Record, userId string) bool {
	m := ActiveOrgMember(app, org, userId)

	return m != nil && m.GetString("role") == OrgRoleOwner
}

// BreakGlassInput describes a request.
type BreakGlassInput struct {
	Collection string
	RecordId   string
	Reason     string
	Basis      string
	// Hours is the wanted length (default and maximum: the org's limit).
	Hours int
}

func loadPastoralNote(app App, collection, id string) (*Collection, *Record, *Record, error) {
	col, err := app.FindCachedCollectionByNameOrId(collection)
	if err != nil || !IsPastoralCollection(col) {
		return nil, nil, nil, orgErr(404, "Note not found.")
	}

	note, err := app.FindRecordById(col, id)
	if err != nil {
		return nil, nil, nil, orgErr(404, "Note not found.")
	}

	org, err := app.FindRecordById(CollectionNameOrgs, note.GetString("org"))
	if err != nil {
		return nil, nil, nil, orgErr(404, "Note not found.")
	}

	return col, note, org, nil
}

// RequestBreakGlass records a request for access to a note.
func RequestBreakGlass(app App, ctx context.Context, auth *Record, in BreakGlassInput) (*Record, error) {
	if auth == nil || auth.IsSuperuser() {
		return nil, orgErr(403, "Only a holder of the breakglass tag can ask for access.")
	}

	if !BreakGlassAvailable(app) {
		return nil, orgErr(404, "Break-glass access is not available.")
	}

	_, note, org, err := loadPastoralNote(app, in.Collection, in.RecordId)
	if err != nil {
		return nil, err
	}

	// who is not even a member must not learn that the note exists
	if ActiveOrgMember(app, org, auth.Id) == nil {
		return nil, orgErr(404, "Note not found.")
	}

	if !IsBreakGlassHolder(app, org, auth.Id) {
		return nil, orgErr(403, "Only a holder of the breakglass tag can ask for access.")
	}

	reason := strings.TrimSpace(in.Reason)
	if n := utf8.RuneCountInString(reason); n < BreakGlassMinReason || n > BreakGlassMaxReason {
		return nil, orgErr(400, "A reason of at least 20 characters is required.")
	}

	if !slices.Contains(BreakGlassBases, in.Basis) {
		return nil, orgErr(400, "The basis must be safety, legal, owner_unavailable or other.")
	}

	if note.GetString("author") == auth.Id {
		return nil, orgErr(400, "The author can already read the note.")
	}

	limit := BreakGlassMaxFor(org)
	hours := in.Hours

	if hours == 0 {
		hours = int(limit / time.Hour)
	}

	if hours < 1 || time.Duration(hours)*time.Hour > limit {
		return nil, orgErr(400, "The access length is longer than this org allows.")
	}

	now := time.Now().UTC()

	if open := openBreakGlass(app, in.Collection, note.Id, auth.Id, now); open != nil {
		return nil, orgErr(409, "There is already a pending or active grant for this note.")
	}

	col, err := app.FindCachedCollectionByNameOrId(CollectionNameBreakGlass)
	if err != nil {
		return nil, err
	}

	var grant *Record

	err = app.RunInTransaction(func(txApp App) error {
		grant = NewRecord(col)
		grant.Set("collection", in.Collection)
		grant.Set("recordId", note.Id)
		grant.Set("org", org.Id)
		grant.Set("grantee", auth.Id)
		grant.Set("reason", reason)
		grant.Set("basis", in.Basis)
		grant.Set("hours", hours)
		grant.Set("requestedAt", types.NowDateTime())

		if err := txApp.Save(grant); err != nil {
			return wrapOrgSaveErr(err)
		}

		return writeBreakGlassAudit(txApp, ctx, AuditActionBreakGlassRequest, grant, map[string]any{
			"hours":        hours,
			"pendingUntil": now.Add(BreakGlassPendingHours * time.Hour).Format(time.RFC3339),
		})
	})
	if err != nil {
		return nil, err
	}

	return grant, nil
}

// openBreakGlass returns a pending or active grant of the grantee on the note.
func openBreakGlass(app App, collection, recordId, grantee string, now time.Time) *Record {
	rows, err := app.FindRecordsByFilter(CollectionNameBreakGlass,
		"collection={:c} && recordId={:r} && grantee={:g} && revoked=false", "", 0, 0,
		dbx.Params{"c": collection, "r": recordId, "g": grantee})
	if err != nil {
		return nil
	}

	for _, g := range rows {
		if s := BreakGlassState(g, now); s == BreakGlassPending || s == BreakGlassActive {
			return g
		}
	}

	return nil
}

// LiveBreakGlass returns the live grant of the grantee on the note, or nil.
func LiveBreakGlass(app App, collection, recordId, grantee string, now time.Time) *Record {
	rows, err := app.FindRecordsByFilter(CollectionNameBreakGlass,
		"collection={:c} && recordId={:r} && grantee={:g} && revoked=false", "", 0, 0,
		dbx.Params{"c": collection, "r": recordId, "g": grantee})
	if err != nil {
		return nil
	}

	for _, g := range rows {
		if BreakGlassState(g, now) == BreakGlassActive && g.GetString("approvedBy") != "" {
			return g
		}
	}

	return nil
}

func usableEmail(r *Record) bool {
	e := strings.TrimSpace(r.Email())

	return e != "" && !strings.HasSuffix(strings.ToLower(e), "@invalid")
}

// breakGlassRecipients works out who is told about an approved grant.
func breakGlassRecipients(app App, org, note, grantee, approver *Record) (recipients []*Record, authorReachable bool) {
	seen := map[string]bool{}

	add := func(r *Record) {
		if r == nil || seen[r.Id] || !usableEmail(r) {
			return
		}

		seen[r.Id] = true
		recipients = append(recipients, r)
	}

	userCol, err := OrgUserCollection(app)
	if err != nil {
		return nil, false
	}

	if author, err := app.FindRecordById(userCol, note.GetString("author")); err == nil && usableEmail(author) {
		add(author)

		authorReachable = true
	}

	members, _ := app.FindRecordsByFilter(CollectionNameOrgMembers, "org={:o} && status='active'", "created", 0, 0, dbx.Params{"o": org.Id})

	for _, m := range members {
		isOwner := m.GetString("role") == OrgRoleOwner
		holder := slices.Contains(OrgTagNames(app, m), PastoralTagBreakGlass)
		uid := m.GetString("user")

		switch {
		case isOwner:
		case holder && !authorReachable && uid != grantee.Id && uid != approver.Id:
		default:
			continue
		}

		if u, err := app.FindRecordById(userCol, uid); err == nil {
			add(u)
		}
	}

	return recipients, authorReachable
}

// ApproveBreakGlass is the second person's decision. It tells the author and
// the owners first and only then opens the grant.
func ApproveBreakGlass(app App, ctx context.Context, auth *Record, grantId string) (*Record, error) {
	if auth == nil || auth.IsSuperuser() {
		return nil, orgErr(403, "Only a holder of the breakglass tag can approve.")
	}

	grant, err := app.FindRecordById(CollectionNameBreakGlass, grantId)
	if err != nil {
		return nil, orgErr(404, "Grant not found.")
	}

	org, err := app.FindRecordById(CollectionNameOrgs, grant.GetString("org"))
	if err != nil || ActiveOrgMember(app, org, auth.Id) == nil {
		return nil, orgErr(404, "Grant not found.")
	}

	if !IsBreakGlassHolder(app, org, auth.Id) {
		return nil, orgErr(403, "Only a holder of the breakglass tag can approve.")
	}

	if grant.GetString("grantee") == auth.Id {
		return nil, orgErr(403, "You cannot approve your own request. A different holder of the breakglass tag must.")
	}

	now := time.Now().UTC()

	switch BreakGlassState(grant, now) {
	case BreakGlassPending:
	case BreakGlassLapsed:
		return nil, orgErr(409, "The request lapsed. Ask again.")
	case BreakGlassRevoked:
		return nil, orgErr(409, "The request was revoked.")
	default:
		return nil, orgErr(409, "The request was already approved.")
	}

	if !IsBreakGlassHolder(app, org, grant.GetString("grantee")) {
		return nil, orgErr(409, "The person who asked no longer holds the breakglass tag in this org.")
	}

	_, note, _, err := loadPastoralNote(app, grant.GetString("collection"), grant.GetString("recordId"))
	if err != nil {
		return nil, orgErr(409, "The note no longer exists.")
	}

	userCol, err := OrgUserCollection(app)
	if err != nil {
		return nil, err
	}

	grantee, err := app.FindRecordById(userCol, grant.GetString("grantee"))
	if err != nil {
		return nil, orgErr(409, "The person who asked no longer exists.")
	}

	hours := grant.GetInt("hours")
	limit := BreakGlassMaxFor(org)

	if hours < 1 || time.Duration(hours)*time.Hour > limit {
		hours = int(limit / time.Hour)
	}

	expires := now.Add(time.Duration(hours) * time.Hour)

	recipients, authorReachable := breakGlassRecipients(app, org, note, grantee, auth)

	notified := 0

	if BreakGlassNotifier != nil && len(recipients) > 0 {
		notified, _ = BreakGlassNotifier(app, &BreakGlassNotice{
			Grant: grant, Note: note, Org: org, Grantee: grantee, Approver: auth,
			Expires: expires, Recipients: recipients, AuthorReachable: authorReachable,
		})
	}

	if notified == 0 {
		return nil, orgErr(503, "No one could be told about this access, so it was not granted. The author or an owner of the org needs a working email address.")
	}

	err = app.RunInTransaction(func(txApp App) error {
		fresh, err := txApp.FindRecordById(CollectionNameBreakGlass, grant.Id)
		if err != nil {
			return orgErr(404, "Grant not found.")
		}

		if BreakGlassState(fresh, now) != BreakGlassPending {
			return orgErr(409, "The request was already decided.")
		}

		from, _ := types.ParseDateTime(now)
		until, _ := types.ParseDateTime(expires)

		fresh.Set("approvedBy", auth.Id)
		fresh.Set("activeFrom", from)
		fresh.Set("expires", until)
		fresh.Set("ownerNotifiedAt", types.NowDateTime())

		if err := txApp.Save(fresh); err != nil {
			return wrapOrgSaveErr(err)
		}

		grant = fresh

		return writeBreakGlassAudit(txApp, ctx, AuditActionBreakGlassOpen, fresh, map[string]any{
			"expires": expires.Format(time.RFC3339), "approvedBy": auth.Id, "notified": notified,
		})
	})
	if err != nil {
		return nil, err
	}

	return grant, nil
}

// RevokeBreakGlass closes a grant (or cancels a pending request). The owner of
// the grant's org, the note's author and the grantee may do it.
func RevokeBreakGlass(app App, ctx context.Context, auth *Record, grantId string) (*Record, error) {
	if auth == nil || auth.IsSuperuser() {
		return nil, orgErr(403, "You cannot revoke this grant.")
	}

	grant, err := app.FindRecordById(CollectionNameBreakGlass, grantId)
	if err != nil {
		return nil, orgErr(404, "Grant not found.")
	}

	org, _ := app.FindRecordById(CollectionNameOrgs, grant.GetString("org"))

	allowed := grant.GetString("grantee") == auth.Id || (org != nil && isOrgOwner(app, org, auth.Id))

	if !allowed {
		if col, err := app.FindCachedCollectionByNameOrId(grant.GetString("collection")); err == nil {
			if note, err := app.FindRecordById(col, grant.GetString("recordId")); err == nil && note.GetString("author") == auth.Id {
				allowed = true
			}
		}
	}

	if !allowed {
		return nil, orgErr(404, "Grant not found.")
	}

	if grant.GetBool("revoked") {
		return nil, orgErr(409, "The grant is already revoked.")
	}

	return revokeBreakGlassRow(app, ctx, grant, auth.Id, "manual")
}

func revokeBreakGlassRow(app App, ctx context.Context, grant *Record, by, cause string) (*Record, error) {
	out := grant

	err := app.RunInTransaction(func(txApp App) error {
		fresh, err := txApp.FindRecordById(CollectionNameBreakGlass, grant.Id)
		if err != nil {
			return orgErr(404, "Grant not found.")
		}

		if fresh.GetBool("revoked") {
			return nil
		}

		fresh.Set("revoked", true)
		fresh.Set("revokedBy", by)
		fresh.Set("revokedAt", types.NowDateTime())

		if err := txApp.Save(fresh); err != nil {
			return wrapOrgSaveErr(err)
		}

		out = fresh

		return writeBreakGlassAudit(txApp, ctx, AuditActionBreakGlassRevoke, fresh, map[string]any{
			"revokedBy": by, "cause": cause,
		})
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// RevokeBreakGlassForMember revokes every open grant (pending or active) of a
// person in an org. It runs when the person is suspended or removed.
func RevokeBreakGlassForMember(app App, ctx context.Context, orgId, userId, cause string) error {
	if !BreakGlassAvailable(app) {
		return nil
	}

	rows, err := app.FindRecordsByFilter(CollectionNameBreakGlass, "org={:o} && grantee={:g} && revoked=false", "", 0, 0,
		dbx.Params{"o": orgId, "g": userId})
	if err != nil {
		return nil
	}

	by := "system"

	for _, g := range rows {
		if _, err := revokeBreakGlassRow(app, ctx, g, by, cause); err != nil {
			return err
		}
	}

	return nil
}

// BreakGlassView is a grant as the routes show it.
type BreakGlassView struct {
	Record *Record
	State  string
	// WithReason is true for org owners and for the grantee.
	WithReason bool
}

// ListBreakGlass lists the grants of an org for an owner (reasons included) or
// a holder of the tag (reasons left out). orgId "" lists the caller's own grants.
func ListBreakGlass(app App, auth *Record, orgId string) ([]BreakGlassView, error) {
	if auth == nil || auth.IsSuperuser() {
		return nil, orgErr(403, "Sign in as a member of the org.")
	}

	var rows []*Record

	if orgId == "" {
		var err error

		rows, err = app.FindRecordsByFilter(CollectionNameBreakGlass, "grantee={:g}", "-created", 200, 0, dbx.Params{"g": auth.Id})
		if err != nil {
			return nil, err
		}
	} else {
		org, err := app.FindRecordById(CollectionNameOrgs, orgId)
		if err != nil || ActiveOrgMember(app, org, auth.Id) == nil {
			return nil, orgErr(404, "Org not found.")
		}

		owner := isOrgOwner(app, org, auth.Id)

		if !owner && !IsBreakGlassHolder(app, org, auth.Id) {
			return nil, orgErr(403, "Only owners and holders of the breakglass tag can list grants.")
		}

		rows, err = app.FindRecordsByFilter(CollectionNameBreakGlass, "org={:o}", "-created", 200, 0, dbx.Params{"o": org.Id})
		if err != nil {
			return nil, err
		}

		out := make([]BreakGlassView, 0, len(rows))
		now := time.Now().UTC()

		for _, g := range rows {
			out = append(out, BreakGlassView{Record: g, State: BreakGlassState(g, now), WithReason: owner || g.GetString("grantee") == auth.Id})
		}

		return out, nil
	}

	out := make([]BreakGlassView, 0, len(rows))
	now := time.Now().UTC()

	for _, g := range rows {
		out = append(out, BreakGlassView{Record: g, State: BreakGlassState(g, now), WithReason: true})
	}

	return out, nil
}

// ---- audit -----------------------------------------------------------------------

func writeBreakGlassAudit(app App, ctx context.Context, action string, grant *Record, extra map[string]any) error {
	if !auditAvailable(app) {
		return nil
	}

	meta := map[string]any{
		"orgId":      grant.GetString("org"),
		"noteId":     grant.GetString("recordId"),
		"collection": grant.GetString("collection"),
		"grantId":    grant.Id,
		"grantee":    grant.GetString("grantee"),
		"reason":     grant.GetString("reason"),
		"basis":      grant.GetString("basis"),
	}

	if d := grant.GetDateTime("expires"); !d.IsZero() {
		meta["expires"] = d.Time().UTC().Format(time.RFC3339)
	}

	for k, v := range extra {
		if v == nil {
			delete(meta, k)

			continue
		}

		meta[k] = v
	}

	entry := &AuditEntry{
		Action:         action,
		CollectionName: CollectionNameBreakGlass,
		RecordId:       grant.Id,
		Meta:           meta,
	}

	if c, err := app.FindCachedCollectionByNameOrId(CollectionNameBreakGlass); err == nil {
		entry.CollectionId = c.Id
	}

	applyAuditContext(ctx, entry)

	return WriteAuditEntry(app, ctx, entry, nil)
}
