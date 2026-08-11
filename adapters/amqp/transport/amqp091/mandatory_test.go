// ═══════════════════════════════════════════════
// Production-readiness remediation tests: mandatory-publish safety
// (c5-mandatory) and metrics threading (c5-metrics-dropped).
//
// c5-mandatory: with mandatory=false the broker CONFIRMS an unroutable
// publish and then silently DISCARDS it, so the bridge acks the source and
// the message is lost with zero telemetry. The managed factory therefore
// refuses to build a sender unless it is mandatory=true OR the operator has
// explicitly opted into the loss via allow_unroutable_drop.
//
// c5-metrics-dropped: NewFactory stored the exporter but the sub-factories
// only received the logger, so every managed sender/receiver metric went to
// a NoopExporter in production. The factory now threads the exporter through
// to the built SenderConfig/ReceiverConfig.
// ═══════════════════════════════════════════════
package amqp091

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

func mandatoryTestSession(t *testing.T) *Session {
	t.Helper()
	sess := NewSession(SessionOptions{BrokerURL: "amqp://localhost/"}, connectivity.SessionEphemeral, slog.Default())
	t.Cleanup(func() { _ = sess.Close(context.Background()) })
	return sess
}

// TestSenderFactory_NewSender_RejectsNonMandatoryDefault proves the managed
// factory refuses a sender that would silently drop unroutable publishes
// (mandatory=false, no allow_unroutable_drop opt-in). Mutation: revert the
// gate in SenderFactory.NewSender and the build succeeds → this fails.
func TestSenderFactory_NewSender_RejectsNonMandatoryDefault(t *testing.T) {
	sf := NewSenderFactory(slog.Default())
	sess := mandatoryTestSession(t)

	_, err := sf.NewSender(context.Background(), ports.SenderSpec{
		ID:     "snd-default",
		Config: Config{Sender: SenderParams{Exchange: "x", RoutingKey: "rk"}},
	}, sess)

	require.Error(t, err, "mandatory=false with no opt-in must be rejected")
	var be *shared.BridgeError
	require.True(t, errors.As(err, &be))
	require.Equal(t, shared.ErrCodeInvalidPayload, be.Code)
	require.ErrorContains(t, err, "mandatory")
	require.ErrorContains(t, err, "allow_unroutable_drop")
}

// TestSenderFactory_NewSender_AcceptsMandatory proves mandatory=true builds a
// sender that returns unroutable publishes (Mandatory threaded to the config).
func TestSenderFactory_NewSender_AcceptsMandatory(t *testing.T) {
	sf := NewSenderFactory(slog.Default())
	sess := mandatoryTestSession(t)

	s, err := sf.NewSender(context.Background(), ports.SenderSpec{
		ID:     "snd-mandatory",
		Config: Config{Sender: SenderParams{Exchange: "x", RoutingKey: "rk", Mandatory: true}},
	}, sess)

	require.NoError(t, err)
	require.True(t, s.(*Sender).cfg.Mandatory)
}

// TestSenderFactory_NewSender_AcceptsAllowUnroutableDrop proves the explicit
// opt-in lets a non-mandatory sender build (deliberately accepting the loss),
// and that the publish itself is still non-mandatory.
func TestSenderFactory_NewSender_AcceptsAllowUnroutableDrop(t *testing.T) {
	sf := NewSenderFactory(slog.Default())
	sess := mandatoryTestSession(t)

	s, err := sf.NewSender(context.Background(), ports.SenderSpec{
		ID: "snd-drop",
		Config: Config{Sender: SenderParams{
			Exchange:            "x",
			RoutingKey:          "rk",
			AllowUnroutableDrop: true,
		}},
	}, sess)

	require.NoError(t, err)
	require.False(t, s.(*Sender).cfg.Mandatory,
		"allow_unroutable_drop must not silently flip the publish to mandatory")
}

// TestFactory_ThreadsMetricsToSender proves the factory's exporter reaches the
// built managed sender (not a NoopExporter). Mutation: drop f.metrics from
// SenderConfig in SenderFactory.NewSender and the sender falls back to
// NoopExporter → the identity assertion fails.
func TestFactory_ThreadsMetricsToSender(t *testing.T) {
	rec := &ports.RecordingExporter{}
	f := NewFactory(slog.Default(), rec)
	sess := mandatoryTestSession(t)

	s, err := f.NewSender(context.Background(), ports.SenderSpec{
		ID:     "snd-metrics",
		Config: Config{Sender: SenderParams{Exchange: "x", RoutingKey: "rk", Mandatory: true}},
	}, sess)
	require.NoError(t, err)

	require.True(t, s.(*Sender).metrics == ports.MetricsExporter(rec),
		"managed sender must export via the factory's exporter, not a NoopExporter")
}

// TestFactory_ThreadsMetricsToReceiver proves the factory's exporter reaches
// the built managed receiver. Mutation mirror of the sender test.
func TestFactory_ThreadsMetricsToReceiver(t *testing.T) {
	rec := &ports.RecordingExporter{}
	f := NewFactory(slog.Default(), rec)
	sess := mandatoryTestSession(t)

	r, err := f.NewReceiver(context.Background(), ports.ReceiverSpec{
		ID:     "rcv-metrics",
		Config: Config{Receiver: ReceiverParams{QueueName: "q"}},
	}, sess)
	require.NoError(t, err)

	require.True(t, r.(*Receiver).metrics == ports.MetricsExporter(rec),
		"managed receiver must export via the factory's exporter, not a NoopExporter")
}

// TestFactory_NoMetrics_FallsBackToNoop proves the optional-exporter contract:
// a factory built without an exporter still yields a working (Noop) sender —
// the variadic threading must not regress direct/no-metrics construction.
func TestFactory_NoMetrics_FallsBackToNoop(t *testing.T) {
	f := NewFactory(slog.Default())
	sess := mandatoryTestSession(t)

	s, err := f.NewSender(context.Background(), ports.SenderSpec{
		ID:     "snd-noop",
		Config: Config{Sender: SenderParams{Exchange: "x", RoutingKey: "rk", Mandatory: true}},
	}, sess)
	require.NoError(t, err)
	require.NotNil(t, s.(*Sender).metrics)
	_, isNoop := s.(*Sender).metrics.(*ports.NoopExporter)
	require.True(t, isNoop, "no factory exporter must fall back to the NoopExporter")
}
