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
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	runtimesession "github.com/mariotoffia/gobridge/runtime/session"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

func TestMQTTSettlementRecovery(t *testing.T) {
	brokerURL := mqttlocal.BrokerURL(t)
	topic := "prod-ready/settlement-recovery/" + mqttlocal.UniqueClientID("topic")
	clientID := mqttlocal.UniqueClientID("settlement-recovery-bridge")
	const (
		originalID = "settlement-recovery-original"
		laterID    = "settlement-recovery-later"
		receiverID = "settlement-recovery-receiver"
	)

	metrics := newRecoveryBarrierMetrics()
	sess := paho.NewSession(paho.SessionOptions{
		BrokerURLs:            []string{brokerURL},
		ClientID:              clientID,
		ConnectTimeout:        5 * time.Second,
		ReconnectTimeout:      5 * time.Second,
		SessionExpiryInterval: 300,
		ReceiveMaximum:        4,
	}, connectivity.SessionPersistent, nil, metrics)
	innerReceiver := paho.NewReceiver(receiverID, sess, paho.WithTopicFilters(topic))
	receiver := newSettlementRecoveryReceiver(innerReceiver)
	sender := newSettlementRecoverySender(originalID)
	dlq := newUnavailableRecoveryDLQ()

	rt := goruntime.New(
		goruntime.WithInstanceID("settlement-recovery"),
		goruntime.WithDLQStore(dlq),
	)
	policy := routing.RoutePolicy{
		DeliveryMode:       routing.DeliveryDirectHold,
		OnPermanentFailure: routing.FailureDLQ,
		SendTimeout:        2 * time.Second,
		MaxReplayAttempts:  3,
	}
	route := goruntime.RouteConfig{
		ID:     "settlement-recovery-route",
		Policy: policy,
		Resolver: goruntime.NewStaticResolver(routing.DispatchPlan{
			BindingID: "settlement-recovery-binding",
			Address:   "sink",
		}),
		SourceCapabilities: directHoldCaps,
	}
	sessionCfg := runtimesession.DefaultConfig(clientID, false)
	sessionCfg.Plan = connectivity.SessionPlan{
		Subscriptions:       []connectivity.SubscriptionPlan{{Topic: topic, QoS: 1}},
		ExpectedReceiverIDs: []string{receiverID},
	}
	if err := rt.AddRoute(route, receiver, sender, sess, &sessionCfg); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
	wait.RequireReceive(t, receiver.Started(), 5*time.Second)
	wait.RequireReceive(t, metrics.reconciled, 5*time.Second)

	publishRecoveryMessage(t, brokerURL, topic, originalID)
	wait.RequireReceive(t, dlq.attempted, 5*time.Second)
	wait.RequireReceive(t, receiver.retryReturned, 5*time.Second)
	if id := wait.RequireReceive(t, receiver.received, 5*time.Second); id != originalID {
		t.Fatalf("initial producer ID = %q, want %q", id, originalID)
	}
	if got := sess.Health(ctx).ServiceLevel; got != ports.ServiceLevelDegraded {
		t.Fatalf("readiness after recovery request = %q, want degraded", got)
	}

	wait.RequireReceive(t, metrics.recycled, 10*time.Second)
	if id := wait.RequireReceive(t, receiver.received, 10*time.Second); id != originalID {
		t.Fatalf("redelivered producer ID = %q, want %q", id, originalID)
	}
	publishRecoveryMessage(t, brokerURL, topic, laterID)

	succeeded := map[string]int{}
	for len(succeeded) < 2 {
		id := wait.RequireReceive(t, sender.succeeded, 10*time.Second)
		succeeded[id]++
	}
	if succeeded[originalID] != 1 || succeeded[laterID] != 1 {
		t.Fatalf("successful producer-ID accounting = %v, want one original and one later", succeeded)
	}
	acked := map[string]int{}
	for len(acked) < 2 {
		id := wait.RequireReceive(t, receiver.acked, 10*time.Second)
		acked[id]++
	}
	if acked[originalID] != 1 || acked[laterID] != 1 {
		t.Fatalf("acknowledged producer-ID accounting = %v, want one original and one later", acked)
	}
	if got := receiver.receiptCounts(); got[originalID] != 2 || got[laterID] != 1 {
		t.Fatalf("receipt producer-ID accounting = %v, want original redelivered once and later delivered once", got)
	}
	if got := dlq.attemptCount(); got != 2 {
		t.Fatalf("DLQ attempts = %d, want exactly 2 bounded unavailable writes", got)
	}
	health := sess.Health(ctx)
	if health.ServiceLevel != ports.ServiceLevelFull || health.UnsettledCount != 0 {
		t.Fatalf("session after recovered progress = %+v, want Full with no unsettled packets", health)
	}
	if health.RecoveryRecycleCount != 1 {
		t.Fatalf("recovery recycle count = %d, want 1", health.RecoveryRecycleCount)
	}
}

func publishRecoveryMessage(t *testing.T, brokerURL, topic, id string) {
	t.Helper()
	publishRawMQTT(t, brokerURL, mqttlocal.UniqueClientID("settlement-recovery-publisher"), &pahov5.Publish{
		Topic:   topic,
		QoS:     1,
		Payload: []byte(id),
		Properties: &pahov5.PublishProperties{User: pahov5.UserProperties{{
			Key:   paho.HeaderMessageID,
			Value: id,
		}}},
	})
}

type settlementRecoveryReceiver struct {
	inner         *paho.Receiver
	retryReturned chan struct{}
	received      chan string
	acked         chan string
	retryOnce     sync.Once
	mu            sync.Mutex
	receipts      map[string]int
}

func newSettlementRecoveryReceiver(inner *paho.Receiver) *settlementRecoveryReceiver {
	return &settlementRecoveryReceiver{
		inner:         inner,
		retryReturned: make(chan struct{}),
		received:      make(chan string, 4),
		acked:         make(chan string, 4),
		receipts:      make(map[string]int),
	}
}

func (r *settlementRecoveryReceiver) Started() <-chan struct{} { return r.inner.Started() }

func (r *settlementRecoveryReceiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	return r.inner.Run(ctx, func(deliveryCtx context.Context, delivery ports.Delivery) error {
		r.mu.Lock()
		id := delivery.Envelope().ID()
		r.receipts[id]++
		r.mu.Unlock()
		r.received <- id
		return emit(deliveryCtx, &settlementRecoveryDelivery{Delivery: delivery, recovered: r})
	})
}

func (r *settlementRecoveryReceiver) receiptCounts() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int, len(r.receipts))
	for id, count := range r.receipts {
		out[id] = count
	}
	return out
}

type settlementRecoveryDelivery struct {
	ports.Delivery
	recovered *settlementRecoveryReceiver
}

func (d *settlementRecoveryDelivery) Ack(ctx context.Context) error {
	err := d.Delivery.Ack(ctx)
	if err == nil {
		d.recovered.acked <- d.Envelope().ID()
	}
	return err
}

func (d *settlementRecoveryDelivery) Retry(ctx context.Context, after time.Duration, reason error) error {
	err := d.Delivery.Retry(ctx, after, reason)
	if err == nil {
		d.recovered.retryOnce.Do(func() { close(d.recovered.retryReturned) })
	}
	return err
}

type settlementRecoverySender struct {
	failID    string
	mu        sync.Mutex
	attempts  map[string]int
	succeeded chan string
}

func newSettlementRecoverySender(failID string) *settlementRecoverySender {
	return &settlementRecoverySender{
		failID:    failID,
		attempts:  make(map[string]int),
		succeeded: make(chan string, 4),
	}
}

func (s *settlementRecoverySender) Send(_ context.Context, msg ports.OutboundMessage) error {
	id := msg.Envelope.ID()
	s.mu.Lock()
	s.attempts[id]++
	attempt := s.attempts[id]
	s.mu.Unlock()
	if id == s.failID && attempt == 1 {
		return shared.ErrInvalidPayload.WithMessage("forced permanent processing failure")
	}
	s.succeeded <- id
	return nil
}

type unavailableRecoveryDLQ struct {
	attempted chan struct{}
	once      sync.Once
	mu        sync.Mutex
	attempts  int
}

func newUnavailableRecoveryDLQ() *unavailableRecoveryDLQ {
	return &unavailableRecoveryDLQ{attempted: make(chan struct{})}
}

func (d *unavailableRecoveryDLQ) Write(context.Context, routing.DLQEntry) error {
	d.mu.Lock()
	d.attempts++
	d.mu.Unlock()
	d.once.Do(func() { close(d.attempted) })
	return shared.ErrUnavailable.WithMessage("forced DLQ outage")
}
func (*unavailableRecoveryDLQ) List(context.Context, routing.DLQFilter) ([]routing.DLQEntry, error) {
	return nil, nil
}
func (*unavailableRecoveryDLQ) Get(context.Context, string) (routing.DLQEntry, error) {
	return routing.DLQEntry{}, shared.ErrNotFound
}
func (*unavailableRecoveryDLQ) Delete(context.Context, []string) (int, error) { return 0, nil }
func (*unavailableRecoveryDLQ) DeleteByFilter(context.Context, routing.DLQFilter) (int, error) {
	return 0, nil
}
func (*unavailableRecoveryDLQ) Purge(context.Context, time.Time) (int, error) { return 0, nil }
func (d *unavailableRecoveryDLQ) attemptCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.attempts
}

type recoveryBarrierMetrics struct {
	*ports.RecordingExporter
	recycled   chan struct{}
	reconciled chan struct{}
	once       sync.Once
}

func newRecoveryBarrierMetrics() *recoveryBarrierMetrics {
	return &recoveryBarrierMetrics{RecordingExporter: &ports.RecordingExporter{}, recycled: make(chan struct{}), reconciled: make(chan struct{}, 4)}
}

func (m *recoveryBarrierMetrics) Timer(name string, value time.Duration, tags ...shared.Tag) {
	m.RecordingExporter.Timer(name, value, tags...)
	if name == paho.MetricMQTTReconcileLatency {
		m.reconciled <- struct{}{}
	}
}

func (m *recoveryBarrierMetrics) Counter(name string, value int64, tags ...shared.Tag) {
	m.RecordingExporter.Counter(name, value, tags...)
	if name == paho.MetricMQTTSessionRecoveryRecycle {
		m.once.Do(func() { close(m.recycled) })
	}
}

var _ ports.Receiver = (*settlementRecoveryReceiver)(nil)
var _ ports.Sender = (*settlementRecoverySender)(nil)
var _ ports.DLQStore = (*unavailableRecoveryDLQ)(nil)
var _ ports.MetricsExporter = (*recoveryBarrierMetrics)(nil)
