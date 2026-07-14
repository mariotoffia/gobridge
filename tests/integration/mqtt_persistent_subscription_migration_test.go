package integration_test

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/adapters/native/store/sqlitemanagedsubscriptions"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

type mqttRouterCounts struct {
	received int64
	dropped  int64
}

func TestMQTTPersistentSubscriptionMigrationReleasesWildcardAndSharedFilters(t *testing.T) {
	brokerURL := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	clientID := mqttlocal.UniqueClientID("managed-migration")
	topicRoot := mqttlocal.UniqueClientID("managed-migration-topic")
	ordinaryFilter := topicRoot + "/#"
	sharedFilter := "$share/" + mqttlocal.UniqueClientID("managed-migration-group") + "/" + topicRoot + "/#"
	storePath := filepath.Join(t.TempDir(), "managed-subscriptions.db")
	const unmatchedGrace = 300 * time.Millisecond
	sessionConfig := paho.Config{Session: paho.SessionOptions{
		BrokerURLs:            []string{brokerURL},
		ClientID:              clientID,
		ConnectTimeout:        5 * time.Second,
		KeepAlive:             10,
		SessionExpiryInterval: 300,
		UnmatchedGrace:        unmatchedGrace,
	}}
	storageIdentity, err := sessionConfig.DurableSessionIdentity(connectivity.SessionPersistent)
	if err != nil {
		t.Fatalf("derive managed-subscription identity: %v", err)
	}

	newBridgeSession := func(t *testing.T, store ports.ManagedSubscriptionStore, metrics ports.MetricsExporter) *paho.Session {
		t.Helper()
		factory := paho.NewFactory(nil, metrics)
		raw, err := factory.NewSession(ctx, ports.SessionSpec{
			ID:                           "persistent-migration",
			Transport:                    "mqtt",
			SessionMode:                  connectivity.SessionPersistent,
			Config:                       sessionConfig,
			ManagedSubscriptionStore:     store,
			ManagedSubscriptionIdentity:  storageIdentity,
			ManagedSubscriptionsRequired: true,
		})
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		return raw.(*paho.Session)
	}

	firstStore, err := sqlitemanagedsubscriptions.NewStore(storePath)
	if err != nil {
		t.Fatalf("open first managed-subscription store: %v", err)
	}
	if err := firstStore.Remember(ctx, storageIdentity, nil); err != nil {
		t.Fatalf("seed managed-subscription baseline: %v", err)
	}
	firstSession := newBridgeSession(t, firstStore, nil)
	if err := firstSession.Start(ctx); err != nil {
		t.Fatalf("first persistent session Start: %v", err)
	}
	oldPlan := connectivity.SessionPlan{Subscriptions: []connectivity.SubscriptionPlan{
		{Topic: ordinaryFilter, QoS: 1},
		{Topic: sharedFilter, QoS: 1},
	}}
	if err := firstSession.Reconcile(ctx, oldPlan); err != nil {
		t.Fatalf("first persistent session Reconcile: %v", err)
	}
	if err := firstSession.Close(ctx); err != nil {
		t.Fatalf("close first persistent session: %v", err)
	}
	if err := firstStore.Close(); err != nil {
		t.Fatalf("close first managed-subscription store: %v", err)
	}

	replacementStore, err := sqlitemanagedsubscriptions.NewStore(storePath)
	if err != nil {
		t.Fatalf("reopen managed-subscription store: %v", err)
	}
	t.Cleanup(func() { _ = replacementStore.Close() })
	persistedHistory, err := replacementStore.List(ctx, storageIdentity)
	if err != nil {
		t.Fatalf("list managed history after process-instance teardown: %v", err)
	}
	if want := []string{sharedFilter, ordinaryFilter}; !slices.Equal(persistedHistory, want) {
		t.Fatalf("managed history after process-instance teardown = %v, want %v", persistedHistory, want)
	}
	recorder := &ports.RecordingExporter{}
	replacement := newBridgeSession(t, replacementStore, recorder)
	if err := replacement.Start(ctx); err != nil {
		t.Fatalf("replacement persistent session Start: %v", err)
	}
	t.Cleanup(func() { _ = replacement.Close(context.Background()) })
	if err := replacement.Reconcile(ctx, connectivity.SessionPlan{}); err == nil {
		t.Fatal("stale filter removal must recycle the resumed broker connection")
	}
	if err := replacement.Reconcile(ctx, connectivity.SessionPlan{}); err != nil {
		t.Fatalf("replacement post-recycle Reconcile: %v", err)
	}
	if history, err := replacementStore.List(ctx, storageIdentity); err != nil || len(history) != 0 {
		t.Fatalf("managed history after replacement = %v, err=%v; want empty", history, err)
	}

	peer := setupMQTTSession(t, mqttlocal.UniqueClientID("managed-migration-peer"), connectivity.SessionEphemeral)
	if err := peer.Reconcile(ctx, connectivity.SessionPlan{Subscriptions: []connectivity.SubscriptionPlan{{Topic: sharedFilter, QoS: 1}}}); err != nil {
		t.Fatalf("shared peer Reconcile: %v", err)
	}
	peerReceiver := paho.NewReceiver("managed-migration-peer", peer, paho.WithTopicFilters(sharedFilter))
	const messageCount = 20
	peerReceived := make(chan struct{}, messageCount)
	receiverCtx, receiverCancel := context.WithCancel(ctx)
	t.Cleanup(receiverCancel)
	go func() {
		_ = peerReceiver.Run(receiverCtx, func(deliveryCtx context.Context, delivery ports.Delivery) error {
			if err := delivery.Ack(deliveryCtx); err != nil {
				return err
			}
			peerReceived <- struct{}{}
			return nil
		})
	}()
	wait.RequireReceive(t, peerReceiver.Started(), 5*time.Second)

	publisher := setupMQTTSession(t, mqttlocal.UniqueClientID("managed-migration-publisher"), connectivity.SessionEphemeral)
	sender := setupMQTTSender(t, publisher)
	for i := 0; i < messageCount; i++ {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      fmt.Sprintf("managed-migration-%d", i),
			Subject: "managed-migration-proof",
			Payload: []byte{byte(i)},
		})
		if err := sender.Send(ctx, ports.OutboundMessage{Envelope: env, Address: topicRoot + "/old"}); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	for i := 0; i < messageCount; i++ {
		wait.RequireReceive(t, peerReceived, 10*time.Second)
	}

	counts := wait.StableFor(t, func() mqttRouterCounts {
		received, dropped := replacement.Router().Stats()
		return mqttRouterCounts{received: received, dropped: dropped}
	}, 2*unmatchedGrace, 3*time.Second)
	if counts != (mqttRouterCounts{}) {
		t.Fatalf("replacement consumed stale-filter traffic: %+v", counts)
	}
	if entries := recorder.FindEntries(paho.MetricMQTTRouterUnmatchedDropped); len(entries) != 0 {
		t.Fatalf("replacement ACK-dropped stale deliveries: %v", entries)
	}
}
