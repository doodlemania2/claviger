package apis_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// Fork-local (Claviger #74).

const (
	auditSuperuserToken = "eyJhbGciOiJIUzI1NiJ9.eyJpZCI6InN5d2JoZWNuaDQ2cmhtMCIsInR5cGUiOiJhdXRoIiwiY29sbGVjdGlvbklkIjoicGJjXzMxNDI2MzU4MjMiLCJleHAiOjI1MjQ2MDQ0NjEsInJlZnJlc2hhYmxlIjp0cnVlfQ.UXgO3j-0BumcugrFjbd7j0M4MQvbrLggLlcu_YNGjoY"
	auditUserToken      = "eyJhbGciOiJIUzI1NiJ9.eyJpZCI6IjRxMXhsY2xtZmxva3UzMyIsInR5cGUiOiJhdXRoIiwiY29sbGVjdGlvbklkIjoiX3BiX3VzZXJzX2F1dGhfIiwiZXhwIjoyNTI0NjA0NDYxLCJyZWZyZXNoYWJsZSI6dHJ1ZX0.ZT3F0Z3iM-xbGgSG3LEKiEzHrPHr8t8IuHLZGGNuxLo"
)

func auditEntries(t testing.TB, app core.App, filter string) []*core.Record {
	entries, err := app.FindRecordsByFilter(core.CollectionNameAuditLog, filter, "created", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func seedAuditEntry(t testing.TB, app core.App) *core.Record {
	err := core.WriteAuditEntry(app, nil, &core.AuditEntry{
		Action:         core.AuditActionCreate,
		Actor:          core.AuditActor{Type: core.AuditActorTypeSystem},
		CollectionName: "demo2",
		RecordId:       "seeded",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return auditEntries(t, app, "targetId='seeded'")[0]
}

func TestAuditLogRecordChanges(t *testing.T) {
	t.Parallel()

	scenarios := []tests.ApiScenario{
		{
			Name:   "create as superuser",
			Method: http.MethodPost,
			URL:    "/api/collections/demo2/records",
			Body:   strings.NewReader(`{"id":"auditcreate0001","title":"hello"}`),
			Headers: map[string]string{
				"Authorization": auditSuperuserToken,
			},
			ExpectedStatus:  200,
			ExpectedContent: []string{`"id":"auditcreate0001"`},
			ExpectedEvents:  map[string]int{"OnAuditEntry": 1},
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
				entries := auditEntries(t, app, "targetId='auditcreate0001'")
				if len(entries) != 1 {
					t.Fatalf("Expected 1 entry, got %d", len(entries))
				}
				e := entries[0]
				if e.GetString("action") != "create" || e.GetString("actorType") != "superuser" ||
					e.GetString("actorCollection") != core.CollectionNameSuperusers || e.GetString("actorId") != "sywbhecnh46rhm0" ||
					e.GetString("targetCollection") != "demo2" || e.GetString("ip") == "" {
					t.Fatalf("Unexpected entry: %s", e.PublicExport())
				}
				if !strings.Contains(e.GetString("changedFields"), `"title"`) || !strings.Contains(e.GetString("after"), `"title":"hello"`) {
					t.Fatalf("Unexpected diff: %s", e.PublicExport())
				}
			},
		},
		{
			Name:   "update as regular user records before and after",
			Method: http.MethodPatch,
			URL:    "/api/collections/demo2/records/llvuca81nly1qls",
			Body:   strings.NewReader(`{"title":"changed"}`),
			Headers: map[string]string{
				"Authorization": auditUserToken,
			},
			ExpectedStatus:  200,
			ExpectedContent: []string{`"title":"changed"`},
			ExpectedEvents:  map[string]int{"OnAuditEntry": 1},
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
				entries := auditEntries(t, app, "targetId='llvuca81nly1qls'")
				if len(entries) != 1 {
					t.Fatalf("Expected 1 entry, got %d", len(entries))
				}
				e := entries[0]
				if e.GetString("action") != "update" || e.GetString("actorType") != "record" ||
					e.GetString("actorCollection") != "users" || e.GetString("actorId") != "4q1xlclmfloku33" {
					t.Fatalf("Unexpected entry: %s", e.PublicExport())
				}
				if !strings.Contains(e.GetString("before"), `"title":"test1"`) || !strings.Contains(e.GetString("after"), `"title":"changed"`) {
					t.Fatalf("Unexpected diff: before=%s after=%s", e.GetString("before"), e.GetString("after"))
				}
				if e.GetString("changedFields") != `["title"]` {
					t.Fatalf("Unexpected changed fields: %s", e.GetString("changedFields"))
				}
			},
		},
		{
			Name:   "no-op update leaves no entry",
			Method: http.MethodPatch,
			URL:    "/api/collections/demo2/records/llvuca81nly1qls",
			Body:   strings.NewReader(`{"title":"test1"}`),
			Headers: map[string]string{
				"Authorization": auditUserToken,
			},
			ExpectedStatus:  200,
			ExpectedContent: []string{`"title":"test1"`},
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
				if n := len(auditEntries(t, app, "targetId='llvuca81nly1qls'")); n != 0 {
					t.Fatalf("Expected no entries, got %d", n)
				}
			},
		},
		{
			Name:   "delete as superuser",
			Method: http.MethodDelete,
			URL:    "/api/collections/demo2/records/llvuca81nly1qls",
			Headers: map[string]string{
				"Authorization": auditSuperuserToken,
			},
			ExpectedStatus: 204,
			// the delete itself + the cascade update of the records referencing it
			ExpectedEvents: map[string]int{"OnAuditEntry": 2},
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
				entries := auditEntries(t, app, "targetId='llvuca81nly1qls'")
				if len(entries) != 1 {
					t.Fatalf("Expected 1 entry, got %d", len(entries))
				}
				if n := len(auditEntries(t, app, "actorType='superuser' && actorId='sywbhecnh46rhm0'")); n != 2 {
					t.Fatalf("Expected the cascade entry to be attributed to the same actor (2 entries), got %d", n)
				}
				e := entries[0]
				if e.GetString("action") != "delete" || !strings.Contains(e.GetString("before"), `"title":"test1"`) || strings.Contains(e.GetString("after"), "title") {
					t.Fatalf("Unexpected entry: %s", e.PublicExport())
				}
			},
		},
		{
			Name:   "anonymous create is attributed to system and redacts password and tokenKey",
			Method: http.MethodPost,
			URL:    "/api/collections/users/records",
			Body: strings.NewReader(`{
				"id":"auditpwuser0001",
				"email":"audit-pw@example.com",
				"password":"correct-horse-battery",
				"passwordConfirm":"correct-horse-battery"
			}`),
			ExpectedStatus:  200,
			ExpectedContent: []string{`"id":"auditpwuser0001"`},
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
				entries := auditEntries(t, app, "targetId='auditpwuser0001'")
				if len(entries) != 1 {
					t.Fatalf("Expected 1 entry, got %d", len(entries))
				}
				e := entries[0]
				if e.GetString("actorType") != "system" {
					t.Fatalf("Expected system actor, got %s", e.GetString("actorType"))
				}
				after := e.GetString("after")
				record, _ := app.FindRecordById("users", "auditpwuser0001")
				for _, leaked := range []string{"correct-horse-battery", record.GetString("tokenKey"), record.GetString("password:hash")} {
					if leaked != "" && strings.Contains(e.GetString("before")+after, leaked) {
						t.Fatalf("Leaked sensitive value %q in %s", leaked, after)
					}
				}
				if !strings.Contains(after, `"password":"[redacted]"`) || !strings.Contains(after, `"tokenKey":"[redacted]"`) {
					t.Fatalf("Expected redacted password and tokenKey, got %s", after)
				}
				if !strings.Contains(after, `"email":"audit-pw@example.com"`) {
					t.Fatalf("Expected visible email, got %s", after)
				}
			},
		},
	}

	for _, scenario := range scenarios {
		scenario.Test(t)
	}
}

func TestAuditLogReadAccess(t *testing.T) {
	t.Parallel()

	scenarios := []tests.ApiScenario{
		{
			Name:            "superuser can list",
			Method:          http.MethodGet,
			URL:             "/api/collections/" + core.CollectionNameAuditLog + "/records",
			Headers:         map[string]string{"Authorization": auditSuperuserToken},
			BeforeTestFunc:  func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) { seedAuditEntry(t, app) },
			ExpectedStatus:  200,
			ExpectedContent: []string{`"totalItems":1`, `"targetId":"seeded"`},
			ExpectedEvents:  map[string]int{"OnRecordsListRequest": 1, "OnRecordEnrich": 1},
		},
		{
			Name:            "regular auth user cannot list",
			Method:          http.MethodGet,
			URL:             "/api/collections/" + core.CollectionNameAuditLog + "/records",
			Headers:         map[string]string{"Authorization": auditUserToken},
			ExpectedStatus:  403,
			ExpectedContent: []string{`"data":{}`},
			ExpectedEvents:  map[string]int{"*": 0},
		},
		{
			Name:            "anonymous cannot list",
			Method:          http.MethodGet,
			URL:             "/api/collections/" + core.CollectionNameAuditLog + "/records",
			ExpectedStatus:  403,
			ExpectedContent: []string{`"data":{}`},
			ExpectedEvents:  map[string]int{"*": 0},
		},
		{
			Name:    "regular auth user cannot view",
			Method:  http.MethodGet,
			URL:     "/api/collections/" + core.CollectionNameAuditLog + "/records/x",
			Headers: map[string]string{"Authorization": auditUserToken},

			ExpectedStatus:  403,
			ExpectedContent: []string{`"data":{}`},
			ExpectedEvents:  map[string]int{"*": 0},
		},
	}

	for _, scenario := range scenarios {
		scenario.Test(t)
	}
}

func TestAuditLogAppendOnlyViaRecordsAPI(t *testing.T) {
	t.Parallel()

	var seededId string
	seed := func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) { seededId = seedAuditEntry(t, app).Id }
	assertUnchanged := func(t testing.TB, app *tests.TestApp, res *http.Response) {
		entry, err := app.FindRecordById(core.CollectionNameAuditLog, seededId)
		if err != nil || entry.GetString("targetId") != "seeded" || entry.GetString("action") != "create" {
			t.Fatalf("Expected the entry to be unchanged, got %v (%v)", entry, err)
		}
	}
	superuser := map[string]string{"Authorization": auditSuperuserToken}

	scenarios := []tests.ApiScenario{
		{
			Name:            "superuser cannot create",
			Method:          http.MethodPost,
			URL:             "/api/collections/" + core.CollectionNameAuditLog + "/records",
			Body:            strings.NewReader(`{"action":"create","targetId":"forged"}`),
			Headers:         superuser,
			BeforeTestFunc:  seed,
			ExpectedStatus:  400,
			ExpectedContent: []string{`"status":400`},
			ExpectedEvents:  map[string]int{"OnAuditEntry": 0},
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
				if n := len(auditEntries(t, app, "targetId='forged'")); n != 0 {
					t.Fatalf("Expected no forged entry, got %d", n)
				}
			},
		},
		{
			Name:            "superuser cannot update",
			Method:          http.MethodPatch,
			URL:             "", // set below
			Body:            strings.NewReader(`{"action":"delete"}`),
			Headers:         superuser,
			BeforeTestFunc:  seed,
			ExpectedStatus:  400,
			ExpectedContent: []string{`"message"`},
			AfterTestFunc:   assertUnchanged,
		},
		{
			Name:            "superuser cannot delete",
			Method:          http.MethodDelete,
			URL:             "", // set below
			Headers:         superuser,
			BeforeTestFunc:  seed,
			ExpectedStatus:  400,
			ExpectedContent: []string{`"status":400`},
			AfterTestFunc:   assertUnchanged,
		},
		{
			Name:            "superuser cannot truncate the collection",
			Method:          http.MethodDelete,
			URL:             "/api/collections/" + core.CollectionNameAuditLog + "/truncate",
			Headers:         superuser,
			BeforeTestFunc:  seed,
			ExpectedStatus:  400,
			ExpectedContent: []string{`"status":400`},
			AfterTestFunc:   assertUnchanged,
		},
		{
			Name:            "superuser cannot delete the collection",
			Method:          http.MethodDelete,
			URL:             "/api/collections/" + core.CollectionNameAuditLog,
			Headers:         superuser,
			BeforeTestFunc:  seed,
			ExpectedStatus:  400,
			ExpectedContent: []string{`"status":400`},
			AfterTestFunc:   assertUnchanged,
		},
		{
			Name:            "superuser cannot rename the collection",
			Method:          http.MethodPatch,
			URL:             "/api/collections/" + core.CollectionNameAuditLog,
			Body:            strings.NewReader(`{"name":"renamed"}`),
			Headers:         superuser,
			ExpectedStatus:  400,
			ExpectedContent: []string{`"status":400`},
		},
		{
			Name:            "superuser cannot loosen the rules",
			Method:          http.MethodPatch,
			URL:             "/api/collections/" + core.CollectionNameAuditLog,
			Body:            strings.NewReader(`{"listRule":"","createRule":"","updateRule":"","deleteRule":""}`),
			Headers:         superuser,
			ExpectedStatus:  400,
			ExpectedContent: []string{`"status":400`},
		},
		{
			Name:            "superuser cannot add a field",
			Method:          http.MethodPatch,
			URL:             "/api/collections/" + core.CollectionNameAuditLog,
			Body:            strings.NewReader(`{"fields":[{"name":"extra","type":"text"}]}`),
			Headers:         superuser,
			ExpectedStatus:  400,
			ExpectedContent: []string{`"status":400`},
		},
		{
			Name:            "superuser cannot rewrite the indexes",
			Method:          http.MethodPatch,
			URL:             "/api/collections/" + core.CollectionNameAuditLog,
			Body:            strings.NewReader(`{"indexes":[]}`),
			Headers:         superuser,
			ExpectedStatus:  400,
			ExpectedContent: []string{`"status":400`},
		},
		{
			Name:            "superuser cannot modify the table with SQL",
			Method:          http.MethodPost,
			URL:             "/api/sql",
			Body:            strings.NewReader(`{"query":"DELETE FROM ` + core.CollectionNameAuditLog + `"}`),
			Headers:         superuser,
			BeforeTestFunc:  seed,
			ExpectedStatus:  400,
			ExpectedContent: []string{`"status":400`},
			AfterTestFunc:   assertUnchanged,
		},
	}

	for i := range scenarios {
		s := scenarios[i]
		if s.URL == "" {
			// the id is only known after seeding, so resolve the URL lazily
			// by registering the entry with a fixed id
			s.URL = "/api/collections/" + core.CollectionNameAuditLog + "/records/seededauditentry1"
			prev := s.BeforeTestFunc
			s.BeforeTestFunc = func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				prev(t, app, e)
				// re-key the seeded entry with raw SQL (test setup only)
				if _, err := app.NonconcurrentDB().NewQuery("UPDATE `" + core.CollectionNameAuditLog + "` SET id='seededauditentry1'").Execute(); err != nil {
					t.Fatal(err)
				}
				seededId = "seededauditentry1"
			}
		}
		s.Test(t)
	}
}
