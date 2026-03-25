// Package builders provides fluent builders for constructing credentials.
package builders

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"time"

	"github.com/mariotoffia/gobridge/bridge/types"
)

// CredentialsBuilder provides a fluent interface for building types.Credentials.
// It supports building credentials from inline strings, files, or generating
// self-signed certificates for testing.
//
// Example usage:
//
//	// Username/password credentials
//	creds, err := builders.NewCredentialsBuilder().
//	    WithUsernamePassword("admin", "secret").
//	    Build()
//
//	// TLS credentials from files
//	creds, err := builders.NewCredentialsBuilder().
//	    WithCertFile("/path/to/cert.pem").
//	    WithKeyFile("/path/to/key.pem").
//	    WithCAFile("/path/to/ca.pem").
//	    Build()
//
//	// Self-signed for testing
//	creds, err := builders.NewCredentialsBuilder().
//	    WithSelfSigned(builders.SelfSignedOptions{
//	        CommonName: "test-server",
//	        ValidFor:   24 * time.Hour,
//	    }).
//	    Build()
type CredentialsBuilder struct {
	username string
	password string

	certPEM  string
	keyPEM   string
	caPEMs   []string
	insecure bool

	selfSignedOpts *SelfSignedOptions

	errors []error
}

// SelfSignedOptions configures self-signed certificate generation.
type SelfSignedOptions struct {
	// CommonName for the certificate subject (e.g., "localhost", "test-server").
	CommonName string
	// Organization for the certificate subject.
	Organization []string
	// DNSNames for Subject Alternative Names.
	DNSNames []string
	// IPAddresses for Subject Alternative Names (as strings, e.g., "127.0.0.1").
	IPAddresses []string
	// ValidFor is how long the certificate is valid.
	ValidFor time.Duration
	// KeyType is the key algorithm ("rsa" or "ecdsa"). Defaults to "ecdsa".
	KeyType string
	// KeySize is the key size for RSA (e.g., 2048, 4096). Ignored for ECDSA.
	KeySize int
	// IsCA generates a CA certificate if true.
	IsCA bool
}

// NewCredentialsBuilder creates a new credentials builder.
func NewCredentialsBuilder() *CredentialsBuilder {
	return &CredentialsBuilder{}
}

// WithUsernamePassword adds username/password credentials.
func (b *CredentialsBuilder) WithUsernamePassword(username, password string) *CredentialsBuilder {
	b.username = username
	b.password = password
	return b
}

// WithCert adds a certificate from an inline PEM string.
func (b *CredentialsBuilder) WithCert(certPEM string) *CredentialsBuilder {
	b.certPEM = certPEM
	return b
}

// WithCertFile adds a certificate from a file.
func (b *CredentialsBuilder) WithCertFile(path string) *CredentialsBuilder {
	data, err := os.ReadFile(path)
	if err != nil {
		b.errors = append(b.errors, fmt.Errorf("failed to read cert file %q: %w", path, err))
		return b
	}
	b.certPEM = string(data)
	return b
}

// WithKey adds a private key from an inline PEM string.
func (b *CredentialsBuilder) WithKey(keyPEM string) *CredentialsBuilder {
	b.keyPEM = keyPEM
	return b
}

// WithKeyFile adds a private key from a file.
func (b *CredentialsBuilder) WithKeyFile(path string) *CredentialsBuilder {
	data, err := os.ReadFile(path)
	if err != nil {
		b.errors = append(b.errors, fmt.Errorf("failed to read key file %q: %w", path, err))
		return b
	}
	b.keyPEM = string(data)
	return b
}

// WithCA adds a CA certificate from an inline PEM string.
// Can be called multiple times to add a certificate chain.
func (b *CredentialsBuilder) WithCA(caPEM string) *CredentialsBuilder {
	b.caPEMs = append(b.caPEMs, caPEM)
	return b
}

// WithCAFile adds a CA certificate from a file.
// Can be called multiple times to add a certificate chain.
func (b *CredentialsBuilder) WithCAFile(path string) *CredentialsBuilder {
	data, err := os.ReadFile(path)
	if err != nil {
		b.errors = append(b.errors, fmt.Errorf("failed to read CA file %q: %w", path, err))
		return b
	}
	b.caPEMs = append(b.caPEMs, string(data))
	return b
}

// WithCAFiles adds multiple CA certificates from files (for certificate chains).
func (b *CredentialsBuilder) WithCAFiles(paths ...string) *CredentialsBuilder {
	for _, path := range paths {
		b.WithCAFile(path)
	}
	return b
}

// WithInsecureSkipVerify sets InsecureSkipVerify to true.
// Should only be used for testing.
func (b *CredentialsBuilder) WithInsecureSkipVerify() *CredentialsBuilder {
	b.insecure = true
	return b
}

// WithSelfSigned generates a self-signed certificate for testing.
// This will generate both the certificate and private key.
func (b *CredentialsBuilder) WithSelfSigned(opts SelfSignedOptions) *CredentialsBuilder {
	b.selfSignedOpts = &opts
	return b
}

// Build constructs the Credentials object.
// Returns an error if any operations failed during building.
func (b *CredentialsBuilder) Build() (*types.Credentials, error) {
	if len(b.errors) > 0 {
		return nil, fmt.Errorf("builder errors: %v", b.errors)
	}

	creds := &types.Credentials{
		Type:        make([]types.CredentialsType, 0),
		Credentials: make([]any, 0),
	}

	// Handle self-signed generation first
	if b.selfSignedOpts != nil {
		if err := b.generateSelfSigned(); err != nil {
			return nil, fmt.Errorf("failed to generate self-signed cert: %w", err)
		}
	}

	// Add username/password credentials
	if b.username != "" {
		creds.Type = append(creds.Type, types.CredentialsTypeUsernamePassword)
		creds.Credentials = append(creds.Credentials, types.UsernamePasswordCredentials{
			Username: b.username,
			Password: b.password,
		})
	}

	// Add TLS credentials if any TLS fields are set
	if b.certPEM != "" || b.keyPEM != "" || len(b.caPEMs) > 0 || b.insecure {
		tlsCreds := types.TlsCredentials{
			CertPEM:            b.certPEM,
			KeyPEM:             b.keyPEM,
			CaPEM:              b.caPEMs,
			InsecureSkipVerify: b.insecure,
		}
		creds.Type = append(creds.Type, types.CredentialsTypeTLS)
		creds.Credentials = append(creds.Credentials, tlsCreds)
	}

	if len(creds.Type) == 0 {
		return nil, fmt.Errorf("no credentials configured")
	}

	return creds, nil
}

// MustBuild is like Build but panics on error.
// Useful for testing and initialization code.
func (b *CredentialsBuilder) MustBuild() *types.Credentials {
	creds, err := b.Build()
	if err != nil {
		panic(err)
	}
	return creds
}

// generateSelfSigned generates a self-signed certificate and populates certPEM and keyPEM.
func (b *CredentialsBuilder) generateSelfSigned() error {
	opts := b.selfSignedOpts

	// Set defaults
	if opts.CommonName == "" {
		opts.CommonName = "localhost"
	}
	if opts.ValidFor == 0 {
		opts.ValidFor = 24 * time.Hour
	}
	if opts.KeyType == "" {
		opts.KeyType = "ecdsa"
	}
	if opts.KeySize == 0 {
		opts.KeySize = 2048
	}

	// Generate private key
	var privateKey any
	var publicKey any
	var err error

	switch opts.KeyType {
	case "rsa":
		key, err := rsa.GenerateKey(rand.Reader, opts.KeySize)
		if err != nil {
			return fmt.Errorf("failed to generate RSA key: %w", err)
		}
		privateKey = key
		publicKey = &key.PublicKey
	case "ecdsa":
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return fmt.Errorf("failed to generate ECDSA key: %w", err)
		}
		privateKey = key
		publicKey = &key.PublicKey
	default:
		return fmt.Errorf("unsupported key type: %s", opts.KeyType)
	}

	// Create certificate template
	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("failed to generate serial number: %w", err)
	}

	notBefore := time.Now()
	notAfter := notBefore.Add(opts.ValidFor)

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   opts.CommonName,
			Organization: opts.Organization,
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  opts.IsCA,
	}

	// Add DNS names
	if len(opts.DNSNames) > 0 {
		template.DNSNames = opts.DNSNames
	} else {
		template.DNSNames = []string{opts.CommonName}
	}

	// Add IP addresses
	for _, ip := range opts.IPAddresses {
		if parsed := parseIP(ip); parsed != nil {
			template.IPAddresses = append(template.IPAddresses, parsed)
		}
	}

	if opts.IsCA {
		template.KeyUsage |= x509.KeyUsageCertSign
	}

	// Create certificate
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, publicKey, privateKey)
	if err != nil {
		return fmt.Errorf("failed to create certificate: %w", err)
	}

	// Encode certificate to PEM
	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certDER,
	})
	b.certPEM = string(certPEM)

	// Encode private key to PEM
	keyPEM, err := encodePrivateKey(privateKey)
	if err != nil {
		return err
	}
	b.keyPEM = string(keyPEM)

	// If this is a CA, also add it to the CA chain
	if opts.IsCA {
		b.caPEMs = append(b.caPEMs, string(certPEM))
	}

	return nil
}

// encodePrivateKey encodes a private key to PEM format.
func encodePrivateKey(key any) ([]byte, error) {
	switch k := key.(type) {
	case *rsa.PrivateKey:
		return pem.EncodeToMemory(&pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: x509.MarshalPKCS1PrivateKey(k),
		}), nil
	case *ecdsa.PrivateKey:
		der, err := x509.MarshalECPrivateKey(k)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal ECDSA key: %w", err)
		}
		return pem.EncodeToMemory(&pem.Block{
			Type:  "EC PRIVATE KEY",
			Bytes: der,
		}), nil
	default:
		return nil, fmt.Errorf("unsupported key type: %T", key)
	}
}

// parseIP parses an IP address string.
func parseIP(s string) []byte {
	// Simple parsing - in production you'd use net.ParseIP
	// but we want to avoid importing net for this
	var ip []byte
	var part byte
	var dots int

	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			part = part*10 + (c - '0')
		} else if c == '.' {
			ip = append(ip, part)
			part = 0
			dots++
		} else {
			return nil // Invalid character
		}
	}

	if dots == 3 {
		ip = append(ip, part)
		return ip
	}

	return nil // Not a valid IPv4
}

// SelfSignedResult contains the generated self-signed certificate and key.
// Use this when you need access to the raw PEM data for other purposes.
type SelfSignedResult struct {
	CertPEM string
	KeyPEM  string
	CAPEM   string // Same as CertPEM for self-signed CA
}

// GenerateSelfSigned generates a self-signed certificate without building Credentials.
// Useful when you need the PEM data for other purposes (e.g., writing to files).
func GenerateSelfSigned(opts SelfSignedOptions) (*SelfSignedResult, error) {
	builder := NewCredentialsBuilder().WithSelfSigned(opts)
	if err := builder.generateSelfSigned(); err != nil {
		return nil, err
	}

	result := &SelfSignedResult{
		CertPEM: builder.certPEM,
		KeyPEM:  builder.keyPEM,
	}

	if opts.IsCA && len(builder.caPEMs) > 0 {
		result.CAPEM = builder.caPEMs[0]
	}

	return result, nil
}

// GenerateTestCredentials generates credentials suitable for testing.
// Creates a self-signed certificate with InsecureSkipVerify enabled.
func GenerateTestCredentials(commonName string) (*types.Credentials, error) {
	return NewCredentialsBuilder().
		WithSelfSigned(SelfSignedOptions{
			CommonName: commonName,
			ValidFor:   24 * time.Hour,
			DNSNames:   []string{commonName, "localhost"},
			IsCA:       true,
		}).
		WithInsecureSkipVerify().
		Build()
}
