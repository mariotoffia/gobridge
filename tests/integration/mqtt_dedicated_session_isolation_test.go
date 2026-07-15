package integration_test

import (
	"context"
	"sync"
	"testing"
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

const mqttDedicatedSessionIsolationBound = 5 * time.Second

// TestMQTTDedicatedSessionIsolation proves one blocked MQTT ingress route cannot
// pin another route when each receiver owns a distinct broker session.
func TestMQTTDedicatedSessionIsolation(t *testing.T) {
	const (
		blockedID  = "dedicated-session-blocked"
		isolatedID = "dedicated-session-isolated"
	)
	brokerURL := mqttlocal.BrokerURL(t)
	topicPrefix := "prod-ready/dedicated-session/" + mqttlocal.UniqueClientID("topic")
	blockedTopic := topicPrefix + "/blocked"
	isolatedTopic := topicPrefix + "/isolated"

	blockedSession := setupMQTTSession(t, mqttlocal.UniqueClientID("dedicated-blocked"), connectivity.SessionEphemeral)
	isolatedSession := setupMQTTSession(t, mqttlocal.UniqueClientID("dedicated-isolated"), connectivity.SessionEphemeral)
	for _, item := range []struct {
		name    string
		session *paho.Session
		topic   string
	}{
		{name: "blocked", session: blockedSession, topic: blockedTopic},
		{name: "isolated", session: isolatedSession, topic: isolatedTopic},
	} {
		if err := item.session.Reconcile(t.Context(), connectivity.SessionPlan{
			Subscriptions: []connectivity.SubscriptionPlan{{Topic: item.topic, QoS: 1}},
		}); err != nil {
			t.Fatalf("Reconcile %s session: %v", item.name, err)
		}
		waitSubReady(t, item.session)
	}

	blockedReceiver := newDedicatedIsolationReceiver(
		paho.NewReceiver("dedicated-blocked-receiver", blockedSession, paho.WithTopicFilters(blockedTopic)),
	)
	isolatedReceiver := newDedicatedIsolationReceiver(
		paho.NewReceiver("dedicated-isolated-receiver", isolatedSession, paho.WithTopicFilters(isolatedTopic)),
	)
	blockedSender := newDedicatedIsolationBarrierSender()
	isolatedSender := newDedicatedIsolationRecordingSender()
	t.Cleanup(blockedSender.release)

	rt := goruntime.New(
		goruntime.WithInstanceID("dedicated-session-isolation"),
		goruntime.WithDLQStore(&e2eDLQStore{}),
	)
	addDedicatedIsolationRoute := func(
		routeID string,
		receiver ports.Receiver,
		sender ports.Sender,
	) {
		t.Helper()
		err := rt.AddRoute(goruntime.RouteConfig{
			ID: routeID,
			Policy: routing.RoutePolicy{
				DeliveryMode: routing.DeliveryDirectHold,
				MaxInFlight:  1,
				SendTimeout:  2 * mqttDedicatedSessionIsolationBound,
			},
			Resolver:           goruntime.NewStaticResolver(routing.DispatchPlan{BindingID: routeID + "-binding", Address: "sink"}),
			SourceCapabilities: directHoldCaps,
		}, receiver, sender, nil, nil)
		if err != nil {
			t.Fatalf("AddRoute %s: %v", routeID, err)
		}
	}
	addDedicatedIsolationRoute("dedicated-blocked-route", blockedReceiver, blockedSender)
	addDedicatedIsolationRoute("dedicated-isolated-route", isolatedReceiver, isolatedSender)

	runCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := rt.Start(runCtx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		blockedSender.release()
		stopCtx, stopCancel := context.WithTimeout(context.Background(), mqttDedicatedSessionIsolationBound)
		defer stopCancel()
		if err := rt.Stop(stopCtx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})

	readyCtx, readyCancel := context.WithTimeout(t.Context(), mqttDedicatedSessionIsolationBound)
	defer readyCancel()
	requireDedicatedIsolationSignal(t, readyCtx, "blocked receiver readiness", blockedReceiver.inner.Started())
	requireDedicatedIsolationSignal(t, readyCtx, "isolated receiver readiness", isolatedReceiver.inner.Started())

	publishDedicatedIsolationMessage(t, brokerURL, blockedTopic, blockedID)
	proofCtx, proofCancel := context.WithTimeout(t.Context(), mqttDedicatedSessionIsolationBound)
	defer proofCancel()
	requireDedicatedIsolationID(t, proofCtx, "blocked route receive", blockedReceiver.received, blockedID)
	requireDedicatedIsolationID(t, proofCtx, "blocked route barrier", blockedSender.entered, blockedID)

	publishDedicatedIsolationMessage(t, brokerURL, isolatedTopic, isolatedID)
	requireDedicatedIsolationID(t, proofCtx, "isolated route receive", isolatedReceiver.received, isolatedID)
	requireDedicatedIsolationID(t, proofCtx, "isolated route send", isolatedSender.sent, isolatedID)
	requireDedicatedIsolationID(t, proofCtx, "isolated route settlement", isolatedReceiver.settled, isolatedID)
	requireDedicatedIsolationFull(t, proofCtx, "isolated session while blocked route is held", isolatedSession)
	requireDedicatedIsolationCounts(t, "blocked route receive while held", blockedReceiver.receivedCounts.snapshot(), map[string]int{blockedID: 1})
	requireDedicatedIsolationCounts(t, "blocked route sender while held", blockedSender.sentCounts.snapshot(), nil)
	requireDedicatedIsolationCounts(t, "blocked route settlement while held", blockedReceiver.settledCounts.snapshot(), nil)
	requireDedicatedIsolationCounts(t, "isolated route receive while blocked route is held", isolatedReceiver.receivedCounts.snapshot(), map[string]int{isolatedID: 1})
	requireDedicatedIsolationCounts(t, "isolated route sender success while blocked route is held", isolatedSender.sentCounts.snapshot(), map[string]int{isolatedID: 1})
	requireDedicatedIsolationCounts(t, "isolated route source ACK while blocked route is held", isolatedReceiver.settledCounts.snapshot(), map[string]int{isolatedID: 1})

	select {
	case got := <-blockedReceiver.settled:
		t.Fatalf("blocked route settled %q before its deterministic barrier was released", got)
	default:
	}

	blockedSender.release()
	requireDedicatedIsolationID(t, proofCtx, "released blocked route send", blockedSender.sent, blockedID)
	requireDedicatedIsolationID(t, proofCtx, "released blocked route settlement", blockedReceiver.settled, blockedID)
	requireDedicatedIsolationCounts(t, "released blocked route receive", blockedReceiver.receivedCounts.snapshot(), map[string]int{blockedID: 1})
	requireDedicatedIsolationCounts(t, "released blocked route sender success", blockedSender.sentCounts.snapshot(), map[string]int{blockedID: 1})
	requireDedicatedIsolationCounts(t, "released blocked route source ACK", blockedReceiver.settledCounts.snapshot(), map[string]int{blockedID: 1})
	requireDedicatedIsolationCounts(t, "isolated route receive after release", isolatedReceiver.receivedCounts.snapshot(), map[string]int{isolatedID: 1})
	requireDedicatedIsolationCounts(t, "isolated route sender success after release", isolatedSender.sentCounts.snapshot(), map[string]int{isolatedID: 1})
	requireDedicatedIsolationCounts(t, "isolated route source ACK after release", isolatedReceiver.settledCounts.snapshot(), map[string]int{isolatedID: 1})
	requireDedicatedIsolationFull(t, proofCtx, "blocked session after release", blockedSession)
	requireDedicatedIsolationFull(t, proofCtx, "isolated session after release", isolatedSession)
}

func publishDedicatedIsolationMessage(t *testing.T, brokerURL, topic, id string) {
	t.Helper()
	publishRawMQTT(t, brokerURL, mqttlocal.UniqueClientID("dedicated-isolation-publisher"), &pahov5.Publish{
		Topic:   topic,
		QoS:     1,
		Payload: []byte(id),
		Properties: &pahov5.PublishProperties{User: pahov5.UserProperties{{
			Key:   paho.HeaderMessageID,
			Value: id,
		}}},
	})
}

func requireDedicatedIsolationSignal(t *testing.T, ctx context.Context, phase string, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatalf("%s exceeded configured isolation bound: %v", phase, ctx.Err())
	}
}

func requireDedicatedIsolationID(t *testing.T, ctx context.Context, phase string, values <-chan string, want string) {
	t.Helper()
	select {
	case got := <-values:
		if got != want {
			t.Fatalf("%s ID = %q, want %q", phase, got, want)
		}
	case <-ctx.Done():
		t.Fatalf("%s exceeded configured isolation bound: %v", phase, ctx.Err())
	}
}

func requireDedicatedIsolationFull(t *testing.T, ctx context.Context, phase string, session *paho.Session) {
	t.Helper()
	health := session.Health(ctx)
	if health.ServiceLevel != ports.ServiceLevelFull || health.UnsettledCount != 0 {
		t.Fatalf("%s health = %+v, want Full with no unsettled deliveries", phase, health)
	}
}

func requireDedicatedIsolationCounts(t *testing.T, phase string, got, want map[string]int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s counts = %v, want %v (duplicate or unexpected ID)", phase, got, want)
	}
	for id, wantCount := range want {
		if got[id] != wantCount {
			t.Fatalf("%s counts = %v, want %v (duplicate or unexpected ID)", phase, got, want)
		}
	}
}

type dedicatedIsolationCounter struct {
	mu   sync.Mutex
	byID map[string]int
}

func (c *dedicatedIsolationCounter) record(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byID == nil {
		c.byID = make(map[string]int)
	}
	c.byID[id]++
}

func (c *dedicatedIsolationCounter) snapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.byID))
	for id, count := range c.byID {
		out[id] = count
	}
	return out
}

type dedicatedIsolationReceiver struct {
	inner          *paho.Receiver
	received       chan string
	settled        chan string
	receivedCounts dedicatedIsolationCounter
	settledCounts  dedicatedIsolationCounter
}

func newDedicatedIsolationReceiver(inner *paho.Receiver) *dedicatedIsolationReceiver {
	return &dedicatedIsolationReceiver{
		inner:    inner,
		received: make(chan string, 2),
		settled:  make(chan string, 2),
	}
}

func (r *dedicatedIsolationReceiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	return r.inner.Run(ctx, func(deliveryCtx context.Context, delivery ports.Delivery) error {
		id := delivery.Envelope().ID()
		r.receivedCounts.record(id)
		r.received <- id
		return emit(deliveryCtx, &dedicatedIsolationDelivery{
			Delivery: delivery,
			settled:  r.settled,
			counts:   &r.settledCounts,
		})
	})
}

type dedicatedIsolationDelivery struct {
	ports.Delivery
	settled chan<- string
	counts  *dedicatedIsolationCounter
	once    sync.Once
}

func (d *dedicatedIsolationDelivery) Ack(ctx context.Context) error {
	if err := d.Delivery.Ack(ctx); err != nil {
		return err
	}
	d.once.Do(func() {
		id := d.Envelope().ID()
		d.counts.record(id)
		d.settled <- id
	})
	return nil
}

type dedicatedIsolationBarrierSender struct {
	entered     chan string
	sent        chan string
	sentCounts  dedicatedIsolationCounter
	releaseGate chan struct{}
	releaseOnce sync.Once
}

func newDedicatedIsolationBarrierSender() *dedicatedIsolationBarrierSender {
	return &dedicatedIsolationBarrierSender{
		entered:     make(chan string, 2),
		sent:        make(chan string, 2),
		releaseGate: make(chan struct{}),
	}
}

func (s *dedicatedIsolationBarrierSender) Send(ctx context.Context, msg ports.OutboundMessage) error {
	s.entered <- msg.Envelope.ID()
	select {
	case <-s.releaseGate:
		id := msg.Envelope.ID()
		s.sentCounts.record(id)
		s.sent <- id
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *dedicatedIsolationBarrierSender) release() {
	s.releaseOnce.Do(func() { close(s.releaseGate) })
}

type dedicatedIsolationRecordingSender struct {
	sent       chan string
	sentCounts dedicatedIsolationCounter
}

func newDedicatedIsolationRecordingSender() *dedicatedIsolationRecordingSender {
	return &dedicatedIsolationRecordingSender{sent: make(chan string, 2)}
}

func (s *dedicatedIsolationRecordingSender) Send(_ context.Context, msg ports.OutboundMessage) error {
	id := msg.Envelope.ID()
	s.sentCounts.record(id)
	s.sent <- id
	return nil
}
