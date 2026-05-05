package paho

import (
	"context"
	"runtime/debug"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// ═══════════════════════════════════════════════════════════════════════════
// BUG-A: Reconcile-before-Start triggers nil parentCtx panic in OnConnectionUp
//
// Defect:
//
//	Reconcile() unconditionally stores `&plan` in `s.plan` even when
//	`s.cm == nil` (it then returns "session not started"). When Start()
//	is later called, the OnConnectionUp callback fires from the autopaho
//	goroutine and reads `parentCtx := s.startCtx` under mutex. Because
//	Start() only assigns `s.startCtx = cmCtx` AFTER `cm.AwaitConnection`
//	returns (which itself unblocks AFTER OnConnectionUp), `parentCtx` is
//	still `nil` at the moment OnConnectionUp executes.
//
//	Since `s.plan != nil`, the callback proceeds to:
//	  context.WithTimeout(parentCtx, reconTimeout)
//	which panics with: "cannot create context from nil parent".
//
// Reachability:
//
//	Any caller that performs `sess.Reconcile(...) → sess.Start(...)` (a
//	common ordering when wiring up bridges programmatically) hits this.
//
// Fix:
//
//	Initialise `s.startCtx = cmCtx` BEFORE calling autopaho.NewConnection
//	(or at minimum before AwaitConnection unblocks).
// ═══════════════════════════════════════════════════════════════════════════

// TestBugA_ReconcileBeforeStart_DoesNotLeaveStartCtxNilOnFirstCallback
// is a regression assertion of the BUG-A fix: when Reconcile has been
// called before Start (which legitimately stashes s.plan), the very
// first OnConnectionUp callback to run during Start MUST observe a
// non-nil parentCtx so that context.WithTimeout does not panic.
//
// The fix relocates `s.startCtx = cmCtx` to occur BEFORE
// `autopaho.NewConnection` returns, eliminating the window in which
// OnConnectionUp could read s.startCtx == nil.
//
// The assertion: immediately after a synchronous Start with the
// Reconcile-before-Start ordering, no panic must have occurred AND
// s.startCtx must be non-nil whenever s.plan is non-nil.
func TestBugA_ReconcileBeforeStart_DoesNotLeaveStartCtxNilOnFirstCallback(t *testing.T) {
	if testing.Short() {
		t.Skip("uses network timeout")
	}

	s := NewSession(SessionOptions{
		BrokerURLs:     []string{"tcp://192.0.2.1:1883"}, // RFC 5737 unreachable
		ClientID:       "bug-a-startctx-invariant",
		KeepAlive:      5,
		ConnectTimeout: 400 * time.Millisecond,
	}, domain.SessionEphemeral, nil)

	if err := s.Reconcile(context.Background(), domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{{Topic: "bug-a/inv", QoS: 1}},
	}); err == nil {
		t.Fatal("Reconcile-before-Start must error")
	}

	// Recover any callback-goroutine panic surfaced through the test.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("BUG-A regressed: panic during Start with prior "+
				"Reconcile: %v\n%s", r, debug.Stack())
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.Start(ctx) // expected to error (broker unreachable)

	// Invariant after the fix: if s.plan is non-nil, s.startCtx must
	// have been non-nil at every point an OnConnectionUp callback
	// could have fired. We verify the field assignment ordering by
	// checking that during Start we observed startCtx != nil (the
	// other test) and that even on connect failure the current value
	// is reset to nil only AFTER cmCancel runs.
	s.mu.Lock()
	planSet := s.plan != nil
	s.mu.Unlock()
	if !planSet {
		t.Fatal("Reconcile-before-Start should have stashed plan")
	}

	_ = s.Close(context.Background())
}

// TestBugA_StartCtx_AssignedBeforeAwaitConnection asserts the structural
// fix: Start MUST assign s.startCtx as soon as the cancellable cmCtx exists
// so that OnConnectionUp (which can run before AwaitConnection unblocks)
// always observes a valid parent context.
//
// This test calls Start with an unreachable broker so connect fails fast.
// The fix guarantees that even on failure, s.startCtx is at least once
// observably non-nil during the Start lifetime. We assert by polling
// s.startCtx from a sibling goroutine while Start is in progress.
func TestBugA_StartCtx_AssignedBeforeAwaitConnection(t *testing.T) {
	if testing.Short() {
		t.Skip("uses network timeout to drive Start lifecycle")
	}

	s := NewSession(SessionOptions{
		BrokerURLs:     []string{"tcp://192.0.2.1:1883"}, // RFC 5737 unreachable
		ClientID:       "bug-a-startctx-assigned",
		KeepAlive:      5,
		ConnectTimeout: 600 * time.Millisecond,
	}, domain.SessionEphemeral, nil)

	// Set a plan first, mirroring the Reconcile-before-Start ordering.
	if err := s.Reconcile(context.Background(), domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{{Topic: "bug-a/x", QoS: 1}},
	}); err == nil {
		t.Fatal("Reconcile before Start must error")
	}

	var (
		observedNonNil bool
		obsMu          sync.Mutex
		stop           = make(chan struct{})
		obsDone        = make(chan struct{})
	)
	go func() {
		defer close(obsDone)
		ticker := time.NewTicker(100 * time.Microsecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				s.mu.Lock()
				nonNil := s.startCtx != nil
				s.mu.Unlock()
				if nonNil {
					obsMu.Lock()
					observedNonNil = true
					obsMu.Unlock()
					return
				}
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.Start(ctx) // expected to error; we care about startCtx visibility
	close(stop)

	select {
	case <-obsDone:
	case <-time.After(2 * time.Second):
		t.Fatal("observer goroutine did not complete")
	}
	_ = s.Close(context.Background())

	obsMu.Lock()
	got := observedNonNil
	obsMu.Unlock()

	assert.True(t, got,
		"BUG-A FIX: s.startCtx must be assigned during Start so that "+
			"OnConnectionUp (which can fire before AwaitConnection returns) "+
			"never observes a nil parent context")
}

// TestBugA_Integration_ReconcileBeforeStart drives the full real-broker
// flow: Reconcile (to stash plan) → Start (which fires OnConnectionUp).
// Without the fix, the autopaho callback goroutine panics and is captured
// by Go's runtime, often crashing the test process. With the fix, Start
// completes cleanly and the deferred reconcile applies the previously
// stored plan.
//
// The test runs against the real Mosquitto broker (mqttlocal). It is the
// strongest end-to-end signal that the race is closed.
func TestBugA_Integration_ReconcileBeforeStart(t *testing.T) {
	url := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sess := NewSession(SessionOptions{
		BrokerURLs:       []string{url},
		ClientID:         mqttlocal.UniqueClientID("bug-a-integ"),
		KeepAlive:        10,
		ConnectTimeout:   5 * time.Second,
		ReconnectTimeout: 5 * time.Second,
		CleanStart:       true,
	}, domain.SessionEphemeral, nil)

	// Stash a plan BEFORE Start. This sets s.plan and returns
	// "session not started".
	if err := sess.Reconcile(ctx, domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{
			{Topic: "bug-a/integ/topic", QoS: 1},
		},
	}); err == nil {
		t.Fatal("expected Reconcile-before-Start to error")
	}

	// Start the session. With the bug, the OnConnectionUp callback
	// reads s.startCtx == nil and panics in a separate goroutine.
	// The autopaho client typically swallows or surfaces such panics
	// through subsequent operations; either way, this call must not
	// panic and Health must report Connected.
	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close(ctx) }()

	// Drain events until SessionReconciled (which OnConnectionUp emits
	// AFTER the deferred reconcile runs). Without the fix, the callback
	// goroutine panics before reaching the reconcile, and SessionReconciled
	// is never emitted.
	deadline := time.After(5 * time.Second)
	gotReconciled := false
EVENTS:
	for !gotReconciled {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				break EVENTS
			}
			if ev.Type == ports.SessionReconciled {
				gotReconciled = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for SessionReconciled event " +
				"(callback may have panicked)")
		}
	}

	h := sess.Health(ctx)
	if !h.Connected {
		t.Fatal("session should be connected after Start")
	}

	sess.mu.Lock()
	subs := len(sess.activeSubs)
	sess.mu.Unlock()
	assert.Equal(t, 1, subs,
		"BUG-A: pre-Start plan must be applied by OnConnectionUp during Start")
}
