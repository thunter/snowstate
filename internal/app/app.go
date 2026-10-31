// Package app wires configuration into a running store, service and server.
// `serve`, `exec` and the admin commands all build on it.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/thunter/snowstate/internal/api"
	"github.com/thunter/snowstate/internal/auth"
	"github.com/thunter/snowstate/internal/config"
	"github.com/thunter/snowstate/internal/obs"
	"github.com/thunter/snowstate/pkg/snowstate/service"
	"github.com/thunter/snowstate/pkg/snowstate/store"
	"github.com/thunter/snowstate/pkg/snowstate/store/memory"
)

// Test store controls (SPEC §12.2 scenario 10). The flag is hidden and refused
// unless SNOWSTATE_ALLOW_TEST_STORE=1.
const (
	AllowTestStoreEnv = "SNOWSTATE_ALLOW_TEST_STORE"
	TestStoreFileEnv  = "SNOWSTATE_TEST_STORE_FILE"
)

// StoreOptions selects a storage backend.
type StoreOptions struct {
	// TestStore is the value of the hidden --store flag.
	TestStore string
	Observer  store.Observer
}

// OpenStore opens the configured store.
func OpenStore(ctx context.Context, cfg *config.Config, opts StoreOptions, log *slog.Logger) (store.Store, error) {
	if opts.TestStore == "" {
		return openSnowflake(ctx, cfg, opts.Observer, log)
	}
	if opts.TestStore != "memory" {
		return nil, fmt.Errorf("unknown --store %q", opts.TestStore)
	}
	if os.Getenv(AllowTestStoreEnv) != "1" {
		return nil, fmt.Errorf("--store=memory is test-only and needs %s=1", AllowTestStoreEnv)
	}
	log.Warn("using the in-memory test store: state is not durable")
	return memory.New(memory.Options{SnapshotFile: os.Getenv(TestStoreFileEnv)})
}

// ServiceOptions builds service options from config.
func ServiceOptions(cfg *config.Config, st store.Store, authz service.Authorizer, log *slog.Logger) service.Options {
	return service.Options{
		Store:                  st,
		Authorizer:             authz,
		MaxStateBytes:          cfg.Limits.MaxStateBytes,
		InlineMaxBytes:         cfg.Storage.InlineMaxBytes,
		RequireLockForWrite:    cfg.State.RequireLockForWrite,
		RejectSerialRegression: cfg.State.RejectSerialRegression,
		AllowForceUnlock:       cfg.Locking.AllowForceUnlock,
		LockTTL:                cfg.Locking.TTL,
		LogReads:               cfg.Audit.LogReads,
		Logger:                 log,
		Retention: service.Retention{
			KeepVersions: cfg.Retention.KeepVersions,
			KeepDays:     cfg.Retention.KeepDays,
			DeletedDays:  cfg.Retention.DeletedDays,
		},
	}
}

// Authenticator builds the authenticator for auth.mode.
func Authenticator(cfg *config.Config, st store.Store, log *slog.Logger) (auth.Authenticator, error) {
	var a auth.Authenticator
	switch cfg.Auth.Mode {
	case "none":
		log.Warn("AUTHENTICATION IS DISABLED: auth.mode=none is for tests only")
		a = auth.None{}
	case "loopback-secret":
		s, err := auth.NewLoopbackSecret()
		if err != nil {
			return nil, err
		}
		a = s
	default:
		a = auth.NewTokenAuth([]byte(os.Getenv(cfg.Auth.PepperEnv)), st, log)
	}
	if cfg.Auth.MTLSPrincipalFromCN {
		a = &auth.MTLS{Tokens: st, Next: a}
	}
	return a, nil
}

// Authorizer picks the gateway or direct authorizer (SPEC §5.3).
func Authorizer(cfg *config.Config) service.Authorizer {
	if cfg.Auth.Mode == "token" {
		return auth.ScopeAuthorizer{}
	}
	return service.AllowAll{}
}

// Server is a running HTTP listener plus an optional metrics listener.
type Server struct {
	http     *http.Server
	metrics  *http.Server
	listener net.Listener
	timeout  time.Duration
	errs     chan error
	log      *slog.Logger
}

// Listen binds the listener so the address is known before serving.
func Listen(cfg *config.Config, handler http.Handler, m *obs.Metrics, log *slog.Logger) (*Server, error) {
	tlsCfg, err := serverTLS(cfg)
	if err != nil {
		return nil, err
	}
	lis, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", cfg.Server.Listen, err)
	}
	s := &Server{
		listener: lis,
		timeout:  cfg.Server.ShutdownTimeout,
		errs:     make(chan error, 2),
		log:      log,
		http: &http.Server{
			Handler:           handler,
			TLSConfig:         tlsCfg,
			ReadHeaderTimeout: 30 * time.Second,
			ReadTimeout:       cfg.Server.ReadTimeout,
			WriteTimeout:      cfg.Server.WriteTimeout,
			ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		},
	}
	if cfg.Metrics.Listen != "" {
		s.metrics = &http.Server{Addr: cfg.Metrics.Listen, Handler: m.Handler(), ReadHeaderTimeout: 30 * time.Second}
	}
	return s, nil
}

// Addr returns the bound address.
func (s *Server) Addr() net.Addr { return s.listener.Addr() }

// Start serves in the background.
func (s *Server) Start() {
	go func() {
		var err error
		if s.http.TLSConfig != nil {
			err = s.http.ServeTLS(s.listener, "", "")
		} else {
			err = s.http.Serve(s.listener)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.errs <- err
		}
	}()
	if s.metrics != nil {
		go func() {
			if err := s.metrics.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.errs <- err
			}
		}()
	}
}

// Errs reports fatal serving errors.
func (s *Server) Errs() <-chan error { return s.errs }

// Shutdown stops accepting connections and drains in-flight requests.
func (s *Server) Shutdown(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	err := s.http.Shutdown(ctx)
	if s.metrics != nil {
		err = errors.Join(err, s.metrics.Shutdown(ctx))
	}
	return err
}

// Handler builds the API handler.
func Handler(cfg *config.Config, svc service.StateService, a auth.Authenticator, m *obs.Metrics, log *slog.Logger) (http.Handler, error) {
	proxies, err := api.ParseCIDRs(cfg.Server.TrustedProxies)
	if err != nil {
		return nil, err
	}
	return api.New(api.Options{
		Service:        svc,
		Auth:           a,
		Metrics:        m,
		Logger:         log,
		MaxStateBytes:  cfg.Limits.MaxStateBytes,
		RequestTimeout: cfg.Server.RequestTimeout,
		TrustedProxies: proxies,
		Limiter:        api.NewRateLimiter(cfg.Auth.FailedPerMinute),
		ServeMetrics:   cfg.Metrics.Listen == "",
	}), nil
}
