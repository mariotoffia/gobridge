package integration_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/adapters/mqtt/transport/paho"
	"github.com/mariotoffia/gobridge/adapters/native/store/sqlitemanagedsubscriptions"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/runtime/dlq"
	"github.com/mariotoffia/gobridge/runtime/session"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// A delivery the broker pinned to a persistent session for a shared filter the
// new configuration removes is written to the dead-letter store as
// SUBSCRIPTION_REMOVED and acknowledged; the migration converges instead of
// making the session terminal, and the broker does not replay it again.
func TestMQTTRemovedSubscriptionHeldDeliveryIsDeadLettered(t *testing.T) {
	brokerURL := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	sharedRoot := mqttlocal.UniqueClientID("removed-sub-dlq-shared")
	sharedFilter := "$share/" + mqttlocal.UniqueClientID("removed-sub-dlq-group") + "/" + sharedRoot + "/#"
	const unmatchedGrace = 300 * time.Millisecond
	ms := newManagedMQTTSession(ctx, t, brokerURL, "removed-sub-dlq", unmatchedGrace)
	store, identity, newSession := ms.store, ms.identity, ms.build

	old := newSession()
	if err := old.Start(ctx); err != nil {
		t.Fatalf("old session Start: %v", err)
	}
	oldPlan := connectivity.SessionPlan{Subscriptions: []connectivity.SubscriptionPlan{{Topic: sharedFilter, QoS: 1}}}
	if err := old.Reconcile(ctx, oldPlan); err != nil {
		t.Fatalf("old session Reconcile: %v", err)
	}
	if err := old.Close(ctx); err != nil {
		t.Fatalf("old session Close: %v", err)
	}

	// The runtime installs the same write: the dead-letter router over the store.
	dlqStore := &fakeDLQStore{}
	dlqRouter := dlq.NewFromConfig(dlq.Config{Store: dlqStore})
	cutover := newSession()
	cutover.SetRemovedSubscriptionDeadLetter(func(writeCtx context.Context, env *messaging.Envelope, filter string) error {
		return dlqRouter.Route(writeCtx, env, "", "", filter, "removed-sub-dlq", "", shared.ErrSubscriptionRemoved, 0)
	})
	if err := cutover.Start(ctx); err != nil {
		t.Fatalf("cutover session Start: %v", err)
	}
	t.Cleanup(func() { _ = cutover.Close(context.Background()) })

	publisher := setupMQTTSession(t, mqttlocal.UniqueClientID("removed-sub-dlq-publisher"), connectivity.SessionEphemeral)
	sender := setupMQTTSender(t, publisher)
	const heldID = "removed-sub-dlq-held-delivery"
	held := messaging.MustEnvelope(messaging.EnvelopeInput{ID: heldID, Subject: "removed-sub-dlq-proof", Payload: []byte(heldID)})
	if err := sender.Send(ctx, ports.OutboundMessage{Envelope: held, Address: sharedRoot + "/buffered"}); err != nil {
		t.Fatalf("publish held shared delivery: %v", err)
	}
	wait.Until(t, 5*time.Second, "cutover session holds the shared delivery", func() bool {
		received, _ := cutover.Router().Stats()
		return received == 1
	})

	if err := cutover.Reconcile(ctx, connectivity.SessionPlan{}); err != nil {
		t.Fatalf("removing the filter with a dead-letter store must converge: %v", err)
	}
	if history, listErr := store.List(ctx, identity); listErr != nil || len(history) != 0 {
		t.Fatalf("history after dead-lettered cutover = %v, err=%v; want empty", history, listErr)
	}
	if health := cutover.Health(ctx); !health.Connected {
		t.Fatalf("dead-lettered cutover left the session disconnected: %+v", health)
	}
	dlqStore.mu.Lock()
	entries := append([]routing.DLQEntry(nil), dlqStore.entries...)
	dlqStore.mu.Unlock()
	if len(entries) != 1 {
		t.Fatalf("DLQ entries after cutover = %d, want exactly the held delivery", len(entries))
	}
	entry := entries[0]
	if entry.ErrorCode() != string(shared.ErrCodeSubscriptionRemoved) || entry.Address() != sharedFilter {
		t.Fatalf("DLQ entry code/address = %q/%q, want %s/%s",
			entry.ErrorCode(), entry.Address(), shared.ErrCodeSubscriptionRemoved, sharedFilter)
	}
	if id := entry.Snapshot().ID(); id != heldID {
		t.Fatalf("DLQ entry envelope = %q, want %q", id, heldID)
	}
	if err := cutover.Close(ctx); err != nil {
		t.Fatalf("cutover session Close: %v", err)
	}

	// The delivery was acknowledged: a fresh instance of the same persistent
	// session receives no replay of it.
	resumed := newSession()
	if err := resumed.Start(ctx); err != nil {
		t.Fatalf("resumed session Start: %v", err)
	}
	t.Cleanup(func() { _ = resumed.Close(context.Background()) })
	counts := wait.StableFor(t, func() mqttRouterCounts {
		received, dropped := resumed.Router().Stats()
		return mqttRouterCounts{received: received, dropped: dropped}
	}, 2*unmatchedGrace, 3*time.Second)
	if counts != (mqttRouterCounts{}) {
		t.Fatalf("broker replayed the dead-lettered delivery: %+v", counts)
	}
}

// A runtime with no lease store writes a delivery the broker queued for a filter
// the configuration removed to the dead-letter store. The session is registered
// the way the builder registers one a binding names: exclusive, its connect
// deferred until it holds a lease. Nothing can grant that lease, so a write
// fenced on it was refused and the session retried the removal forever.
func TestMQTTRemovedSubscriptionQueuedDeliveryIsDeadLetteredWithoutLeaseStore(t *testing.T) {
	brokerURL := mqttlocal.BrokerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	const sessionID = "leaseless-removed-sub"
	root := mqttlocal.UniqueClientID("leaseless-removed-sub-topic")
	filter := root + "/#"
	ms := newManagedMQTTSession(ctx, t, brokerURL, sessionID, 300*time.Millisecond)

	// An earlier configuration subscribed the filter and went away; the broker
	// keeps its persistent session and queues what is published meanwhile.
	old := ms.build()
	if err := old.Start(ctx); err != nil {
		t.Fatalf("old session Start: %v", err)
	}
	if err := old.Reconcile(ctx, connectivity.SessionPlan{Subscriptions: []connectivity.SubscriptionPlan{{Topic: filter, QoS: 1}}}); err != nil {
		t.Fatalf("old session Reconcile: %v", err)
	}
	if err := old.Close(ctx); err != nil {
		t.Fatalf("old session Close: %v", err)
	}
	publisher := setupMQTTSession(t, mqttlocal.UniqueClientID("leaseless-removed-sub-publisher"), connectivity.SessionEphemeral)
	const queuedID = "leaseless-removed-sub-queued"
	queued := messaging.MustEnvelope(messaging.EnvelopeInput{ID: queuedID, Subject: "leaseless-removed-sub", Payload: []byte(queuedID)})
	if err := setupMQTTSender(t, publisher).Send(ctx, ports.OutboundMessage{Envelope: queued, Address: root + "/queued"}); err != nil {
		t.Fatalf("publish queued delivery: %v", err)
	}

	// The new configuration's plan no longer has the filter.
	dlqStore := &fakeDLQStore{}
	rt := goruntime.New(goruntime.WithInstanceID(sessionID), goruntime.WithDLQStore(dlqStore))
	sess := ms.build()
	sessCfg := session.Config{SessionID: sessionID, Exclusive: true, ConnectAfterLease: true}
	if err := rt.RegisterSessionSender(sessCfg, sess, setupMQTTSender(t, sess)); err != nil {
		t.Fatalf("RegisterSessionSender: %v", err)
	}
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("runtime Start: %v", err)
	}
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })

	var entry routing.DLQEntry
	wait.Until(t, 20*time.Second, "the queued delivery is dead-lettered", func() bool {
		dlqStore.mu.Lock()
		defer dlqStore.mu.Unlock()
		if len(dlqStore.entries) == 0 {
			return false
		}
		entry = dlqStore.entries[0]
		return true
	})
	if entry.ErrorCode() != string(shared.ErrCodeSubscriptionRemoved) || entry.Address() != filter || entry.SessionID() != sessionID {
		t.Fatalf("DLQ entry code/address/session = %q/%q/%q, want %s/%s/%s",
			entry.ErrorCode(), entry.Address(), entry.SessionID(), shared.ErrCodeSubscriptionRemoved, filter, sessionID)
	}
	if id := entry.Snapshot().ID(); id != queuedID {
		t.Fatalf("DLQ entry envelope = %q, want %q", id, queuedID)
	}
}

// managedMQTTSession builds instances of one persistent MQTT session whose
// managed subscription history lives in a store seeded with an empty baseline,
// so every instance resumes the same broker session and its history.
type managedMQTTSession struct {
	store    *sqlitemanagedsubscriptions.Store
	identity string
	build    func() *paho.Session
}

func newManagedMQTTSession(ctx context.Context, t *testing.T, brokerURL, sessionID string, unmatchedGrace time.Duration) managedMQTTSession {
	t.Helper()
	cfg := paho.Config{Session: paho.SessionOptions{
		BrokerURLs:            []string{brokerURL},
		ClientID:              mqttlocal.UniqueClientID(sessionID),
		ConnectTimeout:        5 * time.Second,
		KeepAlive:             10,
		SessionExpiryInterval: 300,
		UnmatchedGrace:        unmatchedGrace,
	}}
	identity, err := cfg.DurableSessionIdentity(connectivity.SessionPersistent)
	if err != nil {
		t.Fatalf("derive durable identity: %v", err)
	}
	storePath := filepath.Join(t.TempDir(), "managed-subscriptions", "managed-subscriptions.db")
	store, err := sqlitemanagedsubscriptions.NewStore(storePath)
	if err != nil {
		t.Fatalf("open managed-subscription store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Remember(ctx, identity, nil); err != nil {
		t.Fatalf("seed managed baseline: %v", err)
	}
	build := func() *paho.Session {
		raw, buildErr := paho.NewFactory(nil, nil).NewSession(ctx, ports.SessionSpec{
			ID:                           sessionID,
			Transport:                    "mqtt",
			SessionMode:                  connectivity.SessionPersistent,
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
	return managedMQTTSession{store: store, identity: identity, build: build}
}
