package apis_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

// Fork-local (Claviger #88): data export limits in the settings.

func TestSettingsDataExportPatch(t *testing.T) {
	t.Parallel()

	rejected := map[string]int{"OnSettingsUpdateRequest": 1, "OnModelUpdate": 1, "OnModelValidate": 1, "OnModelAfterUpdateError": 1}
	superuser := map[string]string{"Authorization": auditSuperuserToken}

	(&tests.ApiScenario{
		Name:    "patch stores the limits and returns them",
		Method:  http.MethodPatch,
		URL:     "/api/settings",
		Body:    strings.NewReader(`{"dataExport":{"maxFileMB":5,"maxTotalMB":50,"retentionDays":2,"selfRequestsPerHour":1,"adminRequestsPerHour":9}}`),
		Headers: superuser,
		AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
			c := app.Settings().DataExport
			if c.MaxFileMB != 5 || c.MaxTotalMB != 50 || c.RetentionDays != 2 || c.SelfRequestsPerHour != 1 || c.AdminRequestsPerHour != 9 {
				t.Fatalf("limits not stored: %+v", c)
			}
			if core.DataExportRetentionDays(app) != 2 || core.DataExportMaxFileMB(app) != 5 {
				t.Fatal("accessors do not read the settings")
			}
		},
		ExpectedStatus:  200,
		ExpectedContent: []string{`"dataExport":{`, `"maxFileMB":5`, `"retentionDays":2`, `"adminRequestsPerHour":9`},
		ExpectedEvents:  map[string]int{},
	}).Test(t)

	(&tests.ApiScenario{
		Name:            "out of range value is rejected",
		Method:          http.MethodPatch,
		URL:             "/api/settings",
		Body:            strings.NewReader(`{"dataExport":{"retentionDays":4000}}`),
		Headers:         superuser,
		ExpectedStatus:  400,
		ExpectedContent: []string{`"dataExport":{`, `"retentionDays":{`},
		ExpectedEvents:  rejected,
	}).Test(t)

	(&tests.ApiScenario{
		Name:            "negative value is rejected",
		Method:          http.MethodPatch,
		URL:             "/api/settings",
		Body:            strings.NewReader(`{"dataExport":{"maxFileMB":-1}}`),
		Headers:         superuser,
		ExpectedStatus:  400,
		ExpectedContent: []string{`"maxFileMB":{`},
		ExpectedEvents:  rejected,
	}).Test(t)
}
