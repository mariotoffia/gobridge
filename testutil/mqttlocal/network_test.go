package mqttlocal_test

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"strings"
	"testing"

	"github.com/mariotoffia/gobridge/testutil/dockerexec"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// A client in another container cannot use 127.0.0.1: inside that container
// the address is the container itself. It reaches the broker by container name
// on a shared Docker network, and over TLS that only works when the broker's
// certificate lists that name. This drives the real path: a Mosquitto client in
// a second container verifies the certificate against the fixture CA by name,
// logs in, publishes with a confirmed QoS 1 and reads the message back, while
// the test process still reaches the same broker through 127.0.0.1.
//
// Category: integration (TESTS.md §1) — Docker-backed, skips in -short.

func TestBrokerInstance_ContainerOnTheNetworkReachesTheBrokerByNameOverTLS(t *testing.T) {
	requireDocker(t)
	const user, password = "bridge", "s3cret"

	network := newNetwork(t)
	broker := mqttlocal.NewBrokerInstance(t,
		mqttlocal.WithAuth(user, password),
		mqttlocal.WithTLS(),
		mqttlocal.WithNetwork(network),
	)

	name := broker.ContainerName()
	if want := fmt.Sprintf("tcp://%s:1883", name); broker.NetworkURL() != want {
		t.Fatalf("NetworkURL() = %q, want %q", broker.NetworkURL(), want)
	}
	if want := fmt.Sprintf("ssl://%s:8883", name); broker.NetworkTLSURL() != want {
		t.Fatalf("NetworkTLSURL() = %q, want %q", broker.NetworkTLSURL(), want)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(broker.Material().CAPEM)) {
		t.Fatal("the published CA is not usable PEM")
	}
	if err := connectTLS(t, broker.TLSURL(), &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    pool,
	}, user, password); err != nil {
		t.Fatalf("the test process could not reach the broker through %s: %v", broker.TLSURL(), err)
	}

	// A retained message makes the read-back independent of when the
	// subscriber attaches. mosquitto_pub at QoS 1 exits 0 only after PUBACK,
	// and both clients verify the host name against the CA unless --insecure
	// is given, which it is not.
	message := mqttlocal.UniqueClientID("over-the-network")
	script := `printf '%s' "$CA_PEM" > /tmp/ca.pem &&
mosquitto_pub -h "$HOST" -p 8883 --cafile /tmp/ca.pem -u "$MQTT_USER" -P "$MQTT_PASSWORD" -t "$TOPIC" -q 1 -r -m "$MESSAGE" &&
mosquitto_sub -h "$HOST" -p 8883 --cafile /tmp/ca.pem -u "$MQTT_USER" -P "$MQTT_PASSWORD" -t "$TOPIC" -q 1 -C 1 -W 20`
	out, err := runOnNetwork(t, network, mqttlocal.DefaultImage, map[string]string{
		"CA_PEM":        broker.Material().CAPEM,
		"HOST":          name,
		"MQTT_USER":     user,
		"MQTT_PASSWORD": password,
		"TOPIC":         "gobridge/network/" + message,
		"MESSAGE":       message,
	}, script)
	if err != nil {
		t.Fatalf("a client on network %s could not use %s: %v\n%s", network, broker.NetworkTLSURL(), err, out)
	}
	if got := strings.TrimSpace(string(out)); got != message {
		t.Fatalf("the client on the network read back %q, want %q", got, message)
	}
}

// The shared broker joins a network through ForceStart's options. A client in a
// second container dials it by the name ContainerName returns, in plain MQTT
// without credentials, publishes with a confirmed QoS 1 and reads it back.
func TestForceStart_ContainerOnTheNetworkReachesTheSharedBrokerByName(t *testing.T) {
	requireDocker(t)
	network := newNetwork(t)
	mqttlocal.ForceStart(t, mqttlocal.WithNetwork(network))
	name := mqttlocal.ContainerName(t)

	message := mqttlocal.UniqueClientID("over-the-shared-network")
	script := `mosquitto_pub -h "$HOST" -p 1883 -t "$TOPIC" -q 1 -r -m "$MESSAGE" &&
mosquitto_sub -h "$HOST" -p 1883 -t "$TOPIC" -q 1 -C 1 -W 20`
	out, err := runOnNetwork(t, network, mqttlocal.DefaultImage, map[string]string{
		"HOST":    name,
		"TOPIC":   "gobridge/shared-network/" + message,
		"MESSAGE": message,
	}, script)
	if err != nil {
		t.Fatalf("a client on network %s could not use %s:1883: %v\n%s", network, name, err, out)
	}
	if got := strings.TrimSpace(string(out)); got != message {
		t.Fatalf("the client on the network read back %q, want %q", got, message)
	}
}

// Options given to ForceStart end with the test that gave them. The network the
// first start joined is removed when its subtest ends, so a later start that
// still carried WithNetwork would fail in docker run on the missing network.
func TestForceStart_OptionsEndWithTheTest(t *testing.T) {
	requireDocker(t)
	if !t.Run("start on a network", func(t *testing.T) {
		network := newNetwork(t)
		mqttlocal.ForceStart(t, mqttlocal.WithNetwork(network))
	}) {
		t.FailNow()
	}

	mqttlocal.ForceStart(t)
	out, err := dockerexec.Run(dockerexec.ExecTimeout, "inspect", "--format",
		"{{range $name, $settings := .NetworkSettings.Networks}}{{$name}} {{end}}",
		mqttlocal.ContainerName(t))
	if err != nil {
		t.Fatalf("inspect the networks of %s: %v\n%s", mqttlocal.ContainerName(t), err, out)
	}
	// "bridge" is Docker's default network, the one a container joins when
	// docker run is given no --network.
	if got := strings.TrimSpace(string(out)); got != "bridge" {
		t.Fatalf("a ForceStart(t) without options joined networks %q, want only \"bridge\"", got)
	}
}

func requireDocker(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("Docker-backed broker fixture; skipped in -short")
	}
	if !dockerexec.DockerAvailable() {
		t.Skip("docker not found")
	}
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
	name := "gobridge-mqttclient-" + rand.Text()
	t.Cleanup(func() { _, _ = dockerexec.Remove(name) })
	args := []string{"run", "--name", name, "--network", network, "--entrypoint", "sh"}
	for key, value := range env {
		args = append(args, "-e", key+"="+value)
	}
	return dockerexec.Run(dockerexec.RunTimeout, append(args, image, "-c", script)...)
}
