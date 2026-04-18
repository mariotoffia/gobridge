//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// =========================================================================
// UC1: SQS-IN -> 3 clustered bridges (shared subscription) -> MQTT topic
//      -> 2 fixed bridges (each subscribes independently) -> SQS-OUT-1/2
//
// Validates:
//   - Shared MQTT subscription for competing consumers (A, B, C)
//   - Fan-out: both D and E receive all messages
//   - Exactly 5,000 unique messages per output queue (no duplicates)
//
// Topology:
//   SQS-IN --[$share/uc1grp]--> A,B,C --pub--> uc1/pipeline/data
//           uc1/pipeline/data --> D --> SQS-OUT-1
//           uc1/pipeline/data --> E --> SQS-OUT-2
// =========================================================================

const (
	uc1MsgCount    = 1000
	uc1Topic       = "uc1/pipeline/data"
	uc1PollTimeout = 300 * time.Second
)

func TestUC1_SQS_MQTT_SharedSub_FanOut_SQS(t *testing.T) {
	_ = withFreshInfra(t)
	// --- Infrastructure ---
	sqsInURL, sqsInClient := setupSQSQueue(t, "uc1-in")
	sqsOut1URL, sqsOut1Client := setupSQSQueue(t, "uc1-out1")
	sqsOut2URL, sqsOut2Client := setupSQSQueue(t, "uc1-out2")
	leaseStore, outboxStore := setupDynamoStores(t)
	dlq := &lrDLQStore{}

	// Bridge context: no timeout — bridges run until t.Cleanup stops them.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// --- Ingress bridges A, B, C (competing via $share) ---
	ingressRTs := make([]*goruntime.Runtime, 3)
	for i := range ingressRTs {
		label := string(rune('A' + i))
		sessID := mqttlocal.UniqueClientID(fmt.Sprintf("uc1-ingress-%s", label))
		sess := newMQTTSession(t, sessID, domain.SessionExclusive)
		mqttSnd := setupMQTTSender(t, sess)
		sqsRx := newSQSReceiver(t, sqsInURL)
		sc := lrSessionConfig(sessID)

		rt := goruntime.New(
			goruntime.WithInstanceID(fmt.Sprintf("uc1-ingress-%s", label)),
			goruntime.WithLeaseStore(leaseStore),
			goruntime.WithOutboxStore(outboxStore),
			goruntime.WithDLQStore(dlq),
		)

		routeCfg := goruntime.RouteConfig{
			ID: fmt.Sprintf("uc1-ingress-route-%s", label),
			Policy: domain.RoutePolicy{
				DeliveryMode: domain.DeliverySharedOutbox,
			},
			Resolver: goruntime.NewStaticResolver(
				domain.DispatchPlan{BindingID: "uc1-pub", Address: uc1Topic},
			),
			Bindings: []domain.DestinationBinding{
				{ID: "uc1-pub", SessionID: sessID},
			},
		}

		require.NoError(t, rt.AddRoute(routeCfg, sqsRx, mqttSnd, sess, &sc),
			"AddRoute ingress %s", label)
		require.NoError(t, rt.Start(ctx), "Start ingress %s", label)
		ingressRTs[i] = rt
	}
	t.Cleanup(func() {
		for _, rt := range ingressRTs {
			_ = rt.Stop(context.Background())
		}
	})

	// --- Egress bridge D: subscribe uc1/pipeline/data -> SQS-OUT-1 ---
	egressD := buildEgressBridge(t, ctx, "D", uc1Topic, sqsOut1URL, dlq)
	// --- Egress bridge E: subscribe uc1/pipeline/data -> SQS-OUT-2 ---
	egressE := buildEgressBridge(t, ctx, "E", uc1Topic, sqsOut2URL, dlq)
	t.Cleanup(func() {
		_ = egressD.Stop(context.Background())
		_ = egressE.Stop(context.Background())
	})

	// Wait until all bridges report ReadyForTraffic via DeepHealth.
	allRTs := append(ingressRTs, egressD, egressE)
	gobridgesync(t, 10*time.Second, allRTs...)

	// --- Send 5,000 messages to SQS-IN ---
	t.Logf("UC1: sending %d messages to SQS-IN", uc1MsgCount)
	sendBulkToSQS(t, sqsInClient, sqsInURL, uc1MsgCount, nil)

	// --- Wait for SQS-OUT-1 to have 5,000 messages ---
	t.Log("UC1: polling SQS-OUT-1")
	out1Bodies := pollAllSQS(t, sqsOut1Client, sqsOut1URL, uc1MsgCount, uc1PollTimeout)
	t.Logf("UC1: SQS-OUT-1 received %d messages", len(out1Bodies))

	// --- Wait for SQS-OUT-2 to have 5,000 messages ---
	t.Log("UC1: polling SQS-OUT-2")
	out2Bodies := pollAllSQS(t, sqsOut2Client, sqsOut2URL, uc1MsgCount, uc1PollTimeout)
	t.Logf("UC1: SQS-OUT-2 received %d messages", len(out2Bodies))

	// --- Verify counts ---
	require.Len(t, out1Bodies, uc1MsgCount,
		"SQS-OUT-1 should have %d messages", uc1MsgCount)
	require.Len(t, out2Bodies, uc1MsgCount,
		"SQS-OUT-2 should have %d messages", uc1MsgCount)

	// --- Verify no duplicates per output queue ---
	assertNoDuplicates(t, "SQS-OUT-1", out1Bodies)
	assertNoDuplicates(t, "SQS-OUT-2", out2Bodies)

	// --- DLQ should be empty ---
	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")
}

// ---------------------------------------------------------------------------
// UC1 helper: build an egress bridge subscribing to an MQTT topic,
// forwarding to an SQS queue using direct_hold.
// ---------------------------------------------------------------------------

func buildEgressBridge(
	t *testing.T,
	ctx context.Context,
	label, mqttTopic, sqsQueueURL string,
	dlq ports.DLQStore,
) *goruntime.Runtime {
	t.Helper()

	sessID := mqttlocal.UniqueClientID(fmt.Sprintf("uc1-egress-%s", label))
	sess := setupMQTTSession(t, sessID, domain.SessionEphemeral)

	// Subscribe to the topic.
	require.NoError(t, sess.Reconcile(ctx, domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{
			{Topic: mqttTopic, QoS: 1},
		},
	}), "Reconcile egress %s", label)
	waitSubReady(t, sess, 5*time.Second)

	mqttRx := paho.NewReceiver(fmt.Sprintf("uc1-egress-%s-rx", label), sess)
	sqsSnd := newSQSSender(t, sqsQueueURL)

	rt := goruntime.New(
		goruntime.WithInstanceID(fmt.Sprintf("uc1-egress-%s", label)),
		goruntime.WithDLQStore(dlq),
	)

	routeCfg := goruntime.RouteConfig{
		ID: fmt.Sprintf("uc1-egress-route-%s", label),
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliveryDirectHold,
		},
		Resolver: goruntime.NewStaticResolver(
			domain.DispatchPlan{BindingID: "sqs-out", Address: sqsQueueURL},
		),
		SourceCapabilities: directHoldCaps,
	}

	require.NoError(t, rt.AddRoute(routeCfg, mqttRx, sqsSnd, nil, nil),
		"AddRoute egress %s", label)
	require.NoError(t, rt.Start(ctx), "Start egress %s", label)

	return rt
}

// ---------------------------------------------------------------------------
// Duplicate checker
// ---------------------------------------------------------------------------

func assertNoDuplicates(t *testing.T, name string, bodies []string) {
	t.Helper()
	seen := make(map[string]int, len(bodies))
	for _, b := range bodies {
		seen[b]++
	}
	dupes := 0
	for body, count := range seen {
		if count > 1 {
			dupes++
			if dupes <= 5 {
				t.Errorf("%s: duplicate body (count=%d): %s", name, count, body)
			}
		}
	}
	if dupes > 5 {
		t.Errorf("%s: %d additional duplicates not shown", name, dupes-5)
	}
}
