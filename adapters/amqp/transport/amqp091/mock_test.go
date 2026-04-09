package amqp091

import (
	"sync"

	amqp "github.com/rabbitmq/amqp091-go"
)

// mockConnection implements amqpConnection for unit tests.
type mockConnection struct {
	mu sync.Mutex

	ChannelFn     func() (*amqp.Channel, error)
	CloseFn       func() error
	NotifyCloseFn func(chan *amqp.Error) chan *amqp.Error
	IsClosedFn    func() bool

	ChannelCalls int
	CloseCalls   int
	closed       bool
}

var _ amqpConnection = (*mockConnection)(nil)

func newMockConnection() *mockConnection {
	return &mockConnection{}
}

func (m *mockConnection) Channel() (*amqp.Channel, error) {
	m.mu.Lock()
	m.ChannelCalls++
	m.mu.Unlock()

	if m.ChannelFn != nil {
		return m.ChannelFn()
	}
	return nil, nil
}

func (m *mockConnection) Close() error {
	m.mu.Lock()
	m.CloseCalls++
	m.closed = true
	m.mu.Unlock()

	if m.CloseFn != nil {
		return m.CloseFn()
	}
	return nil
}

func (m *mockConnection) NotifyClose(receiver chan *amqp.Error) chan *amqp.Error {
	if m.NotifyCloseFn != nil {
		return m.NotifyCloseFn(receiver)
	}
	return receiver
}

func (m *mockConnection) IsClosed() bool {
	if m.IsClosedFn != nil {
		return m.IsClosedFn()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// mockAcknowledger implements amqp.Acknowledger for delivery tests.
type mockAcknowledger struct {
	mu sync.Mutex

	AckFn    func(tag uint64, multiple bool) error
	NackFn   func(tag uint64, multiple bool, requeue bool) error
	RejectFn func(tag uint64, requeue bool) error

	AckCalls    int
	NackCalls   int
	RejectCalls int
}

func newMockAcknowledger() *mockAcknowledger {
	return &mockAcknowledger{}
}

func (m *mockAcknowledger) Ack(tag uint64, multiple bool) error {
	m.mu.Lock()
	m.AckCalls++
	m.mu.Unlock()

	if m.AckFn != nil {
		return m.AckFn(tag, multiple)
	}
	return nil
}

func (m *mockAcknowledger) Nack(tag uint64, multiple bool, requeue bool) error {
	m.mu.Lock()
	m.NackCalls++
	m.mu.Unlock()

	if m.NackFn != nil {
		return m.NackFn(tag, multiple, requeue)
	}
	return nil
}

func (m *mockAcknowledger) Reject(tag uint64, requeue bool) error {
	m.mu.Lock()
	m.RejectCalls++
	m.mu.Unlock()

	if m.RejectFn != nil {
		return m.RejectFn(tag, requeue)
	}
	return nil
}
