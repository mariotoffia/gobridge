package sqs

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// Finding 7 — bridge-to-bridge header preservation on egress.
//
// SQS previously stripped ALL reserved x-bridge.* headers on send, losing
// correlation/idempotency across bridge hops. The central policy strips
// only INTERNAL-ONLY headers and preserves BRIDGE-TO-BRIDGE propagated
// headers as SQS message attributes. Native FIFO fields stay out of the
// attribute map (they map to MessageGroupId/MessageDeduplicationId).

func TestHeadersToAttributes_PreservesBridgeToBridge_StripsInternalOnly(t *testing.T) {
	headers := map[string]any{
		messaging.HeaderCorrelationID:   "corr",    // bridge-to-bridge → preserved
		messaging.HeaderCausationID:     "cause",   // bridge-to-bridge → preserved
		messaging.HeaderIdempotencyKey:  "idem",    // bridge-to-bridge → preserved
		messaging.HeaderTenantID:        "tenant",  // bridge-to-bridge → preserved
		messaging.HeaderForwardedFrom:   "bridgeA", // bridge-to-bridge → preserved
		messaging.HeaderTraceParent:     "00-x",    // W3C trace → preserved
		messaging.HeaderRouteID:         "route",   // internal-only → stripped
		messaging.HeaderRouteOverride:   "ovr",     // internal-only → stripped
		messaging.HeaderSourceID:        "src",     // internal-only → stripped
		messaging.HeaderContentType:     "json",    // internal-only → stripped
		"app-custom":                    "v",       // application → preserved
		messaging.HeaderOrderingKey:     "grp",     // native FIFO field → not an attribute
		messaging.HeaderDeduplicationID: "dup",     // native FIFO field → not an attribute
	}

	attrs, dropped := headersToAttributes(headers, sqsMaxMessageAttributes)
	require.Zero(t, dropped)

	for _, k := range []string{
		messaging.HeaderCorrelationID, messaging.HeaderCausationID,
		messaging.HeaderIdempotencyKey, messaging.HeaderTenantID,
		messaging.HeaderForwardedFrom, messaging.HeaderTraceParent, "app-custom",
	} {
		_, ok := attrs[k]
		assert.Truef(t, ok, "%q should be preserved as an SQS attribute", k)
	}
	for _, k := range []string{
		messaging.HeaderRouteID, messaging.HeaderRouteOverride,
		messaging.HeaderSourceID, messaging.HeaderContentType,
	} {
		_, ok := attrs[k]
		assert.Falsef(t, ok, "%q is internal-only and must be stripped on egress", k)
	}
	_, hasOrdering := attrs[messaging.HeaderOrderingKey]
	assert.False(t, hasOrdering, "ordering key maps to native MessageGroupId, not an attribute")
	_, hasDedup := attrs[messaging.HeaderDeduplicationID]
	assert.False(t, hasDedup, "dedup id maps to native MessageDeduplicationId, not an attribute")
}

// Finding 7 × 11 regression: under the SQS attribute COUNT cap, the
// bridge-to-bridge propagation headers MUST win slots over application
// metadata. A name-ascending sort alone keeps the lowest-sorting names,
// but every bridge-to-bridge attribute name sorts last (traceparent /
// tracestate start with 't'; x-bridge.* with 'x'), so under attribute
// pressure a naive sort silently drops idempotency-key / correlation-id
// in favour of lower-sorting app headers — duplicate processing across a
// hop. They must be ranked first.
func TestHeadersToAttributes_PrioritizesBridgeToBridgeUnderCap(t *testing.T) {
	// The 8 bridge-to-bridge headers that become attributes (ordering-key
	// and dedup-id map to native FIFO fields, so they are excluded here).
	bridge := []string{
		messaging.HeaderCorrelationID, messaging.HeaderCausationID,
		messaging.HeaderIdempotencyKey, messaging.HeaderTenantID,
		messaging.HeaderForwardedFrom, messaging.HeaderForwardedHop,
		messaging.HeaderTraceParent, messaging.HeaderTraceState,
	}
	headers := map[string]any{}
	for _, k := range bridge {
		headers[k] = "b2b"
	}
	// 12 application headers, all sorting BEFORE traceparent/tracestate and
	// x-bridge.* — exactly the pressure a naive name-sort loses to.
	const appCount = 12
	for i := 0; i < appCount; i++ {
		headers[fmt.Sprintf("a%02d-app", i)] = "app"
	}

	attrs, dropped := headersToAttributes(headers, sqsMaxMessageAttributes)

	require.Len(t, attrs, sqsMaxMessageAttributes, "the cap must be filled exactly")
	for _, k := range bridge {
		_, ok := attrs[k]
		assert.Truef(t, ok, "bridge-to-bridge header %q must survive the attribute cap", k)
	}
	// 8 bridge + 12 app = 20 eligible, cap 10 → 10 dropped, and the only
	// survivors beyond the 8 bridge headers are application headers.
	require.Equal(t, len(bridge)+appCount-sqsMaxMessageAttributes, dropped,
		"only application headers are sacrificed to the cap")
	survivingApp := 0
	for k := range attrs {
		if !messaging.IsBridgeToBridgeHeader(k) {
			survivingApp++
		}
	}
	assert.Equal(t, sqsMaxMessageAttributes-len(bridge), survivingApp,
		"application headers may only take the slots left after bridge-to-bridge")
}

// TestBuildAttributes_EgressHeaderPolicy exercises the policy through the
// real envelope/sender path (reserved headers installed via the trusted
// constructor), and confirms the Subject attribute is still added.
func TestBuildAttributes_EgressHeaderPolicy(t *testing.T) {
	s, err := NewSender(SenderConfig{QueueURL: "https://sqs.us-east-1.amazonaws.com/123/q"})
	require.NoError(t, err)

	env := messaging.MustEnvelopeWithReserved(messaging.EnvelopeInput{
		ID:      "e1",
		Subject: "evt",
		Payload: []byte("{}"),
		Headers: map[string]any{
			messaging.HeaderCorrelationID: "corr",
			messaging.HeaderRouteID:       "route",
			"app":                         "v",
		},
	})

	attrs := s.buildAttributes(env)

	_, hasCorr := attrs[messaging.HeaderCorrelationID]
	assert.True(t, hasCorr, "correlation-id must be preserved on egress")
	_, hasRoute := attrs[messaging.HeaderRouteID]
	assert.False(t, hasRoute, "route-id is internal-only and must be stripped")
	_, hasApp := attrs["app"]
	assert.True(t, hasApp, "application header must be preserved")
	_, hasSubject := attrs["Subject"]
	assert.True(t, hasSubject, "Subject attribute must be present")
}

// Finding 11 — deterministic SQS attribute-limit enforcement.

func TestHeadersToAttributes_CapsAtMaxDeterministically(t *testing.T) {
	headers := make(map[string]any, 20)
	for i := 0; i < 20; i++ {
		headers[fmt.Sprintf("h%02d", i)] = "v"
	}

	attrs, dropped := headersToAttributes(headers, sqsMaxMessageAttributes)
	require.Len(t, attrs, sqsMaxMessageAttributes)
	require.Equal(t, 10, dropped, "the 10 keys over the cap must be reported as dropped")

	// Sorted selection keeps the lowest-sorting 10 keys (h00..h09).
	for i := 0; i < 10; i++ {
		_, ok := attrs[fmt.Sprintf("h%02d", i)]
		assert.Truef(t, ok, "h%02d should be kept", i)
	}
	for i := 10; i < 20; i++ {
		_, ok := attrs[fmt.Sprintf("h%02d", i)]
		assert.Falsef(t, ok, "h%02d should be dropped", i)
	}
}

func TestHeadersToAttributes_SelectionStableAcrossRuns(t *testing.T) {
	headers := make(map[string]any, 30)
	for i := 0; i < 30; i++ {
		headers[fmt.Sprintf("k%02d", i)] = i
	}

	var first []string
	for run := 0; run < 50; run++ {
		attrs, dropped := headersToAttributes(headers, sqsMaxMessageAttributes)
		require.Len(t, attrs, sqsMaxMessageAttributes)
		require.Equal(t, 20, dropped)

		keys := make([]string, 0, len(attrs))
		for k := range attrs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if run == 0 {
			first = keys
			continue
		}
		assert.Equalf(t, first, keys, "attribute selection must be identical across runs (run %d)", run)
	}
}

func TestBuildAttributes_ReservesSubjectSlot(t *testing.T) {
	s, err := NewSender(SenderConfig{QueueURL: "https://q"})
	require.NoError(t, err)

	headers := make(map[string]any, 20)
	for i := 0; i < 20; i++ {
		headers[fmt.Sprintf("h%02d", i)] = "v"
	}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "e",
		Subject: "subj",
		Payload: []byte("{}"),
		Headers: headers,
	})

	attrs := s.buildAttributes(env)
	require.Len(t, attrs, sqsMaxMessageAttributes, "total attributes must not exceed the SQS cap")

	_, hasSubject := attrs["Subject"]
	require.True(t, hasSubject, "Subject must always get its reserved slot")

	nonSubject := 0
	for k := range attrs {
		if k != "Subject" {
			nonSubject++
		}
	}
	assert.Equal(t, sqsMaxMessageAttributes-1, nonSubject, "one slot reserved for Subject")
}

func TestHeadersToAttributes_DropsInvalidNames(t *testing.T) {
	headers := map[string]any{
		"valid_name":   "ok",
		"valid-2.name": "ok",
		"AWS.reserved": "no",
		"amazon.thing": "no",
		".leading":     "no",
		"trailing.":    "no",
		"double..dot":  "no",
		"bad space":    "no",
		"slash/y":      "no",
	}

	attrs, dropped := headersToAttributes(headers, sqsMaxMessageAttributes)
	require.Zero(t, dropped, "name-invalid headers are skipped, not counted as cap drops")

	_, ok := attrs["valid_name"]
	assert.True(t, ok)
	_, ok = attrs["valid-2.name"]
	assert.True(t, ok)
	for _, bad := range []string{"AWS.reserved", "amazon.thing", ".leading", "trailing.", "double..dot", "bad space", "slash/y"} {
		_, ok := attrs[bad]
		assert.Falsef(t, ok, "invalid name %q must be dropped", bad)
	}
}

func TestIsValidSQSAttributeName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"ok", true},
		{"x-bridge.correlation-id", true},
		{"traceparent", true},
		{"a_b.c-d", true},
		{strings.Repeat("a", 256), true},
		{"", false},
		{strings.Repeat("a", 257), false},
		{"AWS.x", false},
		{"aws.x", false},
		{"Amazon.x", false},
		{".x", false},
		{"x.", false},
		{"a..b", false},
		{"a b", false},
		{"a/b", false},
	}
	for _, tc := range cases {
		assert.Equalf(t, tc.want, isValidSQSAttributeName(tc.name), "name=%q", tc.name)
	}
}

func TestHeadersToAttributes_ZeroBudget_ReturnsNil(t *testing.T) {
	attrs, dropped := headersToAttributes(map[string]any{"a": "b"}, 0)
	assert.Nil(t, attrs)
	assert.Zero(t, dropped)
}
