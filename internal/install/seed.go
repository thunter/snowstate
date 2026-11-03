// Package install sets up an Install: the Seed, and the Foundation steps.
package install

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"strings"

	"github.com/thunter/snowstate/deploy"
)

const keyPlaceholder = "<public key body>"

var errNotPublicKey = errors.New("not a PEM public key")

// SeedSQL returns the Seed with the public key in pemBytes filled in. Errors
// never include the file's contents, which may be a private key.
func SeedSQL(pemBytes []byte) (string, error) {
	block, rest := pem.Decode(pemBytes)
	switch {
	case block == nil:
		return "", errNotPublicKey
	case strings.Contains(block.Type, "PRIVATE KEY"):
		return "", errors.New("this is a private key: pass the public key, from openssl rsa -in <key> -pubout")
	case block.Type == "CERTIFICATE":
		return "", errors.New("this is a certificate, not a public key")
	case block.Type == "RSA PUBLIC KEY":
		return "", errors.New("this is a PKCS#1 public key: use openssl rsa -pubout, which writes the form Snowflake expects")
	case block.Type != "PUBLIC KEY":
		return "", errNotPublicKey
	}
	if next, _ := pem.Decode(rest); next != nil {
		return "", errors.New("the file must hold exactly one PEM block")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return "", errors.New("not a valid public key")
	}
	if _, ok := key.(*rsa.PublicKey); !ok {
		return "", errors.New("must be an RSA public key, as Snowflake key-pair auth requires")
	}
	body := base64.StdEncoding.EncodeToString(block.Bytes)
	return strings.Replace(deploy.SeedSQL, keyPlaceholder, body, 1), nil
}
