package sqs

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
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

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "env-1",
		Subject: "test-subject",
		Payload: []byte(`{"msg":"hello"}`),
		Headers: map[string]any{
			"custom": "value",
		},
	})

	if err := sender.Send(context.Background(), ports.OutboundMessage{Envelope: env}); err != nil {
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

	env := messaging.MustEnvelopeWithReserved(messaging.EnvelopeInput{
		ID:      "env-fifo",
		Payload: []byte("fifo-msg"),
		Headers: map[string]any{
			messaging.HeaderOrderingKey:     "group-A",
			messaging.HeaderDeduplicationID: "dedup-123",
		},
	})

	if err := sender.Send(context.Background(), ports.OutboundMessage{Envelope: env}); err != nil {
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

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "env-fifo-default",
		Payload: []byte("msg"),
	})

	if err := sender.Send(context.Background(), ports.OutboundMessage{Envelope: env}); err != nil {
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

	env := messaging.MustEnvelopeWithReserved(messaging.EnvelopeInput{
		ID:      "env-fifo-override",
		Payload: []byte("msg"),
		Headers: map[string]any{
			messaging.HeaderOrderingKey: "override-group",
		},
	})

	if err := sender.Send(context.Background(), ports.OutboundMessage{Envelope: env}); err != nil {
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

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-delay", Payload: []byte("delayed")})
	if err := sender.Send(context.Background(), ports.OutboundMessage{Envelope: env}); err != nil {
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

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "env-err", Payload: []byte("fail")})
	sendErr := sender.Send(context.Background(), ports.OutboundMessage{Envelope: env})
	if sendErr == nil {
		t.Fatal("expected error")
	}
	if !shared.IsRecoverableError(sendErr) {
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

	envs := make([]*messaging.Envelope, 3)
	for i := range envs {
		envs[i] = messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      "batch-" + string(rune('0'+i)),
			Payload: []byte("msg"),
		})
	}

	results, err := sender.SendBatch(context.Background(), func() []ports.OutboundMessage {
		_msgs := make([]ports.OutboundMessage, len(envs))
		for _i, _e := range envs {
			_msgs[_i] = ports.OutboundMessage{Envelope: _e}
		}
		return _msgs
	}())
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if sent := batchSent(results); sent != 3 {
		t.Fatalf("expected 3 sent, got %d", sent)
	}
}

// Verifies a partial batch failure surfaces per-entry errors (no whole-batch error) while other entries succeed.
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

	envs := []*messaging.Envelope{
		messaging.MustEnvelope(messaging.EnvelopeInput{ID: "ok", Payload: []byte("ok")}),
		messaging.MustEnvelope(messaging.EnvelopeInput{ID: "fail", Payload: []byte("fail")}),
	}

	results, sendErr := sender.SendBatch(context.Background(), func() []ports.OutboundMessage {
		_msgs := make([]ports.OutboundMessage, len(envs))
		for _i, _e := range envs {
			_msgs[_i] = ports.OutboundMessage{Envelope: _e}
		}
		return _msgs
	}())
	if sendErr != nil {
		t.Fatalf("partial failure must not yield a whole-batch error, got %v", sendErr)
	}
	if sent := batchSent(results); sent != 1 {
		t.Fatalf("expected 1 successful, got %d", sent)
	}
	if results[1].Err == nil {
		t.Fatal("expected the failed entry (index 1) to carry an error")
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

	envs := make([]*messaging.Envelope, 12)
	for i := range envs {
		envs[i] = messaging.MustEnvelope(messaging.EnvelopeInput{ID: "lg", Payload: []byte("x")})
	}

	results, err := sender.SendBatch(context.Background(), func() []ports.OutboundMessage {
		_msgs := make([]ports.OutboundMessage, len(envs))
		for _i, _e := range envs {
			_msgs[_i] = ports.OutboundMessage{Envelope: _e}
		}
		return _msgs
	}())
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if sent := batchSent(results); sent != 12 {
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

// Verifies that a partial failure in the first batch does not abort subsequent
// batches. All batches are attempted; total sent includes successes from all.
func TestSender_SendBatch_PartialFailure_ContinuesRemaining(t *testing.T) {
	callNum := 0
	mock := &mockSQSClient{
		SendMessageBatchFn: func(_ context.Context, in *awssqs.SendMessageBatchInput, _ ...func(*awssqs.Options)) (*awssqs.SendMessageBatchOutput, error) {
			callNum++
			if callNum == 1 {
				return &awssqs.SendMessageBatchOutput{
					Successful: []sqstypes.SendMessageBatchResultEntry{{Id: in.Entries[0].Id}},
					Failed:     []sqstypes.BatchResultErrorEntry{{Id: aws.String("1"), Code: aws.String("InternalError"), Message: aws.String("transient")}},
				}, nil
			}
			result := make([]sqstypes.SendMessageBatchResultEntry, len(in.Entries))
			for i := range in.Entries {
				result[i] = sqstypes.SendMessageBatchResultEntry{Id: in.Entries[i].Id}
			}
			return &awssqs.SendMessageBatchOutput{Successful: result}, nil
		},
	}
	sender, err := NewSender(SenderConfig{QueueURL: "https://q", BatchSize: 2, Client: mock})
	if err != nil {
		t.Fatal(err)
	}
	envs := make([]*messaging.Envelope, 4)
	for i := range envs {
		envs[i] = messaging.MustEnvelope(messaging.EnvelopeInput{ID: fmt.Sprintf("env-%d", i), Payload: []byte("msg")})
	}
	results, sendErr := sender.SendBatch(context.Background(), func() []ports.OutboundMessage {
		_msgs := make([]ports.OutboundMessage, len(envs))
		for _i, _e := range envs {
			_msgs[_i] = ports.OutboundMessage{Envelope: _e}
		}
		return _msgs
	}())
	if sendErr != nil {
		t.Fatalf("partial failure must not yield a whole-batch error, got %v", sendErr)
	}
	if sent := batchSent(results); sent != 3 {
		t.Fatalf("expected 3 sent (1+2), got %d", sent)
	}
	if results[1].Err == nil {
		t.Fatal("expected the failed first-batch entry (index 1) to carry an error")
	}
	if callNum != 2 {
		t.Fatalf("expected 2 batch API calls, got %d", callNum)
	}
}

// Verifies that a full API error on one batch does not abort remaining batches.
func TestSender_SendBatch_APIError_ContinuesRemaining(t *testing.T) {
	callNum := 0
	mock := &mockSQSClient{
		SendMessageBatchFn: func(_ context.Context, in *awssqs.SendMessageBatchInput, _ ...func(*awssqs.Options)) (*awssqs.SendMessageBatchOutput, error) {
			callNum++
			if callNum == 1 {
				return nil, errors.New("ServiceUnavailable")
			}
			result := make([]sqstypes.SendMessageBatchResultEntry, len(in.Entries))
			for i := range in.Entries {
				result[i] = sqstypes.SendMessageBatchResultEntry{Id: in.Entries[i].Id}
			}
			return &awssqs.SendMessageBatchOutput{Successful: result}, nil
		},
	}
	sender, err := NewSender(SenderConfig{QueueURL: "https://q", BatchSize: 3, Client: mock})
	if err != nil {
		t.Fatal(err)
	}
	envs := make([]*messaging.Envelope, 6)
	for i := range envs {
		envs[i] = messaging.MustEnvelope(messaging.EnvelopeInput{ID: fmt.Sprintf("env-%d", i), Payload: []byte("x")})
	}
	results, sendErr := sender.SendBatch(context.Background(), func() []ports.OutboundMessage {
		_msgs := make([]ports.OutboundMessage, len(envs))
		for _i, _e := range envs {
			_msgs[_i] = ports.OutboundMessage{Envelope: _e}
		}
		return _msgs
	}())
	if sendErr != nil {
		t.Fatalf("chunk API failure must not yield a whole-batch error, got %v", sendErr)
	}
	if sent := batchSent(results); sent != 3 {
		t.Fatalf("expected 3 sent from second batch, got %d", sent)
	}
	if callNum != 2 {
		t.Fatalf("expected 2 batch API calls, got %d", callNum)
	}
	if results[0].Err == nil || !shared.IsRecoverableError(results[0].Err) {
		t.Fatalf("ServiceUnavailable should surface as a recoverable per-entry error, got %v", results[0].Err)
	}
}

// Verifies each batch gets its own timeout (BUG-9 regression test).
func TestSender_SendBatch_PerBatchTimeout(t *testing.T) {
	var deadlines []time.Time
	mock := &mockSQSClient{
		SendMessageBatchFn: func(ctx context.Context, in *awssqs.SendMessageBatchInput, _ ...func(*awssqs.Options)) (*awssqs.SendMessageBatchOutput, error) {
			dl, ok := ctx.Deadline()
			if !ok {
				t.Fatal("expected deadline on batch context")
			}
			deadlines = append(deadlines, dl)
			time.Sleep(5 * time.Millisecond) // OTHER: ensure distinguishable deadlines
			result := make([]sqstypes.SendMessageBatchResultEntry, len(in.Entries))
			for i := range in.Entries {
				result[i] = sqstypes.SendMessageBatchResultEntry{Id: in.Entries[i].Id}
			}
			return &awssqs.SendMessageBatchOutput{Successful: result}, nil
		},
	}
	sender, err := NewSender(SenderConfig{QueueURL: "https://q", BatchSize: 2, Timeout: 100 * time.Millisecond, Client: mock})
	if err != nil {
		t.Fatal(err)
	}
	envs := make([]*messaging.Envelope, 4)
	for i := range envs {
		envs[i] = messaging.MustEnvelope(messaging.EnvelopeInput{ID: fmt.Sprintf("env-%d", i), Payload: []byte("x")})
	}
	results, err := sender.SendBatch(context.Background(), func() []ports.OutboundMessage {
		_msgs := make([]ports.OutboundMessage, len(envs))
		for _i, _e := range envs {
			_msgs[_i] = ports.OutboundMessage{Envelope: _e}
		}
		return _msgs
	}())
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if sent := batchSent(results); sent != 4 {
		t.Fatalf("expected 4 sent, got %d", sent)
	}
	if len(deadlines) != 2 {
		t.Fatalf("expected 2 batch calls, got %d", len(deadlines))
	}
	// Per-batch timeouts: second deadline must be later than first.
	if !deadlines[1].After(deadlines[0]) {
		t.Fatalf("second batch deadline (%v) should be after first (%v); shared timeout suspected", deadlines[1], deadlines[0])
	}
}

// Verifies generateDeduplicationID is stable for the same envelope content.
func TestGenerateDeduplicationID_Deterministic(t *testing.T) {
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "msg-1",
		Subject: "test",
		Payload: []byte("data"),
	})

	id1 := generateDeduplicationID(env)
	id2 := generateDeduplicationID(env)

	if id1 != id2 {
		t.Fatalf("dedup ID should be deterministic, got %q and %q", id1, id2)
	}
}

// Verifies generateDeduplicationID changes when the payload differs.
func TestGenerateDeduplicationID_DiffersOnPayload(t *testing.T) {
	env1 := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-1", Payload: []byte("a")})
	env2 := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-1", Payload: []byte("b")})

	if generateDeduplicationID(env1) == generateDeduplicationID(env2) {
		t.Fatal("different payloads should produce different dedup IDs")
	}
}
