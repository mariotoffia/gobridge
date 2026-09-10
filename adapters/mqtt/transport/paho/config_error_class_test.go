package paho

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// A rejected MESSAGE and a rejected CONFIGURATION are different outcomes:
// INVALID_PAYLOAD is the Rejected class a route dead-letters a single message
// under, while INVALID_CONFIG is Permanent and means a human must edit the
// blueprint. Reporting a build-time configuration failure as a payload
// rejection makes automation and metrics attribute a deployment error to
// message traffic.

// TestPlaintextCredentialRejection_UsesConfigClass pins the class of the
// cleartext-credential gate.
func TestPlaintextCredentialRejection_UsesConfigClass(t *testing.T) {
	opts := SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "plaintext",
		Username:   "user",
		Password:   shared.NewSecret("secret"),
	}

	err := opts.validatePlaintextCredentials()

	require.ErrorIs(t, err, shared.ErrInvalidConfig)
	be, ok := shared.AsBridgeError(err)
	require.True(t, ok)
	require.Equal(t, shared.ErrorPermanent, be.Class)
}

// TestFactoryConfigurationRejections_UseConfigClass pins the class of every
// build-time configuration rejection the factory can raise.
func TestFactoryConfigurationRejections_UseConfigClass(t *testing.T) {
	session := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "class",
	}, connectivity.SessionEphemeral, nil)
	t.Cleanup(func() { session.Router().shutdown() })
	f := &Factory{}
	ctx := context.Background()

	missingClientID := DefaultConfig()
	missingClientID.Session.BrokerURLs = []string{"tcp://192.0.2.1:1883"}

	badDefaultTopic := DefaultConfig()
	badDefaultTopic.Sender.DefaultTopic = "devices/+/cmd"

	badQoS := DefaultConfig()
	badQoS.Sender.QoS = 5

	cases := map[string]func() error{
		"session config of the wrong type": func() error {
			_, err := f.NewSession(ctx, ports.SessionSpec{ID: "s"})
			return err
		},
		"session without client_id": func() error {
			_, err := f.NewSession(ctx, ports.SessionSpec{ID: "s", Config: &missingClientID})
			return err
		},
		"receiver on a foreign session": func() error {
			_, err := f.NewReceiver(ctx, ports.ReceiverSpec{ID: "rx"}, nil)
			return err
		},
		"receiver without subscriptions": func() error {
			_, err := f.NewReceiver(ctx, ports.ReceiverSpec{ID: "rx"}, session)
			return err
		},
		"sender on a foreign session": func() error {
			_, err := f.NewSender(ctx, ports.SenderSpec{ID: "tx"}, nil)
			return err
		},
		"sender config of the wrong type": func() error {
			_, err := f.NewSender(ctx, ports.SenderSpec{ID: "tx"}, session)
			return err
		},
		"sender qos out of range": func() error {
			_, err := f.NewSender(ctx, ports.SenderSpec{ID: "tx", Config: &badQoS}, session)
			return err
		},
		"sender default_topic is not publishable": func() error {
			_, err := f.NewSender(ctx, ports.SenderSpec{ID: "tx", Config: &badDefaultTopic}, session)
			return err
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			err := build()
			require.Error(t, err)
			require.ErrorIs(t, err, shared.ErrInvalidConfig)
		})
	}
}
