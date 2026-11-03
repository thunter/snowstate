package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A throwaway public key, and its body as Snowflake's RSA_PUBLIC_KEY expects it.
const (
	testPublicKeyPEM = `-----BEGIN PUBLIC KEY-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA6ooAAQaPkAcFAx80Kx9e
SD41D1Rotw776vukPcsr6QsaMMphkaXPXH09jjoXbS/lKvySTUR8Org+Kka0dT9/
onv9u67jdegyvP9NFlDQDIdcsl+7DnmavgAx/6LdY8Es+lt6MD/v0Ne8oLlsm0gq
Igu/Z3ioMP62DnHwdYnAQlQuS3cDUzD1yZjE4ZuVyV2OIHBM8EgMuU0c1XkBfMwO
52kjA36CyhkkYvquo1UnscKh8mnzlrXXLAFu4QsfgBIOLGRP2PRqvLSTMRcx5cik
QNHFu269tnc9zAPMnywGLfBrt49+dBpTjrYm8UQlBpx/nYD9/5aAgEyEy5xPvmpR
iQIDAQAB
-----END PUBLIC KEY-----
`
	testPublicKeyBody = "MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA6ooAAQaPkAcFAx80Kx9eSD41D1Rotw776vukPcsr6QsaMMphkaXPXH09jjoXbS/lKvySTUR8Org+Kka0dT9/onv9u67jdegyvP9NFlDQDIdcsl+7DnmavgAx/6LdY8Es+lt6MD/v0Ne8oLlsm0gqIgu/Z3ioMP62DnHwdYnAQlQuS3cDUzD1yZjE4ZuVyV2OIHBM8EgMuU0c1XkBfMwO52kjA36CyhkkYvquo1UnscKh8mnzlrXXLAFu4QsfgBIOLGRP2PRqvLSTMRcx5cikQNHFu269tnc9zAPMnywGLfBrt49+dBpTjrYm8UQlBpx/nYD9/5aAgEyEy5xPvmpRiQIDAQAB"
)

func runSeed(t *testing.T, keyPEM string) (string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key.pem")
	require.NoError(t, os.WriteFile(path, []byte(keyPEM), 0o600))
	return runSeedWithFile(path)
}

func runSeedWithFile(path string) (string, error) {
	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"seed", "--public-key-file", path})
	err := root.Execute()
	return out.String(), err
}

func TestSeedPrintsSeedSQLWithTheKeyFilledIn(t *testing.T) {
	out, err := runSeed(t, testPublicKeyPEM)
	require.NoError(t, err)

	static, err := os.ReadFile("../../deploy/snowflake/seed.sql")
	require.NoError(t, err)
	want := strings.Replace(string(static), "'<public key body>'", "'"+testPublicKeyBody+"'", 1)
	require.NotEqual(t, string(static), want, "the static Seed SQL has no key placeholder")
	require.Equal(t, want, out)
}

func TestSeedRefusesAPrivateKeyWithoutEchoingIt(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))

	out, err := runSeed(t, keyPEM)
	require.ErrorContains(t, err, "private key")
	require.Empty(t, out)
	for _, line := range strings.Split(keyPEM, "\n")[1:4] {
		require.NotContains(t, err.Error(), line)
	}
}

func TestSeedRejectsKeysSnowflakeCannotUse(t *testing.T) {
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	ecDER, err := x509.MarshalPKIXPublicKey(&ec.PublicKey)
	require.NoError(t, err)

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pkcs1 := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: x509.MarshalPKCS1PublicKey(&rsaKey.PublicKey)}))

	tests := []struct{ name, pem, want string }{
		{"not PEM", "ssh-rsa AAAA not a pem file", "not a PEM public key"},
		{"damaged", "-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----\n", "not a valid public key"},
		{"not RSA", string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: ecDER})), "must be an RSA public key"},
		{"certificate", "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n", "a certificate, not a public key"},
		{"PKCS#1 public key", pkcs1, "use openssl rsa -pubout"},
		{"two blocks", testPublicKeyPEM + testPublicKeyPEM, "exactly one PEM block"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runSeed(t, tc.pem)
			require.ErrorContains(t, err, tc.want)
			require.Empty(t, out)
		})
	}
}

func TestSeedReportsAMissingKeyFile(t *testing.T) {
	_, err := runSeedWithFile(filepath.Join(t.TempDir(), "missing.pem"))
	require.ErrorContains(t, err, "no such file")
}
