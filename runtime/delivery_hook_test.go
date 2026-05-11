package runtime_test

// ═══════════════════════════════════════════════════════════════════════
// Delivery Hook Tests — DirectHold
//
// Validates the DeliveryHook lifecycle for DirectHold delivery mode.
// Covers ingress/egress OnAttempt calls, terminal OnSettled events,
// and edge cases (expired, poison, drop, concurrent).
//
// Hook call flow:
//
//   Receiver ──▶ OnAttempt(ingress) ──▶ Processors ──▶ sender.Send
//       │                                                   │
//       │                              OnAttempt(egress) ◀──┘
//       │                                                   │
//       ▼                              OnSettled(egress) ◀──┘
//   Ack/Retry/DLQ
//
// ═══════════════════════════════════════════════════════════════════════

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// ---------------------------------------------------------------------------
// recordingHook — thread-safe hook for assertions
// ---------------------------------------------------------------------------

type recordingHook struct {
	mu       sync.Mutex
	attempts []ports.DeliveryAttempt
	settled  []ports.DeliveryOutcome
}

func (h *recordingHook) OnAttempt(_ context.Context, evt ports.DeliveryAttempt) {
	h.mu.Lock()
	h.attempts = append(h.attempts, evt)
	h.mu.Unlock()
}

func (h *recordingHook) OnSettled(_ context.Context, evt ports.DeliveryOutcome) {
	h.mu.Lock()
	h.settled = append(h.settled, evt)
	h.mu.Unlock()
}

func (h *recordingHook) Attempts() []ports.DeliveryAttempt {
	h.mu.Lock()
	defer h.mu.Unlock()
	cp := make([]ports.DeliveryAttempt, len(h.attempts))
	copy(cp, h.attempts)
	return cp
}

func (h *recordingHook) Settled() []ports.DeliveryOutcome {
	h.mu.Lock()
	defer h.mu.Unlock()
	cp := make([]ports.DeliveryOutcome, len(h.settled))
	copy(cp, h.settled)
	return cp
}

func (h *recordingHook) AttemptCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.attempts)
}

func (h *recordingHook) SettledCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.settled)
}

// funcHook is a simple function-based hook for one-off tests.
type funcHook struct {
	onAttempt func(context.Context, ports.DeliveryAttempt)
	onSettled func(context.Context, ports.DeliveryOutcome)
}

func (h *funcHook) OnAttempt(ctx context.Context, evt ports.DeliveryAttempt) { h.onAttempt(ctx, evt) }
func (h *funcHook) OnSettled(ctx context.Context, evt ports.DeliveryOutcome) { h.onSettled(ctx, evt) }

// ---------------------------------------------------------------------------
// DirectHold hook tests
// ---------------------------------------------------------------------------

// TestDeliveryHook_DirectHold_Success validates OnAttempt fires for ingress
// (direction=ingress) and egress (direction=egress, err=nil), and OnSettled
// fires exactly once with nil error and Terminal=true on successful delivery.
func TestDeliveryHook_DirectHold_Success(t *testing.T) {
	hook := &recordingHook{}
	receiver, sender, _, _, runner := makeRunner(t, func(cfg *runtime.RouteRunnerConfig) {
		cfg.Hook = hook
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(&messaging.Envelope{ID: "msg-1", Payload: []byte("hello")})
	if err := receiver.Emit(ctx, del); err != nil {
		t.Fatalf("emit: %v", err)
	}
	waitFor(t, time.Second, "delivery acked", func() bool {
		return del.IsAcked() && sender.SentCount() == 1
	})
	cancel()

	attempts := hook.Attempts()
	settled := hook.Settled()

	if len(attempts) != 2 {
		t.Fatalf("expected 2 attempts (1 ingress + 1 egress), got %d", len(attempts))
	}
	if attempts[0].Direction != ports.DirectionIngress {
		t.Errorf("first attempt direction = %s, want ingress", attempts[0].Direction)
	}
	if attempts[0].RouteID != "test-route" {
		t.Errorf("ingress RouteID = %q, want test-route", attempts[0].RouteID)
	}
	if attempts[0].Envelope == nil {
		t.Error("ingress attempt Envelope should not be nil")
	}
	if attempts[1].Direction != ports.DirectionEgress {
		t.Errorf("second attempt direction = %s, want egress", attempts[1].Direction)
	}
	if attempts[1].Err != nil {
		t.Errorf("egress attempt Err = %v, want nil", attempts[1].Err)
	}
	if len(settled) != 1 {
		t.Fatalf("expected 1 settled, got %d", len(settled))
	}
	if settled[0].Err != nil {
		t.Errorf("settled Err = %v, want nil", settled[0].Err)
	}
	if !settled[0].Terminal {
		t.Error("settled Terminal should be true")
	}
	if settled[0].Direction != ports.DirectionEgress {
		t.Errorf("settled direction = %s, want egress", settled[0].Direction)
	}
}

// TestDeliveryHook_DirectHold_PermanentFailure_DLQ validates that a
// permanent send error fires OnAttempt with the error, then OnSettled
// with the same error after DLQ routing.
func TestDeliveryHook_DirectHold_PermanentFailure_DLQ(t *testing.T) {
	hook := &recordingHook{}
	permErr := shared.NewBridgeError("PERM", shared.ErrorPermanent, "permanent failure")
	receiver, sender, _, _, runner := makeRunner(t, func(cfg *runtime.RouteRunnerConfig) {
		cfg.Hook = hook
	})
	sender.SendErr = permErr

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(&messaging.Envelope{ID: "msg-perm", Payload: []byte("fail")})
	if err := receiver.Emit(ctx, del); err != nil {
		t.Fatalf("emit: %v", err)
	}
	waitFor(t, time.Second, "delivery acked", func() bool { return del.IsAcked() })
	cancel()

	attempts := hook.Attempts()
	if len(attempts) < 2 {
		t.Fatalf("expected >= 2 attempts, got %d", len(attempts))
	}
	if attempts[1].Err == nil {
		t.Error("egress attempt should carry the send error")
	}

	settled := hook.Settled()
	if len(settled) != 1 {
		t.Fatalf("expected 1 settled, got %d", len(settled))
	}
	if settled[0].Err == nil {
		t.Error("settled should carry error")
	}
	if !settled[0].Terminal {
		t.Error("settled should be terminal")
	}
}

// TestDeliveryHook_DirectHold_NoHook_NoopSafe validates delivery works
// with no hook configured (noop default, no nil panics).
func TestDeliveryHook_DirectHold_NoHook_NoopSafe(t *testing.T) {
	receiver, sender, _, _, runner := makeRunner(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(&messaging.Envelope{ID: "msg-noop", Payload: []byte("safe")})
	if err := receiver.Emit(ctx, del); err != nil {
		t.Fatalf("emit: %v", err)
	}
	waitFor(t, time.Second, "delivery acked", func() bool {
		return del.IsAcked() && sender.SentCount() == 1
	})
}

// TestDeliveryHook_DirectHold_TransientRetry_NoSettled validates that a
// transient send error fires OnAttempt with the error but does NOT fire
// OnSettled because the delivery is retried by the transport.
//
// ───────────────────────────────────────────────
//
//	sender.Send → transient error → del.Retry
//	OnAttempt(egress, err=transient) ✓
//	OnSettled NOT called              ✓
//
// ───────────────────────────────────────────────
func TestDeliveryHook_DirectHold_TransientRetry_NoSettled(t *testing.T) {
	hook := &recordingHook{}
	receiver, sender, _, _, runner := makeRunner(t, func(cfg *runtime.RouteRunnerConfig) {
		cfg.Hook = hook
		cfg.Policy.DeliveryMode = routing.DeliveryDirectHold
	})
	sender.SendErr = shared.ErrUnavailable

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(&messaging.Envelope{ID: "msg-transient", Payload: []byte("retry")})
	if err := receiver.Emit(ctx, del); err != nil {
		t.Fatalf("emit: %v", err)
	}
	waitFor(t, time.Second, "delivery retried", func() bool { return del.IsRetried() })
	cancel()

	egressFound := false
	for _, a := range hook.Attempts() {
		if a.Direction == ports.DirectionEgress {
			egressFound = true
			if a.Err == nil {
				t.Error("egress attempt should carry transient error")
			}
		}
	}
	if !egressFound {
		t.Error("expected at least one egress OnAttempt")
	}
	if hook.SettledCount() != 0 {
		t.Errorf("expected 0 settled for retried delivery, got %d", hook.SettledCount())
	}
}

// TestDeliveryHook_DirectHold_AttemptCarriesReceiveCount validates that
// the Attempt field on egress OnAttempt reflects receiveCount+1 from
// sqs.ApproximateReceiveCount header.
func TestDeliveryHook_DirectHold_AttemptCarriesReceiveCount(t *testing.T) {
	hook := &recordingHook{}
	receiver, _, _, _, runner := makeRunner(t, func(cfg *runtime.RouteRunnerConfig) {
		cfg.Hook = hook
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{
		ID:      "msg-rc3",
		Payload: []byte("attempt"),
		Headers: map[string]any{"sqs.ApproximateReceiveCount": 3},
	}))
	if err := receiver.Emit(ctx, del); err != nil {
		t.Fatalf("emit: %v", err)
	}
	waitFor(t, time.Second, "delivery acked", func() bool { return del.IsAcked() })
	cancel()

	for _, a := range hook.Attempts() {
		if a.Direction == ports.DirectionEgress {
			if a.Attempt != 4 {
				t.Errorf("egress Attempt = %d, want 4 (receiveCount+1)", a.Attempt)
			}
			return
		}
	}
	t.Error("no egress OnAttempt found")
}

// TestDeliveryHook_DirectHold_MaxAttemptFromPolicy validates that
// MaxAttempts on both OnAttempt and OnSettled matches the route
// policy's MaxReplayAttempts.
func TestDeliveryHook_DirectHold_MaxAttemptFromPolicy(t *testing.T) {
	hook := &recordingHook{}
	receiver, _, _, _, runner := makeRunner(t, func(cfg *runtime.RouteRunnerConfig) {
		cfg.Hook = hook
		cfg.Policy.MaxReplayAttempts = 7
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(&messaging.Envelope{ID: "msg-max", Payload: []byte("max")})
	if err := receiver.Emit(ctx, del); err != nil {
		t.Fatalf("emit: %v", err)
	}
	waitFor(t, time.Second, "delivery acked", func() bool { return del.IsAcked() })
	cancel()

	for _, a := range hook.Attempts() {
		if a.MaxAttempts != 7 {
			t.Errorf("OnAttempt MaxAttempts = %d, want 7", a.MaxAttempts)
		}
	}
	for _, s := range hook.Settled() {
		if s.MaxAttempts != 7 {
			t.Errorf("OnSettled MaxAttempts = %d, want 7", s.MaxAttempts)
		}
	}
}

// TestDeliveryHook_DirectHold_Drop_NoDLQ_RetryUnsupported validates that
// OnSettled fires with the failure reason when the message is dropped
// because the source does not support retry and no DLQ is configured.
//
// ───────────────────────────────────────────────
//
//	sender.Send → transient → del.Retry → ErrNotSupported
//	DLQ store = nil → message dropped
//	OnSettled(err=reason) ✓
//
// ───────────────────────────────────────────────
func TestDeliveryHook_DirectHold_Drop_NoDLQ_RetryUnsupported(t *testing.T) {
	hook := &recordingHook{}
	sender := NewFakeSender()
	sender.SendErr = shared.ErrUnavailable
	receiver := NewFakeReceiver()

	runner := runtime.NewRouteRunnerFromConfig(runtime.RouteRunnerConfig{
		RouteID:    "test-route",
		Policy:     routing.RoutePolicy{DeliveryMode: routing.DeliveryDirectHold}.WithDefaults(),
		Receiver:   receiver,
		Sender:     sender,
		DLQ:        runtime.NewDLQRouter(nil),
		InstanceID: "bridge-1",
		Hook:       hook,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(&messaging.Envelope{ID: "msg-drop", Payload: []byte("drop")})
	del.RetryFnErr = shared.ErrNotSupported
	if err := receiver.Emit(ctx, del); err != nil {
		t.Fatalf("emit: %v", err)
	}
	waitFor(t, time.Second, "delivery acked", func() bool { return del.IsAcked() })
	cancel()

	settled := hook.Settled()
	if len(settled) != 1 {
		t.Fatalf("expected 1 settled (dropped), got %d", len(settled))
	}
	if settled[0].Err == nil {
		t.Error("settled should carry the failure reason")
	}
	if !settled[0].Terminal {
		t.Error("settled should be terminal")
	}
}

// TestDeliveryHook_DirectHold_ExpiredMessage_NoEgressHook validates that
// an expired message fires the ingress OnAttempt but does NOT fire any
// egress OnAttempt or OnSettled.
func TestDeliveryHook_DirectHold_ExpiredMessage_NoEgressHook(t *testing.T) {
	hook := &recordingHook{}
	receiver, _, _, _, runner := makeRunner(t, func(cfg *runtime.RouteRunnerConfig) {
		cfg.Hook = hook
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(&messaging.Envelope{
		ID:        "msg-expired",
		Payload:   []byte("old"),
		ExpiresAt: time.Now().Add(-1 * time.Hour),
	})
	if err := receiver.Emit(ctx, del); err != nil {
		t.Fatalf("emit: %v", err)
	}
	waitFor(t, time.Second, "delivery acked", func() bool { return del.IsAcked() })
	cancel()

	ingressCount, egressCount := 0, 0
	for _, a := range hook.Attempts() {
		if a.Direction == ports.DirectionIngress {
			ingressCount++
		} else {
			egressCount++
		}
	}
	if ingressCount != 1 {
		t.Errorf("expected 1 ingress OnAttempt, got %d", ingressCount)
	}
	if egressCount != 0 {
		t.Errorf("expected 0 egress OnAttempt for expired, got %d", egressCount)
	}
	if hook.SettledCount() != 0 {
		t.Errorf("expected 0 OnSettled for expired, got %d", hook.SettledCount())
	}
}

// TestDeliveryHook_DirectHold_SettledCarriesBindingID validates that
// OnSettled BindingID matches the dispatch plan's binding.
func TestDeliveryHook_DirectHold_SettledCarriesBindingID(t *testing.T) {
	hook := &recordingHook{}
	receiver, _, _, _, runner := makeRunner(t, func(cfg *runtime.RouteRunnerConfig) {
		cfg.Hook = hook
		cfg.Bindings = []routing.DestinationBinding{
			{ID: "bind-alpha", Address: "topic/alpha"},
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()

	del := NewFakeDelivery(&messaging.Envelope{ID: "msg-bind", Payload: []byte("bind")})
	if err := receiver.Emit(ctx, del); err != nil {
		t.Fatalf("emit: %v", err)
	}
	waitFor(t, time.Second, "delivery acked", func() bool { return del.IsAcked() })
	cancel()

	settled := hook.Settled()
	if len(settled) != 1 {
		t.Fatalf("expected 1 settled, got %d", len(settled))
	}
	if settled[0].BindingID != "bind-alpha" {
		t.Errorf("settled BindingID = %q, want bind-alpha", settled[0].BindingID)
	}
}

// TestDeliveryHook_DirectHold_ConcurrentDeliveries validates that
// concurrent deliveries each get their own independent hook calls.
func TestDeliveryHook_DirectHold_ConcurrentDeliveries(t *testing.T) {
	hook := &recordingHook{}
	receiver, sender, _, _, runner := makeRunner(t, func(cfg *runtime.RouteRunnerConfig) {
		cfg.Hook = hook
		cfg.Policy.MaxInFlight = 10
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()

	const n = 10
	for i := range n {
		del := NewFakeDelivery(&messaging.Envelope{
			ID:      "concurrent-" + string(rune('0'+i)),
			Payload: []byte("data"),
		})
		if err := receiver.Emit(ctx, del); err != nil {
			t.Fatalf("emit %d: %v", i, err)
		}
	}

	waitFor(t, 2*time.Second, "all sent", func() bool { return sender.SentCount() == n })
	cancel()

	if hook.AttemptCount() != 2*n {
		t.Errorf("expected %d attempts, got %d", 2*n, hook.AttemptCount())
	}
	if hook.SettledCount() != n {
		t.Errorf("expected %d settled, got %d", n, hook.SettledCount())
	}
}
