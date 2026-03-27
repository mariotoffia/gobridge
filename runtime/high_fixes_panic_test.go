package runtime_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// ═══════════════════════════════════════════════════════════════════════
// T8: Panic Recovery in startBackground Tests
//
// Validates that the runtime catches panics in background goroutines,
// logs the error, marks the component unhealthy, and does not crash.
//
//   ┌────────────┐  panic!   ┌────────────┐
//   │ Background │──────────▶│  recover() │──▶ unhealthy + cancel
//   │ goroutine  │           │  defer     │
//   └────────────┘           └────────────┘
// ═══════════════════════════════════════════════════════════════════════

// TestRuntime_PanicRecovery_MarksUnhealthy validates that a panicking
// background component does not crash the process, is caught by
// recover(), and marks the runtime unhealthy.
//
// Scenario:
// ───────────────────────────────────────────────
//   Receiver.Run panics → startBackground recovers →
//   componentErrors populated → healthy=false
// ───────────────────────────────────────────────
//
// Assertions:
//   - Runtime does not crash (test completes)
//   - Runtime.Healthy() returns false
//   - ComponentErrors contains the panicking component
//   - Error message contains the panic value
func TestRuntime_PanicRecovery_MarksUnhealthy(t *testing.T) {
	rt := goruntime.New(
		goruntime.WithInstanceID("panic-test"),
	)

	panicReceiver := &PanickingReceiver{}
	sender := NewFakeSender()

	cfg := goruntime.RouteConfig{
		ID:                 "panic-route",
		Policy:             domain.RoutePolicy{}.WithDefaults(),
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}

	if err := rt.AddRoute(cfg, panicReceiver, sender, nil, nil); err != nil {
		t.Fatalf("AddRoute failed: %v", err)
	}

	ctx := context.Background()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	waitFor(t, 2*time.Second, "runtime marked unhealthy after panic", func() bool {
		return !rt.Healthy()
	})

	if rt.Healthy() {
		t.Fatal("runtime should be unhealthy after background panic")
	}

	errs := rt.ComponentErrors()
	if len(errs) == 0 {
		t.Fatal("expected component errors after panic")
	}

	found := false
	for name, err := range errs {
		if err != nil {
			errMsg := err.Error()
			if !strings.Contains(errMsg, "test panic in receiver") {
				t.Fatalf("expected panic message to contain %q, got %q", "test panic in receiver", errMsg)
			}
			if !strings.Contains(errMsg, "panic in") {
				t.Fatalf("expected error to indicate panic origin, got %q", errMsg)
			}
			t.Logf("component %q error: %v", name, err)
			found = true
		}
	}
	if !found {
		t.Fatal("expected at least one component error")
	}

	stopCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_ = rt.Stop(stopCtx)
}

// TestRuntime_PanicRecovery_DoesNotCrash validates that the test
// process itself does not crash when a background goroutine panics.
func TestRuntime_PanicRecovery_DoesNotCrash(t *testing.T) {
	rt := goruntime.New(
		goruntime.WithInstanceID("no-crash"),
	)

	panicReceiver := &PanickingReceiver{PanicMsg: "deliberate test panic"}
	cfg := goruntime.RouteConfig{
		ID:                 "no-crash-route",
		Policy:             domain.RoutePolicy{}.WithDefaults(),
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}

	_ = rt.AddRoute(cfg, panicReceiver, NewFakeSender(), nil, nil)
	_ = rt.Start(context.Background())

	waitFor(t, 2*time.Second, "runtime detects panic", func() bool {
		return !rt.Healthy()
	})

	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = rt.Stop(stopCtx)
}

// ---------------------------------------------------------------------------
// PanickingReceiver
// ---------------------------------------------------------------------------

// PanickingReceiver implements ports.Receiver and panics immediately on Run.
type PanickingReceiver struct {
	PanicMsg string
}

func (r *PanickingReceiver) Run(_ context.Context, _ func(context.Context, ports.Delivery) error) error {
	msg := r.PanicMsg
	if msg == "" {
		msg = "test panic in receiver"
	}
	panic(msg)
}
