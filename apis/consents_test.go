package apis_test

import (
	"context"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// Fork-local (Claviger #88, 88-2): the consent routes. The world is the
// household fixture of households_test.go: erin and ivan are guardians of
// kidA, frank is an adult with a login, dave is the guardian of kidB, carol
// (registrar), bob (admin) and alice (owner) are staff of P1.

func consentFactory(t testing.TB) *tests.TestApp {
	app := hhFactory(t)
	ctx := context.Background()

	for _, in := range []map[string]any{
		{"key": "newsletter", "name": "Newsletter", "text": "We email you the newsletter."},
		{"key": "photos", "name": "Photos", "text": "Photos of you.", "requiresGuardian": true},
		{"key": "promo", "name": "Promotions", "text": "Offers.", "marketing": true},
	} {
		if _, err := core.CreateConsentPurpose(app, ctx, in); err != nil {
			t.Fatal(err)
		}
	}

	return app
}

func consentCase(name, who, method, url, body string, status int, has ...string) orgCase {
	return orgCase{name: name, who: who, method: method, url: url, body: body, status: status, has: has, factory: consentFactory}
}

func TestConsentRoutesSelfService(t *testing.T) {
	hhKEK(t)

	runOrgCases(t, []orgCase{
		consentCase("guests", "", "GET", "/api/me/consents", "", 401),
		consentCase("my consents: me and the minor of my household", "erin", "GET", "/api/me/consents", "", 200,
			`"id":"`+hhKidA+`"`, `"minor":true`, `"key":"newsletter"`, `"status":"none"`, `"reason":"marketing_minor"`),
		{name: "my consents: not the other household", who: "erin", url: "/api/me/consents", status: 200,
			hasNot: []string{hhKidB, "Kid B"}, factory: consentFactory},
		consentCase("grant for myself", "frank", "POST", "/api/me/consents/newsletter/grant", "", 200,
			`"changed":true`, `"status":"granted"`, `"givenBy":"`+orgUserID("frank")+`"`, `"method":"web"`),
		consentCase("grant for a minor as a guardian", "erin", "POST", "/api/me/consents/photos/grant", `{"member":"`+hhKidA+`"}`, 200,
			`"subject":"`+hhKidA+`"`, `"givenBy":"`+hhErin+`"`, `"subjectType":"member"`),
		consentCase("a guardian of another household", "dave", "POST", "/api/me/consents/photos/grant", `{"member":"`+hhKidA+`"}`, 404),
		consentCase("an adult who is not a guardian", "frank", "POST", "/api/me/consents/photos/grant", `{"member":"`+hhKidA+`"}`, 404),
		consentCase("marketing for a minor", "erin", "POST", "/api/me/consents/promo/grant", `{"member":"`+hhKidA+`"}`, 403),
		consentCase("marketing for an adult is fine", "frank", "POST", "/api/me/consents/promo/grant", "", 200, `"status":"granted"`),
		consentCase("unknown purpose", "frank", "POST", "/api/me/consents/nope/grant", "", 404),
		consentCase("withdraw with nothing given", "frank", "POST", "/api/me/consents/newsletter/withdraw", "", 409),
		consentCase("api key style: superuser has no consents", "super", "POST", "/api/me/consents/newsletter/grant", "", 403),
		{
			name: "withdraw after a grant", who: "erin", method: "POST", url: "/api/me/consents/photos/withdraw",
			body: `{"member":"` + hhKidA + `"}`, status: 200, has: []string{`"status":"withdrawn"`}, factory: consentFactory,
			before: func(t testing.TB, app *tests.TestApp) {
				orgMust(t, grantFor(app, "erin", "photos", hhKidA))
			},
			after: func(t testing.TB, app *tests.TestApp) {
				events, err := app.FindRecordsByFilter(core.CollectionNameConsentEvents, "subject={:s}", "created", 0, 0, map[string]any{"s": hhKidA})
				orgMust(t, err)

				if len(events) != 2 || events[0].GetString("action") != "grant" || events[1].GetString("action") != "withdraw" {
					t.Fatalf("events: %d", len(events))
				}

				// each write left an audit entry that names the org
				entries, err := app.FindRecordsByFilter(core.CollectionNameAuditLog,
					"targetCollection={:c}", "", 0, 0, map[string]any{"c": core.CollectionNameConsentEvents})
				orgMust(t, err)

				if len(entries) != 2 {
					t.Fatalf("audit entries for events: %d", len(entries))
				}

				for _, e := range entries {
					var meta map[string]any
					_ = e.UnmarshalJSONField("meta", &meta)

					if meta["orgId"] != orgP1 {
						t.Errorf("audit meta: %v", meta)
					}
				}
			},
		},
	})
}

func grantFor(app *tests.TestApp, who, purpose, member string) error {
	u, err := app.FindRecordById("users", orgUserID(who))
	if err != nil {
		return err
	}

	_, err = core.ConsentSelf(app, context.Background(), u, purpose, member, core.ConsentActionGrant)

	return err
}

func TestConsentRoutesPurposes(t *testing.T) {
	hhKEK(t)

	runOrgCases(t, []orgCase{
		consentCase("list purposes for an org member", "erin", "GET", "/api/consent-purposes?org="+orgP1, "", 200,
			`"key":"newsletter"`, `"version":1`),
		consentCase("list for a stranger", "dave", "GET", "/api/consent-purposes?org="+orgP2, "", 404),
		consentCase("guests", "", "GET", "/api/consent-purposes", "", 401),
		consentCase("create needs a superuser", "alice", "POST", "/api/consent-purposes",
			`{"key":"sms","name":"SMS","text":"Texts."}`, 403),
		consentCase("create", "super", "POST", "/api/consent-purposes",
			`{"key":"sms","name":"SMS","text":"Texts.","lawfulBasis":"consent","minAge":16,"org":"`+orgP1+`"}`, 200,
			`"key":"sms"`, `"version":1`, `"minAge":16`, `"org":"`+orgP1+`"`),
		consentCase("create: duplicate key", "super", "POST", "/api/consent-purposes",
			`{"key":"newsletter","name":"x","text":"x"}`, 400),
		consentCase("update the text raises the version", "super", "PATCH", "/api/consent-purposes/newsletter",
			`{"text":"Changed."}`, 200, `"version":2`, `"text":"Changed."`),
		consentCase("update the name keeps the version", "super", "PATCH", "/api/consent-purposes/newsletter",
			`{"name":"News"}`, 200, `"version":1`),
		consentCase("update needs a superuser", "alice", "PATCH", "/api/consent-purposes/newsletter", `{"name":"News"}`, 403),
		consentCase("delete an unused purpose", "super", "DELETE", "/api/consent-purposes/newsletter", "", 204),
		{
			name: "delete a used purpose", who: "super", method: "DELETE", url: "/api/consent-purposes/newsletter",
			status: 409, factory: consentFactory,
			before: func(t testing.TB, app *tests.TestApp) { orgMust(t, grantFor(app, "frank", "newsletter", "")) },
		},
		{
			name: "outdated after a text change", who: "frank", url: "/api/me/consents", status: 200, factory: consentFactory,
			has: []string{`"outdated":true`, `"version":2`},
			before: func(t testing.TB, app *tests.TestApp) {
				orgMust(t, grantFor(app, "frank", "newsletter", ""))

				p, err := core.FindConsentPurpose(app, "newsletter")
				orgMust(t, err)

				_, err = core.UpdateConsentPurpose(app, context.Background(), p, map[string]any{"text": "New terms."})
				orgMust(t, err)
			},
		},
	})
}

func TestConsentRoutesStaff(t *testing.T) {
	hhKEK(t)

	base := "/api/orgs/" + orgP1 + "/consents"
	paper := func(extra string) string {
		return `{"purpose":"photos","member":"` + hhKidA + `","action":"grant","method":"paper","evidenceRef":"form 12","givenBy":"` + hhErin + `"` + extra + `}`
	}

	runOrgCases(t, []orgCase{
		consentCase("the registrar records a paper consent", "carol", "POST", base, paper(""), 200,
			`"changed":true`, `"method":"paper"`, `"evidenceRef":"form 12"`, `"givenBy":"`+hhErin+`"`),
		consentCase("an admin can too", "bob", "POST", base, paper(""), 200),
		consentCase("a member who is not staff", "erin", "POST", base, paper(""), 403),
		consentCase("a stranger", "alice", "POST", "/api/orgs/"+orgP2+"/consents", paper(""), 404),
		consentCase("evidence is required", "bob", "POST", base,
			`{"purpose":"photos","member":"`+hhKidA+`","action":"grant","method":"paper","givenBy":"`+hhErin+`"}`, 400),
		consentCase("a minor needs the guardian who gave it", "bob", "POST", base,
			`{"purpose":"photos","member":"`+hhKidA+`","action":"grant","method":"paper","evidenceRef":"x"}`, 400),
		consentCase("the guardian must be of the household", "bob", "POST", base,
			`{"purpose":"photos","member":"`+hhKidA+`","action":"grant","method":"paper","evidenceRef":"x","givenBy":"`+hhDave+`"}`, 400),
		consentCase("marketing for a minor", "bob", "POST", base,
			`{"purpose":"promo","member":"`+hhKidA+`","action":"grant","method":"paper","evidenceRef":"x","givenBy":"`+hhErin+`"}`, 403),
		consentCase("a person of another org's household", "bob", "POST", base,
			`{"purpose":"newsletter","member":"nonexistent0000","action":"grant","evidenceRef":"x"}`, 404),
		consentCase("an adult by user", "bob", "POST", base,
			`{"purpose":"newsletter","user":"`+orgUserID("frank")+`","action":"grant","method":"staff","evidenceRef":"told us on the phone"}`, 200,
			`"subjectType":"user"`, `"method":"staff"`),
		{
			name: "history of a person", who: "bob", url: base + "?member=" + hhKidA, status: 200, factory: consentFactory,
			has:    []string{`"events":[{`, `"action":"grant"`, `"text":"Photos of you."`, `"actor":"` + orgUserID("erin") + `"`, `"outdated":false`},
			before: func(t testing.TB, app *tests.TestApp) { orgMust(t, grantFor(app, "erin", "photos", hhKidA)) },
		},
		consentCase("history needs staff", "erin", "GET", base+"?member="+hhKidA, "", 403),
		consentCase("history needs a person", "bob", "GET", base, "", 400),
		{
			name: "admin history by subject", who: "super", url: "/api/consents/history?subject=" + hhKidA, status: 200, factory: consentFactory,
			has:    []string{`"consents":[{`, `"events":[{`},
			before: func(t testing.TB, app *tests.TestApp) { orgMust(t, grantFor(app, "erin", "photos", hhKidA)) },
		},
		consentCase("admin history is superuser only", "bob", "GET", "/api/consents/history?subject="+hhKidA, "", 403),
	})
}

// Org-scoped purposes: owners, admins and registrars manage the purposes of
// their own org. carol is a registrar of P1 (staff, but not an admin) and the
// admin of P2.
// tagRegistrar gives the user's membership of the org the registrar tag of
// that org (a tag row is created when the org has none).
func tagRegistrar(org, who string) func(t testing.TB, app *tests.TestApp) {
	return func(t testing.TB, app *tests.TestApp) {
		tag, err := app.FindFirstRecordByFilter(core.CollectionNameOrgRoles, "org={:o} && name={:n}",
			map[string]any{"o": org, "n": core.RegistrarTag})
		if err != nil {
			roles, err := app.FindCollectionByNameOrId(core.CollectionNameOrgRoles)
			orgMust(t, err)

			tag = core.NewRecord(roles)
			tag.Set("org", org)
			tag.Set("name", core.RegistrarTag)
			orgMust(t, app.Save(tag))
		}

		m, err := core.FindOrgMember(app, org, orgUserID(who))
		orgMust(t, err)
		m.Set("tags", append(m.GetStringSlice("tags"), tag.Id))
		orgMust(t, app.Save(m))
	}
}

func TestConsentRoutesOrgPurposes(t *testing.T) {
	hhKEK(t)

	base := "/api/orgs/" + orgP1 + "/consent-purposes"
	seed := func(org string) func(t testing.TB, app *tests.TestApp) {
		return func(t testing.TB, app *tests.TestApp) {
			_, err := core.CreateConsentPurpose(app, context.Background(),
				map[string]any{"key": "own-" + org, "name": "Own", "text": "Own text.", "org": org})
			orgMust(t, err)
		}
	}
	withBefore := func(c orgCase, f func(t testing.TB, app *tests.TestApp)) orgCase {
		c.before = f
		return c
	}
	newBody := `{"key":"trips","name":"Trips","text":"Trip photos."}`

	runOrgCases(t, []orgCase{
		consentCase("an admin creates one, pinned to the org", "bob", "POST", base, newBody, 200,
			`"key":"trips"`, `"org":"`+orgP1+`"`, `"version":1`),
		consentCase("an owner creates one", "alice", "POST", base, newBody, 200),
		consentCase("a plain member is refused", "erin", "POST", base, newBody, 403),
		consentCase("a registrar creates one, pinned to the org", "carol", "POST", base, newBody, 200,
			`"key":"trips"`, `"org":"`+orgP1+`"`),
		consentCase("a viewer is refused", "frank", "POST", base, newBody, 403),
		withBefore(consentCase("a registrar of another org is refused", "erin", "POST", base, newBody, 403), tagRegistrar(orgP2, "erin")),
		withBefore(consentCase("a registrar of another org cannot list", "erin", "GET", base, "", 403), tagRegistrar(orgP2, "erin")),
		withBefore(consentCase("a registrar of another org cannot update", "erin", "PATCH", base+"/own-"+orgP1, `{"name":"x"}`, 403),
			func(t testing.TB, app *tests.TestApp) {
				seed(orgP1)(t, app)
				tagRegistrar(orgP2, "erin")(t, app)
			}),
		withBefore(consentCase("a registrar of another org cannot delete", "erin", "DELETE", base+"/own-"+orgP1, "", 403),
			func(t testing.TB, app *tests.TestApp) {
				seed(orgP1)(t, app)
				tagRegistrar(orgP2, "erin")(t, app)
			}),
		withBefore(consentCase("a registrar still works in their own org", "erin", "POST", "/api/orgs/"+orgP2+"/consent-purposes", newBody, 200,
			`"org":"`+orgP2+`"`), tagRegistrar(orgP2, "erin")),
		withBefore(consentCase("a registrar cannot reach another org's purposes", "carol", "PATCH",
			base+"/own-"+orgP2, `{"name":"x"}`, 404), seed(orgP2)),
		consentCase("a registrar cannot make a global purpose", "carol", "POST", base,
			`{"key":"trips","name":"Trips","text":"x","org":""}`, 200, `"org":"`+orgP1+`"`),
		consentCase("a registrar cannot name another org", "carol", "POST", base,
			`{"key":"trips","name":"Trips","text":"x","org":"`+orgP2+`"}`, 403),
		consentCase("a registrar lists the org's and the global purposes", "carol", "GET", base, "", 200,
			`"key":"newsletter"`),
		withBefore(consentCase("a registrar updates", "carol", "PATCH", base+"/own-"+orgP1, `{"text":"Changed."}`, 200, `"version":2`),
			seed(orgP1)),
		withBefore(consentCase("a registrar retires", "carol", "PATCH", base+"/own-"+orgP1, `{"status":"archived"}`, 200, `"status":"archived"`),
			seed(orgP1)),
		withBefore(consentCase("a registrar deletes an unused purpose", "carol", "DELETE", base+"/own-"+orgP1, "", 204), seed(orgP1)),
		consentCase("a registrar cannot change a global purpose", "carol", "PATCH", base+"/newsletter", `{"name":"x"}`, 403),
		consentCase("a stranger gets a 404", "dave", "POST", "/api/orgs/"+orgP2+"/consent-purposes", newBody, 404),
		consentCase("an admin of another org gets a 404", "bob", "POST", "/api/orgs/"+orgP2+"/consent-purposes", newBody, 404),
		consentCase("a global purpose is not possible: other org in the body", "bob", "POST", base,
			`{"key":"trips","name":"Trips","text":"x","org":"`+orgP2+`"}`, 403),
		consentCase("an empty org in the body still means this org", "bob", "POST", base,
			`{"key":"trips","name":"Trips","text":"x","org":""}`, 200, `"org":"`+orgP1+`"`),
		consentCase("a clashing key (a global purpose) is a clear error", "bob", "POST", base,
			`{"key":"newsletter","name":"x","text":"x"}`, 400, "already exists", "unique across all organizations"),
		consentCase("guests", "", "POST", base, newBody, 401),
		consentCase("list: the org's purposes and the global ones", "bob", "GET", base, "", 200,
			`"key":"newsletter"`, `"key":"photos"`),
		consentCase("list: a member is refused", "erin", "GET", base, "", 403),
		withBefore(consentCase("list: not another org's purposes", "bob", "GET", base, "", 200, `"key":"own-`+orgP1+`"`),
			seed(orgP1)),
		{
			name: "list hides another org's purposes", who: "bob", url: base, status: 200, factory: consentFactory,
			hasNot: []string{"own-" + orgP2}, before: seed(orgP2),
		},
		withBefore(consentCase("update raises the version on a text change", "bob", "PATCH", base+"/own-"+orgP1,
			`{"text":"Changed."}`, 200, `"version":2`, `"org":"`+orgP1+`"`), seed(orgP1)),
		withBefore(consentCase("update the name keeps the version", "bob", "PATCH", base+"/own-"+orgP1,
			`{"name":"Renamed"}`, 200, `"version":1`), seed(orgP1)),
		withBefore(consentCase("archive", "bob", "PATCH", base+"/own-"+orgP1, `{"status":"archived"}`, 200, `"status":"archived"`),
			seed(orgP1)),
		withBefore(consentCase("cannot move to another org", "bob", "PATCH", base+"/own-"+orgP1,
			`{"org":"`+orgP2+`"}`, 403), seed(orgP1)),
		withBefore(consentCase("cannot make it global", "bob", "PATCH", base+"/own-"+orgP1, `{"org":""}`, 200, `"org":"`+orgP1+`"`),
			seed(orgP1)),
		withBefore(consentCase("a member cannot update", "erin", "PATCH", base+"/own-"+orgP1, `{"name":"x"}`, 403), seed(orgP1)),
		consentCase("a global purpose is refused (update)", "bob", "PATCH", base+"/newsletter", `{"name":"x"}`, 403),
		consentCase("a global purpose is refused (delete)", "bob", "DELETE", base+"/newsletter", "", 403),
		withBefore(consentCase("another org's purpose is not found (update)", "bob", "PATCH", base+"/own-"+orgP2, `{"name":"x"}`, 404),
			seed(orgP2)),
		withBefore(consentCase("another org's purpose is not found (delete)", "bob", "DELETE", base+"/own-"+orgP2, "", 404),
			seed(orgP2)),
		withBefore(consentCase("the org's admin of P2 cannot reach P1's purpose", "carol", "PATCH",
			"/api/orgs/"+orgP2+"/consent-purposes/own-"+orgP1, `{"name":"x"}`, 404), seed(orgP1)),
		withBefore(consentCase("delete an unused purpose", "bob", "DELETE", base+"/own-"+orgP1, "", 204), seed(orgP1)),
		{
			name: "delete a purpose with consents is a 409", who: "bob", method: "DELETE", url: base + "/own-" + orgP1,
			status: 409, factory: consentFactory,
			before: func(t testing.TB, app *tests.TestApp) {
				seed(orgP1)(t, app)
				orgMust(t, grantFor(app, "erin", "own-"+orgP1, hhKidA))
			},
		},
		{
			name: "a write is audited with the org", who: "bob", method: "POST", url: base, body: newBody, status: 200, factory: consentFactory,
			after: func(t testing.TB, app *tests.TestApp) {
				entries, err := app.FindRecordsByFilter(core.CollectionNameAuditLog,
					"targetCollection={:c}", "", 0, 0, map[string]any{"c": core.CollectionNameConsentPurposes})
				orgMust(t, err)

				found := false

				for _, e := range entries {
					if e.GetString("meta") != "" && strings.Contains(e.GetString("meta"), orgP1) {
						found = true
					}
				}

				if !found {
					t.Fatalf("no purpose audit entry names the org (%d entries)", len(entries))
				}
			},
		},
	})
}
