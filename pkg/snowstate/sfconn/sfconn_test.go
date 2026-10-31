package sfconn

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	sf "github.com/snowflakedb/gosnowflake/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/youmark/pkcs8"
)

func keyPEM(t *testing.T, passphrase []byte) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	var der []byte
	if passphrase == nil {
		der, err = x509.MarshalPKCS8PrivateKey(key)
	} else {
		der, err = pkcs8.MarshalPrivateKey(key, passphrase, nil)
	}
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func TestDriverConfigByAuthMode(t *testing.T) {
	base := Config{Account: "a", User: "u", LockTimeout: 30 * time.Second, QueryTag: "snowstate"}

	key := base
	key.Auth, key.PrivateKeyPEM = AuthKeypair, keyPEM(t, nil)
	c, err := driverConfig(key)
	require.NoError(t, err)
	assert.Equal(t, sf.AuthTypeJwt, c.Authenticator)
	assert.NotNil(t, c.PrivateKey)
	assert.Equal(t, "30", *c.Params["LOCK_TIMEOUT"])
	assert.Equal(t, "snowstate", *c.Params["QUERY_TAG"])

	pat := base
	pat.Auth, pat.Token = AuthPAT, "t"
	c, err = driverConfig(pat)
	require.NoError(t, err)
	assert.Equal(t, sf.AuthTypePat, c.Authenticator)

	oauth := base
	oauth.Auth, oauth.TokenFilePath = AuthOAuth, "/tmp/t"
	c, err = driverConfig(oauth)
	require.NoError(t, err)
	assert.Equal(t, sf.AuthTypeOAuth, c.Authenticator)
	assert.Equal(t, "/tmp/t", c.TokenFilePath)

	browser := base
	browser.Auth = AuthExternalBrowser
	c, err = driverConfig(browser)
	require.NoError(t, err)
	assert.Equal(t, sf.AuthTypeExternalBrowser, c.Authenticator)

	password := base
	password.Auth, password.Password = AuthPassword, "p"
	c, err = driverConfig(password)
	require.NoError(t, err)
	assert.Equal(t, sf.AuthTypeSnowflake, c.Authenticator)

	keepAlive := browser
	keepAlive.ClientSessionKeepAlive = true
	c, err = driverConfig(keepAlive)
	require.NoError(t, err)
	assert.Equal(t, "true", *c.Params["CLIENT_SESSION_KEEP_ALIVE"])
}

func TestDriverConfigErrors(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no account":    {Auth: AuthPAT},
		"pat no token":  {Account: "a", Auth: AuthPAT},
		"oauth nothing": {Account: "a", Auth: AuthOAuth},
		"password none": {Account: "a", Auth: AuthPassword},
		"unknown auth":  {Account: "a", Auth: "magic"},
		"keypair none":  {Account: "a", Auth: AuthKeypair},
		"keypair bad":   {Account: "a", Auth: AuthKeypair, PrivateKeyPEM: "not pem"},
	} {
		_, err := driverConfig(cfg)
		require.Error(t, err, name)
	}
}

func TestEncryptedKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "k.p8")
	require.NoError(t, os.WriteFile(path, []byte(keyPEM(t, []byte("s3cret"))), 0o600))

	key, err := loadKey(Config{PrivateKeyPath: path, PrivateKeyPassphrase: "s3cret"})
	require.NoError(t, err)
	assert.NotNil(t, key)

	_, err = loadKey(Config{PrivateKeyPath: path, PrivateKeyPassphrase: "not-the-passphrase"})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "not-the-passphrase")

	_, err = loadKey(Config{PrivateKeyPath: filepath.Join(t.TempDir(), "missing")})
	require.Error(t, err)
}

func TestOpenDoesNotConnectAndSetsPool(t *testing.T) {
	db, err := Open(context.Background(), Config{Account: "a", Auth: AuthPAT, Token: "t",
		Pool: Pool{MaxOpen: 7, MaxIdle: 1, ConnMaxLifetime: time.Hour}})
	require.NoError(t, err)
	defer db.Close()
	assert.Equal(t, 7, db.Stats().MaxOpenConnections)
}
