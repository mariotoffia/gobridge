package servicebus

import (
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
)

// --- Finding: delayed retry resets the broker delivery count ---------------

// The scheduled retry copy must carry the accumulated receive count in
// the reserved x-bridge.retry-attempt property: scheduling a fresh
// message resets the broker DeliveryCount to 1, which would otherwise
// bypass the runtime's MaxReplayAttempts gate forever.
func TestBuildRetryMessage_StampsAttemptCounter(t *testing.T) {
	t.Parallel()

	clk := clocktest.New()

	// First retry: original delivery, broker count 1, no bridge counter.
	first := &azservicebus.ReceivedMessage{
		MessageID:     "m1",
		Body:          []byte("payload"),
		DeliveryCount: 1,
	}
	copy1 := buildRetryMessage(first, clk)
	require.Equal(t, int64(1), copy1.ApplicationProperties[asbPropRetryAttempt])

	// Second retry: the redelivered COPY has broker count 1 again, but
	// carries the counter from the first hop — the stamp accumulates.
	second := &azservicebus.ReceivedMessage{
		MessageID:     "m1-r1",
		Body:          []byte("payload"),
		DeliveryCount: 1,
		ApplicationProperties: map[string]any{
			asbPropRetryAttempt:      int64(1),
			asbPropOriginalMessageID: "m1",
		},
	}
	copy2 := buildRetryMessage(second, clk)
	require.Equal(t, int64(2), copy2.ApplicationProperties[asbPropRetryAttempt])
}

// Ingress must stamp the EFFECTIVE receive count (broker DeliveryCount
// + carried bridge counter) into asb.delivery-count so the runtime's
// receive-count gate fires across bridge-scheduled retries.
func TestMessageToHeaders_SumsBridgeAttempts(t *testing.T) {
	t.Parallel()

	msg := &azservicebus.ReceivedMessage{
		MessageID:     "m1-r2",
		DeliveryCount: 1,
		ApplicationProperties: map[string]any{
			asbPropRetryAttempt:      int64(2),
			asbPropOriginalMessageID: "m1",
		},
	}
	h := messageToHeaders(msg)
	require.Equal(t, 3, h[asbHeaderDeliveryCount])

	// Reserved x-bridge.* wire properties never leak into headers.
	require.NotContains(t, h, asbPropRetryAttempt)
	require.NotContains(t, h, asbPropOriginalMessageID)
}

// bridgeAttempts must tolerate the numeric widths an AMQP round-trip
// can produce and fail safe (0) on garbage or negatives.
func TestBridgeAttempts_NumericWidths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		val  any
		want int
	}{
		{"absent", nil, 0},
		{"int", int(2), 2},
		{"int32", int32(3), 3},
		{"int64", int64(4), 4},
		{"uint32", uint32(5), 5},
		{"uint64", uint64(6), 6},
		{"float64", float64(7), 7},
		{"string", "8", 8},
		{"garbage string", "x", 0},
		{"negative", int64(-3), 0},
		{"wrong type", []byte("2"), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			props := map[string]any{}
			if tt.val != nil {
				props[asbPropRetryAttempt] = tt.val
			}
			require.Equal(t, tt.want, bridgeAttempts(props))
		})
	}
}

// --- Finding: duplicate detection silently discards the retry copy ---------

// The copy's wire MessageID must be salted with the attempt number so a
// dedup-enabled queue never drops the scheduled retry, while the FIRST
// delivery's ID is preserved for end-to-end idempotency.
func TestBuildRetryMessage_SaltsMessageID(t *testing.T) {
	t.Parallel()

	clk := clocktest.New()

	first := &azservicebus.ReceivedMessage{MessageID: "m1", DeliveryCount: 1}
	copy1 := buildRetryMessage(first, clk)
	require.NotNil(t, copy1.MessageID)
	require.Equal(t, "m1-r1", *copy1.MessageID)
	require.Equal(t, "m1", copy1.ApplicationProperties[asbPropOriginalMessageID])

	// Next hop: the salt derives from the ORIGINAL ID, never compounds.
	second := &azservicebus.ReceivedMessage{
		MessageID:     "m1-r1",
		DeliveryCount: 1,
		ApplicationProperties: map[string]any{
			asbPropRetryAttempt:      int64(1),
			asbPropOriginalMessageID: "m1",
		},
	}
	copy2 := buildRetryMessage(second, clk)
	require.Equal(t, "m1-r2", *copy2.MessageID)
	require.Equal(t, "m1", copy2.ApplicationProperties[asbPropOriginalMessageID])
}

// Ingress restores the original MessageID as the envelope ID so
// idempotency/dedup sees ONE logical message across retry copies.
func TestReceivedToEnvelope_RestoresOriginalMessageID(t *testing.T) {
	t.Parallel()

	clk := clocktest.New()
	msg := &azservicebus.ReceivedMessage{
		MessageID:     "m1-r2",
		Body:          []byte("p"),
		DeliveryCount: 1,
		ApplicationProperties: map[string]any{
			asbPropRetryAttempt:      int64(2),
			asbPropOriginalMessageID: "m1",
		},
	}
	env, err := receivedToEnvelope(msg, clk)
	require.NoError(t, err)
	require.Equal(t, "m1", env.ID())
}

// --- Finding: retry copy aliases properties and restarts TTL ---------------

// ApplicationProperties must be deep-copied: mutating the copy must
// never write through into the received message.
func TestBuildRetryMessage_DeepCopiesApplicationProperties(t *testing.T) {
	t.Parallel()

	clk := clocktest.New()
	received := &azservicebus.ReceivedMessage{
		MessageID:     "m1",
		DeliveryCount: 1,
		ApplicationProperties: map[string]any{
			"tenant": "acme",
		},
	}
	out := buildRetryMessage(received, clk)

	out.ApplicationProperties["tenant"] = "mutated"
	out.ApplicationProperties["new-key"] = "v"

	require.Equal(t, "acme", received.ApplicationProperties["tenant"])
	require.NotContains(t, received.ApplicationProperties, "new-key")
}

// The copy's TimeToLive must be the REMAINING time to the original
// absolute expiry — retries must not resurrect the message with a
// fresh full TTL.
func TestBuildRetryMessage_PreservesAbsoluteExpiry(t *testing.T) {
	t.Parallel()

	clk := clocktest.New()
	fullTTL := 10 * time.Minute
	expires := clk.Now().Add(90 * time.Second)

	received := &azservicebus.ReceivedMessage{
		MessageID:     "m1",
		DeliveryCount: 1,
		TimeToLive:    &fullTTL,
		ExpiresAt:     &expires,
	}
	out := buildRetryMessage(received, clk)
	require.NotNil(t, out.TimeToLive)
	require.Equal(t, 90*time.Second, *out.TimeToLive)
}

// An already-expired message gets a minimal TTL so the broker drops or
// dead-letters it promptly instead of granting it a fresh lifetime.
func TestBuildRetryMessage_ExpiredGetsMinimalTTL(t *testing.T) {
	t.Parallel()

	clk := clocktest.New()
	expires := clk.Now().Add(-time.Second)
	received := &azservicebus.ReceivedMessage{
		MessageID:     "m1",
		DeliveryCount: 1,
		ExpiresAt:     &expires,
	}
	out := buildRetryMessage(received, clk)
	require.NotNil(t, out.TimeToLive)
	require.Equal(t, time.Millisecond, *out.TimeToLive)
}

// Without an absolute expiry the original TTL is carried by VALUE, not
// by aliasing the received message's pointer.
func TestBuildRetryMessage_TTLNotAliased(t *testing.T) {
	t.Parallel()

	clk := clocktest.New()
	ttl := time.Minute
	received := &azservicebus.ReceivedMessage{
		MessageID:     "m1",
		DeliveryCount: 1,
		TimeToLive:    &ttl,
	}
	out := buildRetryMessage(received, clk)
	require.NotNil(t, out.TimeToLive)
	require.Equal(t, time.Minute, *out.TimeToLive)
	require.NotSame(t, received.TimeToLive, out.TimeToLive)
}
