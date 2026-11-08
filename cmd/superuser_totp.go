package cmd

import (
	"errors"
	"fmt"

	"ganigeorgiev.com/pocketbase/ozzo-validation/v4/is"
	"github.com/doodlemania2/claviger/core"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

// superuserTOTPDisableCommand is the recovery path for a superuser who lost
// both the authenticator app and the backup codes: whoever can run the binary
// against the data directory (shell access to the server) can remove the
// enrollment. The superuser can then sign in with the password alone and enroll
// again.
func superuserTOTPDisableCommand(app core.App) *cobra.Command {
	command := &cobra.Command{
		Use:          "totp-disable",
		Example:      "superuser totp-disable test@example.com",
		Short:        "Removes the TOTP (authenticator app) enrollment and backup codes of the specified superuser",
		SilenceUsage: true,
		RunE: func(command *cobra.Command, args []string) error {
			if len(args) == 0 || args[0] == "" || is.EmailFormat.Validate(args[0]) != nil {
				return errors.New("invalid or missing email address")
			}

			superuser, err := app.FindAuthRecordByEmail(core.CollectionNameSuperusers, args[0])
			if err != nil {
				return fmt.Errorf("superuser with email %q doesn't exist", args[0])
			}

			if _, err := core.FindTOTPByRecord(app, superuser); err != nil {
				color.Yellow("Superuser %q has no TOTP enrollment, nothing to do.", superuser.Email())
				return nil
			}

			if err := core.DeleteTOTPByRecord(app, superuser); err != nil {
				return fmt.Errorf("failed to remove the TOTP enrollment: %w", err)
			}

			// drop half finished sign-ins so they cannot be completed against the old state
			if err := app.DeleteAllMFAsByRecord(superuser); err != nil {
				return fmt.Errorf("TOTP removed but failed to clear the pending MFA sessions: %w", err)
			}

			color.Green("Successfully removed the TOTP enrollment of superuser %q.", superuser.Email())
			return nil
		},
	}

	return command
}
