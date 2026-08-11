package paho

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
)

// ═══════════════════════════════════════════════════════════════════════════
// (MEDIUM): default receive_maximum 65535 = unbounded memory.
//
// paho buffers up to Receive Maximum full publishes under manual
// acknowledgement, and this adapter's startup pending buffer is sized to the
// same value. A slow pipeline with large payloads at the 65535 protocol
// maximum could buffer multiple GiB → OOM.
//
// Fix: NewSession coerces an unset (0) receive_maximum to the byte-budgeted
// DefaultReceiveMaximum (192). 0 is not a legal MQTT v5 Receive Maximum, so
// coercing it is correct.
// ═══════════════════════════════════════════════════════════════════════════

// TestBug_ReceiveMaximumDefault_CoercedAndBoundsPendingBuffer asserts an unset
// receive_maximum is coerced to the lowered default AND that the pending buffer
// is sized to it (bounding worst-case buffered memory).
//
// Counterfactual (proven by disabling the NewSession coercion): pre-fix an
// unset receive_maximum stays 0 and the pending buffer keeps the 65535-entry
// default — the multi-GiB memory ceiling removes.
func TestBug_ReceiveMaximumDefault_CoercedAndBoundsPendingBuffer(t *testing.T) {
	require.Equal(t, uint16(192), DefaultReceiveMaximum,
		"the default is the byte-budgeted 192 ceiling, not the 65535 protocol maximum")

	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "rm-default",
	}, connectivity.SessionEphemeral, nil)

	require.Equal(t, DefaultReceiveMaximum, s.opts.ReceiveMaximum,
		"an unset (0) receive_maximum is coerced to the lowered default")

	r := s.Router()
	r.mu.RLock()
	limit := r.pendingLimit
	r.mu.RUnlock()
	require.Equal(t, int(DefaultReceiveMaximum), limit,
		"the pending buffer is sized to the (coerced) Receive Maximum, bounding memory")
}

// TestBug_ReceiveMaximumExplicit_NotCoerced asserts an explicitly configured
// receive_maximum is honoured verbatim (operators who need a higher ceiling opt
// in), and the pending buffer tracks it.
func TestBug_ReceiveMaximumExplicit_NotCoerced(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs:     []string{"tcp://192.0.2.1:1883"},
		ClientID:       "rm-explicit",
		ReceiveMaximum: 2048,
	}, connectivity.SessionEphemeral, nil)

	require.Equal(t, uint16(2048), s.opts.ReceiveMaximum,
		"an explicit receive_maximum is not coerced")

	r := s.Router()
	r.mu.RLock()
	limit := r.pendingLimit
	r.mu.RUnlock()
	require.Equal(t, 2048, limit, "the pending buffer tracks the explicit Receive Maximum")
}

// TestBug_ReceiveMaximumDefault_WarnsOnCoercion asserts: the
// previously SILENT default coercion now emits a WARN, matching the sibling
// session_expiry coercion in the same constructor. An operator implicitly
// relying on the old 65535 ceiling gets a visible signal that they were capped
// at DefaultReceiveMaximum on upgrade, instead of a silent behaviour change.
//
// Counterfactual (proven by removing the logger.Warn in NewSession's
// receive_maximum coercion): warnCountContaining("receive_maximum") stays 0 —
// the silent cap the finding flags.
func TestBug_ReceiveMaximumDefault_WarnsOnCoercion(t *testing.T) {
	logs := &recordingLogHandler{}
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "rm-warn",
	}, connectivity.SessionEphemeral, slog.New(logs))

	require.Equal(t, DefaultReceiveMaximum, s.opts.ReceiveMaximum,
		"an unset receive_maximum is still coerced to the lowered default (value unchanged)")
	require.Equal(t, 1, logs.warnCountContaining("receive_maximum"),
		"an unset receive_maximum must WARN on coercion (visibility of the lowered default), "+
			"matching the session_expiry sibling")
}

// TestBug_ReceiveMaximumExplicit_NoWarn asserts the coercion WARN fires ONLY on
// the unset path: an explicitly configured receive_maximum is honoured without
// any coercion warning (no false alarm for operators who opted in).
func TestBug_ReceiveMaximumExplicit_NoWarn(t *testing.T) {
	logs := &recordingLogHandler{}
	s := NewSession(SessionOptions{
		BrokerURLs:     []string{"tcp://192.0.2.1:1883"},
		ClientID:       "rm-explicit-nowarn",
		ReceiveMaximum: 4096,
	}, connectivity.SessionEphemeral, slog.New(logs))

	require.Equal(t, uint16(4096), s.opts.ReceiveMaximum)
	require.Equal(t, 0, logs.warnCountContaining("receive_maximum"),
		"an explicit receive_maximum is honoured without a coercion warning")
}
