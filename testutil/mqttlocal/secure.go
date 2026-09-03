package mqttlocal

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mariotoffia/gobridge/testutil/tlsgen"
)

// Authenticated and certificate-validating broker support.
//
// The default fixture is anonymous plaintext Mosquitto, which cannot prove
// anything about the paths a deployment actually uses: a password the broker
// rejects, a certificate chain the client validates, a client certificate the
// broker validates, and the same rules over WebSocket. The material below is
// generated per fixture and lives only in that fixture's temporary directory —
// nothing here is a credential anyone could reuse.
//
// Container-side layout. The generated material is bind-mounted read-only at
// secureMountPath, and the rendered mosquitto.conf refers to it by these paths.

const (
	secureMountPath  = "/mosquitto/secure"
	passwordFileName = "passwd"
	caFileName       = "ca.crt"
	serverCertName   = "server.crt"
	serverKeyName    = "server.key"

	// Container-internal listener ports. Only the enabled ones are published.
	plainPort = 1883
	tlsPort   = 8883
	wsPort    = 9001
	wssPort   = 9443

	// Mosquitto 2.x writes PBKDF2-SHA512 password entries as
	// `$7$<iterations>$<salt>$<hash>` with these parameters. Rendering the
	// entry here rather than shelling out to mosquitto_passwd keeps the
	// fixture to one container.
	passwordIterations = 101
	passwordSaltBytes  = 12
	passwordHashBytes  = sha512.Size
)

// Material is the TLS material a secure fixture generated, published so a test
// can validate the broker and present a client identity it will accept.
type Material struct {
	// CAPEM is the authority that signed both certificates below. A client
	// trusting only this CA validates the broker and nothing else.
	CAPEM string
	// ServerCertPEM and ServerKeyPEM are the broker's own identity, valid for
	// localhost and 127.0.0.1.
	ServerCertPEM string
	ServerKeyPEM  string
	// ClientCertPEM and ClientKeyPEM are an identity the broker accepts when
	// the listener requires a client certificate.
	ClientCertPEM string
	ClientKeyPEM  string
}

// WithAuth requires username/password authentication on every listener and
// disables anonymous access.
func WithAuth(username, password string) Option {
	return func(c *config) {
		c.username = username
		c.password = password
	}
}

// WithTLS adds a TLS listener served by a generated, CA-signed certificate.
// The CA is published through BrokerInstance.Material so a client can validate
// the broker rather than skipping verification.
func WithTLS() Option {
	return func(c *config) { c.tls = true }
}

// WithMutualTLS makes the TLS listener require a client certificate this
// fixture's CA signed. Implies WithTLS.
func WithMutualTLS() Option {
	return func(c *config) {
		c.tls = true
		c.mutualTLS = true
	}
}

// needsSecureMaterial reports whether the fixture must write a material
// directory for the container to mount.
func (c config) needsSecureMaterial() bool {
	return c.username != "" || c.tls
}

// writeSecureMaterial renders the password file and TLS material into a fresh
// temporary directory and returns the directory plus what a client needs. The
// directory is the caller's to remove.
func writeSecureMaterial(c config) (string, *Material, error) {
	dir, err := os.MkdirTemp("", "mqttsecure-*")
	if err != nil {
		return "", nil, fmt.Errorf("create material dir: %w", err)
	}
	// The container reads these as uid 1883; a bind mount preserves the host
	// mode, so 0600 would make Mosquitto fail to start. None of it is secret:
	// it is generated per fixture and discarded with the test.
	if err := os.Chmod(dir, 0o755); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("chmod material dir: %w", err)
	}

	write := func(name, content string) error {
		return os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644) //nolint:gosec // fixture material
	}

	if c.username != "" {
		entry, hashErr := mosquittoPasswordEntry(c.username, c.password)
		if hashErr != nil {
			_ = os.RemoveAll(dir)
			return "", nil, hashErr
		}
		if err := write(passwordFileName, entry); err != nil {
			_ = os.RemoveAll(dir)
			return "", nil, fmt.Errorf("write password file: %w", err)
		}
	}

	material := &Material{}
	if c.tls {
		ca, caErr := tlsgen.Generate(tlsgen.Options{CommonName: "mqttlocal-ca", IsCA: true})
		if caErr != nil {
			_ = os.RemoveAll(dir)
			return "", nil, fmt.Errorf("generate CA: %w", caErr)
		}
		server, serverErr := tlsgen.Generate(tlsgen.Options{
			CommonName:  "localhost",
			DNSNames:    []string{"localhost"},
			IPAddresses: []string{"127.0.0.1"},
			SignedBy:    ca,
		})
		if serverErr != nil {
			_ = os.RemoveAll(dir)
			return "", nil, fmt.Errorf("generate server certificate: %w", serverErr)
		}
		client, clientErr := tlsgen.Generate(tlsgen.Options{
			CommonName: "mqttlocal-client",
			DNSNames:   []string{"mqttlocal-client"},
			SignedBy:   ca,
		})
		if clientErr != nil {
			_ = os.RemoveAll(dir)
			return "", nil, fmt.Errorf("generate client certificate: %w", clientErr)
		}

		material.CAPEM = ca.CertPEM
		material.ServerCertPEM = server.CertPEM
		material.ServerKeyPEM = server.KeyPEM
		material.ClientCertPEM = client.CertPEM
		material.ClientKeyPEM = client.KeyPEM

		for name, content := range map[string]string{
			caFileName:     ca.CertPEM,
			serverCertName: server.CertPEM,
			serverKeyName:  server.KeyPEM,
		} {
			if err := write(name, content); err != nil {
				_ = os.RemoveAll(dir)
				return "", nil, fmt.Errorf("write %s: %w", name, err)
			}
		}
	}

	return dir, material, nil
}

// mosquittoPasswordEntry renders one Mosquitto 2.x password-file line. The
// broker itself is the check that this format is right: a wrong hash fails the
// fixture's readiness probe rather than passing silently.
func mosquittoPasswordEntry(username, password string) (string, error) {
	salt := make([]byte, passwordSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	hash, err := pbkdf2.Key(sha512.New, password, salt, passwordIterations, passwordHashBytes)
	if err != nil {
		return "", fmt.Errorf("derive password hash: %w", err)
	}
	return fmt.Sprintf("%s:$7$%d$%s$%s\n", username, passwordIterations,
		base64.StdEncoding.EncodeToString(salt),
		base64.StdEncoding.EncodeToString(hash)), nil
}

// secureListenerLines renders the authentication and TLS directives shared by
// every listener, plus the TLS listeners themselves.
func secureListenerLines(c config) string {
	s := ""
	if c.username != "" {
		s += fmt.Sprintf("password_file %s/%s\n", secureMountPath, passwordFileName)
	}
	if !c.tls {
		return s
	}
	s += fmt.Sprintf("\nlistener %d 0.0.0.0\nprotocol mqtt\n", tlsPort)
	s += tlsDirectives(c)
	if c.webSocket {
		s += fmt.Sprintf("\nlistener %d 0.0.0.0\nprotocol websockets\n", wssPort)
		s += tlsDirectives(c)
	}
	return s
}

func tlsDirectives(c config) string {
	s := fmt.Sprintf("certfile %s/%s\nkeyfile %s/%s\n",
		secureMountPath, serverCertName, secureMountPath, serverKeyName)
	if c.mutualTLS {
		// cafile is the CLIENT-certificate trust store; without
		// require_certificate Mosquitto would request one and accept its
		// absence, which is not what a mutual-TLS proof needs.
		s += fmt.Sprintf("cafile %s/%s\nrequire_certificate true\n",
			secureMountPath, caFileName)
	}
	return s
}
