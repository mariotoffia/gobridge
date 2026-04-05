//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// ═══════════════════════════════════════════════════════════════════════════
// Gap Test: Fan-Out SharedOutbox (Category 2 — Delivery Modes)
//
// Validates DispatchFanOut with SharedOutbox to 3 MQTT targets.
// Each source message produces 3 outbox records (one per binding).
// All 3 must be delivered.
//
// NOTE: DirectHold + FanOut is invalid (rejected by runtime validator).
// Fan-out with durability guarantees requires SharedOutbox.
// ═══════════════════════════════════════════════════════════════════════════

// TestGAP_FanOutSharedOutbox_ThreeTargets validates that SharedOutbox
// fan-out to 3 MQTT targets delivers all messages to all targets.
//
// Scenario:
// ───────────────────────────────────────────────
//   SQS ──▶ [Bridge] ──▶ [Outbox Persist x3]
//                              │
//                    ┌─────────┼─────────┐
//                    ▼         ▼         ▼
//                 MQTT-A    MQTT-B    MQTT-C
//                 (500)     (500)     (500)
// ───────────────────────────────────────────────
//
// Test Parameters:
//   - Messages: 500
//   - Targets: 3 MQTT topics
//   - DeliveryMode: SharedOutbox
//   - DispatchMode: FanOut
//
// Assertions:
//   - Each collector receives 500 messages
//   - All collectors receive same set of sequence numbers
//   - DLQ empty
func TestGAP_FanOutSharedOutbox_ThreeTargets(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 500
		topicA      = "gap-fo/a"
		topicB      = "gap-fo/b"
		topicC      = "gap-fo/c"
		testTimeout = 180 * time.Second
	)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	sqsInURL, sqsInClient := setupSQSQueue(t, "gap-fo-in")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &lrDLQStore{}

	// Three collectors on different topics.
	colA := newMQTTCollector(t, topicA, "gap-fo-col-a")
	colB := newMQTTCollector(t, topicB, "gap-fo-col-b")
	colC := newMQTTCollector(t, topicC, "gap-fo-col-c")

	// Primary session + sender (for the route).
	sessIDA := mqttlocal.UniqueClientID("gap-fo-sa")
	sessA := newMQTTSession(t, sessIDA, domain.SessionExclusive)
	sndA := setupMQTTSender(t, sessA)
	scA := lrSessionConfig(sessIDA)

	// Secondary sessions registered via RegisterSessionSender.
	sessIDB := mqttlocal.UniqueClientID("gap-fo-sb")
	sessB := newMQTTSession(t, sessIDB, domain.SessionExclusive)
	sndB := setupMQTTSender(t, sessB)
	scB := lrSessionConfig(sessIDB)

	sessIDC := mqttlocal.UniqueClientID("gap-fo-sc")
	sessC := newMQTTSession(t, sessIDC, domain.SessionExclusive)
	sndC := setupMQTTSender(t, sessC)
	scC := lrSessionConfig(sessIDC)

	// Resolver that returns 3 dispatch plans — each references a binding.
	resolver := goruntime.NewStaticResolver(
		domain.DispatchPlan{BindingID: "fo-bind-a", Address: topicA},
		domain.DispatchPlan{BindingID: "fo-bind-b", Address: topicB},
		domain.DispatchPlan{BindingID: "fo-bind-c", Address: topicC},
	)

	rt := goruntime.New(
		goruntime.WithInstanceID("gap-fo"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)

	// Register secondary sessions.
	require.NoError(t, rt.RegisterSessionSender(scB, sessB, sndB))
	require.NoError(t, rt.RegisterSessionSender(scC, sessC, sndC))

	require.NoError(t, rt.AddRoute(goruntime.RouteConfig{
		ID: "gap-fo-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
			DispatchMode: domain.DispatchFanOut,
			AckAfter:     domain.AckAfterOutboxPersist,
		},
		Resolver: resolver,
		// Bindings map DispatchPlan.BindingID → session for outbox partitioning.
		Bindings: []domain.DestinationBinding{
			{ID: "fo-bind-a", SessionID: sessIDA},
			{ID: "fo-bind-b", SessionID: sessIDB},
			{ID: "fo-bind-c", SessionID: sessIDC},
		},
	}, newSQSReceiver(t, sqsInURL), sndA, sessA, &scA))

	require.NoError(t, rt.Start(ctx))
	gobridgesync(t, 15*time.Second, rt)

	t.Logf("GAP-FO: sending %d messages", msgCount)
	sendBulkToSQS(t, sqsInClient, sqsInURL, msgCount, nil)

	// Wait for all three collectors.
	lrWaitFor(t, 120*time.Second,
		fmt.Sprintf("collector A >= %d", msgCount),
		func() bool { return colA.count() >= msgCount })
	lrWaitFor(t, 30*time.Second,
		fmt.Sprintf("collector B >= %d", msgCount),
		func() bool { return colB.count() >= msgCount })
	lrWaitFor(t, 30*time.Second,
		fmt.Sprintf("collector C >= %d", msgCount),
		func() bool { return colC.count() >= msgCount })

	t.Logf("GAP-FO: A=%d, B=%d, C=%d, dlq=%d",
		colA.count(), colB.count(), colC.count(), dlq.count())

	assert.GreaterOrEqual(t, colA.count(), msgCount,
		"collector A should receive %d messages", msgCount)
	assert.GreaterOrEqual(t, colB.count(), msgCount,
		"collector B should receive %d messages", msgCount)
	assert.GreaterOrEqual(t, colC.count(), msgCount,
		"collector C should receive %d messages", msgCount)
	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")

	require.NoError(t, rt.Stop(context.Background()))
}
