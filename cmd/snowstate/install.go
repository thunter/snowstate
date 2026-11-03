package main

import (
	"github.com/spf13/cobra"

	"github.com/thunter/snowstate/internal/config"
	"github.com/thunter/snowstate/internal/install"
	"github.com/thunter/snowstate/internal/obs"
)

func newInstallCmd() *cobra.Command {
	var opts install.Options
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Create an Install: the Foundation, the schema, and its state in snowstate",
		Long: `install writes the Foundation's Terraform code to a directory you keep, shows
the plan for approval, and applies it. It then runs migrate as the admin role
and doctor as the runtime role, and only then moves the Foundation's own state
into snowstate. It runs the real Terraform or OpenTofu, and every step can also
be run by hand (see deploy/terraform/foundation/README.md).

It connects as SNOWSTATE_SEED with the configured key, which the Seed creates.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(cmd, nil, config.ProfileAdmin)
			if err != nil {
				return err
			}
			// Terraform owns this terminal, so only warnings are shown by default.
			if cfg.Log.Level == config.Defaults().Log.Level {
				cfg.Log.Level = "warn"
			}
			opts.Config, opts.Stdin, opts.Stdout = cfg, cmd.InOrStdin(), cmd.OutOrStdout()
			runner := &install.Exec{
				Binary: opts.Binary, Stdin: cmd.InOrStdin(), Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr(),
				Logger: obs.NewLogger(cmd.ErrOrStderr(), cfg.Log.Level, "text"),
			}
			return install.Run(cmd.Context(), opts, runner)
		},
	}
	cmd.Flags().StringVar(&opts.Database, "database", "", "database for the Install; account-level names derive from it")
	cmd.Flags().StringVar(&opts.Dir, "dir", "snowstate-foundation", "directory for the Foundation's Terraform code")
	cmd.Flags().StringVar(&opts.Binary, "binary", "terraform", "Terraform or OpenTofu binary")
	cmd.Flags().BoolVar(&opts.AutoApprove, "auto-approve", false, "apply the plan without asking")
	_ = cmd.MarkFlagRequired("database")
	return cmd
}
