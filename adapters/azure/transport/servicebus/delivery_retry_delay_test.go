package servicebus

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"

	"github.com/mariotoffia/gobridge/domain/messaging"
)

var _ retryScheduler = (*mockRetryScheduler)(nil)

type mockRetryScheduler struct {
	mu              sync.Mutex
	ScheduleCalls   int
	CancelCalls     int
	LastEnqueueTime time.Time
	LastMessages    []*azservicebus.Message
	Err             error
	CancelErr       error
}

func (m *mockRetryScheduler) ScheduleMessages(
	_ context.Context,
	messages []*azservicebus.Message,
	scheduledEnqueueTime time.Time,
	_ *azservicebus.ScheduleMessagesOptions,
) ([]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ScheduleCalls++
	m.LastEnqueueTime = scheduledEnqueueTime
	m.LastMessages = messages
	if m.Err != nil {
		return nil, m.Err
	}
	return []int64{42}, nil
}

func (m *mockRetryScheduler) CancelScheduledMessages(
	_ context.Context,
	_ []int64,
	_ *azservicebus.CancelScheduledMessagesOptions,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.CancelCalls++
	return m.CancelErr
}

// TestDeliveryRetryWithDelaySchedulesMessage verifies Retry with a positive delay
// schedules a copy, completes the original, and does not abandon.
func TestDeliveryRetryWithDelaySchedulesMessage(t *testing.T) {
	t.Parallel()

	mock := &mockASBClient{}
	sched := &mockRetryScheduler{}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-1"})
	body := []byte("payload")
	msg := &azservicebus.ReceivedMessage{MessageID: "orig", Body: body}
	d := newDelivery(context.Background(), env, mock, sched, msg, 30*time.Second, false, nil, nil, nil)

	before := time.Now()
	if err := d.Retry(context.Background(), 5*time.Second, nil); err != nil {
		t.Fatalf("Retry: %v", err)
	}

	sched.mu.Lock()
	sc := sched.ScheduleCalls
	eq := sched.LastEnqueueTime
	msgs := sched.LastMessages
	sched.mu.Unlock()

	if sc != 1 {
		t.Fatalf("ScheduleCalls = %d, want 1", sc)
	}
	if eq.Before(before.Add(4*time.Second)) || eq.After(before.Add(7*time.Second)) {
		t.Fatalf("scheduled enqueue at %v, want ~5s after %v", eq, before)
	}
	if len(msgs) != 1 || !bytes.Equal(msgs[0].Body, body) {
		t.Fatalf("scheduled message body mismatch")
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.CompleteCalls) != 1 || mock.CompleteCalls[0] != msg {
		t.Fatal("expected exactly 1 CompleteMessage call for the original message")
	}
	if len(mock.AbandonCalls) != 0 {
		t.Fatalf("AbandonCalls = %d, want 0", len(mock.AbandonCalls))
	}
}

// TestDeliveryRetryZeroDelayAbandons verifies Retry(0) abandons and does not schedule.
func TestDeliveryRetryZeroDelayAbandons(t *testing.T) {
	t.Parallel()

	mock := &mockASBClient{}
	sched := &mockRetryScheduler{}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-1"})
	msg := &azservicebus.ReceivedMessage{MessageID: "orig"}
	d := newDelivery(context.Background(), env, mock, sched, msg, 30*time.Second, false, nil, nil, nil)

	if err := d.Retry(context.Background(), 0, nil); err != nil {
		t.Fatalf("Retry: %v", err)
	}

	sched.mu.Lock()
	sc := sched.ScheduleCalls
	sched.mu.Unlock()
	if sc != 0 {
		t.Fatalf("ScheduleCalls = %d, want 0", sc)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.AbandonCalls) != 1 || mock.AbandonCalls[0] != msg {
		t.Fatal("expected exactly 1 AbandonMessage call for the original message")
	}
	if len(mock.CompleteCalls) != 0 {
		t.Fatalf("CompleteCalls = %d, want 0", len(mock.CompleteCalls))
	}
}

// TestDeliveryRetryScheduleFailsNoComplete verifies that when ScheduleMessages fails,
// the original message is neither completed nor abandoned, and the error is returned.
func TestDeliveryRetryScheduleFailsNoComplete(t *testing.T) {
	t.Parallel()

	mock := &mockASBClient{}
	sched := &mockRetryScheduler{Err: errors.New("schedule failed")}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-1"})
	msg := &azservicebus.ReceivedMessage{MessageID: "orig", Body: []byte("payload")}
	d := newDelivery(context.Background(), env, mock, sched, msg, 30*time.Second, false, nil, nil, nil)

	err := d.Retry(context.Background(), 5*time.Second, nil)
	if err == nil {
		t.Fatal("expected error from failed ScheduleMessages")
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.CompleteCalls) != 0 {
		t.Fatalf("CompleteCalls = %d, want 0 (should not complete on schedule failure)", len(mock.CompleteCalls))
	}
	if len(mock.AbandonCalls) != 0 {
		t.Fatalf("AbandonCalls = %d, want 0 (should not abandon on schedule failure)", len(mock.AbandonCalls))
	}
}

// TestDeliveryRetryCompleteFailCancelsScheduled verifies that when
// CompleteMessage fails after a successful schedule, the scheduled
// message is cancelled to prevent duplicates.
func TestDeliveryRetryCompleteFailCancelsScheduled(t *testing.T) {
	t.Parallel()

	mock := &mockASBClient{
		CompleteMessageFn: func(context.Context, *azservicebus.ReceivedMessage, *azservicebus.CompleteMessageOptions) error {
			return errors.New("complete failed")
		},
	}
	sched := &mockRetryScheduler{}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-1"})
	msg := &azservicebus.ReceivedMessage{MessageID: "orig", Body: []byte("payload")}
	d := newDelivery(context.Background(), env, mock, sched, msg, 30*time.Second, false, nil, nil, nil)

	err := d.Retry(context.Background(), 5*time.Second, nil)
	if err == nil {
		t.Fatal("expected error from failed CompleteMessage")
	}

	sched.mu.Lock()
	defer sched.mu.Unlock()
	if sched.ScheduleCalls != 1 {
		t.Fatalf("ScheduleCalls = %d, want 1", sched.ScheduleCalls)
	}
	if sched.CancelCalls != 1 {
		t.Fatalf("CancelCalls = %d, want 1 (should cancel scheduled message on complete failure)", sched.CancelCalls)
	}
}

// TestDeliveryRetryNegativeDelayAbandons verifies that a negative delay is treated
// as zero and falls through to AbandonMessage.
func TestDeliveryRetryNegativeDelayAbandons(t *testing.T) {
	t.Parallel()

	mock := &mockASBClient{}
	sched := &mockRetryScheduler{}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-1"})
	msg := &azservicebus.ReceivedMessage{MessageID: "orig"}
	d := newDelivery(context.Background(), env, mock, sched, msg, 30*time.Second, false, nil, nil, nil)

	if err := d.Retry(context.Background(), -1*time.Second, nil); err != nil {
		t.Fatalf("Retry: %v", err)
	}

	sched.mu.Lock()
	sc := sched.ScheduleCalls
	sched.mu.Unlock()
	if sc != 0 {
		t.Fatalf("ScheduleCalls = %d, want 0 for negative delay", sc)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.AbandonCalls) != 1 {
		t.Fatalf("AbandonCalls = %d, want 1", len(mock.AbandonCalls))
	}
}

// TestDeliveryRetryNoSchedulerFallsBackToAbandon verifies a positive delay with nil
// scheduler falls back to abandon for broker redelivery.
func TestDeliveryRetryNoSchedulerFallsBackToAbandon(t *testing.T) {
	t.Parallel()

	mock := &mockASBClient{}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-1"})
	msg := &azservicebus.ReceivedMessage{MessageID: "orig"}
	d := newDelivery(context.Background(), env, mock, nil, msg, 30*time.Second, false, nil, nil, nil)

	if err := d.Retry(context.Background(), 5*time.Second, nil); err != nil {
		t.Fatalf("Retry: %v", err)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.AbandonCalls) != 1 {
		t.Fatalf("AbandonCalls = %d, want 1", len(mock.AbandonCalls))
	}
	if len(mock.CompleteCalls) != 0 {
		t.Fatalf("CompleteCalls = %d, want 0", len(mock.CompleteCalls))
	}
}
