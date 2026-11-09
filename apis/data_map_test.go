package apis_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// Fork-local (Claviger #88, 88-0).

func TestDataMapUnmappedAPI(t *testing.T) {
	t.Parallel()

	scenarios := []tests.ApiScenario{
		{
			Name:            "guest",
			Method:          http.MethodGet,
			URL:             "/api/data-map/unmapped",
			ExpectedStatus:  401,
			ExpectedContent: []string{`"data":{}`},
			ExpectedEvents:  map[string]int{"*": 0},
		},
		{
			Name:            "superuser, lists app collections",
			Method:          http.MethodGet,
			URL:             "/api/data-map/unmapped",
			Headers:         map[string]string{"Authorization": auditSuperuserToken},
			ExpectedStatus:  200,
			ExpectedContent: []string{`"items":[`, `"name":"demo1"`},
			ExpectedEvents:  map[string]int{"*": 0},
		},
	}

	for _, s := range scenarios {
		s.Test(t)
	}
}

func TestDataMapRecordsAPI(t *testing.T) {
	env := newAPIKeyEnv(t)
	const col = "/api/collections/_dataMap/records"

	// a custom registration works
	status, body := env.do("POST", col, `{"collection":"demo1","treatment":"erase","subjectPaths":["owner"],"personalFields":["text"]}`, auditSuperuserToken)
	if status != 200 || !strings.Contains(body, `"builtin":false`) {
		t.Fatalf("custom entry: %d %s", status, body)
	}

	// the builtin flag cannot be set through the API
	status, _ = env.do("POST", col, `{"collection":"demo2","treatment":"none","builtin":true}`, auditSuperuserToken)
	if status != 403 {
		t.Fatalf("builtin create: expected 403, got %d", status)
	}

	// validation errors come back as 400
	status, body = env.do("POST", col, `{"collection":"demo3","treatment":"retain","subjectPaths":["owner"]}`, auditSuperuserToken)
	if status != 400 || !strings.Contains(body, "retainReason") {
		t.Fatalf("retain without reason: %d %s", status, body)
	}

	// built-in entries cannot be changed or deleted, not even by a superuser
	r, err := env.app.FindFirstRecordByData(core.CollectionNameDataMap, "collection", core.CollectionNameFieldKeys)
	if err != nil {
		t.Fatal(err)
	}
	status, _ = env.do("PATCH", col+"/"+r.Id, `{"treatment":"erase"}`, auditSuperuserToken)
	if status != 403 {
		t.Fatalf("builtin update: expected 403, got %d", status)
	}
	status, _ = env.do("DELETE", col+"/"+r.Id, ``, auditSuperuserToken)
	if status != 403 {
		t.Fatalf("builtin delete: expected 403, got %d", status)
	}

	// a regular user sees nothing (null rules)
	status, _ = env.do("GET", col, ``, "")
	if status != 403 {
		t.Fatalf("guest list: expected 403, got %d", status)
	}
}
