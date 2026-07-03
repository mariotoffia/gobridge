package httpapi

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeSelfSignedCert generates an ephemeral self-signed cert/key pair for
// 127.0.0.1 and writes them to temp PEM files, returning their paths.
func writeSelfSignedCert(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "gobridge-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  nil,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	require.NoError(t, err)

	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")

	certOut, err := os.Create(certPath)
	require.NoError(t, err)
	require.NoError(t, pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}))
	require.NoError(t, certOut.Close())

	keyDER, err := x509.MarshalECPrivateKey(priv)
	require.NoError(t, err)
	keyOut, err := os.Create(keyPath)
	require.NoError(t, err)
	require.NoError(t, pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	require.NoError(t, keyOut.Close())

	return certPath, keyPath
}

// When a cert/key pair is configured the admin and monitor listeners serve
// HTTPS, AdminURL/MonitorURL report the https scheme, and a TLS client can
// reach the (unauthenticated) health probe.
func TestServer_TLS_ServesHTTPS(t *testing.T) {
	certPath, keyPath := writeSelfSignedCert(t)

	rt := testRuntime()
	cfg := testConfig()
	cfg.TLSCertFile = certPath
	cfg.TLSKeyFile = keyPath
	s := New(rt, cfg)

	require.NoError(t, s.Start(context.Background()))
	t.Cleanup(func() { _ = s.Stop(context.Background()) })

	assert.True(t, strings.HasPrefix(s.AdminURL(), "https://"),
		"AdminURL scheme must be https, got %q", s.AdminURL())
	assert.True(t, strings.HasPrefix(s.MonitorURL(), "https://"),
		"MonitorURL scheme must be https, got %q", s.MonitorURL())

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed test cert
		},
		Timeout: 5 * time.Second,
	}
	resp, err := client.Get(s.MonitorURL() + "/api/v1/monitor/health")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	// Health returns 200 or 503 depending on runtime state; either proves the
	// TLS handshake and HTTP round-trip succeeded.
	assert.Contains(t, []int{http.StatusOK, http.StatusServiceUnavailable}, resp.StatusCode)
}

// A bad TLS keypair fails Start fast rather than on first request.
func TestServer_TLS_BadKeypairFailsStart(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(certPath, []byte("not a cert"), 0o600))
	require.NoError(t, os.WriteFile(keyPath, []byte("not a key"), 0o600))

	rt := testRuntime()
	cfg := testConfig()
	cfg.TLSCertFile = certPath
	cfg.TLSKeyFile = keyPath
	s := New(rt, cfg)

	err := s.Start(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TLS")
}
