//go:build longrunning

package longrunning_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// publisherTinyPropertiesMode is the command suffix that switches the
// publisher helper from the worst ACCEPTED message to the worst FORWARDABLE
// one: a PUBLISH with no payload whose metadata section is filled with
// five-byte User Properties up to the source's advertised Maximum Packet Size.
const publisherTinyPropertiesMode = "tiny-properties"

// mqttMetadataAllowanceBytes is the allowance the source adds to
// max_payload_bytes when it advertises its Maximum Packet Size — the contract
// in docs/transports/mqtt-options.md. A packet built against a stale value is
// dropped by the broker as too large for the subscriber, and the proof fails
// loudly on its ack count rather than silently proving nothing.
const mqttMetadataAllowanceBytes = 128 << 10

// tinyPropertyPublish builds the packet the ingress memory model has to
// survive on a real broker: as many minimum-size (empty key, empty value)
// User Properties as fit the advertised Maximum Packet Size for
// maxPayloadBytes, and nothing else. The overhead reserves worst-case digit
// counts for the Remaining Length and the properties length, so the packet
// is at most a few bytes under the limit.
func tinyPropertyPublish(topic string, qos byte, maxPayloadBytes int) *pahov5.Publish {
	overhead := 1 + 4 + 2 + len(topic) + 2 + 4
	count := (maxPayloadBytes + mqttMetadataAllowanceBytes - overhead) / 5
	return &pahov5.Publish{
		Topic:      topic,
		QoS:        qos,
		Properties: &pahov5.PublishProperties{User: make(pahov5.UserProperties, count)},
	}
}

// TestMQTTIngressMemoryPublisherProcess is the publisher half of the MQTT
// ingress memory proofs. It runs in its own process (and, under the cgroup
// harness, its own container) so its allocations are never charged to the
// measured bridge. Commands arrive one per line as "<qos> <count>" for the
// worst accepted message and "<qos> <count> tiny-properties" for the worst
// forwardable one; each is answered with DONE once every publish is
// acknowledged. Over the control socket it serves connections one after
// another, so several proofs can share one helper.
func TestMQTTIngressMemoryPublisherProcess(t *testing.T) {
	if os.Getenv("GOBRIDGE_MQTT_MEMORY_PUBLISHER_HELPER") != "1" {
		t.Skip("subprocess-only MQTT ingress memory publisher")
	}
	payloadBytes, err := strconv.Atoi(os.Getenv("GOBRIDGE_MQTT_MEMORY_PAYLOAD_BYTES"))
	require.NoError(t, err)
	topic := os.Getenv("GOBRIDGE_MQTT_MEMORY_TOPIC")

	ctx, cancel := context.WithTimeout(t.Context(), 280*time.Second)
	t.Cleanup(cancel)
	session := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{os.Getenv("GOBRIDGE_MQTT_MEMORY_BROKER_URL")},
		ClientID:       mqttlocal.UniqueClientID("ingress-memory-publisher"),
		KeepAlive:      30,
		ConnectTimeout: 15 * time.Second,
		CleanStart:     true,
	}, connectivity.SessionEphemeral, nil)
	t.Cleanup(func() { _ = session.Close(context.Background()) })
	require.NoError(t, session.Start(ctx))

	headers := make(map[string]any, 127)
	value := strings.Repeat("v", 256)
	for i := range 125 {
		key := fmt.Sprintf("proof-%03d", i)
		headers[key] = value
	}
	// Two legal MQTT UTF-8 values drive encoded metadata close to the accepted
	// 128 KiB ceiling while 125 safe headers plus the generated message ID drive
	// the User Property count to its exact cap of 128. The large values are
	// intentionally dropped by Envelope header hygiene after admission, but
	// remain retained in Paho's unsettled wire packets during the measurement.
	headers["proof-filler-a"] = strings.Repeat("a", 46_000)
	headers["proof-filler-b"] = strings.Repeat("b", 46_000)
	message := ports.OutboundMessage{
		Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{
			Payload: make([]byte, payloadBytes),
			Headers: headers,
		}),
		Address: topic,
	}

	serve := func(input io.Reader, output io.Writer) {
		scanner := bufio.NewScanner(input)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			require.GreaterOrEqual(t, len(fields), 2, "command must be <qos> <count> [mode]")
			qos, parseErr := strconv.Atoi(fields[0])
			require.NoError(t, parseErr)
			count, parseErr := strconv.Atoi(fields[1])
			require.NoError(t, parseErr)
			if len(fields) > 2 && fields[2] == publisherTinyPropertiesMode {
				publish := tinyPropertyPublish(topic, byte(qos), payloadBytes)
				for range count {
					_, publishErr := session.ConnectionManager().Publish(ctx, publish)
					require.NoError(t, publishErr)
				}
			} else {
				sender := paho.NewSender(session, paho.SenderOptions{
					QoS:     byte(qos),
					Timeout: 30 * time.Second,
				})
				for range count {
					require.NoError(t, sender.Send(ctx, message))
				}
			}
			fmt.Fprintln(output, "DONE")
		}
		require.NoError(t, scanner.Err())
	}

	address := os.Getenv("GOBRIDGE_MQTT_MEMORY_PUBLISHER_LISTEN")
	if address == "" {
		fmt.Fprintln(os.Stdout, "READY")
		serve(os.Stdin, os.Stdout)
		return
	}
	listener, err := net.Listen("tcp", address)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	for {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		serve(conn, conn)
		_ = conn.Close()
	}
}
