package apis_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// Fork-local (Claviger #110): admin action audit entries.

func requireSuperuserEntry(t testing.TB, e *core.Record, action, target string) {
	t.Helper()
	if e.GetString("action") != action || e.GetString("targetCollection") != target ||
		e.GetString("actorType") != core.AuditActorTypeSuperuser || e.GetString("actorId") != "sywbhecnh46rhm0" ||
		e.GetString("ip") == "" {
		t.Fatalf("Unexpected entry: %s", e.PublicExport())
	}
}

func TestAuditSettingsUpdate(t *testing.T) {
	t.Parallel()

	(&tests.ApiScenario{
		Name:   "settings change is audited with redacted secrets",
		Method: http.MethodPatch,
		URL:    "/api/settings",
		Body: strings.NewReader(`{
			"meta": {"appName": "audited"},
			"smtp": {"enabled": true, "host": "mail.example.com", "port": 587, "password": "smtp-top-secret"},
			"s3": {"enabled": true, "bucket": "b", "region": "r", "endpoint": "https://example.com", "accessKey": "ak", "secret": "s3-top-secret"},
			"captcha": {"secretKey": "captcha-top-secret"}
		}`),
		Headers:        map[string]string{"Authorization": auditSuperuserToken},
		ExpectedStatus: 200,
		ExpectedContent: []string{
			`"appName":"audited"`,
		},

		AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
			entries := auditEntries(t, app, "action='settings.update' && actorType='superuser'")
			if len(entries) != 1 {
				t.Fatalf("Expected 1 entry, got %d", len(entries))
			}
			e := entries[0]
			requireSuperuserEntry(t, e, core.AuditActionSettingsUpdate, "_params")
			if e.GetString("targetId") != "settings" {
				t.Fatalf("Unexpected target: %s", e.PublicExport())
			}

			raw := e.GetString("changedFields") + tests.CompactJSON(e.GetString("before")) + tests.CompactJSON(e.GetString("after"))
			for _, secret := range []string{"smtp-top-secret", "s3-top-secret", "captcha-top-secret"} {
				if strings.Contains(raw, secret) {
					t.Fatalf("Secret %q leaked into the audit entry: %s", secret, raw)
				}
			}

			changed := e.GetString("changedFields")
			for _, k := range []string{`"meta.appName"`, `"smtp.host"`, `"smtp.password"`, `"s3.secret"`, `"captcha.secretKey"`} {
				if !strings.Contains(changed, k) {
					t.Fatalf("Expected %s in changedFields %s", k, changed)
				}
			}
			if !strings.Contains(tests.CompactJSON(e.GetString("after")), `"smtp.password":"[redacted]"`) ||
				!strings.Contains(tests.CompactJSON(e.GetString("after")), `"meta.appName":"audited"`) {
				t.Fatalf("Unexpected after: %s", tests.CompactJSON(e.GetString("after")))
			}
		},
	}).Test(t)
}

func TestAuditCollectionSchemaChanges(t *testing.T) {
	t.Parallel()

	(&tests.ApiScenario{
		Name:   "collection create",
		Method: http.MethodPost,
		URL:    "/api/collections",
		Body: strings.NewReader(`{"name":"auditcol","type":"base","fields":[{"name":"title","type":"text"}],
			"listRule":"@request.auth.id != ''"}`),
		Headers:         map[string]string{"Authorization": auditSuperuserToken},
		ExpectedStatus:  200,
		ExpectedContent: []string{`"name":"auditcol"`},

		AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
			entries := auditEntries(t, app, "action='collection.create' && actorType='superuser'")
			if len(entries) != 1 {
				t.Fatalf("Expected 1 entry, got %d", len(entries))
			}
			requireSuperuserEntry(t, entries[0], core.AuditActionCollectionCreate, "_collections")
			if !strings.Contains(tests.CompactJSON(entries[0].GetString("meta")), `"collectionName":"auditcol"`) ||
				!strings.Contains(tests.CompactJSON(entries[0].GetString("after")), `"listRule":"@request.auth.id != ''"`) {
				t.Fatalf("Unexpected entry: %s", entries[0].PublicExport())
			}
		},
	}).Test(t)

	(&tests.ApiScenario{
		Name:   "collection update: field add/remove/rename/type change, rule and index changes",
		Method: http.MethodPatch,
		URL:    "/api/collections/auditupd",
		BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
			c := core.NewBaseCollection("auditupd")
			c.Fields.Add(
				&core.TextField{Id: "f_keep", Name: "keep"},
				&core.TextField{Id: "f_ren", Name: "oldname"},
				&core.TextField{Id: "f_typ", Name: "typed"},
				&core.TextField{Id: "f_del", Name: "gone"},
			)
			if err := app.Save(c); err != nil {
				t.Fatal(err)
			}
		},
		Body: strings.NewReader(`{
			"listRule": "id = ''",
			"fields": [
				{"id":"text3208210256","name":"id","type":"text","primaryKey":true,"system":true,"required":true,"pattern":"^[a-z0-9]+$","min":15,"max":15,"autogeneratePattern":"[a-z0-9]{15}"},
				{"id":"f_keep","name":"keep","type":"text"},
				{"id":"f_ren","name":"newname","type":"text"},
				{"id":"f_typ","name":"typed","type":"text","required":true},
				{"name":"brandnew","type":"text"}
			],
			"indexes": ["CREATE INDEX ` + "`idx_audit_new`" + ` ON ` + "`auditupd`" + ` (` + "`keep`" + `)"]
		}`),
		Headers:         map[string]string{"Authorization": auditSuperuserToken},
		ExpectedStatus:  200,
		ExpectedContent: []string{`"name":"auditupd"`},
		AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
			entries := auditEntries(t, app, "action='collection.update' && actorType='superuser'")
			if len(entries) != 1 {
				t.Fatalf("Expected 1 entry, got %d", len(entries))
			}
			e := entries[0]
			requireSuperuserEntry(t, e, core.AuditActionCollectionUpdate, "_collections")
			meta := tests.CompactJSON(e.GetString("meta"))
			for _, want := range []string{
				`"collectionName":"auditupd"`,
				`"added":[{"name":"brandnew"`,
				`"removed":[{"name":"gone"`,
				`"renamed":[{"from":"oldname","to":"newname"}]`,
				`"modified":[{"name":"typed","properties":["required"]}]`,
				`idx_audit_new`,
			} {
				if !strings.Contains(meta, want) {
					t.Fatalf("Expected %s in meta %s", want, meta)
				}
			}
			if !strings.Contains(e.GetString("changedFields"), `"listRule"`) ||
				!strings.Contains(e.GetString("changedFields"), `"fields"`) ||
				!strings.Contains(e.GetString("changedFields"), `"indexes"`) ||
				!strings.Contains(tests.CompactJSON(e.GetString("after")), `"listRule":"id = ''"`) {
				t.Fatalf("Unexpected diff: %s", e.PublicExport())
			}
		},
	}).Test(t)

	(&tests.ApiScenario{
		Name:   "collection delete",
		Method: http.MethodDelete,
		URL:    "/api/collections/auditdel",
		BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
			if err := app.Save(core.NewBaseCollection("auditdel")); err != nil {
				t.Fatal(err)
			}
		},
		Headers:        map[string]string{"Authorization": auditSuperuserToken},
		ExpectedStatus: 204,

		AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
			entries := auditEntries(t, app, "action='collection.delete' && actorType='superuser'")
			if len(entries) != 1 {
				t.Fatalf("Expected 1 entry, got %d", len(entries))
			}
			requireSuperuserEntry(t, entries[0], core.AuditActionCollectionDelete, "_collections")
			if !strings.Contains(tests.CompactJSON(entries[0].GetString("meta")), `"collectionName":"auditdel"`) {
				t.Fatalf("Unexpected entry: %s", entries[0].PublicExport())
			}
		},
	}).Test(t)

	(&tests.ApiScenario{
		Name:   "collections import marks entries and redacts oauth2 secrets",
		Method: http.MethodPut,
		URL:    "/api/collections/import",
		Body: strings.NewReader(`{"deleteMissing":false,"collections":[
			{"name":"auditimp","type":"base","fields":[{"name":"x","type":"text"}]},
			{"name":"users","oauth2":{"enabled":true,"providers":[{"name":"github","clientId":"cid","clientSecret":"oauth-top-secret"}]}}
		]}`),
		Headers:        map[string]string{"Authorization": auditSuperuserToken},
		ExpectedStatus: 204,

		AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
			entries := auditEntries(t, app, "targetCollection='_collections' && actorType='superuser'")
			if len(entries) != 2 {
				t.Fatalf("Expected 2 entries, got %d", len(entries))
			}
			for _, e := range entries {
				if e.GetString("actorType") != core.AuditActorTypeSuperuser || e.GetString("ip") == "" ||
					!strings.Contains(tests.CompactJSON(e.GetString("meta")), `"import":true`) {
					t.Fatalf("Unexpected entry: %s", e.PublicExport())
				}
				if strings.Contains(tests.CompactJSON(e.GetString("before"))+tests.CompactJSON(e.GetString("after"))+tests.CompactJSON(e.GetString("meta")), "oauth-top-secret") {
					t.Fatalf("OAuth2 secret leaked: %s", e.PublicExport())
				}
			}
			actions := entries[0].GetString("action") + "," + entries[1].GetString("action")
			if !strings.Contains(actions, "collection.create") || !strings.Contains(actions, "collection.update") {
				t.Fatalf("Unexpected actions: %s", actions)
			}
		},
	}).Test(t)
}
