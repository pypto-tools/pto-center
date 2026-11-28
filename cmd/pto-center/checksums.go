package main

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"path/filepath"

	f "github.com/pypto-tools/pto-center/internal/localfiles"
)

// Sign the checksums of the bootstrap archives with the same publisher as the
// managed JSON bundles. OpenSSL can verify them before installing any PTO binary.
func signChecksums(dir, signingKey, trust string) error {
	key, err := f.Key(signingKey, ed25519.PrivateKeySize)
	if err != nil {
		return err
	}
	pub, err := f.Public(trust)
	if err != nil {
		return err
	}
	priv := ed25519.PrivateKey(key)
	if !priv.Public().(ed25519.PublicKey).Equal(pub) {
		return errors.New("signing key does not match the publisher public key")
	}
	checksums, err := f.ReadLimit(filepath.Join(dir, "SHA256SUMS"), 1<<20)
	if err != nil {
		return err
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return err
	}
	if err = f.Create(filepath.Join(dir, "release.pem"), pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0644); err != nil {
		return err
	}
	return f.Create(filepath.Join(dir, "SHA256SUMS.sig"), ed25519.Sign(priv, checksums), 0644)
}
