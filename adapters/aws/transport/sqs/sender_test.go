package sqs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/mariotoffia/gobridge/domain"
)

// Verifies Send maps envelope body, subject, and headers to SendMessage input.
func TestSender_Send_Basic(t *testing.T) {
	mock := &mockSQSClient{}
	sender, err := NewSender(SenderConfig{
		QueueURL: "https://q",
		Client:   mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	env := &domain.Envelope{
		ID:      "env-1",
		Subject: "test-subject",
		Payload: []byte(`{"msg":"hello"}`),
		Headers: map[string]any{
			"custom": "value",
		},
	}

	if err := sender.Send(context.Background(), env); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if len(mock.SendCalls) != 1 {
		t.Fatalf("expected 1 SendMessage call, got %d", len(mock.SendCalls))
	}

	input := mock.SendCalls[0]
	if *input.QueueUrl != "https://q" {
		t.Fatal("wrong queue URL")
	}
	if *input.MessageBody != `{"msg":"hello"}` {
		t.Fatal("wrong message body")
	}
	if input.MessageAttributes["Subject"].StringValue == nil || *input.MessageAttributes["Subject"].StringValue != "test-subject" {
		t.Fatal("Subject not set as message attribute")
	}
	if input.MessageAttributes["custom"].StringValue == nil || *input.MessageAttributes["custom"].StringValue != "value" {
		t.Fatal("custom header not mapped")
	}
}

// Verifies FIFO sends set MessageGroupId and MessageDeduplicationId from headers.
func TestSender_Send_FIFO_WithHeaders(t *testing.T) {
	mock := &mockSQSClient{}
	sender, err := NewSender(SenderConfig{
		QueueURL: "https://q.fifo",
		FIFO:     true,
		Client:   mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	env := &domain.Envelope{
		ID:      "env-fifo",
		Payload: []byte("fifo-msg"),
		Headers: map[string]any{
			domain.HeaderOrderingKey:     "group-A",
			domain.HeaderDeduplicationID: "dedup-123",
		},
	}

	if err := sender.Send(context.Background(), env); err != nil {
		t.Fatalf("Send: %v", err)
	}

	input := mock.SendCalls[0]
	if input.MessageGroupId == nil || *input.MessageGroupId != "group-A" {
		t.Fatalf("MessageGroupId: got %v, want group-A", input.MessageGroupId)
	}
	if input.MessageDeduplicationId == nil || *input.MessageDeduplicationId != "dedup-123" {
		t.Fatalf("MessageDeduplicationId: got %v, want dedup-123", input.MessageDeduplicationId)
	}
}

// Verifies FIFO sends use configured default group ID and auto-generated deduplication ID.
func TestSender_Send_FIFO_DefaultGroup(t *testing.T) {
	mock := &mockSQSClient{}
	sender, err := NewSender(SenderConfig{
		QueueURL:       "https://q.fifo",
		MessageGroupID: "default-group",
		Client:         mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	env := &domain.Envelope{
		ID:      "env-fifo-default",
		Payload: []byte("msg"),
	}

	if err := sender.Send(context.Background(), env); err != nil {
		t.Fatalf("Send: %v", err)
	}

	input := mock.SendCalls[0]
	if input.MessageGroupId == nil || *input.MessageGroupId != "default-group" {
		t.Fatal("should use default MessageGroupID from config")
	}
	if input.MessageDeduplicationId == nil || *input.MessageDeduplicationId == "" {
		t.Fatal("should auto-generate dedup ID for FIFO")
	}
}

// Verifies header ordering key overrides the configured default MessageGroupID.
func TestSender_Send_FIFO_HeaderOverridesDefault(t *testing.T) {
	mock := &mockSQSClient{}
	sender, err := NewSender(SenderConfig{
		QueueURL:       "https://q.fifo",
		MessageGroupID: "default-group",
		Client:         mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	env := &domain.Envelope{
		ID:      "env-fifo-override",
		Payload: []byte("msg"),
		Headers: map[string]any{
			domain.HeaderOrderingKey: "override-group",
		},
	}

	if err := sender.Send(context.Background(), env); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if *mock.SendCalls[0].MessageGroupId != "override-group" {
		t.Fatal("header should override default group ID")
	}
}

// Verifies configured delay seconds are applied on SendMessage.
func TestSender_Send_WithDelay(t *testing.T) {
	mock := &mockSQSClient{}
	sender, err := NewSender(SenderConfig{
		QueueURL:     "https://q",
		DelaySeconds: 15,
		Client:       mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	env := &domain.Envelope{ID: "env-delay", Payload: []byte("delayed")}
	if err := sender.Send(context.Background(), env); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if mock.SendCalls[0].DelaySeconds != 15 {
		t.Fatalf("expected delay 15, got %d", mock.SendCalls[0].DelaySeconds)
	}
}

// Verifies Send maps ServiceUnavailable-style failures to recoverable domain errors.
func TestSender_Send_Error(t *testing.T) {
	mock := &mockSQSClient{
		SendMessageFn: func(_ context.Context, _ *awssqs.SendMessageInput, _ ...func(*awssqs.Options)) (*awssqs.SendMessageOutput, error) {
			return nil, errors.New("ServiceUnavailable")
		},
	}
	sender, err := NewSender(SenderConfig{
		QueueURL: "https://q",
		Client:   mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	env := &domain.Envelope{ID: "env-err", Payload: []byte("fail")}
	sendErr := sender.Send(context.Background(), env)
	if sendErr == nil {
		t.Fatal("expected error")
	}
	if !domain.IsRecoverableError(sendErr) {
		t.Fatal("ServiceUnavailable should be recoverable")
	}
}

// Verifies SendBatch sends all envelopes in one batch when under the limit.
func TestSender_SendBatch_Basic(t *testing.T) {
	mock := &mockSQSClient{
		SendMessageBatchFn: func(_ context.Context, in *awssqs.SendMessageBatchInput, _ ...func(*awssqs.Options)) (*awssqs.SendMessageBatchOutput, error) {
			result := make([]sqstypes.SendMessageBatchResultEntry, len(in.Entries))
			for i := range in.Entries {
				result[i] = sqstypes.SendMessageBatchResultEntry{
					Id: in.Entries[i].Id,
				}
			}
			return &awssqs.SendMessageBatchOutput{Successful: result}, nil
		},
	}

	sender, err := NewSender(SenderConfig{
		QueueURL: "https://q",
		Client:   mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	envs := make([]*domain.Envelope, 3)
	for i := range envs {
		envs[i] = &domain.Envelope{
			ID:      "batch-" + string(rune('0'+i)),
			Payload: []byte("msg"),
		}
	}

	sent, err := sender.SendBatch(context.Background(), envs)
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if sent != 3 {
		t.Fatalf("expected 3 sent, got %d", sent)
	}
}

// Verifies partial batch failures return an error while reporting successful entry count.
func TestSender_SendBatch_PartialFailure(t *testing.T) {
	mock := &mockSQSClient{
		SendMessageBatchFn: func(_ context.Context, in *awssqs.SendMessageBatchInput, _ ...func(*awssqs.Options)) (*awssqs.SendMessageBatchOutput, error) {
			return &awssqs.SendMessageBatchOutput{
				Successful: []sqstypes.SendMessageBatchResultEntry{
					{Id: in.Entries[0].Id},
				},
				Failed: []sqstypes.BatchResultErrorEntry{
					{Id: aws.String("1"), Code: aws.String("InternalError"), Message: aws.String("internal"), SenderFault: false},
				},
			}, nil
		},
	}

	sender, err := NewSender(SenderConfig{
		QueueURL: "https://q",
		Client:   mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	envs := []*domain.Envelope{
		{ID: "ok", Payload: []byte("ok")},
		{ID: "fail", Payload: []byte("fail")},
	}

	sent, sendErr := sender.SendBatch(context.Background(), envs)
	if sendErr == nil {
		t.Fatal("expected error on partial failure")
	}
	if sent != 1 {
		t.Fatalf("expected 1 successful, got %d", sent)
	}
}

// Verifies SendBatch splits sends across multiple API calls according to BatchSize.
func TestSender_SendBatch_LargeBatch(t *testing.T) {
	batchCalls := 0
	mock := &mockSQSClient{
		SendMessageBatchFn: func(_ context.Context, in *awssqs.SendMessageBatchInput, _ ...func(*awssqs.Options)) (*awssqs.SendMessageBatchOutput, error) {
			batchCalls++
			result := make([]sqstypes.SendMessageBatchResultEntry, len(in.Entries))
			for i := range in.Entries {
				result[i] = sqstypes.SendMessageBatchResultEntry{Id: in.Entries[i].Id}
			}
			return &awssqs.SendMessageBatchOutput{Successful: result}, nil
		},
	}

	sender, err := NewSender(SenderConfig{
		QueueURL:  "https://q",
		BatchSize: 5,
		Client:    mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	envs := make([]*domain.Envelope, 12)
	for i := range envs {
		envs[i] = &domain.Envelope{ID: "lg", Payload: []byte("x")}
	}

	sent, err := sender.SendBatch(context.Background(), envs)
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if sent != 12 {
		t.Fatalf("expected 12 sent, got %d", sent)
	}
	if batchCalls != 3 {
		t.Fatalf("expected 3 batch calls (5+5+2), got %d", batchCalls)
	}
}

// Verifies NewSender rejects configuration without a queue URL.
func TestSender_Validate_RequiresQueue(t *testing.T) {
	_, err := NewSender(SenderConfig{})
	if err == nil {
		t.Fatal("expected error when no queue specified")
	}
}

// Verifies applyDefaults sets expected batch size and timeout.
func TestSender_ConfigDefaults(t *testing.T) {
	cfg := SenderConfig{QueueURL: "https://q"}
	cfg.applyDefaults()

	if cfg.BatchSize != 10 {
		t.Fatalf("default BatchSize: got %d, want 10", cfg.BatchSize)
	}
	if cfg.Timeout != 30*time.Second {
		t.Fatalf("default Timeout: got %v, want 30s", cfg.Timeout)
	}
}

// Verifies applyDefaults clamps invalid delay and batch size to supported ranges.
func TestSender_ConfigDefaults_Clamps(t *testing.T) {
	cfg := SenderConfig{QueueURL: "https://q", DelaySeconds: -5, BatchSize: 50}
	cfg.applyDefaults()

	if cfg.DelaySeconds != 0 {
		t.Fatalf("negative delay should clamp to 0, got %d", cfg.DelaySeconds)
	}
	if cfg.BatchSize != 10 {
		t.Fatalf("batch > 10 should clamp to 10, got %d", cfg.BatchSize)
	}
}

// Verifies generateDeduplicationID is stable for the same envelope content.
func TestGenerateDeduplicationID_Deterministic(t *testing.T) {
	env := &domain.Envelope{
		ID:      "msg-1",
		Subject: "test",
		Payload: []byte("data"),
	}

	id1 := generateDeduplicationID(env)
	id2 := generateDeduplicationID(env)

	if id1 != id2 {
		t.Fatalf("dedup ID should be deterministic, got %q and %q", id1, id2)
	}
}

// Verifies generateDeduplicationID changes when the payload differs.
func TestGenerateDeduplicationID_DiffersOnPayload(t *testing.T) {
	env1 := &domain.Envelope{ID: "msg-1", Payload: []byte("a")}
	env2 := &domain.Envelope{ID: "msg-1", Payload: []byte("b")}

	if generateDeduplicationID(env1) == generateDeduplicationID(env2) {
		t.Fatal("different payloads should produce different dedup IDs")
	}
}
