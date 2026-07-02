package amqp091

import (
	"sync"
)

// mockConnection implements amqpConnection for unit tests. Tests are
// exempt from the ACL gate, so this file may import the SDK directly
// in order to construct *amqpChannel wrappers around real or nil
// SDK channels for negative-path tests.
type mockConnection struct {
	mu sync.Mutex

	ChannelFn       func() (*amqpChannel, error)
	CloseFn         func() error
	NotifyCloseChan chan error
	BlockedChan     chan connBlockState
	IsClosedFn      func() bool

	ChannelCalls int
	CloseCalls   int
	closed       bool
}

var _ amqpConnection = (*mockConnection)(nil)

func newMockConnection() *mockConnection {
	return &mockConnection{}
}

func (m *mockConnection) Channel() (*amqpChannel, error) {
	m.mu.Lock()
	m.ChannelCalls++
	m.mu.Unlock()

	if m.ChannelFn != nil {
		return m.ChannelFn()
	}
	return nil, nil
}

// channelCalls returns the number of Channel() invocations under lock so
// tests can poll it from another goroutine without racing.
func (m *mockConnection) channelCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ChannelCalls
}

func (m *mockConnection) Close() error {
	m.mu.Lock()
	m.CloseCalls++
	m.closed = true
	ch := m.NotifyCloseChan
	blocked := m.BlockedChan
	m.mu.Unlock()

	// When the test wired a NotifyClose listener, simulate the SDK's
	// behaviour of closing the listener channel on connection close.
	if ch != nil {
		// Best-effort: the test may have already closed the channel.
		func() {
			defer func() { _ = recover() }()
			close(ch)
		}()
	}
	// Mirror the SDK: closing the connection closes the blocked stream
	// so any session blocked-watcher goroutine drains and exits.
	if blocked != nil {
		func() {
			defer func() { _ = recover() }()
			close(blocked)
		}()
	}

	if m.CloseFn != nil {
		return m.CloseFn()
	}
	return nil
}

// NotifyClose returns the test-supplied channel as the lifecycle
// notification stream. If none was provided, returns a closed channel
// (i.e., "no notification will arrive") to keep the reconnect loop
// blocked until the test cancels it.
func (m *mockConnection) NotifyClose() <-chan error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.NotifyCloseChan == nil {
		m.NotifyCloseChan = make(chan error, 1)
	}
	return m.NotifyCloseChan
}

func (m *mockConnection) IsClosed() bool {
	if m.IsClosedFn != nil {
		return m.IsClosedFn()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// NotifyBlocked returns the test-supplied block-notification channel, or
// a non-emitting channel when none was wired. The channel is closed on
// Close so any session blocked-watcher goroutine exits with the
// connection.
func (m *mockConnection) NotifyBlocked() <-chan connBlockState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.BlockedChan == nil {
		m.BlockedChan = make(chan connBlockState, 1)
	}
	return m.BlockedChan
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
