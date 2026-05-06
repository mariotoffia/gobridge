// Validates that Send releases its mutex before invoking link.Send so
// many goroutines can have publishes in flight against the same Sender
// simultaneously. go-amqp's Sender.Send is documented as safe for
// concurrent use; this test pins the contract that we do not silently
// re-introduce per-Send serialisation.
package amqp10

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
)

// mockSenderLink is an amqpSenderLink test double whose Send blocks on
// a controllable barrier so the test can observe how many sends are
// in flight at once.
type mockSenderLink struct {
	mu sync.Mutex

	sendBlock chan struct{}
	sendErr   error
	closeErr  error

	inFlight    atomic.Int32
	maxInFlight atomic.Int32
	closed      atomic.Bool
	closeCalls  atomic.Int32
}

func newMockSenderLink() *mockSenderLink {
	return &mockSenderLink{sendBlock: make(chan struct{})}
}

func (m *mockSenderLink) SendEnvelope(ctx context.Context, _ *messaging.Envelope) error {
	cur := m.inFlight.Add(1)
	for {
		obs := m.maxInFlight.Load()
		if cur <= obs || m.maxInFlight.CompareAndSwap(obs, cur) {
			break
		}
	}
	defer m.inFlight.Add(-1)

	m.mu.Lock()
	block := m.sendBlock
	err := m.sendErr
	m.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (m *mockSenderLink) Close(_ context.Context) error {
	m.closeCalls.Add(1)
	m.closed.Store(true)
	return m.closeErr
}

func (m *mockSenderLink) release() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sendBlock != nil {
		close(m.sendBlock)
		m.sendBlock = nil
	}
}

// TestSender_ConcurrentSends_DoNotSerialiseOnNetwork validates that
// many goroutines calling Send() on the same Sender do their network
// I/O concurrently. Previously the entire Send held s.mu across
// link.Send, which serialised throughput to one publish at a time.
func TestSender_ConcurrentSends_DoNotSerialiseOnNetwork(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost:5672"},
		connectivity.SessionEphemeral, slog.Default())

	s, err := NewSender(SenderConfig{Address: "queue/x", Session: sess}, sess)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	link := newMockSenderLink()
	s.mu.Lock()
	s.link = link
	s.mu.Unlock()

	const callers = 8
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := range callers {
		go func(i int) {
			defer wg.Done()
			_ = s.Send(context.Background(), ports.OutboundMessage{Envelope: &messaging.Envelope{
				ID:      "x",
				Payload: []byte{byte(i)},
			}})
		}(i)
	}

	deadline := time.After(3 * time.Second)
	for link.maxInFlight.Load() < callers {
		select {
		case <-deadline:
			t.Fatalf("expected %d concurrent sends, max observed = %d "+
				"(Send is serialised on the mutex across link.Send)",
				callers, link.maxInFlight.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}

	link.release()
	wg.Wait()
}

// TestSender_ConcurrentSendFailure_OnlyClosesLinkOnce validates that
// when several concurrent Sends all see the same link error, the link
// is detached and closed exactly once — not once per failing Send.
// Otherwise repeated link.Close calls would race the broker.
func TestSender_ConcurrentSendFailure_OnlyClosesLinkOnce(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost:5672"},
		connectivity.SessionEphemeral, slog.Default())

	s, err := NewSender(SenderConfig{Address: "queue/x", Session: sess}, sess)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	link := newMockSenderLink()
	link.sendErr = errors.New("amqp:link:detach-forced")
	s.mu.Lock()
	s.link = link
	s.mu.Unlock()

	const callers = 10
	var wg sync.WaitGroup
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			_ = s.Send(context.Background(), ports.OutboundMessage{Envelope: &messaging.Envelope{ID: "x"}})
		}()
	}

	link.release()
	wg.Wait()

	deadline := time.After(2 * time.Second)
	for !link.closed.Load() {
		select {
		case <-deadline:
			t.Fatal("link was never closed after concurrent failures")
		case <-time.After(5 * time.Millisecond):
		}
	}
	// OTHER: settle delay for async close propagation.
	time.Sleep(50 * time.Millisecond)
	if calls := link.closeCalls.Load(); calls != 1 {
		t.Fatalf("link.Close was called %d times, want exactly 1 "+
			"(losers of the failure race must not also close the link)", calls)
	}
}

// TestSender_Send_ReleasesLockBeforeNetworkIO validates by direct
// observation: while a Send is blocked in link.Send, another caller
// can acquire the Sender mutex (e.g., via Close).
func TestSender_Send_ReleasesLockBeforeNetworkIO(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost:5672"},
		connectivity.SessionEphemeral, slog.Default())

	s, err := NewSender(SenderConfig{Address: "queue/x", Session: sess}, sess)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	link := newMockSenderLink()
	s.mu.Lock()
	s.link = link
	s.mu.Unlock()

	sendDone := make(chan error, 1)
	go func() {
		sendDone <- s.Send(context.Background(), ports.OutboundMessage{Envelope: &messaging.Envelope{ID: "x"}})
	}()

	// Wait until the Send is parked inside link.Send.
	deadline := time.After(2 * time.Second)
	for link.inFlight.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("Send did not enter link.Send within 2s")
		case <-time.After(5 * time.Millisecond):
		}
	}

	// Try to grab the mutex via Close. If Send held the mutex across
	// link.Send, this would block until link.release() unblocks Send.
	closeReturned := make(chan struct{})
	go func() {
		_ = s.Close(context.Background())
		close(closeReturned)
	}()

	select {
	case <-closeReturned:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Close blocked while a concurrent Send was in flight; " +
			"Send is still holding the mutex across network I/O")
	}

	link.release()
	<-sendDone
}

// TestSender_NoLink_AfterFailure validates that after a failed Send
// detaches the link, a subsequent Send on a fresh link succeeds.
// Confirms the off-mutex close does not break the recovery path.
func TestSender_NoLink_AfterFailure(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost:5672"},
		connectivity.SessionEphemeral, slog.Default())

	s, err := NewSender(SenderConfig{Address: "queue/x", Session: sess}, sess)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	failing := newMockSenderLink()
	failing.sendErr = errors.New("amqp:link:detach-forced")
	s.mu.Lock()
	s.link = failing
	s.mu.Unlock()

	close(failing.sendBlock)
	failing.sendBlock = nil

	if err := s.Send(context.Background(), ports.OutboundMessage{Envelope: &messaging.Envelope{ID: "x"}}); err == nil {
		t.Fatal("expected Send error from failing link")
	}

	deadline := time.After(time.Second)
	for {
		s.mu.Lock()
		l := s.link
		s.mu.Unlock()
		if l == nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("link was not detached after Send failure")
		case <-time.After(5 * time.Millisecond):
		}
	}

	// Inject a healthy mock for the next Send.
	healthy := newMockSenderLink()
	close(healthy.sendBlock)
	healthy.sendBlock = nil
	s.mu.Lock()
	s.link = healthy
	s.mu.Unlock()

	if err := s.Send(context.Background(), ports.OutboundMessage{Envelope: &messaging.Envelope{ID: "y"}}); err != nil {
		t.Fatalf("Send on fresh link after failure: %v", err)
	}
}

// TestSender_ConcurrentSends_RaceDetector exists purely to maximise
// concurrent reads/writes against Sender state under -race so any
// regressing memory race shows up in CI.
func TestSender_ConcurrentSends_RaceDetector(t *testing.T) {
	sess := NewSession(SessionOptions{Address: "amqp://localhost:5672"},
		connectivity.SessionEphemeral, slog.Default())

	s, err := NewSender(SenderConfig{Address: "queue/x",
		Session: sess, Metrics: &ports.NoopExporter{}}, sess)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	link := newMockSenderLink()
	close(link.sendBlock)
	link.sendBlock = nil
	s.mu.Lock()
	s.link = link
	s.mu.Unlock()

	const goroutines = 16
	const perG = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range perG {
				_ = s.Send(context.Background(), ports.OutboundMessage{Envelope: &messaging.Envelope{ID: "z"}})
			}
		}()
	}
	wg.Wait()
}
