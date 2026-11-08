package cmd_test

import (
	"testing"

	"github.com/doodlemania2/claviger/cmd"
	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tests"
)

func TestSuperuserTOTPDisableCommand(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	su, err := app.FindAuthRecordByEmail(core.CollectionNameSuperusers, "test@example.com")
	if err != nil {
		t.Fatal(err)
	}

	row := core.NewTOTP(app)
	row.SetCollectionRef(su.Collection().Id)
	row.SetRecordRef(su.Id)
	if err := row.SetSecret(app, "JBSWY3DPEHPK3PXP"); err != nil {
		t.Fatal(err)
	}
	row.SetEnabled(true)
	if err := app.Save(row); err != nil {
		t.Fatal(err)
	}

	mfa := core.NewMFA(app)
	mfa.SetCollectionRef(su.Collection().Id)
	mfa.SetRecordRef(su.Id)
	mfa.SetMethod(core.MFAMethodPassword)
	if err := app.Save(mfa); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) error {
		command := cmd.NewSuperuserCommand(app)
		command.SetArgs(args)
		command.SilenceErrors = true
		return command.Execute()
	}

	if err := run("totp-disable", "missing@example.com"); err == nil {
		t.Fatal("expected an error for an unknown superuser")
	}
	if err := run("totp-disable", "invalid"); err == nil {
		t.Fatal("expected an error for an invalid email")
	}
	if on, _ := core.HasActiveTOTP(app, su); !on {
		t.Fatal("enrollment must be untouched after failed runs")
	}

	if err := run("totp-disable", "test@example.com"); err != nil {
		t.Fatal(err)
	}
	if on, _ := core.HasActiveTOTP(app, su); on {
		t.Fatal("enrollment must be gone")
	}
	if _, err := app.FindMFAById(mfa.Id); err == nil {
		t.Fatal("pending MFA sessions must be cleared")
	}

	// idempotent
	if err := run("totp-disable", "test@example.com"); err != nil {
		t.Fatal(err)
	}
}
