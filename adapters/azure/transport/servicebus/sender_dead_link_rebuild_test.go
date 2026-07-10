package servicebus

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// senderClosed reports whether the mock sender's Close was called, read
// under its mutex so assertions are race-free.
func senderClosed(m *mockSenderAPI) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// sentCount returns the number of SendMessage calls the mock recorded.
func sentCount(m *mockSenderAPI) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sentMessages)
}

// --- HIGH-1: sender rebuilds a terminally closed link ----------------------

// TestSender_RebuildsClosedLinkOnNextSend proves the Sender tears down a
// terminally CLOSED link (typed *azservicebus.Error{Code: CodeClosed}) and
// rebuilds a fresh one on the NEXT send, instead of reusing the dead link
// forever. Before the fix, Send returned the error and left the closed
// sender installed, so every later send failed until process restart or a
// credential rotation.
//
// Mutation: drop the invalidateOnClosedLink call in Send. Then the closed
// link is never torn down, the second send reuses it (buildCount stays 1),
// and the "rebuilt exactly once" / "second send succeeded" assertions FAIL.
func TestSender_RebuildsClosedLinkOnNextSend(t *testing.T) {
	t.Parallel()

	first := &mockSenderAPI{
		sendMessageFn: func(context.Context, *azservicebus.Message, *azservicebus.SendMessageOptions) error {
			// A TYPED closed-link condition: the local sender link is
			// terminally closed and never recovers on its own.
			return &azservicebus.Error{Code: azservicebus.CodeClosed}
		},
	}
	second := &mockSenderAPI{} // healthy replacement

	var builds atomic.Int32
	s, err := NewSender(SenderConfig{
		QueueName:  "q",
		Connection: ConnectionConfig{Namespace: "ns.servicebus.windows.net"},
	})
	require.NoError(t, err)
	s.buildSenderFn = func(context.Context, ConnectionConfig) (asbSenderAPI, *asbClientHandle, error) {
		if builds.Add(1) == 1 {
			return first, nil, nil
		}
		return second, nil, nil
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "id-1", Payload: []byte("p")})

	// First send hits the terminally-closed link and surfaces the error.
	err = s.Send(context.Background(), ports.OutboundMessage{Envelope: env})
	require.Error(t, err)
	require.ErrorIs(t, err, shared.ErrConnectionLost)
	require.True(t, senderClosed(first), "the dead link is closed so the next send rebuilds")

	// Second send rebuilds a fresh link and succeeds.
	err = s.Send(context.Background(), ports.OutboundMessage{Envelope: env})
	require.NoError(t, err)
	require.Equal(t, int32(2), builds.Load(), "the sender rebuilt its closed link exactly once")
	require.Equal(t, 1, sentCount(second), "the rebuilt link carried the second send")
	require.Same(t, asbSenderAPI(second), s.currentClient(), "the fresh link is now live")
}

// TestSender_TransientErrorDoesNotRebuild guards against over-eager
// teardown: a self-healing transient error (CodeConnectionLost — the SDK
// reopens the AMQP connection on the next send) must NOT tear down the
// link, otherwise every transient blip would churn the sender stack.
//
// Mutation: broaden invalidateOnClosedLink to trigger on any error. Then
// the link is torn down here and the "retained" assertions FAIL.
func TestSender_TransientErrorDoesNotRebuild(t *testing.T) {
	t.Parallel()

	first := &mockSenderAPI{
		sendMessageFn: func(context.Context, *azservicebus.Message, *azservicebus.SendMessageOptions) error {
			return &azservicebus.Error{Code: azservicebus.CodeConnectionLost}
		},
	}
	var builds atomic.Int32
	s, err := NewSender(SenderConfig{
		QueueName:  "q",
		Connection: ConnectionConfig{Namespace: "ns.servicebus.windows.net"},
	})
	require.NoError(t, err)
	s.buildSenderFn = func(context.Context, ConnectionConfig) (asbSenderAPI, *asbClientHandle, error) {
		builds.Add(1)
		return first, nil, nil
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "id-1", Payload: []byte("p")})
	require.Error(t, s.Send(context.Background(), ports.OutboundMessage{Envelope: env}))
	require.False(t, senderClosed(first), "a self-healing CodeConnectionLost must NOT tear down the link")
	require.Same(t, asbSenderAPI(first), s.currentClient(), "the link is retained for the SDK to self-heal")
	require.Equal(t, int32(1), builds.Load(), "no rebuild on a transient error")
}

// TestSender_InvalidateClient_FencedByIdentity pins the fence that keeps a
// stale closed-link teardown from destroying the link a concurrent
// credential rotation just installed. The invalidation is keyed on the
// exact seam the failed send used; when a rotation has already swapped in a
// fresh seam (a new object), the stale teardown must be a no-op.
//
// Mutation: drop the `s.client != used` identity guard in invalidateClient
// (tear down unconditionally). Then the rotation's fresh link is nilled and
// the "fresh link survives" assertion FAILS.
func TestSender_InvalidateClient_FencedByIdentity(t *testing.T) {
	t.Parallel()

	clientA := &mockSenderAPI{}
	clientB := &mockSenderAPI{}

	s, err := NewSender(SenderConfig{
		QueueName:  "q",
		Connection: ConnectionConfig{Namespace: "ns.servicebus.windows.net"},
	})
	require.NoError(t, err)

	// Install clientA, then simulate a concurrent rotation that swapped in
	// clientB (a fresh object) before the stale invalidation of clientA runs.
	s.swapClient(clientA, nil, s.connectionSnapshot())
	s.swapClient(clientB, nil, s.connectionSnapshot())

	old, oldHandle := s.invalidateClient(clientA)
	require.Nil(t, old, "a stale invalidation must not surface the live seam")
	require.Nil(t, oldHandle)
	require.Same(t, asbSenderAPI(clientB), s.currentClient(),
		"the rotation's fresh link must survive a stale closed-link teardown")
}

// TestSender_InvalidateClient_TornDownOncePerObject proves a double
// invalidation of the SAME closed seam clears it exactly once and never
// double-closes: two racing sends that both observe CodeClosed must not
// each tear down (and Close) the link.
func TestSender_InvalidateClient_TornDownOncePerObject(t *testing.T) {
	t.Parallel()

	clientA := &mockSenderAPI{}
	s, err := NewSender(SenderConfig{
		QueueName:  "q",
		Connection: ConnectionConfig{Namespace: "ns.servicebus.windows.net"},
	})
	require.NoError(t, err)
	s.swapClient(clientA, nil, s.connectionSnapshot())

	old, _ := s.invalidateClient(clientA)
	require.Same(t, asbSenderAPI(clientA), old, "first teardown surfaces the dead seam")

	old2, handle2 := s.invalidateClient(clientA)
	require.Nil(t, old2, "second teardown of the already-cleared seam is a no-op")
	require.Nil(t, handle2)
}

// TestSender_ConcurrentSendAndSendBatch_ClosedLink_NeverNilDerefs is the
// review's BLOCKING-issue guard. Every built link is terminally CLOSED:
// Send fails at SendMessage and SendBatch fails at NewMessageBatch, both
// with CodeClosed, so invalidateClient nils s.client on essentially every
// operation. Many goroutines hammer Send and SendBatch at once, maximizing
// the window in which a caller could observe a nil seam BETWEEN "ensure"
// and "use".
//
// Before the fix, Send did ensureClient(ctx) then a SEPARATE
// currentClient(): a concurrent invalidation between the two handed back a
// nil seam and sendOne / sendChunk panicked on the interface call. The
// atomic ensureAndSnapshotClient closes that window — the seam is resolved
// once under the lock and is always non-nil.
//
// Mutation: split ensureAndSnapshotClient back into ensureClient +
// currentClient in Send/SendBatch. Then `go test -race -count=3` panics
// with a nil-pointer dereference and this test FAILS.
func TestSender_ConcurrentSendAndSendBatch_ClosedLink_NeverNilDerefs(t *testing.T) {
	t.Parallel()

	newClosedLink := func() *mockSenderAPI {
		return &mockSenderAPI{
			sendMessageFn: func(context.Context, *azservicebus.Message, *azservicebus.SendMessageOptions) error {
				return &azservicebus.Error{Code: azservicebus.CodeClosed}
			},
			newMessageBatchFn: func(context.Context, *azservicebus.MessageBatchOptions) (*azservicebus.MessageBatch, error) {
				return nil, &azservicebus.Error{Code: azservicebus.CodeClosed}
			},
		}
	}

	var builds atomic.Int32
	s, err := NewSender(SenderConfig{
		QueueName:  "q",
		BatchSize:  4,
		Connection: ConnectionConfig{Namespace: "ns.servicebus.windows.net"},
	})
	require.NoError(t, err)
	s.buildSenderFn = func(context.Context, ConnectionConfig) (asbSenderAPI, *asbClientHandle, error) {
		builds.Add(1)
		return newClosedLink(), nil, nil
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "id", Payload: []byte("p")})

	const workers = 8
	const iters = 60
	var (
		wg           sync.WaitGroup
		start        = make(chan struct{})
		batchAnomaly atomic.Bool
	)
	for w := range workers {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start // release together to widen the race window
			for range iters {
				if id%2 == 0 {
					// Send must error on a closed link but NEVER nil-deref.
					_ = s.Send(context.Background(), ports.OutboundMessage{Envelope: env})
					continue
				}
				// SendBatch returns (results, nil); each result carries the
				// classified error. A nil seam would panic in sendChunk.
				res, berr := s.SendBatch(context.Background(),
					[]ports.OutboundMessage{{Envelope: env}, {Envelope: env}})
				if berr != nil || len(res) != 2 {
					batchAnomaly.Store(true)
				}
			}
		}(w)
	}
	close(start)
	wg.Wait()

	require.False(t, batchAnomaly.Load(), "SendBatch must always return (results, nil) with one result per message")
	require.Greater(t, builds.Load(), int32(1),
		"closed links were repeatedly torn down and rebuilt (no dead-link reuse, no nil seam)")
	require.Nil(t, s.cfg.Client, "test drove the real build seam, not an injected client")
}

