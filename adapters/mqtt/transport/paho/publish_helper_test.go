package paho

import (
	"testing"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
)

// mustPublishFromEnvelope builds an egress publish and fails the test if
// construction is rejected. Tests that assert on the CONTENT of a valid packet
// use it so a wire-limit rejection surfaces as a named failure rather than a
// nil-pointer panic further down; the rejection paths themselves call
// PublishFromEnvelope directly and assert on the error.
func mustPublishFromEnvelope(
	tb testing.TB,
	env *messaging.Envelope,
	topic string,
	opts SenderOptions,
	clk clock.Clock,
	metrics ...ports.MetricsExporter,
) *pahov5.Publish {
	tb.Helper()
	pub, err := PublishFromEnvelope(env, topic, opts, clk, metrics...)
	require.NoError(tb, err)
	return pub
}
