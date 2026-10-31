package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/thunter/snowstate/internal/app"
	"github.com/thunter/snowstate/internal/config"
	"github.com/thunter/snowstate/pkg/snowstate/store/snowflake"
)

// withStore loads config and runs fn with the Snowflake store. Admin commands
// talk to Snowflake directly, not through the HTTP API (SPEC §8).
func withStore(cmd *cobra.Command, fn func(ctx context.Context, cfg *config.Config, st *snowflake.Store) error) error {
	cfg, err := loadConfig(cmd, nil, config.ProfileAdmin)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	st, err := app.OpenSnowflake(ctx, cfg, nil)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	return fn(ctx, cfg, st)
}

func newMigrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Create or upgrade the schema",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			return withStore(cmd, func(ctx context.Context, _ *config.Config, st *snowflake.Store) error {
				res, err := st.Migrate(ctx, dryRun)
				if err != nil {
					return err
				}
				verb := "applied"
				if dryRun {
					verb = "would apply"
				}
				if len(res.Applied) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "schema is up to date")
				}
				for _, m := range res.Applied {
					fmt.Fprintln(cmd.OutOrStdout(), verb, m)
				}
				return nil
			})
		},
	}
	cmd.Flags().Bool("dry-run", false, "list migrations without running them")
	return cmd
}

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check connectivity, grants, hybrid support and a round trip",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(cmd, nil, config.ProfileAdmin)
			if err != nil {
				return err
			}
			if !app.Doctor(cmd.Context(), cfg, cmd.OutOrStdout()) {
				return errors.New("doctor found problems")
			}
			return nil
		},
	}
}
