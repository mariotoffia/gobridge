package tlsgen

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
	"net"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
)

// Options configures self-signed certificate generation.
type Options struct {
	CommonName   string
	Organization []string
	DNSNames     []string
	IPAddresses  []string
	ValidFor     time.Duration
	KeyType      string // "ecdsa" (default) or "rsa"
	KeySize      int    // RSA only; default: 2048
	IsCA         bool

	// SignedBy, when set, issues the certificate from that authority instead
	// of self-signing it. The result carries the issuer in CAPEM so a caller
	// has the trust anchor to validate the chain with. Mutual TLS needs this:
	// a client certificate and a server certificate that are each their own
	// authority prove nothing about who vouched for whom.
	SignedBy *Result
}

// Result holds generated PEM material.
type Result struct {
	CertPEM string
	KeyPEM  string
	CAPEM   string
}

// Generate creates a self-signed certificate and private key based on the
// supplied options. Defaults are applied for zero-value fields.
func Generate(opts Options) (*Result, error) {
	if opts.CommonName == "" {
		opts.CommonName = "localhost"
	}
	if opts.ValidFor == 0 {
		opts.ValidFor = time.Hour
	}
	if opts.KeyType == "" {
		opts.KeyType = "ecdsa"
	}
	if opts.KeySize == 0 {
		opts.KeySize = 2048
	}
	if len(opts.DNSNames) == 0 {
		opts.DNSNames = []string{opts.CommonName}
	}

	privateKey, publicKey, err := generateKeyPair(opts.KeyType, opts.KeySize)
	if err != nil {
		return nil, err
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial number: %w", err)
	}

	now := time.Now()
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   opts.CommonName,
			Organization: opts.Organization,
		},
		NotBefore:             now,
		NotAfter:              now.Add(opts.ValidFor),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  opts.IsCA,
		DNSNames:              opts.DNSNames,
	}

	if opts.IsCA {
		template.KeyUsage |= x509.KeyUsageCertSign
	}

	for _, addr := range opts.IPAddresses {
		ip := net.ParseIP(addr)
		if ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		}
	}

	issuer, issuerKey := &template, privateKey
	if opts.SignedBy != nil {
		parsed, parsedKey, parseErr := parseIssuer(opts.SignedBy)
		if parseErr != nil {
			return nil, parseErr
		}
		issuer, issuerKey = parsed, parsedKey
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, issuer, publicKey, issuerKey)
	if err != nil {
		return nil, fmt.Errorf("create certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	keyPEM, err := marshalPrivateKey(privateKey)
	if err != nil {
		return nil, err
	}

	result := &Result{
		CertPEM: string(certPEM),
		KeyPEM:  string(keyPEM),
	}
	switch {
	case opts.SignedBy != nil:
		result.CAPEM = opts.SignedBy.CertPEM
	case opts.IsCA:
		result.CAPEM = result.CertPEM
	}

	return result, nil
}

// parseIssuer recovers the signing certificate and private key from a
// previously generated result.
func parseIssuer(parent *Result) (*x509.Certificate, any, error) {
	block, _ := pem.Decode([]byte(parent.CertPEM))
	if block == nil {
		return nil, nil, fmt.Errorf("issuer certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse issuer certificate: %w", err)
	}
	keyBlock, _ := pem.Decode([]byte(parent.KeyPEM))
	if keyBlock == nil {
		return nil, nil, fmt.Errorf("issuer key is not PEM")
	}
	switch keyBlock.Type {
	case "EC PRIVATE KEY":
		key, parseErr := x509.ParseECPrivateKey(keyBlock.Bytes)
		if parseErr != nil {
			return nil, nil, fmt.Errorf("parse issuer EC key: %w", parseErr)
		}
		return cert, key, nil
	case "RSA PRIVATE KEY":
		key, parseErr := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
		if parseErr != nil {
			return nil, nil, fmt.Errorf("parse issuer RSA key: %w", parseErr)
		}
		return cert, key, nil
	default:
		return nil, nil, fmt.Errorf("unsupported issuer key type %q", keyBlock.Type)
	}
}

// MustGenerate calls Generate and panics on error.
func MustGenerate(opts Options) *Result {
	r, err := Generate(opts)
	if err != nil {
		panic(fmt.Sprintf("tlsgen: %v", err))
	}
	return r
}

// TestCredentialSet generates a self-signed CA certificate and returns it as a
// connectivity.CredentialSet ready for use in tests. Panics on generation failure.
func TestCredentialSet(commonName string) *connectivity.CredentialSet {
	r := MustGenerate(Options{
		CommonName: commonName,
		DNSNames:   []string{commonName, "localhost"},
		ValidFor:   time.Hour,
		IsCA:       true,
	})

	tls := connectivity.NewTLSMaterial(r.CertPEM, r.KeyPEM, []string{r.CAPEM}, true)
	return connectivity.NewCredentialSet(nil, &tls)
}

func generateKeyPair(keyType string, keySize int) (any, any, error) {
	switch keyType {
	case "ecdsa":
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, nil, fmt.Errorf("generate ECDSA key: %w", err)
		}
		return key, &key.PublicKey, nil
	case "rsa":
		key, err := rsa.GenerateKey(rand.Reader, keySize)
		if err != nil {
			return nil, nil, fmt.Errorf("generate RSA key: %w", err)
		}
		return key, &key.PublicKey, nil
	default:
		return nil, nil, fmt.Errorf("unsupported key type: %q", keyType)
	}
}

func marshalPrivateKey(key any) ([]byte, error) {
	switch k := key.(type) {
	case *ecdsa.PrivateKey:
		der, err := x509.MarshalECPrivateKey(k)
		if err != nil {
			return nil, fmt.Errorf("marshal EC private key: %w", err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
	case *rsa.PrivateKey:
		der := x509.MarshalPKCS1PrivateKey(k)
		return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}), nil
	default:
		return nil, fmt.Errorf("unsupported private key type: %T", key)
	}
}
