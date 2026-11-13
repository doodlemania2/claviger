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

// Fork-local (Claviger #84): transfers of a household member without a login
// (a minor), decided by a guardian of the member's household, and the decline
// of the receiving org in both audit logs.
//
// Fixture: the households fixture (H1 in P1: erin and ivan guardians, kidA a
// minor without a login; H2 in P1: dave and kidB) plus H3 in P2, where erin is
// a guardian. ivan owns P2 and has no household there.

const (
	trKid     = "transferkidaaaa" // kidA: P1 -> P2, pending
	trH3      = "househ3aaaaaaaa"
	trH4      = "househ4aaaaaaaa"
	trHErin3  = "hmerin3aaaaaaaa"
	trHErin4  = "hmerin4aaaaaaaa"
	trKidBody = `{"member":"` + hhKidA + `","to":"` + orgP2 + `"}`
)

func trMemberFactory(t testing.TB) *tests.TestApp {
	app := hhFactory(t)

	hCol, err := app.FindCollectionByNameOrId(core.CollectionNameHouseholds)
	orgMust(t, err)

	mCol, err := app.FindCollectionByNameOrId(core.CollectionNameHouseholdMembers)
	orgMust(t, err)

	h := core.NewRecord(hCol)
	h.Id = trH3
	h.Set("org", orgP2)
	h.Set("name", "Erin at P2")
	h.Set("status", "active")
	orgMust(t, app.Save(h))

	m := core.NewRecord(mCol)
	m.Id = trHErin3
	m.Set("household", trH3)
	m.Set("user", orgUserID("erin"))
	m.Set("name", "Erin E")
	m.Set("role", "head")
	m.Set("guardian", true)
	m.Set("status", "active")
	orgMust(t, app.Save(m))

	return app
}

func withKidTransfer(t testing.TB, app *tests.TestApp) {
	t.Helper()

	col, err := app.FindCollectionByNameOrId(core.CollectionNameOrgTransfers)
	orgMust(t, err)

	r := core.NewRecord(col)
	r.Id = trKid
	r.Set("member", hhKidA)
	r.Set("fromOrg", orgP1)
	r.Set("toOrg", orgP2)
	r.Set("state", core.OrgTransferPending)
	r.Set("sourceOutcome", core.OrgTransferSuspend)
	r.Set("startedBy", orgUserID("alice"))
	r.Set("expiresAt", types.NowDateTime().Add(24*time.Hour))
	orgMust(t, app.Save(r))
}

// withSecondTargetHousehold gives erin a second household in P2.
func withSecondTargetHousehold(t testing.TB, app *tests.TestApp) {
	t.Helper()

	withKidTransfer(t, app)

	hCol, _ := app.FindCollectionByNameOrId(core.CollectionNameHouseholds)
	mCol, _ := app.FindCollectionByNameOrId(core.CollectionNameHouseholdMembers)

	h := core.NewRecord(hCol)
	h.Id = trH4
	h.Set("org", orgP2)
	h.Set("name", "Erin again at P2")
	h.Set("status", "active")
	orgMust(t, app.Save(h))

	m := core.NewRecord(mCol)
	m.Id = trHErin4
	m.Set("household", trH4)
	m.Set("user", orgUserID("erin"))
	m.Set("name", "Erin E")
	m.Set("role", "head")
	m.Set("guardian", true)
	m.Set("status", "active")
	orgMust(t, app.Save(m))
}

func kidCase(c orgCase) orgCase {
	c.factory = trMemberFactory

	return c
}

func kidUntouched(t testing.TB, app *tests.TestApp) {
	t.Helper()

	if s := transferState(t, app, trKid); s != core.OrgTransferPending {
		t.Fatalf("transfer state %s", s)
	}

	src, err := app.FindRecordById(core.CollectionNameHouseholdMembers, hhKidA)
	orgMust(t, err)

	if src.GetString("status") != "active" || src.GetString("household") != hhH1 {
		t.Fatalf("the source row changed: %s %s", src.GetString("status"), src.GetString("household"))
	}

	n, _ := app.CountRecords(core.CollectionNameHouseholdMembers, dbx.HashExp{"household": trH3, "name": "Kid A"})
	if n != 0 {
		t.Fatal("a row was created in the target household")
	}
}

func TestOrgTransferMemberStartRoute(t *testing.T) {
	hhKEK(t)

	url := "/api/orgs/" + orgP1 + "/transfers"

	started := func(by string) func(testing.TB, *tests.TestApp) {
		return func(t testing.TB, app *tests.TestApp) {
			rows, err := app.FindAllRecords(core.CollectionNameOrgTransfers)
			orgMust(t, err)

			if len(rows) != 1 || rows[0].GetString("member") != hhKidA || rows[0].GetString("person") != "" ||
				rows[0].GetString("startedBy") != orgUserID(by) {
				t.Fatalf("unexpected transfers: %v", rows)
			}

			if s, _ := app.FindRecordById(core.CollectionNameHouseholdMembers, hhKidA); s.GetString("status") != "active" {
				t.Fatal("starting must not change the member")
			}
		}
	}

	none := func(t testing.TB, app *tests.TestApp) {
		if n, _ := app.CountRecords(core.CollectionNameOrgTransfers); n != 0 {
			t.Fatalf("no transfer expected, got %d", n)
		}
	}

	runOrgCases(t, []orgCase{
		kidCase(orgCase{name: "guest", method: http.MethodPost, url: url, body: trKidBody, status: 401}),
		kidCase(orgCase{name: "a guardian starts for the child", who: "erin", method: http.MethodPost, url: url, body: trKidBody,
			status: 200, has: []string{`"member":"` + hhKidA + `"`, `"personName":"Kid A"`, `"state":"pending"`}, after: started("erin")}),
		kidCase(orgCase{name: "the owner starts for the child", who: "alice", method: http.MethodPost, url: url, body: trKidBody,
			status: 200, after: started("alice")}),
		kidCase(orgCase{name: "a registrar starts for the child", who: "carol", method: http.MethodPost, url: url, body: trKidBody,
			status: 200, after: started("carol")}),
		kidCase(orgCase{name: "a registrar starts a transfer of a person with a login", who: "carol", method: http.MethodPost, url: url,
			body: `{"user":"` + orgUserID("dave") + `","to":"` + orgP2 + `"}`, status: 200, has: []string{`"state":"pending"`}}),
		kidCase(orgCase{name: "a registrar cannot move an admin", who: "carol", method: http.MethodPost, url: url,
			body: `{"user":"` + orgUserID("bob") + `","to":"` + orgP2 + `"}`, status: 403, after: none}),
		kidCase(orgCase{name: "a registrar of a different org cannot", who: "gina", method: http.MethodPost, url: url, body: trKidBody,
			status: 404, before: registrarOfP2, after: none}),
		kidCase(orgCase{name: "a registrar tag on a viewer does not count", who: "dave", method: http.MethodPost, url: "/api/orgs/" + orgP1 + "/transfers",
			body: trKidBody, status: 403, before: tagViewerRegistrar, after: none}),
		kidCase(orgCase{name: "a guardian of another household cannot", who: "dave", method: http.MethodPost, url: url, body: trKidBody,
			status: 403, after: none}),
		kidCase(orgCase{name: "an adult who is not a guardian cannot", who: "frank", method: http.MethodPost, url: url, body: trKidBody,
			status: 403, after: none}),
		kidCase(orgCase{name: "a member of the org who is nobody here cannot", who: "gina", method: http.MethodPost, url: url, body: trKidBody,
			status: 404, after: none}),
		kidCase(orgCase{name: "a member with a login is a user transfer", who: "alice", method: http.MethodPost, url: url,
			body: `{"member":"` + hhErin + `","to":"` + orgP2 + `"}`, status: 400, after: none}),
		kidCase(orgCase{name: "a user and a member together", who: "alice", method: http.MethodPost, url: url,
			body: `{"member":"` + hhKidA + `","user":"` + orgUserID("bob") + `","to":"` + orgP2 + `"}`, status: 400, after: none}),
		kidCase(orgCase{name: "a member of another org's household", who: "alice", method: http.MethodPost, url: "/api/orgs/" + orgP2 + "/transfers",
			body: `{"member":"` + hhKidA + `","to":"` + orgP1 + `"}`, status: 404, after: none}),
		kidCase(orgCase{name: "unknown member", who: "alice", method: http.MethodPost, url: url,
			body: `{"member":"nosuchmember0000","to":"` + orgP2 + `"}`, status: 404, after: none}),
		kidCase(orgCase{name: "the same org", who: "alice", method: http.MethodPost, url: url,
			body: `{"member":"` + hhKidA + `","to":"` + orgP1 + `"}`, status: 400, after: none}),
		kidCase(orgCase{name: "archived target", who: "alice", method: http.MethodPost, url: url,
			body: `{"member":"` + hhKidA + `","to":"` + orgA + `"}`, status: 409, after: none}),
		kidCase(orgCase{name: "no second pending transfer", who: "erin", method: http.MethodPost, url: url, body: trKidBody,
			status: 409, before: withKidTransfer}),
	})
}

func TestOrgTransferMemberAcceptRoute(t *testing.T) {
	hhKEK(t)

	url := func(verb string) string { return "/api/orgs/" + orgP1 + "/transfers/" + trKid + "/" + verb }

	moved := func(t testing.TB, app *tests.TestApp) {
		if s := transferState(t, app, trKid); s != core.OrgTransferAccepted {
			t.Fatalf("transfer state %s", s)
		}

		src, _ := app.FindRecordById(core.CollectionNameHouseholdMembers, hhKidA)
		if src.GetString("status") != "left" {
			t.Fatalf("the source row should be left, got %s", src.GetString("status"))
		}

		rows, err := app.FindAllRecords(core.CollectionNameHouseholdMembers, dbx.HashExp{"household": trH3, "name": "Kid A"})
		orgMust(t, err)

		if len(rows) != 1 || !rows[0].GetBool("isMinor") || rows[0].GetString("user") != "" ||
			rows[0].GetString("birthDate") != hhYears(9) || rows[0].GetString("status") != "active" {
			t.Fatalf("unexpected row in the target household: %v", rows)
		}

		es, err := app.FindAllRecords(core.CollectionNameAuditLog, dbx.HashExp{"action": core.AuditActionOrgTransferAccept})
		orgMust(t, err)

		if len(es) != 1 {
			t.Fatalf("audit entries: %d", len(es))
		}

		if m := orgAuditMeta(t, es[0]); m["member"] != hhKidA || m["household"] != trH3 || m["toOrg"] != orgP2 {
			t.Fatalf("meta %v", m)
		}
	}

	runOrgCases(t, []orgCase{
		kidCase(orgCase{name: "guest", method: http.MethodPost, url: url("accept"), status: 401, before: withKidTransfer}),
		kidCase(orgCase{name: "a guardian accepts for the child", who: "erin", method: http.MethodPost, url: url("accept"), status: 200,
			before: withKidTransfer, has: []string{`"state":"accepted"`, `"membership":null`, `"name":"Kid A"`, `"household":"` + trH3 + `"`}, after: moved}),
		kidCase(orgCase{name: "from the target org url", who: "erin", method: http.MethodPost,
			url: "/api/orgs/" + orgP2 + "/transfers/" + trKid + "/accept", body: `{"household":"` + trH3 + `"}`, status: 200,
			before: withKidTransfer, after: moved}),
		kidCase(orgCase{name: "remove deletes the source row", who: "erin", method: http.MethodPost, url: url("accept"), status: 200,
			before: func(t testing.TB, app *tests.TestApp) {
				withKidTransfer(t, app)

				r, _ := app.FindRecordById(core.CollectionNameOrgTransfers, trKid)
				r.Set("sourceOutcome", core.OrgTransferRemove)
				orgMust(t, app.Save(r))
			},
			after: func(t testing.TB, app *tests.TestApp) {
				if _, err := app.FindRecordById(core.CollectionNameHouseholdMembers, hhKidA); err == nil {
					t.Fatal("the source row should be gone")
				}

				if n, _ := app.CountRecords(core.CollectionNameHouseholdMembers, dbx.HashExp{"household": trH3, "name": "Kid A"}); n != 1 {
					t.Fatalf("target rows: %d", n)
				}
			}}),
		kidCase(orgCase{name: "a guardian of another household cannot accept", who: "dave", method: http.MethodPost, url: url("accept"), status: 403,
			before: withKidTransfer, after: kidUntouched}),
		kidCase(orgCase{name: "an adult who is not a guardian cannot accept", who: "frank", method: http.MethodPost, url: url("accept"), status: 403,
			before: withKidTransfer, after: kidUntouched}),
		kidCase(orgCase{name: "the owner cannot accept for the child", who: "alice", method: http.MethodPost, url: url("accept"), status: 403,
			before: withKidTransfer, after: kidUntouched}),
		kidCase(orgCase{name: "registrar staff cannot accept", who: "carol", method: http.MethodPost, url: url("accept"), status: 403,
			before: withKidTransfer, after: kidUntouched}),
		kidCase(orgCase{name: "a superuser cannot accept", who: "super", method: http.MethodPost, url: url("accept"), status: 403,
			before: withKidTransfer, after: kidUntouched}),
		kidCase(orgCase{name: "a guardian with no household in the target org", who: "ivan", method: http.MethodPost, url: url("accept"), status: 409,
			before: withKidTransfer, after: kidUntouched}),
		kidCase(orgCase{name: "two households: one must be chosen", who: "erin", method: http.MethodPost, url: url("accept"), status: 400,
			before: withSecondTargetHousehold, after: kidUntouched}),
		kidCase(orgCase{name: "the chosen household", who: "erin", method: http.MethodPost, url: url("accept"), body: `{"household":"` + trH4 + `"}`, status: 200,
			before: withSecondTargetHousehold, has: []string{`"household":"` + trH4 + `"`}}),
		kidCase(orgCase{name: "a household the guardian does not guard", who: "erin", method: http.MethodPost, url: url("accept"),
			body: `{"household":"` + hhH2 + `"}`, status: 400, before: withKidTransfer, after: kidUntouched}),
		kidCase(orgCase{name: "the source household of someone else", who: "erin", method: http.MethodPost, url: url("accept"),
			body: `{"household":"nosuchhousehold0"}`, status: 400, before: withKidTransfer, after: kidUntouched}),
	})
}

func TestOrgTransferMemberDeclineCancelAndList(t *testing.T) {
	hhKEK(t)

	url := func(verb string) string { return "/api/orgs/" + orgP1 + "/transfers/" + trKid + "/" + verb }

	state := func(want string) func(testing.TB, *tests.TestApp) {
		return func(t testing.TB, app *tests.TestApp) {
			if s := transferState(t, app, trKid); s != want {
				t.Fatalf("state %s, want %s", s, want)
			}

			if src, _ := app.FindRecordById(core.CollectionNameHouseholdMembers, hhKidA); src.GetString("status") != "active" {
				t.Fatal("the member must not change")
			}
		}
	}

	runOrgCases(t, []orgCase{
		kidCase(orgCase{name: "a guardian declines", who: "ivan", method: http.MethodPost, url: url("decline"), status: 200,
			before: withKidTransfer, has: []string{`"state":"declined"`}, after: func(t testing.TB, app *tests.TestApp) {
				state(core.OrgTransferDeclined)(t, app)

				es, err := app.FindAllRecords(core.CollectionNameAuditLog, dbx.HashExp{"action": core.AuditActionOrgTransferDecline})
				orgMust(t, err)

				if len(es) != 1 || orgAuditMeta(t, es[0])["side"] != "person" || orgAuditMeta(t, es[0])["orgId"] != orgP1 {
					t.Fatalf("a decline by the person side is in the source log only: %d", len(es))
				}
			}}),
		kidCase(orgCase{name: "a guardian of another household cannot decline", who: "dave", method: http.MethodPost, url: url("decline"), status: 403,
			before: withKidTransfer, after: state(core.OrgTransferPending)}),
		kidCase(orgCase{name: "staff of the source cannot decline for the child", who: "alice", method: http.MethodPost, url: url("decline"), status: 403,
			before: withKidTransfer, after: state(core.OrgTransferPending)}),
		kidCase(orgCase{name: "the target admin declines: both orgs log it", who: "carol", method: http.MethodPost,
			url: "/api/orgs/" + orgP2 + "/transfers/" + trKid + "/decline", status: 200,
			before: withKidTransfer, has: []string{`"state":"declined"`}, after: func(t testing.TB, app *tests.TestApp) {
				state(core.OrgTransferDeclined)(t, app)

				es, err := app.FindAllRecords(core.CollectionNameAuditLog, dbx.HashExp{"action": core.AuditActionOrgTransferDecline})
				orgMust(t, err)

				if len(es) != 2 {
					t.Fatalf("audit entries: %d", len(es))
				}

				seen := map[string]map[string]any{}
				for _, e := range es {
					m := orgAuditMeta(t, e)
					seen[m["orgId"].(string)] = m
				}

				src, dst := seen[orgP1], seen[orgP2]
				if src == nil || dst == nil {
					t.Fatalf("expected one entry per org, got %v", seen)
				}

				for _, m := range []map[string]any{src, dst} {
					if m["side"] != "target" || m["decidedBy"] != orgUserID("carol") || m["fromOrg"] != orgP1 || m["toOrg"] != orgP2 {
						t.Fatalf("meta %v", m)
					}
				}

				if src["member"] != hhKidA {
					t.Fatalf("the source entry names the member: %v", src)
				}

				if _, ok := dst["member"]; ok {
					t.Fatalf("the target entry must not name the person: %v", dst)
				}
			}}),
		kidCase(orgCase{name: "an admin of the target cannot accept", who: "carol", method: http.MethodPost,
			url: "/api/orgs/" + orgP2 + "/transfers/" + trKid + "/accept", status: 403,
			before: withKidTransfer, after: state(core.OrgTransferPending)}),
		kidCase(orgCase{name: "a guardian cancels", who: "erin", method: http.MethodPost, url: url("cancel"), status: 200,
			before: withKidTransfer, after: state(core.OrgTransferCancelled)}),
		kidCase(orgCase{name: "a guardian of another household cannot cancel", who: "dave", method: http.MethodPost, url: url("cancel"), status: 403,
			before: withKidTransfer, after: state(core.OrgTransferPending)}),
		kidCase(orgCase{name: "the guardian sees it in mine", who: "erin", url: "/api/orgs/transfers", status: 200,
			before: withKidTransfer, has: []string{trKid, `"personName":"Kid A"`}}),
		kidCase(orgCase{name: "another guardian does not see it in mine", who: "dave", url: "/api/orgs/transfers", status: 200,
			before: withKidTransfer, hasNot: []string{trKid}}),
		kidCase(orgCase{name: "stays of a child are empty", who: "alice", url: url("stays"), status: 200,
			before: withKidTransfer, has: []string{`"stays":[]`}}),
	})
}

// tagViewerRegistrar gives dave (a viewer of P1, guardian of H2 only) the registrar tag.
func tagViewerRegistrar(t testing.TB, app *tests.TestApp) {
	t.Helper()

	m, err := core.FindOrgMember(app, orgP1, orgUserID("dave"))
	orgMust(t, err)

	tag, err := app.FindRecordById(core.CollectionNameOrgRoles, "tagregistrarp1a")
	orgMust(t, err)

	m.Set("tags", []string{tag.Id})
	orgMust(t, app.Save(m))
}

// registrarOfP2 makes gina (admin of the diocese, no role in P1) a P2 member
// tagged registrar there.
func registrarOfP2(t testing.TB, app *tests.TestApp) {
	t.Helper()

	roles, err := app.FindCollectionByNameOrId(core.CollectionNameOrgRoles)
	orgMust(t, err)

	tag := core.NewRecord(roles)
	tag.Id = "tagregistrarp2a"
	tag.Set("org", orgP2)
	tag.Set("name", core.HouseholdRegistrarTag)
	orgMust(t, app.Save(tag))

	members, err := app.FindCollectionByNameOrId(core.CollectionNameOrgMembers)
	orgMust(t, err)

	m := core.NewRecord(members)
	m.Set("org", orgP2)
	m.Set("user", orgUserID("gina"))
	m.Set("role", "member")
	m.Set("status", "active")
	m.Set("tags", []string{tag.Id})
	orgMust(t, app.Save(m))
}
