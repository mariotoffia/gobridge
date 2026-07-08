package httpapi

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeSelfSignedCertTo writes a fresh self-signed cert/key pair carrying the
// given CommonName to the supplied paths (truncating any existing files), so a
// test can rotate the pair in place and assert the reloader picks it up.
func writeSelfSignedCertTo(t *testing.T, certPath, keyPath, commonName string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	require.NoError(t, err)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	require.NoError(t, os.WriteFile(certPath, certPEM, 0o600))

	keyDER, err := x509.MarshalECPrivateKey(priv)
	require.NoError(t, err)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	require.NoError(t, os.WriteFile(keyPath, keyPEM, 0o600))
}

// leafCommonName parses the leaf certificate DER and returns its CommonName.
// tls.Certificate.Leaf is not reliably populated by LoadX509KeyPair across Go
// versions, so parse Certificate[0] directly.
func leafCommonName(t *testing.T, cert *tls.Certificate) string {
	t.Helper()
	require.NotEmpty(t, cert.Certificate)
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	require.NoError(t, err)
	return leaf.Subject.CommonName
}

// TestCertReloader_PicksUpRotatedCert pins finding 6: an in-process cert-manager
// renewal (atomic file replace, which bumps mtime) must be served on the next
// handshake WITHOUT a restart, while unchanged files keep serving the cached
// certificate (no per-handshake reload).
func TestCertReloader_PicksUpRotatedCert(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")

	writeSelfSignedCertTo(t, certPath, keyPath, "cn-before")

	cr, err := newCertReloader(certPath, keyPath, nil)
	require.NoError(t, err)

	got1, err := cr.getCertificate(nil)
	require.NoError(t, err)
	require.NotNil(t, got1)
	assert.Equal(t, "cn-before", leafCommonName(t, got1))

	// A second handshake with unchanged files must serve the SAME cached
	// certificate — no reload, no re-parse.
	got1b, err := cr.getCertificate(nil)
	require.NoError(t, err)
	assert.Same(t, got1, got1b, "unchanged files must serve the cached certificate")

	// Rotate the pair in place and force a strictly-later mtime (cert-manager's
	// atomic rename always bumps mtime; os.Chtimes makes the test independent of
	// filesystem timestamp granularity).
	before := fileModTime(certPath)
	writeSelfSignedCertTo(t, certPath, keyPath, "cn-after")
	rotated := before.Add(time.Second)
	require.NoError(t, os.Chtimes(certPath, rotated, rotated))
	require.NoError(t, os.Chtimes(keyPath, rotated, rotated))

	got2, err := cr.getCertificate(nil)
	require.NoError(t, err)
	assert.Equal(t, "cn-after", leafCommonName(t, got2),
		"a rotated certificate must be served without a process restart")
}

// TestCertReloader_ReloadFailureKeepsLastGood pins the fail-safe: a rotation
// caught mid-write (a valid mtime bump but an unparseable pair) must NOT break
// TLS — the reloader keeps serving the last-good certificate.
func TestCertReloader_ReloadFailureKeepsLastGood(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")

	writeSelfSignedCertTo(t, certPath, keyPath, "cn-good")
	cr, err := newCertReloader(certPath, keyPath, nil)
	require.NoError(t, err)

	good, err := cr.getCertificate(nil)
	require.NoError(t, err)
	require.Equal(t, "cn-good", leafCommonName(t, good))

	// Simulate a half-written cert file with a bumped mtime.
	before := fileModTime(certPath)
	require.NoError(t, os.WriteFile(certPath, []byte("-----BEGIN CERTIFICATE-----\nnot-valid\n-----END CERTIFICATE-----\n"), 0o600))
	bumped := before.Add(time.Second)
	require.NoError(t, os.Chtimes(certPath, bumped, bumped))

	got, err := cr.getCertificate(nil)
	require.NoError(t, err, "a failed reload must not surface an error to the handshake")
	assert.Equal(t, "cn-good", leafCommonName(t, got),
		"a failed reload must keep serving the last-good certificate")
}
