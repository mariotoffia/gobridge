package sqs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/shared"
)

// ---------------------------------------------------------------------------
// BUG-2: SendBatch continues on partial failure
//
// These tests verify that SendBatch collects errors and continues
// processing remaining batches instead of returning on first failure.
// ---------------------------------------------------------------------------

// Verifies all batches succeed when the API reports no failures.
// Total sent count must equal the number of input envelopes.
func TestSendBatch_AllBatchesSucceed(t *testing.T) {
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

	envs := makeEnvelopes(13)
	sent, err := sender.SendBatch(context.Background(), envs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sent != 13 {
		t.Fatalf("expected 13 sent, got %d", sent)
	}
	if batchCalls != 3 {
		t.Fatalf("expected 3 batch calls (5+5+3), got %d", batchCalls)
	}
}

// Verifies that a partial failure in the first batch does not prevent
// subsequent batches from executing. The sent count must include
// successes from all batches.
func TestSendBatch_FirstBatchPartialFailure_RemainingExecute(t *testing.T) {
	callNum := 0
	mock := &mockSQSClient{
		SendMessageBatchFn: func(_ context.Context, in *awssqs.SendMessageBatchInput, _ ...func(*awssqs.Options)) (*awssqs.SendMessageBatchOutput, error) {
			callNum++
			if callNum == 1 {
				// First batch: 3 entries, 2 succeed, 1 fails.
				return &awssqs.SendMessageBatchOutput{
					Successful: []sqstypes.SendMessageBatchResultEntry{
						{Id: in.Entries[0].Id},
						{Id: in.Entries[1].Id},
					},
					Failed: []sqstypes.BatchResultErrorEntry{
						{Id: aws.String("2"), Code: aws.String("InternalError"), Message: aws.String("transient")},
					},
				}, nil
			}
			// Remaining batches succeed fully.
			result := make([]sqstypes.SendMessageBatchResultEntry, len(in.Entries))
			for i := range in.Entries {
				result[i] = sqstypes.SendMessageBatchResultEntry{Id: in.Entries[i].Id}
			}
			return &awssqs.SendMessageBatchOutput{Successful: result}, nil
		},
	}
	sender, err := NewSender(SenderConfig{
		QueueURL:  "https://q",
		BatchSize: 3,
		Client:    mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	envs := makeEnvelopes(9) // 3 batches of 3
	sent, sendErr := sender.SendBatch(context.Background(), envs)
	if sendErr == nil {
		t.Fatal("expected error from partial failure in first batch")
	}
	// 2 from first batch + 3 + 3 from remaining = 8
	if sent != 8 {
		t.Fatalf("expected 8 sent, got %d", sent)
	}
	if callNum != 3 {
		t.Fatalf("expected 3 batch API calls, got %d", callNum)
	}
}

// Verifies that a full API error on a middle batch does not prevent
// batches before and after from executing.
func TestSendBatch_MiddleBatchAPIError_OtherBatchesExecute(t *testing.T) {
	callNum := 0
	mock := &mockSQSClient{
		SendMessageBatchFn: func(_ context.Context, in *awssqs.SendMessageBatchInput, _ ...func(*awssqs.Options)) (*awssqs.SendMessageBatchOutput, error) {
			callNum++
			if callNum == 2 {
				return nil, errors.New("ServiceUnavailable")
			}
			result := make([]sqstypes.SendMessageBatchResultEntry, len(in.Entries))
			for i := range in.Entries {
				result[i] = sqstypes.SendMessageBatchResultEntry{Id: in.Entries[i].Id}
			}
			return &awssqs.SendMessageBatchOutput{Successful: result}, nil
		},
	}
	sender, err := NewSender(SenderConfig{
		QueueURL:  "https://q",
		BatchSize: 3,
		Client:    mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	envs := makeEnvelopes(9) // 3 batches of 3
	sent, sendErr := sender.SendBatch(context.Background(), envs)
	if sendErr == nil {
		t.Fatal("expected error from middle batch API failure")
	}
	// Batch 1: 3 ok, Batch 2: API error (0), Batch 3: 3 ok = 6
	if sent != 6 {
		t.Fatalf("expected 6 sent, got %d", sent)
	}
	if callNum != 3 {
		t.Fatalf("expected all 3 batch calls to execute, got %d", callNum)
	}
	if !shared.IsRecoverableError(sendErr) {
		t.Fatal("ServiceUnavailable should be recoverable")
	}
}

// Verifies that when all batches fail the error contains all failures
// and the sent count is zero.
func TestSendBatch_AllBatchesFail_CombinedError(t *testing.T) {
	callNum := 0
	mock := &mockSQSClient{
		SendMessageBatchFn: func(_ context.Context, _ *awssqs.SendMessageBatchInput, _ ...func(*awssqs.Options)) (*awssqs.SendMessageBatchOutput, error) {
			callNum++
			return nil, fmt.Errorf("batch-%d: ServiceUnavailable", callNum)
		},
	}
	sender, err := NewSender(SenderConfig{
		QueueURL:  "https://q",
		BatchSize: 2,
		Client:    mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	envs := makeEnvelopes(6) // 3 batches of 2
	sent, sendErr := sender.SendBatch(context.Background(), envs)
	if sendErr == nil {
		t.Fatal("expected combined error when all batches fail")
	}
	if sent != 0 {
		t.Fatalf("expected 0 sent when all batches fail, got %d", sent)
	}
	if callNum != 3 {
		t.Fatalf("expected 3 batch calls even if all fail, got %d", callNum)
	}
	// Verify combined error contains messages from all failures.
	errMsg := sendErr.Error()
	for i := 1; i <= 3; i++ {
		needle := fmt.Sprintf("batch-%d", i)
		if !strings.Contains(errMsg, needle) {
			t.Fatalf("combined error should contain %q, got: %s", needle, errMsg)
		}
	}
}

// Verifies mixed failures: some batches have partial failures, one has
// an API error, and one succeeds fully.
func TestSendBatch_MixedPartialAndAPIErrors(t *testing.T) {
	callNum := 0
	mock := &mockSQSClient{
		SendMessageBatchFn: func(_ context.Context, in *awssqs.SendMessageBatchInput, _ ...func(*awssqs.Options)) (*awssqs.SendMessageBatchOutput, error) {
			callNum++
			switch callNum {
			case 1:
				// Full success.
				result := make([]sqstypes.SendMessageBatchResultEntry, len(in.Entries))
				for i := range in.Entries {
					result[i] = sqstypes.SendMessageBatchResultEntry{Id: in.Entries[i].Id}
				}
				return &awssqs.SendMessageBatchOutput{Successful: result}, nil
			case 2:
				// Partial failure: 1 of 2 succeeds.
				return &awssqs.SendMessageBatchOutput{
					Successful: []sqstypes.SendMessageBatchResultEntry{
						{Id: in.Entries[0].Id},
					},
					Failed: []sqstypes.BatchResultErrorEntry{
						{Id: aws.String("1"), Code: aws.String("InternalError"), Message: aws.String("partial")},
					},
				}, nil
			case 3:
				// Full API error.
				return nil, errors.New("ServiceUnavailable")
			case 4:
				// Full success.
				result := make([]sqstypes.SendMessageBatchResultEntry, len(in.Entries))
				for i := range in.Entries {
					result[i] = sqstypes.SendMessageBatchResultEntry{Id: in.Entries[i].Id}
				}
				return &awssqs.SendMessageBatchOutput{Successful: result}, nil
			}
			t.Fatalf("unexpected batch call %d", callNum)
			return nil, nil
		},
	}
	sender, err := NewSender(SenderConfig{
		QueueURL:  "https://q",
		BatchSize: 2,
		Client:    mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	envs := makeEnvelopes(8) // 4 batches of 2
	sent, sendErr := sender.SendBatch(context.Background(), envs)
	if sendErr == nil {
		t.Fatal("expected combined error from partial + API failures")
	}
	// Batch 1: 2 ok, Batch 2: 1 ok, Batch 3: 0 (API err), Batch 4: 2 ok = 5
	if sent != 5 {
		t.Fatalf("expected 5 sent, got %d", sent)
	}
	if callNum != 4 {
		t.Fatalf("expected 4 batch calls, got %d", callNum)
	}
}

// Verifies a single-envelope batch with failure is handled correctly.
func TestSendBatch_SingleEnvelopeFailure(t *testing.T) {
	mock := &mockSQSClient{
		SendMessageBatchFn: func(_ context.Context, _ *awssqs.SendMessageBatchInput, _ ...func(*awssqs.Options)) (*awssqs.SendMessageBatchOutput, error) {
			return &awssqs.SendMessageBatchOutput{
				Failed: []sqstypes.BatchResultErrorEntry{
					{Id: aws.String("0"), Code: aws.String("InternalError"), Message: aws.String("only entry failed")},
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

	envs := []*domain.Envelope{{ID: "solo", Payload: []byte("x")}}
	sent, sendErr := sender.SendBatch(context.Background(), envs)
	if sendErr == nil {
		t.Fatal("expected error for single-envelope failure")
	}
	if sent != 0 {
		t.Fatalf("expected 0 sent, got %d", sent)
	}
}

// Verifies empty input returns no error and zero sent.
func TestSendBatch_EmptyInput(t *testing.T) {
	batchCalls := 0
	mock := &mockSQSClient{
		SendMessageBatchFn: func(_ context.Context, _ *awssqs.SendMessageBatchInput, _ ...func(*awssqs.Options)) (*awssqs.SendMessageBatchOutput, error) {
			batchCalls++
			return &awssqs.SendMessageBatchOutput{}, nil
		},
	}
	sender, err := NewSender(SenderConfig{
		QueueURL: "https://q",
		Client:   mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	sent, err := sender.SendBatch(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error for empty input: %v", err)
	}
	if sent != 0 {
		t.Fatalf("expected 0 sent for empty input, got %d", sent)
	}
	if batchCalls != 0 {
		t.Fatalf("expected 0 batch calls for empty input, got %d", batchCalls)
	}

	// Also test with empty slice (non-nil).
	sent, err = sender.SendBatch(context.Background(), []*domain.Envelope{})
	if err != nil {
		t.Fatalf("unexpected error for empty slice: %v", err)
	}
	if sent != 0 {
		t.Fatalf("expected 0 sent for empty slice, got %d", sent)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func makeEnvelopes(n int) []*domain.Envelope {
	envs := make([]*domain.Envelope, n)
	for i := range envs {
		envs[i] = &domain.Envelope{
			ID:      fmt.Sprintf("env-%d", i),
			Payload: []byte(fmt.Sprintf("payload-%d", i)),
		}
	}
	return envs
}
