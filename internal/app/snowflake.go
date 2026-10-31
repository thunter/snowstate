package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/thunter/snowstate/internal/config"
	"github.com/thunter/snowstate/pkg/snowstate/sfconn"
	"github.com/thunter/snowstate/pkg/snowstate/store"
	"github.com/thunter/snowstate/pkg/snowstate/store/snowflake"
)

// ConnConfig translates configuration into sfconn.Config, resolving secrets
// from the environment variables the config names.
func ConnConfig(cfg *config.Config) sfconn.Config {
	sc := &cfg.Snowflake
	return sfconn.Config{
		Account:              sc.Account,
		User:                 sc.User,
		Role:                 sc.Role,
		Warehouse:            sc.Warehouse,
		Database:             sc.Database,
		Schema:               sc.Schema,
		Auth:                 sc.Auth,
		PrivateKeyPath:       sc.PrivateKeyPath,
		PrivateKeyPEM:        os.Getenv(sc.PrivateKeyPEMEnv),
		PrivateKeyPassphrase: os.Getenv(sc.PrivateKeyPassphraseEnv),
		Token:                os.Getenv(sc.TokenEnv),
		TokenFilePath:        sc.TokenFilePath,
		Password:             os.Getenv(sc.PasswordEnv),
		WIF: sfconn.WIF{
			Provider:      sc.WIF.Provider,
			TokenSource:   sc.WIF.TokenSource,
			Audience:      sc.WIF.Audience,
			TokenFilePath: sc.WIF.TokenFilePath,
			TokenEnv:      sc.WIF.TokenEnv,
		},
		ClientSessionKeepAlive: sc.ClientSessionKeepAlive,
		QueryTag:               sc.QueryTag,
		LockTimeout:            sc.LockTimeout,
		Pool: sfconn.Pool{
			MaxOpen:         sc.Pool.MaxOpen,
			MaxIdle:         sc.Pool.MaxIdle,
			ConnMaxLifetime: sc.Pool.ConnMaxLifetime,
		},
	}
}

// OpenSnowflake opens the Snowflake store.
func OpenSnowflake(ctx context.Context, cfg *config.Config, obs store.Observer) (*snowflake.Store, error) {
	return openStore(ctx, cfg, ConnConfig(cfg), obs)
}

// OpenForProvisioning opens the store with no session schema, because Snowflake
// refuses a login that names a schema which does not exist yet, and creating
// that schema is what migrate is for. The store qualifies every name itself.
func OpenForProvisioning(ctx context.Context, cfg *config.Config) (*snowflake.Store, error) {
	conn := ConnConfig(cfg)
	conn.Schema = ""
	return openStore(ctx, cfg, conn, nil)
}

func openStore(ctx context.Context, cfg *config.Config, conn sfconn.Config, obs store.Observer) (*snowflake.Store, error) {
	db, err := sfconn.Open(ctx, conn)
	if err != nil {
		return nil, err
	}
	st := snowflake.New(snowflake.Options{
		DB:            db,
		Database:      cfg.Snowflake.Database,
		Schema:        cfg.Snowflake.Schema,
		ControlSchema: cfg.Tenancy.ControlSchema,
		Tenancy:       cfg.Tenancy.Mode,
		LockMode:      cfg.Locking.Mode,
		QueryTag:      cfg.Snowflake.QueryTag,
		Observer:      obs,
	})
	if err := st.Validate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("invalid snowflake names: %w", err)
	}
	return st, nil
}

func openSnowflake(ctx context.Context, cfg *config.Config, obs store.Observer, _ *slog.Logger) (store.Store, error) {
	// A typed-nil *snowflake.Store must not escape as a non-nil interface.
	st, err := OpenSnowflake(ctx, cfg, obs)
	if err != nil {
		return nil, err
	}
	return st, nil
}
