package paho_test

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
)

type integrationManagedHistory struct {
	mu     sync.Mutex
	values map[string]map[string]struct{}
}

func (s *integrationManagedHistory) List(_ context.Context, identity string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	set, ok := s.values[identity]
	if !ok {
		return nil, shared.ErrNotFound
	}
	out := make([]string, 0, len(set))
	for filter := range set {
		out = append(out, filter)
	}
	sort.Strings(out)
	return out, nil
}
func (s *integrationManagedHistory) Remember(_ context.Context, identity string, filters []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	set, ok := s.values[identity]
	if !ok {
		set = map[string]struct{}{}
		s.values[identity] = set
	}
	for _, filter := range filters {
		set[filter] = struct{}{}
	}
	return nil
}
func (s *integrationManagedHistory) Forget(_ context.Context, identity string, filters []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	set, ok := s.values[identity]
	if !ok {
		return shared.ErrNotFound
	}
	for _, filter := range filters {
		delete(set, filter)
	}
	return nil
}
func (s *integrationManagedHistory) snapshot(identity string) []string {
	out, _ := s.List(context.Background(), identity)
	return out
}

func TestIntegration_ManagedHistoryRestartRemovesWildcardAndSharedFilters(t *testing.T) {
	brokerURL := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	clientID := mqttlocal.UniqueClientID("managed-restart")
	identity := "safe-" + clientID
	topicRoot := mqttlocal.UniqueClientID("managed-topic")
	ordinary := topicRoot + "/#"
	sharedFilter := "$share/managed-group/" + topicRoot + "/#"
	desired := topicRoot + "/new/#"
	history := &integrationManagedHistory{values: map[string]map[string]struct{}{identity: {}}}
	recorder := &ports.RecordingExporter{}
	factory := paho.NewFactory(nil, recorder)
	newDurable := func(t *testing.T) *paho.Session {
		t.Helper()
		raw, err := factory.NewSession(ctx, ports.SessionSpec{
			ID: "managed", Transport: "mqtt", SessionMode: connectivity.SessionPersistent,
			Config:                   paho.Config{Session: paho.SessionOptions{BrokerURLs: []string{brokerURL}, ClientID: clientID, ConnectTimeout: 5 * time.Second, KeepAlive: 10, UnmatchedGrace: 300 * time.Millisecond}},
			ManagedSubscriptionStore: history, ManagedSubscriptionIdentity: identity, ManagedSubscriptionsRequired: true,
		})
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		return raw.(*paho.Session)
	}

	first := newDurable(t)
	if err := first.Start(ctx); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if err := first.Reconcile(ctx, connectivity.SessionPlan{Subscriptions: []connectivity.SubscriptionPlan{{Topic: ordinary, QoS: 1}, {Topic: sharedFilter, QoS: 1}}}); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	if err := first.Close(ctx); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	replacement := newDurable(t)
	if err := replacement.Start(ctx); err != nil {
		t.Fatalf("replacement Start: %v", err)
	}
	plan := connectivity.SessionPlan{Subscriptions: []connectivity.SubscriptionPlan{{Topic: desired, QoS: 1}}}
	if err := replacement.Reconcile(ctx, plan); err != nil {
		t.Fatalf("replacement cleanup/recycle convergence: %v", err)
	}
	defer func() { _ = replacement.Close(context.Background()) }()
	if got := history.snapshot(identity); len(got) != 1 || got[0] != desired {
		t.Fatalf("history after restart = %v", got)
	}

	peer := paho.NewSession(paho.SessionOptions{BrokerURLs: []string{brokerURL}, ClientID: mqttlocal.UniqueClientID("managed-peer"), CleanStart: true, ConnectTimeout: 5 * time.Second, KeepAlive: 10}, connectivity.SessionEphemeral, nil)
	if err := peer.Start(ctx); err != nil {
		t.Fatalf("peer Start: %v", err)
	}
	defer func() { _ = peer.Close(context.Background()) }()
	if err := peer.Reconcile(ctx, connectivity.SessionPlan{Subscriptions: []connectivity.SubscriptionPlan{{Topic: sharedFilter, QoS: 1}}}); err != nil {
		t.Fatalf("peer Reconcile: %v", err)
	}
	recv := paho.NewReceiver("peer", peer, paho.WithTopicFilters(sharedFilter))
	received := make(chan struct{}, 12)
	recvCtx, recvCancel := context.WithCancel(ctx)
	defer recvCancel()
	go func() {
		_ = recv.Run(recvCtx, func(ctx context.Context, delivery ports.Delivery) error {
			received <- struct{}{}
			return delivery.Ack(ctx)
		})
	}()
	sender := paho.NewSender(peer, paho.SenderOptions{QoS: 1, Timeout: 5 * time.Second})
	const count = 12
	for i := 0; i < count; i++ {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{Subject: "managed-restart-proof", Payload: []byte{byte(i)}})
		if err := sender.Send(ctx, ports.OutboundMessage{Envelope: env, Address: topicRoot + "/old"}); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	for i := 0; i < count; i++ {
		select {
		case <-received:
		case <-ctx.Done():
			t.Fatalf("shared peer received %d/%d after stale-filter removal: %v", i, count, ctx.Err())
		}
	}
	if entries := recorder.FindEntries(paho.MetricMQTTRouterUnmatchedDropped); len(entries) != 0 {
		t.Fatalf("replacement ACK-dropped stale shared deliveries: %v", entries)
	}
}
