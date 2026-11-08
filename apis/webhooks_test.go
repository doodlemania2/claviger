package apis_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// Fork-local (Claviger #77).

func webhookSuperHeaders() map[string]string {
	return map[string]string{"Authorization": auditSuperuserToken}
}

func seedWebhookEndpoint(t testing.TB, app core.App, url string) *core.Record {
	col, err := app.FindCollectionByNameOrId(core.CollectionNameWebhooks)
	if err != nil {
		t.Fatal(err)
	}
	r := core.NewRecord(col)
	r.Id = "whapitest000001"
	r.Set("url", url)
	r.Set("events", []string{"*"})
	r.Set("enabled", true)
	if err := core.SetWebhookSecret(app, r, core.GenerateWebhookSecret()); err != nil {
		t.Fatal(err)
	}
	if err := app.Save(r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestWebhooksCreate(t *testing.T) {
	t.Parallel()

	scenarios := []tests.ApiScenario{
		{
			Name:            "unauthorized",
			Method:          http.MethodPost,
			URL:             "/api/webhooks",
			Body:            strings.NewReader(`{"url":"https://example.com/h","events":["*"]}`),
			ExpectedStatus:  401,
			ExpectedContent: []string{`"data":{}`},
		},
		{
			Name:            "regular user",
			Method:          http.MethodPost,
			URL:             "/api/webhooks",
			Body:            strings.NewReader(`{"url":"https://example.com/h","events":["*"]}`),
			Headers:         map[string]string{"Authorization": auditUserToken},
			ExpectedStatus:  403,
			ExpectedContent: []string{`"data":{}`},
		},
		{
			Name:            "plain http is refused",
			Method:          http.MethodPost,
			URL:             "/api/webhooks",
			Body:            strings.NewReader(`{"url":"http://example.com/h","events":["*"]}`),
			Headers:         webhookSuperHeaders(),
			ExpectedStatus:  400,
			ExpectedContent: []string{`"url":{`},
		},
		{
			Name:            "metadata address is refused",
			Method:          http.MethodPost,
			URL:             "/api/webhooks",
			Body:            strings.NewReader(`{"url":"https://169.254.169.254/x","events":["*"]}`),
			Headers:         webhookSuperHeaders(),
			ExpectedStatus:  400,
			ExpectedContent: []string{`"url":{`},
		},
		{
			Name:            "missing events",
			Method:          http.MethodPost,
			URL:             "/api/webhooks",
			Body:            strings.NewReader(`{"url":"https://example.com/h"}`),
			Headers:         webhookSuperHeaders(),
			ExpectedStatus:  400,
			ExpectedContent: []string{`"events":{`},
		},
		{
			Name:               "success shows the secret once",
			Method:             http.MethodPost,
			URL:                "/api/webhooks",
			Body:               strings.NewReader(`{"url":"https://example.com/h","events":["demo2:create"],"description":"d"}`),
			Headers:            webhookSuperHeaders(),
			ExpectedStatus:     200,
			ExpectedContent:    []string{`"secret":"whsec_`, `"enabled":true`, `"url":"https://example.com/h"`},
			NotExpectedContent: []string{`"prevSecret"`},
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
				recs, _ := app.FindAllRecords(core.CollectionNameWebhooks)
				if len(recs) != 1 || recs[0].GetString("secret") == "" {
					t.Fatalf("endpoint not stored with a secret")
				}
			},
		},
	}

	for _, s := range scenarios {
		s.Test(t)
	}
}

func TestWebhooksRecordsApi(t *testing.T) {
	t.Parallel()

	scenarios := []tests.ApiScenario{
		{
			Name:    "list never exposes secrets",
			Method:  http.MethodGet,
			URL:     "/api/collections/_webhooks/records",
			Headers: webhookSuperHeaders(),
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				seedWebhookEndpoint(t, app, "https://example.com/h")
			},
			ExpectedStatus:     200,
			ExpectedContent:    []string{`"totalItems":1`},
			NotExpectedContent: []string{`whsec_`, `"secret"`, `"prevSecret"`},
		},
		{
			Name:            "generic create is blocked",
			Method:          http.MethodPost,
			URL:             "/api/collections/_webhooks/records",
			Headers:         webhookSuperHeaders(),
			Body:            strings.NewReader(`{"url":"https://example.com/h","events":["*"],"enabled":true}`),
			ExpectedStatus:  400,
			ExpectedContent: []string{`Use POST /api/webhooks`},
		},
		{
			Name:            "regular user cannot list",
			Method:          http.MethodGet,
			URL:             "/api/collections/_webhooks/records",
			Headers:         map[string]string{"Authorization": auditUserToken},
			ExpectedStatus:  403,
			ExpectedContent: []string{`"data":{}`},
		},
	}

	for _, s := range scenarios {
		s.Test(t)
	}

	// the update scenario needs the seeded id in the URL
	(&tests.ApiScenario{
		Name:    "update cannot replace the secret",
		Method:  http.MethodPatch,
		URL:     "/api/collections/_webhooks/records/whapitest000001",
		Headers: webhookSuperHeaders(),
		Body:    strings.NewReader(`{"secret":"whsec_AAAA","description":"changed"}`),
		BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
			seedWebhookEndpoint(t, app, "https://example.com/h")
		},
		ExpectedStatus:  200,
		ExpectedContent: []string{`"description":"changed"`},
		AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
			r, _ := app.FindRecordById(core.CollectionNameWebhooks, "whapitest000001")
			if strings.Contains(r.GetString("secret"), "AAAA") {
				t.Fatal("secret was overwritten through the records API")
			}
		},
	}).Test(t)
}

func seedBlockedEndpoint(t testing.TB, app *tests.TestApp) {
	ep := seedWebhookEndpoint(t, app, "https://example.com/h")
	// A private target is refused before any connection, so the test event
	// fails fast and offline. Written straight to the db because the
	// config-time validation rejects it.
	if _, err := app.NonconcurrentDB().NewQuery("UPDATE _webhooks SET url='https://10.0.0.1/h' WHERE id={:id}").
		Bind(map[string]any{"id": ep.Id}).Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestWebhooksTestRotateRedeliver(t *testing.T) {
	t.Parallel()

	scenarios := []tests.ApiScenario{
		{
			Name: "test event records a failed delivery", Method: http.MethodPost, URL: "/api/webhooks/whapitest000001/test",
			Headers: webhookSuperHeaders(), ExpectedStatus: 200,
			BeforeTestFunc:  func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) { seedBlockedEndpoint(t, app) },
			ExpectedContent: []string{`"eventType":"webhook.test"`, `"status":"dead"`, `not allowed`},
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
				ds, _ := app.FindAllRecords(core.CollectionNameWebhookDeliveries)
				if len(ds) != 1 {
					t.Fatalf("expected the test delivery to be logged, got %d", len(ds))
				}
			},
		},
		{
			Name: "rotate returns a new secret once", Method: http.MethodPost, URL: "/api/webhooks/whapitest000001/rotate-secret",
			Headers: webhookSuperHeaders(), ExpectedStatus: 200,
			BeforeTestFunc:     func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) { seedBlockedEndpoint(t, app) },
			ExpectedContent:    []string{`"secret":"whsec_`},
			NotExpectedContent: []string{`"prevSecret"`},
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
				r, _ := app.FindRecordById(core.CollectionNameWebhooks, "whapitest000001")
				if r.GetString("prevSecret") == "" || r.GetString("secret") == r.GetString("prevSecret") {
					t.Fatal("rotation did not keep the previous secret")
				}
			},
		},
		{
			Name: "unknown endpoint", Method: http.MethodPost, URL: "/api/webhooks/missing/test",
			Headers: webhookSuperHeaders(), ExpectedStatus: 404, ExpectedContent: []string{`"data":{}`},
		},
		{
			Name: "regular user cannot send a test", Method: http.MethodPost, URL: "/api/webhooks/whapitest000001/test",
			Headers: map[string]string{"Authorization": auditUserToken}, ExpectedStatus: 403, ExpectedContent: []string{`"data":{}`},
		},
		{
			Name: "test events cannot be redelivered", Method: http.MethodPost, URL: "/api/webhook-deliveries/whdeliv00000001/redeliver",
			Headers: webhookSuperHeaders(), ExpectedStatus: 400, ExpectedContent: []string{`cannot be redelivered`},
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				ep := seedWebhookEndpoint(t, app, "https://example.com/h")
				d := core.NewRecord(mustCollection(t, app, core.CollectionNameWebhookDeliveries))
				d.Id = "whdeliv00000001"
				d.Set("webhook", ep.Id)
				d.Set("eventType", core.WebhookEventTest)
				d.Set("status", core.WebhookDeliveryDead)
				if err := app.UnsafeWithoutHooks().SaveNoValidate(d); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			Name: "deliveries are read-only through the records API", Method: http.MethodPatch,
			URL:     "/api/collections/_webhookDeliveries/records/whdeliv00000001",
			Headers: webhookSuperHeaders(), Body: strings.NewReader(`{"status":"pending"}`), ExpectedStatus: 403,
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				ep := seedWebhookEndpoint(t, app, "https://example.com/h")
				col, _ := app.FindCollectionByNameOrId(core.CollectionNameWebhookDeliveries)
				d := core.NewRecord(col)
				d.Id = "whdeliv00000001"
				d.Set("webhook", ep.Id)
				d.Set("eventType", "record.created")
				d.Set("status", core.WebhookDeliveryDead)
				if err := app.UnsafeWithoutHooks().SaveNoValidate(d); err != nil {
					t.Fatal(err)
				}
			},
			ExpectedContent: []string{`read-only`},
		},
	}

	for _, s := range scenarios {
		s.Test(t)
	}
}

func mustCollection(t testing.TB, app core.App, name string) *core.Collection {
	c, err := app.FindCollectionByNameOrId(name)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
