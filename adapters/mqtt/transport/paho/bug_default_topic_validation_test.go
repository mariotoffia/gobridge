package paho

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// (MEDIUM): a sender's default_topic is used verbatim as the PUBLISH topic
// when an outbound message carries no Address, bypassing the runtime
// AddressValidator. A wildcard, $share-prefixed or malformed default_topic would then
// only fail at first publish — as a broker DISCONNECT that tears down the shared
// session. The factory must fail closed at build time.
//
// Mutation killed: remove the ValidateMQTTTopic(default_topic) guard in
// (*Factory).NewSender → the wildcard case builds without error and this test
// fails.
// ═══════════════════════════════════════════════════════════════════════════
func TestFactory_NewSender_ValidatesDefaultTopic(t *testing.T) {
	f := &Factory{}
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://broker:1883"},
		ClientID:   "a6",
	}, connectivity.SessionEphemeral, nil)
	defer sess.Router().shutdown()

	reject := []struct {
		name  string
		topic string
	}{
		{"multi-level wildcard", "sensors/#"},
		{"single-level wildcard", "sensors/+/temp"},
		{"shared-subscription prefix", "$share/group/x"},
		{"embedded null", "sensors/\x00/temp"},
	}
	for _, tc := range reject {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			spec := ports.SenderSpec{
				ID:     "snd",
				Config: Config{Sender: SenderOptions{QoS: 1, DefaultTopic: tc.topic}},
			}
			_, err := f.NewSender(context.Background(), spec, sess)
			require.Error(t, err, "an invalid default_topic must be rejected at build time")
			require.ErrorIs(t, err, shared.ErrInvalidConfig)
		})
	}

	t.Run("accepts a valid concrete topic", func(t *testing.T) {
		spec := ports.SenderSpec{
			ID:     "snd",
			Config: Config{Sender: SenderOptions{QoS: 1, DefaultTopic: "sensors/archive/x"}},
		}
		_, err := f.NewSender(context.Background(), spec, sess)
		require.NoError(t, err, "a valid default_topic must build")
	})

	t.Run("accepts an empty default_topic (no fallback)", func(t *testing.T) {
		spec := ports.SenderSpec{
			ID:     "snd",
			Config: Config{Sender: SenderOptions{QoS: 1}},
		}
		_, err := f.NewSender(context.Background(), spec, sess)
		require.NoError(t, err, "an empty default_topic is legal — the fallback is simply unset")
	})
}
