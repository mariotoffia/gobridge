package sqs

// Production-readiness regression tests for FIFO sender
// misconfiguration: a ".fifo" target queue without any message-group
// configuration used to surface only at the broker as MissingParameter —
// classified transient (retried forever) on the single-send path but
// permanent (SenderFault) on the batch path. The fault is now caught at
// build time (NewSender) and, for per-envelope groups, rejected
// per-message before any SDK call with shared.ErrInvalidPayload.

import (
	"context"
	"errors"
	"testing"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// Verifies NewSender fails fast when the target queue is FIFO (".fifo"
// suffix on URL or name) but neither a default MessageGroupID nor the
// FIFO flag (per-envelope groups) is configured.
func TestNewSender_FIFOQueueRequiresGroupConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     SenderConfig
		wantErr bool
	}{
		{
			name:    "fifo URL without group config is rejected at build time",
			cfg:     SenderConfig{QueueURL: "https://sqs.us-east-1.amazonaws.com/123/orders.fifo"},
			wantErr: true,
		},
		{
			name:    "fifo queue name without group config is rejected at build time",
			cfg:     SenderConfig{QueueName: "orders.fifo"},
			wantErr: true,
		},
		{
			name: "fifo URL with default message group is accepted",
			cfg: SenderConfig{
				QueueURL:       "https://sqs.us-east-1.amazonaws.com/123/orders.fifo",
				MessageGroupID: "orders",
			},
			wantErr: false,
		},
		{
			name: "fifo URL with per-envelope groups (fifo: true) is accepted",
			cfg: SenderConfig{
				QueueURL: "https://sqs.us-east-1.amazonaws.com/123/orders.fifo",
				FIFO:     true,
			},
			wantErr: false,
		},
		{
			name:    "standard queue needs no group config",
			cfg:     SenderConfig{QueueURL: "https://sqs.us-east-1.amazonaws.com/123/orders"},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewSender(tt.cfg)
			if tt.wantErr && err == nil {
				t.Fatal("NewSender: expected a build-time FIFO configuration error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("NewSender: unexpected error: %v", err)
			}
		})
	}
}

// Verifies Send on a per-envelope-group FIFO sender rejects an envelope
// that carries no ordering-key header with shared.ErrInvalidPayload
// (permanent / rejected — never retried as transient) BEFORE any SDK
// call.
func TestSender_Send_FIFOMissingGroup_RejectedBeforeSDK(t *testing.T) {
	mock := &mockSQSClient{}
	s, err := NewSender(SenderConfig{
		QueueURL: "https://sqs.us-east-1.amazonaws.com/123/orders.fifo",
		FIFO:     true,
		Client:   mock,
	})
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "e-nogroup",
		Payload: []byte("p"),
	}) // no x-bridge.ordering-key header

	err = s.Send(context.Background(), ports.OutboundMessage{Envelope: env})
	if !errors.Is(err, shared.ErrInvalidPayload) {
		t.Fatalf("Send: got %v, want shared.ErrInvalidPayload", err)
	}
	if n := len(mock.SendCalls); n != 0 {
		t.Fatalf("SendMessage was called %d times; the rejection must happen before the SDK", n)
	}
}

// Verifies SendBatch pre-validation: one envelope without a resolvable
// message group rejects the whole batch with shared.ErrInvalidPayload
// and no SendMessageBatch dispatch — consistent with the single-send
// classification for the same deterministic config fault.
func TestSender_SendBatch_FIFOMissingGroup_FailsFastBeforeSDK(t *testing.T) {
	var batchCalls int
	mock := &mockSQSClient{
		SendMessageBatchFn: func(_ context.Context, _ *awssqs.SendMessageBatchInput, _ ...func(*awssqs.Options)) (*awssqs.SendMessageBatchOutput, error) {
			batchCalls++
			return &awssqs.SendMessageBatchOutput{}, nil
		},
	}
	s, err := NewSender(SenderConfig{
		QueueURL: "https://sqs.us-east-1.amazonaws.com/123/orders.fifo",
		FIFO:     true,
		Client:   mock,
	})
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	withGroup := messaging.MustEnvelopeWithReserved(messaging.EnvelopeInput{
		ID:      "e-ok",
		Payload: []byte("p"),
		Headers: map[string]any{messaging.HeaderOrderingKey: "group-A"},
	})
	withoutGroup := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "e-nogroup",
		Payload: []byte("p"),
	})

	results, err := s.SendBatch(context.Background(), []ports.OutboundMessage{
		{Envelope: withGroup},
		{Envelope: withoutGroup},
	})
	if results != nil {
		t.Fatalf("SendBatch results = %v, want nil on pre-validation failure", results)
	}
	if !errors.Is(err, shared.ErrInvalidPayload) {
		t.Fatalf("SendBatch: got %v, want shared.ErrInvalidPayload", err)
	}
	if batchCalls != 0 {
		t.Fatalf("SendMessageBatch was called %d times; pre-validation must fail fast", batchCalls)
	}
}

// Verifies the defense-in-depth mapping: should a MissingParameter still
// escape to the broker (e.g. a hand-built client bypassing validation),
// it is classified permanent (ErrInvalidPayload / rejected), matching
// the batch path's SenderFault classification — not transient.
func TestMapError_MissingParameter_IsPermanent(t *testing.T) {
	err := MapError(errors.New("MissingParameter: The request must contain the parameter MessageGroupId."))
	if !errors.Is(err, shared.ErrInvalidPayload) {
		t.Fatalf("got %v, want shared.ErrInvalidPayload", err)
	}
	if shared.IsRecoverableError(err) {
		t.Fatal("a FIFO config fault must not be classified recoverable")
	}
}
