package core_test

import (
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

func TestDataExportConfigValidate(t *testing.T) {
	t.Parallel()

	scenarios := []struct {
		name    string
		c       core.DataExportConfig
		wantErr bool
	}{
		{"zero is valid (defaults)", core.DataExportConfig{}, false},
		{"typical", core.DataExportConfig{MaxFileMB: 100, MaxTotalMB: 1024, RetentionDays: 7, SelfRequestsPerHour: 3, AdminRequestsPerHour: 60}, false},
		{"negative file", core.DataExportConfig{MaxFileMB: -1}, true},
		{"negative total", core.DataExportConfig{MaxTotalMB: -1}, true},
		{"at the upper limits", core.DataExportConfig{MaxFileMB: 1024, MaxTotalMB: 10240, RetentionDays: 30}, false},
		{"retention too long", core.DataExportConfig{RetentionDays: 31}, true},
		{"total too big", core.DataExportConfig{MaxTotalMB: 10241}, true},
		{"self rate too high", core.DataExportConfig{SelfRequestsPerHour: 1001}, true},
		{"admin rate too high", core.DataExportConfig{AdminRequestsPerHour: 10001}, true},
		{"file too big", core.DataExportConfig{MaxFileMB: 1025}, true},
	}
	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			if err := s.c.Validate(); (err != nil) != s.wantErr {
				t.Fatalf("wantErr=%v, got %v", s.wantErr, err)
			}
		})
	}
}

func TestDataExportLimitsDefaultsSettingsAndEnv(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	// the env vars must not leak in from the environment
	t.Setenv(core.DataExportMaxFileMBEnv, "")
	t.Setenv(core.DataExportMaxTotalMBEnv, "")
	t.Setenv(core.DataExportRetentionDaysEnv, "")

	check := func(label string, file, total, days, self, admin int) {
		t.Helper()
		got := [5]int{core.DataExportMaxFileMB(app), core.DataExportMaxTotalMB(app), core.DataExportRetentionDays(app), core.DataExportSelfRequestsPerHour(app), core.DataExportAdminRequestsPerHour(app)}
		want := [5]int{file, total, days, self, admin}
		if got != want {
			t.Fatalf("%s: expected %v, got %v", label, want, got)
		}
	}

	// the test app starts from the defaults (new installation) ...
	check("defaults", 100, 1024, 7, 3, 60)

	// ... and a zero in an older stored row falls back to the defaults
	app.Settings().DataExport = core.DataExportConfig{}
	check("zero values", 100, 1024, 7, 3, 60)

	app.Settings().DataExport = core.DataExportConfig{MaxFileMB: 5, MaxTotalMB: 50, RetentionDays: 2, SelfRequestsPerHour: 1, AdminRequestsPerHour: 9}
	check("settings", 5, 50, 2, 1, 9)

	// the deprecated env vars still win
	t.Setenv(core.DataExportMaxFileMBEnv, "7")
	t.Setenv(core.DataExportMaxTotalMBEnv, "70")
	t.Setenv(core.DataExportRetentionDaysEnv, "3")
	check("env wins", 7, 70, 3, 1, 9)

	// an invalid env value is ignored
	t.Setenv(core.DataExportMaxFileMBEnv, "abc")
	t.Setenv(core.DataExportRetentionDaysEnv, "0")
	check("invalid env ignored", 5, 70, 2, 1, 9)
}
