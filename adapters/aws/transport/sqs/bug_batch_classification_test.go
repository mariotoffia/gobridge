package sqs

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// BUG-6: SQS Batch Error Classification
//
// SendBatch wraps ALL batch failures with shared.ErrUnavailable (Transient)
// regardless of the SenderFault flag. When SenderFault=true, the error
// should be Permanent (client's request was malformed).
// ═══════════════════════════════════════════════════════════════════════════

// TestBug6_SendBatch_SenderFaultClassifiedAsPermanent verifies that batch
// failures with SenderFault=true are classified as Permanent (not Transient).
func TestBug6_SendBatch_SenderFaultClassifiedAsPermanent(t *testing.T) {
	mock := &mockSQSClient{
		SendMessageBatchFn: func(_ context.Context, _ *awssqs.SendMessageBatchInput, _ ...func(*awssqs.Options)) (*awssqs.SendMessageBatchOutput, error) {
			return &awssqs.SendMessageBatchOutput{
				Successful: []sqstypes.SendMessageBatchResultEntry{
					{Id: aws.String("0")},
				},
				Failed: []sqstypes.BatchResultErrorEntry{
					{
						Id:          aws.String("1"),
						SenderFault: true,
						Code:        aws.String("InvalidParameterValue"),
						Message:     aws.String("invalid message body"),
					},
				},
			}, nil
		},
	}

	s := &Sender{
		queueURL: "https://sqs.us-west-1.amazonaws.com/123/q",
		cfg:      SenderConfig{Timeout: 10 * time.Second, BatchSize: 10},
		metrics:  &ports.NoopExporter{},
	}
	s.storeClient(mock)

	envs := []*messaging.Envelope{
		messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-0", Payload: []byte(`{"ok":true}`)}),
		messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-1", Payload: []byte(`{"bad":true}`)}),
	}

	results, err := s.SendBatch(context.Background(), func() []ports.OutboundMessage {
		_msgs := make([]ports.OutboundMessage, len(envs))
		for _i, _e := range envs {
			_msgs[_i] = ports.OutboundMessage{Envelope: _e}
		}
		return _msgs
	}())
	if err != nil {
		t.Fatalf("dispatched batch must not yield a whole-batch error, got %v", err)
	}
	if sent := batchSent(results); sent != 1 {
		t.Fatalf("expected 1 sent, got %d", sent)
	}

	// FIX VERIFIED: SenderFault=true now uses ErrInvalidPayload (Rejected).
	// ErrorRejected is a non-retriable, payload-level error — semantically
	// correct for SQS sender faults (malformed requests). The failed entry
	// is input index 1; its per-message error carries the classification.
	if results[1].Err == nil {
		t.Fatal("expected the sender-fault entry (index 1) to carry an error")
	}
	be, ok := shared.AsBridgeError(results[1].Err)
	if !ok {
		t.Fatalf("expected BridgeError, got %T: %v", results[1].Err, results[1].Err)
	}
	if be.Class != shared.ErrorRejected {
		t.Errorf("expected ErrorRejected for SenderFault=true, got %s", be.Class)
	}
	t.Logf("BUG-6 FIX VERIFIED: SenderFault=true classified as %s", be.Class)
}

// TestBug6_SendBatch_AllFailuresCorrectClassification verifies that server
// faults are classified as Transient and sender faults as Rejected.
func TestBug6_SendBatch_AllFailuresCorrectClassification(t *testing.T) {
	tests := []struct {
		name          string
		senderFault   bool
		code          string
		expectedClass shared.ErrorClass
	}{
		{"server fault", false, "InternalError", shared.ErrorTransient},
		{"sender fault", true, "InvalidParameterValue", shared.ErrorRejected},
		{"access denied", true, "AccessDenied", shared.ErrorRejected},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mock := &mockSQSClient{
				SendMessageBatchFn: func(_ context.Context, _ *awssqs.SendMessageBatchInput, _ ...func(*awssqs.Options)) (*awssqs.SendMessageBatchOutput, error) {
					return &awssqs.SendMessageBatchOutput{
						Failed: []sqstypes.BatchResultErrorEntry{
							{
								Id:          aws.String("0"),
								SenderFault: tc.senderFault,
								Code:        aws.String(tc.code),
								Message:     aws.String("test error"),
							},
						},
					}, nil
				},
			}

			s := &Sender{
				queueURL: "https://sqs.us-west-1.amazonaws.com/123/q",
				cfg:      SenderConfig{Timeout: 10 * time.Second, BatchSize: 10},
				metrics:  &ports.NoopExporter{},
			}
			s.storeClient(mock)

			envs := []*messaging.Envelope{
				messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-0", Payload: []byte(`{"test":true}`)}),
			}

			results, err := s.SendBatch(context.Background(), func() []ports.OutboundMessage {
				_msgs := make([]ports.OutboundMessage, len(envs))
				for _i, _e := range envs {
					_msgs[_i] = ports.OutboundMessage{Envelope: _e}
				}
				return _msgs
			}())
			if err != nil {
				t.Fatalf("dispatched batch must not yield a whole-batch error, got %v", err)
			}
			if results[0].Err == nil {
				t.Fatal("expected the failed entry (index 0) to carry an error")
			}

			be, ok := shared.AsBridgeError(results[0].Err)
			if !ok {
				t.Fatalf("expected BridgeError, got %T: %v", results[0].Err, results[0].Err)
			}
			if be.Class != tc.expectedClass {
				t.Errorf("SenderFault=%v Code=%s: expected %s, got %s",
					tc.senderFault, tc.code, tc.expectedClass, be.Class)
			}
			t.Logf("BUG-6 FIX VERIFIED: SenderFault=%v Code=%s → %s",
				tc.senderFault, tc.code, be.Class)
		})
	}
}
