package apis

import (
	"net/http"
	"slices"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/router"
)

// Fork-local (Claviger #84, 84-4): member transfers between orgs, with consent.
//
//	GET  /api/orgs/transfers                          my transfers, and those of people I am a guardian of (?state=)
//	POST /api/orgs/{id}/transfers                     {user | member, to, outcome?}: start a transfer out of {id}
//	GET  /api/orgs/{id}/transfers                     transfers out of or into {id}; staff see all, members their own (?state=)
//	POST /api/orgs/{id}/transfers/{tid}/accept        the person only (a guardian for a child); moves the membership; {household?}
//	POST /api/orgs/{id}/transfers/{tid}/decline       the person, or an owner/admin of the target org (veto)
//	POST /api/orgs/{id}/transfers/{tid}/cancel        the starter, staff of the source org, or the person
//	GET  /api/orgs/{id}/transfers/{tid}/stays         counts of the person's records that stay (staff of the source org)
//
// {id} is the source or the target org. See docs/orgs.md, "Transfers". API
// keys can never change a transfer (orgSession refuses them), and an owner,
// admin or superuser cannot accept for the person.
func bindOrgTransfersApi(sub *router.RouterGroup[*core.RequestEvent]) {
	sub.POST("/{id}/transfers", orgTransfersStart)
	sub.GET("/{id}/transfers", orgTransfersList)
	sub.POST("/{id}/transfers/{tid}/accept", orgTransfersAccept)
	sub.POST("/{id}/transfers/{tid}/decline", orgTransfersDecline)
	sub.POST("/{id}/transfers/{tid}/cancel", orgTransfersCancel)
	sub.GET("/{id}/transfers/{tid}/stays", orgTransfersStays)
}

type orgTransferBody struct {
	User    string `json:"user" form:"user"`
	Member  string `json:"member" form:"member"`
	To      string `json:"to" form:"to"`
	Outcome string `json:"outcome" form:"outcome"`
}

// orgTransferJSON is the view of a transfer. The person's id is shown to staff
// of the source org and to the person; the target org only learns that someone
// is on the way.
func orgTransferJSON(app core.App, t *core.Record, showPerson bool) map[string]any {
	out := map[string]any{
		"id":            t.Id,
		"fromOrg":       t.GetString("fromOrg"),
		"toOrg":         t.GetString("toOrg"),
		"state":         t.GetString("state"),
		"sourceOutcome": t.GetString("sourceOutcome"),
		"expiresAt":     t.GetDateTime("expiresAt"),
		"decidedAt":     t.GetDateTime("decidedAt"),
		"created":       t.GetDateTime("created"),
	}

	for _, k := range []string{"fromOrg", "toOrg"} {
		if o, err := app.FindRecordById(core.CollectionNameOrgs, t.GetString(k)); err == nil {
			out[k+"Name"] = o.GetString("name")
		}
	}

	if showPerson {
		out["person"] = t.GetString("person")
		out["startedBy"] = t.GetString("startedBy")

		if mid := t.GetString("member"); mid != "" {
			out["member"] = mid

			if m, err := app.FindRecordById(core.CollectionNameHouseholdMembers, mid); err == nil {
				out["personName"] = m.GetString("name")
			}
		} else if u, err := app.FindRecordById(core.OrgUserCollectionName(), t.GetString("person")); err == nil {
			out["personName"] = u.GetString("name")
		}
	}

	return out
}

func orgTransferGate(e *core.RequestEvent) error {
	if !core.OrgTransfersAvailable(e.App) {
		return e.NotFoundError("Transfers are not available.", nil)
	}

	return nil
}

func orgTransfersStart(e *core.RequestEvent) error {
	org, actor, err := orgSession(e)
	if err != nil {
		return err
	}

	if err := orgTransferGate(e); err != nil {
		return err
	}

	var body orgTransferBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("Failed to read the request data.", err)
	}

	t, err := core.StartOrgTransfer(e.App, auditContext(e), actor, org,
		core.OrgTransferInput{User: body.User, Member: body.Member, To: body.To, Outcome: body.Outcome})
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, orgTransferJSON(e.App, t, true))
}

// transferFromPath loads the transfer and checks that {id} is its source or
// target org.
func transferFromPath(e *core.RequestEvent, org *core.Record) (*core.Record, error) {
	t, err := e.App.FindRecordById(core.CollectionNameOrgTransfers, e.Request.PathValue("tid"))
	if err != nil || (t.GetString("fromOrg") != org.Id && t.GetString("toOrg") != org.Id) {
		return nil, e.NotFoundError("Transfer not found.", nil)
	}

	return t, nil
}

// transferSession loads the transfer under an org that is its source or its
// target. It does not require a membership of that org: the person is a member
// of the source only, and a guardian may be a member of neither. Who may act is
// decided in core.
func transferSession(e *core.RequestEvent) (*core.Record, *core.Record, error) {
	if err := orgReadOnlyForKeys(e); err != nil {
		return nil, nil, err
	}

	if err := orgTransferGate(e); err != nil {
		return nil, nil, err
	}

	org, err := core.FindOrg(e.App, e.Request.PathValue("id"))
	if err != nil {
		return nil, nil, e.NotFoundError("Organization not found.", nil)
	}

	t, err := transferFromPath(e, org)
	if err != nil {
		return nil, nil, err
	}

	return org, t, nil
}

func orgTransfersAccept(e *core.RequestEvent) error {
	_, t, err := transferSession(e)
	if err != nil {
		return err
	}

	var body struct {
		Household string `json:"household" form:"household"`
	}

	// the body is optional: only a transfer of a household member without a
	// login (a child) reads it, to pick the receiving household
	if e.Request.ContentLength != 0 {
		if err := e.BindBody(&body); err != nil {
			return e.BadRequestError("Failed to read the request data.", err)
		}
	}

	res, err := core.AcceptOrgTransferInto(e.App, auditContext(e), e.Auth, t, body.Household)
	if err != nil {
		return orgHTTP(e, err)
	}

	out := map[string]any{
		"transfer":   orgTransferJSON(e.App, res.Transfer, true),
		"membership": nil,
		"stays":      res.Stays,
	}

	if res.Membership != nil {
		out["membership"] = orgMemberJSON(e.App, res.Membership, false)
	}

	if res.Member != nil {
		out["member"] = householdMemberJSON(res.Member, false)
	}

	return e.JSON(http.StatusOK, out)
}

func orgTransfersDecline(e *core.RequestEvent) error {
	_, t, err := transferSession(e)
	if err != nil {
		return err
	}

	out, err := core.DeclineOrgTransfer(e.App, auditContext(e), e.Auth, t)
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, orgTransferJSON(e.App, out, true))
}

func orgTransfersCancel(e *core.RequestEvent) error {
	_, t, err := transferSession(e)
	if err != nil {
		return err
	}

	out, err := core.CancelOrgTransfer(e.App, auditContext(e), e.Auth, t)
	if err != nil {
		return orgHTTP(e, err)
	}

	return e.JSON(http.StatusOK, orgTransferJSON(e.App, out, true))
}

func orgTransfersStays(e *core.RequestEvent) error {
	org, t, err := transferSession(e)
	if err != nil {
		return err
	}

	if t.GetString("fromOrg") != org.Id || !core.NewOrgActor(e.App, org, e.Auth).CanManage() {
		return e.ForbiddenError("Only staff of the source organization can see what stays.", nil)
	}

	stays, err := core.OrgTransferStays(e.App, org.Id, t.GetString("person"))
	if err != nil {
		return e.InternalServerError("", err)
	}

	return e.JSON(http.StatusOK, map[string]any{"stays": stays})
}

// transferMineExp matches the transfers of the caller, of the minors with a
// login they guard, and of the household members without a login they guard.
func transferMineExp(e *core.RequestEvent) dbx.Expression {
	mine := append([]string{e.Auth.Id}, core.GuardedUserIds(e.App, e.Auth.Id)...)
	ids := make([]any, len(mine))

	for i, m := range mine {
		ids[i] = m
	}

	exp := dbx.In("person", ids...)

	guarded := core.GuardedMemberIds(e.App, e.Auth.Id)
	if len(guarded) == 0 {
		return exp
	}

	members := make([]any, len(guarded))

	for i, m := range guarded {
		members[i] = m
	}

	return dbx.Or(exp, dbx.In("member", members...))
}

func transferStateFilter(e *core.RequestEvent) (dbx.Expression, error) {
	state := e.Request.URL.Query().Get("state")
	if state == "" {
		return nil, nil
	}

	if !slices.Contains(core.OrgTransferStates, state) {
		return nil, e.BadRequestError("Invalid state.", nil)
	}

	return dbx.HashExp{"state": state}, nil
}

func orgTransfersList(e *core.RequestEvent) error {
	org, actor, err := orgSession(e)
	if err != nil {
		return err
	}

	if err := orgTransferGate(e); err != nil {
		return err
	}

	_ = core.ExpireOrgTransfers(e.App, auditContext(e), timeNow())

	exps := []dbx.Expression{dbx.Or(dbx.HashExp{"fromOrg": org.Id}, dbx.HashExp{"toOrg": org.Id})}

	if st, err := transferStateFilter(e); err != nil {
		return err
	} else if st != nil {
		exps = append(exps, st)
	}

	staff := actor.CanManage()

	if !staff {
		exps = append(exps, transferMineExp(e))
	}

	rows, err := e.App.FindAllRecords(core.CollectionNameOrgTransfers, exps...)
	if err != nil {
		return e.InternalServerError("", err)
	}

	sortTransfers(rows)

	items := make([]map[string]any, 0, len(rows))

	for _, t := range rows {
		items = append(items, orgTransferJSON(e.App, t, !staff || t.GetString("fromOrg") == org.Id))
	}

	return e.JSON(http.StatusOK, map[string]any{"items": items})
}

func orgTransfersMine(e *core.RequestEvent) error {
	if err := orgReadOnlyForKeys(e); err != nil {
		return err
	}

	if err := orgTransferGate(e); err != nil {
		return err
	}

	_ = core.ExpireOrgTransfers(e.App, auditContext(e), timeNow())

	exps := []dbx.Expression{transferMineExp(e)}

	if st, err := transferStateFilter(e); err != nil {
		return err
	} else if st != nil {
		exps = append(exps, st)
	}

	rows, err := e.App.FindAllRecords(core.CollectionNameOrgTransfers, exps...)
	if err != nil {
		return e.InternalServerError("", err)
	}

	sortTransfers(rows)

	items := make([]map[string]any, 0, len(rows))

	for _, t := range rows {
		items = append(items, orgTransferJSON(e.App, t, true))
	}

	return e.JSON(http.StatusOK, map[string]any{"items": items})
}

func sortTransfers(rows []*core.Record) {
	slices.SortFunc(rows, func(a, b *core.Record) int {
		return b.GetDateTime("created").Time().Compare(a.GetDateTime("created").Time())
	})
}

func timeNow() time.Time { return time.Now() }
