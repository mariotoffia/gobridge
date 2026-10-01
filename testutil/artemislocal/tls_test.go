package artemislocal_test

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Azure/go-amqp"

	"github.com/mariotoffia/gobridge/testutil/artemislocal"
	"github.com/mariotoffia/gobridge/testutil/dockerexec"
)

// A client that must use amqps:// needs a broker that serves TLS with a
// certificate it can verify, and a client in another container needs that
// certificate to name the broker's container. These drive both paths for real:
// the test process over 127.0.0.1 trusting only the fixture CA, and a second
// container on a shared Docker network that verifies the broker by container
// name, logs in and sends a message the test process then receives.
//
// Category: integration (TESTS.md §1) — Docker-backed, skips in -short.

const amqpTimeout = 30 * time.Second

func TestTLSEndpoint_ClientTrustingOnlyTheCASendsAndReceives(t *testing.T) {
	requireStartedBroker(t)
	artemislocal.ForceStart(t, artemislocal.WithTLS())

	endpoint := artemislocal.TLSEndpoint(t)
	if !strings.HasPrefix(endpoint, "amqps://127.0.0.1:") {
		t.Fatalf("TLSEndpoint() = %q, want amqps://127.0.0.1:<port>", endpoint)
	}
	for getter, got := range map[string]string{
		"NetworkEndpoint":    artemislocal.NetworkEndpoint(t),
		"NetworkTLSEndpoint": artemislocal.NetworkTLSEndpoint(t),
	} {
		if got != "" {
			t.Errorf("%s() = %q without WithNetwork, want \"\"", getter, got)
		}
	}

	// An empty trust store must fail, or the successful dial below would prove
	// nothing about the certificate.
	if _, err := dial(t, endpoint, x509.NewCertPool()); err == nil {
		t.Fatal("an empty trust store validated the broker certificate")
	}

	conn, err := dial(t, endpoint, caPool(t))
	if err != nil {
		t.Fatalf("a client trusting only CAPEM could not log in at %s: %v", endpoint, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), amqpTimeout)
	defer cancel()
	session, err := conn.NewSession(ctx, nil)
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	address := "queue://" + artemislocal.UniqueAddress("gobridge-tls")
	receiver, err := session.NewReceiver(ctx, address, &amqp.ReceiverOptions{
		Credit: 1, SourceCapabilities: []string{"queue"},
	})
	if err != nil {
		t.Fatalf("attach receiver to %s: %v", address, err)
	}
	sender, err := session.NewSender(ctx, address, &amqp.SenderOptions{
		TargetCapabilities: []string{"queue"},
	})
	if err != nil {
		t.Fatalf("attach sender to %s: %v", address, err)
	}
	body := artemislocal.UniqueAddress("over-tls")
	if err := sender.Send(ctx, amqp.NewMessage([]byte(body)), nil); err != nil {
		t.Fatalf("send to %s: %v", address, err)
	}
	msg, err := receiver.Receive(ctx, nil)
	if err != nil {
		t.Fatalf("receive from %s: %v", address, err)
	}
	if err := receiver.AcceptMessage(ctx, msg); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if got := string(msg.GetData()); got != body {
		t.Fatalf("received %q, want %q", got, body)
	}
}

func TestNetworkTLSEndpoint_ContainerOnTheNetworkLogsInByName(t *testing.T) {
	requireStartedBroker(t)
	network := newNetwork(t)
	artemislocal.ForceStart(t, artemislocal.WithTLS(), artemislocal.WithNetwork(network))

	networkTLS, err := url.Parse(artemislocal.NetworkTLSEndpoint(t))
	if err != nil || networkTLS.Scheme != "amqps" || networkTLS.Port() != "5671" ||
		!strings.HasPrefix(networkTLS.Hostname(), "gobridge-artemis-") {
		t.Fatalf("NetworkTLSEndpoint() = %q, want amqps://<container name>:5671", artemislocal.NetworkTLSEndpoint(t))
	}
	host := networkTLS.Hostname()
	if want := "amqp://" + host + ":5672"; artemislocal.NetworkEndpoint(t) != want {
		t.Fatalf("NetworkEndpoint() = %q, want %q", artemislocal.NetworkEndpoint(t), want)
	}

	// The Artemis command-line producer is a Qpid JMS AMQP 1.0 client. With
	// transport.verifyHost it checks the certificate names the host it dialled,
	// and the trust store it is given holds only the fixture CA.
	user, password := artemislocal.Credentials()
	queue := artemislocal.UniqueAddress("gobridge-network")
	message := artemislocal.UniqueAddress("over-the-network")
	script := `printf '%s' "$CA_PEM" > /tmp/ca.pem &&
keytool -importcert -noprompt -alias ca -file /tmp/ca.pem -keystore /tmp/trust.p12 -storetype PKCS12 -storepass changeit &&
/opt/activemq-artemis/bin/artemis producer --silent --protocol AMQP \
  --url "amqps://$HOST:5671?transport.trustStoreLocation=/tmp/trust.p12&transport.trustStorePassword=changeit&transport.trustStoreType=PKCS12&transport.verifyHost=true" \
  --user "$AMQP_USER" --password "$AMQP_PASSWORD" --destination "queue://$QUEUE" --message-count 1 --message "$MESSAGE"`
	out, err := runOnNetwork(t, network, artemislocal.DefaultImage, map[string]string{
		"CA_PEM":        artemislocal.CAPEM(t),
		"HOST":          host,
		"AMQP_USER":     user,
		"AMQP_PASSWORD": password,
		"QUEUE":         queue,
		"MESSAGE":       message,
	}, script)
	if err != nil {
		t.Fatalf("a client on network %s could not send through %s: %v\n%s",
			network, artemislocal.NetworkTLSEndpoint(t), err, out)
	}

	// The message the container sent by name arrives at the broker the test
	// process reaches over 127.0.0.1.
	conn, err := dial(t, artemislocal.TLSEndpoint(t), caPool(t))
	if err != nil {
		t.Fatalf("dial %s: %v", artemislocal.TLSEndpoint(t), err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), amqpTimeout)
	defer cancel()
	session, err := conn.NewSession(ctx, nil)
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	receiver, err := session.NewReceiver(ctx, queue, &amqp.ReceiverOptions{
		Credit: 1, SourceCapabilities: []string{"queue"},
	})
	if err != nil {
		t.Fatalf("attach receiver to %s: %v", queue, err)
	}
	msg, err := receiver.Receive(ctx, nil)
	if err != nil {
		t.Fatalf("receive the message the network client sent: %v\nclient output:\n%s", err, out)
	}
	if err := receiver.AcceptMessage(ctx, msg); err != nil {
		t.Fatalf("accept: %v", err)
	}
	// A JMS text message travels as an AMQP value, not as a data section.
	if got := fmt.Sprint(msg.Value); got != message {
		t.Fatalf("received %q, want %q", got, message)
	}
}

func TestNetworkEndpoint_WithoutTLSServesNoTLSAddress(t *testing.T) {
	requireStartedBroker(t)
	network := newNetwork(t)
	artemislocal.ForceStart(t, artemislocal.WithNetwork(network))

	if got := artemislocal.NetworkEndpoint(t); !strings.HasPrefix(got, "amqp://gobridge-artemis-") ||
		!strings.HasSuffix(got, ":5672") {
		t.Fatalf("NetworkEndpoint() = %q, want amqp://<container name>:5672", got)
	}
	for getter, got := range map[string]string{
		"TLSEndpoint":        artemislocal.TLSEndpoint(t),
		"NetworkTLSEndpoint": artemislocal.NetworkTLSEndpoint(t),
		"CAPEM":              artemislocal.CAPEM(t),
	} {
		if got != "" {
			t.Errorf("%s() = %q without WithTLS, want \"\"", getter, got)
		}
	}
}

// ARTEMIS_URL points the package at a broker it did not start, so it has no
// TLS listener, network or CA of its own to report, whatever the options say.
func TestNewEndpoints_AreEmptyForAnExternalBroker(t *testing.T) {
	const external = "amqp://127.0.0.1:1"
	t.Setenv("ARTEMIS_URL", external)

	if got := artemislocal.ForceStart(t, artemislocal.WithTLS(), artemislocal.WithNetwork("unused")); got != external {
		t.Fatalf("ForceStart() = %q, want the ARTEMIS_URL %q", got, external)
	}
	for getter, got := range map[string]string{
		"TLSEndpoint":        artemislocal.TLSEndpoint(t),
		"NetworkEndpoint":    artemislocal.NetworkEndpoint(t),
		"NetworkTLSEndpoint": artemislocal.NetworkTLSEndpoint(t),
		"CAPEM":              artemislocal.CAPEM(t),
	} {
		if got != "" {
			t.Errorf("%s() = %q for an external broker, want \"\"", getter, got)
		}
	}
}

// Options given to ForceStart end with the test that gave them. The network the
// first start joined is removed when its subtest ends, so a later start that
// still carried WithNetwork would fail in docker run on the missing network.
func TestForceStart_OptionsEndWithTheTest(t *testing.T) {
	requireStartedBroker(t)
	if !t.Run("start on a network", func(t *testing.T) {
		network := newNetwork(t)
		artemislocal.ForceStart(t, artemislocal.WithNetwork(network))
		if got := artemislocal.NetworkEndpoint(t); got == "" {
			t.Fatalf("NetworkEndpoint() = \"\" after ForceStart(t, WithNetwork(%q))", network)
		}
	}) {
		t.FailNow()
	}

	artemislocal.ForceStart(t)
	if got := artemislocal.NetworkEndpoint(t); got != "" {
		t.Fatalf("NetworkEndpoint() = %q after a ForceStart(t) without options, want \"\"", got)
	}
}

// requireStartedBroker skips where these tests cannot start a broker of their
// own: in -short, without Docker, or when ARTEMIS_URL names an external one.
func requireStartedBroker(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("Docker-backed broker fixture; skipped in -short")
	}
	if !dockerexec.DockerAvailable() {
		t.Skip("docker not found")
	}
	if os.Getenv("ARTEMIS_URL") != "" {
		t.Skip("ARTEMIS_URL names an external broker; TLS and network addresses exist only for a broker this package starts")
	}
}

func caPool(t *testing.T) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(artemislocal.CAPEM(t))) {
		t.Fatalf("CAPEM() is not usable PEM: %q", artemislocal.CAPEM(t))
	}
	return pool
}

// dial logs in over TLS, trusting only roots. The connection closes when the
// test ends.
func dial(t *testing.T, endpoint string, roots *x509.CertPool) (*amqp.Conn, error) {
	t.Helper()
	user, password := artemislocal.Credentials()
	ctx, cancel := context.WithTimeout(t.Context(), amqpTimeout)
	defer cancel()
	conn, err := amqp.Dial(ctx, endpoint, &amqp.ConnOptions{
		SASLType:  amqp.SASLTypePlain(user, password),
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
	})
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, nil
}

// newNetwork creates a Docker network only this test uses and removes it when
// the test ends. Call it before starting the broker: cleanups run last in,
// first out, so the broker has left the network by the time it is removed.
func newNetwork(t *testing.T) string {
	t.Helper()
	name := "gobridge-test-net-" + rand.Text()
	if out, err := dockerexec.Run(dockerexec.ExecTimeout, "network", "create", name); err != nil {
		if !dockerexec.MustSucceed() {
			t.Skipf("create Docker network %s (docker absent): %v\n%s", name, err, out)
		}
		t.Fatalf("create Docker network %s: %v\n%s", name, err, out)
	}
	t.Cleanup(func() {
		if out, err := dockerexec.Run(dockerexec.RemoveTimeout, "network", "rm", name); err != nil {
			t.Errorf("remove Docker network %s: %v\n%s", name, err, out)
		}
	})
	return name
}

// runOnNetwork runs script with sh in a throwaway container of image attached
// to network, and returns the combined output. The container and its anonymous
// volumes are removed when the test ends, also when the script times out.
func runOnNetwork(t *testing.T, network, image string, env map[string]string, script string) ([]byte, error) {
	t.Helper()
	name := "gobridge-artemisclient-" + rand.Text()
	t.Cleanup(func() { _, _ = dockerexec.Remove(name) })
	args := []string{"run", "--name", name, "--network", network, "--entrypoint", "sh"}
	for key, value := range env {
		args = append(args, "-e", key+"="+value)
	}
	return dockerexec.Run(dockerexec.RunTimeout, append(args, image, "-c", script)...)
}
