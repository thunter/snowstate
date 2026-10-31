// Package sfconn opens Snowflake connections. It has no dependency on the CLI
// or config libraries, so the M7 provider can fill Config from HCL attributes.
package sfconn

import (
	"context"
	"crypto/rsa"
	"database/sql"
	"database/sql/driver"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	sf "github.com/snowflakedb/gosnowflake"
	"github.com/youmark/pkcs8"
)

// Auth modes.
const (
	AuthKeypair          = "keypair"
	AuthPAT              = "pat"
	AuthOAuth            = "oauth"
	AuthExternalBrowser  = "externalbrowser"
	AuthWorkloadIdentity = "workload_identity"
	AuthPassword         = "password"
)

// DefaultWIFAudience is the OIDC audience Snowflake expects.
const DefaultWIFAudience = "snowflakecomputing.com"

// WIF configures workload identity federation.
type WIF struct {
	Provider      string // oidc | aws | azure | gcp
	TokenSource   string // github | file | env
	Audience      string
	TokenFilePath string
	TokenEnv      string
}

// Pool configures database/sql.
type Pool struct {
	MaxOpen         int
	MaxIdle         int
	ConnMaxLifetime time.Duration
}

// Config is a plain description of a connection. Secrets are held as values.
type Config struct {
	Account, User, Role, Warehouse, Database, Schema string

	Auth                 string
	PrivateKeyPath       string
	PrivateKeyPEM        string
	PrivateKeyPassphrase string
	Token                string
	TokenFilePath        string
	Password             string
	WIF                  WIF
	// OIDCToken fetches a token for a new connection. When nil, one is built
	// from WIF. Tests can replace it.
	OIDCToken func(ctx context.Context) (string, error)

	ClientSessionKeepAlive bool
	QueryTag               string
	LockTimeout            time.Duration
	Pool                   Pool
}

// Open returns a pool. It does not connect; call PingContext for that.
func Open(_ context.Context, cfg Config) (*sql.DB, error) {
	base, err := driverConfig(cfg)
	if err != nil {
		return nil, err
	}
	// Driver logs could otherwise carry SQL text. Nothing here needs them.
	if err := sf.GetLogger().SetLogLevel("fatal"); err != nil {
		return nil, err
	}
	var conn driver.Connector = connector{base: base, cfg: cfg}
	db := sql.OpenDB(conn)
	if cfg.Pool.MaxOpen > 0 {
		db.SetMaxOpenConns(cfg.Pool.MaxOpen)
	}
	if cfg.Pool.MaxIdle > 0 {
		db.SetMaxIdleConns(cfg.Pool.MaxIdle)
	}
	if cfg.Pool.ConnMaxLifetime > 0 {
		db.SetConnMaxLifetime(cfg.Pool.ConnMaxLifetime)
	}
	return db, nil
}

// connector builds a driver connector per connection, so a workload identity
// token is fetched fresh each time (SPEC §5.2).
type connector struct {
	base sf.Config
	cfg  Config
}

func (c connector) Connect(ctx context.Context) (driver.Conn, error) {
	conf := c.base
	if c.cfg.Auth == AuthWorkloadIdentity && strings.EqualFold(c.cfg.WIF.Provider, "oidc") {
		fetch := c.cfg.OIDCToken
		if fetch == nil {
			fetch = oidcTokenFetcher(c.cfg.WIF)
		}
		token, err := fetch(ctx)
		if err != nil {
			return nil, err
		}
		conf.Token = token
	}
	return sf.NewConnector(sf.SnowflakeDriver{}, conf).Connect(ctx)
}

func (c connector) Driver() driver.Driver { return sf.SnowflakeDriver{} }

func driverConfig(cfg Config) (sf.Config, error) {
	if cfg.Account == "" {
		return sf.Config{}, errors.New("snowflake: account is required")
	}
	lockTimeout := strconv.Itoa(int(cfg.LockTimeout.Seconds()))
	queryTag := cfg.QueryTag
	out := sf.Config{
		Account:     cfg.Account,
		User:        cfg.User,
		Role:        cfg.Role,
		Warehouse:   cfg.Warehouse,
		Database:    cfg.Database,
		Schema:      cfg.Schema,
		Application: "snowstate",
		Params: map[string]*string{
			"LOCK_TIMEOUT": &lockTimeout,
			"QUERY_TAG":    &queryTag,
		},
	}
	if cfg.ClientSessionKeepAlive {
		keepAlive := "true"
		out.Params["CLIENT_SESSION_KEEP_ALIVE"] = &keepAlive
	}
	switch cfg.Auth {
	case AuthKeypair, "":
		key, err := loadKey(cfg)
		if err != nil {
			return sf.Config{}, err
		}
		out.Authenticator = sf.AuthTypeJwt
		out.PrivateKey = key
	case AuthPAT:
		if cfg.Token == "" {
			return sf.Config{}, errors.New("snowflake: auth=pat needs a token")
		}
		out.Authenticator = sf.AuthTypePat
		out.Token = cfg.Token
	case AuthOAuth:
		if cfg.Token == "" && cfg.TokenFilePath == "" {
			return sf.Config{}, errors.New("snowflake: auth=oauth needs a token or token_file_path")
		}
		out.Authenticator = sf.AuthTypeOAuth
		out.Token, out.TokenFilePath = cfg.Token, cfg.TokenFilePath
	case AuthExternalBrowser:
		out.Authenticator = sf.AuthTypeExternalBrowser
	case AuthPassword:
		if cfg.User == "" || cfg.Password == "" {
			return sf.Config{}, errors.New("snowflake: auth=password needs a user and password")
		}
		out.Authenticator = sf.AuthTypeSnowflake
		out.Password = cfg.Password
	case AuthWorkloadIdentity:
		out.Authenticator = sf.AuthTypeWorkloadIdentityFederation
		out.WorkloadIdentityProvider = strings.ToUpper(cfg.WIF.Provider)
	default:
		return sf.Config{}, fmt.Errorf("snowflake: unknown auth mode %q", cfg.Auth)
	}
	return out, nil
}

func loadKey(cfg Config) (*rsa.PrivateKey, error) {
	raw := []byte(cfg.PrivateKeyPEM)
	if len(raw) == 0 {
		if cfg.PrivateKeyPath == "" {
			return nil, errors.New("snowflake: auth=keypair needs private_key_path or a PEM key")
		}
		var err error
		raw, err = os.ReadFile(cfg.PrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("snowflake: reading private key: %w", err)
		}
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("snowflake: private key is not PEM")
	}
	var passphrase []byte
	if cfg.PrivateKeyPassphrase != "" {
		passphrase = []byte(cfg.PrivateKeyPassphrase)
	}
	key, err := pkcs8.ParsePKCS8PrivateKeyRSA(block.Bytes, passphrase)
	if err != nil {
		// The error is deliberately not wrapped: it adds nothing and this
		// code path handles secret material.
		return nil, errors.New("snowflake: cannot parse private key (wrong passphrase, or not PKCS#8 RSA?)")
	}
	return key, nil
}

// oidcTokenFetcher is replaced for GitHub Actions in M6.5.
func oidcTokenFetcher(wif WIF) func(context.Context) (string, error) {
	return func(context.Context) (string, error) {
		return "", fmt.Errorf("snowflake: unsupported wif.token_source %q", wif.TokenSource)
	}
}
