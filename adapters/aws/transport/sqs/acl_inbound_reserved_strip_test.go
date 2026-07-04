package sqs

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

// receiverForTest builds a Receiver suitable for direct convertMessage
// invocation without an actual AWS client.
func receiverForTest(t *testing.T, snsUnwrap bool) *Receiver {
	t.Helper()
	r, err := NewReceiver(ReceiverConfig{
		QueueURL:  "http://test/queue",
		SNSUnwrap: snsUnwrap,
	}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	return r
}

// TestSQSInbound_MessageAttributes_StripsReservedMixedCase pins
// that caller-supplied x-bridge.* MessageAttributes (including
// mixed-case spoof attempts) cannot survive ingress. The chokepoint
// for the strip is messaging.NewEnvelope.
func TestSQSInbound_MessageAttributes_StripsReservedMixedCase(t *testing.T) {
	r := receiverForTest(t, false)
	msg := sqstypes.Message{
		MessageId:     aws.String("m-1"),
		ReceiptHandle: aws.String("rh"),
		Body:          aws.String("body"),
		MessageAttributes: map[string]sqstypes.MessageAttributeValue{
			"x-bridge.route-id":     {DataType: aws.String("String"), StringValue: aws.String("forged-route")},
			"X-Bridge.Tenant-Id":    {DataType: aws.String("String"), StringValue: aws.String("forged-tenant")},
			"x-BRIDGE.causation-id": {DataType: aws.String("String"), StringValue: aws.String("forged-cause")},
			"tenant":                {DataType: aws.String("String"), StringValue: aws.String("acme")},
		},
	}

	env, _, err := r.convertMessage(context.Background(), "http://test/queue", msg)
	if err != nil {
		t.Fatalf("convertMessage: %v", err)
	}
	for key := range env.Headers() {
		if messaging.IsReservedHeader(key) {
			t.Errorf("reserved header survived strip: %q (case-insensitive check)", key)
		}
	}
	if env.Headers()["tenant"] != "acme" {
		t.Errorf("non-reserved header dropped; tenant = %v", env.Headers()["tenant"])
	}
}

// TestSQSInbound_SNSUnwrap_StripsReserved verifies the SNS-unwrap path:
// the inner SNS Subject is honoured but reserved headers in the outer
// MessageAttributes are still stripped via the NewEnvelope chokepoint.
func TestSQSInbound_SNSUnwrap_StripsReserved(t *testing.T) {
	r := receiverForTest(t, true)
	body := `{"Type":"Notification","TopicArn":"arn:aws:sns:us-east-1:111:t","Subject":"sns-subj","Message":"inner"}`
	msg := sqstypes.Message{
		MessageId:     aws.String("m-2"),
		ReceiptHandle: aws.String("rh"),
		Body:          aws.String(body),
		MessageAttributes: map[string]sqstypes.MessageAttributeValue{
			"x-bridge.correlation-id": {DataType: aws.String("String"), StringValue: aws.String("forged")},
			"k":                       {DataType: aws.String("String"), StringValue: aws.String("v")},
		},
	}

	env, _, err := r.convertMessage(context.Background(), "http://test/queue", msg)
	if err != nil {
		t.Fatalf("convertMessage: %v", err)
	}
	if _, ok := env.Headers()[messaging.HeaderCorrelationID]; ok {
		t.Errorf("reserved header survived SNS-unwrap path")
	}
	if env.Headers()["sns.subject"] != "sns-subj" {
		t.Errorf("sns.subject not preserved: %v", env.Headers()["sns.subject"])
	}
	if env.Subject() != "sns-subj" {
		t.Errorf("inner SNS subject must be set on envelope; got %q", env.Subject())
	}
}

// TestSQSInbound_ReplaceHeaders_DefensiveStrip pins the
// Envelope.ReplaceHeaders defensive-strip contract added in fix-round
// 1: even if an adapter regresses to using ReplaceHeaders for a map
// containing reserved keys, those keys are scrubbed.
func TestSQSInbound_ReplaceHeaders_DefensiveStrip(t *testing.T) {
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "id-1", Subject: "s"})
	env.ReplaceHeaders(map[string]any{
		messaging.HeaderRouteID: "forged",
		"X-Bridge.Tenant-Id":    "forged",
		"keep":                  "yes",
	})
	for key := range env.Headers() {
		if messaging.IsReservedHeader(key) {
			t.Errorf("ReplaceHeaders failed defensive strip; key=%q survived", key)
		}
	}
	if env.Headers()["keep"] != "yes" {
		t.Errorf("non-reserved header dropped by defensive strip")
	}
}

// TestSQSInbound_SNSStarHeadersPreserved makes sure the sns.* keys
// (which are NOT bridge-reserved despite "looking like" reserved
// metadata) survive the chokepoint.
func TestSQSInbound_SNSStarHeadersPreserved(t *testing.T) {
	r := receiverForTest(t, true)
	body := `{"Type":"Notification","TopicArn":"arn:aws:sns:us-east-1:111:t","Subject":"s","Message":"m"}`
	msg := sqstypes.Message{
		MessageId:     aws.String("m-3"),
		ReceiptHandle: aws.String("rh"),
		Body:          aws.String(body),
	}
	env, _, err := r.convertMessage(context.Background(), "http://test/queue", msg)
	if err != nil {
		t.Fatalf("convertMessage: %v", err)
	}
	if env.Headers()["sns.topic_arn"] != "arn:aws:sns:us-east-1:111:t" {
		t.Errorf("sns.topic_arn missing or wrong: %v", env.Headers()["sns.topic_arn"])
	}
	if env.Headers()["sns.subject"] != "s" {
		t.Errorf("sns.subject missing or wrong: %v", env.Headers()["sns.subject"])
	}
}

// TestSQSInbound_ReservedConstants_Table iterates the documented reserved
// header constants to guarantee none can be smuggled through
// MessageAttributes into the envelope's header map.
//
// HeaderIdempotencyKey is deliberately EXCLUDED from the table: as of
// WP-SQS-LIFT the idempotency key supplied as an x-bridge.idempotency-key
// message attribute is LIFTED into the envelope's first-class field on
// ingress (the sanctioned producer-supplied path — see messaging.NewEnvelope
// and the amqp10 adapter), so it legitimately appears on the envelope rather
// than being stripped. That lift is pinned by dedup_lift_test.go.
// HeaderOrderingKey and HeaderDeduplicationID REMAIN in the table because
// dedup/ordering ride the native FIFO fields (MessageDeduplicationId /
// MessageGroupId), never a message attribute, so a spoofed x-bridge.* dedup
// or ordering attribute must still be stripped.
func TestSQSInbound_ReservedConstants_Table(t *testing.T) {
	reserved := []string{
		messaging.HeaderCorrelationID,
		messaging.HeaderCausationID,
		// HeaderIdempotencyKey intentionally omitted — lifted on ingress
		// (WP-SQS-LIFT); its survival is asserted in dedup_lift_test.go.
		messaging.HeaderContentType,
		messaging.HeaderSourceID,
		messaging.HeaderRouteID,
		messaging.HeaderOrderingKey,
		messaging.HeaderDeduplicationID,
		messaging.HeaderTenantID,
		messaging.HeaderRouteOverride,
		messaging.HeaderForwardedFrom,
		messaging.HeaderForwardedHop,
	}
	for _, key := range reserved {
		t.Run(key, func(t *testing.T) {
			r := receiverForTest(t, false)
			msg := sqstypes.Message{
				MessageId:     aws.String("m"),
				ReceiptHandle: aws.String("rh"),
				Body:          aws.String("b"),
				MessageAttributes: map[string]sqstypes.MessageAttributeValue{
					key: {DataType: aws.String("String"), StringValue: aws.String("forged")},
				},
			}
			env, _, err := r.convertMessage(context.Background(), "http://test/queue", msg)
			if err != nil {
				t.Fatalf("convertMessage: %v", err)
			}
			if _, ok := env.Headers()[key]; ok {
				t.Errorf("reserved key %q survived strip", key)
			}
		})
	}
}
