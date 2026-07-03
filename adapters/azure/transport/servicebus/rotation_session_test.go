package servicebus

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// waitUntil spins (Gosched, no sleep) until cond is true or the guard
// duration elapses; fails the test on timeout.
func waitUntil(t *testing.T, guard time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(guard)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		runtime.Gosched()
	}
}

// --- Finding: credential rotation nil-panics the receiver poll loop --------

// Swapping the receiver stack while pollAndConvert runs concurrently
// must be race-free and panic-free: the poll loop snapshots the client
// under the stack lock and only ever observes a complete stack.
func TestReceiver_SwapStackDuringPolling_NoRaceNoPanic(t *testing.T) {
	t.Parallel()

	var polls1, polls2 atomic.Int64
	mock1 := &mockASBClient{
		ReceiveMessagesFn: func(context.Context, int, *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
			polls1.Add(1)
			return nil, nil
		},
	}
	mock2 := &mockASBClient{
		ReceiveMessagesFn: func(context.Context, int, *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
			polls2.Add(1)
			return nil, nil
		},
	}

	recv, err := NewReceiver(ReceiverConfig{QueueName: "q", Client: mock1}, nil)
	require.NoError(t, err)
	require.NoError(t, recv.ensureClient(context.Background()))

	const workers = 4
	const iters = 200
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				if _, pollErr := recv.pollAndConvert(context.Background()); pollErr != nil {
					t.Errorf("pollAndConvert: %v", pollErr)
					return
				}
			}
		}()
	}

	old := recv.swapStack(receiverStack{client: mock2})
	require.Same(t, mock1, old.client)
	wg.Wait()

	require.Same(t, mock2, recv.currentClient())
	require.Positive(t, polls2.Load(), "polls must continue on the swapped-in client")
}

// A live rotation must atomically REPLACE the stack — never nil the
// client out from under the poll loop. Exercises the real (non-mock)
// build path: azservicebus client/receiver/sender construction is lazy
// (no dial), so this runs without any network.
func TestReceiver_ApplyCredentials_RotatesStackLive(t *testing.T) {
	t.Parallel()

	const cs1 = "Endpoint=sb://ns1.servicebus.windows.net/;SharedAccessKeyName=k;SharedAccessKey=djE="
	const cs2 = "Endpoint=sb://ns2.servicebus.windows.net/;SharedAccessKeyName=k;SharedAccessKey=djI="

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	recv, err := NewReceiver(ReceiverConfig{
		QueueName:  "q",
		Connection: ConnectionConfig{ConnectionString: shared.NewSecret(cs1)},
	}, nil)
	require.NoError(t, err)
	require.NoError(t, recv.ensureClient(ctx))
	t.Cleanup(func() { _ = recv.Close(context.Background()) })

	oldClient := recv.currentClient()
	require.NotNil(t, oldClient)

	set := connectivity.NewCredentialSet(pwCred("", cs2), nil)
	require.NoError(t, recv.ApplyCredentials(ctx, set))

	newClient := recv.currentClient()
	require.NotNil(t, newClient, "rotation must never nil the live client")
	require.NotSame(t, oldClient, newClient, "rotation must swap in a rebuilt client")
	require.NotNil(t, recv.currentScheduler(), "queue receiver keeps a retry scheduler after rotation")
}

// Rotation before Run only stashes the new connection; nothing to
// rebuild, no error.
func TestReceiver_ApplyCredentials_NotStarted_StashesOnly(t *testing.T) {
	t.Parallel()

	recv, err := NewReceiver(ReceiverConfig{
		QueueName:  "q",
		Connection: ConnectionConfig{ConnectionString: shared.NewSecret("Endpoint=sb://a.servicebus.windows.net/;SharedAccessKeyName=k;SharedAccessKey=djE=")},
	}, nil)
	require.NoError(t, err)

	set := connectivity.NewCredentialSet(pwCred("", "Endpoint=sb://b.servicebus.windows.net/;SharedAccessKeyName=k;SharedAccessKey=djI="), nil)
	require.NoError(t, recv.ApplyCredentials(context.Background(), set))
	require.Nil(t, recv.currentClient())
}

// --- Finding: sender client swap races with in-flight sends ----------------

// Concurrent Send calls during a client swap must be race-free (-race)
// and every send must land on one of the two clients.
func TestSender_SendDuringSwap_NoRace(t *testing.T) {
	t.Parallel()

	mock1 := &mockSenderAPI{}
	mock2 := &mockSenderAPI{}

	sender, err := NewSender(SenderConfig{QueueName: "q", Client: mock1})
	require.NoError(t, err)

	const workers = 4
	const iters = 100
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				env := messaging.MustEnvelope(messaging.EnvelopeInput{
					ID:      fmt.Sprintf("m-%d-%d", w, i),
					Payload: []byte("p"),
				})
				if sendErr := sender.Send(context.Background(), ports.OutboundMessage{Envelope: env}); sendErr != nil {
					t.Errorf("Send: %v", sendErr)
					return
				}
			}
		}(w)
	}

	oldClient, oldHandle := sender.swapClient(mock2, nil, sender.connectionSnapshot())
	require.Same(t, mock1, oldClient)
	require.Nil(t, oldHandle)
	wg.Wait()

	require.Same(t, mock2, sender.currentClient())
	mock1.mu.Lock()
	n1 := len(mock1.sentMessages)
	mock1.mu.Unlock()
	mock2.mu.Lock()
	n2 := len(mock2.sentMessages)
	mock2.mu.Unlock()
	require.Equal(t, workers*iters, n1+n2, "every send lands on exactly one client")
	require.Positive(t, n2, "sends must continue on the swapped-in client")
}

// --- Finding: SendBatch shares one timeout across all chunks ---------------

// cfg.Timeout is documented per call: each chunk must get its OWN
// deadline context, not share one across the whole batch.
func TestSender_SendBatch_PerChunkContext(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var ctxs []context.Context
	mock := &mockSenderAPI{
		newMessageBatchFn: func(ctx context.Context, _ *azservicebus.MessageBatchOptions) (*azservicebus.MessageBatch, error) {
			mu.Lock()
			ctxs = append(ctxs, ctx)
			mu.Unlock()
			return nil, errors.New("no batch in unit test")
		},
	}
	sender, err := NewSender(SenderConfig{QueueName: "q", Client: mock, BatchSize: 1, Timeout: 5 * time.Second})
	require.NoError(t, err)

	msgs := make([]ports.OutboundMessage, 3)
	for i := range msgs {
		msgs[i] = ports.OutboundMessage{Envelope: messaging.MustEnvelope(messaging.EnvelopeInput{
			ID: fmt.Sprintf("b-%d", i), Payload: []byte("p"),
		})}
	}
	results, err := sender.SendBatch(context.Background(), msgs)
	require.NoError(t, err)
	require.Len(t, results, 3)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, ctxs, 3, "one NewMessageBatch (chunk) per message at BatchSize=1")
	for i, c := range ctxs {
		_, ok := c.Deadline()
		require.True(t, ok, "chunk %d context must carry the per-call timeout", i)
		for j := i + 1; j < len(ctxs); j++ {
			require.NotEqual(t, ctxs[i], ctxs[j], "chunks %d and %d must not share one timeout context", i, j)
		}
	}
}

// --- Finding: one-shot session accept crash-loops rolling deploys ----------

// com.microsoft:session-cannot-be-locked is EXPECTED while the old pod
// still holds the session lock; accept must retry with backoff and
// succeed once the lock is released.
func TestAcceptSessionWithRetry_RetriesLockContention(t *testing.T) {
	t.Parallel()

	fake := clocktest.New()
	mock := &mockASBClient{}
	var attempts atomic.Int32
	accept := func(context.Context) (asbAPI, error) {
		if attempts.Add(1) < 3 {
			return nil, errors.New("amqp: link detached: com.microsoft:session-cannot-be-locked: the requested session 's1' cannot be accepted")
		}
		return mock, nil
	}

	type result struct {
		seam asbAPI
		err  error
	}
	done := make(chan result, 1)
	go func() {
		seam, err := acceptSessionWithRetry(context.Background(), accept, fake, nil)
		done <- result{seam, err}
	}()

	// Drive the fake clock through the backoff waits until the accept
	// loop finishes; the max jittered delay per step is 37.5s.
	guard := time.Now().Add(5 * time.Second)
	for {
		select {
		case res := <-done:
			require.NoError(t, res.err)
			require.Same(t, mock, res.seam)
			require.Equal(t, int32(3), attempts.Load())
			return
		default:
		}
		if time.Now().After(guard) {
			t.Fatal("acceptSessionWithRetry did not finish")
		}
		if fake.TimerCount() > 0 {
			fake.Advance(pollBackoffMax + 10*time.Second)
		} else {
			runtime.Gosched()
		}
	}
}

// Permanent accept failures (unauthorized, not found) must fail on the
// FIRST attempt — retrying cannot fix them.
func TestAcceptSessionWithRetry_PermanentFailsFast(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	accept := func(context.Context) (asbAPI, error) {
		attempts.Add(1)
		return nil, errors.New("401 unauthorized access to entity")
	}
	_, err := acceptSessionWithRetry(context.Background(), accept, clocktest.New(), nil)
	require.Error(t, err)
	require.Equal(t, int32(1), attempts.Load())
}

// Retryable failures are bounded by sessionAcceptMaxAttempts.
func TestAcceptSessionWithRetry_ExhaustsAttempts(t *testing.T) {
	t.Parallel()

	fake := clocktest.New()
	var attempts atomic.Int32
	accept := func(context.Context) (asbAPI, error) {
		attempts.Add(1)
		return nil, errors.New("com.microsoft:session-cannot-be-locked")
	}

	done := make(chan error, 1)
	go func() {
		_, err := acceptSessionWithRetry(context.Background(), accept, fake, nil)
		done <- err
	}()

	guard := time.Now().Add(5 * time.Second)
	for {
		select {
		case err := <-done:
			require.Error(t, err)
			require.Equal(t, int32(sessionAcceptMaxAttempts), attempts.Load())
			return
		default:
		}
		if time.Now().After(guard) {
			t.Fatal("acceptSessionWithRetry did not finish")
		}
		if fake.TimerCount() > 0 {
			fake.Advance(pollBackoffMax + 10*time.Second)
		} else {
			runtime.Gosched()
		}
	}
}

// --- Finding: session gaps ---------------------------------------------

// A non-session receiver on a session-enabled entity can never receive;
// the poll loop must fail fast with ErrNotSupported instead of
// warn-looping forever.
func TestPollLoop_SessionRequired_FailsFast(t *testing.T) {
	t.Parallel()

	mock := &mockASBClient{
		ReceiveMessagesFn: func(context.Context, int, *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
			return nil, errors.New("amqp:not-allowed: It is not possible for an entity that requires sessions to create a non-sessionful message receiver")
		},
	}
	recv, err := NewReceiver(ReceiverConfig{QueueName: "q", Client: mock}, nil)
	require.NoError(t, err)

	runErr := recv.Run(context.Background(), func(context.Context, ports.Delivery) error { return nil })
	require.Error(t, runErr)
	require.ErrorIs(t, runErr, shared.ErrNotSupported)
}

// In session mode ALL deliveries share one session lock: exactly ONE
// renewer ticker must exist (not one per in-flight delivery), and it
// renews the session (nil message).
func TestPollLoop_SessionMode_SingleRenewer(t *testing.T) {
	t.Parallel()

	fake := clocktest.New()
	renewed := make(chan *azservicebus.ReceivedMessage, 4)

	var batchSent atomic.Bool
	mock := &mockASBClient{
		ReceiveMessagesFn: func(ctx context.Context, _ int, _ *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
			if batchSent.CompareAndSwap(false, true) {
				return []*azservicebus.ReceivedMessage{
					{MessageID: "s-1", Body: []byte("1")},
					{MessageID: "s-2", Body: []byte("2")},
					{MessageID: "s-3", Body: []byte("3")},
				}, nil
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
		RenewMessageLockFn: func(_ context.Context, msg *azservicebus.ReceivedMessage, _ *azservicebus.RenewMessageLockOptions) error {
			renewed <- msg
			return nil
		},
	}

	recv, err := NewReceiver(ReceiverConfig{
		QueueName:    "q",
		SessionID:    "s1",
		LockDuration: 10 * time.Second,
		Client:       mock,
		Clock:        fake,
	}, nil)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var emitted atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- recv.Run(ctx, func(context.Context, ports.Delivery) error {
			emitted.Add(1)
			return nil
		})
	}()

	// The session renewer runs in its own goroutine, so wait for both the
	// batch emission AND the renewer ticker registration before asserting.
	waitUntil(t, 5*time.Second, func() bool {
		return emitted.Load() == 3 && fake.TickerCount() >= 1
	}, "batch not emitted or renewer not started")

	// One session renewer — NOT one auto-extend ticker per delivery.
	require.Equal(t, 1, fake.TickerCount(), "session mode must run exactly one renewer")

	fake.Advance(6 * time.Second) // interval = LockDuration/2 = 5s
	select {
	case msg := <-renewed:
		require.Nil(t, msg, "session renewal targets the session, not a message")
	case <-time.After(5 * time.Second):
		t.Fatal("session renewer never fired")
	}

	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

// --- Finding: batch-tail locks lapse under backpressure ---------------------

// Lock auto-renewal must start for EVERY message of the batch right
// after receive — before the (blocking) emit loop — so the tail of a
// batch cannot expire while the head is being processed.
func TestPollLoop_BatchWideAutoExtendStart(t *testing.T) {
	t.Parallel()

	fake := clocktest.New()
	var batchSent atomic.Bool
	mock := &mockASBClient{
		ReceiveMessagesFn: func(ctx context.Context, _ int, _ *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
			if batchSent.CompareAndSwap(false, true) {
				return []*azservicebus.ReceivedMessage{
					{MessageID: "b-1", Body: []byte("1")},
					{MessageID: "b-2", Body: []byte("2")},
					{MessageID: "b-3", Body: []byte("3")},
				}, nil
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	recv, err := NewReceiver(ReceiverConfig{
		QueueName:    "q",
		MaxMessages:  10,
		LockDuration: 10 * time.Second,
		Client:       mock,
		Clock:        fake,
	}, nil)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	release := make(chan struct{})
	firstEmit := make(chan struct{})
	var once sync.Once
	done := make(chan error, 1)
	go func() {
		done <- recv.Run(ctx, func(context.Context, ports.Delivery) error {
			once.Do(func() { close(firstEmit) })
			<-release // simulate pipeline backpressure on every delivery
			return nil
		})
	}()

	<-firstEmit
	// While the FIRST emit is still blocked, renewal goroutines for the
	// whole batch must already be ticking.
	waitUntil(t, 5*time.Second, func() bool { return fake.TickerCount() >= 3 },
		"auto-extend must start for all batched messages before the emit loop")

	close(release)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

// --- Finding: unbounded lock renewal -----------------------------------

// Renewal must stop at MaxLockRenewalDuration: processing is cancelled,
// the cap metric fires, and no further renewals happen.
func TestAutoExtend_MaxLockRenewalCap(t *testing.T) {
	t.Parallel()

	fake := clocktest.New()
	rec := &ports.RecordingExporter{}
	renewCalled := make(chan struct{})
	mock := &mockASBClient{
		RenewMessageLockFn: func(context.Context, *azservicebus.ReceivedMessage, *azservicebus.RenewMessageLockOptions) error {
			renewCalled <- struct{}{}
			return nil
		},
	}

	deliveryCtx, deliveryCancel := context.WithCancel(context.Background())
	defer deliveryCancel()

	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "cap"})
	msg := &azservicebus.ReceivedMessage{MessageID: "cap"}
	d := newDelivery(deliveryCtx, env, mock, nil, msg,
		deliveryTuning{lockDuration: 2 * time.Second, autoExtend: true, maxLockRenewal: 3 * time.Second},
		deliveryCancel, nil, rec, fake)
	defer d.stop()

	for fake.TickerCount() == 0 {
		runtime.Gosched()
	}

	// interval = 1s; deadline = start + 3s. Ticks at 1s and 2s renew.
	fake.Advance(1100 * time.Millisecond)
	<-renewCalled
	fake.Advance(1100 * time.Millisecond)
	<-renewCalled

	// Third tick fires past the 3s cap: no renewal, processing cancelled.
	fake.Advance(1100 * time.Millisecond)
	select {
	case <-deliveryCtx.Done():
	case <-renewCalled:
		t.Fatal("renewal past MaxLockRenewalDuration must not happen")
	case <-time.After(5 * time.Second):
		t.Fatal("processing context must be cancelled when the renewal cap is exceeded")
	}

	require.Len(t, rec.FindEntries(MetricASBLockRenewalCapExceeded), 1)
}

// --- Finding: poll receive failures unobservable -----------------------

// Failed polls must increment MetricASBReceiveFailures.
func TestPollLoop_ReceiveFailureMetric(t *testing.T) {
	t.Parallel()

	fake := clocktest.New()
	rec := &ports.RecordingExporter{}
	mock := &mockASBClient{
		ReceiveMessagesFn: func(context.Context, int, *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error) {
			return nil, errors.New("boom")
		},
	}
	recv, err := NewReceiver(ReceiverConfig{
		QueueName: "q",
		Client:    mock,
		Clock:     fake,
		Metrics:   rec,
	}, nil)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- recv.Run(ctx, func(context.Context, ports.Delivery) error { return nil })
	}()

	waitUntil(t, 5*time.Second, func() bool {
		return len(rec.FindEntries(MetricASBReceiveFailures)) >= 1
	}, "receive failure metric never recorded")

	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

// --- Finding: validation gaps (receive_mode / sub_queue / clamp) -----------

func TestReceiverConfig_Validate_ReceiveModeAndSubQueue(t *testing.T) {
	t.Parallel()

	base := func() ReceiverConfig {
		return ReceiverConfig{QueueName: "q", Client: &mockASBClient{}}
	}

	tests := []struct {
		name    string
		mutate  func(*ReceiverConfig)
		wantErr bool
	}{
		{"empty mode ok", func(*ReceiverConfig) {}, false},
		{"PeekLock ok", func(c *ReceiverConfig) { c.ReceiveMode = "PeekLock" }, false},
		{"peeklock case-insensitive ok", func(c *ReceiverConfig) { c.ReceiveMode = "peeklock" }, false},
		{"RECEIVEANDDELETE case-insensitive ok", func(c *ReceiverConfig) { c.ReceiveMode = "RECEIVEANDDELETE" }, false},
		{"unknown mode rejected", func(c *ReceiverConfig) { c.ReceiveMode = "PeekAndLock" }, true},
		{"deadletter ok", func(c *ReceiverConfig) { c.SubQueue = "deadletter" }, false},
		{"DeadLetter case-insensitive ok", func(c *ReceiverConfig) { c.SubQueue = "DeadLetter" }, false},
		{"transferdeadletter ok", func(c *ReceiverConfig) { c.SubQueue = "transferdeadletter" }, false},
		{"dead-letter typo rejected", func(c *ReceiverConfig) { c.SubQueue = "dead-letter" }, true},
		{"dlq typo rejected", func(c *ReceiverConfig) { c.SubQueue = "dlq" }, true},
		{"session+sub_queue rejected", func(c *ReceiverConfig) { c.SessionID = "s"; c.SubQueue = "deadletter" }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base()
			tt.mutate(&cfg)
			err := cfg.validate()
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestConfig_Validate_ReceiveModeSubQueueSession(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		rcv     ReceiverParams
		wantErr bool
	}{
		{"valid", ReceiverParams{QueueName: "q", ReceiveMode: "peeklock", SubQueue: "DEADLETTER"}, false},
		{"bad mode", ReceiverParams{QueueName: "q", ReceiveMode: "delete"}, true},
		{"bad sub_queue", ReceiverParams{QueueName: "q", SubQueue: "dead-letter"}, true},
		{"session+sub_queue", ReceiverParams{QueueName: "q", SessionID: "s", SubQueue: "deadletter"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{Receiver: tt.rcv}
			err := cfg.Validate()
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

// buildReceiverOptions must map any ACCEPTED sub_queue casing onto the
// SDK enum — never silently fall through to the main queue.
func TestBuildReceiverOptions_SubQueueCaseInsensitive(t *testing.T) {
	t.Parallel()

	require.Equal(t, azservicebus.SubQueueDeadLetter,
		buildReceiverOptions(asbReceiverOptions{SubQueue: "DeadLetter"}).SubQueue)
	require.Equal(t, azservicebus.SubQueueTransfer,
		buildReceiverOptions(asbReceiverOptions{SubQueue: "TRANSFERDEADLETTER"}).SubQueue)
	require.Equal(t, azservicebus.SubQueue(0),
		buildReceiverOptions(asbReceiverOptions{}).SubQueue)
}

// receiveAndDelete matching is case-insensitive so an accepted casing
// can never silently run PeekLock semantics.
func TestReceiverConfig_ReceiveAndDeleteCaseInsensitive(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"ReceiveAndDelete", "receiveanddelete", "RECEIVEANDDELETE"} {
		c := ReceiverConfig{ReceiveMode: mode}
		require.True(t, c.receiveAndDelete(), mode)
	}
	for _, mode := range []string{"", "PeekLock", "peeklock"} {
		c := ReceiverConfig{ReceiveMode: mode}
		require.False(t, c.receiveAndDelete(), mode)
	}
}

// --- Finding: ReceiveAndDelete shutdown loss window --------------------

// ReceiveAndDelete pre-settles at the broker; MaxMessages must be
// clamped to 1 so at most one message is in the loss window.
func TestNewReceiver_ReceiveAndDeleteClampsMaxMessages(t *testing.T) {
	t.Parallel()

	recv, err := NewReceiver(ReceiverConfig{
		QueueName:   "q",
		ReceiveMode: "ReceiveAndDelete",
		MaxMessages: 50,
		Client:      &mockASBClient{},
	}, nil)
	require.NoError(t, err)
	require.Equal(t, 1, recv.cfg.MaxMessages)

	// PeekLock keeps the configured batch size.
	recv2, err := NewReceiver(ReceiverConfig{
		QueueName:   "q",
		MaxMessages: 50,
		Client:      &mockASBClient{},
	}, nil)
	require.NoError(t, err)
	require.Equal(t, 50, recv2.cfg.MaxMessages)
}

// --- Finding: max_lock_renewal_duration default -------------------------

func TestReceiverConfig_ApplyDefaults_MaxLockRenewal(t *testing.T) {
	t.Parallel()

	c := ReceiverConfig{}
	c.applyDefaults()
	require.Equal(t, defaultMaxLockRenewalDuration, c.MaxLockRenewalDuration)

	c2 := ReceiverConfig{MaxLockRenewalDuration: time.Minute}
	c2.applyDefaults()
	require.Equal(t, time.Minute, c2.MaxLockRenewalDuration)
}
