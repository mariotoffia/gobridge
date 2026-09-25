package mqttlocal_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/testutil/dockerexec"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// A broker fixture that says it requires credentials, or a certificate, and
// quietly does not is worse than no fixture: every test above it still passes
// while proving nothing. These checks drive the fixture's NEGATIVE cases — the
// connection that must be REFUSED — because those are the ones a silently
// permissive broker would turn green.
//
// Category: integration (TESTS.md §1) — Docker-backed, skips in -short.

const secureDialTimeout = 10 * time.Second

func TestSecureBroker_AnonymousConnectIsRefused(t *testing.T) {
	broker := mqttlocal.NewBrokerInstance(t,
		mqttlocal.WithAuth("bridge", "s3cret"),
	)

	err := connectPlain(t, broker.URL(), "", "")
	if err == nil {
		t.Fatal("an authenticated broker accepted an anonymous connection")
	}

	if err := connectPlain(t, broker.URL(), "bridge", "s3cret"); err != nil {
		t.Fatalf("the configured credentials were refused: %v", err)
	}
}

func TestSecureBroker_WrongPasswordIsRefused(t *testing.T) {
	broker := mqttlocal.NewBrokerInstance(t,
		mqttlocal.WithAuth("bridge", "s3cret"),
	)

	if err := connectPlain(t, broker.URL(), "bridge", "wrong"); err == nil {
		t.Fatal("the broker accepted a wrong password")
	}
}

func TestSecureBroker_TLSListenerServesTheFixtureCA(t *testing.T) {
	broker := mqttlocal.NewBrokerInstance(t,
		mqttlocal.WithAuth("bridge", "s3cret"),
		mqttlocal.WithTLS(),
	)

	material := broker.Material()
	if material == nil || material.CAPEM == "" {
		t.Fatal("a TLS fixture must publish the CA a client validates against")
	}
	if broker.TLSURL() == "" {
		t.Fatal("a TLS fixture must publish its TLS endpoint")
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(material.CAPEM)) {
		t.Fatal("the published CA is not usable PEM")
	}
	if err := connectTLS(t, broker.TLSURL(), &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    pool,
	}, "bridge", "s3cret"); err != nil {
		t.Fatalf("a CA-validating TLS connect failed: %v", err)
	}

	// The certificate must be the fixture's, not any certificate: an empty
	// trust store has to fail.
	if err := connectTLS(t, broker.TLSURL(), &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    x509.NewCertPool(),
	}, "bridge", "s3cret"); err == nil {
		t.Fatal("an empty trust store validated the broker certificate")
	}
}

func TestSecureBroker_MutualTLSRequiresAClientCertificate(t *testing.T) {
	broker := mqttlocal.NewBrokerInstance(t,
		mqttlocal.WithTLS(),
		mqttlocal.WithMutualTLS(),
	)

	material := broker.Material()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(material.CAPEM)) {
		t.Fatal("the published CA is not usable PEM")
	}

	if err := connectTLS(t, broker.TLSURL(), &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    pool,
	}, "", ""); err == nil {
		t.Fatal("a mutual-TLS listener accepted a client with no certificate")
	}

	clientCert, err := tls.X509KeyPair([]byte(material.ClientCertPEM), []byte(material.ClientKeyPEM))
	if err != nil {
		t.Fatalf("the published client certificate is not a usable pair: %v", err)
	}
	if err := connectTLS(t, broker.TLSURL(), &tls.Config{
		MinVersion:   tls.VersionTLS12,
		RootCAs:      pool,
		Certificates: []tls.Certificate{clientCert},
	}, "", ""); err != nil {
		t.Fatalf("the fixture client certificate was refused: %v\n%s", err, refusalEvidence(broker))
	}
}

// refusalEvidence is what a refused client certificate is diagnosed by. The
// broker log names no reason — Mosquitto logs "certificate verify failed" for
// a foreign CA and a not-yet-valid certificate alike — and the alert names only
// a class: OpenSSL answers a certificate that is not yet valid by the broker's
// clock with a bad-certificate alert. So it puts the broker's clock beside the
// host's and lists each certificate's issuer and validity window.
func refusalEvidence(broker *mqttlocal.BrokerInstance) string {
	name := broker.ContainerName()
	clock, _ := dockerexec.Run(dockerexec.ExecTimeout, "exec", name, "date", "-u", "+%Y-%m-%dT%H:%M:%SZ")
	evidence := fmt.Sprintf("host clock %s, broker clock %s\n",
		time.Now().UTC().Format(time.RFC3339), bytes.TrimSpace(clock))
	material := broker.Material()
	for _, c := range [][2]string{{"CA", material.CAPEM}, {"client", material.ClientCertPEM}} {
		evidence += c[0] + " " + describeCertificate(c[1]) + "\n"
	}
	logs, _ := dockerexec.Run(dockerexec.LogsTimeout, "logs", "--tail", "20", name)
	return evidence + "broker log:\n" + string(logs)
}

func describeCertificate(certPEM string) string {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return "is not PEM"
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("%s issued by %s, valid %s to %s", cert.Subject, cert.Issuer,
		cert.NotBefore.Format(time.RFC3339), cert.NotAfter.Format(time.RFC3339))
}

// ---------------------------------------------------------------------------

func connectPlain(t *testing.T, brokerURL, username, password string) error {
	t.Helper()
	conn, err := net.DialTimeout("tcp", hostPort(t, brokerURL), secureDialTimeout)
	if err != nil {
		return err
	}
	return mqttConnect(t, conn, username, password)
}

func connectTLS(t *testing.T, brokerURL string, cfg *tls.Config, username, password string) error {
	t.Helper()
	dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: secureDialTimeout}, Config: cfg}
	ctx, cancel := context.WithTimeout(t.Context(), secureDialTimeout)
	defer cancel()
	conn, err := dialer.DialContext(ctx, "tcp", hostPort(t, brokerURL))
	if err != nil {
		return err
	}
	return mqttConnect(t, conn, username, password)
}

func mqttConnect(t *testing.T, conn net.Conn, username, password string) error {
	t.Helper()
	t.Cleanup(func() { _ = conn.Close() })

	client := paho.NewClient(paho.ClientConfig{Conn: conn})
	ctx, cancel := context.WithTimeout(t.Context(), secureDialTimeout)
	defer cancel()

	packet := &paho.Connect{
		ClientID:   mqttlocal.UniqueClientID("secure-probe"),
		CleanStart: true,
		KeepAlive:  30,
	}
	if username != "" {
		packet.Username = username
		packet.UsernameFlag = true
		packet.Password = []byte(password)
		packet.PasswordFlag = true
	}
	ack, err := client.Connect(ctx, packet)
	if err != nil {
		return err
	}
	if ack != nil && ack.ReasonCode != 0 {
		return errors.New("broker refused the connection")
	}
	_ = client.Disconnect(&paho.Disconnect{ReasonCode: 0})
	return nil
}

func hostPort(t *testing.T, brokerURL string) string {
	t.Helper()
	for _, prefix := range []string{"tcp://", "ssl://", "ws://", "wss://"} {
		if len(brokerURL) > len(prefix) && brokerURL[:len(prefix)] == prefix {
			return brokerURL[len(prefix):]
		}
	}
	return brokerURL
}
