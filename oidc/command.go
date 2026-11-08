package oidc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/doodlemania2/claviger/core"
	"github.com/spf13/cobra"
)

// NewCommand returns the "oidc" command group.
func NewCommand(app core.App) *cobra.Command {
	command := &cobra.Command{
		Use:   "oidc",
		Short: "Manage the OpenID Connect provider",
	}

	command.AddCommand(rotateKeysCommand(app))

	return command
}

func rotateKeysCommand(app core.App) *cobra.Command {
	var (
		algs  []string
		grace time.Duration
	)

	command := &cobra.Command{
		Use:   "rotate-keys",
		Short: "Create new signing keys and retire the current ones after a grace period",
		Long: "Creates a new active signing key for each algorithm in use (or for --alg), " +
			"and keeps the previous key in the JWKS until the grace period has passed, " +
			"so tokens it signed still verify.",
		Example:      "oidc rotate-keys --grace 168h\noidc rotate-keys --alg ES256",
		SilenceUsage: true,
		RunE: func(command *cobra.Command, args []string) error {
			if ok, why := enabled(app); !ok {
				return errors.New("the OpenID Connect provider is disabled: " + why)
			}

			ids, err := NewStore(app).RotateKeys(context.Background(), algs, grace)
			if err != nil {
				return err
			}

			for _, id := range ids {
				fmt.Fprintf(command.OutOrStdout(), "created signing key %s\n", id)
			}

			fmt.Fprintf(command.OutOrStdout(), "the previous keys stay in the JWKS for %s\n", grace)

			return nil
		},
	}

	command.Flags().StringSliceVar(&algs, "alg", nil, "algorithm to rotate (RS256 or ES256); default: every algorithm in use")
	command.Flags().DurationVar(&grace, "grace", DefaultKeyGrace, "how long the previous keys stay in the JWKS")

	return command
}
