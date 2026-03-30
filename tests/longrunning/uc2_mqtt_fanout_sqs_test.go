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
// UC2: MQTT -> Content-Routed Fan-Out to 3 SQS queues
//
// Validates:
//   - MQTT wildcard subscription (uc2/devices/+/telemetry)
//   - Header-based routing: factory=A -> factory-a queue, etc.
//   - Each queue receives exactly 1,000 messages
//   - Payload integrity across the bridge
//
// Topology:
//   3 MQTT publishers (factory A, B, C)
//     --> uc2/devices/{id}/telemetry
//     --> Bridge with MatchByHeader routing
//     --> SQS-A, SQS-B, SQS-C
// =========================================================================

const (
	uc2MsgsPerFactory = 1000
	uc2PollTimeout        = 90 * time.Second
)

func TestUC2_MQTT_ContentRouted_FanOut_To_SQS(t *testing.T) {
	_ = withFreshInfra(t)
	// --- Infrastructure ---
	sqsAURL, sqsAClient := setupSQSQueue(t, "uc2-factory-a")
	sqsBURL, sqsBClient := setupSQSQueue(t, "uc2-factory-b")
	sqsCURL, sqsCClient := setupSQSQueue(t, "uc2-factory-c")
	dlq := &lrDLQStore{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// --- Bridge: MQTT receiver -> 3 SQS senders via content routing ---
	rxSessID := mqttlocal.UniqueClientID("uc2-rx")
	rxSess := setupMQTTSession(t, rxSessID, domain.SessionEphemeral)

	// Subscribe to wildcard topic.
	require.NoError(t, rxSess.Reconcile(ctx, domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{
			{Topic: "uc2/devices/+/telemetry", QoS: 1},
		},
	}), "Reconcile rx session")
	time.Sleep(300 * time.Millisecond)

	mqttRx := paho.NewReceiver("uc2-rx", rxSess)

	// Create SQS senders for each factory.
	sndA := newSQSSender(t, sqsAURL)
	sndB := newSQSSender(t, sqsBURL)
	sndC := newSQSSender(t, sqsCURL)

	// Use shared_outbox with binding-based routing: each binding has
	// its own session (SQS queue) and a noop session wrapper.
	leaseStore, outboxStore := setupDynamoStores(t)

	sidA := uniqueID("uc2-sqs-a")
	sidB := uniqueID("uc2-sqs-b")
	sidC := uniqueID("uc2-sqs-c")

	bindings := []domain.DestinationBinding{
		{ID: "bind-a", SessionID: sidA, Address: sqsAURL, Transport: "sqs"},
		{ID: "bind-b", SessionID: sidB, Address: sqsBURL, Transport: "sqs"},
		{ID: "bind-c", SessionID: sidC, Address: sqsCURL, Transport: "sqs"},
	}

	scA := lrSessionConfig(sidA)
	scB := lrSessionConfig(sidB)
	scC := lrSessionConfig(sidC)

	// Fake sessions for SQS (no real session needed for SQS senders).
	fSessB := newNoopSession()
	fSessC := newNoopSession()
	fSessA := newNoopSession()

	rt := goruntime.New(
		goruntime.WithInstanceID("uc2-bridge"),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithDLQStore(dlq),
		goruntime.WithLogger(testLogger(t)),
	)

	require.NoError(t, rt.RegisterSessionSender(scB, fSessB, sndB),
		"RegisterSessionSender B")
	require.NoError(t, rt.RegisterSessionSender(scC, fSessC, sndC),
		"RegisterSessionSender C")

	routeCfg := goruntime.RouteConfig{
		ID: "uc2-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
		},
		Resolver: goruntime.NewBindingResolver(bindings,
			goruntime.MatchByHeader("factory", map[string]string{
				"A": "bind-a",
				"B": "bind-b",
				"C": "bind-c",
			})),
		Bindings: bindings,
	}

	require.NoError(t, rt.AddRoute(routeCfg, mqttRx, sndA, fSessA, &scA),
		"AddRoute")
	require.NoError(t, rt.Start(ctx), "Start bridge")
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })

	// Wait until bridge reports ReadyForTraffic via DeepHealth.
	gobridgesync(t, 10*time.Second, rt)

	// --- Publish 1,000 messages per factory ---
	factories := []string{"A", "B", "C"}
	for _, factory := range factories {
		pubSessID := mqttlocal.UniqueClientID(
			fmt.Sprintf("uc2-pub-%s", factory))
		pubSess := setupMQTTSession(t, pubSessID, domain.SessionEphemeral)
		pubSnd := paho.NewSender(pubSess, paho.SenderOptions{
			QoS:     1,
			Timeout: 10 * time.Second,
		})

		for i := 0; i < uc2MsgsPerFactory; i++ {
			deviceID := fmt.Sprintf("dev-%s-%d", factory, i)
			topic := fmt.Sprintf("uc2/devices/%s/telemetry", deviceID)
			env := &domain.Envelope{
				ID:      fmt.Sprintf("uc2-%s-%d", factory, i),
				Subject: topic,
				Payload: []byte(fmt.Sprintf(
					`{"factory":"%s","seq":%d}`, factory, i)),
				Headers: map[string]any{
					"factory": factory,
				},
			}
			require.NoError(t, pubSnd.Send(ctx, env),
				"MQTT publish factory=%s seq=%d", factory, i)
		}
		t.Logf("UC2: published %d messages for factory %s",
			uc2MsgsPerFactory, factory)
	}

	// --- Poll each SQS queue for 1,000 messages ---
	t.Log("UC2: polling SQS-A")
	bodiesA := pollAllSQS(t, sqsAClient, sqsAURL,
		uc2MsgsPerFactory, uc2PollTimeout)
	t.Logf("UC2: SQS-A received %d messages", len(bodiesA))

	t.Log("UC2: polling SQS-B")
	bodiesB := pollAllSQS(t, sqsBClient, sqsBURL,
		uc2MsgsPerFactory, uc2PollTimeout)
	t.Logf("UC2: SQS-B received %d messages", len(bodiesB))

	t.Log("UC2: polling SQS-C")
	bodiesC := pollAllSQS(t, sqsCClient, sqsCURL,
		uc2MsgsPerFactory, uc2PollTimeout)
	t.Logf("UC2: SQS-C received %d messages", len(bodiesC))

	// --- Verify counts ---
	require.Len(t, bodiesA, uc2MsgsPerFactory,
		"factory-a queue should have %d messages", uc2MsgsPerFactory)
	require.Len(t, bodiesB, uc2MsgsPerFactory,
		"factory-b queue should have %d messages", uc2MsgsPerFactory)
	require.Len(t, bodiesC, uc2MsgsPerFactory,
		"factory-c queue should have %d messages", uc2MsgsPerFactory)

	// --- Verify payload integrity: each body contains its factory ---
	for _, b := range bodiesA {
		assert.Contains(t, b, `"factory":"A"`,
			"factory-a message should contain factory A")
	}
	for _, b := range bodiesB {
		assert.Contains(t, b, `"factory":"B"`,
			"factory-b message should contain factory B")
	}
	for _, b := range bodiesC {
		assert.Contains(t, b, `"factory":"C"`,
			"factory-c message should contain factory C")
	}

	// --- DLQ should be empty ---
	assert.Equal(t, 0, dlq.count(), "DLQ should be empty")
}

// ---------------------------------------------------------------------------
// noopSession — minimal ports.Session for SQS senders that do not
// need MQTT session management.
// ---------------------------------------------------------------------------

type noopSession struct {
	events chan ports.SessionEvent
}

var _ ports.Session = (*noopSession)(nil)

func newNoopSession() *noopSession {
	ch := make(chan ports.SessionEvent, 1)
	ch <- ports.SessionEvent{Type: ports.SessionConnected}
	return &noopSession{events: ch}
}

func (s *noopSession) Start(_ context.Context) error { return nil }
func (s *noopSession) Close(_ context.Context) error { return nil }
func (s *noopSession) Reconcile(
	_ context.Context, _ domain.SessionPlan,
) error {
	return nil
}
func (s *noopSession) Health(_ context.Context) ports.SessionHealth {
	return ports.SessionHealth{Connected: true, Ready: true, ServiceLevel: ports.ServiceLevelFull}
}
func (s *noopSession) Events() <-chan ports.SessionEvent {
	return s.events
}
