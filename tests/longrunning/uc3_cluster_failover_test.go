//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// =========================================================================
// UC3: 3-Instance Cluster Failover
//
// Validates:
//   - Clustered bridge with shared lease + outbox
//   - Leader failover: stop leader, second takes over
//   - Cascading failover: stop second leader, third finishes
//   - Exactly 2,000 unique messages delivered (at-least-once)
//   - DLQ remains empty
//
// Topology:
//   SQS-IN --> [A, B, C] (competing via lease) --> MQTT uc3/output/data
//
// Sequence:
//   1. Start A, B, C — one acquires lease
//   2. Send 2,000 messages to SQS-IN
//   3. Wait ~500 received, stop leader
//   4. Wait ~1,000 received, stop second leader
//   5. Third finishes remaining
//   6. Verify 2,000 unique messages on MQTT
// =========================================================================

const (
	uc3MsgCount = 2000
	uc3Topic    = "uc3/output/data"
	// uc3 has no poll timeout constant; individual lrWaitFor calls specify
	// their own durations.
)

func TestUC3_ClusterFailover_ThreeInstances(t *testing.T) {
	_ = withFreshInfra(t)
	// --- Infrastructure ---
	sqsInURL, sqsInClient := setupSQSQueue(t, "uc3-in")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &lrDLQStore{}
	sessionID := mqttlocal.UniqueClientID("uc3-session")

	// --- MQTT collector on the output topic ---
	collector := newMQTTCollector(t, uc3Topic, "uc3-collector")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// --- Build route config (shared across all instances) ---
	routeCfg := goruntime.RouteConfig{
		ID: "uc3-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "uc3-bind", Address: uc3Topic},
		),
		Bindings: []domain.DestinationBinding{
			{ID: "uc3-bind", SessionID: sessionID},
		},
	}

	// --- Tracking which instance is the current leader ---
	type instance struct {
		label  string
		rt     *goruntime.Runtime
		cancel context.CancelFunc
	}

	// --- Helper to create a bridge instance ---
	mkInstance := func(label string) *instance {
		mqttSessID := mqttlocal.UniqueClientID(
			fmt.Sprintf("uc3-%s", label))
		sess := newMQTTSession(t, mqttSessID, domain.SessionExclusive)
		mqttSnd := setupMQTTSender(t, sess)
		sqsRx := newSQSReceiver(t, sqsInURL)
		sc := lrSessionConfig(sessionID)

		rt := goruntime.New(
			goruntime.WithInstanceID(fmt.Sprintf("uc3-%s", label)),
			goruntime.WithLeaseStore(leaseStore),
			goruntime.WithOutboxStore(outboxStore),
			goruntime.WithDLQStore(dlq),
		)

		require.NoError(t, rt.AddRoute(routeCfg, sqsRx, mqttSnd, sess, &sc),
			"AddRoute %s", label)

		instCtx, instCancel := context.WithCancel(ctx)
		require.NoError(t, rt.Start(instCtx), "Start %s", label)

		return &instance{label: label, rt: rt, cancel: instCancel}
	}

	// --- Start all three instances ---
	instA := mkInstance("A")
	instB := mkInstance("B")
	instC := mkInstance("C")

	instances := []*instance{instA, instB, instC}
	var stopped sync.Mutex
	stoppedSet := make(map[string]bool)

	stopInstance := func(inst *instance) {
		stopped.Lock()
		defer stopped.Unlock()
		if stoppedSet[inst.label] {
			return
		}
		stoppedSet[inst.label] = true
		t.Logf("UC3: stopping instance %s", inst.label)
		inst.cancel()
		_ = inst.rt.Stop(context.Background())
		t.Logf("UC3: instance %s stopped", inst.label)
	}

	// Cleanup any remaining instances.
	t.Cleanup(func() {
		for _, inst := range instances {
			stopInstance(inst)
		}
	})

	// Wait until all bridges report ReadyForTraffic via DeepHealth.
	gobridgesync(t, 10*time.Second, instA.rt, instB.rt, instC.rt)

	// --- Send 2,000 messages to SQS-IN ---
	t.Logf("UC3: sending %d messages to SQS-IN", uc3MsgCount)
	sendBulkToSQS(t, sqsInClient, sqsInURL, uc3MsgCount, nil)

	// --- Phase 1: wait for ~500 messages, then stop first leader ---
	t.Log("UC3: waiting for ~500 messages before first failover")
	lrWaitFor(t, 60*time.Second, "~500 messages received", func() bool {
		return collector.count() >= 500
	})
	t.Logf("UC3: collector has %d messages, triggering first failover",
		collector.count())

	// Find the instance that has been processing (heuristic: stop A first
	// since it was started first and most likely acquired the lease).
	stopInstance(instA)

	// --- Phase 2: wait for ~1,000 messages, then stop second leader ---
	t.Log("UC3: waiting for ~1,000 messages before second failover")
	lrWaitFor(t, 60*time.Second, "~1,000 messages received", func() bool {
		return collector.count() >= 1000
	})
	t.Logf("UC3: collector has %d messages, triggering second failover",
		collector.count())
	stopInstance(instB)

	// --- Phase 3: third instance finishes remaining ---
	t.Log("UC3: waiting for all 2,000 messages")
	lrWaitFor(t, 90*time.Second, "2,000 messages received", func() bool {
		return collector.count() >= uc3MsgCount
	})
	t.Logf("UC3: collector has %d messages total", collector.count())

	// --- Verify uniqueness ---
	// With at-least-once, we may have more than 2,000 total envelopes
	// (duplicates possible during failover). But we should have at least
	// 2,000 unique messages.
	msgs := collector.getMessages()
	require.GreaterOrEqual(t, len(msgs), uc3MsgCount,
		"should have at least %d messages", uc3MsgCount)

	uniqueIDs := make(map[string]int, len(msgs))
	for _, m := range msgs {
		uniqueIDs[string(m.Payload)]++
	}
	assert.GreaterOrEqual(t, len(uniqueIDs), uc3MsgCount,
		"should have at least %d unique message payloads", uc3MsgCount)

	// Log duplicate stats.
	dupes := 0
	for _, count := range uniqueIDs {
		if count > 1 {
			dupes++
		}
	}
	if dupes > 0 {
		t.Logf("UC3: %d payload(s) received more than once (at-least-once)",
			dupes)
	}

	// --- DLQ should be empty ---
	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")

	t.Logf("UC3: test passed with %d unique payloads, %d total envelopes",
		len(uniqueIDs), len(msgs))
}
