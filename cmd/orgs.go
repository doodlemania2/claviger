package cmd

import (
	"fmt"

	"github.com/doodlemania2/claviger/core"
	"github.com/spf13/cobra"
)

// Fork-local (Claviger #84).

// NewOrgsCommand creates the `orgs` command group.
func NewOrgsCommand(app core.App) *cobra.Command {
	command := &cobra.Command{
		Use:   "orgs",
		Short: "Maintain organizations",
	}

	command.AddCommand(orgsClosureRepairCommand(app))

	return command
}

func orgsClosureRepairCommand(app core.App) *cobra.Command {
	var dryRun bool

	command := &cobra.Command{
		Use:   "closure-repair",
		Short: "Rebuilds the org closure table from the parent of each organization",
		Long: "Compares the _orgClosure table with the organization tree (_orgs.parent) and fixes every\n" +
			"missing, extra or wrong-depth row in one transaction, then prints what it found.\n" +
			"Run it after restoring a backup or editing the database by hand. It is idempotent.\n" +
			"Stop the server first, or run it against a stopped instance's --dir.",
		Example:      "orgs closure-repair --dry-run",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			rep, err := core.RepairOrgClosure(app, dryRun)
			if err != nil {
				return err
			}

			out := command.OutOrStdout()
			fmt.Fprintf(out, "closure rows: expected %d, found %d\n", rep.Expected, rep.Actual)
			fmt.Fprintf(out, "differences: %d missing, %d extra, %d wrong depth\n", rep.Missing, rep.Extra, rep.WrongDepth)

			for _, id := range rep.Cycles {
				fmt.Fprintf(out, "warning: organization %s has a parent chain that loops or is too deep\n", id)
			}

			switch {
			case rep.Differences() == 0:
				fmt.Fprintln(out, "the closure table is consistent")
			case rep.Applied:
				fmt.Fprintln(out, "repaired")
			default:
				fmt.Fprintln(out, "dry run: nothing changed")
			}

			return nil
		},
	}

	command.Flags().BoolVar(&dryRun, "dry-run", false, "report the differences without changing anything")

	return command
}
