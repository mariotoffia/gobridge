package servicebus

import (
	"context"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
)

type mockASBClient struct {
	mu sync.Mutex

	ReceiveMessagesFn  func(ctx context.Context, count int, options *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error)
	CompleteMessageFn  func(ctx context.Context, message *azservicebus.ReceivedMessage, options *azservicebus.CompleteMessageOptions) error
	AbandonMessageFn   func(ctx context.Context, message *azservicebus.ReceivedMessage, options *azservicebus.AbandonMessageOptions) error
	RenewMessageLockFn func(ctx context.Context, message *azservicebus.ReceivedMessage, options *azservicebus.RenewMessageLockOptions) error

	CompleteCalls []*azservicebus.ReceivedMessage
	AbandonCalls  []*azservicebus.ReceivedMessage
	RenewCalls    []*azservicebus.ReceivedMessage
}

var _ asbAPI = (*mockASBClient)(nil)

func (m *mockASBClient) ReceiveMessages(ctx context.Context, count int, options *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
	if m.ReceiveMessagesFn != nil {
		return m.ReceiveMessagesFn(ctx, count, options)
	}
	return nil, nil
}

func (m *mockASBClient) CompleteMessage(ctx context.Context, message *azservicebus.ReceivedMessage, options *azservicebus.CompleteMessageOptions) error {
	m.mu.Lock()
	m.CompleteCalls = append(m.CompleteCalls, message)
	m.mu.Unlock()

	if m.CompleteMessageFn != nil {
		return m.CompleteMessageFn(ctx, message, options)
	}
	return nil
}

func (m *mockASBClient) AbandonMessage(ctx context.Context, message *azservicebus.ReceivedMessage, options *azservicebus.AbandonMessageOptions) error {
	m.mu.Lock()
	m.AbandonCalls = append(m.AbandonCalls, message)
	m.mu.Unlock()

	if m.AbandonMessageFn != nil {
		return m.AbandonMessageFn(ctx, message, options)
	}
	return nil
}

func (m *mockASBClient) RenewMessageLock(ctx context.Context, message *azservicebus.ReceivedMessage, options *azservicebus.RenewMessageLockOptions) error {
	m.mu.Lock()
	m.RenewCalls = append(m.RenewCalls, message)
	m.mu.Unlock()

	if m.RenewMessageLockFn != nil {
		return m.RenewMessageLockFn(ctx, message, options)
	}
	return nil
}
