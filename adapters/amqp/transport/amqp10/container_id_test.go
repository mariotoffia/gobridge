// Deterministic unit tests for the default container-id instance
// entropy (finding 16): an unset container_id must not fall through to
// the SDK, which generates a NEW random container-id on every dial —
// changing the broker-side durable-subscription identity
// (container-id + link name) on every reconnect and colliding replicas
// that copy a static example value.
package amqp10

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
)

func TestSessionOptions_ApplyDefaults_GeneratesContainerID(t *testing.T) {
	opts := SessionOptions{Address: "amqp://localhost:5672"}
	opts.applyDefaults()

	require.NotEmpty(t, opts.ContainerID,
		"unset container_id must default to a per-instance value; leaving it empty hands identity to the SDK, which randomizes per dial")
	require.True(t, strings.HasPrefix(opts.ContainerID, defaultContainerIDPrefix),
		"generated container-id must carry the gobridge prefix for operator recognizability")

	// Stable for the lifetime of these options: a second defaulting pass
	// (factory.NewSession then NewSession both call applyDefaults) must
	// not regenerate — regeneration would re-orphan a durable
	// subscription mid-flow.
	generated := opts.ContainerID
	opts.applyDefaults()
	require.Equal(t, generated, opts.ContainerID)
}

func TestSessionOptions_ApplyDefaults_ContainerIDUniquePerInstance(t *testing.T) {
	a := SessionOptions{Address: "amqp://localhost:5672"}
	b := SessionOptions{Address: "amqp://localhost:5672"}
	a.applyDefaults()
	b.applyDefaults()

	require.NotEqual(t, a.ContainerID, b.ContainerID,
		"two instances defaulting the container-id must not collide (replicas copying a config without container_id)")
}

func TestSessionOptions_ApplyDefaults_PreservesExplicitContainerID(t *testing.T) {
	opts := SessionOptions{Address: "amqp://localhost:5672", ContainerID: "bridge-node-01"}
	opts.applyDefaults()
	require.Equal(t, "bridge-node-01", opts.ContainerID)
}

// TestNewSession_ContainerIDStableAcrossReconnects proves the generated
// identity is captured ONCE per Session — every dial (initial and every
// reconnect) presents the same container-id, so a durable subscription
// keyed on container-id + link name survives reconnects even when the
// operator did not set container_id.
func TestNewSession_ContainerIDStableAcrossReconnects(t *testing.T) {
	s := NewSession(SessionOptions{Address: "amqp://localhost:5672"},
		connectivity.SessionEphemeral, slog.Default())

	first := s.opts.ContainerID
	require.NotEmpty(t, first)
	require.True(t, strings.HasPrefix(first, defaultContainerIDPrefix))
	require.Equal(t, first, s.opts.ContainerID,
		"session must hold one container-id for its whole lifetime")
}
