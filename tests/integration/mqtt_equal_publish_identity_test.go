package integration_test

import (
	"context"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/adapters/native/store/memorylease"
	"github.com/mariotoffia/gobridge/adapters/native/store/memoryoutbox"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/tests/testutil/prodid"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// TestMQTTEqualPublishIdentity proves the shared-outbox ingress contract against
// a real broker: equal-valued publishes without producer identity remain
// distinct, while an explicit producer identity remains stable across a
// publisher reconnect and is deduplicated.
func TestMQTTEqualPublishIdentity(t *testing.T) {
	brokerURL := mqttlocal.BrokerURL(t)
	topic := "prod-ready/equal-publish/" + mqttlocal.UniqueClientID("topic")

	bridgeSession := setupMQTTSession(t, mqttlocal.UniqueClientID("equal-bridge"), connectivity.SessionEphemeral)
	if err := bridgeSession.Reconcile(context.Background(), connectivity.SessionPlan{
		Subscriptions: []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	waitSubReady(t, bridgeSession)

	mqttReceiver := paho.NewReceiver("equal-rx", bridgeSession, paho.WithTopicFilters(topic))
	source := &ackCountingReceiver{inner: mqttReceiver}
	destination := newFakeSender()
	destinationSession := newFakeSession()
	const destinationSessionID = "equal-destination"

	outbox := memoryoutbox.NewStore()
	leases := memorylease.NewStore(memorylease.WithAcknowledgeSingleReplica(true))
	rt := goruntime.New(
		goruntime.WithInstanceID("equal-bridge"),
		goruntime.WithLeaseStore(leases),
		goruntime.WithOutboxStore(outbox),
		goruntime.WithDLQStore(&e2eDLQStore{}),
	)
	route := goruntime.RouteConfig{
		ID:       "equal-route",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliverySharedOutbox},
		Resolver: goruntime.NewStaticResolver(routing.DispatchPlan{BindingID: "equal-binding", Address: "output"}),
		Bindings: []routing.DestinationBinding{{ID: "equal-binding", SessionID: destinationSessionID}},
	}
	sessionConfig := e2eFastSessionConfig(destinationSessionID)
	if err := rt.AddRoute(route, source, destination, destinationSession, &sessionConfig); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	select {
	case <-mqttReceiver.Started():
	case <-time.After(5 * time.Second):
		t.Fatal("MQTT receiver did not start")
	}

	payload := []byte(`{"state":"on"}`)
	publishRawMQTT(t, brokerURL, mqttlocal.UniqueClientID("equal-publisher"),
		&pahov5.Publish{Topic: topic, QoS: 1, Payload: payload},
		&pahov5.Publish{Topic: topic, QoS: 1, Payload: payload},
	)

	wait.Until(t, 10*time.Second, "two source acknowledgements", func() bool { return source.count() == 2 })
	wait.Until(t, 10*time.Second, "two distinct shared-outbox outputs", func() bool { return destination.sentCount() == 2 })
	first := destination.getSent()
	if first[0].ID() == first[1].ID() {
		t.Fatalf("equal no-ID publishes collapsed to Envelope ID %q", first[0].ID())
	}

	producerID := "producer-event-42"
	accountant, err := prodid.New([]string{producerID}, false)
	if err != nil {
		t.Fatalf("new producer accountant: %v", err)
	}
	explicit := func() *pahov5.Publish {
		return &pahov5.Publish{
			Topic:   topic,
			QoS:     1,
			Payload: []byte(`{"state":"explicit"}`),
			Properties: &pahov5.PublishProperties{User: pahov5.UserProperties{
				{Key: paho.HeaderMessageID, Value: producerID},
			}},
		}
	}
	publisherID := mqttlocal.UniqueClientID("identity-redelivery")
	publishRawMQTT(t, brokerURL, publisherID, explicit())
	publishRawMQTT(t, brokerURL, publisherID, explicit())

	wait.Until(t, 10*time.Second, "explicit identity source acknowledgements", func() bool { return source.count() == 4 })
	wait.Until(t, 10*time.Second, "one explicit identity output", func() bool { return destination.sentCount() == 3 })
	sent := destination.getSent()
	if sent[2].ID() != producerID {
		t.Fatalf("explicit producer identity = %q, want %q", sent[2].ID(), producerID)
	}
	accountant.ObserveOutput(producerID, sent[2].ID())
	if report := accountant.Reconcile(); !report.Exact() {
		t.Fatalf("explicit producer-ID redelivery accounting failed: %s", report.String())
	}
}

type ackCountingReceiver struct {
	inner ports.Receiver
	acked atomic.Int64
}

func (r *ackCountingReceiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	return r.inner.Run(ctx, func(ctx context.Context, delivery ports.Delivery) error {
		return emit(ctx, &ackCountingDelivery{Delivery: delivery, acked: &r.acked})
	})
}

func (r *ackCountingReceiver) count() int64 { return r.acked.Load() }

type ackCountingDelivery struct {
	ports.Delivery
	acked *atomic.Int64
	once  sync.Once
}

func (d *ackCountingDelivery) Ack(ctx context.Context) error {
	err := d.Delivery.Ack(ctx)
	if err == nil {
		d.once.Do(func() { d.acked.Add(1) })
	}
	return err
}

func publishRawMQTT(t *testing.T, brokerURL, clientID string, publishes ...*pahov5.Publish) {
	t.Helper()
	endpoint, err := url.Parse(brokerURL)
	if err != nil {
		t.Fatalf("parse MQTT broker URL: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cm, err := autopaho.NewConnection(ctx, autopaho.ClientConfig{
		ServerUrls:                    []*url.URL{endpoint},
		KeepAlive:                     10,
		CleanStartOnInitialConnection: true,
		ClientConfig:                  pahov5.ClientConfig{ClientID: clientID},
	})
	if err != nil {
		t.Fatalf("new raw MQTT publisher: %v", err)
	}
	defer func() { _ = cm.Disconnect(context.Background()) }()
	if err := cm.AwaitConnection(ctx); err != nil {
		t.Fatalf("await raw MQTT publisher: %v", err)
	}
	for i, publish := range publishes {
		if _, err := cm.Publish(ctx, publish); err != nil {
			t.Fatalf("raw MQTT publish %d: %v", i, err)
		}
	}
}
