package core_test

import (
	"testing"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

func TestNormalizeRecoveryCode(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]string{
		"abcde fghjk":                "ABCDEFGHJK",
		" ABCDE-FGHJK ":              "ABCDEFGHJK",
		"abcdefghjk":                 "ABCDEFGHJK",
		"ABCDEFGHJKLMNPQRSTUVWXYZ23": "ABCDEFGHJKLMNPQRSTUVWXYZ23",
		"short":                      "",
		"ABCDE FGHJK L":              "",
		"":                           "",
		"ABCDE0FGHJK1":               "ABCDEFGHJK", // 0 and 1 are not in the alphabet
	} {
		if got := core.NormalizeRecoveryCode(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestFormatRecoveryCode(t *testing.T) {
	t.Parallel()

	if got := core.FormatRecoveryCode("ABCDEFGHJK"); got != "ABCDE FGHJK" {
		t.Fatal(got)
	}
}

func TestRecoveryEnvSettingsAreBounded(t *testing.T) {
	t.Setenv(core.RecoveryCodeMinutesEnv, "30")
	t.Setenv(core.RecoveryIssuePerHourEnv, "9")

	if core.RecoveryLifetime().Minutes() != 30 || core.RecoveryIssueLimit() != 9 {
		t.Fatalf("%v %d", core.RecoveryLifetime(), core.RecoveryIssueLimit())
	}

	for _, bad := range []string{"4", "61", "x", "", "-1"} {
		t.Setenv(core.RecoveryCodeMinutesEnv, bad)

		if core.RecoveryLifetime().Minutes() != core.DefaultRecoveryCodeMinutes {
			t.Errorf("%q: %v", bad, core.RecoveryLifetime())
		}
	}
}

func TestRecoveryCollectionAndDataMap(t *testing.T) {
	t.Parallel()

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	col, err := app.FindCollectionByNameOrId(core.CollectionNameRecoveryCodes)
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"user", "org", "issuedBy", "confirmedBy", "channel", "codeHash", "expires", "usedAt", "attempts", "verification", "note", "created", "updated"} {
		if col.Fields.GetByName(name) == nil {
			t.Errorf("missing field %s", name)
		}
	}

	if !col.Fields.GetByName("codeHash").GetHidden() {
		t.Error("codeHash must be hidden")
	}

	if !col.System {
		t.Error("not a system collection")
	}
}
