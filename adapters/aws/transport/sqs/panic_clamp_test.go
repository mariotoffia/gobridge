package sqs

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// Finding 3 — Extend()/ticker.Reset(0) panic.
//
// Extend() could store a 0/1-second visibility timeout when the requested
// deadline resolved to "now or earlier", which then drove the auto-extend
// loop to call ticker.Reset(0) — a panic on both time.Ticker and the fake
// clock (clocktest panics on a non-positive interval). The fix clamps the
// applied/stored visibility to minAutoExtendVisibilitySeconds and floors
// the derived tick interval at 1s. These tests pin both layers
// deterministically with a fake clock.

// TestExtend_PastDeadline_ClampsVisibilityFloor verifies a degenerate
// Extend never issues a 0/1-second ChangeMessageVisibility (which would
// surface the in-flight message and invite duplicate processing) and never
// stores a sub-floor timeout. autoExtend=false isolates Extend's own clamp
// (no goroutine), so the assertion is fully deterministic.
func TestExtend_PastDeadline_ClampsVisibilityFloor(t *testing.T) {
	cases := []struct {
		name  string
		delta time.Duration
	}{
		{"deadline_now", 0},
		{"deadline_in_past", -10 * time.Second},
		{"deadline_below_floor", 1 * time.Second}, // 1s < 2s floor
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := &mockSQSClient{}
			fake := clocktest.New()

			del := newDelivery(
				context.Background(),
				messaging.MustEnvelope(messaging.EnvelopeInput{ID: "x", Subject: "s"}),
				mock,
				"https://sqs.us-west-1.amazonaws.com/q",
				"rh",
				30,    // visibilityTimeout
				false, // autoExtend OFF: no goroutine, isolate the clamp
				nil,
				nil,
				&ports.NoopExporter{},
				fake,
			)

			require.NoError(t, del.Extend(context.Background(), fake.Now().Add(tc.delta)))

			mock.mu.Lock()
			defer mock.mu.Unlock()
			require.Len(t, mock.ChangeVisibilityCalls, 1)
			assert.Equal(t, minAutoExtendVisibilitySeconds,
				mock.ChangeVisibilityCalls[0].VisibilityTimeout,
				"degenerate Extend must clamp to the 2s floor, never 0/1")
			assert.Equal(t, minAutoExtendVisibilitySeconds, del.visibilityTimeout.Load(),
				"stored visibility must be the clamped value so the next tick stays valid")
		})
	}
}

// TestAutoExtendLoop_DegenerateExtend_NoTickerResetPanic drives the
// auto-extend loop across the 0/1s boundary with a fake clock. A panic in
// the loop goroutine (ticker.Reset on a non-positive interval) would crash
// the whole test binary, so reaching the end of the test IS the assertion.
//
// The loop starts at visibility=4 (2s tick interval). A degenerate Extend
// (deadline == now) clamps the stored visibility to 2s, so the next tick
// recomputes the interval to 1s and calls ticker.Reset(1s) — which without
// the fix would have been ticker.Reset(0) and panicked.
func TestAutoExtendLoop_DegenerateExtend_NoTickerResetPanic(t *testing.T) {
	mock := &mockSQSClient{}
	fake := clocktest.New()

	parentCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	del := newDelivery(
		parentCtx,
		messaging.MustEnvelope(messaging.EnvelopeInput{ID: "x", Subject: "s"}),
		mock,
		"https://sqs.us-west-1.amazonaws.com/q",
		"rh",
		4,    // visibilityTimeout → initial tick interval 2s
		true, // autoExtend ON
		nil,
		nil,
		&ports.NoopExporter{},
		fake,
	)

	wait.Until(t, time.Second, "auto-extend ticker registered", func() bool {
		return fake.TickerCount() >= 1
	})

	// Degenerate Extend: deadline == now → clamps to the 2s floor.
	require.NoError(t, del.Extend(context.Background(), fake.Now()))
	mock.mu.Lock()
	require.GreaterOrEqual(t, len(mock.ChangeVisibilityCalls), 1)
	assert.Equal(t, minAutoExtendVisibilitySeconds, mock.ChangeVisibilityCalls[0].VisibilityTimeout)
	mock.mu.Unlock()

	// Fire the next tick at the original 2s interval. The loop reads the
	// clamped vis=2, extends, recomputes interval=1s != 2s and calls
	// ticker.Reset(1s). With the bug this is Reset(0) → goroutine panic.
	fake.Advance(2 * time.Second)
	wait.Until(t, time.Second, "auto-extend tick fired after degenerate Extend", func() bool {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		return len(mock.ChangeVisibilityCalls) >= 2 // Extend + tick
	})

	require.NoError(t, del.Ack(context.Background()))
}
