package sqs

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// Finding 9 — transport capabilities.
//
// CapDelayedSend is declared (SQS supports DelaySeconds). CapSharedConsumer
// is deliberately OMITTED: declaring it would force fencing on unfenced SQS
// direct_hold routes via runtime/validator.go. SQS is a competing-consumer
// work queue, not a broadcast shared consumer.
func TestFactory_Capabilities(t *testing.T) {
	caps := NewFactory(nil).Capabilities()

	assert.Contains(t, caps, ports.CapVisibilityExtension)
	assert.Contains(t, caps, ports.CapSourceRedelivery)
	assert.Contains(t, caps, ports.CapDelayedSend)
	assert.NotContains(t, caps, ports.CapSharedConsumer,
		"shared_consumer must stay omitted; it would force fencing on SQS direct_hold routes")
}

// Finding 10 — poll-backoff / init-timeout knobs are deployable via plugin config.

func TestConfig_ToReceiverConfig_ThreadsResilienceKnobs(t *testing.T) {
	c := Config{
		QueueURL:              "https://q",
		InitTimeout:           5 * time.Second,
		PollBackoffInitial:    2 * time.Second,
		PollBackoffMax:        45 * time.Second,
		PollBackoffMultiplier: 3.0,
	}
	rc := c.toReceiverConfig()

	assert.Equal(t, 5*time.Second, rc.InitTimeout)
	assert.Equal(t, 2*time.Second, rc.PollBackoffInitial)
	assert.Equal(t, 45*time.Second, rc.PollBackoffMax)
	assert.Equal(t, 3.0, rc.PollBackoffMultiplier)
}

func TestReceiverFactory_ResilienceKnobsPassthrough(t *testing.T) {
	f := NewReceiverFactory(nil)
	spec := ports.ReceiverSpec{
		ID: "r",
		Config: Config{
			QueueURL:              "https://q",
			PollBackoffInitial:    3 * time.Second,
			PollBackoffMax:        33 * time.Second,
			PollBackoffMultiplier: 4,
			InitTimeout:           7 * time.Second,
		},
	}
	recv, err := f.NewReceiver(context.Background(), spec, nil)
	require.NoError(t, err)

	r, ok := recv.(*Receiver)
	require.True(t, ok)
	assert.Equal(t, 3*time.Second, r.cfg.PollBackoffInitial)
	assert.Equal(t, 33*time.Second, r.cfg.PollBackoffMax)
	assert.Equal(t, 4.0, r.cfg.PollBackoffMultiplier)
	assert.Equal(t, 7*time.Second, r.cfg.InitTimeout)
}

func TestConfig_Validate_ResilienceKnobs(t *testing.T) {
	base := Config{QueueURL: "https://q"}
	require.NoError(t, base.Validate())

	t.Run("multiplier_below_one_rejected", func(t *testing.T) {
		c := base
		c.PollBackoffMultiplier = 0.5
		require.Error(t, c.Validate())
	})
	t.Run("multiplier_one_ok", func(t *testing.T) {
		c := base
		c.PollBackoffMultiplier = 1
		require.NoError(t, c.Validate())
	})
	t.Run("max_below_initial_rejected", func(t *testing.T) {
		c := base
		c.PollBackoffInitial = 30 * time.Second
		c.PollBackoffMax = 10 * time.Second
		require.Error(t, c.Validate())
	})
	t.Run("negative_duration_rejected", func(t *testing.T) {
		c := base
		c.PollBackoffInitial = -time.Second
		require.Error(t, c.Validate())
	})
}

// D2-FU2 — visibility_timeout is bounded by the SQS broker limit (0..12h).
func TestConfig_Validate_VisibilityTimeoutBounds(t *testing.T) {
	base := Config{QueueURL: "https://q"}

	t.Run("zero_means_default_ok", func(t *testing.T) {
		c := base
		c.VisibilityTimeout = 0
		require.NoError(t, c.Validate())
	})
	t.Run("max_12h_ok", func(t *testing.T) {
		c := base
		c.VisibilityTimeout = 43200
		require.NoError(t, c.Validate())
	})
	t.Run("above_12h_rejected", func(t *testing.T) {
		c := base
		c.VisibilityTimeout = 43201
		require.Error(t, c.Validate())
	})
	t.Run("negative_rejected", func(t *testing.T) {
		c := base
		c.VisibilityTimeout = -1
		require.Error(t, c.Validate())
	})
}

// Finding 2 — per-route visibility timeout, threaded into the validator.
//
// EffectiveVisibilityTimeout exposes the configured value (default 30s);
// the builder threads it (and AutoExtendEnabled) into the runtime
// validator's SourceVisibilityTimeout instead of the hardcoded
// Factory.VisibilityTimeout().
func TestConfig_EffectiveVisibilityTimeout(t *testing.T) {
	assert.Equal(t, 30*time.Second,
		Config{QueueURL: "https://q"}.EffectiveVisibilityTimeout(),
		"unset visibility falls back to the receiver default")
	assert.Equal(t, 120*time.Second,
		Config{QueueURL: "https://q", VisibilityTimeout: 120}.EffectiveVisibilityTimeout())
}

// AutoExtendEnabled defaults on (nil) and honours an explicit flag,
// mirroring ReceiverConfig.autoExtendEnabled. The validator uses it to
// skip the SendTimeout-vs-window check for auto-renewed windows (D2).
func TestConfig_AutoExtendEnabled(t *testing.T) {
	assert.True(t, Config{QueueURL: "https://q"}.AutoExtendEnabled(),
		"unset auto_extend + unset window (defaults to 30s) is enabled")
	off := false
	assert.False(t, Config{QueueURL: "https://q", AutoExtend: &off}.AutoExtendEnabled())
	on := true
	assert.True(t, Config{QueueURL: "https://q", AutoExtend: &on}.AutoExtendEnabled())

	// Boundary: the runtime starts the renewal goroutine only when the
	// window >= minAutoExtendVisibilitySeconds (2s); see
	// TestAutoExtend_Boundary_Timeout1_Disabled. AutoExtendEnabled must
	// mirror that so the validator does not skip the finite-window check
	// for a window the runtime refuses to renew (D2 boundary regression).
	assert.False(t, Config{QueueURL: "https://q", VisibilityTimeout: 1, AutoExtend: &on}.AutoExtendEnabled(),
		"visibility_timeout=1 is below the auto-extend floor: runtime runs a fixed 1s window, so not effective")
	assert.True(t, Config{QueueURL: "https://q", VisibilityTimeout: 2, AutoExtend: &on}.AutoExtendEnabled(),
		"visibility_timeout=2 meets the auto-extend floor: runtime renews")
}

func TestFactory_VisibilityTimeout_DefaultUnchanged(t *testing.T) {
	// The singleton factory reports the 30s default because it has no
	// per-route config; the per-route value is threaded via the receiver
	// Config's EffectiveVisibilityTimeout (Finding 2 / D2).
	assert.Equal(t, 30*time.Second, NewFactory(nil).VisibilityTimeout())
}
