package sqs

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/ports"
)

// TestSenderFactory_MaxMessageBytes_ReachesEffectiveCeiling is the regression
// for Finding 4's config-driven gap. WithMaxMessageBytes is a Go-only
// functional option with ZERO non-test call sites, so the production path
// SenderFactory.NewSender -> toSenderConfig -> NewSender never lifted the
// ceiling: maxMessageBytes was always the hardcoded 256 KiB default. An
// operator who raises a queue's MaximumMessageSize via YAML (max_message_bytes)
// must reach the sender's effective ceiling; an absent/0 value must keep the
// 256 KiB default and must NOT zero the ceiling (which would drop ALL egress
// attributes, including the rank-0 idempotency-key / traceparent headers).
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
		const raised = 1_048_576 // 1 MiB — a queue with a raised MaximumMessageSize
		s := build(t, Config{
			QueueURL:        "https://sqs.us-west-1.amazonaws.com/123/test",
			MaxMessageBytes: raised,
		})
		require.Equal(t, raised, s.maxMessageBytes,
			"max_message_bytes from config must reach the sender's effective ceiling (Finding 4)")
	})

	t.Run("absent/0 keeps the 256 KiB default and never zeroes the ceiling", func(t *testing.T) {
		t.Parallel()
		s := build(t, Config{
			QueueURL: "https://sqs.us-west-1.amazonaws.com/123/test",
			// MaxMessageBytes omitted -> 0
		})
		require.Equal(t, sqsMaxMessageBytes, s.maxMessageBytes,
			"absent/0 max_message_bytes must keep the default ceiling, not zero it")
		require.Equal(t, 262144, s.maxMessageBytes,
			"the default ceiling is 256 KiB (262144)")
	})
}
