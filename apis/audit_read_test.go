package apis_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/apis"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
	"github.com/doodlemania2/claviger/tools/subscriptions"
	"github.com/doodlemania2/claviger/tools/types"
)

// Fork-local (Claviger #86, 86-3): read audit for flagged collections.

const auditReadOrgId = "auditreadorg001"

// seedAuditReadApp creates the "audit_notes" collection (public view/list,
// an org relation), n records in it and, optionally, flags it.
func seedAuditReadApp(t testing.TB, app core.App, n int, flag bool) {
	t.Helper()

	orgs, err := app.FindCollectionByNameOrId(core.CollectionNameOrgs)
	if err != nil {
		t.Fatal(err)
	}
	org := core.NewRecord(orgs)
	org.Id = auditReadOrgId
	org.Set("name", "Audit Parish")
	org.Set("slug", "audit-parish")
	if err := app.Save(org); err != nil {
		t.Fatal(err)
	}

	col := core.NewBaseCollection("audit_notes")
	col.ListRule = types.Pointer("")
	col.ViewRule = types.Pointer("")
	col.Fields.Add(
		&core.TextField{Name: "title"},
		&core.RelationField{Name: "org", CollectionId: orgs.Id, MaxSelect: 1},
	)
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}

	for i := 1; i <= n; i++ {
		r := core.NewRecord(col)
		r.Id = fmt.Sprintf("auditnote%06d", i)
		r.Set("title", fmt.Sprintf("note %d", i))
		r.Set("org", auditReadOrgId)
		if err := app.Save(r); err != nil {
			t.Fatal(err)
		}
	}

	if flag {
		app.Settings().Audit.ReadCollections = []string{"audit_notes"}
		if err := app.Save(app.Settings()); err != nil {
			t.Fatal(err)
		}
	}

	// drop the entries written while seeding
	if _, err := core.DeleteExpiredAuditEntries(app, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
}

func auditReadFactory(n int, flag bool) func(t testing.TB) *tests.TestApp {
	return func(t testing.TB) *tests.TestApp {
		app, err := tests.NewTestApp()
		if err != nil {
			t.Fatal(err)
		}
		seedAuditReadApp(t, app, n, flag)
		return app
	}
}

func readEntries(t testing.TB, app core.App) []*core.Record {
	return auditEntries(t, app, "action='read'")
}

func TestAuditReadView(t *testing.T) {
	t.Parallel()

	(&tests.ApiScenario{
		Name:            "superuser view of a flagged collection",
		Method:          http.MethodGet,
		URL:             "/api/collections/audit_notes/records/auditnote000002",
		Headers:         map[string]string{"Authorization": auditSuperuserToken},
		TestAppFactory:  auditReadFactory(3, true),
		ExpectedStatus:  200,
		ExpectedContent: []string{`"id":"auditnote000002"`},
		ExpectedEvents:  map[string]int{"OnAuditEntry": 1, "OnRecordViewRequest": 1, "OnRecordEnrich": 1},
		AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
			entries := readEntries(t, app)
			if len(entries) != 1 {
				t.Fatalf("Expected 1 read entry, got %d", len(entries))
			}
			e := entries[0]
			if e.GetString("actorType") != "superuser" || e.GetString("actorId") != "sywbhecnh46rhm0" ||
				e.GetString("targetCollection") != "audit_notes" || e.GetString("targetId") != "auditnote000002" ||
				e.GetString("ip") == "" {
				t.Fatalf("Unexpected entry: %s", e.PublicExport())
			}
			meta := tests.CompactJSON(e.GetString("meta"))
			for _, want := range []string{`"orgId":"` + auditReadOrgId + `"`, `"recordIds":["auditnote000002"]`, `"count":1`, `"truncated":false`} {
				if !strings.Contains(meta, want) {
					t.Fatalf("Expected %s in meta %s", want, meta)
				}
			}
			if strings.Contains(e.GetString("before")+e.GetString("after"), "note 2") {
				t.Fatalf("Record content leaked into the read entry: %s", e.PublicExport())
			}
		},
	}).Test(t)
}

func TestAuditReadList(t *testing.T) {
	t.Parallel()

	(&tests.ApiScenario{
		Name:            "regular user list of a flagged collection",
		Method:          http.MethodGet,
		URL:             "/api/collections/audit_notes/records",
		Headers:         map[string]string{"Authorization": auditUserToken},
		TestAppFactory:  auditReadFactory(3, true),
		ExpectedStatus:  200,
		ExpectedContent: []string{`"totalItems":3`},
		ExpectedEvents:  map[string]int{"OnAuditEntry": 1, "OnRecordsListRequest": 1, "OnRecordEnrich": 3},
		AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
			entries := readEntries(t, app)
			if len(entries) != 1 {
				t.Fatalf("Expected ONE read entry for the whole list, got %d", len(entries))
			}
			e := entries[0]
			if e.GetString("actorType") != "record" || e.GetString("actorCollection") != "users" || e.GetString("targetId") != "" {
				t.Fatalf("Unexpected entry: %s", e.PublicExport())
			}
			meta := tests.CompactJSON(e.GetString("meta"))
			for _, want := range []string{`"count":3`, `"truncated":false`, `"orgId":"` + auditReadOrgId + `"`, `"auditnote000001"`, `"auditnote000003"`} {
				if !strings.Contains(meta, want) {
					t.Fatalf("Expected %s in meta %s", want, meta)
				}
			}
		},
	}).Test(t)

	(&tests.ApiScenario{
		Name:            "anonymous list is attributed to the system actor",
		Method:          http.MethodGet,
		URL:             "/api/collections/audit_notes/records",
		TestAppFactory:  auditReadFactory(1, true),
		ExpectedStatus:  200,
		ExpectedContent: []string{`"totalItems":1`},
		ExpectedEvents:  map[string]int{"OnAuditEntry": 1, "OnRecordsListRequest": 1, "OnRecordEnrich": 1},
		AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
			entries := readEntries(t, app)
			if len(entries) != 1 || entries[0].GetString("actorType") != "system" || entries[0].GetString("ip") == "" {
				t.Fatalf("Unexpected entries: %v", entries)
			}
			if meta := tests.CompactJSON(entries[0].GetString("meta")); !strings.Contains(meta, `"anonymous":true`) {
				t.Fatalf("Expected meta.anonymous, got %s", meta)
			}
		},
	}).Test(t)
}

func TestAuditReadNothingToAudit(t *testing.T) {
	t.Parallel()

	noEntries := func(t testing.TB, app *tests.TestApp, res *http.Response) {
		if n := len(readEntries(t, app)); n != 0 {
			t.Fatalf("Expected no read entries, got %d", n)
		}
	}

	scenarios := []tests.ApiScenario{
		{
			Name:            "unflagged collection (demo2) is not audited",
			Method:          http.MethodGet,
			URL:             "/api/collections/demo2/records",
			Headers:         map[string]string{"Authorization": auditSuperuserToken},
			TestAppFactory:  auditReadFactory(1, true),
			ExpectedStatus:  200,
			ExpectedContent: []string{`"items":[`},
			ExpectedEvents:  map[string]int{"OnAuditEntry": 0, "OnRecordsListRequest": 1, "OnRecordEnrich": 3},
			AfterTestFunc:   noEntries,
		},
		{
			Name:            "unflagged view",
			Method:          http.MethodGet,
			URL:             "/api/collections/demo2/records/0yxhwia2amd8gec",
			Headers:         map[string]string{"Authorization": auditSuperuserToken},
			TestAppFactory:  auditReadFactory(1, true),
			ExpectedStatus:  200,
			ExpectedContent: []string{`"id":"0yxhwia2amd8gec"`},
			ExpectedEvents:  map[string]int{"OnAuditEntry": 0, "OnRecordViewRequest": 1, "OnRecordEnrich": 1},
			AfterTestFunc:   noEntries,
		},
		{
			Name:            "empty setting audits nothing",
			Method:          http.MethodGet,
			URL:             "/api/collections/audit_notes/records",
			TestAppFactory:  auditReadFactory(2, false),
			ExpectedStatus:  200,
			ExpectedContent: []string{`"totalItems":2`},
			ExpectedEvents:  map[string]int{"OnAuditEntry": 0, "OnRecordsListRequest": 1, "OnRecordEnrich": 2},
			AfterTestFunc:   noEntries,
		},
		{
			Name:            "a list with no records writes no entry",
			Method:          http.MethodGet,
			URL:             "/api/collections/audit_notes/records?filter=title='nope'",
			TestAppFactory:  auditReadFactory(2, true),
			ExpectedStatus:  200,
			ExpectedContent: []string{`"totalItems":0`},
			ExpectedEvents:  map[string]int{"OnAuditEntry": 0, "OnRecordsListRequest": 1},
			AfterTestFunc:   noEntries,
		},
		{
			Name:            "a missing record writes no entry",
			Method:          http.MethodGet,
			URL:             "/api/collections/audit_notes/records/missing",
			TestAppFactory:  auditReadFactory(1, true),
			ExpectedStatus:  404,
			ExpectedContent: []string{`"data":{}`},
			ExpectedEvents:  map[string]int{"*": 0},
			AfterTestFunc:   noEntries,
		},
	}

	for _, scenario := range scenarios {
		scenario.Test(t)
	}
}

func TestAuditReadTruncation(t *testing.T) {
	t.Parallel()

	(&tests.ApiScenario{
		Name:            "ids are capped, the count is not",
		Method:          http.MethodGet,
		URL:             "/api/collections/audit_notes/records?perPage=150&sort=id",
		Headers:         map[string]string{"Authorization": auditSuperuserToken},
		TestAppFactory:  auditReadFactory(core.AuditReadMaxRecordIds+5, true),
		ExpectedStatus:  200,
		ExpectedContent: []string{fmt.Sprintf(`"totalItems":%d`, core.AuditReadMaxRecordIds+5)},
		ExpectedEvents:  map[string]int{"*": 0, "OnAuditEntry": 1, "OnRecordsListRequest": 1, "OnRecordEnrich": core.AuditReadMaxRecordIds + 5},
		AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
			entries := readEntries(t, app)
			if len(entries) != 1 {
				t.Fatalf("Expected 1 entry, got %d", len(entries))
			}
			meta := tests.CompactJSON(entries[0].GetString("meta"))
			if !strings.Contains(meta, fmt.Sprintf(`"count":%d`, core.AuditReadMaxRecordIds+5)) || !strings.Contains(meta, `"truncated":true`) {
				t.Fatalf("Unexpected meta: %s", meta)
			}
			if got := strings.Count(meta, `"auditnote`); got != core.AuditReadMaxRecordIds {
				t.Fatalf("Expected %d ids, got %d", core.AuditReadMaxRecordIds, got)
			}
			if !strings.Contains(meta, `"auditnote000100"`) || strings.Contains(meta, `"auditnote000101"`) {
				t.Fatalf("Expected the first %d ids only: %s", core.AuditReadMaxRecordIds, meta)
			}
		},
	}).Test(t)
}

func TestAuditReadExpand(t *testing.T) {
	t.Parallel()

	// the org record is not flagged but audit_notes is, reached through the
	// back-relation expand of the org
	(&tests.ApiScenario{
		Name:            "flagged records returned through expand are audited",
		Method:          http.MethodGet,
		URL:             "/api/collections/" + core.CollectionNameOrgs + "/records/" + auditReadOrgId + "?expand=audit_notes_via_org",
		Headers:         map[string]string{"Authorization": auditSuperuserToken},
		TestAppFactory:  auditReadFactory(2, true),
		ExpectedStatus:  200,
		ExpectedContent: []string{`"audit_notes_via_org":[`, `"id":"auditnote000001"`},
		AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
			entries := readEntries(t, app)
			if len(entries) != 1 {
				t.Fatalf("Expected 1 entry, got %d", len(entries))
			}
			e := entries[0]
			meta := tests.CompactJSON(e.GetString("meta"))
			if e.GetString("targetCollection") != "audit_notes" || e.GetString("actorType") != "superuser" ||
				!strings.Contains(meta, `"via":"expand"`) || !strings.Contains(meta, `"count":2`) {
				t.Fatalf("Unexpected entry: %s", e.PublicExport())
			}
		},
	}).Test(t)
}

func TestAuditReadRealtime(t *testing.T) {
	t.Parallel()

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	if _, err := apis.NewRouter(app); err != nil {
		t.Fatal(err)
	}

	seedAuditReadApp(t, app, 0, true)

	user, err := app.FindAuthRecordByEmail("users", "test@example.com")
	if err != nil {
		t.Fatal(err)
	}

	client := subscriptions.NewDefaultClient()
	client.Set(apis.RealtimeClientAuthKey, user)
	client.Set(apis.RealtimeClientIPKey, "203.0.113.9")
	client.Subscribe("audit_notes/*", "audit_notes")
	app.SubscriptionsBroker().Register(client)

	col, _ := app.FindCollectionByNameOrId("audit_notes")
	rec := core.NewRecord(col)
	rec.Id = "auditnoterealt1"
	rec.Set("title", "live")
	rec.Set("org", auditReadOrgId)
	if err := app.Save(rec); err != nil {
		t.Fatal(err)
	}

	select {
	case msg := <-client.Channel():
		if !strings.Contains(string(msg.Data), `"auditnoterealt1"`) {
			t.Fatalf("Unexpected message: %s", msg.Data)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Timed out waiting for the realtime message")
	}

	entries := readEntries(t, app)
	if len(entries) != 1 {
		t.Fatalf("Expected 1 read entry for 2 matching subscriptions, got %d", len(entries))
	}
	e := entries[0]
	meta := tests.CompactJSON(e.GetString("meta"))
	if e.GetString("actorId") != user.Id || e.GetString("ip") != "203.0.113.9" || e.GetString("targetId") != "auditnoterealt1" ||
		!strings.Contains(meta, `"via":"realtime"`) || !strings.Contains(meta, `"event":"create"`) || !strings.Contains(meta, `"orgId":"`+auditReadOrgId+`"`) {
		t.Fatalf("Unexpected entry: %s", e.PublicExport())
	}
}

func TestAuditReadSettingsValidation(t *testing.T) {
	t.Parallel()

	patch := func(name, body string, status int, content ...string) tests.ApiScenario {
		return tests.ApiScenario{
			Name:            name,
			Method:          http.MethodPatch,
			URL:             "/api/settings",
			Body:            strings.NewReader(body),
			Headers:         map[string]string{"Authorization": auditSuperuserToken},
			ExpectedStatus:  status,
			ExpectedContent: content,
		}
	}

	ok := patch("existing collections incl. a system one", `{"audit":{"readCollections":["demo2","users","_orgs"]}}`, 200,
		`"readCollections":["demo2","users","_orgs"]`)
	ok.AfterTestFunc = func(t testing.TB, app *tests.TestApp, res *http.Response) {
		entries := auditEntries(t, app, "action='settings.update'")
		if len(entries) != 1 || !strings.Contains(entries[0].GetString("changedFields"), `"audit.readCollections"`) {
			t.Fatalf("Expected the flag change in the settings audit entry: %v", entries)
		}
	}

	scenarios := []tests.ApiScenario{
		ok,
		patch("unknown collection", `{"audit":{"readCollections":["demo2","nope"]}}`, 400,
			`"readCollections":{"1":{"code":"validation_audit_read_unknown"`),
		patch("the audit log itself", `{"audit":{"readCollections":["_auditLog"]}}`, 400,
			`"code":"validation_audit_read_self"`),
		patch("duplicate", `{"audit":{"readCollections":["demo2","demo2"]}}`, 400,
			`"code":"validation_audit_read_duplicate"`),
		patch("empty name", `{"audit":{"readCollections":[""]}}`, 400, `"readCollections"`),
		patch("clearing the list", `{"audit":{"readCollections":[]}}`, 200, `"readCollections":[]`),
	}

	for _, scenario := range scenarios {
		scenario.Test(t)
	}
}

func TestAuditRecordReadHelper(t *testing.T) {
	t.Parallel()

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	seedAuditReadApp(t, app, 2, false) // NOT flagged: the helper writes whenever it is called

	col, _ := app.FindCollectionByNameOrId("audit_notes")
	records, err := app.FindAllRecords(col)
	if err != nil {
		t.Fatal(err)
	}

	user, _ := app.FindAuthRecordByEmail("users", "test@example.com")
	ctx := core.WithAuditContext(context.Background(), core.NewAuditActorFromRecord(user), "198.51.100.7")

	extra := map[string]any{"grantId": "grant123", "count": 999}
	if err := core.AuditRecordRead(app, ctx, col, records[:1], extra); err != nil {
		t.Fatal(err)
	}
	if err := core.AuditRecordRead(app, ctx, col, nil, nil); err != nil {
		t.Fatal(err)
	}

	entries := readEntries(t, app)
	if len(entries) != 1 {
		t.Fatalf("Expected 1 entry (none for no records), got %d", len(entries))
	}

	meta := tests.CompactJSON(entries[0].GetString("meta"))
	if !strings.Contains(meta, `"grantId":"grant123"`) || !strings.Contains(meta, `"count":1`) || entries[0].GetString("ip") != "198.51.100.7" {
		t.Fatalf("Unexpected entry: %s", entries[0].PublicExport())
	}
}
