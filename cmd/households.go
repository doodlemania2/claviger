package cmd

import (
	"fmt"

	"github.com/doodlemania2/claviger/core"
	"github.com/spf13/cobra"
)

// Fork-local (Claviger #80).

// NewHouseholdsCommand creates the `households` command group.
func NewHouseholdsCommand(app core.App) *cobra.Command {
	command := &cobra.Command{
		Use:   "households",
		Short: "Maintain households",
	}

	command.AddCommand(householdsRepairCopiesCommand(app))

	return command
}

func householdsRepairCopiesCommand(app core.App) *cobra.Command {
	var dryRun bool

	command := &cobra.Command{
		Use:   "repair-copies",
		Short: "Resets the household copy of records about people to the household of their member",
		Long: "App collections that describe a person carry a relation to _householdMembers and a copy of the\n" +
			"household. This finds the records whose copy differs from the household of their member and\n" +
			"fixes them in one transaction (the member row is the truth). Run it after restoring a backup or\n" +
			"editing the database by hand. It is idempotent.\n" +
			"Stop the server first, or run it against a stopped instance's --dir.",
		Example:      "households repair-copies --dry-run",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			if !core.HouseholdsAvailable(app) {
				return fmt.Errorf("households are not available in this database")
			}

			rows, err := core.RepairHouseholdCopies(app, dryRun)
			if err != nil {
				return err
			}

			out := command.OutOrStdout()
			total := 0

			for _, r := range rows {
				fmt.Fprintf(out, "%s: %d drifted\n", r.Collection, r.Drifted)
				total += r.Drifted
			}

			switch {
			case total == 0:
				fmt.Fprintln(out, "every household copy is consistent")
			case dryRun:
				fmt.Fprintln(out, "dry run: nothing changed")
			default:
				fmt.Fprintln(out, "repaired")
			}

			return nil
		},
	}

	command.Flags().BoolVar(&dryRun, "dry-run", false, "report the differences without changing anything")

	return command
}
