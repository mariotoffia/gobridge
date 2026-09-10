package sqs

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// TestSenderFactory_MaxMessageBytes_ReachesEffectiveCeiling pins the
// config-driven ceiling. WithMaxMessageBytes is a Go-only functional option
// with ZERO non-test call sites, so the production path
// SenderFactory.NewSender -> toSenderConfig -> NewSender once ignored it
// entirely and maxMessageBytes was always the hardcoded default. An operator
// whose queue's MaximumMessageSize differs from the service default must reach
// the sender's effective ceiling from YAML (max_message_bytes); an absent/0
// value must keep the default and must NOT zero the ceiling (which would drop
// ALL egress attributes, including the rank-0 idempotency-key / traceparent
// headers).
//
// The test drives the real factory wiring end to end and reads the sender's
// effective ceiling (s.maxMessageBytes, same-package access) — the exact value
// buildAttributes enforces.
func TestSenderFactory_MaxMessageBytes_ReachesEffectiveCeiling(t *testing.T) {
	t.Parallel()

	build := func(t *testing.T, cfg Config) *Sender {
		t.Helper()
		f := NewSenderFactory(nil)
		sender, err := f.NewSender(context.Background(), ports.SenderSpec{
			ID:     "s1",
			Config: cfg,
		}, nil)
		require.NoError(t, err)
		s, ok := sender.(*Sender)
		require.True(t, ok, "expected *Sender")
		return s
	}

	t.Run("explicit YAML value reaches the effective ceiling", func(t *testing.T) {
		t.Parallel()
		const lowered = 262_144 // 256 KiB — a queue provisioned below the 1 MiB default
		s := build(t, Config{
			QueueURL:        "https://sqs.us-west-1.amazonaws.com/123/test",
			MaxMessageBytes: lowered,
		})
		require.Equal(t, lowered, s.maxMessageBytes,
			"max_message_bytes from config must reach the sender's effective ceiling")
	})

	t.Run("absent/0 keeps the 1 MiB default and never zeroes the ceiling", func(t *testing.T) {
		t.Parallel()
		s := build(t, Config{
			QueueURL: "https://sqs.us-west-1.amazonaws.com/123/test",
			// MaxMessageBytes omitted -> 0
		})
		require.Equal(t, sqsMaxMessageBytes, s.maxMessageBytes,
			"absent/0 max_message_bytes must keep the default ceiling, not zero it")
		require.Equal(t, 1048576, s.maxMessageBytes,
			"the default ceiling is the service's own default MaximumMessageSize, 1 MiB (1048576)")
	})
}
