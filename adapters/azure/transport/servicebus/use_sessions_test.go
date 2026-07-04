package servicebus

// use_sessions_test.go — Finding 9 (audit 501111a): consuming a
// session-enabled entity WITHOUT pinning a session_id. The receiver
// accepts the next available session (AcceptNextSessionForQueue /
// ...ForSubscription), drains it, and rotates to the next session once
// a poll comes back empty and every delivery has settled. "No session
// available" (SDK CodeTimeout) is an idle entity, not a failure.
//
// Written failing-first: on the pre-fix code the `use_sessions` key is
// rejected by the strict decoder, and a session-required entity
// fail-fasts with ErrNotSupported instead of accepting sessions.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// namedMetrics records counter emissions BY NAME so tests can assert
// that a specific metric did (or did not) fire. Hand-rolled fake per
// TESTS.md.
type namedMetrics struct {
	mu     sync.Mutex
	counts map[string]int64
}

var _ ports.MetricsExporter = (*namedMetrics)(nil)

func (m *namedMetrics) Counter(name string, v int64, _ ...shared.Tag) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.counts == nil {
		m.counts = map[string]int64{}
	}
	m.counts[name] += v
}

func (m *namedMetrics) Gauge(string, float64, ...shared.Tag)       {}
func (m *namedMetrics) Histogram(string, float64, ...shared.Tag)   {}
func (m *namedMetrics) Timer(string, time.Duration, ...shared.Tag) {}
func (m *namedMetrics) Flush(context.Context) error                { return nil }
func (m *namedMetrics) Close(context.Context) error                { return nil }

func (m *namedMetrics) count(name string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts[name]
}

// --- config surface ---------------------------------------------------------

// The documented `use_sessions` key must decode through the REAL
// production plugin-options decoder (strict, ErrorUnused) and pass the
// plugin validation gate.
func TestPluginOptionsDecode_UseSessions(t *testing.T) {
	t.Parallel()

	input := map[string]any{
		"receiver": map[string]any{
			"queue_name":   "orders",
			"use_sessions": true,
		},
	}

	var cfg Config
	require.NoError(t, parser.NewRawConfig(input).Decode(&cfg))
	require.True(t, cfg.Receiver.UseSessions)
	require.NoError(t, cfg.Validate())
}

func TestConfig_Validate_UseSessionsCombos(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		receiver ReceiverParams
		wantErr  string
	}{
		{
			name:     "use_sessions alone is valid",
			receiver: ReceiverParams{QueueName: "q", UseSessions: true},
		},
		{
			name:     "use_sessions with session_id is rejected",
			receiver: ReceiverParams{QueueName: "q", UseSessions: true, SessionID: "s1"},
			wantErr:  "use_sessions",
		},
		{
			name:     "use_sessions with sub_queue is rejected",
			receiver: ReceiverParams{QueueName: "q", UseSessions: true, SubQueue: "deadletter"},
			wantErr:  "use_sessions",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := Config{Receiver: tc.receiver}.Validate()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestNewReceiver_UseSessionsCombos(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     ReceiverConfig
		wantErr string
	}{
		{
			name: "use_sessions alone is valid",
			cfg:  ReceiverConfig{QueueName: "q", UseSessions: true, Client: &mockASBClient{}},
		},
		{
			name:    "use_sessions with session_id is rejected",
			cfg:     ReceiverConfig{QueueName: "q", UseSessions: true, SessionID: "s1", Client: &mockASBClient{}},
			wantErr: "use_sessions",
		},
		{
			name:    "use_sessions with sub_queue is rejected",
			cfg:     ReceiverConfig{QueueName: "q", UseSessions: true, SubQueue: "deadletter", Client: &mockASBClient{}},
			wantErr: "use_sessions",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewReceiver(tc.cfg, nil)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// --- poll-loop behaviour -----------------------------------------------------

// newUseSessionsReceiver builds a use_sessions receiver wired to the
// buildStackFn (empty stack — no SDK client) and acceptNextFn test
// seams.
func newUseSessionsReceiver(t *testing.T, cfg ReceiverConfig, accept func(context.Context) (asbAPI, error)) *Receiver {
	t.Helper()
	cfg.QueueName = "q"
	cfg.UseSessions = true
	// Dummy namespace satisfies the connection-presence check; the
	// buildStackFn seam below intercepts before any real SDK build.
	cfg.Connection = ConnectionConfig{Namespace: "ns.example"}
	recv, err := NewReceiver(cfg, nil)
	require.NoError(t, err)
	recv.buildStackFn = func(context.Context, ConnectionConfig) (receiverStack, error) {
		return receiverStack{}, nil
	}
	recv.acceptNextFn = accept
	return recv
}

// Happy path + idle rotation: the receiver accepts session A, drains
// it, and — once the poll comes back empty with everything settled —
// closes A and accepts the NEXT session (round-robin).
func TestReceiver_UseSessions_AcceptsAndRotatesOnIdle(t *testing.T) {
	t.Parallel()

	sessA := &closeableASBClient{}
	var aReceives atomic.Int32
	sessA.ReceiveMessagesFn = func(context.Context, int, *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
		if aReceives.Add(1) == 1 {
			return []*azservicebus.ReceivedMessage{{MessageID: "msg-a", Body: []byte("a")}}, nil
		}
		return nil, nil // session drained: idle poll triggers rotation once msg-a settles
	}
	sessB := &closeableASBClient{}
	sessB.ReceiveMessagesFn = func(context.Context, int, *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
		return []*azservicebus.ReceivedMessage{{MessageID: "msg-b", Body: []byte("b")}}, nil
	}

	var accepts atomic.Int32
	recv := newUseSessionsReceiver(t, ReceiverConfig{AutoExtend: boolPtr(false)},
		func(context.Context) (asbAPI, error) {
			if accepts.Add(1) == 1 {
				return sessA, nil
			}
			return sessB, nil
		})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var mu sync.Mutex
	var got []string
	runErr := make(chan error, 1)
	go func() {
		runErr <- recv.Run(ctx, func(dctx context.Context, del ports.Delivery) error {
			mu.Lock()
			got = append(got, del.Envelope().ID())
			n := len(got)
			mu.Unlock()
			if n == 1 {
				return del.Ack(dctx)
			}
			cancel()
			return nil
		})
	}()

	require.ErrorIs(t, <-runErr, context.Canceled)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"msg-a", "msg-b"}, got,
		"receiver must drain session A, rotate, and consume from session B")
	require.GreaterOrEqual(t, accepts.Load(), int32(2), "an idle session must trigger accept of the NEXT session")
	require.GreaterOrEqual(t, sessA.closeCalls.Load(), int32(1), "the rotated-out session receiver must be closed")
	require.Len(t, sessA.CompleteCalls, 1, "msg-a must be settled against the session it was received on")
}

// "No session available right now" surfaces from the SDK as
// CodeTimeout. That is an IDLE entity: quiet backoff, no
// receive-failure metric, and the poll loop keeps trying until a
// session appears.
func TestReceiver_UseSessions_NoSessionAvailableIsIdle(t *testing.T) {
	t.Parallel()

	fake := clocktest.New()
	metrics := &namedMetrics{}

	sess := &closeableASBClient{}
	sess.ReceiveMessagesFn = func(context.Context, int, *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
		return []*azservicebus.ReceivedMessage{{MessageID: "msg-1", Body: []byte("x")}}, nil
	}

	var accepts atomic.Int32
	recv := newUseSessionsReceiver(t,
		ReceiverConfig{AutoExtend: boolPtr(false), Clock: fake, Metrics: metrics},
		func(context.Context) (asbAPI, error) {
			if accepts.Add(1) <= 2 {
				// The SDK's no-sessions-available signal.
				return nil, &azservicebus.Error{Code: azservicebus.CodeTimeout}
			}
			return sess, nil
		})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var mu sync.Mutex
	var got []string
	runErr := make(chan error, 1)
	go func() {
		runErr <- recv.Run(ctx, func(context.Context, ports.Delivery) error {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, "msg-1")
			cancel()
			return nil
		})
	}()

	// Drive the idle backoff waits with the fake clock until the third
	// accept succeeds.
	waitUntil(t, 10*time.Second, func() bool {
		if accepts.Load() >= 3 {
			return true
		}
		if fake.TimerCount() > 0 {
			fake.Advance(pollBackoffMax + 10*time.Second)
		}
		return false
	}, "receiver must keep accepting through no-session-available idles")

	require.ErrorIs(t, <-runErr, context.Canceled)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"msg-1"}, got)
	require.Zero(t, metrics.count(MetricASBReceiveFailures),
		"no-session-available (CodeTimeout) is an idle entity, not a receive failure")
}

// A receive error on the held session sheds it (the link is suspect —
// e.g. session lock lost) and the next iteration accepts a FRESH
// session, so the receiver self-heals instead of erroring forever on a
// dead seam.
func TestReceiver_UseSessions_ReceiveErrorShedsSession(t *testing.T) {
	t.Parallel()

	fake := clocktest.New()
	metrics := &namedMetrics{}

	sessA := &closeableASBClient{}
	sessA.ReceiveMessagesFn = func(context.Context, int, *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
		return nil, &azservicebus.Error{Code: azservicebus.CodeConnectionLost}
	}
	sessB := &closeableASBClient{}
	sessB.ReceiveMessagesFn = func(context.Context, int, *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
		return []*azservicebus.ReceivedMessage{{MessageID: "msg-b", Body: []byte("b")}}, nil
	}

	var accepts atomic.Int32
	recv := newUseSessionsReceiver(t,
		ReceiverConfig{AutoExtend: boolPtr(false), Clock: fake, Metrics: metrics},
		func(context.Context) (asbAPI, error) {
			if accepts.Add(1) == 1 {
				return sessA, nil
			}
			return sessB, nil
		})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var mu sync.Mutex
	var got []string
	runErr := make(chan error, 1)
	go func() {
		runErr <- recv.Run(ctx, func(context.Context, ports.Delivery) error {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, "msg-b")
			cancel()
			return nil
		})
	}()

	// Drive the post-error backoff with the fake clock until session B
	// is accepted.
	waitUntil(t, 10*time.Second, func() bool {
		if accepts.Load() >= 2 {
			return true
		}
		if fake.TimerCount() > 0 {
			fake.Advance(pollBackoffMax + 10*time.Second)
		}
		return false
	}, "receiver must accept a fresh session after a receive error")

	require.ErrorIs(t, <-runErr, context.Canceled)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"msg-b"}, got)
	require.GreaterOrEqual(t, sessA.closeCalls.Load(), int32(1), "the erroring session receiver must be closed")
	require.GreaterOrEqual(t, metrics.count(MetricASBReceiveFailures), int64(1),
		"a genuine receive error must count as a receive failure")
}

// Idle rotation must WAIT for in-flight deliveries: closing the session
// seam while a delivery received from it is unsettled would fail its
// Ack/Retry and force redelivery churn.
func TestReceiver_UseSessions_IdleRotationDeferredWhileInFlight(t *testing.T) {
	t.Parallel()

	sessA := &closeableASBClient{}
	var aReceives atomic.Int32
	sessA.ReceiveMessagesFn = func(context.Context, int, *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
		if aReceives.Add(1) == 1 {
			return []*azservicebus.ReceivedMessage{{MessageID: "msg-a", Body: []byte("a")}}, nil
		}
		return nil, nil // idle polls while msg-a is still in flight
	}
	sessB := &closeableASBClient{}
	sessB.ReceiveMessagesFn = func(ctx context.Context, _ int, _ *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	var accepts atomic.Int32
	recv := newUseSessionsReceiver(t, ReceiverConfig{AutoExtend: boolPtr(false)},
		func(context.Context) (asbAPI, error) {
			if accepts.Add(1) == 1 {
				return sessA, nil
			}
			return sessB, nil
		})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	emitted := make(chan ports.Delivery, 1)
	runErr := make(chan error, 1)
	go func() {
		runErr <- recv.Run(ctx, func(_ context.Context, del ports.Delivery) error {
			emitted <- del // deliberately NOT settled yet
			return nil
		})
	}()

	del := <-emitted

	// Several idle polls with msg-a unsettled: rotation must be held.
	waitUntil(t, 10*time.Second, func() bool { return aReceives.Load() >= 3 },
		"session A must keep being polled while a delivery is in flight")
	require.EqualValues(t, 1, accepts.Load(), "no rotation while a delivery is unsettled")
	require.Zero(t, sessA.closeCalls.Load(), "the session seam must not be closed under an unsettled delivery")

	// Settle: rotation may now proceed.
	require.NoError(t, del.Ack(context.Background()))
	waitUntil(t, 10*time.Second, func() bool { return accepts.Load() >= 2 },
		"receiver must rotate to the next session after settlement")
	waitUntil(t, 10*time.Second, func() bool { return sessA.closeCalls.Load() >= 1 },
		"the drained session receiver must be closed on rotation")

	cancel()
	require.ErrorIs(t, <-runErr, context.Canceled)
}

// In use_sessions mode all in-flight deliveries share ONE session lock:
// the single session-renewer goroutine must renew it (per-delivery
// auto-extend stays off).
func TestReceiver_UseSessions_SessionRenewerRenews(t *testing.T) {
	t.Parallel()

	fake := clocktest.New()

	sess := &closeableASBClient{}
	sess.ReceiveMessagesFn = func(ctx context.Context, _ int, _ *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	recv := newUseSessionsReceiver(t,
		ReceiverConfig{LockDuration: 30 * time.Second, Clock: fake}, // AutoExtend defaults ON
		func(context.Context) (asbAPI, error) { return sess, nil })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	runErr := make(chan error, 1)
	go func() {
		runErr <- recv.Run(ctx, func(context.Context, ports.Delivery) error {
			t.Error("unexpected emit")
			return nil
		})
	}()

	// Drive the renewer ticker (LockDuration/2 = 15s) until a renewal
	// lands on the held session receiver.
	waitUntil(t, 10*time.Second, func() bool {
		sess.mu.Lock()
		n := len(sess.RenewCalls)
		sess.mu.Unlock()
		if n >= 1 {
			return true
		}
		if fake.TickerCount() > 0 {
			fake.Advance(16 * time.Second)
		}
		return false
	}, "session renewer must renew the session lock in use_sessions mode")

	cancel()
	require.ErrorIs(t, <-runErr, context.Canceled)
}

// A started use_sessions receiver legitimately holds NO session seam
// between sessions — a live credential rotation in that state must
// still rebuild the stack (non-session ordering: build first, then
// commit-and-swap), not mistake the receiver for "not started" and
// merely stash the connection.
func TestReceiver_UseSessions_ApplyCredentialsRebuildsWithoutHeldSession(t *testing.T) {
	t.Parallel()

	const cs1 = "Endpoint=sb://ns1.servicebus.windows.net/;SharedAccessKeyName=k;SharedAccessKey=djE="
	const cs2 = "Endpoint=sb://ns2.servicebus.windows.net/;SharedAccessKeyName=k;SharedAccessKey=djI="

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Real (lazy, no-dial) SDK build path: use_sessions cold init makes
	// a live handle with a nil receiver seam.
	recv, err := NewReceiver(ReceiverConfig{
		QueueName:   "q",
		UseSessions: true,
		Connection:  ConnectionConfig{ConnectionString: shared.NewSecret(cs1)},
	}, nil)
	require.NoError(t, err)
	require.NoError(t, recv.ensureClient(ctx))
	t.Cleanup(func() { _ = recv.Close(context.Background()) })

	recv.initMu.Lock()
	h1 := recv.asbClient
	recv.initMu.Unlock()
	require.NotNil(t, h1, "cold init must build the client handle")
	require.Nil(t, recv.currentClient(), "no session is held before the poll loop accepts one")

	set := connectivity.NewCredentialSet(pwCred("", cs2), nil)
	require.NoError(t, recv.ApplyCredentials(ctx, set))

	recv.initMu.Lock()
	h2 := recv.asbClient
	recv.initMu.Unlock()
	require.NotNil(t, h2)
	require.NotSame(t, h1, h2,
		"a started use_sessions receiver (live handle, nil seam) must rebuild the stack on rotation, not stash the connection")
}
