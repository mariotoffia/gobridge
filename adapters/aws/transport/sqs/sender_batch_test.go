package sqs

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// ---------------------------------------------------------------------------
// BUG-2: SendBatch continues on partial failure
//
// These tests verify that SendBatch keeps dispatching remaining chunks
// after a partial or chunk-level failure. Under the ports.BatchSender
// result contract the per-message outcome is reported in the returned
// []ports.BatchResult (nil Err = sent); the top-level error stays nil
// once dispatch begins.
// ---------------------------------------------------------------------------

// Verifies all batches succeed when the API reports no failures.
// Every result entry is nil-Err so the success count equals the input.
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
	results, err := sender.SendBatch(context.Background(), func() []ports.OutboundMessage {
		_msgs := make([]ports.OutboundMessage, len(envs))
		for _i, _e := range envs {
			_msgs[_i] = ports.OutboundMessage{Envelope: _e}
		}
		return _msgs
	}())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 13 {
		t.Fatalf("expected 13 results, got %d", len(results))
	}
	if sent := batchSent(results); sent != 13 {
		t.Fatalf("expected 13 sent, got %d", sent)
	}
	if batchCalls != 3 {
		t.Fatalf("expected 3 batch calls (5+5+3), got %d", batchCalls)
	}
}

// Verifies that a partial failure in the first batch does not prevent
// subsequent batches from executing. The success count must include
// successes from all batches, and the failed index carries its error.
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
	// 2 from first batch + 3 + 3 from remaining = 8.
	if sent := batchSent(results); sent != 8 {
		t.Fatalf("expected 8 sent, got %d", sent)
	}
	if results[2].Err == nil {
		t.Fatal("expected the failed first-batch entry (index 2) to carry an error")
	}
	if callNum != 3 {
		t.Fatalf("expected 3 batch API calls, got %d", callNum)
	}
}

// Verifies that a full API error on a middle batch does not prevent
// batches before and after from executing. The failed chunk's entries
// each carry the recoverable error.
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
	// Batch 1: 3 ok, Batch 2: API error (0), Batch 3: 3 ok = 6.
	if sent := batchSent(results); sent != 6 {
		t.Fatalf("expected 6 sent, got %d", sent)
	}
	if callNum != 3 {
		t.Fatalf("expected all 3 batch calls to execute, got %d", callNum)
	}
	// Middle chunk is input indices 3,4,5; each carries the recoverable error.
	if results[3].Err == nil || !shared.IsRecoverableError(results[3].Err) {
		t.Fatalf("ServiceUnavailable should surface as a recoverable per-entry error, got %v", results[3].Err)
	}
}

// Verifies that when all batches fail every result carries its error and
// the success count is zero, while all chunks are still attempted.
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
	results, sendErr := sender.SendBatch(context.Background(), func() []ports.OutboundMessage {
		_msgs := make([]ports.OutboundMessage, len(envs))
		for _i, _e := range envs {
			_msgs[_i] = ports.OutboundMessage{Envelope: _e}
		}
		return _msgs
	}())
	if sendErr != nil {
		t.Fatalf("dispatched batch must not yield a whole-batch error, got %v", sendErr)
	}
	if sent := batchSent(results); sent != 0 {
		t.Fatalf("expected 0 sent when all batches fail, got %d", sent)
	}
	if callNum != 3 {
		t.Fatalf("expected 3 batch calls even if all fail, got %d", callNum)
	}
	// Every entry across every chunk surfaces its (recoverable) failure.
	for i, r := range results {
		if r.Err == nil {
			t.Fatalf("expected result %d to carry an error", i)
		}
		if !shared.IsRecoverableError(r.Err) {
			t.Fatalf("ServiceUnavailable should be recoverable, result %d = %v", i, r.Err)
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
	results, sendErr := sender.SendBatch(context.Background(), func() []ports.OutboundMessage {
		_msgs := make([]ports.OutboundMessage, len(envs))
		for _i, _e := range envs {
			_msgs[_i] = ports.OutboundMessage{Envelope: _e}
		}
		return _msgs
	}())
	if sendErr != nil {
		t.Fatalf("partial + API failures must not yield a whole-batch error, got %v", sendErr)
	}
	// Batch 1: 2 ok, Batch 2: 1 ok, Batch 3: 0 (API err), Batch 4: 2 ok = 5.
	if sent := batchSent(results); sent != 5 {
		t.Fatalf("expected 5 sent, got %d", sent)
	}
	// Failed input indices: 3 (partial), 4 and 5 (API error chunk).
	for _, idx := range []int{3, 4, 5} {
		if results[idx].Err == nil {
			t.Fatalf("expected result %d to carry an error", idx)
		}
	}
	if callNum != 4 {
		t.Fatalf("expected 4 batch calls, got %d", callNum)
	}
}

// Verifies a single-envelope batch with failure is handled correctly:
// the lone result carries the error and the success count is zero.
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

	envs := []*messaging.Envelope{messaging.MustEnvelope(messaging.EnvelopeInput{ID: "solo", Payload: []byte("x")})}
	results, sendErr := sender.SendBatch(context.Background(), func() []ports.OutboundMessage {
		_msgs := make([]ports.OutboundMessage, len(envs))
		for _i, _e := range envs {
			_msgs[_i] = ports.OutboundMessage{Envelope: _e}
		}
		return _msgs
	}())
	if sendErr != nil {
		t.Fatalf("dispatched batch must not yield a whole-batch error, got %v", sendErr)
	}
	if len(results) != 1 || results[0].Err == nil {
		t.Fatalf("expected the single result to carry an error, got %+v", results)
	}
	if sent := batchSent(results); sent != 0 {
		t.Fatalf("expected 0 sent, got %d", sent)
	}
}

// Verifies empty input returns no error, no successes, and no API calls.
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

	results, err := sender.SendBatch(context.Background(), []ports.OutboundMessage(nil))
	if err != nil {
		t.Fatalf("unexpected error for empty input: %v", err)
	}
	if sent := batchSent(results); sent != 0 {
		t.Fatalf("expected 0 sent for empty input, got %d", sent)
	}
	if batchCalls != 0 {
		t.Fatalf("expected 0 batch calls for empty input, got %d", batchCalls)
	}

	// Also test with empty slice (non-nil).
	results, err = sender.SendBatch(context.Background(), []ports.OutboundMessage{})
	if err != nil {
		t.Fatalf("unexpected error for empty slice: %v", err)
	}
	if sent := batchSent(results); sent != 0 {
		t.Fatalf("expected 0 sent for empty slice, got %d", sent)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func makeEnvelopes(n int) []*messaging.Envelope {
	envs := make([]*messaging.Envelope, n)
	for i := range envs {
		envs[i] = messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      fmt.Sprintf("env-%d", i),
			Payload: []byte(fmt.Sprintf("payload-%d", i)),
		})
	}
	return envs
}

// batchSent counts the successful (nil-Err) entries in a SendBatch
// result, i.e. the number of messages the transport accepted.
func batchSent(results []ports.BatchResult) int {
	n := 0
	for _, r := range results {
		if r.Err == nil {
			n++
		}
	}
	return n
}
