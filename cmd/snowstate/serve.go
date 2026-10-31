package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/thunter/snowstate/internal/app"
	"github.com/thunter/snowstate/internal/config"
	"github.com/thunter/snowstate/internal/obs"
	"github.com/thunter/snowstate/pkg/snowstate/service"
)

// loadConfig reads config with the global flag and validates it for a profile.
func loadConfig(cmd *cobra.Command, overrides map[string]any, p config.Profile) (*config.Config, error) {
	path, err := cmd.Flags().GetString("config")
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(path, overrides)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(p); err != nil {
		return nil, fmt.Errorf("invalid configuration:\n%w", err)
	}
	return cfg, nil
}

func newServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the shared HTTP state backend",
		Args:  cobra.NoArgs,
		RunE:  runServe,
	}
	cmd.Flags().String("listen", "", "listen address (overrides server.listen)")
	cmd.Flags().String("auth-mode", "", "token | loopback-secret | none (overrides auth.mode)")
	cmd.Flags().String("log-level", "", "debug | info | warn | error (overrides log.level)")
	addTestStoreFlag(cmd)
	return cmd
}

func addTestStoreFlag(cmd *cobra.Command) {
	cmd.Flags().String("store", "", "test-only storage backend")
	_ = cmd.Flags().MarkHidden("store")
}

func flagOverrides(cmd *cobra.Command, mapping map[string]string) map[string]any {
	out := map[string]any{}
	for flag, key := range mapping {
		if f := cmd.Flags().Lookup(flag); f != nil && f.Changed {
			out[key] = f.Value.String()
		}
	}
	return out
}

func runServe(cmd *cobra.Command, _ []string) error {
	cfg, err := loadConfig(cmd, flagOverrides(cmd, map[string]string{
		"listen": "server.listen", "auth-mode": "auth.mode", "log-level": "log.level",
	}), config.ProfileServe)
	if err != nil {
		return err
	}
	log := obs.NewLogger(os.Stdout, cfg.Log.Level, cfg.Log.Format)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var metrics *obs.Metrics
	var svc *service.Service
	metrics = obs.NewMetrics(func() int {
		if svc == nil {
			return 0
		}
		return svc.AuditDepth()
	})
	testStore, _ := cmd.Flags().GetString("store")
	st, err := app.OpenStore(ctx, cfg, app.StoreOptions{TestStore: testStore, Observer: metrics}, log)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	opts := app.ServiceOptions(cfg, st, app.Authorizer(cfg), log)
	opts.Hooks = service.Hooks{
		LockConflict: metrics.LockConflicts.Inc,
		StateBytes:   func(n int) { metrics.StateBytes.Observe(float64(n)) },
	}
	svc = service.New(opts)
	defer svc.Close()

	a, err := app.Authenticator(cfg, st, log)
	if err != nil {
		return err
	}
	handler, err := app.Handler(cfg, svc, a, metrics, log)
	if err != nil {
		return err
	}
	srv, err := app.Listen(cfg, handler, metrics, log)
	if err != nil {
		return err
	}
	srv.Start()
	log.Info("snowstate listening", "addr", srv.Addr().String())

	select {
	case err := <-srv.Errs():
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	stop()
	return srv.Shutdown(context.Background())
}
