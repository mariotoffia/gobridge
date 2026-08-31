package paho

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// A negative duration is accepted by every typed configuration entry point
// that does not check the sign, and only fails once the session dials: a
// negative connect_timeout produces an already-expired context, so the session
// can never start while the configuration looks valid. The effective
// Config.Validate is the single gate every entry point passes through, so the
// sign check belongs there.

// TestConfigValidate_RejectsNegativeDurations pins the sign check on every
// configurable duration.
func TestConfigValidate_RejectsNegativeDurations(t *testing.T) {
	cases := map[string]func(*Config){
		"connect_timeout":      func(c *Config) { c.Session.ConnectTimeout = -time.Second },
		"reconnect_timeout":    func(c *Config) { c.Session.ReconnectTimeout = -time.Second },
		"reconcile_timeout":    func(c *Config) { c.Session.ReconcileTimeout = -time.Second },
		"reconnect_delay":      func(c *Config) { c.Session.ReconnectDelay = -time.Second },
		"reconnect_max_delay":  func(c *Config) { c.Session.ReconnectMaxDelay = -time.Second },
		"unmatched_grace":      func(c *Config) { c.Session.UnmatchedGrace = -time.Second },
		"sender.timeout":       func(c *Config) { c.Sender.Timeout = -time.Second },
		"throttle_retry_after": func(c *Config) { c.Sender.ThrottleRetryAfter = -time.Second },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Session.BrokerURLs = []string{"tcp://192.0.2.1:1883"}
			cfg.Session.ClientID = "negative-duration"
			mutate(&cfg)

			require.ErrorIs(t, cfg.Validate(), shared.ErrInvalidConfig)
		})
	}
}

// TestFactoryNewSession_RejectsNegativeConnectTimeout pins that the build
// boundary refuses the configuration instead of constructing a session whose
// every connect attempt expires before it is made.
func TestFactoryNewSession_RejectsNegativeConnectTimeout(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Session.BrokerURLs = []string{"tcp://192.0.2.1:1883"}
	cfg.Session.ClientID = "negative-connect-timeout"
	cfg.Session.ConnectTimeout = -time.Second

	f := &Factory{}
	_, err := f.NewSession(context.Background(), ports.SessionSpec{ID: "s1", Config: &cfg})

	require.ErrorIs(t, err, shared.ErrInvalidConfig)
}

// TestSessionConfig_ConnectTimeoutCoercesNonPositive pins the dial-side
// defense-in-depth for a hand-built SessionOptions that never passed
// Config.Validate: a non-positive value falls back to the documented default
// rather than producing an already-expired await context.
func TestSessionConfig_ConnectTimeoutCoercesNonPositive(t *testing.T) {
	for _, configured := range []time.Duration{0, -time.Second} {
		s := NewSession(SessionOptions{
			BrokerURLs:     []string{"tcp://192.0.2.1:1883"},
			ClientID:       "coerce-connect-timeout",
			ConnectTimeout: configured,
		}, connectivity.SessionEphemeral, nil)

		require.Equal(t, DefaultConnectTimeout, s.connectTimeout(),
			"connect_timeout %v must coerce to the default", configured)
	}
}

// TestConfigValidate_AcceptsZeroAndPositiveDurations guards the rejection from
// being over-broad: zero means "use the documented default" on every one of
// these fields and must stay valid.
func TestConfigValidate_AcceptsZeroAndPositiveDurations(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Session.BrokerURLs = []string{"tcp://192.0.2.1:1883"}
	cfg.Session.ClientID = "zero-durations"
	cfg.Session.ConnectTimeout = 0
	cfg.Session.ReconnectTimeout = 0
	cfg.Session.ReconcileTimeout = 0
	cfg.Session.ReconnectDelay = 0
	cfg.Session.ReconnectMaxDelay = 0
	cfg.Session.UnmatchedGrace = 0
	cfg.Sender.Timeout = 0
	cfg.Sender.ThrottleRetryAfter = 0

	require.NoError(t, cfg.Validate())
}
