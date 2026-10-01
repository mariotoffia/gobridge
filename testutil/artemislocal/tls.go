package artemislocal

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/testutil/dockerexec"
	"github.com/mariotoffia/gobridge/testutil/tlsgen"
)

// AMQP 1.0 over TLS.
//
// The pinned image's start script copies every file in etc-override into the
// broker instance's etc directory right after it creates the instance, and
// Artemis loads etc/broker.properties when it starts. So the fixture writes the
// server key and certificate plus a broker.properties that adds a TLS acceptor,
// and mounts the directory there read-only. The plaintext acceptor on 5672
// stays as it is.

const (
	// tlsContainerPort is the AMQP 1.0 TLS acceptor inside the container.
	tlsContainerPort  = 5671
	overrideMountPath = "/var/lib/artemis-instance/etc-override"
	serverPEMName     = "server.pem"
	propertiesName    = "broker.properties"
)

// brokerProperties adds the TLS acceptor. A PEM key store takes the private
// key and the certificate from one file.
var brokerProperties = fmt.Sprintf(`acceptorConfigurations.tls.factoryClassName=org.apache.activemq.artemis.core.remoting.impl.netty.NettyAcceptorFactory
acceptorConfigurations.tls.params.host=0.0.0.0
acceptorConfigurations.tls.params.port=%d
acceptorConfigurations.tls.params.protocols=AMQP
acceptorConfigurations.tls.params.sslEnabled=true
acceptorConfigurations.tls.params.keyStoreType=PEM
acceptorConfigurations.tls.params.keyStorePath=/var/lib/artemis-instance/etc/%s
`, tlsContainerPort, serverPEMName)

// WithTLS adds an AMQP 1.0 TLS listener next to the plaintext one. The fixture
// generates a certificate authority and a server certificate it signed, valid
// for localhost, 127.0.0.1 and the container name. A client dials
// [TLSEndpoint] (or [NetworkTLSEndpoint] from the Docker network) and trusts
// [CAPEM]; it does not need to skip verification.
func WithTLS() Option {
	return func(o *options) { o.tls = true }
}

// TLSEndpoint returns the AMQP 1.0 TLS address for the test process
// (amqps://127.0.0.1:<port>). It is empty when the package was not configured
// [WithTLS], or when ARTEMIS_URL pointed it at a broker it did not start.
//
// Same start/skip semantics as [Endpoint].
func TLSEndpoint(t testing.TB) string {
	t.Helper()
	return address(t, func(a addresses) string { return a.tlsEndpoint })
}

// NetworkTLSEndpoint returns the AMQP 1.0 TLS address a client on the Docker
// network uses (amqps://<container name>:5671). It is empty unless the package
// was configured with both [WithTLS] and [WithNetwork], and when ARTEMIS_URL
// pointed it at a broker it did not start.
//
// Same start/skip semantics as [Endpoint].
func NetworkTLSEndpoint(t testing.TB) string {
	t.Helper()
	return address(t, func(a addresses) string { return a.networkTLSEndpoint })
}

// CAPEM returns the PEM certificate of the authority that signed the broker's
// TLS certificate; a client trusting only it validates the broker. It is empty
// when the package was not configured [WithTLS], or when ARTEMIS_URL pointed it
// at a broker it did not start.
//
// Same start/skip semantics as [Endpoint].
func CAPEM(t testing.TB) string {
	t.Helper()
	return address(t, func(a addresses) string { return a.caPEM })
}

// writeTLSMaterial generates the authority and the server certificate, writes
// the files the container mounts into a fresh temporary directory, and returns
// the directory and the authority's PEM certificate. The directory is the
// caller's to remove.
func writeTLSMaterial(containerName string) (string, string, error) {
	ca, err := tlsgen.Generate(tlsgen.Options{CommonName: "artemislocal-ca", IsCA: true})
	if err != nil {
		return "", "", fmt.Errorf("generate CA: %w", err)
	}
	server, err := tlsgen.Generate(tlsgen.Options{
		CommonName:  "localhost",
		DNSNames:    []string{"localhost", containerName},
		IPAddresses: []string{"127.0.0.1"},
		SignedBy:    ca,
	})
	if err != nil {
		return "", "", fmt.Errorf("generate server certificate: %w", err)
	}

	dir, err := os.MkdirTemp("", "artemistls-*")
	if err != nil {
		return "", "", fmt.Errorf("create TLS material dir: %w", err)
	}
	// The container reads these as its own non-root user, and a bind mount
	// keeps the host mode, so 0700 and 0600 would stop the broker starting.
	// None of it is secret: it is generated per fixture and removed with it.
	if err := os.Chmod(dir, 0o755); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", fmt.Errorf("chmod TLS material dir: %w", err)
	}
	for name, content := range map[string]string{
		serverPEMName:  server.KeyPEM + server.CertPEM,
		propertiesName: brokerProperties,
	} {
		//nolint:gosec // per-fixture test material, readable by the container user
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			_ = os.RemoveAll(dir)
			return "", "", fmt.Errorf("write %s: %w", name, err)
		}
	}
	return dir, ca.CertPEM, nil
}

// waitTLSReady gates on a real AMQP 1.0 SASL login over TLS that trusts only
// caPEM, so a started fixture has a TLS listener serving the certificate a
// client will validate, not merely an open port.
func waitTLSReady(endpoint, caPEM string) error {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(caPEM)) {
		return errors.New("artemislocal: the generated CA is not usable PEM")
	}
	return dockerexec.WaitProbe("Artemis AMQP over TLS on "+endpoint, 30*time.Second, time.Second,
		amqpProbe(endpoint, &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}))
}
