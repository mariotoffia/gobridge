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

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, publicKey, privateKey)
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
	if opts.IsCA {
		result.CAPEM = result.CertPEM
	}

	return result, nil
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

	return &connectivity.CredentialSet{
		TLS: &connectivity.TLSMaterial{
			CertPEM:            r.CertPEM,
			KeyPEM:             r.KeyPEM,
			CAPEMs:             []string{r.CAPEM},
			InsecureSkipVerify: true,
		},
	}
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
