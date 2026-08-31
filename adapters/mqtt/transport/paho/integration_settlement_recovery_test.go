package paho_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestIntegration_ResumedSessionRedeliveryIsNotPurged exercises the full
// resume-and-replay path against a live broker: a persistent session holds
// un-acked QoS 1 publishes, the connection is recycled, and the broker replays
// the whole backlog in the burst that follows CONNACK. Every replayed message
// must reach the receiver and settle.
//
// This is the end-to-end regression for that path. It does NOT deterministically
// hit the ordering window inside it — whether a replayed publish reaches the
// router before autopaho's connection-up callback depends on the broker's
// timing — so the window itself is pinned by the router unit tests, which drive
// the callback seam directly. A publish dropped in that window is not merely
// delayed: it is never acked, so it sits at the head of paho's
// contiguous-prefix acknowledgement tracker and silently swallows the PUBACK of
// every message settled after it.
func TestIntegration_ResumedSessionRedeliveryIsNotPurged(t *testing.T) {
	if testing.Short() {
		t.Skip("requires a live MQTT broker")
	}
	url := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	topic := "reconnect/backlog/" + mqttlocal.UniqueClientID("topic")
	plan := connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}

	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:            []string{url},
		ClientID:              mqttlocal.UniqueClientID("reconnect-backlog"),
		KeepAlive:             10,
		ConnectTimeout:        10 * time.Second,
		CleanStart:            false,
		SessionExpiryInterval: 300,
	}, connectivity.SessionPersistent, nil)
	t.Cleanup(func() { _ = sess.Close(context.Background()) })

	require.NoError(t, sess.Start(ctx))
	require.NoError(t, sess.Reconcile(ctx, plan))
	waitSubActive(t, sess, 10*time.Second)

	// No receiver is registered yet, so these land in the router's startup-grace
	// buffer un-acked — exactly the state a broker replays on resume.
	const backlog = 20
	publishBacklog(t, ctx, url, topic, backlog)
	wait.Until(t, 15*time.Second, "the broker delivered the backlog to the un-registered session", func() bool {
		return sess.Router().PendingCount() == backlog
	})

	// Recycle the connection. The resumed session replays every un-acked QoS 1
	// with fresh packet IDs, starting the instant CONNACK is written.
	require.NoError(t, sess.Reload(ctx))
	require.NoError(t, sess.Reconcile(ctx, plan))

	var mu sync.Mutex
	seen := make(map[string]struct{}, backlog)
	receiver := paho.NewReceiver("rx-backlog", sess, paho.WithTopicFilters(topic))
	runCtx, stopReceiver := context.WithCancel(ctx)
	runDone := make(chan error, 1)
	go func() {
		runDone <- receiver.Run(runCtx, func(ctx context.Context, delivery ports.Delivery) error {
			mu.Lock()
			seen[string(delivery.Envelope().Payload())] = struct{}{}
			mu.Unlock()
			return delivery.Ack(ctx)
		})
	}()
	wait.RequireClosed(t, receiver.Started(), 10*time.Second)

	wait.Until(t, 30*time.Second, "every replayed publish reaches the receiver", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) == backlog
	})

	stopReceiver()
	if err := <-runDone; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("receiver stopped with error: %v", err)
	}
}

// TestIntegration_SettlementRecoverySurvivesASlowSettlement drives a real
// settlement recovery whose ingress drain is held by a downstream that is slow
// but perfectly cooperative.
//
// The hold is deliberately longer than the five-second bound the drain used to
// carry: a settlement is bounded by the route that owns it (send-wedge,
// processor and store ceilings, all of which start at 30 seconds), so an
// adapter-local bound below that classified ordinary slowness as an
// unrecoverable drain failure — terminalizing the session and restarting every
// unrelated route in the process.
func TestIntegration_SettlementRecoverySurvivesASlowSettlement(t *testing.T) {
	if testing.Short() {
		t.Skip("requires a live MQTT broker")
	}
	url := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	topic := "recovery/slow/" + mqttlocal.UniqueClientID("topic")
	plan := connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}
	metrics := &ports.RecordingExporter{}

	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:            []string{url},
		ClientID:              mqttlocal.UniqueClientID("recovery-slow"),
		KeepAlive:             10,
		ConnectTimeout:        10 * time.Second,
		CleanStart:            false,
		SessionExpiryInterval: 300,
	}, connectivity.SessionPersistent, nil, metrics)
	t.Cleanup(func() { _ = sess.Close(context.Background()) })

	// A cooperative downstream that settles well past the old five-second cap.
	const slowSettlement = 6 * time.Second
	sess.SetIngressQuiescenceWaiter(func(waitCtx context.Context) error {
		timer := time.NewTimer(slowSettlement)
		defer timer.Stop()
		select {
		case <-timer.C:
			return nil
		case <-waitCtx.Done():
			return waitCtx.Err()
		}
	})

	require.NoError(t, sess.Start(ctx))
	require.NoError(t, sess.Reconcile(ctx, plan))
	waitSubActive(t, sess, 10*time.Second)

	var deliveries int
	var mu sync.Mutex
	retried := make(chan struct{}, 1)
	receiver := paho.NewReceiver("rx-recovery", sess, paho.WithTopicFilters(topic))
	runCtx, stopReceiver := context.WithCancel(ctx)
	runDone := make(chan error, 1)
	go func() {
		runDone <- receiver.Run(runCtx, func(ctx context.Context, delivery ports.Delivery) error {
			mu.Lock()
			deliveries++
			first := deliveries == 1
			mu.Unlock()
			if first {
				// The route could not settle this delivery: ask the transport
				// for the bounded recycle that makes the broker redeliver it.
				if err := delivery.Retry(ctx, 0, shared.ErrUnavailable); err != nil {
					return err
				}
				select {
				case retried <- struct{}{}:
				default:
				}
				return nil
			}
			return delivery.Ack(ctx)
		})
	}()
	wait.RequireClosed(t, receiver.Started(), 10*time.Second)

	publishBacklog(t, ctx, url, topic, 1)
	wait.RequireReceive(t, retried, 20*time.Second)

	wait.Until(t, 60*time.Second, "the slow drain still completes a session recycle", func() bool {
		return len(metrics.FindEntries("MQTTSessionRecoveryRecycle")) == 1
	})
	wait.Until(t, 60*time.Second, "the recycled session redelivers and returns to full readiness", func() bool {
		health := sess.Health(ctx)
		mu.Lock()
		redelivered := deliveries > 1
		mu.Unlock()
		return redelivered && health.ServiceLevel == ports.ServiceLevelFull
	})
	require.NoError(t, sess.Health(ctx).LastError,
		"a cooperative slow settlement is not a session failure")

	stopReceiver()
	if err := <-runDone; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("receiver stopped with error: %v", err)
	}
}

// publishBacklog sends n QoS 1 messages to topic from an independent session.
func publishBacklog(t *testing.T, ctx context.Context, url, topic string, n int) {
	t.Helper()
	publisher := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{url},
		ClientID:       mqttlocal.UniqueClientID("backlog-publisher"),
		KeepAlive:      10,
		ConnectTimeout: 10 * time.Second,
		CleanStart:     true,
	}, connectivity.SessionEphemeral, nil)
	require.NoError(t, publisher.Start(ctx))
	defer func() { _ = publisher.Close(context.Background()) }()

	sender := paho.NewSender(publisher, paho.SenderOptions{QoS: 1, Timeout: 10 * time.Second})
	for i := range n {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			Subject: topic,
			Payload: []byte(fmt.Sprintf("backlog-%d", i)),
		})
		require.NoError(t, sender.Send(ctx, ports.OutboundMessage{Envelope: env, Address: topic}))
	}
}
