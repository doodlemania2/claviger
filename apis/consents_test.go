package apis_test

import (
	"context"
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
