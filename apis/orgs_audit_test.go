package apis_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// Fork-local (Claviger #79a): audit entries, webhooks and API keys around orgs.

func orgAuditMeta(t testing.TB, e *core.Record) map[string]any {
	t.Helper()

	var meta map[string]any
	if err := e.UnmarshalJSONField("meta", &meta); err != nil {
		t.Fatal(err)
	}

	return meta
}

func TestOrgAuditEntries(t *testing.T) {
	t.Parallel()

	runOrgCases(t, []orgCase{
		{
			name: "adding a member is audited with the actor and the org", who: "bob", method: http.MethodPost,
			url: "/api/orgs/" + orgP1 + "/members", body: `{"user":"frank@orgs.test","role":"viewer","tags":["choir"]}`, status: 200,
			after: func(t testing.TB, app *tests.TestApp) {
				got := auditEntries(t, app, "targetCollection='_orgMembers' && action='create' && actorId='"+orgUserID("bob")+"'")
				if len(got) != 1 {
					t.Fatalf("expected one member entry by bob, got %d", len(got))
				}

				e := got[0]
				if e.GetString("actorType") != "record" || e.GetString("actorCollection") != "users" {
					t.Fatalf("unexpected actor %s/%s", e.GetString("actorType"), e.GetString("actorCollection"))
				}

				if meta := orgAuditMeta(t, e); meta["orgId"] != orgP1 {
					t.Fatalf("expected meta.orgId %s, got %v", orgP1, meta)
				}

				// the new tag row is audited too
				tags := auditEntries(t, app, "targetCollection='_orgRoles' && action='create' && actorType='record'")
				if len(tags) != 1 || orgAuditMeta(t, tags[0])["orgId"] != orgP1 {
					t.Fatalf("expected one tag entry with meta.orgId, got %d", len(tags))
				}
			},
		},
		{
			name: "a role change records before and after", who: "bob", method: http.MethodPatch,
			url: "/api/orgs/" + orgP1 + "/members/mem00000000000e", body: `{"role":"member"}`, status: 200,
			after: func(t testing.TB, app *tests.TestApp) {
				got := auditEntries(t, app, "targetCollection='_orgMembers' && action='update' && actorId='"+orgUserID("bob")+"'")
				if len(got) != 1 {
					t.Fatalf("expected one entry, got %d", len(got))
				}

				e := got[0]

				var changed []string
				_ = e.UnmarshalJSONField("changedFields", &changed)
				if strings.Join(changed, ",") != "role" {
					t.Fatalf("changed fields %v", changed)
				}

				var before, after map[string]any
				_ = e.UnmarshalJSONField("before", &before)
				_ = e.UnmarshalJSONField("after", &after)

				if before["role"] != "viewer" || after["role"] != "member" {
					t.Fatalf("before %v after %v", before, after)
				}

				if orgAuditMeta(t, e)["orgId"] != orgP1 {
					t.Fatal("missing meta.orgId")
				}
			},
		},
		{
			name: "a superuser bypass is audited as the superuser", who: "super", method: http.MethodPatch,
			url: "/api/orgs/" + orgP1 + "/members/mem00000000000b", body: `{"role":"admin"}`, status: 409,
			after: func(t testing.TB, app *tests.TestApp) {
				// the last-owner guard holds for superusers: nothing was written
				if n := len(auditEntries(t, app, "targetCollection='_orgMembers' && action='update'")); n != 0 {
					t.Fatalf("expected no entry, got %d", n)
				}
			},
		},
		{
			name: "a superuser changes a member and is the actor", who: "super", method: http.MethodPatch,
			url: "/api/orgs/" + orgP1 + "/members/mem00000000000d", body: `{"status":"suspended"}`, status: 200,
			after: func(t testing.TB, app *tests.TestApp) {
				got := auditEntries(t, app, "targetCollection='_orgMembers' && action='update'")
				if len(got) != 1 || got[0].GetString("actorType") != "superuser" || got[0].GetString("actorId") == "" {
					t.Fatalf("expected one superuser entry, got %d", len(got))
				}

				if orgAuditMeta(t, got[0])["orgId"] != orgP1 {
					t.Fatal("missing meta.orgId")
				}
			},
		},
		{
			name: "archiving is audited", who: "alice", method: http.MethodPatch, url: "/api/orgs/" + orgP1,
			body: `{"archived":true}`, status: 200,
			after: func(t testing.TB, app *tests.TestApp) {
				got := auditEntries(t, app, "targetCollection='_orgs' && action='update' && actorId='"+orgUserID("alice")+"'")
				if len(got) != 1 {
					t.Fatalf("expected one entry, got %d", len(got))
				}

				var changed []string
				_ = got[0].UnmarshalJSONField("changedFields", &changed)
				if strings.Join(changed, ",") != "archived" || orgAuditMeta(t, got[0])["orgId"] != orgP1 {
					t.Fatalf("changed %v meta %v", changed, orgAuditMeta(t, got[0]))
				}
			},
		},
		{
			name: "creating an org is audited with its own id", who: "super", method: http.MethodPost, url: "/api/orgs",
			body: `{"name":"Fresh Church","owner":"frank@orgs.test"}`, status: 200,
			after: func(t testing.TB, app *tests.TestApp) {
				org, err := core.FindOrg(app, "fresh-church")
				orgMust(t, err)

				got := auditEntries(t, app, "targetCollection='_orgs' && action='create' && actorType='superuser'")
				if len(got) != 1 || orgAuditMeta(t, got[0])["orgId"] != org.Id {
					t.Fatalf("expected one entry with the org id, got %d", len(got))
				}

				own := auditEntries(t, app, "targetCollection='_orgMembers' && action='create' && actorType='superuser'")
				if len(own) != 1 || orgAuditMeta(t, own[0])["orgId"] != org.Id {
					t.Fatalf("expected the owner membership to be audited, got %d", len(own))
				}
			},
		},
		{
			name: "removing a member is audited", who: "bob", method: http.MethodDelete,
			url: "/api/orgs/" + orgP1 + "/members/mem00000000000e", status: 204,
			after: func(t testing.TB, app *tests.TestApp) {
				got := auditEntries(t, app, "targetCollection='_orgMembers' && action='delete'")
				if len(got) != 1 || got[0].GetString("actorId") != orgUserID("bob") || orgAuditMeta(t, got[0])["orgId"] != orgP1 {
					t.Fatalf("expected one delete entry by bob, got %d", len(got))
				}
			},
		},
		{
			name: "a refused change leaves no entry", who: "carol", method: http.MethodPatch,
			url: "/api/orgs/" + orgP1 + "/members/mem00000000000e", body: `{"role":"member"}`, status: 403,
			after: func(t testing.TB, app *tests.TestApp) {
				if n := len(auditEntries(t, app, "targetCollection='_orgMembers' && action='update'")); n != 0 {
					t.Fatalf("expected no entry, got %d", n)
				}
			},
		},
		{
			name: "org-owned records carry meta.orgId", who: "carol", method: http.MethodPost,
			url: "/api/collections/docs/records", body: `{"title":"n","org":"` + orgP1 + `"}`, status: 200,
			factory: presetFactory("members", "org"),
			after: func(t testing.TB, app *tests.TestApp) {
				got := auditEntries(t, app, "targetCollection='docs' && action='create' && actorType='record'")
				if len(got) != 1 || orgAuditMeta(t, got[0])["orgId"] != orgP1 {
					t.Fatalf("expected one entry with meta.orgId, got %d", len(got))
				}
			},
		},
	})
}

func TestOrgWebhooksUseCollectionNames(t *testing.T) {
	t.Parallel()

	runOrgCases(t, []orgCase{{
		name: "webhook filters on _orgMembers events", who: "bob", method: http.MethodPost,
		url: "/api/orgs/" + orgP1 + "/members", body: `{"user":"frank@orgs.test"}`, status: 200,
		factory: func(t testing.TB) *tests.TestApp {
			app := orgsFactory(t)

			ep := seedWebhookEndpoint(t, app, "https://example.com/hook")
			ep.Set("events", []string{"_orgMembers:create", "_orgs:*"})
			orgMust(t, app.Save(ep))

			// deliveries from the fixture itself do not count
			all, _ := app.FindAllRecords(core.CollectionNameWebhookDeliveries)
			for _, d := range all {
				orgMust(t, app.Delete(d))
			}

			return app
		},
		after: func(t testing.TB, app *tests.TestApp) {
			all, err := app.FindAllRecords(core.CollectionNameWebhookDeliveries)
			orgMust(t, err)

			if len(all) != 1 {
				t.Fatalf("expected exactly the _orgMembers:create delivery (not the tag or other events), got %d", len(all))
			}

			raw, _ := json.Marshal(all[0].Get("payload"))
			if !strings.Contains(string(raw), `"collection":"_orgMembers"`) || !strings.Contains(string(raw), `"org":"`+orgP1+`"`) {
				t.Fatalf("unexpected payload %s", raw)
			}
		},
	}})
}

func TestOrgAPIKeysCannotChangeOrgs(t *testing.T) {
	t.Parallel()

	env := newAPIKeyEnv(t)
	seedOrgs(t, env.app)

	// a "*" key owned by a superuser: the strongest key there is
	key, _ := env.mint(core.CollectionNameSuperusers, akSuperuserId, "*")

	// reads work (the key acts as its owner)
	if status, body := env.do("GET", "/api/orgs", "", key); status != 200 || !strings.Contains(body, `"totalItems":3`) {
		t.Fatalf("list: %d %s", status, body)
	}

	// every write is refused
	for _, c := range []struct{ method, url, body string }{
		{"POST", "/api/orgs", `{"name":"Evil Church"}`},
		{"PATCH", "/api/orgs/" + orgP1, `{"name":"x"}`},
		{"DELETE", "/api/orgs/" + orgA, `{"confirm":"closed"}`},
		{"POST", "/api/orgs/" + orgP1 + "/members", `{"user":"frank@orgs.test","role":"owner"}`},
		{"PATCH", "/api/orgs/" + orgP1 + "/members/mem00000000000d", `{"role":"owner"}`},
		{"DELETE", "/api/orgs/" + orgP1 + "/members/mem00000000000d", ``},
	} {
		if status, body := env.do(c.method, c.url, c.body, key); status != 403 {
			t.Fatalf("%s %s: expected 403, got %d %s", c.method, c.url, status, body)
		}
	}

	// the membership collections are not reachable with a key either
	if status, _ := env.do("GET", "/api/collections/_orgMembers/records", "", key); status != 403 {
		t.Fatalf("expected the records API to refuse the key, got %d", status)
	}

	// a user-owned key with a narrow scope cannot reach the org routes at all
	userKey, _ := env.mint("users", orgUserID("bob"), "collection:docs:read")
	if status, _ := env.do("GET", "/api/orgs", "", userKey); status != 403 {
		t.Fatalf("expected 403 for a scoped key, got %d", status)
	}
}

func TestOrgNoBehaviourChangeWithoutOrgs(t *testing.T) {
	t.Parallel()

	// A fresh app has the collections but no rows; ordinary writes are
	// untouched: no org meta in the audit entries, no extra checks.
	app, err := tests.NewTestApp()
	orgMust(t, err)
	defer app.Cleanup()

	for _, name := range []string{core.CollectionNameOrgs, core.CollectionNameOrgMembers, core.CollectionNameOrgRoles} {
		n, err := app.CountRecords(name)
		orgMust(t, err)

		if n != 0 {
			t.Fatalf("%s has %d rows in a fresh app", name, n)
		}
	}

	users, _ := app.FindCollectionByNameOrId("users")
	u := core.NewRecord(users)
	u.SetEmail("plain@orgs.test")
	u.SetPassword("password123")
	orgMust(t, app.Save(u))

	entries := auditEntries(t, app, "targetCollection='users' && targetId='"+u.Id+"'")
	if len(entries) != 1 {
		t.Fatalf("expected one entry, got %d", len(entries))
	}

	if _, ok := orgAuditMeta(t, entries[0])["orgId"]; ok {
		t.Fatal("a plain write must not get meta.orgId")
	}

	// the org API answers with empty lists
	runOrgCases(t, []orgCase{
		{name: "the user lists no orgs", who: "bob", url: "/api/orgs", status: 200, has: []string{`"items":[]`},
			factory: func(t testing.TB) *tests.TestApp {
				a, err := tests.NewTestApp()
				orgMust(t, err)

				users, _ := a.FindCollectionByNameOrId("users")
				r := core.NewRecord(users)
				r.Id = orgUserID("bob")
				r.SetEmail("bob@orgs.test")
				r.Set("tokenKey", orgTokenKey("bob"))
				r.SetPassword("password123")
				orgMust(t, a.Save(r))

				return a
			}},
	})
}
