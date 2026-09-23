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
	"github.com/mariotoffia/gobridge/runtime/dlq"
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

	clientID := mqttlocal.UniqueClientID("removed-sub-dlq")
	sharedRoot := mqttlocal.UniqueClientID("removed-sub-dlq-shared")
	sharedFilter := "$share/" + mqttlocal.UniqueClientID("removed-sub-dlq-group") + "/" + sharedRoot + "/#"
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
	newSession := func() *paho.Session {
		raw, buildErr := paho.NewFactory(nil, nil).NewSession(ctx, ports.SessionSpec{
			ID:                           "removed-sub-dlq",
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
