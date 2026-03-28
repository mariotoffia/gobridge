package servicebus

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
)

// closeableASBClient extends mockASBClient with a Close method that tracks
// calls and supports configurable delay and context inspection.
type closeableASBClient struct {
	mockASBClient

	closeFn    func(ctx context.Context) error
	closeCalls atomic.Int32
	closeCtxMu sync.Mutex
	closeCtx   context.Context
}

var _ asbAPI = (*closeableASBClient)(nil)

func (m *closeableASBClient) Close(ctx context.Context) error {
	m.closeCalls.Add(1)
	m.closeCtxMu.Lock()
	m.closeCtx = ctx
	m.closeCtxMu.Unlock()

	if m.closeFn != nil {
		return m.closeFn(ctx)
	}
	return nil
}

func (m *closeableASBClient) lastCloseCtx() context.Context {
	m.closeCtxMu.Lock()
	defer m.closeCtxMu.Unlock()
	return m.closeCtx
}

// closeableScheduler implements retryScheduler with a Close method that
// tracks calls and supports configurable delay.
type closeableScheduler struct {
	scheduleMessagesFn       func(ctx context.Context, messages []*azservicebus.Message, scheduledEnqueueTime time.Time, options *azservicebus.ScheduleMessagesOptions) ([]int64, error)
	cancelScheduledMessageFn func(ctx context.Context, sequenceNumbers []int64, options *azservicebus.CancelScheduledMessagesOptions) error
	closeFn                  func(ctx context.Context) error

	closeCalls atomic.Int32
	closeCtxMu sync.Mutex
	closeCtx   context.Context
}

func (m *closeableScheduler) ScheduleMessages(ctx context.Context, messages []*azservicebus.Message, scheduledEnqueueTime time.Time, options *azservicebus.ScheduleMessagesOptions) ([]int64, error) {
	if m.scheduleMessagesFn != nil {
		return m.scheduleMessagesFn(ctx, messages, scheduledEnqueueTime, options)
	}
	return nil, nil
}

func (m *closeableScheduler) CancelScheduledMessages(ctx context.Context, sequenceNumbers []int64, options *azservicebus.CancelScheduledMessagesOptions) error {
	if m.cancelScheduledMessageFn != nil {
		return m.cancelScheduledMessageFn(ctx, sequenceNumbers, options)
	}
	return nil
}

func (m *closeableScheduler) Close(ctx context.Context) error {
	m.closeCalls.Add(1)
	m.closeCtxMu.Lock()
	m.closeCtx = ctx
	m.closeCtxMu.Unlock()

	if m.closeFn != nil {
		return m.closeFn(ctx)
	}
	return nil
}

func (m *closeableScheduler) lastCloseCtx() context.Context {
	m.closeCtxMu.Lock()
	defer m.closeCtxMu.Unlock()
	return m.closeCtx
}
