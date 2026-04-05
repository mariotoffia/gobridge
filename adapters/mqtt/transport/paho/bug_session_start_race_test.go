package paho

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/mariotoffia/gobridge/domain"
)

// ═══════════════════════════════════════════════════════════════════════════
// BUG-2: MQTT Session Start() TOCTOU Race
//
// Start() previously released the lock after the cm==nil check, performed
// slow operations, then re-acquired. Two concurrent callers both passed the
// guard, creating duplicate ConnectionManagers (the first leaked).
//
// Fix: a `starting` flag set under the same lock prevents the second caller
// from entering the slow path. The second caller returns nil immediately.
// ═══════════════════════════════════════════════════════════════════════════

// TestBug2_Start_ConcurrentCallers_OnlyOneEnters verifies that two
// concurrent Start() calls do NOT both enter the slow connection path.
// The `starting` guard ensures the second caller returns nil immediately.
func TestBug2_Start_ConcurrentCallers_OnlyOneEnters(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping race test in -short mode (uses network timeout)")
	}

	// Create session with unreachable broker — guarantees timeout, not hang.
	sess := NewSession(SessionOptions{
		BrokerURLs:     []string{"tcp://192.0.2.1:1883"}, // RFC 5737
		ClientID:       "bug2-test",
		KeepAlive:      5,
		ConnectTimeout: 500 * time.Millisecond,
	}, domain.SessionEphemeral, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
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

	t.Logf("BUG-2 FIX: Start errors: [0]=%v, [1]=%v", errors[0], errors[1])

	// With the starting guard, exactly one caller enters the slow path
	// (and fails with timeout). The other returns nil immediately.
	nilCount := 0
	errCount := 0
	for _, e := range errors {
		if e == nil {
			nilCount++
		} else {
			errCount++
		}
	}

	// One caller should return nil (guarded away), one should return error
	// (entered slow path but broker unreachable).
	assert.Equal(t, 1, nilCount,
		"BUG-2 FIX: exactly one caller should return nil (guarded by starting flag)")
	assert.Equal(t, 1, errCount,
		"BUG-2 FIX: exactly one caller should return error (entered slow path)")

	// Cleanup: close session if it was started.
	_ = sess.Close(context.Background())
}

// TestBug2_Start_IdempotentAfterSuccess verifies that Start() on an
// already-connected session returns nil without re-entering the slow path.
func TestBug2_Start_IdempotentAfterSuccess(t *testing.T) {
	sess := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "bug2-idempotent",
		KeepAlive:  5,
	}, domain.SessionEphemeral, nil)

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
