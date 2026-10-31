package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/thunter/snowstate/internal/config"
	"github.com/thunter/snowstate/internal/obs"
	"github.com/thunter/snowstate/internal/sidecar"
)

// exitError carries the child's exit code through cobra to main.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func newExecCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "exec [flags] -- <command> [args]",
		Short: "Run a command with an ephemeral loopback state backend",
		Long: `exec starts a loopback server, points the http backend at it through
TF_HTTP_* variables, runs the command and shuts down. The root module must
declare an empty backend block: terraform { backend "http" {} }`,
		Args: cobra.MinimumNArgs(1),
		RunE: runExec,
	}
	cmd.Flags().SetInterspersed(false)
	cmd.Flags().String("key", "", "state key (or SNOWSTATE_KEY)")
	cmd.Flags().Bool("release-stale-lock", false, "release a lock the command left behind")
	cmd.Flags().Duration("shutdown-grace", sidecar.DefaultShutdownGrace, "how long the server outlives a second signal")
	addTestStoreFlag(cmd)
	return cmd
}

func runExec(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig(cmd, nil, config.ProfileExec)
	if err != nil {
		return err
	}
	// Terraform owns this terminal, so only warnings are shown by default.
	if cfg.Log.Level == config.Defaults().Log.Level {
		cfg.Log.Level = "warn"
	}
	log := obs.NewLogger(os.Stderr, cfg.Log.Level, "text")

	key, _ := cmd.Flags().GetString("key")
	release, _ := cmd.Flags().GetBool("release-stale-lock")
	grace, _ := cmd.Flags().GetDuration("shutdown-grace")
	testStore, _ := cmd.Flags().GetString("store")

	code, err := sidecar.Run(cmd.Context(), sidecar.Options{
		Config: cfg, Key: key, Args: args, ReleaseStaleLock: release, ShutdownGrace: grace,
		TestStore: testStore, Logger: log,
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return exitError{code: code}
	}
	return nil
}
