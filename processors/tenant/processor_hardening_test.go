package tenant

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// decrementFailTracker succeeds on the in-flight increment (+1) and the
// message count but fails the in-flight decrement (-1), isolating the
// deferred-cleanup error path.
type decrementFailTracker struct{}

func (decrementFailTracker) IncrementMessages(context.Context, string, int64) error { return nil }
func (decrementFailTracker) IncrementInFlight(_ context.Context, _ string, delta int64) error {
	if delta < 0 {
		return errors.New("decrement failed")
	}
	return nil
}

func hasTag(tags []shared.Tag, key, value string) bool {
	for _, tg := range tags {
		if tg.Key == key && tg.Value == value {
			return true
		}
	}
	return false
}

// TestNew_ReservedTenantHeader_RejectedAtConstruction is the L6 fail-fast:
// configuring a reserved x-bridge.* source header (which ingress strips) must
// be rejected at construction, not silently resolve no tenant per message.
func TestNew_ReservedTenantHeader_RejectedAtConstruction(t *testing.T) {
	_, err := New(Config{TenantHeader: messaging.HeaderTenantID})
	require.ErrorIs(t, err, ErrTenantHeaderReserved)

	be, ok := shared.AsBridgeError(err)
	require.True(t, ok)
	assert.Equal(t, shared.ErrorPermanent, be.Class)
}

// TestReservedTenantHeader_IsStrippedAtIngress proves root cause: the
// runtime strips reserved x-bridge.* headers at ingress, so the old default
// (x-bridge.tenant-id) would never reach the processor.
func TestReservedTenantHeader_IsStrippedAtIngress(t *testing.T) {
	headers := map[string]any{messaging.HeaderTenantID: "acme"}
	stripped := messaging.StripReservedHeaders(headers)

	_, ok := stripped[messaging.HeaderTenantID]
	assert.False(t, ok, "reserved tenant header must be stripped at ingress")
}

// TestDefaultTenantHeader_ResolvesAfterIngressStrip is the L6 end-to-end proof:
// the non-reserved default header survives the ingress reserved-strip, so the
// zero-config tenant lookup actually works.
func TestDefaultTenantHeader_ResolvesAfterIngressStrip(t *testing.T) {
	require.False(t, messaging.IsReservedHeader(DefaultTenantHeader),
		"default tenant header must be non-reserved")

	v := &stubValidator{info: ports.TenantInfo{ID: "acme", Active: true}}
	p := mustNew(t, Config{RequireTenant: true}, WithValidator(v))

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		Subject: "test",
		Headers: map[string]any{DefaultTenantHeader: "acme"},
	})
	// Simulate the runtime ingress strip (runner.go).
	env.ReplaceHeaders(messaging.StripReservedHeaders(env.Headers()))

	called := false
	err := p.Process(context.Background(), env, func(_ context.Context, _ *messaging.Envelope) error {
		called = true
		return nil
	})
	require.NoError(t, err)
	require.True(t, called, "tenant must resolve from the non-reserved default header after ingress strip")
}

// TestProcess_DecrementError_EmitsMetricAndLog is the regression for the
// deferred in-flight decrement failure: it must surface a metric and a log
// line (previously the error was silently discarded).
func TestProcess_DecrementError_EmitsMetricAndLog(t *testing.T) {
	metrics := &ports.RecordingExporter{}
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	p := mustNew(t, Config{}, WithUsageTracker(decrementFailTracker{}),
		WithMetrics(metrics), WithLogger(logger))
	env := envelope("acme", 0)

	require.NoError(t, p.Process(context.Background(), env, nextOK))

	entries := metrics.FindEntries(metricTenantTrackerErrors)
	require.Len(t, entries, 1)
	assert.True(t, hasTag(entries[0].Tags, "op", "decrement"),
		"tracker-error metric must be tagged op=decrement")
	assert.Contains(t, logBuf.String(), "tenant usage tracker error")
	assert.Contains(t, logBuf.String(), "acme")
}

// TestProcess_MessageCountError_EmitsMetricButSucceeds is the regression for
// the advisory message-count failure: observable via a metric, yet Process still
// returns nil (message-count is not control-flow-bearing).
func TestProcess_MessageCountError_EmitsMetricButSucceeds(t *testing.T) {
	metrics := &ports.RecordingExporter{}
	tracker := &mockTracker{incrementMessagesErr: errors.New("tracker unavailable")}

	p := mustNew(t, Config{}, WithUsageTracker(tracker), WithMetrics(metrics))
	env := envelope("acme", 0)

	require.NoError(t, p.Process(context.Background(), env, nextOK))

	entries := metrics.FindEntries(metricTenantTrackerErrors)
	require.Len(t, entries, 1)
	assert.True(t, hasTag(entries[0].Tags, "op", "message_count"))
}

// TestProcess_InFlightIncrementError_ClassifiedTransient is the regression
// for the in-flight increment failure: observable and classified transient so
// the runtime retries a tracker hiccup rather than DLQ-ing the message.
func TestProcess_InFlightIncrementError_ClassifiedTransient(t *testing.T) {
	metrics := &ports.RecordingExporter{}
	tracker := &stubTracker{failOnIF: true}

	p := mustNew(t, Config{}, WithUsageTracker(tracker), WithMetrics(metrics))
	env := envelope("acme", 0)

	err := p.Process(context.Background(), env, nextOK)
	require.Error(t, err)
	require.True(t, shared.IsRecoverableError(err), "tracker failure must be retryable")

	entries := metrics.FindEntries(metricTenantTrackerErrors)
	require.Len(t, entries, 1)
	assert.True(t, hasTag(entries[0].Tags, "op", "increment"))
}

// TestProcess_Reject_EmitsMetric is the regression for tenant rejects: a
// policy rejection increments the reject counter with a low-cardinality reason.
func TestProcess_Reject_EmitsMetric(t *testing.T) {
	tests := []struct {
		name     string
		cfg      Config
		validate *stubValidator
		env      *messaging.Envelope
		reason   string
	}{
		{
			name:   "missing required tenant",
			cfg:    Config{RequireTenant: true},
			env:    messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "test"}),
			reason: "missing_required",
		},
		{
			name:     "disabled tenant",
			cfg:      Config{},
			validate: &stubValidator{info: ports.TenantInfo{ID: "acme", Active: false}},
			env:      envelope("acme", 0),
			reason:   "disabled",
		},
		{
			name:     "oversize payload",
			cfg:      Config{},
			validate: &stubValidator{info: ports.TenantInfo{ID: "acme", Active: true, MaxMessageSizeBytes: 10}},
			env:      envelope("acme", 100),
			reason:   "oversize",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			metrics := &ports.RecordingExporter{}
			opts := []Option{WithMetrics(metrics)}
			if tc.validate != nil {
				opts = append(opts, WithValidator(tc.validate))
			}
			p := mustNew(t, tc.cfg, opts...)

			err := p.Process(context.Background(), tc.env, nextOK)
			require.ErrorIs(t, err, shared.ErrInvalidPayload)

			entries := metrics.FindEntries(metricTenantRejects)
			require.Len(t, entries, 1)
			assert.True(t, hasTag(entries[0].Tags, "reason", tc.reason))
		})
	}
}

// TestProcess_TrackerError_TenantIDNotInMetricTags guards against a cardinality
// explosion: the tenant ID must never be a metric dimension (logs only).
func TestProcess_TrackerError_TenantIDNotInMetricTags(t *testing.T) {
	metrics := &ports.RecordingExporter{}
	p := mustNew(t, Config{}, WithUsageTracker(decrementFailTracker{}), WithMetrics(metrics))

	require.NoError(t, p.Process(context.Background(), envelope("secret-tenant-id", 0), nextOK))

	for _, e := range metrics.Entries() {
		for _, tg := range e.Tags {
			assert.NotEqual(t, "secret-tenant-id", tg.Value, "tenant ID must not be a metric tag")
			assert.False(t, strings.Contains(tg.Key, "tenant"), "no tenant-keyed metric dimension")
		}
	}
}
