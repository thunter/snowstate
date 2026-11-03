// Command snowstate is a Terraform/OpenTofu http state backend on Snowflake.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/thunter/snowstate/internal/buildinfo"
)

func main() {
	err := newRootCmd().Execute()
	var exit exitError
	if errors.As(err, &exit) {
		os.Exit(exit.code)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "snowstate",
		Short:         "Terraform/OpenTofu http state backend that stores state in Snowflake",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().String("config", "", "config file (default ./snowstate.yaml if present)")
	root.AddCommand(newServeCmd(), newExecCmd(), newMigrateCmd(), newDoctorCmd(),
		newTokenCmd(), newStateCmd(), newLockCmd(), newProjectCmd(), newGCCmd(), newSeedCmd(), newInstallCmd())
	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintln(cmd.OutOrStdout(), buildinfo.String())
		},
	})
	return root
}
