// Validates HIGH-5: outbound QoS 1/2 packet state is IN-MEMORY (autopaho), so
// it is not durable across process death. The adapter surfaces this once per
// session when a QoS 1/2 sender is built, so operators do not mistake MQTT QoS
// for durable egress (durable egress is the bridge's shared_outbox /
// idempotent-replay responsibility). QoS 0 carries no such expectation and is
// not warned.
package paho

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

const outboxAdvisorySubstr = "IN-MEMORY"

// TestBug_NewSender_QoS12_WarnsOutboxDurabilityOnce proves HIGH-5: building a
// QoS 1/2 sender emits the egress-durability advisory exactly once per session
// (deduped across senders). Without the fix no advisory is emitted and the
// count assertion fails.
func TestBug_NewSender_QoS12_WarnsOutboxDurabilityOnce(t *testing.T) {
	logs := &recordingLogHandler{}
	f := &Factory{Logger: slog.New(logs)}
	sess := NewSession(SessionOptions{
		BrokerURLs:            []string{"tcp://broker:1883"},
		ClientID:              "outbox-warn",
		SessionExpiryInterval: 3600,
	}, connectivity.SessionPersistent, slog.New(logs))

	// First QoS 2 sender -> advisory fires once.
	_, err := f.NewSender(context.Background(), ports.SenderSpec{
		ID:     "snd-qos2",
		Config: Config{Sender: SenderOptions{QoS: 2}},
	}, sess)
	require.NoError(t, err)
	require.Equal(t, 1, logs.warnCountContaining(outboxAdvisorySubstr),
		"HIGH-5: a QoS 2 sender must warn that egress packet state is not durable across process death")

	// A second QoS 1 sender on the SAME session -> deduped, still exactly 1.
	_, err = f.NewSender(context.Background(), ports.SenderSpec{
		ID:     "snd-qos1",
		Config: Config{Sender: SenderOptions{QoS: 1}},
	}, sess)
	require.NoError(t, err)
	require.Equal(t, 1, logs.warnCountContaining(outboxAdvisorySubstr),
		"the advisory must dedupe per session, not fire once per sender")
}

// TestBug_NewSender_QoS0_NoOutboxWarn pins that QoS 0 senders — best-effort by
// protocol — do NOT trigger the durability advisory, keeping it noise-free.
func TestBug_NewSender_QoS0_NoOutboxWarn(t *testing.T) {
	logs := &recordingLogHandler{}
	f := &Factory{Logger: slog.New(logs)}
	sess := NewSession(SessionOptions{
		BrokerURLs:            []string{"tcp://broker:1883"},
		ClientID:              "outbox-qos0",
		SessionExpiryInterval: 3600,
	}, connectivity.SessionPersistent, slog.New(logs))

	_, err := f.NewSender(context.Background(), ports.SenderSpec{
		ID:     "snd-qos0",
		Config: Config{Sender: SenderOptions{QoS: 0}},
	}, sess)
	require.NoError(t, err)
	require.Equal(t, 0, logs.warnCountContaining(outboxAdvisorySubstr),
		"QoS 0 is best-effort by protocol; it must not emit the egress-durability advisory")
}
