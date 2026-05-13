package runtime_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// ═══════════════════════════════════════════════════════════════════════
// Shared-Outbox Broker-Disconnect Mid-Flight Recovery (Scenario #3 unit)
//
// The broker-crash case is exercised end-to-end in
// tests/longrunning/gap_broker_crash_test.go with a real Mosquitto
// container. This file complements that with a deterministic, no-Docker
// test that exercises the core contract of shared_outbox delivery under
// mid-flight egress failure:
//
//   1. Envelope arrives on receiver.
//   2. Runtime persists it to the outbox and acks the source.
//   3. Drainer claims the record and invokes the egress sender.
//   4. Egress sender fails with a transient BridgeError (simulating a
//      broker disconnect).
//   5. The outbox record stays un-completed (status != Completed).
//   6. Egress recovers (sender starts returning nil).
//   7. The drainer re-claims the stale-claimed record and the Complete
//      call succeeds.
//
//   receiver ──▶ outbox(Pending) ──ack──▶ source
//                     │
//                  claim
//                     ▼
//               drainer.Send ──✗── transient ──▶ record stays Claimed
//                     │
//             stale-claim age elapses
//                     ▼
//               drainer.Send ──✓──▶ Complete
//
//  ┌──────┬──────────────────────────────────────────────┬──────────┐
//  │ ID   │ Description                                  │ Type     │
//  ├──────┼──────────────────────────────────────────────┼──────────┤
//  │ TR1  │ Transient send → record stays, retry succeeds│ unit     │
//  │ TR2  │ Completion count bounded (no duplicate Complete)│ unit  │
//  └──────┴──────────────────────────────────────────────┴──────────┘
// ═══════════════════════════════════════════════════════════════════════

// TestSharedOutbox_TransientSenderFailure_RecoversOnRetry validates
// scenario #3 at the unit-integration level: the egress sender fails
// transiently (simulating broker disconnect), the outbox record is not
// completed, and after the sender recovers the drainer retries and
// completes the record successfully. No Docker required.
//
// Scenario:
//
//	Send 3 messages while the egress sender is "down" (transient fail).
//	Verify: sources are acked (outbox persist succeeded), drainer made
//	        at least one send attempt per record, but CompletedCount
//	        remains 0.
//	Switch the sender to "up" and wait for StaleClaimAge to elapse so
//	the drainer re-claims the Claimed-but-not-Completed records.
//	Verify: all 3 records get sent AND completed.
func TestSharedOutbox_TransientSenderFailure_RecoversOnRetry(t *testing.T) {
	outbox := NewFakeOutboxStore()
	// Aggressively short stale-claim age so re-claim happens within the
	// test timeout. 100ms is well below the 5s test budget but above
	// the drainer's polling resolution (30ms from fastSessionConfig).
	outbox.StaleClaimAge = 100 * time.Millisecond

	lease := NewFakeLeaseStore()
	dlq := NewFakeDLQStore()

	rt := newTestRuntime("bridge-transient", outbox, lease, dlq)

	receiver := NewFakeReceiver()

	var senderUp atomic.Bool
	var sendAttempts atomic.Int64
	var (
		mu       sync.Mutex
		attempts []string // order-preserving list of envelope IDs attempted
	)

	sender := NewFakeSender()
	sender.SendFn = func(env *messaging.Envelope) error {
		sendAttempts.Add(1)
		mu.Lock()
		attempts = append(attempts, env.ID)
		mu.Unlock()
		if !senderUp.Load() {
			return shared.NewBridgeError(
				"BROKER_DISCONNECTED",
				shared.ErrorTransient,
				"simulated broker disconnect",
			)
		}
		return nil
	}

	sess := NewFakeSession()
	sessCfg := fastSessionConfig("mqtt-sess-transient")

	cfg := goruntime.RouteConfig{
		ID: "transient-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
			// A generous MaxReplayAttempts so transient retries don't
			// flip to poison/DLQ during this test.
			MaxReplayAttempts: 20,
		},
		Resolver: &FakeResolver{
			Plans: []routing.DispatchPlan{
				{BindingID: "b1", Address: "devices/out"},
			},
		},
		Bindings: []routing.DestinationBinding{
			{ID: "b1", SessionID: "mqtt-sess-transient"},
		},
	}

	require.NoError(t, rt.AddRoute(cfg, receiver, sender, sess, &sessCfg))

	ctx := t.Context()

	require.NoError(t, rt.Start(ctx))
	defer func() { _ = rt.Stop(context.Background()) }()

	waitFor(t, 2*time.Second, "sess started", func() bool {
		return sess.IsStarted()
	})

	// Emit while sender is "down".
	const msgCount = 3
	dels := make([]*FakeDelivery, msgCount)
	for i := range msgCount {
		env := &messaging.Envelope{
			ID:      envID(i),
			Payload: []byte("payload"),
		}
		dels[i] = NewFakeDelivery(env)
		require.NoError(t, receiver.Emit(ctx, dels[i]))
	}

	// Sources must be acked once persist succeeds — this is the
	// shared_outbox "decouple-from-egress" property.
	for i, del := range dels {
		waitFor(t, 2*time.Second, "source acked after persist", del.IsAcked)
		require.Truef(t, del.IsAcked(), "delivery %d must be acked after outbox persist", i)
	}

	// All 3 must be persisted in outbox.
	waitFor(t, 2*time.Second, "all records persisted", func() bool {
		return outbox.RecordCount() >= msgCount
	})

	// Drainer must have attempted at least once per record while down.
	waitFor(t, 3*time.Second, "drainer attempted send while down", func() bool {
		return sendAttempts.Load() >= msgCount
	})

	// Critical invariant: while the sender is transiently failing, NO
	// record is marked completed. The drainer must keep them Claimed
	// so a future drain cycle (after stale-claim expiry) re-picks them.
	// Give it a bit of time to confirm this is stable.
	stableCheckDeadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(stableCheckDeadline) {
		require.Equal(t, 0, outbox.CompletedCount(),
			"no records must be completed while sender is transient-failing")
		time.Sleep(25 * time.Millisecond) // NEGATIVE: verify no records complete during transient-failure window
	}

	// Bring the sender back up. The drainer should re-claim stale
	// records (after StaleClaimAge) and complete them.
	senderUp.Store(true)

	waitFor(t, 5*time.Second, "all records completed after recovery", func() bool {
		return outbox.CompletedCount() >= msgCount
	})

	assert.Equal(t, msgCount, outbox.CompletedCount(),
		"exactly msgCount records should be completed after recovery")

	// No DLQ entries — transient errors must not escalate.
	assert.Zero(t, dlq.Count(),
		"transient failures must not route to DLQ")

	// Sanity: each envelope ID was attempted at least twice (once while
	// down, once after recovery). We use >= 2 * msgCount as a lower
	// bound because replay cycles may attempt more than twice.
	mu.Lock()
	totalAttempts := len(attempts)
	mu.Unlock()
	assert.GreaterOrEqual(t, totalAttempts, 2*msgCount,
		"expected at least one retry per record after recovery")
}

// envID returns a deterministic envelope ID for the given index.
func envID(i int) string {
	return "transient-msg-" + string(rune('a'+i))
}
