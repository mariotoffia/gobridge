package paho

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/autopaho"

	"github.com/mariotoffia/gobridge/circuitbreaker"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// fakeCM returns a non-nil ConnectionManager pointer suitable for
// bypassing nil-cm guards in unit tests. Calling its methods will
// likely segfault — only use when the code path under test does NOT
// invoke Publish/Subscribe/Unsubscribe.
var fakeCM = &autopaho.ConnectionManager{}

// ═══════════════════════════════════════════════════════════════════════════
// Sender + CircuitBreakerSender thorough analysis
// ═══════════════════════════════════════════════════════════════════════════

// TestAnaSender_NoSession_NoCM_ReturnsErrUnavailable validates the fast
// pre-check at the top of Send: if the session has no ConnectionManager,
// Send returns ErrUnavailable without attempting any IO.
func TestAnaSender_NoSession_NoCM_ReturnsErrUnavailable(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-no-cm",
	}, domain.SessionEphemeral, nil)

	s := NewSender(sess, SenderOptions{Timeout: time.Second, DefaultTopic: "t/x"})

	err := s.Send(context.Background(), &domain.Envelope{Subject: "t/x", Payload: []byte("p")})
	if err == nil {
		t.Fatal("expected error from Send when CM is nil")
	}
	be, ok := err.(*domain.BridgeError)
	if !ok {
		t.Fatalf("err type = %T, want *domain.BridgeError", err)
	}
	if be.Code != domain.ErrUnavailable.Code {
		t.Fatalf("err code = %s, want %s", be.Code, domain.ErrUnavailable.Code)
	}
}

// TestAnaSender_EmptyTopicAndNoDefault_ReturnsErrInvalidTopic validates
// that Send rejects envelopes without a usable topic with a typed error.
func TestAnaSender_EmptyTopicAndNoDefault_ReturnsErrInvalidTopic(t *testing.T) {
	// Construct a Session with a fake CM-non-nil to bypass the first
	// guard. We only need the topic-resolution code path.
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-empty-topic",
	}, domain.SessionEphemeral, nil)
	sess.mu.Lock()
	// Use the unexported sentinel — tests are in-package so this is OK.
	sess.cm = fakeCM
	sess.mu.Unlock()

	s := NewSender(sess, SenderOptions{Timeout: time.Second})

	err := s.Send(context.Background(), &domain.Envelope{Payload: []byte("p")})
	if err == nil {
		t.Fatal("expected error for empty topic / no default")
	}
	be, ok := err.(*domain.BridgeError)
	if !ok {
		t.Fatalf("err type = %T, want *domain.BridgeError", err)
	}
	if be.Code != domain.ErrInvalidTopic.Code {
		t.Fatalf("err code = %s, want %s", be.Code, domain.ErrInvalidTopic.Code)
	}
}

// TestAnaSender_DefaultTopicUsedWhenSubjectEmpty proves that the
// SenderOptions.DefaultTopic is used when the envelope has no subject.
// We assert this via PublishFromEnvelope — Send itself requires a real
// broker.
func TestAnaSender_DefaultTopicUsedWhenSubjectEmpty(t *testing.T) {
	env := &domain.Envelope{Payload: []byte("x")}
	pub := PublishFromEnvelope(env, SenderOptions{DefaultTopic: "fallback/t", QoS: 1})
	if pub.Topic != "fallback/t" {
		t.Fatalf("topic = %q, want %q", pub.Topic, "fallback/t")
	}
}

// TestAnaSender_ApplyTimeout_PreservesShorterParentDeadline pins the
// documented behaviour: if the caller's context already has a deadline,
// Sender.applyTimeout does NOT impose a (possibly shorter or longer)
// new one — the caller's deadline is honoured.
func TestAnaSender_ApplyTimeout_PreservesShorterParentDeadline(t *testing.T) {
	s := &Sender{opts: SenderOptions{Timeout: 60 * time.Second}, metrics: &noopTestExporter{}}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	got, gcancel := s.applyTimeout(ctx)
	defer gcancel()

	d, ok := got.Deadline()
	if !ok {
		t.Fatal("expected deadline preserved on returned context")
	}
	remaining := time.Until(d)
	if remaining > 200*time.Millisecond {
		t.Fatalf("remaining = %v; expected ~100ms (parent) not 60s (opts)", remaining)
	}
}

// TestAnaSender_ApplyTimeout_LongerParentDeadline_NotShortenedByOpts
// characterises the current behaviour: when the parent deadline is
// LONGER than opts.Timeout, opts.Timeout is IGNORED. The deadline is
// the longer parent deadline. This is a known design choice ("do not
// shorten an existing deadline") but operators should be aware it
// means SenderOptions.Timeout is effectively only a floor.
func TestAnaSender_ApplyTimeout_LongerParentDeadline_NotShortenedByOpts(t *testing.T) {
	s := &Sender{opts: SenderOptions{Timeout: 100 * time.Millisecond}, metrics: &noopTestExporter{}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, gcancel := s.applyTimeout(ctx)
	defer gcancel()

	d, _ := got.Deadline()
	remaining := time.Until(d)
	if remaining < 4*time.Second {
		t.Fatalf("remaining = %v; expected ~5s (parent), opts.Timeout was NOT applied (by design)", remaining)
	}
}

// TestAnaSender_ApplyTimeout_NegativeTimeout_FallsBackToDefault verifies
// negative SenderOptions.Timeout is treated as zero and the 60s safety
// net is applied (when ctx has no deadline).
func TestAnaSender_ApplyTimeout_NegativeTimeout_FallsBackToDefault(t *testing.T) {
	s := &Sender{opts: SenderOptions{Timeout: -1 * time.Second}, metrics: &noopTestExporter{}}

	got, gcancel := s.applyTimeout(context.Background())
	defer gcancel()

	d, ok := got.Deadline()
	if !ok {
		t.Fatal("expected fallback deadline for negative Timeout")
	}
	remaining := time.Until(d)
	if remaining < 55*time.Second || remaining > 65*time.Second {
		t.Fatalf("remaining = %v, want ~60s fallback", remaining)
	}
}

// TestAnaCBSender_NewCircuitBreakerSender_NilSession_NoPanic verifies
// that NewCircuitBreakerSender does not panic when the inner Sender's
// session is nil; it falls back to the generic key.
func TestAnaCBSender_NewCircuitBreakerSender_NilSession_NoPanic(t *testing.T) {
	defer func() {
		if rv := recover(); rv != nil {
			t.Fatalf("constructor panicked: %v", rv)
		}
	}()
	cbs := NewCircuitBreakerSender(&Sender{metrics: &noopTestExporter{}}, CBConfig{})
	if cbs == nil {
		t.Fatal("constructor returned nil")
	}
	if cbs.breaker == nil {
		t.Fatal("breaker should be initialised")
	}
}

// TestAnaCBSender_NonRecoverableError_DoesNotTripCircuit verifies that
// errors classified as NON-recoverable by IsRecoverableError do NOT
// count toward the failure threshold and the breaker stays closed.
func TestAnaCBSender_NonRecoverableError_DoesNotTripCircuit(t *testing.T) {
	rec := &ports.RecordingExporter{}

	// Inner sender that always returns a non-recoverable error.
	// ErrInvalidTopic is non-recoverable.
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-cb-nonrec",
	}, domain.SessionEphemeral, nil, rec)
	sess.mu.Lock()
	sess.cm = fakeCM
	sess.mu.Unlock()

	inner := NewSender(sess, SenderOptions{Timeout: time.Second})

	cbs := NewCircuitBreakerSender(inner, CBConfig{
		FailureThreshold: 1, // very low: any countable failure trips
		SuccessThreshold: 1,
		ResetTimeout:     10 * time.Minute,
	})

	// First call: invalid topic → non-recoverable → not counted.
	_ = cbs.Send(context.Background(), &domain.Envelope{Payload: []byte("p")})

	// Inject a non-recoverable error directly to verify CountError result.
	if domain.IsRecoverableError(domain.ErrInvalidTopic) {
		t.Fatal("test invariant: ErrInvalidTopic must be non-recoverable")
	}

	// Try again — circuit must still allow it (closed).
	if err := cbs.breaker.BeforeRequest(); err != nil {
		t.Fatalf("breaker should remain closed after non-recoverable failure, got: %v", err)
	}
}

// TestAnaCBSender_RecoversAfterResetTimeout drives the breaker through
// open → half-open → closed using a very short ResetTimeout, exercising
// the state machine.
func TestAnaCBSender_RecoversAfterResetTimeout(t *testing.T) {
	cfg := circuitbreaker.Config{
		FailureThreshold: 1,
		SuccessThreshold: 1,
		ResetTimeout:     50 * time.Millisecond,
		CountError:       func(error) bool { return true },
	}
	br := circuitbreaker.NewBreaker("ana-cb-recover", cfg, nil)

	// Trip the breaker.
	_ = br.BeforeRequest()
	br.AfterRequest(domain.ErrUnavailable)

	// Immediate request rejected.
	if err := br.BeforeRequest(); err == nil {
		t.Fatal("breaker must reject right after trip")
	}

	// OTHER: wait past the reset timeout, then probe → should be allowed.
	time.Sleep(80 * time.Millisecond)

	if err := br.BeforeRequest(); err != nil {
		t.Fatalf("breaker should allow probe after reset timeout, got %v", err)
	}
	br.AfterRequest(nil) // success closes the breaker

	if err := br.BeforeRequest(); err != nil {
		t.Fatalf("breaker should be closed after probe success, got %v", err)
	}
	br.AfterRequest(nil)
}

// TestAnaCBSender_ErrUnavailableWithRetryAfter verifies that the error
// returned by an open breaker is a *domain.BridgeError with a
// RetryAfter hint, so callers (route runners, DLQ routers) can throttle.
func TestAnaCBSender_ErrUnavailableWithRetryAfter(t *testing.T) {
	cfg := circuitbreaker.Config{
		FailureThreshold: 1,
		SuccessThreshold: 1,
		ResetTimeout:     1 * time.Second,
		CountError:       func(error) bool { return true },
	}
	br := circuitbreaker.NewBreaker("ana-cb-retry", cfg, nil)

	_ = br.BeforeRequest()
	br.AfterRequest(domain.ErrUnavailable)

	err := br.BeforeRequest()
	if err == nil {
		t.Fatal("breaker should reject after trip")
	}
	be, ok := err.(*domain.BridgeError)
	if !ok {
		t.Fatalf("err type = %T, want *domain.BridgeError", err)
	}
	if be.Code != domain.ErrUnavailable.Code {
		t.Fatalf("err code = %s, want %s", be.Code, domain.ErrUnavailable.Code)
	}
	if be.RetryAfter == 0 {
		t.Fatal("expected RetryAfter > 0 from open breaker")
	}
}

// TestAnaCBSender_CBConfigDefaults_AreSane validates that the zero-value
// CBConfig produces non-zero defaults so that operators do not get
// surprising behaviour from an unset config.
func TestAnaCBSender_CBConfigDefaults_AreSane(t *testing.T) {
	cfg := CBConfig{}.toCircuitBreakerConfig()
	if cfg.FailureThreshold <= 0 {
		t.Errorf("default FailureThreshold = %d, want > 0", cfg.FailureThreshold)
	}
	if cfg.SuccessThreshold <= 0 {
		t.Errorf("default SuccessThreshold = %d, want > 0", cfg.SuccessThreshold)
	}
	if cfg.ResetTimeout <= 0 {
		t.Errorf("default ResetTimeout = %v, want > 0", cfg.ResetTimeout)
	}
	if cfg.CountError == nil {
		t.Error("default CountError must not be nil")
	}
}

// TestAnaSender_ContextDoneBeforeSend_ReturnsClassifiedError validates
// that a Send invoked with an already-cancelled context returns a
// classified BridgeError (not a raw context.Canceled) without IO.
func TestAnaSender_ContextDoneBeforeSend_ReturnsClassifiedError(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-send-ctx-done",
	}, domain.SessionEphemeral, nil)
	sess.mu.Lock()
	sess.cm = fakeCM
	sess.mu.Unlock()

	s := NewSender(sess, SenderOptions{Timeout: time.Second, DefaultTopic: "t"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := s.Send(ctx, &domain.Envelope{Subject: "t", Payload: []byte("p")})
	if err == nil {
		t.Fatal("expected error from Send with cancelled ctx")
	}
	if errors.Is(err, context.Canceled) {
		// Acceptable as long as it's wrapped in a BridgeError.
	}
	if _, ok := err.(*domain.BridgeError); !ok {
		t.Fatalf("err type = %T, want *domain.BridgeError", err)
	}
}
