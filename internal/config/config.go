// Package config loads and validates snowstate configuration (SPEC §9).
//
// Precedence is flags > environment > YAML file > defaults. Environment
// variables use the SNOWSTATE_ prefix with "__" for nesting, for example
// SNOWSTATE_SNOWFLAKE__ACCOUNT. Variables without "__" (SNOWSTATE_KEY,
// SNOWSTATE_TOKEN_PEPPER, SNOWSTATE_IT_*) are not configuration keys.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// DefaultConfigFile is used when --config is not given and the file exists.
const DefaultConfigFile = "snowstate.yaml"

// Config is the full configuration tree.
type Config struct {
	Server    Server    `koanf:"server"`
	TLS       TLS       `koanf:"tls"`
	Auth      Auth      `koanf:"auth"`
	Snowflake Snowflake `koanf:"snowflake"`
	Tenancy   Tenancy   `koanf:"tenancy"`
	Locking   Locking   `koanf:"locking"`
	State     State     `koanf:"state"`
	Storage   Storage   `koanf:"storage"`
	Limits    Limits    `koanf:"limits"`
	Retention Retention `koanf:"retention"`
	Audit     Audit     `koanf:"audit"`
	Metrics   Metrics   `koanf:"metrics"`
	Log       Log       `koanf:"log"`
}

// Server configures the HTTP listener.
type Server struct {
	Listen          string        `koanf:"listen"`
	RequestTimeout  time.Duration `koanf:"request_timeout"`
	ReadTimeout     time.Duration `koanf:"read_timeout"`
	WriteTimeout    time.Duration `koanf:"write_timeout"`
	ShutdownTimeout time.Duration `koanf:"shutdown_timeout"`
	TrustedProxies  []string      `koanf:"trusted_proxies"`
}

// TLS configures server TLS and optional mTLS.
type TLS struct {
	CertFile          string `koanf:"cert_file"`
	KeyFile           string `koanf:"key_file"`
	ClientCAFile      string `koanf:"client_ca_file"`
	DisableForTesting bool   `koanf:"disable_for_testing"`
}

// Auth configures client authentication.
type Auth struct {
	Mode                string `koanf:"mode"`
	PepperEnv           string `koanf:"pepper_env"`
	MTLSPrincipalFromCN bool   `koanf:"mtls_principal_from_cn"`
	FailedPerMinute     int    `koanf:"failed_per_minute"`
}

// WIF configures workload identity federation.
type WIF struct {
	Provider      string `koanf:"provider"`
	TokenSource   string `koanf:"token_source"`
	Audience      string `koanf:"audience"`
	TokenFilePath string `koanf:"token_file_path"`
	TokenEnv      string `koanf:"token_env"`
}

// Pool configures the database/sql pool.
type Pool struct {
	MaxOpen         int           `koanf:"max_open"`
	MaxIdle         int           `koanf:"max_idle"`
	ConnMaxLifetime time.Duration `koanf:"conn_max_lifetime"`
}

// Snowflake configures the connection to Snowflake.
type Snowflake struct {
	Account                 string        `koanf:"account"`
	User                    string        `koanf:"user"`
	Role                    string        `koanf:"role"`
	Warehouse               string        `koanf:"warehouse"`
	Database                string        `koanf:"database"`
	Schema                  string        `koanf:"schema"`
	Auth                    string        `koanf:"auth"`
	WIF                     WIF           `koanf:"wif"`
	ClientSessionKeepAlive  bool          `koanf:"client_session_keep_alive"`
	PrivateKeyPath          string        `koanf:"private_key_path"`
	PrivateKeyPEMEnv        string        `koanf:"private_key_pem_env"`
	PrivateKeyPassphraseEnv string        `koanf:"private_key_passphrase_env"`
	TokenEnv                string        `koanf:"token_env"`
	TokenFilePath           string        `koanf:"token_file_path"`
	PasswordEnv             string        `koanf:"password_env"`
	QueryTag                string        `koanf:"query_tag"`
	LockTimeout             time.Duration `koanf:"lock_timeout"`
	Pool                    Pool          `koanf:"pool"`
}

// Tenancy configures how projects map to schemas (SPEC §5.3).
type Tenancy struct {
	Mode          string `koanf:"mode"`
	ControlSchema string `koanf:"control_schema"`
}

// Locking configures the lock algorithm (SPEC §6.5).
type Locking struct {
	Mode                     string        `koanf:"mode"`
	StandardModeAcknowledged bool          `koanf:"standard_mode_acknowledged"`
	AllowForceUnlock         bool          `koanf:"allow_force_unlock"`
	TTL                      time.Duration `koanf:"ttl"`
}

// State configures write rules.
type State struct {
	RequireLockForWrite    bool `koanf:"require_lock_for_write"`
	RejectSerialRegression bool `koanf:"reject_serial_regression"`
}

// Storage configures blob placement.
type Storage struct {
	InlineMaxBytes int64 `koanf:"inline_max_bytes"`
}

// Limits configures request limits.
type Limits struct {
	MaxStateBytes int64 `koanf:"max_state_bytes"`
}

// Retention configures GC.
type Retention struct {
	KeepVersions int `koanf:"keep_versions"`
	KeepDays     int `koanf:"keep_days"`
	DeletedDays  int `koanf:"deleted_days"`
}

// Audit configures the audit trail.
type Audit struct {
	LogReads bool `koanf:"log_reads"`
}

// Metrics configures the Prometheus endpoint.
type Metrics struct {
	Listen string `koanf:"listen"`
}

// Log configures logging.
type Log struct {
	Level  string `koanf:"level"`
	Format string `koanf:"format"`
}

// Defaults returns the configuration from SPEC §9.
func Defaults() Config {
	return Config{
		Server: Server{
			Listen:          ":8443",
			RequestTimeout:  60 * time.Second,
			ReadTimeout:     5 * time.Minute,
			WriteTimeout:    5 * time.Minute,
			ShutdownTimeout: 30 * time.Second,
		},
		Auth: Auth{Mode: "token", PepperEnv: "SNOWSTATE_TOKEN_PEPPER", FailedPerMinute: 10},
		Snowflake: Snowflake{ //nolint:gosec // env var names, not credentials
			Role:                   "SNOWSTATE_SVC",
			Warehouse:              "SNOWSTATE_WH",
			Database:               "TF_STATE",
			Schema:                 "BACKEND",
			Auth:                   "keypair",
			ClientSessionKeepAlive: true,
			WIF: WIF{
				Provider:    "oidc",
				TokenSource: "github",
				Audience:    "snowflakecomputing.com",
			},
			PrivateKeyPEMEnv: "SNOWSTATE_SNOWFLAKE_PRIVATE_KEY_PEM",
			TokenEnv:         "SNOWSTATE_SNOWFLAKE_TOKEN",
			PasswordEnv:      "SNOWSTATE_SNOWFLAKE_PASSWORD",
			QueryTag:         "snowstate",
			LockTimeout:      30 * time.Second,
			Pool:             Pool{MaxOpen: 10, MaxIdle: 2, ConnMaxLifetime: time.Hour},
		},
		Tenancy:   Tenancy{Mode: "shared", ControlSchema: "CONTROL"},
		Locking:   Locking{Mode: "hybrid", AllowForceUnlock: true},
		Storage:   Storage{InlineMaxBytes: 8 << 20},
		Limits:    Limits{MaxStateBytes: 256 << 20},
		Retention: Retention{KeepVersions: 100, DeletedDays: 30},
		Audit:     Audit{LogReads: true},
		Log:       Log{Level: "info", Format: "json"},
	}
}

// Load reads configuration. path may be empty, in which case DefaultConfigFile
// is used when present. overrides holds dotted keys set from flags.
func Load(path string, overrides map[string]any) (*Config, error) {
	k := koanf.New(".")
	cfg := Defaults()

	explicit := path != ""
	if !explicit {
		path = DefaultConfigFile
	}
	if _, err := os.Stat(path); err == nil {
		if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
			return nil, fmt.Errorf("config %s: %w", path, err)
		}
	} else if explicit {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}

	envProvider := env.Provider(".", env.Opt{
		Prefix: "SNOWSTATE_",
		TransformFunc: func(key, value string) (string, any) {
			name := strings.TrimPrefix(key, "SNOWSTATE_")
			if !strings.Contains(name, "__") {
				return "", nil // not a config key
			}
			return strings.ToLower(strings.ReplaceAll(name, "__", ".")), value
		},
	})
	if err := k.Load(envProvider, nil); err != nil {
		return nil, fmt.Errorf("environment: %w", err)
	}

	for key, value := range overrides {
		if err := k.Set(key, value); err != nil {
			return nil, fmt.Errorf("flag %s: %w", key, err)
		}
	}

	if err := k.UnmarshalWithConf("", &cfg, koanf.UnmarshalConf{Tag: "koanf"}); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &cfg, nil
}

// Profile selects which rules Validate applies.
type Profile int

// Profiles for the different commands.
const (
	// ProfileServe validates `snowstate serve`.
	ProfileServe Profile = iota
	// ProfileExec validates `snowstate exec`.
	ProfileExec
	// ProfileAdmin validates commands that talk to Snowflake directly.
	ProfileAdmin
)

// Validate returns every problem found, joined into one error.
func (c *Config) Validate(p Profile) error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if !oneOf(c.Log.Level, "debug", "info", "warn", "error") {
		add("log.level: unknown level %q", c.Log.Level)
	}
	if !oneOf(c.Log.Format, "json", "text") {
		add("log.format: unknown format %q", c.Log.Format)
	}
	if !oneOf(c.Tenancy.Mode, "shared", "schema_per_project") {
		add("tenancy.mode: unknown mode %q", c.Tenancy.Mode)
	}
	if !oneOf(c.Locking.Mode, "hybrid", "standard") {
		add("locking.mode: unknown mode %q", c.Locking.Mode)
	}
	if c.Locking.Mode == "standard" && !c.Locking.StandardModeAcknowledged {
		add("locking.mode=standard is experimental: set locking.standard_mode_acknowledged=true to accept the risk")
	}
	if c.Locking.TTL < 0 {
		add("locking.ttl: must not be negative")
	}
	if c.Limits.MaxStateBytes <= 0 {
		add("limits.max_state_bytes: must be positive")
	}
	if c.Storage.InlineMaxBytes < 0 {
		add("storage.inline_max_bytes: must not be negative")
	}
	if c.Retention.KeepVersions < 0 || c.Retention.KeepDays < 0 || c.Retention.DeletedDays < 0 {
		add("retention: values must not be negative")
	}

	if p == ProfileServe {
		c.validateServer(add)
	}
	return errors.Join(errs...)
}

func (c *Config) validateServer(add func(string, ...any)) {
	if c.Server.RequestTimeout <= 0 {
		add("server.request_timeout: must be positive")
	}
	if !oneOf(c.Auth.Mode, "token", "loopback-secret", "none") {
		add("auth.mode: unknown mode %q", c.Auth.Mode)
	}
	loopback := IsLoopback(c.Server.Listen)
	if (c.Auth.Mode == "loopback-secret" || c.Auth.Mode == "none") && !loopback {
		add("auth.mode=%s is only allowed on a loopback listener, not %q", c.Auth.Mode, c.Server.Listen)
	}
	if c.Auth.Mode == "token" && os.Getenv(c.Auth.PepperEnv) == "" {
		add("auth.pepper_env: environment variable %s must hold a 32+ byte secret", c.Auth.PepperEnv)
	} else if c.Auth.Mode == "token" && len(os.Getenv(c.Auth.PepperEnv)) < 32 {
		add("auth.pepper_env: %s must be at least 32 bytes", c.Auth.PepperEnv)
	}
	hasTLS := c.TLS.CertFile != "" || c.TLS.KeyFile != ""
	if (c.TLS.CertFile == "") != (c.TLS.KeyFile == "") {
		add("tls: cert_file and key_file must be set together")
	}
	if !hasTLS && !loopback && !c.TLS.DisableForTesting {
		add("tls.cert_file: TLS is required on non-loopback listener %q", c.Server.Listen)
	}
	if c.TLS.ClientCAFile != "" && !hasTLS {
		add("tls.client_ca_file: mTLS needs tls.cert_file and tls.key_file")
	}
	if c.Auth.MTLSPrincipalFromCN && c.TLS.ClientCAFile == "" {
		add("auth.mtls_principal_from_cn: needs tls.client_ca_file")
	}
	for _, cidr := range c.Server.TrustedProxies {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			add("server.trusted_proxies: %v", err)
		}
	}
}

// IsLoopback reports whether a listen address is bound only to loopback.
// An empty host (":8443") binds every interface, so it is not loopback.
func IsLoopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func oneOf(v string, allowed ...string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}
