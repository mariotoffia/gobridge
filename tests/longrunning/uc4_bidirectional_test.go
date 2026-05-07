//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

// TestUC4_Bidirectional_SQS_MQTT validates simultaneous bidirectional
// traffic between SQS and MQTT with zero cross-contamination.
//
// Topology:
//
//	Direction A: SQS-NORTH -> [Bridge-A] -> MQTT "uc4/south/data" -> [Collector-South]
//	Direction B: MQTT "uc4/north/data" -> [Bridge-B] -> SQS-SOUTH -> [Poll-South]
//
// Volume: 2,000 messages per direction (4,000 total).
// Verification: no cross-contamination; direction-A payloads only in
// collector-south, direction-B payloads only in SQS-SOUTH.
func TestUC4_Bidirectional_SQS_MQTT(t *testing.T) {
	_ = withFreshInfra(t)
	const (
		msgCount    = 2000
		pollTimeout = 120 * time.Second
	)

	// -- Infrastructure: SQS queues ----------------------------------------
	northQueueURL, northSQSClient := setupSQSQueue(t, "uc4-north")
	southQueueURL, southSQSClient := setupSQSQueue(t, "uc4-south")

	// -- MQTT sessions for bridges -----------------------------------------
	sessAID := mqttlocal.UniqueClientID("uc4-bridge-a")
	sessA := setupMQTTSession(t, sessAID, connectivity.SessionEphemeral)
	mqttSenderA := setupMQTTSender(t, sessA)

	sessBID := mqttlocal.UniqueClientID("uc4-bridge-b")
	sessB := setupMQTTSession(t, sessBID, connectivity.SessionEphemeral)

	// Reconcile Bridge-B subscription on uc4/north/data.
	err := sessB.Reconcile(context.Background(), connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{
			{Topic: "uc4/north/data", QoS: 1},
		},
	})
	require.NoError(t, err, "Bridge-B Reconcile")
	waitSubReady(t, sessB, 5*time.Second)

	mqttReceiverB := paho.NewReceiver("uc4-rx-b", sessB)
	sqsSenderSouth := newSQSSender(t, southQueueURL)
	sqsReceiverNorth := newSQSReceiver(t, northQueueURL)

	// -- MQTT collector on uc4/south/data (direction A sink) ----------------
	collectorSouth := newMQTTCollector(t, "uc4/south/data", "uc4-col-south")

	// -- DLQ stores --------------------------------------------------------
	dlqA := &lrDLQStore{}
	dlqB := &lrDLQStore{}

	// -- Bridge-A: SQS-NORTH -> MQTT uc4/south/data -----------------------
	rtA := goruntime.New(
		goruntime.WithInstanceID("uc4-bridge-A"),
		goruntime.WithDLQStore(dlqA),
	)

	routeA := goruntime.RouteConfig{
		ID: "uc4-route-a",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "mqtt-south", Address: "uc4/south/data"},
		),
		SourceCapabilities: []ports.Capability{
			ports.CapSourceRedelivery,
			ports.CapVisibilityExtension,
		},
	}
	require.NoError(t, rtA.AddRoute(routeA, sqsReceiverNorth, mqttSenderA, nil, nil))

	// -- Bridge-B: MQTT uc4/north/data -> SQS-SOUTH ----------------------
	rtB := goruntime.New(
		goruntime.WithInstanceID("uc4-bridge-B"),
		goruntime.WithDLQStore(dlqB),
	)

	routeB := goruntime.RouteConfig{
		ID: "uc4-route-b",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
		},
		Resolver: goruntime.NewStaticResolver(
			routing.DispatchPlan{BindingID: "sqs-south", Address: southQueueURL},
		),
		SourceCapabilities: directHoldCaps,
	}
	require.NoError(t, rtB.AddRoute(routeB, mqttReceiverB, sqsSenderSouth, nil, nil))

	// -- Start both bridges ------------------------------------------------
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, rtA.Start(ctx))
	defer func() { _ = rtA.Stop(context.Background()) }()

	require.NoError(t, rtB.Start(ctx))
	defer func() { _ = rtB.Stop(context.Background()) }()

	// -- Concurrent traffic injection --------------------------------------
	var wg sync.WaitGroup

	// Direction A: send 2,000 messages to SQS-NORTH.
	// sendBulkToSQS produces body "msg-{i}"; we tag with header direction=A.
	wg.Add(1)
	go func() {
		defer wg.Done()
		sendBulkToSQS(t, northSQSClient, northQueueURL, msgCount,
			func(i int) map[string]string {
				return map[string]string{"direction": "A"}
			},
		)
	}()

	// Direction B: publish 2,000 messages to MQTT uc4/north/data.
	// We use payload prefix "dirB-" to distinguish from direction A.
	wg.Add(1)
	go func() {
		defer wg.Done()
		pubSess := setupMQTTSession(t, mqttlocal.UniqueClientID("uc4-pub-b"), connectivity.SessionEphemeral)
		pubTx := paho.NewSender(pubSess, paho.SenderOptions{
			QoS:     1,
			Timeout: 10 * time.Second,
		})
		for i := 0; i < msgCount; i++ {
			env := &messaging.Envelope{
				ID:      fmt.Sprintf("uc4-dirB-%d", i),
				Subject: "uc4/north/data",
				Payload: []byte(fmt.Sprintf("dirB-%d", i)),
				Headers: map[string]any{"direction": "B"},
			}
			if err := pubTx.Send(context.Background(), ports.OutboundMessage{Envelope: env, Address: "uc4/north/data"}); err != nil {
				t.Errorf("MQTT publish dirB-%d: %v", i, err)
				return
			}
		}
	}()

	wg.Wait()

	// -- Wait for direction A: MQTT collector-south == 2,000 ---------------
	lrWaitFor(t, pollTimeout, "collector-south to reach 2000", func() bool {
		return collectorSouth.count() >= msgCount
	})

	// -- Wait for direction B: SQS-SOUTH == 2,000 -------------------------
	southBodies := pollAllSQS(t, southSQSClient, southQueueURL, msgCount, pollTimeout)

	// -- Verification: counts ----------------------------------------------
	require.Equal(t, msgCount, collectorSouth.count(),
		"collector-south should have exactly %d messages", msgCount)
	require.Equal(t, msgCount, len(southBodies),
		"SQS-SOUTH should have exactly %d messages", msgCount)

	// -- Verification: no cross-contamination ------------------------------
	// Direction A payloads come from sendBulkToSQS (format "msg-{i}").
	// Direction B payloads are "dirB-{i}".
	// Collector-south (direction A sink) must NOT contain "dirB-" payloads.
	southMsgs := collectorSouth.getMessages()
	for _, msg := range southMsgs {
		payload := string(msg.Payload)
		require.False(t, strings.HasPrefix(payload, "dirB-"),
			"collector-south received direction-B message: %q", payload)
	}

	// SQS-SOUTH (direction B sink) must contain ONLY "dirB-" payloads.
	for _, body := range southBodies {
		require.True(t, strings.HasPrefix(body, "dirB-"),
			"SQS-SOUTH received non-dirB message: %q", body)
	}

	// -- Verify DLQ is empty -----------------------------------------------
	require.Equal(t, 0, dlqA.count(), "Bridge-A DLQ should be empty")
	require.Equal(t, 0, dlqB.count(), "Bridge-B DLQ should be empty")

	t.Logf("UC4: Bidirectional traffic verified -- %d direction-A to MQTT, %d direction-B to SQS",
		collectorSouth.count(), len(southBodies))
}
