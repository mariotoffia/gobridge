package tlsgen_test

import (
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/testutil/tlsgen"
)

// Verifies default Generate options produce an ECDSA key and localhost SANs.
func TestGenerate_DefaultECDSA(t *testing.T) {
	r, err := tlsgen.Generate(tlsgen.Options{})
	require.NoError(t, err)
	require.NotNil(t, r)

	assert.NotEmpty(t, r.CertPEM)
	assert.NotEmpty(t, r.KeyPEM)

	cert := parseCert(t, r.CertPEM)
	assert.Equal(t, "localhost", cert.Subject.CommonName)
	assert.Contains(t, cert.DNSNames, "localhost")

	key := parseKey(t, r.KeyPEM)
	assert.IsType(t, &ecdsa.PrivateKey{}, key)
}

// Verifies RSA key type and size are reflected in the generated private key.
func TestGenerate_RSA(t *testing.T) {
	r, err := tlsgen.Generate(tlsgen.Options{
		KeyType: "rsa",
		KeySize: 4096,
	})
	require.NoError(t, err)
	require.NotNil(t, r)

	cert := parseCert(t, r.CertPEM)
	assert.Equal(t, "localhost", cert.Subject.CommonName)

	key := parseKey(t, r.KeyPEM)
	rsaKey, ok := key.(*rsa.PrivateKey)
	require.True(t, ok, "expected RSA private key")
	assert.Equal(t, 4096, rsaKey.N.BitLen())
}

// Verifies CA generation sets IsCA, cert-sign key usage, and CA PEM equal to the cert PEM.
func TestGenerate_CA(t *testing.T) {
	r, err := tlsgen.Generate(tlsgen.Options{IsCA: true})
	require.NoError(t, err)

	cert := parseCert(t, r.CertPEM)
	assert.True(t, cert.IsCA)
	assert.NotZero(t, cert.KeyUsage&x509.KeyUsageCertSign)

	assert.NotEmpty(t, r.CAPEM)
	assert.Equal(t, r.CertPEM, r.CAPEM)
}

// Verifies custom DNS names and IP addresses appear on the certificate SAN extension.
func TestGenerate_CustomSANs(t *testing.T) {
	r, err := tlsgen.Generate(tlsgen.Options{
		DNSNames:    []string{"example.com", "*.example.com"},
		IPAddresses: []string{"127.0.0.1", "::1"},
	})
	require.NoError(t, err)

	cert := parseCert(t, r.CertPEM)
	assert.ElementsMatch(t, []string{"example.com", "*.example.com"}, cert.DNSNames)
	require.Len(t, cert.IPAddresses, 2)

	hasIPv4, hasIPv6 := false, false
	for _, ip := range cert.IPAddresses {
		if ip.To4() != nil {
			hasIPv4 = true
		} else {
			hasIPv6 = true
		}
	}
	assert.True(t, hasIPv4, "expected IPv4 address")
	assert.True(t, hasIPv6, "expected IPv6 address")
}

// Verifies Generate returns an error for an unsupported key type.
func TestGenerate_InvalidKeyType(t *testing.T) {
	_, err := tlsgen.Generate(tlsgen.Options{KeyType: "ed25519"})
	assert.Error(t, err)
}

// Verifies MustGenerate returns non-empty material for valid options.
func TestMustGenerate_Success(t *testing.T) {
	r := tlsgen.MustGenerate(tlsgen.Options{})
	assert.NotNil(t, r)
	assert.NotEmpty(t, r.CertPEM)
}

// Verifies MustGenerate panics when Generate would fail.
func TestMustGenerate_Panics(t *testing.T) {
	assert.Panics(t, func() {
		tlsgen.MustGenerate(tlsgen.Options{KeyType: "invalid"})
	})
}

// Verifies TestCredentialSet builds a TLS-only credential set with expected cert metadata.
func TestTestCredentialSet(t *testing.T) {
	cs := tlsgen.TestCredentialSet("test-server")
	require.NotNil(t, cs)
	require.NotNil(t, cs.TLS())
	assert.Nil(t, cs.Password())
	assert.True(t, cs.TLS().InsecureSkipVerify())
	assert.NotEmpty(t, cs.TLS().CertPEM())
	assert.NotEmpty(t, cs.TLS().KeyPEM().Reveal())
	require.Len(t, cs.TLS().CAPEMs(), 1)

	cert := parseCert(t, cs.TLS().CertPEM())
	assert.Equal(t, "test-server", cert.Subject.CommonName)
	assert.True(t, cert.IsCA)
	assert.Contains(t, cert.DNSNames, "test-server")
	assert.Contains(t, cert.DNSNames, "localhost")
}

func parseCert(t *testing.T, certPEM string) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode([]byte(certPEM))
	require.NotNil(t, block, "failed to decode certificate PEM")
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	return cert
}

func parseKey(t *testing.T, keyPEM string) any {
	t.Helper()
	block, _ := pem.Decode([]byte(keyPEM))
	require.NotNil(t, block, "failed to decode key PEM")

	switch block.Type {
	case "EC PRIVATE KEY":
		key, err := x509.ParseECPrivateKey(block.Bytes)
		require.NoError(t, err)
		return key
	case "RSA PRIVATE KEY":
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		require.NoError(t, err)
		return key
	default:
		t.Fatalf("unexpected PEM block type: %s", block.Type)
		return nil
	}
}
