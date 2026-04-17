package paho

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// BUG-RDR: Receiver concurrent Run on same instance steals/loses handlers
//
// Defect:
//
//	Receiver.Run uses r.id as the handler key when calling
//	router.Register. If Run is invoked twice on the same Receiver
//	concurrently:
//	  1. Run #1 registers handler H1 under key r.id.
//	  2. Run #2 registers handler H2 under key r.id (silently overwrites
//	     H1 in the router map — see TestAnaRouter_DuplicateRegister).
//	  3. Now only H2 receives messages.
//	  4. When Run #1's ctx fires (or its goroutine returns first), its
//	     deferred Unregister(r.id) removes H2 — the wrong handler.
//	  5. Run #2 now appears alive but has no handler in the router and
//	     will never see another message until ctx fires.
//
//	The same hazard exists if two Receivers are constructed with the
//	same id: NewReceiver(id, sess) followed by NewReceiver(id, sess).
//
// Fix:
//
//	Receiver must reject concurrent Run on the same instance with a
//	typed ErrUnavailable so the caller's misuse is loudly surfaced
//	rather than silently corrupting the router state.
// ═══════════════════════════════════════════════════════════════════════════

// TestBugRDR_ConcurrentRunOnSameReceiver_ReturnsError verifies the fix:
// two simultaneous Run calls on the same Receiver must result in
// exactly one of them succeeding (or both eventually returning) WITHOUT
// silent handler theft.
func TestBugRDR_ConcurrentRunOnSameReceiver_ReturnsError(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "bug-rdr-1",
	}, domain.SessionEphemeral, nil)

	r := NewReceiver("rx-rdr", sess)

	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel1()
	defer cancel2()

	res1 := make(chan error, 1)
	res2 := make(chan error, 1)

	go func() {
		res1 <- r.Run(ctx1, func(_ context.Context, _ ports.Delivery) error { return nil })
	}()
	// Give #1 a head start so its Register is visible.
	time.Sleep(20 * time.Millisecond)
	go func() {
		res2 <- r.Run(ctx2, func(_ context.Context, _ ports.Delivery) error { return nil })
	}()

	// At least one of the calls must reject with an error WITHOUT both
	// succeeding silently. The fix returns ErrUnavailable for the
	// second concurrent caller.
	var (
		earlyErr error
		got      bool
	)
	select {
	case earlyErr = <-res2:
		got = true
	case <-time.After(500 * time.Millisecond):
	}

	if !got {
		t.Fatal("BUG-RDR: second concurrent Run on same Receiver must return promptly with an error")
	}
	if earlyErr == nil {
		t.Fatal("BUG-RDR: second concurrent Run must return an error, not nil")
	}
	be, ok := earlyErr.(*domain.BridgeError)
	if !ok {
		t.Fatalf("BUG-RDR: err type = %T, want *domain.BridgeError", earlyErr)
	}
	if be.Code != domain.ErrUnavailable.Code {
		t.Errorf("BUG-RDR: err code = %s, want %s", be.Code, domain.ErrUnavailable.Code)
	}

	// Now stop the first Run cleanly.
	cancel1()
	select {
	case <-res1:
	case <-time.After(2 * time.Second):
		t.Fatal("first Run did not return after cancel")
	}
}

// TestBugRDR_HandlerRemainsForFirstRun_DespiteSecondRunRejection
// verifies the consequence of the fix: the FIRST Run keeps its handler
// in the router and continues to receive messages even after the second
// Run is rejected.
func TestBugRDR_HandlerRemainsForFirstRun_DespiteSecondRunRejection(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "bug-rdr-2",
	}, domain.SessionEphemeral, nil)

	r := NewReceiver("rx-rdr-2", sess)

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()

	var got1 atomic.Int32
	go func() {
		_ = r.Run(ctx1, func(_ context.Context, _ ports.Delivery) error {
			got1.Add(1)
			return nil
		})
	}()
	// Wait for #1 to register.
	time.Sleep(50 * time.Millisecond)

	// Second concurrent Run — must error promptly (not block until ctx
	// fires). We guard with a short timeout so the test cannot hang
	// when the bug is present; the fix returns ErrUnavailable
	// immediately.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel2()
	res2 := make(chan error, 1)
	go func() {
		res2 <- r.Run(ctx2, func(_ context.Context, _ ports.Delivery) error { return nil })
	}()
	select {
	case err := <-res2:
		if err == nil {
			t.Fatal("BUG-RDR: second concurrent Run must return error")
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("BUG-RDR: second concurrent Run did not return promptly with an error " +
			"(it blocked until parent ctx fires) — the fix must reject before any IO")
	}

	// First Run must still have its handler registered and must receive
	// dispatched messages.
	if sess.Router().HandlerCount() != 1 {
		t.Fatalf("BUG-RDR: router must have exactly 1 handler after second Run rejection, got %d",
			sess.Router().HandlerCount())
	}

	for i := 0; i < 3; i++ {
		sess.Router().Route(newTestPacketPublish("t/x", []byte("p")))
	}

	deadline := time.After(2 * time.Second)
	for got1.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("BUG-RDR: first Run only received %d/3 messages", got1.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestBugRDR_SequentialRunOnSameReceiver_AllowedAfterFirstReturns
// verifies that the guard does NOT prevent legitimate re-use: after
// the first Run returns, a second Run must succeed.
func TestBugRDR_SequentialRunOnSameReceiver_AllowedAfterFirstReturns(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "bug-rdr-3",
	}, domain.SessionEphemeral, nil)

	r := NewReceiver("rx-rdr-3", sess)

	// First Run with cancellable ctx.
	ctx1, cancel1 := context.WithCancel(context.Background())
	res1 := make(chan error, 1)
	go func() {
		res1 <- r.Run(ctx1, func(_ context.Context, _ ports.Delivery) error { return nil })
	}()
	time.Sleep(50 * time.Millisecond)
	cancel1()
	select {
	case <-res1:
	case <-time.After(2 * time.Second):
		t.Fatal("first Run did not return after cancel")
	}

	// Second Run on same Receiver must succeed (running bit cleared).
	ctx2, cancel2 := context.WithCancel(context.Background())
	res2 := make(chan error, 1)
	go func() {
		res2 <- r.Run(ctx2, func(_ context.Context, _ ports.Delivery) error { return nil })
	}()
	time.Sleep(50 * time.Millisecond)
	if sess.Router().HandlerCount() != 1 {
		t.Fatalf("BUG-RDR: HandlerCount = %d after sequential second Run, want 1", sess.Router().HandlerCount())
	}
	cancel2()
	select {
	case <-res2:
	case <-time.After(2 * time.Second):
		t.Fatal("second Run did not return after cancel")
	}
}
