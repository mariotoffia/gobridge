// ═══════════════════════════════════════════════
// Production-readiness remediation tests: bare-int duration guard.
//
// A YAML/JSON decoder that bypasses the bridge's strict root parser
// (direct yaml.Unmarshal into Config, a programmatic spec, an embedder's
// own decode) turns a bare number into NANOSECONDS when the target is
// time.Duration: `heartbeat: 30` becomes 30ns — a nonsensical value that
// previously sailed through validation and produced a connection that
// heartbeats/times out at nanosecond scale. No configured duration has a
// legitimate sub-millisecond value, so Validate rejects non-zero
// durations below 1ms with a message that names the offending key and
// the unit requirement. Zero stays allowed (= "use the default").
//
// The error message IS the deliverable of this fix (the operator must be
// told which key and why), so these tests assert on its content — the
// TESTS.md "no err.Error() comparison" rule targets error identity, and
// these are plain validation errors with no Code/Class to assert.
//
// ═══════════════════════════════════════════════
package amqp091

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// TestConfig_Validate_SubMillisecondDurationRejected pins the floor for
// every duration key reachable through the typed Config: a value that
// looks like a bare-int decode accident (30 → 30ns) fails validation
// naming the key and the 1ms unit floor.
func TestConfig_Validate_SubMillisecondDurationRejected(t *testing.T) {
	cases := []struct {
		key string
		cfg Config
	}{
		{"session.heartbeat", Config{Session: SessionOptions{Heartbeat: 30}}},
		{"session.connect_timeout", Config{Session: SessionOptions{ConnectTimeout: 30}}},
		{"session.reconnect_delay", Config{Session: SessionOptions{ReconnectDelay: 30}}},
		{"session.reconnect_max_delay", Config{Session: SessionOptions{ReconnectMaxDelay: 30}}},
		{"sender.timeout", Config{Sender: SenderParams{Timeout: 30}}},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			err := tc.cfg.Validate()
			require.Error(t, err, "30ns (bare-int 30) must fail validation")
			require.ErrorContains(t, err, tc.key,
				"message must name the offending config key")
			require.ErrorContains(t, err, "1ms",
				"message must name the unit floor")
			require.ErrorContains(t, err, "nanoseconds",
				"message must explain the bare-number-decodes-as-nanoseconds trap")
		})
	}
}

// TestConfig_Validate_NegativeDurationRejected — negative durations are
// below the floor too; they must not slip through as "not sub-ms".
func TestConfig_Validate_NegativeDurationRejected(t *testing.T) {
	c := Config{Session: SessionOptions{Heartbeat: -time.Second}}
	require.Error(t, c.Validate())
}

// TestConfig_Validate_ZeroAndMillisecondPlusDurationsAccepted — zero
// means "use the default" and anything >= 1ms is a deliberate setting.
func TestConfig_Validate_ZeroAndMillisecondPlusDurationsAccepted(t *testing.T) {
	require.NoError(t, Config{}.Validate(), "all-zero durations must validate")

	ok := Config{
		Session: SessionOptions{
			Heartbeat:         time.Millisecond,
			ConnectTimeout:    30 * time.Second,
			ReconnectDelay:    time.Second,
			ReconnectMaxDelay: 30 * time.Second,
		},
		Sender: SenderParams{Timeout: 5 * time.Second},
	}
	require.NoError(t, ok.Validate())
}

// TestFactory_NewSession_SubMillisecondDurationRejected — defense in
// depth: a programmatic ports.SessionSpec bypasses the config decoder's
// Validate, so the managed factory must re-reject the bare-int accident
// before dialing with a 30ns heartbeat.
func TestFactory_NewSession_SubMillisecondDurationRejected(t *testing.T) {
	f := NewFactory(nil)
	_, err := f.NewSession(context.Background(), ports.SessionSpec{
		ID: "s1",
		Config: &Config{Session: SessionOptions{
			BrokerURL: "amqp://broker.example:5672/",
			Heartbeat: 30, // 30ns — bare-int decode accident
		}},
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "session.heartbeat")
}

// TestSenderFactory_NewSender_SubMillisecondTimeoutRejected — same
// defense-in-depth boundary for the sender role.
func TestSenderFactory_NewSender_SubMillisecondTimeoutRejected(t *testing.T) {
	sf := NewSenderFactory(nil)
	sess := NewSession(SessionOptions{BrokerURL: "amqp://broker.example:5672/"}, connectivity.SessionEphemeral, nil)
	t.Cleanup(func() { _ = sess.Close(context.Background()) })

	_, err := sf.NewSender(context.Background(), ports.SenderSpec{
		ID: "snd1",
		Config: &Config{Sender: SenderParams{
			Exchange:  "x",
			Mandatory: true, // pass the managed-route mandatory gate so the timeout floor is reached
			Timeout:   30,   // 30ns
		}},
	}, sess)
	require.Error(t, err)
	require.ErrorContains(t, err, "sender.timeout")
}
