package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultsMatchSpec(t *testing.T) {
	c := Defaults()
	assert.Equal(t, 60*time.Second, c.Server.RequestTimeout)
	assert.Equal(t, int64(8388608), c.Storage.InlineMaxBytes)
	assert.Equal(t, int64(268435456), c.Limits.MaxStateBytes)
	assert.Equal(t, 100, c.Retention.KeepVersions)
	assert.Equal(t, 30*time.Second, c.Snowflake.LockTimeout)
	assert.True(t, c.Locking.AllowForceUnlock)
	assert.True(t, c.Audit.LogReads)
	// A default Install's runtime role and warehouse (D60).
	assert.Equal(t, "TF_STATE_SVC", c.Snowflake.Role)
	assert.Equal(t, "TF_STATE_WH", c.Snowflake.Warehouse)
	assert.Equal(t, "TF_STATE", c.Snowflake.Database)
}

func TestPrecedenceFlagsOverEnvOverFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	body := "server:\n  listen: \"127.0.0.1:1\"\n  request_timeout: 5s\nsnowflake:\n  account: fromfile\n  wif:\n    token_source: file\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	t.Setenv("SNOWSTATE_SNOWFLAKE__ACCOUNT", "fromenv")
	t.Setenv("SNOWSTATE_SNOWFLAKE__WIF__TOKEN_SOURCE", "env")
	t.Setenv("SNOWSTATE_SERVER__LISTEN", "127.0.0.1:2")

	c, err := Load(path, map[string]any{"server.listen": "127.0.0.1:3"})
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:3", c.Server.Listen)
	assert.Equal(t, "fromenv", c.Snowflake.Account)
	assert.Equal(t, "env", c.Snowflake.WIF.TokenSource)
	assert.Equal(t, 5*time.Second, c.Server.RequestTimeout)
	assert.Equal(t, 10, c.Snowflake.Pool.MaxOpen, "untouched defaults survive")
}

func TestNonConfigEnvIgnored(t *testing.T) {
	t.Setenv("SNOWSTATE_KEY", "platform/net/prod")
	t.Setenv("SNOWSTATE_TOKEN_PEPPER", "x")
	_, err := Load("", nil)
	require.NoError(t, err)
}

func TestExplicitMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"), nil)
	require.Error(t, err)
}

func TestBadYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	require.NoError(t, os.WriteFile(path, []byte("server: [unclosed"), 0o600))
	_, err := Load(path, nil)
	require.Error(t, err)
}

func TestValidateAggregatesErrors(t *testing.T) {
	c := Defaults()
	c.Log.Level = "loud"
	c.Tenancy.Mode = "weird"
	c.Limits.MaxStateBytes = 0
	err := c.Validate(ProfileAdmin)
	require.Error(t, err)
	for _, want := range []string{"log.level", "tenancy.mode", "limits.max_state_bytes"} {
		assert.Contains(t, err.Error(), want)
	}
}

func TestStandardModeNeedsAcknowledgement(t *testing.T) {
	c := Defaults()
	c.Locking.Mode = "standard"
	require.ErrorContains(t, c.Validate(ProfileAdmin), "standard_mode_acknowledged")
	c.Locking.StandardModeAcknowledged = true
	require.NoError(t, c.Validate(ProfileAdmin))
}

func TestServeRules(t *testing.T) {
	pepper := strings.Repeat("p", 32)
	tests := []struct {
		name   string
		mutate func(*Config)
		pepper string
		want   string
	}{
		{"tls required off loopback", func(c *Config) {}, pepper, "TLS is required"},
		{"pepper missing", func(c *Config) { c.Server.Listen = "127.0.0.1:0" }, "", "SNOWSTATE_TOKEN_PEPPER"},
		{"pepper short", func(c *Config) { c.Server.Listen = "127.0.0.1:0" }, "short", "at least 32"},
		{"none off loopback", func(c *Config) { c.Auth.Mode = "none"; c.TLS.DisableForTesting = true }, pepper, "loopback listener"},
		{"loopback-secret off loopback", func(c *Config) { c.Auth.Mode = "loopback-secret"; c.TLS.DisableForTesting = true }, pepper, "loopback listener"},
		{"cert without key", func(c *Config) { c.TLS.CertFile = "a" }, pepper, "set together"},
		{"mtls without tls", func(c *Config) { c.Server.Listen = "127.0.0.1:0"; c.TLS.ClientCAFile = "ca" }, pepper, "needs tls.cert_file"},
		{"cn without ca", func(c *Config) { c.Server.Listen = "127.0.0.1:0"; c.Auth.MTLSPrincipalFromCN = true }, pepper, "needs tls.client_ca_file"},
		{"bad cidr", func(c *Config) { c.Server.Listen = "127.0.0.1:0"; c.Server.TrustedProxies = []string{"nope"} }, pepper, "trusted_proxies"},
		{"bad timeout", func(c *Config) { c.Server.Listen = "127.0.0.1:0"; c.Server.RequestTimeout = 0 }, pepper, "request_timeout"},
		{"bad auth mode", func(c *Config) { c.Server.Listen = "127.0.0.1:0"; c.Auth.Mode = "magic" }, pepper, "auth.mode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SNOWSTATE_TOKEN_PEPPER", tt.pepper)
			c := Defaults()
			tt.mutate(&c)
			require.ErrorContains(t, c.Validate(ProfileServe), tt.want)
		})
	}
}

func TestServeLoopbackIsValid(t *testing.T) {
	c := Defaults()
	c.Server.Listen = "127.0.0.1:0"
	c.Auth.Mode = "none"
	require.NoError(t, c.Validate(ProfileServe))
}

func TestIsLoopback(t *testing.T) {
	assert.True(t, IsLoopback("127.0.0.1:80"))
	assert.True(t, IsLoopback("[::1]:80"))
	assert.True(t, IsLoopback("localhost:80"))
	assert.False(t, IsLoopback(":80"))
	assert.False(t, IsLoopback("0.0.0.0:80"))
	assert.False(t, IsLoopback("10.0.0.1:80"))
	assert.False(t, IsLoopback("garbage"))
}
