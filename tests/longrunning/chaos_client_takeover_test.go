//go:build longrunning

package longrunning_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/tests/testutil/prodid"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

func TestTask14_DuplicateClientIDTakeoverStorm(t *testing.T) {
	broker := mqttlocal.NewBrokerInstance(t, mqttlocal.WithPersistence(true))
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	clientID := mqttlocal.UniqueClientID("task14-takeover")
	topic := "task14/takeover/" + mqttlocal.UniqueClientID("topic")
	plan := connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}
	metricsA := &ports.RecordingExporter{}
	metricsB := &ports.RecordingExporter{}
	newContender := func(metrics ports.MetricsExporter) *paho.Session {
		return paho.NewSession(paho.SessionOptions{
			BrokerURLs:            []string{broker.URL()},
			ClientID:              clientID,
			KeepAlive:             2,
			ConnectTimeout:        10 * time.Second,
			ReconnectDelay:        50 * time.Millisecond,
			ReconnectTimeout:      2 * time.Second,
			CleanStart:            false,
			SessionExpiryInterval: 300,
		}, connectivity.SessionExclusive, testLogger(t), metrics)
	}

	first := newContender(metricsA)
	t.Cleanup(func() { _ = first.Close(context.Background()) })
	require.NoError(t, first.Start(ctx))
	require.NoError(t, first.Reconcile(ctx, plan))
	accountant, err := prodid.New(task14TakeoverKeys(20), false)
	require.NoError(t, err)
	receiver := paho.NewReceiver("task14-takeover-first", first, paho.WithTopicFilters(topic))
	receiverCtx, stopReceiver := context.WithCancel(ctx)
	receiverDone := make(chan error, 1)
	go func() {
		receiverDone <- receiver.Run(receiverCtx, func(ctx context.Context, delivery ports.Delivery) error {
			envelope := delivery.Envelope()
			accountant.ObserveOutput(envelope.ID(), envelope.ID())
			return delivery.Ack(ctx)
		})
	}()
	wait.RequireClosed(t, receiver.Started(), 5*time.Second)
	pump := startTakeoverReconcilePump(ctx, first, plan)

	second := newContender(metricsB)
	t.Cleanup(func() { _ = second.Close(context.Background()) })
	t.Cleanup(pump.stop)
	require.NoError(t, second.Start(ctx))
	wait.Until(t, 30*time.Second, "real duplicate-client takeover storm", func() bool {
		return takeoverMetricCount(metricsA)+takeoverMetricCount(metricsB) >= 2
	})

	// Remove one contender and require the surviving connection to converge
	// within a finite window. No traffic is published during the collision, so
	// post-recovery accounting can require exactly one output per producer key.
	require.NoError(t, second.Close(ctx))
	wait.Until(t, 30*time.Second, "takeover survivor reconnect and reconcile", func() bool {
		health := first.Health(ctx)
		return health.Connected &&
			health.SubscriptionsSatisfied != nil &&
			*health.SubscriptionsSatisfied &&
			health.ServiceLevel == ports.ServiceLevelFull
	})
	if err := pump.latestError(); err != nil {
		t.Logf("takeover reconcile encountered a bounded transient error before recovery: %v", err)
	}

	publisher := paho.NewSession(paho.SessionOptions{
		BrokerURLs:     []string{broker.URL()},
		ClientID:       mqttlocal.UniqueClientID("task14-takeover-publisher"),
		KeepAlive:      10,
		ConnectTimeout: 10 * time.Second,
		CleanStart:     true,
	}, connectivity.SessionEphemeral, testLogger(t))
	t.Cleanup(func() { _ = publisher.Close(context.Background()) })
	require.NoError(t, publisher.Start(ctx))
	sender := paho.NewSender(publisher, paho.SenderOptions{QoS: 1, Timeout: 5 * time.Second})
	for _, key := range task14TakeoverKeys(20) {
		envelope := messaging.MustEnvelope(messaging.EnvelopeInput{
			ID: key, Subject: topic, Payload: []byte(key),
		})
		require.NoError(t, sender.Send(ctx, ports.OutboundMessage{Envelope: envelope, Address: topic}))
	}
	wait.Until(t, 20*time.Second, "post-takeover exact delivery", func() bool {
		return len(accountant.Reconcile().Missing) == 0
	})
	if report := accountant.Reconcile(); !report.Exact() {
		t.Fatalf("takeover accounting failed: %s", report.String())
	}

	pump.stop()
	stopReceiver()
	if err := <-receiverDone; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("takeover receiver stopped with error: %v", err)
	}
}

func task14TakeoverKeys(count int) []string {
	keys := make([]string, count)
	for i := range count {
		keys[i] = "takeover-" + string(rune('a'+i))
	}
	return keys
}

func takeoverMetricCount(metrics *ports.RecordingExporter) int {
	return len(metrics.FindEntries(paho.MetricMQTTSessionTakeover))
}

type takeoverReconcilePump struct {
	mu       sync.Mutex
	err      error
	cancel   context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once
}

func startTakeoverReconcilePump(
	ctx context.Context,
	session *paho.Session,
	plan connectivity.SessionPlan,
) *takeoverReconcilePump {
	pumpCtx, cancel := context.WithCancel(ctx)
	pump := &takeoverReconcilePump{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(pump.done)
		for {
			select {
			case <-pumpCtx.Done():
				return
			case event, ok := <-session.Events():
				if !ok {
					return
				}
				if event.Type != ports.SessionConnected {
					continue
				}
				err := session.Reconcile(pumpCtx, plan)
				pump.mu.Lock()
				pump.err = err
				pump.mu.Unlock()
			}
		}
	}()
	return pump
}

func (p *takeoverReconcilePump) stop() {
	p.stopOnce.Do(func() {
		p.cancel()
		<-p.done
	})
}

func (p *takeoverReconcilePump) latestError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}
