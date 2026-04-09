// Provides test doubles for internal interfaces used by the AMQP 1.0 adapter.
package amqp10

import (
	"context"
	"sync"

	"github.com/Azure/go-amqp"
)

// mockConn implements amqpConn for unit tests.
type mockConn struct {
	mu         sync.Mutex
	session    *amqp.Session
	sessionErr error
	closeErr   error
	closed     bool
}

func (m *mockConn) NewSession(_ context.Context, _ *amqp.SessionOptions) (*amqp.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.session, m.sessionErr
}

func (m *mockConn) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return m.closeErr
}

// mockSettler implements settler for unit tests.
type mockSettler struct {
	mu             sync.Mutex
	acceptCalls    int
	releaseCalls   int
	modifyCalls    int
	lastModifyOpts *amqp.ModifyMessageOptions
	acceptErr      error
	releaseErr     error
	modifyErr      error
}

func newMockSettler() *mockSettler {
	return &mockSettler{}
}

func (m *mockSettler) AcceptMessage(_ context.Context, _ *amqp.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.acceptCalls++
	return m.acceptErr
}

func (m *mockSettler) ReleaseMessage(_ context.Context, _ *amqp.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.releaseCalls++
	return m.releaseErr
}

func (m *mockSettler) ModifyMessage(_ context.Context, _ *amqp.Message, opts *amqp.ModifyMessageOptions) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.modifyCalls++
	m.lastModifyOpts = opts
	return m.modifyErr
}

// mockDialFunc returns a dialFunc that yields the given connection or error.
func mockDialFunc(conn amqpConn, err error) dialFunc {
	return func(_ context.Context, _ string, _ *amqp.ConnOptions) (amqpConn, error) {
		return conn, err
	}
}
