package apis_test

import (
	"net/http"
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #84, 84-4): the member transfer routes.

const (
	trBob  = "transferbobaaaa" // bob: P1 -> P2, pending
	trDave = "transferdaveaaa" // dave (viewer): P1 -> P2, pending
	trOld  = "transferoldaaaa" // bob: P1 -> P2, pending but expired
	trDone = "transferdoneaaa" // carol: P2 -> P1, declined
)

func seedTransfer(t testing.TB, app *tests.TestApp, id, person, from, to, outcome, state string, expires time.Duration) {
	t.Helper()

	col, err := app.FindCollectionByNameOrId(core.CollectionNameOrgTransfers)
	orgMust(t, err)

	r := core.NewRecord(col)
	r.Id = id
	r.Set("person", orgUserID(person))
	r.Set("fromOrg", from)
	r.Set("toOrg", to)
	r.Set("state", state)
	r.Set("sourceOutcome", outcome)
	r.Set("startedBy", orgUserID("alice"))
	r.Set("expiresAt", types.NowDateTime().Add(expires))
	orgMust(t, app.Save(r))
}

func withTransfers(t testing.TB, app *tests.TestApp) {
	t.Helper()

	seedTransfer(t, app, trBob, "bob", orgP1, orgP2, core.OrgTransferSuspend, core.OrgTransferPending, 24*time.Hour)
	seedTransfer(t, app, trDave, "dave", orgP1, orgP2, core.OrgTransferRemove, core.OrgTransferPending, 24*time.Hour)
}

func transferState(t testing.TB, app *tests.TestApp, id string) string {
	t.Helper()

	r, err := app.FindRecordById(core.CollectionNameOrgTransfers, id)
	orgMust(t, err)

	return r.GetString("state")
}

func memberStatus(app *tests.TestApp, org, who string) string {
	m, err := core.FindOrgMember(app, org, orgUserID(who))
	if err != nil {
		return "none"
	}

	return m.GetString("status")
}

func TestOrgTransferStartRoute(t *testing.T) {
	t.Parallel()

	created := func(who string) func(testing.TB, *tests.TestApp) {
		return func(t testing.TB, app *tests.TestApp) {
			n, _ := app.CountRecords(core.CollectionNameOrgTransfers)
			if n != 1 {
				t.Fatalf("expected one transfer, got %d", n)
			}

			m := memberStatus(app, orgP1, who)
			if m != "active" {
				t.Fatalf("starting must not change the membership, got %s", m)
			}
		}
	}

	start := func(user, to string) string { return `{"user":"` + user + `","to":"` + to + `"}` }

	runOrgCases(t, []orgCase{
		{name: "guest", method: http.MethodPost, url: "/api/orgs/" + orgP1 + "/transfers", body: start(orgUserID("bob"), orgP2), status: 401},
		{name: "an admin starts", who: "alice", method: http.MethodPost, url: "/api/orgs/" + orgP1 + "/transfers",
			body: start(orgUserID("bob"), orgP2), status: 200, after: created("bob"),
			has: []string{`"state":"pending"`, `"sourceOutcome":"suspend"`, `"toOrgName":"Parish Two"`}},
		{name: "by slug and email, with an outcome", who: "bob", method: http.MethodPost, url: "/api/orgs/parish-1/transfers",
			body: `{"user":"carol@orgs.test","to":"parish-2","outcome":"remove"}`, status: 409}, // carol is in P2 already
		{name: "the person starts their own", who: "bob", method: http.MethodPost, url: "/api/orgs/" + orgP1 + "/transfers",
			body: start(orgUserID("bob"), orgP2), status: 200, after: created("bob")},
		{name: "a viewer moves themself", who: "dave", method: http.MethodPost, url: "/api/orgs/" + orgP1 + "/transfers",
			body: start(orgUserID("dave"), orgP2), status: 200},
		{name: "a member cannot start for someone else", who: "carol", method: http.MethodPost, url: "/api/orgs/" + orgP1 + "/transfers",
			body: start(orgUserID("dave"), orgP2), status: 403},
		{name: "an admin cannot move the owner above them", who: "bob", method: http.MethodPost, url: "/api/orgs/" + orgP1 + "/transfers",
			body: start(orgUserID("alice"), orgP2), status: 403},
		{name: "not a member of the source org", who: "erin", method: http.MethodPost, url: "/api/orgs/" + orgP1 + "/transfers",
			body: start(orgUserID("bob"), orgP2), status: 404},
		{name: "the same org", who: "alice", method: http.MethodPost, url: "/api/orgs/" + orgP1 + "/transfers",
			body: start(orgUserID("bob"), orgP1), status: 400},
		{name: "unknown target", who: "alice", method: http.MethodPost, url: "/api/orgs/" + orgP1 + "/transfers",
			body: start(orgUserID("bob"), "nowhere"), status: 404},
		{name: "archived target", who: "alice", method: http.MethodPost, url: "/api/orgs/" + orgP1 + "/transfers",
			body: start(orgUserID("bob"), orgA), status: 409},
		{name: "the person is not in the source", who: "alice", method: http.MethodPost, url: "/api/orgs/" + orgP1 + "/transfers",
			body: start(orgUserID("erin"), orgP2), status: 404},
		{name: "already in the target", who: "alice", method: http.MethodPost, url: "/api/orgs/" + orgP1 + "/transfers",
			body: start(orgUserID("carol"), orgP2), status: 409},
		{name: "a suspended member", who: "alice", method: http.MethodPost, url: "/api/orgs/" + orgP1 + "/transfers",
			body: start(orgUserID("sam"), orgP2), status: 404},
		{name: "bad outcome", who: "alice", method: http.MethodPost, url: "/api/orgs/" + orgP1 + "/transfers",
			body: `{"user":"` + orgUserID("bob") + `","to":"` + orgP2 + `","outcome":"burn"}`, status: 400},
		{name: "no duplicate pending transfer", who: "alice", method: http.MethodPost, url: "/api/orgs/" + orgP1 + "/transfers",
			body: start(orgUserID("bob"), orgP2), status: 409, before: withTransfers},
		{name: "a superuser starts", who: "super", method: http.MethodPost, url: "/api/orgs/" + orgP1 + "/transfers",
			body: start(orgUserID("bob"), orgP2), status: 200},
	})
}

func TestOrgTransferAcceptRoute(t *testing.T) {
	t.Parallel()

	url := func(id, verb string) string { return "/api/orgs/" + orgP1 + "/transfers/" + id + "/" + verb }

	untouched := func(t testing.TB, app *tests.TestApp) {
		if s := memberStatus(app, orgP1, "bob"); s != "active" {
			t.Fatalf("source membership changed: %s", s)
		}

		if s := memberStatus(app, orgP2, "bob"); s != "none" {
			t.Fatalf("target membership created: %s", s)
		}

		if s := transferState(t, app, trBob); s != core.OrgTransferPending {
			t.Fatalf("transfer state %s", s)
		}
	}

	runOrgCases(t, []orgCase{
		{name: "guest", method: http.MethodPost, url: url(trBob, "accept"), status: 401, before: withTransfers},
		{name: "the owner cannot accept alone", who: "alice", method: http.MethodPost, url: url(trBob, "accept"), status: 403,
			before: withTransfers, after: untouched},
		{name: "an admin of the target cannot accept", who: "ivan", method: http.MethodPost, url: "/api/orgs/" + orgP2 + "/transfers/" + trBob + "/accept",
			status: 403, before: withTransfers, after: untouched},
		{name: "a superuser cannot accept", who: "super", method: http.MethodPost, url: url(trBob, "accept"), status: 403,
			before: withTransfers, after: untouched},
		{name: "someone else's transfer", who: "dave", method: http.MethodPost, url: url(trBob, "accept"), status: 403,
			before: withTransfers, after: untouched},
		{name: "a transfer of another org", who: "bob", method: http.MethodPost, url: "/api/orgs/" + orgA + "/transfers/" + trBob + "/accept",
			status: 404, before: withTransfers, after: untouched},
		{name: "the person accepts and is suspended in the source", who: "bob", method: http.MethodPost, url: url(trBob, "accept"), status: 200,
			before: withTransfers, has: []string{`"state":"accepted"`, `"membership"`, `"stays"`},
			after: func(t testing.TB, app *tests.TestApp) {
				if s := memberStatus(app, orgP1, "bob"); s != "suspended" {
					t.Fatalf("source: %s", s)
				}

				if s := memberStatus(app, orgP2, "bob"); s != "active" {
					t.Fatalf("target: %s", s)
				}

				if s := transferState(t, app, trBob); s != core.OrgTransferAccepted {
					t.Fatalf("state %s", s)
				}
			}},
		{name: "the person accepts from the target org url", who: "dave", method: http.MethodPost,
			url: "/api/orgs/" + orgP2 + "/transfers/" + trDave + "/accept", status: 200, before: withTransfers,
			after: func(t testing.TB, app *tests.TestApp) {
				if s := memberStatus(app, orgP1, "dave"); s != "none" {
					t.Fatalf("the source membership should be removed: %s", s)
				}

				if s := memberStatus(app, orgP2, "dave"); s != "active" {
					t.Fatalf("target: %s", s)
				}
			}},
		{name: "an expired transfer", who: "bob", method: http.MethodPost, url: url(trOld, "accept"), status: 409,
			before: func(t testing.TB, app *tests.TestApp) {
				seedTransfer(t, app, trOld, "bob", orgP1, orgP2, core.OrgTransferSuspend, core.OrgTransferPending, -time.Hour)
			},
			after: func(t testing.TB, app *tests.TestApp) {
				if s := transferState(t, app, trOld); s != core.OrgTransferExpired {
					t.Fatalf("state %s", s)
				}
			}},
		{name: "a closed transfer", who: "carol", method: http.MethodPost, url: "/api/orgs/" + orgP2 + "/transfers/" + trDone + "/accept", status: 409,
			before: func(t testing.TB, app *tests.TestApp) {
				seedTransfer(t, app, trDone, "carol", orgP2, orgP1, core.OrgTransferSuspend, core.OrgTransferDeclined, time.Hour)
			}},
	})
}

func TestOrgTransferDeclineAndCancelRoutes(t *testing.T) {
	t.Parallel()

	url := func(id, verb string) string { return "/api/orgs/" + orgP1 + "/transfers/" + id + "/" + verb }

	state := func(want string) func(testing.TB, *tests.TestApp) {
		return func(t testing.TB, app *tests.TestApp) {
			if s := transferState(t, app, trBob); s != want {
				t.Fatalf("state %s, want %s", s, want)
			}

			if s := memberStatus(app, orgP1, "bob"); s != "active" {
				t.Fatalf("the membership must not change: %s", s)
			}
		}
	}

	runOrgCases(t, []orgCase{
		{name: "the person declines", who: "bob", method: http.MethodPost, url: url(trBob, "decline"), status: 200,
			before: withTransfers, has: []string{`"state":"declined"`}, after: state(core.OrgTransferDeclined)},
		{name: "a target admin declines", who: "ivan", method: http.MethodPost, url: "/api/orgs/" + orgP2 + "/transfers/" + trBob + "/decline",
			status: 200, before: withTransfers, has: []string{`"state":"declined"`},
			after: func(t testing.TB, app *tests.TestApp) {
				state(core.OrgTransferDeclined)(t, app)

				r, _ := app.FindRecordById(core.CollectionNameOrgTransfers, trBob)
				if r.GetString("decidedBy") != orgUserID("ivan") {
					t.Fatalf("decidedBy %q", r.GetString("decidedBy"))
				}

				es, err := app.FindAllRecords(core.CollectionNameAuditLog, dbx.HashExp{"action": core.AuditActionOrgTransferDecline})
				orgMust(t, err)

				if len(es) != 1 {
					t.Fatalf("audit entries: %d", len(es))
				}

				m := orgAuditMeta(t, es[0])
				if m["side"] != "target" || m["decidedBy"] != orgUserID("ivan") || m["fromOrg"] != orgP1 || m["toOrg"] != orgP2 {
					t.Fatalf("meta %v", m)
				}
			}},
		{name: "a target admin cannot accept", who: "carol", method: http.MethodPost, url: "/api/orgs/" + orgP2 + "/transfers/" + trBob + "/accept",
			status: 403, before: withTransfers, after: state(core.OrgTransferPending)},
		{name: "a non-admin of the target cannot decline", who: "erin", method: http.MethodPost, url: "/api/orgs/" + orgP2 + "/transfers/" + trBob + "/decline",
			status: 403, before: withTransfers, after: state(core.OrgTransferPending)},
		{name: "a superuser cannot decline", who: "super", method: http.MethodPost, url: url(trBob, "decline"),
			status: 403, before: withTransfers, after: state(core.OrgTransferPending)},
		{name: "an admin cannot decline for the person", who: "alice", method: http.MethodPost, url: url(trBob, "decline"), status: 403,
			before: withTransfers, after: state(core.OrgTransferPending)},
		{name: "an admin of the source cancels", who: "alice", method: http.MethodPost, url: url(trBob, "cancel"), status: 200,
			before: withTransfers, has: []string{`"state":"cancelled"`}, after: state(core.OrgTransferCancelled)},
		{name: "the person cancels", who: "bob", method: http.MethodPost, url: url(trBob, "cancel"), status: 200,
			before: withTransfers, after: state(core.OrgTransferCancelled)},
		{name: "a member who is not involved cannot cancel", who: "carol", method: http.MethodPost, url: url(trBob, "cancel"), status: 403,
			before: withTransfers, after: state(core.OrgTransferPending)},
		{name: "an admin of the target cannot cancel", who: "ivan", method: http.MethodPost, url: "/api/orgs/" + orgP2 + "/transfers/" + trBob + "/cancel",
			status: 403, before: withTransfers, after: state(core.OrgTransferPending)},
		{name: "unknown transfer", who: "alice", method: http.MethodPost, url: url("nonexistent0000", "cancel"), status: 404, before: withTransfers},
	})
}

func TestOrgTransferListRoutes(t *testing.T) {
	t.Parallel()

	runOrgCases(t, []orgCase{
		{name: "guest", url: "/api/orgs/" + orgP1 + "/transfers", status: 401},
		{name: "staff of the source see all", who: "alice", url: "/api/orgs/" + orgP1 + "/transfers", status: 200, before: withTransfers,
			has: []string{trBob, trDave, `"person":"` + orgUserID("bob") + `"`, `"personName"`}},
		{name: "staff can filter", who: "alice", url: "/api/orgs/" + orgP1 + "/transfers?state=declined", status: 200, before: withTransfers,
			hasNot: []string{trBob}},
		{name: "bad state", who: "alice", url: "/api/orgs/" + orgP1 + "/transfers?state=what", status: 400},
		{name: "a member sees only their own", who: "dave", url: "/api/orgs/" + orgP1 + "/transfers", status: 200, before: withTransfers,
			has: []string{trDave}, hasNot: []string{trBob}},
		{name: "staff of the target see the incoming without the person", who: "ivan", url: "/api/orgs/" + orgP2 + "/transfers", status: 200,
			before: withTransfers, has: []string{trBob, `"fromOrgName":"Parish One"`}, hasNot: []string{`"person"`, orgUserID("bob")}},
		{name: "not a member", who: "frank", url: "/api/orgs/" + orgP1 + "/transfers", status: 404, before: withTransfers},
		{name: "mine", who: "bob", url: "/api/orgs/transfers", status: 200, before: withTransfers,
			has: []string{trBob}, hasNot: []string{trDave}},
		{name: "mine, nothing", who: "frank", url: "/api/orgs/transfers", status: 200, before: withTransfers, hasNot: []string{trBob}},
		{name: "an expired one is closed on read", who: "bob", url: "/api/orgs/transfers", status: 200,
			before: func(t testing.TB, app *tests.TestApp) {
				seedTransfer(t, app, trOld, "bob", orgP1, orgP2, core.OrgTransferSuspend, core.OrgTransferPending, -time.Hour)
			},
			has: []string{`"state":"expired"`}},
	})
}

func TestOrgTransferStaysRoute(t *testing.T) {
	t.Parallel()

	url := "/api/orgs/" + orgP1 + "/transfers/" + trBob + "/stays"

	runOrgCases(t, []orgCase{
		{name: "staff of the source", who: "alice", url: url, status: 200, before: withTransfers, has: []string{`"stays"`}},
		{name: "a plain member is not staff", who: "dave", url: url, status: 403, before: withTransfers},
		{name: "staff of the target", who: "ivan", url: "/api/orgs/" + orgP2 + "/transfers/" + trBob + "/stays", status: 403, before: withTransfers},
	})
}

func TestOrgTransferCollectionIsHidden(t *testing.T) {
	t.Parallel()

	runOrgCases(t, []orgCase{
		{name: "the records API shows it to nobody", who: "alice", url: "/api/collections/" + core.CollectionNameOrgTransfers + "/records",
			status: 403, before: withTransfers},
	})
}
