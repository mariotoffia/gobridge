package sqs

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// WP-SQS-LIFT — cross-hop bridge identity lift on SQS ingress.
//
// A sending bridge's SQS egress propagates its idempotency key as the
// x-bridge.idempotency-key MESSAGE ATTRIBUTE and its dedup/ordering keys as
// the NATIVE FIFO fields MessageDeduplicationId / MessageGroupId. Because
// messaging.NewEnvelope strips every x-bridge.* key from the untrusted
// Headers map (the anti-spoof chokepoint), convertMessage must LIFT those
// values into EnvelopeInput's first-class IdempotencyKey / DeduplicationID /
// OrderingKey fields — the trusted path applied after the strip — or the
// receiving hop loses dedup and ordering. These tests pin that lift and its
// boundaries; the amqp10 adapter implements the analogous pattern.

// TestConvertMessage_LiftsIdempotencyKeyFromBridgeAttribute verifies that the
// idempotency key riding the x-bridge.idempotency-key message attribute is
// lifted onto the envelope, and that the case-insensitive match mirrors
// messaging.IsReservedHeader (a MiXeD-cAsE key still lifts). Each case uses a
// single attribute key so the match is deterministic (map iteration order is
// unspecified).
func TestConvertMessage_LiftsIdempotencyKeyFromBridgeAttribute(t *testing.T) {
	cases := []struct {
		name    string
		attrKey string
	}{
		{"canonical case", messaging.HeaderIdempotencyKey},
		{"mixed case", "X-Bridge.Idempotency-Key"},
		{"upper/lower mix", "x-BRIDGE.idempotency-KEY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := receiverForTest(t, false)
			msg := sqstypes.Message{
				MessageId:     aws.String("m"),
				ReceiptHandle: aws.String("rh"),
				Body:          aws.String("b"),
				MessageAttributes: map[string]sqstypes.MessageAttributeValue{
					tc.attrKey: {DataType: aws.String("String"), StringValue: aws.String("idem-42")},
				},
			}

			env, _, err := r.convertMessage(context.Background(), "http://test/queue", msg)
			require.NoError(t, err)

			got, ok := env.Header(messaging.HeaderIdempotencyKey)
			require.Truef(t, ok,
				"idempotency key must be lifted into the envelope from attribute %q (survived the reserved strip)", tc.attrKey)
			assert.Equal(t, "idem-42", got)
		})
	}
}

// TestConvertMessage_LiftsFIFOSystemAttributes verifies that the native FIFO
// system attributes MessageDeduplicationId / MessageGroupId are lifted onto
// the envelope's dedup / ordering reserved headers.
func TestConvertMessage_LiftsFIFOSystemAttributes(t *testing.T) {
	r := receiverForTest(t, false)
	msg := sqstypes.Message{
		MessageId:     aws.String("m"),
		ReceiptHandle: aws.String("rh"),
		Body:          aws.String("b"),
		Attributes: map[string]string{
			string(sqstypes.MessageSystemAttributeNameMessageDeduplicationId): "dedup-99",
			string(sqstypes.MessageSystemAttributeNameMessageGroupId):         "group-7",
		},
	}

	env, _, err := r.convertMessage(context.Background(), "http://test/queue", msg)
	require.NoError(t, err)

	dedup, ok := env.Header(messaging.HeaderDeduplicationID)
	require.True(t, ok, "MessageDeduplicationId must be lifted onto the envelope dedup header")
	assert.Equal(t, "dedup-99", dedup)

	group, ok := env.Header(messaging.HeaderOrderingKey)
	require.True(t, ok, "MessageGroupId must be lifted onto the envelope ordering header")
	assert.Equal(t, "group-7", group)
}

// TestConvertMessage_SpoofedDedupAttributeDoesNotSurvive pins that dedup and
// ordering are lifted ONLY from the native FIFO fields — never from
// x-bridge.dedup-id / x-bridge.ordering-key MESSAGE ATTRIBUTES, which egress
// never emits. Accepting them from attributes would create an ingress surface
// egress never produces, so they must be stripped and not lifted.
func TestConvertMessage_SpoofedDedupAttributeDoesNotSurvive(t *testing.T) {
	r := receiverForTest(t, false)
	msg := sqstypes.Message{
		MessageId:     aws.String("m"),
		ReceiptHandle: aws.String("rh"),
		Body:          aws.String("b"),
		MessageAttributes: map[string]sqstypes.MessageAttributeValue{
			messaging.HeaderDeduplicationID: {DataType: aws.String("String"), StringValue: aws.String("forged-dedup")},
			messaging.HeaderOrderingKey:     {DataType: aws.String("String"), StringValue: aws.String("forged-group")},
		},
		// No native FIFO system attributes: dedup/ordering must come only
		// from msg.Attributes, which is empty here.
	}

	env, _, err := r.convertMessage(context.Background(), "http://test/queue", msg)
	require.NoError(t, err)

	_, hasDedup := env.Header(messaging.HeaderDeduplicationID)
	assert.False(t, hasDedup,
		"x-bridge.dedup-id is never emitted as an attribute by egress; it must not be lifted from a message attribute")
	_, hasOrdering := env.Header(messaging.HeaderOrderingKey)
	assert.False(t, hasOrdering,
		"x-bridge.ordering-key is never emitted as an attribute by egress; it must not be lifted from a message attribute")
}

// TestConvertMessage_NoBridgeAttributes_NoIdentityHeaders pins that a plain
// message (application attributes only, no bridge identity) stamps none of the
// three identity headers — the lift is conditional on a value being present.
func TestConvertMessage_NoBridgeAttributes_NoIdentityHeaders(t *testing.T) {
	r := receiverForTest(t, false)
	msg := sqstypes.Message{
		MessageId:     aws.String("m"),
		ReceiptHandle: aws.String("rh"),
		Body:          aws.String("b"),
		MessageAttributes: map[string]sqstypes.MessageAttributeValue{
			"app": {DataType: aws.String("String"), StringValue: aws.String("v")},
		},
	}

	env, _, err := r.convertMessage(context.Background(), "http://test/queue", msg)
	require.NoError(t, err)

	for _, key := range []string{
		messaging.HeaderIdempotencyKey,
		messaging.HeaderDeduplicationID,
		messaging.HeaderOrderingKey,
	} {
		_, ok := env.Header(key)
		assert.Falsef(t, ok, "plain message must not stamp identity header %q", key)
	}

	// The ordinary application attribute still round-trips through ingress.
	app, ok := env.Header("app")
	require.True(t, ok)
	assert.Equal(t, "v", app)
}

// TestConvertMessage_NonStringIdempotencyAttributeIgnored pins that a
// non-String idempotency attribute (DataType == "Number") is not lifted:
// bridgeAttrString requires a String attribute, mirroring the amqp10 adapter's
// string-typed bridgeHeaderString.
func TestConvertMessage_NonStringIdempotencyAttributeIgnored(t *testing.T) {
	r := receiverForTest(t, false)
	msg := sqstypes.Message{
		MessageId:     aws.String("m"),
		ReceiptHandle: aws.String("rh"),
		Body:          aws.String("b"),
		MessageAttributes: map[string]sqstypes.MessageAttributeValue{
			messaging.HeaderIdempotencyKey: {DataType: aws.String("Number"), StringValue: aws.String("12345")},
		},
	}

	env, _, err := r.convertMessage(context.Background(), "http://test/queue", msg)
	require.NoError(t, err)

	_, ok := env.Header(messaging.HeaderIdempotencyKey)
	assert.False(t, ok, "a non-String idempotency attribute (DataType=Number) must not be lifted")
}

// TestConvertMessage_EmptyIdempotencyAttribute_StampsNoHeader pins the
// absent == empty equivalence: an x-bridge.idempotency-key attribute that is
// PRESENT with a plain String DataType but an empty StringValue lifts to ""
// and NewEnvelope's (value != "") guard stamps no header — indistinguishable
// from the attribute being absent.
func TestConvertMessage_EmptyIdempotencyAttribute_StampsNoHeader(t *testing.T) {
	r := receiverForTest(t, false)
	msg := sqstypes.Message{
		MessageId:     aws.String("m"),
		ReceiptHandle: aws.String("rh"),
		Body:          aws.String("b"),
		MessageAttributes: map[string]sqstypes.MessageAttributeValue{
			messaging.HeaderIdempotencyKey: {DataType: aws.String("String"), StringValue: aws.String("")},
		},
	}

	env, _, err := r.convertMessage(context.Background(), "http://test/queue", msg)
	require.NoError(t, err)

	_, ok := env.Header(messaging.HeaderIdempotencyKey)
	assert.False(t, ok, "a present-but-empty idempotency attribute must stamp no header (empty == absent)")
}

// TestConvertMessage_RoundTripsEgressIdentity binds the egress and ingress
// halves of the cross-hop identity contract. Egress (acl_outbound.go) and
// ingress (acl_inbound.go) independently encode "idempotency rides a String
// message attribute; dedup/ordering ride the native FIFO fields" — if one
// side drifts, the isolated unit suites on each side can still pass. This
// test runs the REAL egress mappers (headersToAttributes + extractFIFOFields)
// on a stamped source envelope, reconstructs the SQS message exactly as the
// broker surfaces it on receive, feeds it through convertMessage, and asserts
// all three identity values round-trip. A drift on either side breaks it.
func TestConvertMessage_RoundTripsEgressIdentity(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	// Source envelope with identity stamped the trusted first-class way.
	src, err := messaging.NewEnvelope(messaging.EnvelopeInput{
		ID:              "src-1",
		Subject:         "evt",
		Payload:         []byte("{}"),
		IdempotencyKey:  "idem-abc",
		DeduplicationID: "dedup-xyz",
		OrderingKey:     "group-1",
	}, now)
	require.NoError(t, err)

	// REAL egress mapping — do not hand-fake the attribute map.
	attrs, dropped := headersToAttributes(src.Headers(), sqsMaxMessageAttributes, 0)
	require.Zero(t, dropped)
	idemAttr, ok := attrs[messaging.HeaderIdempotencyKey]
	require.True(t, ok, "egress must emit the idempotency key as a message attribute")
	require.Equal(t, "String", aws.ToString(idemAttr.DataType),
		"egress must emit the idempotency key with the plain String DataType ingress requires")
	// Dedup/ordering must NOT be attributes — they ride the native FIFO fields.
	_, hasDedupAttr := attrs[messaging.HeaderDeduplicationID]
	require.False(t, hasDedupAttr, "dedup id must not be an egress attribute")
	_, hasOrderAttr := attrs[messaging.HeaderOrderingKey]
	require.False(t, hasOrderAttr, "ordering key must not be an egress attribute")

	groupID, dedupID := extractFIFOFields(src.Headers())
	require.Equal(t, "group-1", groupID)
	require.Equal(t, "dedup-xyz", dedupID)

	// Reconstruct the message exactly as SQS surfaces it on receive: egress
	// attributes plus the native FIFO fields exposed via msg.Attributes.
	msg := sqstypes.Message{
		MessageId:         aws.String("m"),
		ReceiptHandle:     aws.String("rh"),
		Body:              aws.String("{}"),
		MessageAttributes: attrs,
		Attributes: map[string]string{
			string(sqstypes.MessageSystemAttributeNameMessageDeduplicationId): dedupID,
			string(sqstypes.MessageSystemAttributeNameMessageGroupId):         groupID,
		},
	}

	r := receiverForTest(t, false)
	env2, _, err := r.convertMessage(context.Background(), "http://test/queue", msg)
	require.NoError(t, err)

	idem, ok := env2.Header(messaging.HeaderIdempotencyKey)
	require.True(t, ok, "idempotency key must round-trip egress→SQS→ingress")
	assert.Equal(t, "idem-abc", idem)

	dedup, ok := env2.Header(messaging.HeaderDeduplicationID)
	require.True(t, ok, "dedup id must round-trip egress→SQS→ingress")
	assert.Equal(t, "dedup-xyz", dedup)

	group, ok := env2.Header(messaging.HeaderOrderingKey)
	require.True(t, ok, "ordering key must round-trip egress→SQS→ingress")
	assert.Equal(t, "group-1", group)
}
