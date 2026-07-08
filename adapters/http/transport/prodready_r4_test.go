package transport_test

// Black-box tests for the prod-ready R4 remediation (findings HTTP-H1
// through HTTP-L11). Deterministic per TESTS.md: fake clock for every
// time-dependent path, testutil/wait for synchronisation, recording
// metrics exporter for observability assertions, no sleeps.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/http/transport"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// runAckingReceiver runs recv with an emit that counts deliveries and
// Acks immediately. The Run goroutine is cancelled via t.Cleanup.
func runAckingReceiver(t *testing.T, recv ports.Receiver, count *atomic.Int64) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = recv.Run(ctx, func(ctx context.Context, d ports.Delivery) error {
			if count != nil {
				count.Add(1)
			}
			return d.Ack(ctx)
		})
	}()
	waitReceiverReady(t, recv, 2*time.Second)
}

// stubBreaker is a hand-rolled ports.CircuitBreaker fake.
type stubBreaker struct {
	rejectWith error // returned from BeforeRequest when non-nil
	mu         sync.Mutex
	before     int
	outcomes   []error
}

func (b *stubBreaker) BeforeRequest() error {
	b.mu.Lock()
	b.before++
	b.mu.Unlock()
	return b.rejectWith
}

func (b *stubBreaker) AfterRequest(err error) {
	b.mu.Lock()
	b.outcomes = append(b.outcomes, err)
	b.mu.Unlock()
}

func (b *stubBreaker) snapshot() (int, []error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]error, len(b.outcomes))
	copy(out, b.outcomes)
	return b.before, out
}

// ---------------------------------------------------------------------------
// Finding HTTP-H1: SSE Send observability for zero-delivery outcomes
// ---------------------------------------------------------------------------

func TestSSESender_Send_NoSubscribersEmitsMetric(t *testing.T) {
	rec := &ports.RecordingExporter{}
	factory := transport.NewFactory(transport.WithFactoryMetrics(rec))
	sender, err := factory.NewSender(context.Background(),
		ports.SenderSpec{ID: "nosub", Config: transport.Config{Mode: "sse"}}, nil)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}

	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID: "evt-nosub", Subject: "s.nosub", Payload: []byte(`{}`),
	})
	// SSE egress is at-most-once by contract: no subscribers is NOT an
	// error, but it must be counted so operators can alert on it.
	if err := sender.Send(context.Background(), ports.OutboundMessage{Envelope: env}); err != nil {
		t.Fatalf("Send with zero subscribers must return nil (documented at-most-once), got %v", err)
	}

	entries := rec.FindEntries(transport.MetricSSENoSubscribers)
	if len(entries) != 1 {
		t.Fatalf("expected 1 %s entry, got %d", transport.MetricSSENoSubscribers, len(entries))
	}
}

// ---------------------------------------------------------------------------
// Finding HTTP-H2: 429/408 are transient; Retry-After honoured
// ---------------------------------------------------------------------------

func TestForwarder_429And408AreTransient(t *testing.T) {
	cases := []struct {
		code     int
		sentinel *shared.BridgeError
	}{
		{http.StatusTooManyRequests, shared.ErrThrottled},
		{http.StatusRequestTimeout, shared.ErrTimeout},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.code), func(t *testing.T) {
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.code)
			}))
			defer remote.Close()

			fwd := transport.NewHTTPForwarderWithConfig("/transport/http",
				transport.ForwarderConfig{MaxRetries: 0, Timeout: 5 * time.Second})
			peer := &persistence.PeerInfo{
				InstanceID: "throttled-peer",
				Endpoints:  map[string]string{"http": remote.URL},
			}
			env := messaging.MustEnvelope(messaging.EnvelopeInput{
				ID: "msg-transient", Subject: "t", Payload: []byte(`{}`),
			})

			err := fwd.Forward(context.Background(), peer, "recv-1", env)
			if err == nil {
				t.Fatalf("expected error for status %d", tc.code)
			}
			if !shared.IsRecoverableError(err) {
				t.Fatalf("status %d must be transient (retried, not dead-lettered), got non-recoverable: %v", tc.code, err)
			}
			if !errors.Is(err, tc.sentinel) {
				t.Fatalf("status %d must map to %v, got %v", tc.code, tc.sentinel, err)
			}
		})
	}
}

func TestForwarder_RetryAfterHintSurfacedAndClamped(t *testing.T) {
	cases := []struct {
		name       string
		retryAfter string
		want       time.Duration
	}{
		{"seconds", "7", 7 * time.Second},
		{"clamped_to_max", "86400", 30 * time.Second},
		{"garbage_ignored", "soon", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", tc.retryAfter)
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer remote.Close()

			fwd := transport.NewHTTPForwarderWithConfig("/transport/http",
				transport.ForwarderConfig{MaxRetries: 0, Timeout: 5 * time.Second})
			peer := &persistence.PeerInfo{
				InstanceID: "peer-ra",
				Endpoints:  map[string]string{"http": remote.URL},
			}
			env := messaging.MustEnvelope(messaging.EnvelopeInput{
				ID: "msg-ra", Subject: "t", Payload: []byte(`{}`),
			})

			err := fwd.Forward(context.Background(), peer, "recv-ra", env)
			if err == nil {
				t.Fatal("expected error")
			}
			if got := shared.GetRetryAfter(err); got != tc.want {
				t.Fatalf("Retry-After %q: GetRetryAfter = %v, want %v", tc.retryAfter, got, tc.want)
			}
		})
	}
}

func TestForwarder_RetryWaitHonoursRetryAfterHint(t *testing.T) {
	var requests atomic.Int64
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Retry-After", "5")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer remote.Close()

	fake := clocktest.NewAt(time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC))
	fwd := transport.NewHTTPForwarderWithConfig("/transport/http", transport.ForwarderConfig{
		MaxRetries:        1,
		RetryInitialDelay: time.Millisecond,
		RetryMaxDelay:     2 * time.Millisecond,
		Timeout:           5 * time.Second,
		Clock:             fake,
	})
	peer := &persistence.PeerInfo{
		InstanceID: "peer-wait",
		Endpoints:  map[string]string{"http": remote.URL},
	}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID: "msg-wait", Subject: "t", Payload: []byte(`{}`),
	})

	result := make(chan error, 1)
	go func() { result <- fwd.Forward(context.Background(), peer, "recv-wait", env) }()

	// Forwarder got the 429 and is now parked on Clock.After(hint).
	wait.Until(t, 2*time.Second, "retry timer registered", func() bool {
		return fake.TimerCount() >= 1
	})

	// The hint (5s) must override the 1ms backoff: 1s of fake time is
	// not enough for the retry to fire.
	fake.Advance(1 * time.Second)
	select {
	case err := <-result:
		t.Fatalf("retry fired before the Retry-After hint elapsed: %v", err)
	default:
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("expected 1 request before the hint elapses, got %d", n)
	}

	fake.Advance(4 * time.Second) // total 5s == hint
	if err := wait.RequireReceive(t, result, 2*time.Second); err != nil {
		t.Fatalf("Forward after honoured Retry-After: %v", err)
	}
	if n := requests.Load(); n != 2 {
		t.Fatalf("expected exactly 2 requests, got %d", n)
	}
}

// ---------------------------------------------------------------------------
// Finding HTTP-H3: client disconnect must not abort dispatch
// ---------------------------------------------------------------------------

func TestReceiver_ClientDisconnectDoesNotAbortDispatch(t *testing.T) {
	factory := transport.NewFactory()
	recv, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{ID: "detach"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	type emitted struct {
		ctx context.Context
		del ports.Delivery
	}
	emitCh := make(chan emitted, 1)

	runCtx, cancelRun := context.WithCancel(context.Background())
	t.Cleanup(cancelRun)
	go func() {
		_ = recv.Run(runCtx, func(ctx context.Context, d ports.Delivery) error {
			emitCh <- emitted{ctx: ctx, del: d}
			return nil
		})
	}()
	waitReceiverReady(t, recv, 2*time.Second)

	clientCtx, disconnect := context.WithCancel(context.Background())
	body := strings.NewReader(`{"subject":"t.detach","payload":{}}`)
	req := httptest.NewRequest("POST", "/transport/http/receivers/detach/messages", body).
		WithContext(clientCtx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	served := make(chan struct{})
	go func() {
		factory.Handler().ServeHTTP(rec, req)
		close(served)
	}()

	got := wait.RequireReceive(t, emitCh, 2*time.Second)

	// Simulate the client dropping the connection mid-dispatch.
	disconnect()
	wait.RequireClosed(t, served, 2*time.Second)

	// The HANDLER answers 504 (outcome unknown to the client)...
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504 to the disconnected client, got %d: %s", rec.Code, rec.Body.String())
	}
	// ...but the DISPATCH context must survive the disconnect so the
	// pipeline can run to completion.
	if err := got.ctx.Err(); err != nil {
		t.Fatalf("dispatch context aborted by client disconnect: %v", err)
	}
	// Late pipeline completion must not error or block.
	if err := got.del.Ack(context.Background()); err != nil {
		t.Fatalf("late Ack after client disconnect: %v", err)
	}
}

// TestReceiver_DispatchBoundedByMaxDispatchDuration pins the second half
// of the HTTP-H3 contract after finding 3: the dispatch context is
// detached from the client's CANCELLATION (context.WithoutCancel) but is
// UNCONDITIONALLY bounded by MaxDispatchDuration — it does NOT rely on
// the client/request context carrying a deadline (a bare http.Server
// installs none). The dispatch deadline is therefore the configured cap,
// not the client's far-future deadline. Client disconnect still must not
// abort the dispatch.
func TestReceiver_DispatchBoundedByMaxDispatchDuration(t *testing.T) {
	factory := transport.NewFactory()
	// 30m cap: comfortably inside the far-future client deadline below,
	// so the dispatch deadline being the cap (not the client's) is
	// observable, yet far enough out never to fire during the test.
	const maxDispatch = 30 * time.Minute
	recv, err := factory.NewReceiver(context.Background(),
		ports.ReceiverSpec{ID: "bound", Config: transport.Config{MaxDispatchDuration: maxDispatch}}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	type emitted struct {
		ctx context.Context
		del ports.Delivery
	}
	emitCh := make(chan emitted, 1)

	runCtx, cancelRun := context.WithCancel(context.Background())
	t.Cleanup(cancelRun)
	go func() {
		_ = recv.Run(runCtx, func(ctx context.Context, d ports.Delivery) error {
			emitCh <- emitted{ctx: ctx, del: d}
			return nil
		})
	}()
	waitReceiverReady(t, recv, 2*time.Second)

	// Far-future absolute client deadline (1h): the dispatch must be
	// bounded by the 30m cap, i.e. STRICTLY before this, proving the cap
	// — not the client deadline — governs the detached dispatch.
	deadline := time.Now().Add(time.Hour)
	clientCtx, cancelClient := context.WithDeadline(context.Background(), deadline)
	t.Cleanup(cancelClient)

	body := strings.NewReader(`{"subject":"t.bound","payload":{}}`)
	req := httptest.NewRequest("POST", "/transport/http/receivers/bound/messages", body).
		WithContext(clientCtx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	served := make(chan struct{})
	go func() {
		factory.Handler().ServeHTTP(rec, req)
		close(served)
	}()

	got := wait.RequireReceive(t, emitCh, 2*time.Second)

	gotDeadline, ok := got.ctx.Deadline()
	if !ok {
		t.Fatal("dispatch context has no deadline: detached dispatch must stay bounded by max_dispatch_duration")
	}
	// Bounded by the cap, not the client's far-future deadline.
	if !gotDeadline.Before(deadline) {
		t.Fatalf("dispatch deadline = %v, want strictly before the client deadline %v (bounded by max_dispatch_duration)", gotDeadline, deadline)
	}

	// Client disconnect (before the dispatch cap) must still not abort
	// the detached-but-bounded dispatch.
	cancelClient()
	wait.RequireClosed(t, served, 2*time.Second)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504 to the disconnected client, got %d: %s", rec.Code, rec.Body.String())
	}
	if err := got.ctx.Err(); err != nil {
		t.Fatalf("client disconnect aborted the bounded dispatch context: %v", err)
	}
	// Late settlement must not error and releases the dispatch deadline.
	if err := got.del.Ack(context.Background()); err != nil {
		t.Fatalf("late Ack after client disconnect: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Finding HTTP-M4: operator paths fail the build, never panic
// ---------------------------------------------------------------------------

func TestConfig_Validate_RejectsBadPaths(t *testing.T) {
	cases := []struct {
		name string
		path string
		ok   bool
	}{
		{"missing_leading_slash", "nopath", false},
		{"servemux_wildcard", "/a/{b}", false},
		{"embedded_space", "/a b", false},
		{"valid", "/custom/ingress", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := transport.Config{Path: tc.path}.Validate()
			if tc.ok && err != nil {
				t.Fatalf("Validate(%q): unexpected error %v", tc.path, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("Validate(%q): expected error", tc.path)
			}
		})
	}
}

func TestFactory_BadPathReturnsErrorNotPanic(t *testing.T) {
	factory := transport.NewFactory()

	// Configured path without leading slash.
	if _, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{
		ID: "bad-cfg-path", Config: transport.Config{Path: "nopath"},
	}, nil); err == nil {
		t.Fatal("NewReceiver with unrooted path must return an error")
	}

	// Spec ID smuggling ServeMux metacharacters into the generated path.
	if _, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{
		ID: "bad{id}",
	}, nil); err == nil {
		t.Fatal("NewReceiver with ServeMux metacharacters in the generated path must return an error")
	}

	// Same guard on the sender side.
	if _, err := factory.NewSender(context.Background(), ports.SenderSpec{
		ID: "bad-sse", Config: transport.Config{Mode: "sse", Path: "nopath"},
	}, nil); err == nil {
		t.Fatal("NewSender with unrooted path must return an error")
	}
}

// ---------------------------------------------------------------------------
// Finding HTTP-M5: ingress idempotency window + forwarder Idempotency-Key
// ---------------------------------------------------------------------------

func TestReceiver_IngressDedup_DuplicateAcknowledgedWithoutReEmit(t *testing.T) {
	cases := []struct {
		name   string
		header string
	}{
		{"idempotency_key", "Idempotency-Key"},
		{"dedup_id", "X-Dedup-Id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &ports.RecordingExporter{}
			factory := transport.NewFactory(transport.WithFactoryMetrics(rec))
			recv, err := factory.NewReceiver(context.Background(),
				ports.ReceiverSpec{ID: "dedup-" + tc.name}, nil)
			if err != nil {
				t.Fatalf("NewReceiver: %v", err)
			}
			var emits atomic.Int64
			runAckingReceiver(t, recv, &emits)

			url := "/transport/http/receivers/dedup-" + tc.name + "/messages"
			headers := map[string]string{tc.header: "dup-key-1"}
			body := map[string]any{"subject": "t.dup", "payload": json.RawMessage(`{}`)}

			if res := postJSON(t, factory.Handler(), url, body, headers); res.Code != http.StatusOK {
				t.Fatalf("first POST: expected 200, got %d: %s", res.Code, res.Body.String())
			}
			if res := postJSON(t, factory.Handler(), url, body, headers); res.Code != http.StatusOK {
				t.Fatalf("duplicate POST: expected 200 (absorbed), got %d: %s", res.Code, res.Body.String())
			}

			if n := emits.Load(); n != 1 {
				t.Fatalf("expected exactly 1 emit for duplicate key, got %d", n)
			}
			if n := len(rec.FindEntries(transport.MetricHTTPIngressDuplicates)); n != 1 {
				t.Fatalf("expected 1 %s entry, got %d", transport.MetricHTTPIngressDuplicates, n)
			}
		})
	}
}

func TestReceiver_IngressDedup_FailedAttemptIsReprocessed(t *testing.T) {
	factory := transport.NewFactory()
	recv, err := factory.NewReceiver(context.Background(),
		ports.ReceiverSpec{ID: "dedup-fail"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	var emits atomic.Int64
	runCtx, cancelRun := context.WithCancel(context.Background())
	t.Cleanup(cancelRun)
	go func() {
		_ = recv.Run(runCtx, func(ctx context.Context, d ports.Delivery) error {
			if emits.Add(1) == 1 {
				return d.Retry(ctx, 0, errors.New("first attempt fails"))
			}
			return d.Ack(ctx)
		})
	}()
	waitReceiverReady(t, recv, 2*time.Second)

	url := "/transport/http/receivers/dedup-fail/messages"
	headers := map[string]string{"Idempotency-Key": "retry-key-1"}
	body := map[string]any{"subject": "t.retry", "payload": json.RawMessage(`{}`)}

	if res := postJSON(t, factory.Handler(), url, body, headers); res.Code != http.StatusInternalServerError {
		t.Fatalf("failed attempt: expected 500, got %d", res.Code)
	}
	// The key must NOT have been recorded on failure: the client retry
	// is re-processed, not swallowed.
	if res := postJSON(t, factory.Handler(), url, body, headers); res.Code != http.StatusOK {
		t.Fatalf("retry after failure: expected 200, got %d: %s", res.Code, res.Body.String())
	}
	if n := emits.Load(); n != 2 {
		t.Fatalf("expected 2 emits (failure not recorded in window), got %d", n)
	}
}

func TestReceiver_IngressDedup_NoKeyMeansNoDedup(t *testing.T) {
	factory := transport.NewFactory()
	recv, err := factory.NewReceiver(context.Background(),
		ports.ReceiverSpec{ID: "dedup-none"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	var emits atomic.Int64
	runAckingReceiver(t, recv, &emits)

	url := "/transport/http/receivers/dedup-none/messages"
	body := map[string]any{"subject": "t.none", "payload": json.RawMessage(`{}`)}
	for i := 0; i < 2; i++ {
		if res := postJSON(t, factory.Handler(), url, body, nil); res.Code != http.StatusOK {
			t.Fatalf("POST %d: expected 200, got %d", i+1, res.Code)
		}
	}
	if n := emits.Load(); n != 2 {
		t.Fatalf("keyless requests must not dedup, expected 2 emits, got %d", n)
	}
}

func TestForwarder_SetsIdempotencyKeyOnForwards(t *testing.T) {
	keys := make(chan string, 2)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys <- r.Header.Get("Idempotency-Key")
		w.WriteHeader(http.StatusOK)
	}))
	defer remote.Close()

	fwd := transport.NewHTTPForwarderWithConfig("/transport/http",
		transport.ForwarderConfig{MaxRetries: 0, Timeout: 5 * time.Second})
	peer := &persistence.PeerInfo{
		InstanceID: "peer-ik",
		Endpoints:  map[string]string{"http": remote.URL},
	}

	// Envelope WITHOUT its own idempotency key: derived from envelope ID.
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID: "env-derived-1", Subject: "t", Payload: []byte(`{}`),
	})
	if err := fwd.Forward(context.Background(), peer, "recv-ik", env); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if got := wait.RequireReceive(t, keys, 2*time.Second); got != "env-derived-1" {
		t.Fatalf("expected Idempotency-Key derived from envelope ID, got %q", got)
	}

	// Envelope WITH its own key: preserved, not overwritten.
	envKeyed := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID: "env-keyed-1", Subject: "t", Payload: []byte(`{}`), IdempotencyKey: "client-key-9",
	})
	if err := fwd.Forward(context.Background(), peer, "recv-ik", envKeyed); err != nil {
		t.Fatalf("Forward keyed: %v", err)
	}
	if got := wait.RequireReceive(t, keys, 2*time.Second); got != "client-key-9" {
		t.Fatalf("expected envelope's own Idempotency-Key preserved, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Finding HTTP-M6: circuit breaker on forwarder; SSE Close/drain
// ---------------------------------------------------------------------------

func TestForwarder_BreakerOpenFailsFast(t *testing.T) {
	var requests atomic.Int64
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer remote.Close()

	rejection := shared.ErrUnavailable.
		WithMessage("circuit open").
		WithRetryAfter(2 * time.Second)
	breaker := &stubBreaker{rejectWith: rejection}
	rec := &ports.RecordingExporter{}
	fwd := transport.NewHTTPForwarderWithConfig("/transport/http", transport.ForwarderConfig{
		Timeout: 5 * time.Second,
		Breaker: breaker,
		Metrics: rec,
	})
	peer := &persistence.PeerInfo{
		InstanceID: "dead-peer",
		Endpoints:  map[string]string{"http": remote.URL},
	}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID: "msg-breaker", Subject: "t", Payload: []byte(`{}`),
	})

	err := fwd.Forward(context.Background(), peer, "recv-cb", env)
	if err == nil {
		t.Fatal("expected breaker-open rejection")
	}
	if !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("breaker rejection must surface unchanged (ErrUnavailable), got %v", err)
	}
	if got := shared.GetRetryAfter(err); got != 2*time.Second {
		t.Fatalf("breaker RetryAfter hint must survive, got %v", got)
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("open breaker must fail fast WITHOUT a network call, saw %d requests", n)
	}
	if n := len(rec.FindEntries(transport.MetricHTTPForwardBreakerOpen)); n != 1 {
		t.Fatalf("expected 1 %s entry, got %d", transport.MetricHTTPForwardBreakerOpen, n)
	}
	if before, outcomes := breaker.snapshot(); before != 1 || len(outcomes) != 0 {
		t.Fatalf("rejected request must not be recorded as an outcome: before=%d outcomes=%d", before, len(outcomes))
	}
}

func TestForwarder_BreakerObservesOutcome(t *testing.T) {
	var status atomic.Int64
	status.Store(http.StatusInternalServerError)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	defer remote.Close()

	breaker := &stubBreaker{}
	fwd := transport.NewHTTPForwarderWithConfig("/transport/http", transport.ForwarderConfig{
		MaxRetries: 0,
		Timeout:    5 * time.Second,
		Breaker:    breaker,
	})
	peer := &persistence.PeerInfo{
		InstanceID: "flaky-peer",
		Endpoints:  map[string]string{"http": remote.URL},
	}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID: "msg-outcome", Subject: "t", Payload: []byte(`{}`),
	})

	if err := fwd.Forward(context.Background(), peer, "recv-out", env); err == nil {
		t.Fatal("expected error for 500")
	}
	status.Store(http.StatusOK)
	if err := fwd.Forward(context.Background(), peer, "recv-out", env); err != nil {
		t.Fatalf("Forward after recovery: %v", err)
	}

	before, outcomes := breaker.snapshot()
	if before != 2 || len(outcomes) != 2 {
		t.Fatalf("expected 2 gated requests with 2 outcomes, got before=%d outcomes=%d", before, len(outcomes))
	}
	if outcomes[0] == nil {
		t.Fatal("failed forward must be recorded as a failure outcome")
	}
	if outcomes[1] != nil {
		t.Fatalf("successful forward must be recorded as success, got %v", outcomes[1])
	}
}

func TestSSESender_CloseUnblocksHandlersAndRefusesNewClients(t *testing.T) {
	factory := transport.NewFactory()
	sender, err := factory.NewSender(context.Background(),
		ports.SenderSpec{ID: "drain", Config: transport.Config{Mode: "sse"}}, nil)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	sse, ok := sender.(*transport.SSESender)
	if !ok {
		t.Fatalf("expected *transport.SSESender, got %T", sender)
	}

	srv := httptest.NewServer(factory.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/transport/http/senders/drain/events")
	if err != nil {
		t.Fatalf("connect SSE client: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	streamEnded := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		close(streamEnded)
	}()

	waitCtx, cancelWait := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelWait()
	if err := sse.WaitClientConnected(waitCtx, 1); err != nil {
		t.Fatalf("waiting for SSE client: %v", err)
	}

	closeCtx, cancelClose := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelClose()
	if err := sse.Close(closeCtx); err != nil {
		t.Fatalf("Close must drain connected clients within the deadline: %v", err)
	}
	wait.RequireClosed(t, streamEnded, 2*time.Second)
	if n := sse.ClientCount(); n != 0 {
		t.Fatalf("expected 0 clients after Close, got %d", n)
	}

	// A client arriving during/after shutdown is refused, not parked.
	late, err := http.Get(srv.URL + "/transport/http/senders/drain/events")
	if err != nil {
		t.Fatalf("late client: %v", err)
	}
	defer func() { _ = late.Body.Close() }()
	if late.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for client after Close, got %d", late.StatusCode)
	}
}

func TestFactory_CloseDrainsAllSSESenders(t *testing.T) {
	factory := transport.NewFactory()
	senders := make([]*transport.SSESender, 0, 2)
	for _, id := range []string{"fan-a", "fan-b"} {
		s, err := factory.NewSender(context.Background(),
			ports.SenderSpec{ID: id, Config: transport.Config{Mode: "sse"}}, nil)
		if err != nil {
			t.Fatalf("NewSender %s: %v", id, err)
		}
		senders = append(senders, s.(*transport.SSESender))
	}

	srv := httptest.NewServer(factory.Handler())
	t.Cleanup(srv.Close)

	for i, id := range []string{"fan-a", "fan-b"} {
		resp, err := http.Get(srv.URL + "/transport/http/senders/" + id + "/events")
		if err != nil {
			t.Fatalf("connect %s: %v", id, err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		go func() { _, _ = io.Copy(io.Discard, resp.Body) }()

		waitCtx, cancelWait := context.WithTimeout(context.Background(), 2*time.Second)
		if err := senders[i].WaitClientConnected(waitCtx, 1); err != nil {
			cancelWait()
			t.Fatalf("waiting for client on %s: %v", id, err)
		}
		cancelWait()
	}

	closeCtx, cancelClose := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelClose()
	if err := factory.Close(closeCtx); err != nil {
		t.Fatalf("Factory.Close: %v", err)
	}
	for i, s := range senders {
		if n := s.ClientCount(); n != 0 {
			t.Fatalf("sender %d still has %d clients after Factory.Close", i, n)
		}
	}
}

// ---------------------------------------------------------------------------
// Finding HTTP-M8: SSE redirect is opt-in; internal endpoint never leaks
// ---------------------------------------------------------------------------

func TestSSESender_RemoteRoute_RedirectIsOptIn(t *testing.T) {
	const internalEndpoint = "http://internal-peer:8080"
	peer := &persistence.PeerInfo{
		InstanceID: "owner-node",
		Endpoints:  map[string]string{"http": internalEndpoint},
	}

	cases := []struct {
		name             string
		redirectEndpoint string
		wantStatus       int
		wantLocation     bool
	}{
		{"default_no_redirect", "", http.StatusServiceUnavailable, false},
		{"configured_key_missing_on_peer", "http_public", http.StatusServiceUnavailable, false},
		{"configured_key_present", "http", http.StatusTemporaryRedirect, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			factory := transport.NewFactory(
				transport.WithRouteLocator(&stubLocator{peer: peer, local: false}),
			)
			sender, err := factory.NewSender(context.Background(), ports.SenderSpec{
				ID:     "redir-" + tc.name,
				Config: transport.Config{Mode: "sse", RedirectEndpoint: tc.redirectEndpoint},
			}, nil)
			if err != nil {
				t.Fatalf("NewSender: %v", err)
			}
			sender.(*transport.SSESender).SetRouteID("route-remote")

			req := httptest.NewRequest("GET", "/transport/http/senders/redir-"+tc.name+"/events", nil)
			rec := httptest.NewRecorder()
			factory.Handler().ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("expected %d, got %d: %s", tc.wantStatus, rec.Code, rec.Body.String())
			}
			loc := rec.Header().Get("Location")
			if tc.wantLocation && !strings.HasPrefix(loc, internalEndpoint) {
				t.Fatalf("expected redirect to configured endpoint, got Location %q", loc)
			}
			if !tc.wantLocation {
				if loc != "" {
					t.Fatalf("refusal must not carry a Location header, got %q", loc)
				}
				if strings.Contains(rec.Body.String(), internalEndpoint) {
					t.Fatalf("refusal body leaks the internal endpoint: %s", rec.Body.String())
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Finding HTTP-M9: per-receiver forward auth keys
// ---------------------------------------------------------------------------

func TestForwarder_PerReceiverAPIKeyWithClusterFallback(t *testing.T) {
	apiKeys := make(chan string, 2)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiKeys <- r.Header.Get("X-API-Key")
		w.WriteHeader(http.StatusOK)
	}))
	defer remote.Close()

	fwd := transport.NewHTTPForwarderWithConfig("/transport/http", transport.ForwarderConfig{
		MaxRetries: 0,
		Timeout:    5 * time.Second,
		ReceiverAPIKeys: map[string]string{
			"recv-a": "receiver-a-key-0123",
		},
	}, "shared-cluster-key1")
	peer := &persistence.PeerInfo{
		InstanceID: "peer-keys",
		Endpoints:  map[string]string{"http": remote.URL},
	}
	env := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID: "msg-keys", Subject: "t", Payload: []byte(`{}`),
	})

	if err := fwd.Forward(context.Background(), peer, "recv-a", env); err != nil {
		t.Fatalf("Forward recv-a: %v", err)
	}
	if got := wait.RequireReceive(t, apiKeys, 2*time.Second); got != "receiver-a-key-0123" {
		t.Fatalf("recv-a must use its per-receiver key, got %q", got)
	}

	if err := fwd.Forward(context.Background(), peer, "recv-b", env); err != nil {
		t.Fatalf("Forward recv-b: %v", err)
	}
	if got := wait.RequireReceive(t, apiKeys, 2*time.Second); got != "shared-cluster-key1" {
		t.Fatalf("recv-b must fall back to the cluster key, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Finding HTTP-L10: Retry(nil) is a retry, not an Ack
// ---------------------------------------------------------------------------

func TestHTTPDelivery_RetryNilReasonReturns500(t *testing.T) {
	factory := transport.NewFactory()
	recv, err := factory.NewReceiver(context.Background(),
		ports.ReceiverSpec{ID: "retry-nil"}, nil)
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}

	runCtx, cancelRun := context.WithCancel(context.Background())
	t.Cleanup(cancelRun)
	go func() {
		_ = recv.Run(runCtx, func(ctx context.Context, d ports.Delivery) error {
			// The buggy behaviour treated a nil reason as success (200).
			return d.Retry(ctx, 0, nil)
		})
	}()
	waitReceiverReady(t, recv, 2*time.Second)

	res := postJSON(t, factory.Handler(), "/transport/http/receivers/retry-nil/messages",
		map[string]any{"subject": "t.retrynil", "payload": json.RawMessage(`{}`)}, nil)
	if res.Code != http.StatusInternalServerError {
		t.Fatalf("Retry(nil) must be indistinguishable from Retry(err) — expected 500, got %d: %s",
			res.Code, res.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Finding HTTP-L11: protocol strictness (Content-Type, auth scheme)
// ---------------------------------------------------------------------------

func TestReceiver_ContentTypeMatching(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		wantStatus  int
	}{
		{"uppercase_type", "Application/JSON", http.StatusOK},
		{"uppercase_with_charset", "APPLICATION/JSON; charset=UTF-8", http.StatusOK},
		{"prefix_overmatch_rejected", "application/jsonfoo", http.StatusUnsupportedMediaType},
		{"wrong_type_rejected", "text/json", http.StatusUnsupportedMediaType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			factory := transport.NewFactory()
			recv, err := factory.NewReceiver(context.Background(),
				ports.ReceiverSpec{ID: "ct-" + tc.name}, nil)
			if err != nil {
				t.Fatalf("NewReceiver: %v", err)
			}
			runAckingReceiver(t, recv, nil)

			req := httptest.NewRequest("POST", "/transport/http/receivers/ct-"+tc.name+"/messages",
				strings.NewReader(`{"subject":"t.ct","payload":{}}`))
			req.Header.Set("Content-Type", tc.contentType)
			rec := httptest.NewRecorder()
			factory.Handler().ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("Content-Type %q: expected %d, got %d: %s",
					tc.contentType, tc.wantStatus, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestReceiver_BearerSchemeCaseInsensitive(t *testing.T) {
	const key = "super-secret-key-123"
	cases := []struct {
		name       string
		authHeader string
		wantStatus int
	}{
		{"canonical", "Bearer " + key, http.StatusOK},
		{"lowercase_scheme", "bearer " + key, http.StatusOK},
		{"uppercase_scheme", "BEARER " + key, http.StatusOK},
		{"wrong_token", "Bearer not-the-right-key99", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			factory := transport.NewFactory()
			recv, err := factory.NewReceiver(context.Background(), ports.ReceiverSpec{
				ID:     "auth-" + tc.name,
				Config: transport.Config{APIKey: shared.NewSecret(key)},
			}, nil)
			if err != nil {
				t.Fatalf("NewReceiver: %v", err)
			}
			runAckingReceiver(t, recv, nil)

			req := httptest.NewRequest("POST", "/transport/http/receivers/auth-"+tc.name+"/messages",
				strings.NewReader(`{"subject":"t.auth","payload":{}}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", tc.authHeader)
			rec := httptest.NewRecorder()
			factory.Handler().ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("Authorization %q: expected %d, got %d: %s",
					tc.authHeader, tc.wantStatus, rec.Code, rec.Body.String())
			}
		})
	}
}
