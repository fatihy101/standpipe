package testutil

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
)

// WriteAppKey writes a PEM-encoded RSA private key into dir and returns its
// path.
//
// forge.New reads github.private_key_path at construction — deliberately, so a
// misconfigured credential fails fast rather than at first dispatch. That makes
// a real key file a precondition for building a Client at all, which is why
// this lives here rather than being reinvented in each test.
//
// 2048 bits: RSA key generation dominates the runtime of any test that calls
// this, and the fake GitHub never verifies the signature.
func WriteAppKey(dir string) (string, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "app.pem")
	buf := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		return "", err
	}
	return path, nil
}
