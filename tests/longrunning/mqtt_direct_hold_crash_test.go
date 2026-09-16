//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/tests/testutil/prodid"
	"github.com/mariotoffia/gobridge/testutil/flocilocal"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

func TestMQTTDirectHoldCrashRecovery(t *testing.T) {
	t.Cleanup(mqttlocal.Shutdown)
	t.Cleanup(flocilocal.Shutdown)
	brokerURL := mqttlocal.BrokerURL(t)
	queue, _ := setupSQSQueue(t, "mqtt-crash")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	for _, name := range []string{"managed", "dlq"} {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, name), 0o700))
	}
	clientID := mqttlocal.UniqueClientID("mqtt-crash")
	topic := mqttlocal.UniqueClientID("mqtt-crash-topic")
	configPath := filepath.Join(dir, "bridge.yaml")
	document := fmt.Sprintf(`
bridge: {id: mqtt-crash}
stores:
  managed_subscriptions:
    type: sqlite
    options: {path: %q}
  dlq:
    type: sqlite
    options: {path: %q}
sessions:
  - id: ingress
    transport: mqtt
    session_mode: persistent
    options:
      session:
        broker_url: %q
        client_id: %q
        clean_start: false
        session_expiry_interval: 600
        connect_timeout: 5s
        reconcile_timeout: 5s
receivers:
  - id: receiver
    session_id: ingress
    topics:
      - {topic: %q, qos: 1}
      - {topic: %q, qos: 0}
senders:
  - id: sender
    transport: sqs
    options:
      queue_url: %q
      endpoint: %q
      region: us-west-1
bindings:
  - {id: destination, sender_id: sender, address: %q}
routes:
  - id: mixed
    receiver_id: receiver
    delivery_mode: direct_hold
    bindings: [destination]
    policy: {allow_unfenced: true, max_in_flight: 2, send_timeout: 90s}
`, filepath.Join(dir, "managed", "history.db"), filepath.Join(dir, "dlq", "messages.db"),
		brokerURL, clientID, topic+"/alarms", topic+"/readings", queue, flocilocal.Endpoint(t), queue)
	require.NoError(t, os.WriteFile(configPath, []byte(document), 0o600))
	require.NoError(t, mqttCrashBuilder(t, configPath, false).
		SeedManagedSubscriptionBaselines(ctx, map[string][]string{"ingress": {}}))

	env := map[string]string{
		mqttCrashChildEnv: "1", mqttCrashConfigEnv: configPath, mqttCrashHoldEnv: "1",
		"FLOCI_ENDPOINT": flocilocal.Endpoint(t), "MQTT_BROKER_URL": brokerURL,
	}
	child := startNodeProcess(t, "held MQTT bridge", "TestMQTTDirectHoldCrashChild", env,
		"MQTT_READY", "MQTT_HELD:", "MQTT_ACCEPTED:", "MQTT_TERMINAL:")
	child.awaitToken(t, "MQTT_READY", 30*time.Second)

	publisher := newMQTTSessionWithBroker(t, brokerURL, mqttlocal.UniqueClientID("crash-publisher"), connectivity.SessionEphemeral, 4)
	require.NoError(t, publisher.Start(ctx))
	publish := func(id string, qos byte, suffix string) {
		t.Helper()
		sender := paho.NewSender(publisher, paho.SenderOptions{QoS: qos, Timeout: 5 * time.Second})
		require.NoError(t, sender.Send(ctx, ports.OutboundMessage{
			Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{ID: id, Payload: []byte(id)}),
			Address:  topic + suffix,
		}))
	}
	publish("held-alarm", 1, "/alarms")
	publish("held-reading", 0, "/readings")
	// Receipt precedes target acceptance. Only SIGKILL and a new child with
	// the same durable identity can prove recovery across process death.
	child.awaitToken(t, "MQTT_HELD:held-alarm", 15*time.Second)
	child.awaitToken(t, "MQTT_HELD:held-reading", 15*time.Second)
	assert.NotContains(t, child.capturedOutput(), "MQTT_ACCEPTED:")
	assert.NotContains(t, child.capturedOutput(), "MQTT_TERMINAL:")
	require.NoError(t, ctx.Err(), "crash/restart must finish within the original 600-second expiry")
	child.kill(t)

	env[mqttCrashHoldEnv] = "0"
	restarted := startNodeProcess(t, "resumed MQTT bridge", "TestMQTTDirectHoldCrashChild", env,
		"MQTT_READY", "MQTT_HELD:", "MQTT_ACCEPTED:", "MQTT_TERMINAL:")
	restarted.awaitToken(t, "MQTT_ACCEPTED:held-alarm", 30*time.Second)
	restarted.awaitToken(t, "MQTT_READY", 30*time.Second)
	publish("later-alarm", 1, "/alarms")
	publish("later-reading", 0, "/readings")
	restarted.awaitToken(t, "MQTT_ACCEPTED:later-alarm", 15*time.Second)
	restarted.awaitToken(t, "MQTT_ACCEPTED:later-reading", 15*time.Second)
	require.NoError(t, ctx.Err())

	accountant, err := prodid.New([]string{"held-alarm", "held-reading", "later-alarm", "later-reading"}, false)
	require.NoError(t, err)
	receiver := newSQSReceiver(t, queue)
	receiveCtx, stopReceive := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- receiver.Run(receiveCtx, func(deliveryCtx context.Context, delivery ports.Delivery) error {
			accountant.ObserveOutput(string(delivery.Envelope().Payload()), delivery.Envelope().ID())
			return delivery.Ack(deliveryCtx)
		})
	}()
	t.Cleanup(func() {
		stopReceive()
		result := wait.RequireReceive(t, done, 10*time.Second)
		if result != nil {
			assert.ErrorIs(t, result, context.Canceled)
		}
	})
	wait.Until(t, 20*time.Second, "durable and post-restart producer IDs reached SQS", func() bool {
		for _, id := range accountant.Reconcile().Missing {
			if id != "held-reading" {
				return false
			}
		}
		return true
	})
	report := accountant.Reconcile()
	assert.Empty(t, report.Unexpected)
	assert.Empty(t, report.IdentityCollisions)
	assert.Empty(t, report.DLQ)
	assert.Empty(t, report.IntentionallyDropped)
	for _, missing := range report.Missing {
		assert.Equal(t, "held-reading", missing, "only pre-crash QoS 0 may be missing")
	}
	assert.False(t, strings.Contains(restarted.capturedOutput(), "MQTT_TERMINAL:"))
	t.Logf("producer accounting after SIGKILL: %s", report.String())
}
