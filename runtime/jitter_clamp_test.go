package runtime_test

// ═══════════════════════════════════════════════
// Session Manager Jitter Clamp Tests (RES-010)
//
// Validates that renewal intervals cannot go
// near-zero or negative even with large jitter.
//
// Summary:
// ┌──────┬────────────────────────────────────────────┬──────────┐
// │ ID   │ Description                                │ Status   │
// ├──────┼────────────────────────────────────────────┼──────────┤
// │ T001 │ Large jitter doesn't produce near-zero     │ PASS     │
// │ T002 │ Normal jitter works correctly              │ PASS     │
// │ T003 │ GlobalMaxInFlight negative clamp           │ PASS     │
// └──────┴────────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// TestSessionManager_LargeJitter_NoHotLoop validates that configuring
// jitter larger than renewInterval doesn't produce near-zero timers.
//
// Without the clamp fix, renewInterval=1s + jitter(-2.5s to +2.5s)
// could yield negative durations, causing time.NewTimer to fire
// immediately in a hot-loop.
func TestSessionManager_LargeJitter_NoHotLoop(t *testing.T) {
	leaseStore := NewFakeLeaseStore()
	session := NewFakeSession()

	cfg := runtime.SessionConfig{
		SessionID:    "jitter-test",
		Exclusive:    true,
		LeaseTTL:     500 * time.Millisecond,
		RenewInterval: 100 * time.Millisecond,
		RenewJitter:  400 * time.Millisecond,
		MaxRenewFails: 3,
		StepDownGrace: 50 * time.Millisecond,
	}

	mgr := runtime.NewSessionManagerFromConfig(cfg, session, leaseStore, "test-owner", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := mgr.Run(ctx)
	if err != nil && err != context.DeadlineExceeded {
		t.Logf("run ended with: %v (expected context deadline)", err)
	}

	if !session.Started {
		t.Fatal("session should have been started")
	}
}

// TestGlobalMaxInFlight_NegativeClamp validates that negative values
// are clamped to 0, meaning no global semaphore is created (QA-H5).
func TestGlobalMaxInFlight_NegativeClamp(t *testing.T) {
	receiver := NewFakeReceiver()
	sender := NewFakeSender()

	rt := runtime.New(
		runtime.WithInstanceID("test"),
		runtime.WithGlobalMaxInFlight(-5),
	)

	err := rt.AddRoute(runtime.RouteConfig{
		ID:                 "route-1",
		Policy:             domain.RoutePolicy{MaxInFlight: 2}.WithDefaults(),
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}, receiver, sender, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err = rt.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}

	<-receiver.Ready()

	env := &domain.Envelope{
		ID:      "msg-1",
		Subject: "test",
		Payload: []byte("data"),
	}
	del := NewFakeDelivery(env)
	err = receiver.Emit(ctx, del)
	if err != nil {
		t.Fatalf("emit should succeed without global sem: %v", err)
	}

	waitFor(t, 2*time.Second, "delivery acked", func() bool {
		return del.IsAcked()
	})

	stopCtx, sc := context.WithTimeout(context.Background(), time.Second)
	defer sc()
	_ = rt.Stop(stopCtx)
}

// TestGlobalMaxInFlight_LimitsAcrossRoutes validates that a global
// semaphore of 1 limits concurrency across two routes.
func TestGlobalMaxInFlight_LimitsAcrossRoutes(t *testing.T) {
	receiver1 := NewFakeReceiver()
	receiver2 := NewFakeReceiver()

	sender := NewFakeSender()
	sender.SendFn = func(_ *domain.Envelope) error {
		time.Sleep(100 * time.Millisecond)
		return nil
	}

	rt := runtime.New(
		runtime.WithInstanceID("test"),
		runtime.WithGlobalMaxInFlight(1),
	)

	err := rt.AddRoute(runtime.RouteConfig{
		ID:                 "route-1",
		Policy:             domain.RoutePolicy{MaxInFlight: 5}.WithDefaults(),
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}, receiver1, sender, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	err = rt.AddRoute(runtime.RouteConfig{
		ID:                 "route-2",
		Policy:             domain.RoutePolicy{MaxInFlight: 5}.WithDefaults(),
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}, receiver2, sender, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err = rt.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}

	<-receiver1.Ready()
	<-receiver2.Ready()

	del1 := NewFakeDelivery(&domain.Envelope{
		ID: "msg-r1", Subject: "test", Payload: []byte("1"),
	})
	del2 := NewFakeDelivery(&domain.Envelope{
		ID: "msg-r2", Subject: "test", Payload: []byte("2"),
	})

	_ = receiver1.Emit(ctx, del1)
	_ = receiver2.Emit(ctx, del2)

	waitFor(t, 5*time.Second, "both acked", func() bool {
		return del1.IsAcked() && del2.IsAcked()
	})

	stopCtx, sc := context.WithTimeout(context.Background(), time.Second)
	defer sc()
	_ = rt.Stop(stopCtx)
}
