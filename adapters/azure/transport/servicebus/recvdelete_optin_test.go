package servicebus

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config/parser"
)

// --- c6-recvdelete: at-most-once opt-in gate + loud startup warning --------

// TestReceiverConfig_ReceiveAndDelete_RequiresOptIn pins the programmatic
// gate: ReceiveAndDelete (at-most-once, lossy) is rejected unless
// AllowAtMostOnce is set. PeekLock never needs it.
//
// Mutation: drop the gate in ReceiverConfig.validate and the no-opt-in
// config validates, silently enabling unrecoverable-loss semantics.
func TestReceiverConfig_ReceiveAndDelete_RequiresOptIn(t *testing.T) {
	t.Parallel()

	noOptIn := ReceiverConfig{QueueName: "q", Client: &mockASBClient{}, ReceiveMode: "ReceiveAndDelete"}
	require.Error(t, noOptIn.validate(),
		"ReceiveAndDelete must be rejected without allow_at_most_once")

	optedIn := noOptIn
	optedIn.AllowAtMostOnce = true
	require.NoError(t, optedIn.validate(),
		"ReceiveAndDelete must be accepted with allow_at_most_once")

	peekLock := ReceiverConfig{QueueName: "q", Client: &mockASBClient{}}
	require.NoError(t, peekLock.validate(), "PeekLock never needs the opt-in")
}

// TestConfig_Validate_ReceiveAndDelete_RequiresOptIn pins the parse-time
// plugin gate (fail-fast at config decode), mirroring the programmatic one.
func TestConfig_Validate_ReceiveAndDelete_RequiresOptIn(t *testing.T) {
	t.Parallel()

	cfg := Config{Receiver: ReceiverParams{QueueName: "q", ReceiveMode: "ReceiveAndDelete"}}
	require.Error(t, cfg.Validate(), "ReceiveAndDelete must fail parse-time without allow_at_most_once")

	cfg.Receiver.AllowAtMostOnce = true
	require.NoError(t, cfg.Validate())
}

// TestPluginOptionsDecode_AllowAtMostOnce proves the new config key decodes
// through the REAL registry decoder and unlocks ReceiveAndDelete.
func TestPluginOptionsDecode_AllowAtMostOnce(t *testing.T) {
	t.Parallel()

	input := map[string]any{
		"receiver": map[string]any{
			"queue_name":         "orders",
			"receive_mode":       "ReceiveAndDelete",
			"allow_at_most_once": true,
		},
	}
	var cfg Config
	require.NoError(t, parser.NewRawConfig(input).Decode(&cfg))
	require.True(t, cfg.Receiver.AllowAtMostOnce)
	require.NoError(t, cfg.Validate())
}

// TestNewReceiver_ReceiveAndDelete_EmitsAtMostOnceWarning proves the loud
// startup warning fires whenever ReceiveAndDelete is active, so the lossy
// at-most-once semantics can never be selected silently.
//
// Mutation: remove the unconditional warning and the log is empty of the
// AT-MOST-ONCE notice.
func TestNewReceiver_ReceiveAndDelete_EmitsAtMostOnceWarning(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	_, err := NewReceiver(ReceiverConfig{
		QueueName:       "q",
		ReceiveMode:     "ReceiveAndDelete",
		AllowAtMostOnce: true,
		Client:          &mockASBClient{},
		Logger:          logger,
	}, nil)
	require.NoError(t, err)

	out := buf.String()
	require.Contains(t, out, "AT-MOST-ONCE")
	require.Contains(t, out, "ReceiveAndDelete")

	// PeekLock must NOT emit the at-most-once warning.
	var plBuf bytes.Buffer
	plLogger := slog.New(slog.NewTextHandler(&plBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	_, err = NewReceiver(ReceiverConfig{QueueName: "q", Client: &mockASBClient{}, Logger: plLogger}, nil)
	require.NoError(t, err)
	require.NotContains(t, plBuf.String(), "AT-MOST-ONCE")
}
