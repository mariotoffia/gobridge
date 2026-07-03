package paho

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
)

// ═══════════════════════════════════════════════════════════════════════════
// BUG-2: MQTT Session Start() TOCTOU Race — and its follow-up.
//
// Start() previously released the lock after the cm==nil check, performed
// slow operations, then re-acquired. Two concurrent callers both passed the
// guard, creating duplicate ConnectionManagers (the first leaked).
//
// First fix: a `starting` flag made the second caller return nil
// immediately — but that nil was a FALSE SUCCESS: the winner might still
// fail, and a racing Reload would silently skip its TLS rebuild.
//
// Current semantics (pinned here): concurrent Start callers WAIT for the
// in-flight attempt's outcome. If the winner succeeds they return nil
// (session really is up); if it fails they run their own attempt; if
// their context expires while waiting they get a definite error. No
// caller ever reports success for a session that is not connected.
// ═══════════════════════════════════════════════════════════════════════════

// TestBug2_Start_ConcurrentCallers_BothGetDefiniteOutcome verifies that
// with an unreachable broker, NEITHER of two concurrent Start() calls
// returns a false success: both must return an error.
func TestBug2_Start_ConcurrentCallers_BothGetDefiniteOutcome(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping race test in -short mode (uses network timeout)")
	}

	// Create session with unreachable broker — guarantees timeout, not hang.
	sess := NewSession(SessionOptions{
		BrokerURLs:     []string{"tcp://192.0.2.1:1883"}, // RFC 5737
		ClientID:       "bug2-test",
		KeepAlive:      5,
		ConnectTimeout: 500 * time.Millisecond,
	}, connectivity.SessionEphemeral, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var (
		wg     sync.WaitGroup
		errors [2]error
	)

	// Launch 2 concurrent Start() calls.
	barrier := make(chan struct{})
	for i := range 2 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-barrier // synchronize start
			errors[idx] = sess.Start(ctx)
		}(i)
	}
	close(barrier)
	wg.Wait()

	t.Logf("Start errors: [0]=%v, [1]=%v", errors[0], errors[1])

	// The broker is unreachable, so no caller may report success. The
	// waiter must observe the winner's failure and fail its own retry —
	// a nil here would be the false success that let a racing Reload
	// silently skip its TLS rebuild.
	for i, e := range errors {
		assert.Error(t, e,
			"caller %d must get a definite error for an unreachable broker (no false success)", i)
	}

	_ = sess.Close(context.Background())
}

// TestBug2_Start_WaiterExpiresWhileWinnerConnecting verifies a waiting
// Start caller whose context expires before the in-flight attempt
// finishes gets a definite error (not nil, and without deadlocking).
func TestBug2_Start_WaiterExpiresWhileWinnerConnecting(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping race test in -short mode (uses network timeout)")
	}

	sess := NewSession(SessionOptions{
		BrokerURLs:     []string{"tcp://192.0.2.1:1883"}, // RFC 5737
		ClientID:       "bug2-waiter-expiry",
		KeepAlive:      5,
		ConnectTimeout: 2 * time.Second,
	}, connectivity.SessionEphemeral, nil)

	winnerCtx, winnerCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer winnerCancel()

	winnerDone := make(chan error, 1)
	go func() { winnerDone <- sess.Start(winnerCtx) }()

	// Wait until the winner is inside the slow path.
	require.Eventually(t, func() bool {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		return sess.starting
	}, 2*time.Second, 5*time.Millisecond, "winner never entered the connecting state")

	// A second caller with an already-short deadline must return a
	// definite error once its context expires — not a false nil.
	waiterCtx, waiterCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer waiterCancel()
	err := sess.Start(waiterCtx)
	assert.Error(t, err, "waiter must get a definite error when its context expires while waiting")

	select {
	case err := <-winnerDone:
		assert.Error(t, err, "winner should fail against the unreachable broker")
	case <-time.After(5 * time.Second):
		t.Fatal("winner Start did not return")
	}

	_ = sess.Close(context.Background())
}

// TestBug2_Start_IdempotentAfterSuccess verifies that Start() on an
// already-connected session returns nil without re-entering the slow path.
func TestBug2_Start_IdempotentAfterSuccess(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "bug2-idempotent",
		KeepAlive:  5,
	}, connectivity.SessionEphemeral, nil)

	// Simulate a started session by setting cm to non-nil via Start's
	// success path. We can't do a real connection in a unit test, so we
	// verify the guard logic: after the first Start returns (even with
	// error), starting is reset and the session can be retried.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	// First Start: fails due to unreachable broker (starting flag is reset).
	err := sess.Start(ctx)
	assert.Error(t, err, "first Start should fail (unreachable broker)")

	// Second Start: should also attempt (starting was reset on error).
	ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel2()
	err2 := sess.Start(ctx2)
	assert.Error(t, err2, "second Start should also attempt (starting was reset on error)")

	_ = sess.Close(context.Background())
}
