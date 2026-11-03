package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/thunter/snowstate/internal/install"
)

func newSeedCmd() *cobra.Command {
	var keyFile string
	cmd := &cobra.Command{
		Use:   "seed",
		Short: "Print the Seed SQL for an admin to run",
		Long: `seed prints the SQL that creates the SNOWSTATE_SEED role and user, with the
public key filled in. An admin runs it once per account, as ACCOUNTADMIN.
It never connects to Snowflake.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pemBytes, err := os.ReadFile(keyFile) //nolint:gosec // the operator names their own key file
			if err != nil {
				return err
			}
			sql, err := install.SeedSQL(pemBytes)
			if err != nil {
				return fmt.Errorf("%s: %w", keyFile, err)
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), sql)
			return err
		},
	}
	cmd.Flags().StringVar(&keyFile, "public-key-file", "", "PEM public key for the SNOWSTATE_SEED user")
	_ = cmd.MarkFlagRequired("public-key-file")
	return cmd
}
