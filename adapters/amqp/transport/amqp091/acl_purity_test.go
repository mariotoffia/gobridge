// ═══════════════════════════════════════════════
// Production-readiness remediation tests: inbound/outbound ACL correctness.
//
//   - LOW: inbound AMQP Expiration (relative TTL) mapped to envelope
//     ExpiresAt (absolute deadline) so multi-hop routing does not restart
//     the TTL at every hop.
//   - LOW: egress Priority/Timestamp header coercion — YAML/JSON configs
//     supply int/float/string, not the exact Go types, so `priority: 9`
//     used to publish at 0.
//   - LOW (HARD RULE): no SDK type (amqp.Table, amqp.Decimal) may cross
//     the ACL into envelope headers; nested field tables/arrays are
//     rendered recursively to stdlib types.
//
// ═══════════════════════════════════════════════
package amqp091

import (
	"math"
	"strconv"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
)

// TestDeliveryToEnvelope_ExpirationMapsToExpiresAt is the LOW regression:
// an inbound per-message Expiration must land on the envelope's ExpiresAt
// as an absolute deadline (receipt time + TTL).
//
// Counterfactual (drop the ExpiresAt mapping): HasExpiry is false and the
// RemainingTTL round-trip assertion fails.
func TestDeliveryToEnvelope_ExpirationMapsToExpiresAt(t *testing.T) {
	fake := clocktest.New()
	now := fake.Now()

	d := amqp.Delivery{MessageId: "m1", Body: []byte("x"), Expiration: "60000"}
	env, err := deliveryToEnvelope(d, fake)
	require.NoError(t, err)

	require.True(t, env.HasExpiry(), "inbound Expiration must map to an envelope expiry")
	require.WithinDuration(t, now.Add(60*time.Second), env.ExpiresAt(), 0,
		"ExpiresAt must be receipt time + the TTL")
	// Egress re-derives the relative TTL from ExpiresAt; at receipt time it
	// equals the original 60s, and it shrinks (not restarts) on later hops.
	require.Equal(t, 60*time.Second, env.RemainingTTL(fake))
}

// TestDeliveryToEnvelope_NoOrInvalidExpiration_NoExpiry confirms empty and
// malformed Expiration strings never set an expiry.
func TestDeliveryToEnvelope_NoOrInvalidExpiration_NoExpiry(t *testing.T) {
	fake := clocktest.New()
	for _, exp := range []string{"", "not-a-number", "-5", "12.5"} {
		d := amqp.Delivery{MessageId: "m", Body: []byte("x"), Expiration: exp}
		env, err := deliveryToEnvelope(d, fake)
		require.NoError(t, err)
		require.False(t, env.HasExpiry(), "Expiration %q must not set an expiry", exp)
	}
}

// TestHeadersToPublishing_PriorityCoercion is the LOW regression: egress
// must coerce numeric/string priority overrides, not only exact uint8.
//
// Counterfactual (revert to headers[HeaderPriority].(uint8)): the int/
// int64/float64/string cases all publish at 0 and fail.
func TestHeadersToPublishing_PriorityCoercion(t *testing.T) {
	cases := []struct {
		name string
		val  any
		want uint8
	}{
		{"yaml int 9", 9, 9},
		{"int64 5", int64(5), 5},
		{"float64 7", float64(7), 7},
		{"string 3", "3", 3},
		{"uint8 passthrough", uint8(4), 4},
		{"out of range 300 -> 0", 300, 0},
		{"non-integral 2.5 -> 0", 2.5, 0},
		{"garbage string -> 0", "high", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pub := headersToPublishing(map[string]any{HeaderPriority: tc.val})
			require.Equal(t, tc.want, pub.Priority)
		})
	}
}

// TestHeadersToPublishing_TimestampCoercion confirms POSIX-seconds and
// RFC3339 timestamp overrides are coerced (not only exact time.Time).
func TestHeadersToPublishing_TimestampCoercion(t *testing.T) {
	want := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)

	require.True(t, headersToPublishing(map[string]any{HeaderTimestamp: want}).Timestamp.Equal(want))
	require.True(t, headersToPublishing(map[string]any{HeaderTimestamp: want.Unix()}).Timestamp.Equal(want))
	require.True(t, headersToPublishing(map[string]any{HeaderTimestamp: int(want.Unix())}).Timestamp.Equal(want))
	require.True(t, headersToPublishing(map[string]any{HeaderTimestamp: want.Format(time.RFC3339)}).Timestamp.Equal(want))
	require.True(t, headersToPublishing(map[string]any{HeaderTimestamp: "not-a-time"}).Timestamp.IsZero())
}

// TestDeliveryToHeaders_NestedTable_NoSDKTypeLeaks is the ACL-purity
// regression: nested amqp.Table / amqp.Decimal values must be rendered to
// stdlib types so no SDK type crosses the boundary into envelope headers.
//
// Counterfactual (copy the header value verbatim): the nested value stays
// an amqp.Table and assertNoSDKTypes fatals.
func TestDeliveryToHeaders_NestedTable_NoSDKTypeLeaks(t *testing.T) {
	d := amqp.Delivery{
		Headers: amqp.Table{
			"flat":    "v",
			"nested":  amqp.Table{"inner": "x", "deep": amqp.Table{"n": int32(1)}},
			"list":    []any{"a", amqp.Table{"k": "v"}, amqp.Decimal{Scale: 2, Value: 12345}},
			"decimal": amqp.Decimal{Scale: 1, Value: 5}, // 0.5
		},
	}

	h := deliveryToHeaders(d)
	assertNoSDKTypes(t, h)

	nested, ok := h["nested"].(map[string]any)
	require.True(t, ok, "nested must be map[string]any, got %T", h["nested"])
	require.Equal(t, "x", nested["inner"])

	deep, ok := nested["deep"].(map[string]any)
	require.True(t, ok, "deep must be map[string]any, got %T", nested["deep"])
	require.Equal(t, int32(1), deep["n"])

	list, ok := h["list"].([]any)
	require.True(t, ok, "list must be []any, got %T", h["list"])
	_, isMap := list[1].(map[string]any)
	require.True(t, isMap, "list element must be map[string]any, got %T", list[1])
	require.Equal(t, 123.45, list[2], "amqp.Decimal must render to float64")

	require.Equal(t, 0.5, h["decimal"], "amqp.Decimal must render to float64")
}

// assertNoSDKTypes recursively fails if any amqp.* SDK carrier remains
// anywhere in the rendered header tree.
func assertNoSDKTypes(t *testing.T, v any) {
	t.Helper()
	switch x := v.(type) {
	case amqp.Table:
		t.Fatalf("amqp.Table leaked into envelope headers: %#v", x)
	case amqp.Decimal:
		t.Fatalf("amqp.Decimal leaked into envelope headers: %#v", x)
	case map[string]any:
		for _, e := range x {
			assertNoSDKTypes(t, e)
		}
	case []any:
		for _, e := range x {
			assertNoSDKTypes(t, e)
		}
	}
}

// TestParseExpirationMillis_OverRange_NoExpiry is the integer-overflow
// regression: time.Duration is int64 NANOSECONDS, so ms*time.Millisecond
// wraps NEGATIVE once ms exceeds MaxInt64/1e6. A wrapped-negative TTL would
// make ExpiresAt land in the PAST and the delivery would be dropped/DLQ'd as
// already-expired — silent loss for a producer's huge "never expire"
// sentinel. Over-range must fail toward delivery (ok=false → no expiry).
//
// Counterfactual (remove the over-range guard): "9999999999999" returns a
// negative duration with ok=true and the boundary/over-range cases fail.
func TestParseExpirationMillis_OverRange_NoExpiry(t *testing.T) {
	// Largest ms that does not overflow: MaxInt64 / 1e6 (truncated).
	const maxOKMillis = int64(math.MaxInt64) / int64(time.Millisecond) // 9223372036854

	cases := []struct {
		name   string
		in     string
		wantOK bool
		wantD  time.Duration
	}{
		{"normal 5000ms", "5000", true, 5 * time.Second},
		{"boundary max-ok", strconv.FormatInt(maxOKMillis, 10), true, time.Duration(maxOKMillis) * time.Millisecond},
		{"boundary max-ok+1 overflows", strconv.FormatInt(maxOKMillis+1, 10), false, 0},
		{"audit repro 9999999999999", "9999999999999", false, 0},
		{"math.MaxInt64 ms", strconv.FormatInt(math.MaxInt64, 10), false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, ok := parseExpirationMillis(tc.in)
			require.Equal(t, tc.wantOK, ok, "ok for %q", tc.in)
			require.Equal(t, tc.wantD, d, "duration for %q", tc.in)
			if ok {
				require.Positive(t, d, "a mapped TTL must be positive, never wrapped-negative")
			}
		})
	}
}

// TestDeliveryToEnvelope_OverRangeExpiration_NoPastExpiry proves the
// end-to-end effect: an over-range Expiration on a delivery yields an
// envelope with NO expiry (a ZERO ExpiresAt), never a past deadline that
// would make the message look already-expired.
func TestDeliveryToEnvelope_OverRangeExpiration_NoPastExpiry(t *testing.T) {
	fake := clocktest.New()

	d := amqp.Delivery{MessageId: "m1", Body: []byte("x"), Expiration: "9999999999999"}
	env, err := deliveryToEnvelope(d, fake)
	require.NoError(t, err)

	require.False(t, env.HasExpiry(), "over-range Expiration must not set an expiry")
	require.True(t, env.ExpiresAt().IsZero(), "ExpiresAt must be zero, never a wrapped past deadline")
	require.False(t, env.IsExpired(fake), "an over-range TTL must not read as already-expired")
}

// TestTimestampFromHeader_OverRange_Ignored is the regression: huge
// uint64/float seconds must be rejected (ok=false) rather than overflowing
// int64(t) in time.Unix into a garbage pre-epoch timestamp — parity with
// priorityFromHeader's range rejection.
func TestTimestampFromHeader_OverRange_Ignored(t *testing.T) {
	// In-range values still coerce.
	want := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	require.True(t, headersToPublishing(map[string]any{HeaderTimestamp: uint64(want.Unix())}).Timestamp.Equal(want))
	require.True(t, headersToPublishing(map[string]any{HeaderTimestamp: float64(want.Unix())}).Timestamp.Equal(want))

	// Out-of-range values are ignored (zero timestamp), never a garbage
	// pre-epoch one.
	for _, tc := range []struct {
		name string
		val  any
	}{
		{"uint64 MaxUint64", uint64(math.MaxUint64)},
		{"uint64 > MaxInt64", uint64(math.MaxInt64) + 1},
		{"float64 1e30", float64(1e30)},
		{"float64 -1e30", float64(-1e30)},
		{"float64 +Inf", math.Inf(1)},
		{"float64 NaN", math.NaN()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := headersToPublishing(map[string]any{HeaderTimestamp: tc.val}).Timestamp
			require.True(t, ts.IsZero(), "out-of-range %v must be ignored, got %v", tc.val, ts)
		})
	}
}
