package sqs

import (
	"context"
	"testing"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/mariotoffia/gobridge/ports"
)

// ---------------------------------------------------------------------------
// BUG-9: Per-batch timeout
//
// These tests verify that each batch in SendBatch gets its own
// context.WithTimeout instead of sharing a single timeout across all
// batches.
// ---------------------------------------------------------------------------

// Verifies each batch gets a fresh deadline derived from the configured
// timeout. The second batch deadline must be later than the first.
func TestSendBatch_PerBatchTimeout_FreshDeadline(t *testing.T) {
	var deadlines []time.Time
	mock := &mockSQSClient{
		SendMessageBatchFn: func(ctx context.Context, in *awssqs.SendMessageBatchInput, _ ...func(*awssqs.Options)) (*awssqs.SendMessageBatchOutput, error) {
			dl, ok := ctx.Deadline()
			if !ok {
				t.Fatal("expected deadline on batch context")
			}
			deadlines = append(deadlines, dl)
			time.Sleep(10 * time.Millisecond) // OTHER: shift wall clock for distinguishable deadlines
			result := make([]sqstypes.SendMessageBatchResultEntry, len(in.Entries))
			for i := range in.Entries {
				result[i] = sqstypes.SendMessageBatchResultEntry{Id: in.Entries[i].Id}
			}
			return &awssqs.SendMessageBatchOutput{Successful: result}, nil
		},
	}
	sender, err := NewSender(SenderConfig{
		QueueURL:  "https://q",
		BatchSize: 2,
		Timeout:   200 * time.Millisecond,
		Client:    mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	envs := makeEnvelopes(6) // 3 batches of 2
	sent, err := sender.SendBatch(context.Background(), func() []ports.OutboundMessage {
		_msgs := make([]ports.OutboundMessage, len(envs))
		for _i, _e := range envs {
			_msgs[_i] = ports.OutboundMessage{Envelope: _e}
		}
		return _msgs
	}())
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if sent != 6 {
		t.Fatalf("expected 6 sent, got %d", sent)
	}
	if len(deadlines) != 3 {
		t.Fatalf("expected 3 deadlines, got %d", len(deadlines))
	}
	for i := 1; i < len(deadlines); i++ {
		if !deadlines[i].After(deadlines[i-1]) {
			t.Fatalf("deadline[%d] (%v) should be after deadline[%d] (%v); "+
				"shared timeout suspected", i, deadlines[i], i-1, deadlines[i-1])
		}
	}
}

// Verifies that a timeout in one batch does not prevent subsequent
// batches from executing with a fresh timeout.
func TestSendBatch_TimeoutOneBatch_OthersUnaffected(t *testing.T) {
	callNum := 0
	mock := &mockSQSClient{
		SendMessageBatchFn: func(ctx context.Context, in *awssqs.SendMessageBatchInput, _ ...func(*awssqs.Options)) (*awssqs.SendMessageBatchOutput, error) {
			callNum++
			if callNum == 1 {
				// Simulate a slow call that exceeds the per-batch timeout.
				<-ctx.Done()
				return nil, ctx.Err()
			}
			// Remaining batches complete immediately.
			result := make([]sqstypes.SendMessageBatchResultEntry, len(in.Entries))
			for i := range in.Entries {
				result[i] = sqstypes.SendMessageBatchResultEntry{Id: in.Entries[i].Id}
			}
			return &awssqs.SendMessageBatchOutput{Successful: result}, nil
		},
	}
	sender, err := NewSender(SenderConfig{
		QueueURL:  "https://q",
		BatchSize: 2,
		Timeout:   50 * time.Millisecond,
		Client:    mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	envs := makeEnvelopes(4) // 2 batches of 2
	sent, sendErr := sender.SendBatch(context.Background(), func() []ports.OutboundMessage {
		_msgs := make([]ports.OutboundMessage, len(envs))
		for _i, _e := range envs {
			_msgs[_i] = ports.OutboundMessage{Envelope: _e}
		}
		return _msgs
	}())
	if sendErr == nil {
		t.Fatal("expected error from timed-out first batch")
	}
	if callNum != 2 {
		t.Fatalf("expected 2 batch calls, got %d", callNum)
	}
	// Second batch should succeed with its own timeout.
	if sent != 2 {
		t.Fatalf("expected 2 sent from second batch, got %d", sent)
	}
}

// Verifies that parent context cancellation propagates to all batches.
func TestSendBatch_ParentContextCancel_PropagatesAllBatches(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	callNum := 0
	mock := &mockSQSClient{
		SendMessageBatchFn: func(bctx context.Context, in *awssqs.SendMessageBatchInput, _ ...func(*awssqs.Options)) (*awssqs.SendMessageBatchOutput, error) {
			callNum++
			if callNum == 1 {
				// First batch succeeds, then we cancel parent.
				result := make([]sqstypes.SendMessageBatchResultEntry, len(in.Entries))
				for i := range in.Entries {
					result[i] = sqstypes.SendMessageBatchResultEntry{Id: in.Entries[i].Id}
				}
				cancel()
				return &awssqs.SendMessageBatchOutput{Successful: result}, nil
			}
			// Subsequent batches should see a cancelled context.
			select {
			case <-bctx.Done():
				return nil, bctx.Err()
			default:
				return nil, bctx.Err()
			}
		},
	}
	sender, err := NewSender(SenderConfig{
		QueueURL:  "https://q",
		BatchSize: 2,
		Timeout:   5 * time.Second,
		Client:    mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	envs := makeEnvelopes(4) // 2 batches
	sent, sendErr := sender.SendBatch(ctx, func() []ports.OutboundMessage {
		_msgs := make([]ports.OutboundMessage, len(envs))
		for _i, _e := range envs {
			_msgs[_i] = ports.OutboundMessage{Envelope: _e}
		}
		return _msgs
	}())

	// First batch should have succeeded.
	if sent < 2 {
		t.Fatalf("expected at least 2 sent from first batch, got %d", sent)
	}
	// The second batch should fail due to cancelled context.
	if sendErr == nil && callNum > 1 {
		t.Fatal("expected error after parent context cancellation")
	}
}

// Verifies that with a large message count (100 messages, batch size 10),
// the last batch still gets a full timeout, not a depleted shared one.
func TestSendBatch_LargeBatch_LastBatchFullTimeout(t *testing.T) {
	const (
		batchSize       = 10
		totalMsgs       = 100
		perBatchTO      = 500 * time.Millisecond
		expectedBatches = totalMsgs / batchSize
	)

	var deadlines []time.Time
	var callTimes []time.Time
	mock := &mockSQSClient{
		SendMessageBatchFn: func(ctx context.Context, in *awssqs.SendMessageBatchInput, _ ...func(*awssqs.Options)) (*awssqs.SendMessageBatchOutput, error) {
			dl, ok := ctx.Deadline()
			if !ok {
				t.Fatal("expected deadline on batch context")
			}
			callTimes = append(callTimes, time.Now())
			deadlines = append(deadlines, dl)
			time.Sleep(2 * time.Millisecond) // OTHER: simulate work for deadline verification
			result := make([]sqstypes.SendMessageBatchResultEntry, len(in.Entries))
			for i := range in.Entries {
				result[i] = sqstypes.SendMessageBatchResultEntry{Id: in.Entries[i].Id}
			}
			return &awssqs.SendMessageBatchOutput{Successful: result}, nil
		},
	}
	sender, err := NewSender(SenderConfig{
		QueueURL:  "https://q",
		BatchSize: batchSize,
		Timeout:   perBatchTO,
		Client:    mock,
	})
	if err != nil {
		t.Fatal(err)
	}

	envs := makeEnvelopes(totalMsgs)
	sent, err := sender.SendBatch(context.Background(), func() []ports.OutboundMessage {
		_msgs := make([]ports.OutboundMessage, len(envs))
		for _i, _e := range envs {
			_msgs[_i] = ports.OutboundMessage{Envelope: _e}
		}
		return _msgs
	}())
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if sent != totalMsgs {
		t.Fatalf("expected %d sent, got %d", totalMsgs, sent)
	}
	if len(deadlines) != expectedBatches {
		t.Fatalf("expected %d deadlines, got %d", expectedBatches, len(deadlines))
	}

	// The last batch must have nearly a full timeout remaining.
	lastIdx := len(deadlines) - 1
	remaining := deadlines[lastIdx].Sub(callTimes[lastIdx])
	// Allow 100ms tolerance for scheduling jitter.
	minRemaining := perBatchTO - 100*time.Millisecond
	if remaining < minRemaining {
		t.Fatalf("last batch timeout remaining %v is less than %v; "+
			"shared timeout suspected", remaining, minRemaining)
	}
}
