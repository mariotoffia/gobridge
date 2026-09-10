package parser

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// backoffJitterYAML renders a minimal blueprint whose single route carries the
// given backoff block, so the tri-state can be asserted through the real
// decoder rather than a struct literal.
func backoffJitterYAML(backoff string) string {
	return `
bridge:
  id: bridge-1

receivers:
  - id: rx1
    transport: http

senders:
  - id: tx1
    transport: http

bindings:
  - id: b1
    sender_id: tx1
    address: /events

routes:
  - id: r1
    receiver_id: rx1
    bindings: [b1]
    policy:
      backoff:
` + backoff
}

// TestParse_BackoffJitterTriState proves the wire boundary keeps "omitted" and
// "explicitly zero" apart. The distinction is the whole point of the field:
// omitted takes the recommended de-correlation default, while an explicit 0 is
// an operator asking for deterministic backoff. A plain float64 collapsed both
// to zero, which is what left every config-loaded route un-jittered.
func TestParse_BackoffJitterTriState(t *testing.T) {
	t.Run("omitted", func(t *testing.T) {
		cfg, err := Parse(strings.NewReader(backoffJitterYAML("        initial_interval: 1s\n")),
			FormatYAML, passthroughRegistry("http"))
		require.NoError(t, err)
		require.Nil(t, cfg.Routes[0].Policy.Backoff.Jitter,
			"an omitted jitter must stay nil so the recommended default applies")
	})

	t.Run("explicit zero", func(t *testing.T) {
		cfg, err := Parse(strings.NewReader(backoffJitterYAML("        jitter: 0\n")),
			FormatYAML, passthroughRegistry("http"))
		require.NoError(t, err)
		require.NotNil(t, cfg.Routes[0].Policy.Backoff.Jitter,
			"jitter: 0 is an explicit opt-out and must survive decoding as a set value")
		require.Equal(t, 0.0, *cfg.Routes[0].Policy.Backoff.Jitter)
	})

	t.Run("explicit fraction", func(t *testing.T) {
		cfg, err := Parse(strings.NewReader(backoffJitterYAML("        jitter: 0.35\n")),
			FormatYAML, passthroughRegistry("http"))
		require.NoError(t, err)
		require.NotNil(t, cfg.Routes[0].Policy.Backoff.Jitter)
		require.InDelta(t, 0.35, *cfg.Routes[0].Policy.Backoff.Jitter, 1e-9)
	})
}
