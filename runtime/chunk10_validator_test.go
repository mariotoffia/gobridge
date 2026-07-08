package runtime_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
)

// ══════════════════════════════════════════════════════════════════════════
// Chunk 10 validation regressions (F4, F8)
// ══════════════════════════════════════════════════════════════════════════

// TestValidator_PipelineTimeExceedsVisibility_Rejected is the F4 regression:
// per-processor budgets are own-time-only and disarm during next(), so N
// compliant processors legally consume N×ProcessorTimeout before the send. On a
// FIXED-visibility source (auto-extend off) the validator must reject a route
// whose worst-case pipeline time (N×ProcessorTimeout + SendTimeout + DLQ budget)
// exceeds the window, even though every individual timeout passes its own check
// (SendTimeout here is well under VisibilityTimeout/2).
func TestValidator_PipelineTimeExceedsVisibility_Rejected(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-bridge"))
	cfg, rx, tx, sess, sessCfg := validDirectHoldEntry()
	cfg.SourceVisibilityTimeout = 30 * time.Second
	cfg.SourceAutoExtend = false
	cfg.Policy.ProcessorTimeout = 10 * time.Second
	cfg.Policy.SendTimeout = 2 * time.Second // < vis/2 (15s): the old check stays green
	cfg.Processors = []ports.Processor{&timeoutProcessor{}, &timeoutProcessor{}, &timeoutProcessor{}}

	if err := rt.AddRoute(cfg, rx, tx, sess, sessCfg); err != nil {
		t.Fatal(err)
	}

	err := rt.Start(context.Background())
	if err == nil {
		t.Fatal("expected validation error: 3×10s processors + 2s send + DLQ budget exceeds the 30s window")
	}
	if !strings.Contains(err.Error(), "worst-case pipeline time") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestValidator_PipelineTimeAutoExtend_NotRejected proves the F4 bound applies
// only to fixed-window sources: an auto-extend source renews the window in the
// background, so the same processor/send budget must NOT be rejected for
// pipeline time.
func TestValidator_PipelineTimeAutoExtend_NotRejected(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-bridge"))
	cfg, rx, tx, sess, sessCfg := validDirectHoldEntry()
	cfg.SourceVisibilityTimeout = 30 * time.Second
	cfg.SourceAutoExtend = true // renews the window → the fixed bound does not apply
	cfg.Policy.ProcessorTimeout = 10 * time.Second
	cfg.Policy.SendTimeout = 2 * time.Second
	cfg.Processors = []ports.Processor{&timeoutProcessor{}, &timeoutProcessor{}, &timeoutProcessor{}}

	if err := rt.AddRoute(cfg, rx, tx, sess, sessCfg); err != nil {
		t.Fatal(err)
	}

	// Pre-cancel so Start returns straight after validation without launching the
	// route background loops.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rt.Start(ctx); err != nil && strings.Contains(err.Error(), "worst-case pipeline time") {
		t.Fatalf("auto-extend source must not be rejected for pipeline time: %v", err)
	}
}

// TestValidator_NegativeBackoffMaxInterval_Rejected is the F8 startup regression:
// WithDefaults fills only ZERO Backoff fields, so a negative MaxInterval survives
// to the runner where route.retryDelay's `> 0` cap guard never fires and
// float64 growth overflows time.Duration. Start-time validation must reject it.
func TestValidator_NegativeBackoffMaxInterval_Rejected(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-bridge"))
	cfg, rx, tx, sess, sessCfg := validDirectHoldEntry()
	cfg.Policy.Backoff.MaxInterval = -1 * time.Second

	if err := rt.AddRoute(cfg, rx, tx, sess, sessCfg); err != nil {
		t.Fatal(err)
	}

	err := rt.Start(context.Background())
	if err == nil {
		t.Fatal("expected validation error for a negative Backoff.MaxInterval")
	}
	if !strings.Contains(err.Error(), "Backoff.MaxInterval") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestValidator_PipelineTime_DLQBudgetExcludedForDropPolicy proves the F4 DLQ
// budget is conditional: a drop-policy route (like any route with no DLQ store)
// never performs the inline DLQ write, so its worst-case source hold must exclude
// the budget. With N×ProcessorTimeout + SendTimeout UNDER the window but +DLQ
// budget OVER it, an unconditional budget would wrongly reject this safe config at
// startup — a real upgrade-breakage for a previously-booting deployment. It must
// pass pipeline-time validation.
func TestValidator_PipelineTime_DLQBudgetExcludedForDropPolicy(t *testing.T) {
	rt := runtime.New(runtime.WithInstanceID("test-bridge"))
	cfg, rx, tx, sess, sessCfg := validDirectHoldEntry() // OnPermanentFailure=drop, no DLQ store
	cfg.SourceVisibilityTimeout = 25 * time.Second
	cfg.SourceAutoExtend = false
	cfg.Policy.ProcessorTimeout = 8 * time.Second
	cfg.Policy.SendTimeout = 2 * time.Second // < vis/2 (12.5s): the send-window check stays green
	cfg.Processors = []ports.Processor{&timeoutProcessor{}, &timeoutProcessor{}}
	// 2×8s + 2s = 18s < 25s window (passes); + 10.5s DLQ budget = 28.5s > 25s
	// (would fail if the budget were counted). Drop policy → budget excluded.

	if err := rt.AddRoute(cfg, rx, tx, sess, sessCfg); err != nil {
		t.Fatal(err)
	}
	// Pre-cancel so Start returns straight after validation without launching loops.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rt.Start(ctx); err != nil && strings.Contains(err.Error(), "worst-case pipeline time") {
		t.Fatalf("drop-policy route must not be rejected for a phantom DLQ budget: %v", err)
	}
}

// TestValidator_PipelineTime_DLQBudgetCountedWhenDLQConfigured is the other half:
// the exact timing that passes under a drop policy MUST be rejected once a DLQ
// store is configured and the terminal policy is dlq, because the inline failure
// path then really does hold the source through the DLQ write. This guards
// against the F4 conditional silently disabling the budget where it applies.
func TestValidator_PipelineTime_DLQBudgetCountedWhenDLQConfigured(t *testing.T) {
	rt := runtime.New(
		runtime.WithInstanceID("test-bridge"),
		runtime.WithDLQStore(NewFakeDLQStore()),
	)
	cfg, rx, tx, sess, sessCfg := validDirectHoldEntry()
	cfg.Policy.OnPermanentFailure = routing.FailureDLQ // inline DLQ write on permanent failure
	cfg.SourceVisibilityTimeout = 25 * time.Second
	cfg.SourceAutoExtend = false
	cfg.Policy.ProcessorTimeout = 8 * time.Second
	cfg.Policy.SendTimeout = 2 * time.Second
	cfg.Processors = []ports.Processor{&timeoutProcessor{}, &timeoutProcessor{}}
	// 2×8s + 2s = 18s < 25s window, but + 10.5s DLQ budget = 28.5s > 25s: the route
	// holds the source through the DLQ write past the window → rejected.

	if err := rt.AddRoute(cfg, rx, tx, sess, sessCfg); err != nil {
		t.Fatal(err)
	}
	err := rt.Start(context.Background())
	if err == nil {
		t.Fatal("expected rejection: 2×8s + 2s + 10.5s DLQ budget exceeds the 25s window")
	}
	if !strings.Contains(err.Error(), "worst-case pipeline time") {
		t.Fatalf("unexpected error: %v", err)
	}
}
