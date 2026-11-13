package cmd

import (
	"fmt"

	"github.com/doodlemania2/claviger/core"
	"github.com/spf13/cobra"
)

// Fork-local (Claviger #87).

// NewRegistersCommand creates the `registers` command group.
func NewRegistersCommand(app core.App) *cobra.Command {
	command := &cobra.Command{
		Use:   "registers",
		Short: "Maintain permanent registers",
	}

	command.AddCommand(registersVerifyCommand(app))

	return command
}

func registersVerifyCommand(app core.App) *cobra.Command {
	command := &cobra.Command{
		Use:   "verify",
		Short: "Recomputes the hash chain of the permanent registers and reports the breaks",
		Long: "For every permanent register that has the hash chain on, recomputes the chain from the final\n" +
			"entries and reports an entry that was changed, removed, inserted or renumbered outside the\n" +
			"application (raw SQL, a bad restore). It prints the head of each chain; keep it somewhere else if\n" +
			"you also want to notice entries removed from the end. Exits with an error when a break is found.\n" +
			"Read-only. To check a backup, restore it into a scratch data directory and run this against it\n" +
			"with --dir.",
		Example:      "registers verify",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			if _, err := app.FindCachedCollectionByNameOrId(core.CollectionNamePermanentCollections); err != nil {
				return fmt.Errorf("permanent registers are not available in this database")
			}

			results, err := core.VerifyRegisters(app)
			if err != nil {
				return err
			}

			out := command.OutOrStdout()
			breaks := 0

			if len(results) == 0 {
				fmt.Fprintln(out, "no permanent registers are configured")
			}

			for _, r := range results {
				switch {
				case !r.Chain:
					fmt.Fprintf(out, "%s: %d final entries, hash chain off (nothing to verify)\n", r.Collection, r.Finalized)
				case r.OK():
					fmt.Fprintf(out, "%s: ok, %d final entries, head %d %s\n", r.Collection, r.Finalized, r.HeadSeq, r.HeadHash)
				default:
					fmt.Fprintf(out, "%s: %d break(s) in %d final entries\n", r.Collection, len(r.Breaks), r.Finalized)
				}

				for _, b := range r.Breaks {
					fmt.Fprintf(out, "  entry %s (number %d): %s\n", b.Entry, b.Seq, b.Problem)
				}

				breaks += len(r.Breaks)
			}

			if breaks > 0 {
				return fmt.Errorf("%d break(s) found", breaks)
			}

			return nil
		},
	}

	return command
}
