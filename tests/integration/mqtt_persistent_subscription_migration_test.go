package integration_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/adapters/native/store/memorylease"
	"github.com/mariotoffia/gobridge/adapters/native/store/sqlitemanagedsubscriptions"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	runtimesession "github.com/mariotoffia/gobridge/runtime/session"
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
	storePath := filepath.Join(t.TempDir(), "managed-subscriptions", "managed-subscriptions.db")
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
	if err := replacement.Reconcile(ctx, connectivity.SessionPlan{}); err != nil {
		t.Fatalf("stale cleanup/recycle must converge the replacement generation: %v", err)
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

func TestMQTTExclusiveDefaultProfileNoBufferMigrationConvergesWithinLease(t *testing.T) {
	brokerURL := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
	t.Cleanup(cancel)

	clientID := mqttlocal.UniqueClientID("managed-default-exclusive")
	filter := mqttlocal.UniqueClientID("managed-default-exclusive-topic") + "/#"
	storePath := filepath.Join(t.TempDir(), "managed-subscriptions", "managed-subscriptions.db")
	cfg := paho.DefaultConfig()
	cfg.Session.BrokerURLs = []string{brokerURL}
	cfg.Session.ClientID = clientID
	cfg.Session.SessionExpiryInterval = 300
	identity, err := cfg.DurableSessionIdentity(connectivity.SessionExclusive)
	if err != nil {
		t.Fatalf("derive exclusive durable identity: %v", err)
	}

	store, err := sqlitemanagedsubscriptions.NewStore(storePath)
	if err != nil {
		t.Fatalf("open managed-subscription store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Remember(ctx, identity, nil); err != nil {
		t.Fatalf("seed managed baseline: %v", err)
	}
	newSession := func() *paho.Session {
		raw, buildErr := paho.NewFactory(nil, nil).NewSession(ctx, ports.SessionSpec{
			ID:                           "managed-default-exclusive",
			Transport:                    "mqtt",
			SessionMode:                  connectivity.SessionExclusive,
			Config:                       cfg,
			ManagedSubscriptionStore:     store,
			ManagedSubscriptionIdentity:  identity,
			ManagedSubscriptionsRequired: true,
		})
		if buildErr != nil {
			t.Fatalf("NewSession: %v", buildErr)
		}
		return raw.(*paho.Session)
	}

	old := newSession()
	if err := old.Start(ctx); err != nil {
		t.Fatalf("old exclusive session Start: %v", err)
	}
	if err := old.Reconcile(ctx, connectivity.SessionPlan{Subscriptions: []connectivity.SubscriptionPlan{{Topic: filter, QoS: 1}}}); err != nil {
		t.Fatalf("old exclusive session Reconcile: %v", err)
	}
	if err := old.Close(ctx); err != nil {
		t.Fatalf("old exclusive session Close: %v", err)
	}

	replacement := newSession()
	leaseStore := memorylease.NewStore(memorylease.WithAcknowledgeSingleReplica(true))
	managerCfg := runtimesession.HAConfig("managed-default-exclusive", true)
	managerCfg.ConnectAfterLease = true
	managerCfg.Plan = connectivity.SessionPlan{}
	manager := runtimesession.NewWithMetrics(managerCfg, replacement, leaseStore, "owner-default-exclusive", nil, &ports.NoopExporter{}, clock.System)
	runErr := make(chan error, 1)
	go func() { runErr <- manager.Run(ctx) }()
	t.Cleanup(func() { _ = manager.Close(context.Background()) })

	var activationErr error
	wait.Until(t, 45*time.Second, "default exclusive managed migration convergence", func() bool {
		select {
		case activationErr = <-runErr:
			return true
		default:
		}
		history, listErr := store.List(ctx, identity)
		return listErr == nil && len(history) == 0 && replacement.Health(ctx).ServiceLevel == ports.ServiceLevelFull
	})
	if activationErr != nil {
		t.Fatalf("default exclusive activation failed before 30s replay grace converged: %v", activationErr)
	}
	if history, listErr := store.List(ctx, identity); listErr != nil || len(history) != 0 {
		t.Fatalf("managed history after default exclusive migration = %v, err=%v; want empty", history, listErr)
	}

	cancel()
	if err := wait.RequireReceive(t, runErr, 5*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("manager Run after convergence cancellation = %v, want context.Canceled", err)
	}
}

func TestMQTTPersistentSubscriptionMigrationPinnedSharedDeliveryRequiresRestoreDrainRetry(t *testing.T) {
	brokerURL := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	clientID := mqttlocal.UniqueClientID("managed-pinned-migration")
	ordinaryRoot := mqttlocal.UniqueClientID("managed-pinned-ordinary")
	sharedRoot := mqttlocal.UniqueClientID("managed-pinned-shared")
	ordinaryFilter := ordinaryRoot + "/#"
	sharedFilter := "$share/" + mqttlocal.UniqueClientID("managed-pinned-group") + "/" + sharedRoot + "/#"
	storePath := filepath.Join(t.TempDir(), "managed-subscriptions", "managed-subscriptions.db")
	const unmatchedGrace = 300 * time.Millisecond
	cfg := paho.Config{Session: paho.SessionOptions{
		BrokerURLs:            []string{brokerURL},
		ClientID:              clientID,
		ConnectTimeout:        5 * time.Second,
		KeepAlive:             10,
		SessionExpiryInterval: 300,
		UnmatchedGrace:        unmatchedGrace,
	}}
	identity, err := cfg.DurableSessionIdentity(connectivity.SessionPersistent)
	if err != nil {
		t.Fatalf("derive durable identity: %v", err)
	}
	store, err := sqlitemanagedsubscriptions.NewStore(storePath)
	if err != nil {
		t.Fatalf("open managed-subscription store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Remember(ctx, identity, nil); err != nil {
		t.Fatalf("seed managed baseline: %v", err)
	}

	newSession := func(metrics ports.MetricsExporter) *paho.Session {
		factory := paho.NewFactory(nil, metrics)
		raw, err := factory.NewSession(ctx, ports.SessionSpec{
			ID:                           "persistent-pinned-migration",
			Transport:                    "mqtt",
			SessionMode:                  connectivity.SessionPersistent,
			Config:                       cfg,
			ManagedSubscriptionStore:     store,
			ManagedSubscriptionIdentity:  identity,
			ManagedSubscriptionsRequired: true,
		})
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		return raw.(*paho.Session)
	}
	oldPlan := connectivity.SessionPlan{Subscriptions: []connectivity.SubscriptionPlan{
		{Topic: ordinaryFilter, QoS: 1},
		{Topic: sharedFilter, QoS: 1},
	}}

	old := newSession(nil)
	if err := old.Start(ctx); err != nil {
		t.Fatalf("old session Start: %v", err)
	}
	if err := old.Reconcile(ctx, oldPlan); err != nil {
		t.Fatalf("old session Reconcile: %v", err)
	}
	if err := old.Close(ctx); err != nil {
		t.Fatalf("old session Close: %v", err)
	}

	recorder := &ports.RecordingExporter{}
	failedCutover := newSession(recorder)
	if err := failedCutover.Start(ctx); err != nil {
		t.Fatalf("failed-cutover session Start: %v", err)
	}

	publisher := setupMQTTSession(t, mqttlocal.UniqueClientID("managed-pinned-publisher"), connectivity.SessionEphemeral)
	sender := setupMQTTSender(t, publisher)
	const pinnedID = "managed-pinned-shared-delivery"
	pinned := messaging.MustEnvelope(messaging.EnvelopeInput{
		ID: pinnedID, Subject: "managed-pinned-proof", Payload: []byte(pinnedID),
	})
	if err := sender.Send(ctx, ports.OutboundMessage{Envelope: pinned, Address: sharedRoot + "/buffered"}); err != nil {
		t.Fatalf("publish pinned shared delivery: %v", err)
	}
	wait.Until(t, 5*time.Second, "stale member receives pinned shared delivery", func() bool {
		received, _ := failedCutover.Router().Stats()
		return received == 1
	})

	err = failedCutover.Reconcile(ctx, connectivity.SessionPlan{})
	if !errors.Is(err, shared.ErrTransportClosedPermanently) {
		t.Fatalf("pinned migration error = %v, want terminal migration-required marker", err)
	}
	if !strings.Contains(err.Error(), "restore the old configuration") {
		t.Fatalf("pinned migration error lacks restore/drain/retry guidance: %v", err)
	}
	if history, listErr := store.List(ctx, identity); listErr != nil || !slices.Equal(history, []string{sharedFilter, ordinaryFilter}) {
		t.Fatalf("history after failed cutover = %v, err=%v; want preserved exact filters", history, listErr)
	}
	if health := failedCutover.Health(ctx); health.ServiceLevel == ports.ServiceLevelFull || health.Connected {
		t.Fatalf("failed cutover claimed usable readiness: %+v", health)
	}
	if _, dropped := failedCutover.Router().Stats(); dropped != 0 {
		t.Fatalf("failed cutover dropped %d pinned deliveries", dropped)
	}
	if entries := recorder.FindEntries(paho.MetricMQTTRouterUnmatchedDropped); len(entries) != 0 {
		t.Fatalf("failed cutover ACK-dropped pinned delivery: %v", entries)
	}
	if err := failedCutover.Close(ctx); err != nil {
		t.Fatalf("failed cutover Close: %v", err)
	}

	// Restore the exact old plan and handler on a fresh process/session instance.
	// The broker-pinned QoS1 delivery resumes on the same ClientID and is settled
	// once before cutover is retried.
	restored := newSession(nil)
	if err := restored.Start(ctx); err != nil {
		t.Fatalf("restored old-config session Start: %v", err)
	}
	restoredReceiver := paho.NewReceiver("restored-old-handler", restored, paho.WithTopicFilters(sharedFilter))
	restoredReceived := make(chan string, 4)
	restoredCtx, restoredCancel := context.WithCancel(ctx)
	go func() {
		_ = restoredReceiver.Run(restoredCtx, func(deliveryCtx context.Context, delivery ports.Delivery) error {
			id := delivery.Envelope().ID()
			if err := delivery.Ack(deliveryCtx); err != nil {
				return err
			}
			restoredReceived <- id
			return nil
		})
	}()
	wait.RequireReceive(t, restoredReceiver.Started(), 5*time.Second)
	if err := restored.Reconcile(ctx, oldPlan); err != nil {
		t.Fatalf("restore old config Reconcile: %v", err)
	}
	if id := wait.RequireReceive(t, restoredReceived, 10*time.Second); id != pinnedID {
		t.Fatalf("restored handler drained %q, want %q", id, pinnedID)
	}
	wait.Silent(t, restoredReceived, 2*unmatchedGrace)
	restoredCancel()
	if err := restored.Close(ctx); err != nil {
		t.Fatalf("restored old-config session Close: %v", err)
	}

	// Join the shared peer only after the pinned delivery has been drained. A
	// broker may otherwise redistribute an unacknowledged shared delivery on the
	// first recycle, so that sequence cannot portably prove the fail-closed path.
	peer := setupMQTTSession(t, mqttlocal.UniqueClientID("managed-pinned-peer"), connectivity.SessionEphemeral)
	if err := peer.Reconcile(ctx, connectivity.SessionPlan{Subscriptions: []connectivity.SubscriptionPlan{{Topic: sharedFilter, QoS: 1}}}); err != nil {
		t.Fatalf("peer Reconcile: %v", err)
	}
	peerReceiver := paho.NewReceiver("managed-pinned-peer", peer, paho.WithTopicFilters(sharedFilter))
	peerReceived := make(chan string, 64)
	peerCtx, peerCancel := context.WithCancel(ctx)
	t.Cleanup(peerCancel)
	go func() {
		_ = peerReceiver.Run(peerCtx, func(deliveryCtx context.Context, delivery ports.Delivery) error {
			id := delivery.Envelope().ID()
			if err := delivery.Ack(deliveryCtx); err != nil {
				return err
			}
			peerReceived <- id
			return nil
		})
	}()
	wait.RequireReceive(t, peerReceiver.Started(), 5*time.Second)

	// Retry the removal after the old handler drained the broker-pinned delivery.
	retry := newSession(nil)
	if err := retry.Start(ctx); err != nil {
		t.Fatalf("retry session Start: %v", err)
	}
	t.Cleanup(func() { _ = retry.Close(context.Background()) })
	if err := retry.Reconcile(ctx, connectivity.SessionPlan{}); err != nil {
		t.Fatalf("drained migration retry: %v", err)
	}
	if history, listErr := store.List(ctx, identity); listErr != nil || len(history) != 0 {
		t.Fatalf("history after successful retry = %v, err=%v; want empty", history, listErr)
	}

	const liveCount = 20
	for i := 0; i < liveCount; i++ {
		id := fmt.Sprintf("managed-pinned-live-%d", i)
		env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: id, Subject: "managed-pinned-proof", Payload: []byte(id)})
		if err := sender.Send(ctx, ports.OutboundMessage{Envelope: env, Address: sharedRoot + "/live"}); err != nil {
			t.Fatalf("publish post-cutover shared %d: %v", i, err)
		}
	}
	seen := make(map[string]int, liveCount)
	for i := 0; i < liveCount; i++ {
		id := wait.RequireReceive(t, peerReceived, 10*time.Second)
		seen[id]++
	}
	for i := 0; i < liveCount; i++ {
		id := fmt.Sprintf("managed-pinned-live-%d", i)
		if seen[id] != 1 {
			t.Fatalf("peer post-cutover accounting for %q = %d, all=%v", id, seen[id], seen)
		}
	}

	ordinary := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "managed-pinned-ordinary-proof", Subject: "managed-pinned-proof", Payload: []byte("ordinary")})
	if err := sender.Send(ctx, ports.OutboundMessage{Envelope: ordinary, Address: ordinaryRoot + "/old"}); err != nil {
		t.Fatalf("publish post-cutover ordinary proof: %v", err)
	}
	counts := wait.StableFor(t, func() mqttRouterCounts {
		received, dropped := retry.Router().Stats()
		return mqttRouterCounts{received: received, dropped: dropped}
	}, 2*unmatchedGrace, 3*time.Second)
	if counts != (mqttRouterCounts{}) {
		t.Fatalf("retry session stole post-cutover traffic: %+v", counts)
	}
}
