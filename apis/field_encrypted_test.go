package apis_test

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// Fork-local (Claviger #75). Not parallel: the key is set through the environment.

func setEncKEK(t *testing.T) {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAVIGER_KEK", base64.StdEncoding.EncodeToString(b))
	t.Setenv("CLAVIGER_KEK_FILE", "")
}

// encSetup creates the "apienc" collection (readable and creatable by anyone
// signed in as a user, with a hidden encrypted field) and one record.
func encSetup(t testing.TB, app *tests.TestApp) {
	open := ""
	col := core.NewBaseCollection("apienc")
	col.ListRule = &open
	col.ViewRule = &open
	col.CreateRule = &open
	col.UpdateRule = &open
	col.Fields.Add(
		&core.TextField{Name: "name"},
		&core.EncryptedTextField{Name: "phone", Max: 30},
		&core.EncryptedTextField{Name: "ssn", Hidden: true},
	)
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}

	r := core.NewRecord(col)
	r.Id = "encrecord000001"
	r.Set("name", "Ann")
	r.Set("phone", "+1 555 0100")
	r.Set("ssn", "123-45-6789")
	if err := app.Save(r); err != nil {
		t.Fatal(err)
	}
}

func TestEncryptedFieldAPI(t *testing.T) {
	setEncKEK(t)

	su := map[string]string{"Authorization": auditSuperuserToken}

	scenarios := []tests.ApiScenario{
		{
			Name:           "list returns plaintext and hides the hidden encrypted field",
			Method:         http.MethodGet,
			URL:            "/api/collections/apienc/records",
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) { encSetup(t, app) },
			ExpectedStatus: 200,
			ExpectedContent: []string{
				`"phone":"+1 555 0100"`,
			},
			NotExpectedContent: []string{`"ssn"`, `123-45-6789`, `enc:v1:`},
		},
		{
			Name:           "filter on an encrypted field is a clear 400",
			Method:         http.MethodGet,
			URL:            "/api/collections/apienc/records?filter=" + "phone%3D%22%2B1+555+0100%22",
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) { encSetup(t, app) },
			ExpectedStatus: 400,
			ExpectedContent: []string{
				`"message":"Encrypted fields cannot be used in filters, sorts or API rules`,
			},
		},
		{
			Name:           "filter on a hidden encrypted field is rejected for superusers too",
			Method:         http.MethodGet,
			URL:            "/api/collections/apienc/records?filter=ssn~%22123%22",
			Headers:        su,
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) { encSetup(t, app) },
			ExpectedStatus: 400,
			ExpectedContent: []string{
				`Encrypted fields cannot be used`,
			},
		},
		{
			Name:           "sort by an encrypted field is a clear 400",
			Method:         http.MethodGet,
			URL:            "/api/collections/apienc/records?sort=-phone",
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) { encSetup(t, app) },
			ExpectedStatus: 400,
			ExpectedContent: []string{
				`Encrypted fields cannot be used`,
			},
		},
		{
			Name:           "plain filters and sorts still work",
			Method:         http.MethodGet,
			URL:            "/api/collections/apienc/records?filter=name%3D%22Ann%22&sort=-name",
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) { encSetup(t, app) },
			ExpectedStatus: 200,
			ExpectedContent: []string{
				`"totalItems":1`,
			},
		},
		{
			Name:    "create round trips and stores ciphertext",
			Method:  http.MethodPost,
			URL:     "/api/collections/apienc/records",
			Body:    strings.NewReader(`{"id":"encrecord000002","name":"Bob","phone":"+1 555 0111","ssn":"987-65-4321"}`),
			Headers: su,
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				encSetup(t, app)
			},
			ExpectedStatus:  200,
			ExpectedContent: []string{`"phone":"+1 555 0111"`, `"ssn":"987-65-4321"`}, // superusers see hidden fields
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
				var phone, ssn string
				err := app.DB().Select("phone", "ssn").From("apienc").Where(dbx.HashExp{"id": "encrecord000002"}).Row(&phone, &ssn)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.HasPrefix(phone, "enc:v1:") || !strings.HasPrefix(ssn, "enc:v1:") ||
					strings.Contains(phone+ssn, "555") || strings.Contains(ssn, "987") {
					t.Fatalf("expected ciphertext in the database, got %q / %q", phone, ssn)
				}

				// the audit log never sees the plaintext
				entries, _ := app.FindAllRecords(core.CollectionNameAuditLog, dbx.HashExp{"targetId": "encrecord000002"})
				if len(entries) != 1 {
					t.Fatalf("expected 1 audit entry, got %d", len(entries))
				}
				if s := entries[0].GetString("after"); strings.Contains(s, "555") || strings.Contains(s, "987") {
					t.Fatalf("audit entry leaks the plaintext: %s", s)
				}
			},
		},
		{
			Name:   "pasting another record's ciphertext is rejected",
			Method: http.MethodPost,
			URL:    "/api/collections/apienc/records",
			Body:   strings.NewReader(`{"id":"encrecord000003","phone":"enc:v1:abcd:AAAA"}`),
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				encSetup(t, app)
			},
			ExpectedStatus:  400,
			ExpectedContent: []string{`"phone":{"code":"validation_reserved_prefix"`},
		},
		{
			Name:   "too long value is a validation error",
			Method: http.MethodPost,
			URL:    "/api/collections/apienc/records",
			Body:   strings.NewReader(`{"phone":"` + strings.Repeat("9", 31) + `"}`),
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				encSetup(t, app)
			},
			ExpectedStatus:  400,
			ExpectedContent: []string{`"phone":{"code":"validation_max_text_constraint"`},
		},
		{
			Name:           "a regular user reads plaintext but never the hidden field",
			Method:         http.MethodGet,
			URL:            "/api/collections/apienc/records/encrecord000001",
			Headers:        map[string]string{"Authorization": auditUserToken},
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) { encSetup(t, app) },
			ExpectedStatus: 200,
			ExpectedContent: []string{
				`"phone":"+1 555 0100"`,
			},
			NotExpectedContent: []string{`"ssn"`, `123-45-6789`},
		},
		{
			Name:           "a superuser reads the hidden encrypted field as plaintext",
			Method:         http.MethodGet,
			URL:            "/api/collections/apienc/records/encrecord000001",
			Headers:        su,
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) { encSetup(t, app) },
			ExpectedStatus: 200,
			ExpectedContent: []string{
				`"phone":"+1 555 0100"`,
				`"ssn":"123-45-6789"`,
			},
			NotExpectedContent: []string{`enc:v1:`},
		},
		{
			Name:    "the data keys list is superuser only",
			Method:  http.MethodGet,
			URL:     "/api/collections/_fieldKeys/records",
			Headers: map[string]string{"Authorization": auditUserToken},
			BeforeTestFunc: func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
				encSetup(t, app)
			},
			ExpectedStatus:  403,
			ExpectedContent: []string{`Only superusers can perform this action`},
		},
	}

	for _, s := range scenarios {
		s.Test(t)
	}
}

func encJSONSetup(t testing.TB, app *tests.TestApp) {
	open := ""
	col := core.NewBaseCollection("apiencjson")
	col.ListRule = &open
	col.ViewRule = &open
	col.CreateRule = &open
	col.Fields.Add(
		&core.TextField{Name: "name"},
		&core.EncryptedJSONField{Name: "profile", MaxSize: 100},
		&core.EncryptedJSONField{Name: "secretdoc", Hidden: true},
	)
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}

	r := core.NewRecord(col)
	r.Id = "encjson0000001x"
	r.Set("name", "Ann")
	r.Set("profile", map[string]any{"phone": "+1 555 0100"})
	r.Set("secretdoc", map[string]any{"ssn": "123-45-6789"})
	if err := app.Save(r); err != nil {
		t.Fatal(err)
	}
}

func TestEncryptedJSONFieldAPI(t *testing.T) {
	setEncKEK(t)

	su := map[string]string{"Authorization": auditSuperuserToken}
	setup := func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) { encJSONSetup(t, app) }

	scenarios := []tests.ApiScenario{
		{
			Name:               "list returns the JSON and hides the hidden field",
			Method:             http.MethodGet,
			URL:                "/api/collections/apiencjson/records",
			BeforeTestFunc:     setup,
			ExpectedStatus:     200,
			ExpectedContent:    []string{`"profile":{"phone":"+1 555 0100"}`},
			NotExpectedContent: []string{`secretdoc`, `123-45-6789`, `enc:v1:`},
		},
		{
			Name:           "filter on it is a clear 400",
			Method:         http.MethodGet,
			URL:            "/api/collections/apiencjson/records?filter=profile%3D%22x%22",
			BeforeTestFunc: setup,
			ExpectedStatus: 400,
			ExpectedContent: []string{
				`Encrypted fields cannot be used in filters, sorts or API rules`,
			},
		},
		{
			Name:           "sort by it is a clear 400",
			Method:         http.MethodGet,
			URL:            "/api/collections/apiencjson/records?sort=profile",
			BeforeTestFunc: setup,
			ExpectedStatus: 400,
			ExpectedContent: []string{
				`Encrypted fields cannot be used`,
			},
		},
		{
			Name:           "create stores ciphertext and keeps the audit log clean",
			Method:         http.MethodPost,
			URL:            "/api/collections/apiencjson/records",
			Body:           strings.NewReader(`{"id":"encjson0000002x","name":"Bob","profile":{"phone":"+1 555 0111"},"secretdoc":["a","b"]}`),
			Headers:        su,
			BeforeTestFunc: setup,
			ExpectedStatus: 200,
			ExpectedContent: []string{
				`"profile":{"phone":"+1 555 0111"}`, `"secretdoc":["a","b"]`,
			},
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
				var p, s string
				if err := app.DB().Select("profile", "secretdoc").From("apiencjson").Where(dbx.HashExp{"id": "encjson0000002x"}).Row(&p, &s); err != nil {
					t.Fatal(err)
				}
				if !strings.HasPrefix(p, "enc:v1:") || !strings.HasPrefix(s, "enc:v1:") || strings.Contains(p, "555") {
					t.Fatalf("expected ciphertext in the database, got %q / %q", p, s)
				}
				entries, _ := app.FindAllRecords(core.CollectionNameAuditLog, dbx.HashExp{"targetId": "encjson0000002x"})
				if len(entries) != 1 || strings.Contains(entries[0].GetString("after"), "555") {
					t.Fatalf("audit entry missing or leaking: %v", entries)
				}
			},
		},
		{
			Name:            "a string that looks like ciphertext is just a JSON string",
			Method:          http.MethodPost,
			URL:             "/api/collections/apiencjson/records",
			Body:            strings.NewReader(`{"id":"encjson0000003x","profile":"enc:v1:abcd:AAAA"}`),
			BeforeTestFunc:  setup,
			ExpectedStatus:  200,
			ExpectedContent: []string{`"profile":"enc:v1:abcd:AAAA"`},
		},
		{
			Name:            "too large is a validation error",
			Method:          http.MethodPost,
			URL:             "/api/collections/apiencjson/records",
			Body:            strings.NewReader(`{"profile":"` + strings.Repeat("9", 120) + `"}`),
			BeforeTestFunc:  setup,
			ExpectedStatus:  400,
			ExpectedContent: []string{`"profile":{"code":"validation_json_size_limit"`},
		},
	}

	for _, s := range scenarios {
		s.Test(t)
	}
}
