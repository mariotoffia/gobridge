package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

func makeDrainer(t *testing.T, token domain.LeaseToken, opts ...func(*goruntime.OutboxDrainerConfig)) (*FakeOutboxStore, *FakeSender, *FakeDLQStore, *goruntime.OutboxDrainer) {
	t.Helper()
	outbox := NewFakeOutboxStore()
	sender := NewFakeSender()
	dlqStore := NewFakeDLQStore()
	leaseStore := NewFakeLeaseStore()

	pk := domain.OutboxPartitionKey("sess-1", "")
	_, _ = leaseStore.Acquire(context.Background(), "sess-1", token.Owner, 30*time.Second)

	cfg := goruntime.OutboxDrainerConfig{
		OutboxStore:   outbox,
		LeaseStore:    leaseStore,
		Sender:        sender,
		DLQ:           goruntime.NewDLQRouter(dlqStore),
		RouteID:       "route-1",
		PartitionKey:  pk,
		LeaseID:       "sess-1",
		OwnerID:       token.Owner,
		Policy:        domain.RoutePolicy{}.WithDefaults(),
		Strategy:  domain.NewFixedPoll(50 * time.Millisecond),
		BatchSize: 100,
		TokenFn: func() (domain.LeaseToken, bool) {
			return token, true
		},
	}
	for _, o := range opts {
		o(&cfg)
	}
	drainer := goruntime.NewOutboxDrainerFromConfig(cfg)
	return outbox, sender, dlqStore, drainer
}

// TestOutboxDrainer_HappyPath verifies a pending outbox record is sent and marked completed.
func TestOutboxDrainer_HappyPath(t *testing.T) {
	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox, sender, _, drainer := makeDrainer(t, token)

	ctx := context.Background()
	rec := domain.OutboxRecord{
		ID:         "rec-1",
		RouteID:    "route-1",
		EnvelopeID: "env-1",
		BindingID:  "bind-1",
		SessionID:  "sess-1",
		Envelope:   domain.Envelope{ID: "env-1", Payload: []byte("data")},
		Status:     domain.OutboxPending,
	}
	_ = outbox.Persist(ctx, []domain.OutboxRecord{rec})

	drainCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_ = drainer.Run(drainCtx)

	if sender.SentCount() != 1 {
		t.Fatalf("expected 1 sent, got %d", sender.SentCount())
	}
	if outbox.CompletedCount() != 1 {
		t.Fatalf("expected 1 completed, got %d", outbox.CompletedCount())
	}
}

// TestOutboxDrainer_ExpiredRecord verifies expired records skip send and are DLQed.
func TestOutboxDrainer_ExpiredRecord(t *testing.T) {
	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox, sender, dlqStore, drainer := makeDrainer(t, token)

	ctx := context.Background()
	rec := domain.OutboxRecord{
		ID:         "rec-exp",
		RouteID:    "route-1",
		EnvelopeID: "env-exp",
		BindingID:  "bind-1",
		SessionID:  "sess-1",
		Envelope:   domain.Envelope{ID: "env-exp", ExpiresAt: time.Now().Add(-time.Second)},
		Status:     domain.OutboxPending,
	}
	_ = outbox.Persist(ctx, []domain.OutboxRecord{rec})

	drainCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_ = drainer.Run(drainCtx)

	if sender.SentCount() != 0 {
		t.Fatal("expired record should not be sent")
	}
	if dlqStore.Count() != 1 {
		t.Fatalf("expected 1 DLQ entry, got %d", dlqStore.Count())
	}
}

// TestOutboxDrainer_PoisonMessage verifies replay count above max sends to DLQ without sending.
func TestOutboxDrainer_PoisonMessage(t *testing.T) {
	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox, sender, dlqStore, drainer := makeDrainer(t, token, func(cfg *goruntime.OutboxDrainerConfig) {
		cfg.Policy.MaxReplayAttempts = 2
	})

	ctx := context.Background()
	rec := domain.OutboxRecord{
		ID:          "rec-poison",
		RouteID:     "route-1",
		EnvelopeID:  "env-poison",
		BindingID:   "bind-1",
		SessionID:   "sess-1",
		Envelope:    domain.Envelope{ID: "env-poison", Payload: []byte("bad")},
		Status:      domain.OutboxPending,
		ReplayCount: 3,
	}
	_ = outbox.Persist(ctx, []domain.OutboxRecord{rec})

	drainCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_ = drainer.Run(drainCtx)

	if sender.SentCount() != 0 {
		t.Fatal("poison message should not be sent")
	}
	if dlqStore.Count() != 1 {
		t.Fatalf("expected 1 DLQ entry for poison, got %d", dlqStore.Count())
	}
}

// TestOutboxDrainer_StaleFencingToken verifies Run returns an error when the lease token is stale.
func TestOutboxDrainer_StaleFencingToken(t *testing.T) {
	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox, _, _, drainer := makeDrainer(t, token, func(cfg *goruntime.OutboxDrainerConfig) {
		staleToken := domain.LeaseToken{Version: 99, Owner: "bridge-1"}
		cfg.TokenFn = func() (domain.LeaseToken, bool) {
			return staleToken, true
		}
	})

	ctx := context.Background()
	rec := domain.OutboxRecord{
		ID:         "rec-stale",
		RouteID:    "route-1",
		EnvelopeID: "env-stale",
		BindingID:  "bind-1",
		SessionID:  "sess-1",
		Envelope:   domain.Envelope{ID: "env-stale"},
		Status:     domain.OutboxPending,
	}
	_ = outbox.Persist(ctx, []domain.OutboxRecord{rec})

	drainCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	err := drainer.Run(drainCtx)

	if err == nil {
		t.Fatal("expected stale fencing token error")
	}
}

// TestOutboxDrainer_NoLease verifies draining does not send when no lease token is available.
func TestOutboxDrainer_NoLease(t *testing.T) {
	outbox := NewFakeOutboxStore()
	sender := NewFakeSender()
	dlqStore := NewFakeDLQStore()

	cfg := goruntime.OutboxDrainerConfig{
		OutboxStore:   outbox,
		Sender:        sender,
		DLQ:           goruntime.NewDLQRouter(dlqStore),
		RouteID:       "route-1",
		PartitionKey:  domain.OutboxPartitionKey("sess-1", ""),
		OwnerID:       "bridge-1",
		Policy:   domain.RoutePolicy{}.WithDefaults(),
		Strategy: domain.NewFixedPoll(50 * time.Millisecond),
		TokenFn: func() (domain.LeaseToken, bool) {
			return domain.LeaseToken{}, false
		},
	}
	drainer := goruntime.NewOutboxDrainerFromConfig(cfg)

	ctx := context.Background()
	rec := domain.OutboxRecord{
		ID: "rec-nolease", RouteID: "route-1", EnvelopeID: "env-nolease",
		BindingID: "bind-1", SessionID: "sess-1",
		Envelope: domain.Envelope{ID: "env-nolease"}, Status: domain.OutboxPending,
	}
	_ = outbox.Persist(ctx, []domain.OutboxRecord{rec})

	drainCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_ = drainer.Run(drainCtx)

	if sender.SentCount() != 0 {
		t.Fatal("should not send when lease is not held")
	}
}

// TestOutboxDrainer_AppliesAddress verifies the record address overrides the envelope subject on send.
func TestOutboxDrainer_AppliesAddress(t *testing.T) {
	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox, sender, _, drainer := makeDrainer(t, token)

	ctx := context.Background()
	rec := domain.OutboxRecord{
		ID:         "rec-addr",
		RouteID:    "route-1",
		EnvelopeID: "env-addr",
		BindingID:  "bind-1",
		SessionID:  "sess-1",
		Address:    "factory/a/orders/42",
		Envelope:   domain.Envelope{ID: "env-addr", Subject: "original-subject", Payload: []byte("data")},
		Status:     domain.OutboxPending,
	}
	_ = outbox.Persist(ctx, []domain.OutboxRecord{rec})

	drainCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_ = drainer.Run(drainCtx)

	if sender.SentCount() != 1 {
		t.Fatalf("expected 1 sent, got %d", sender.SentCount())
	}
	if sender.Sent[0].Subject != "factory/a/orders/42" {
		t.Fatalf("expected subject from outbox record address, got %q", sender.Sent[0].Subject)
	}
}

// TestOutboxDrainer_EmptyAddressPreservesSubject verifies an empty record address keeps the original subject.
func TestOutboxDrainer_EmptyAddressPreservesSubject(t *testing.T) {
	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox, sender, _, drainer := makeDrainer(t, token)

	ctx := context.Background()
	rec := domain.OutboxRecord{
		ID:         "rec-noaddr",
		RouteID:    "route-1",
		EnvelopeID: "env-noaddr",
		BindingID:  "bind-1",
		SessionID:  "sess-1",
		Address:    "",
		Envelope:   domain.Envelope{ID: "env-noaddr", Subject: "original", Payload: []byte("data")},
		Status:     domain.OutboxPending,
	}
	_ = outbox.Persist(ctx, []domain.OutboxRecord{rec})

	drainCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_ = drainer.Run(drainCtx)

	if sender.SentCount() != 1 {
		t.Fatalf("expected 1 sent, got %d", sender.SentCount())
	}
	if sender.Sent[0].Subject != "original" {
		t.Fatalf("empty address should preserve original subject, got %q", sender.Sent[0].Subject)
	}
}

// TestOutboxDrainer_PermanentSendError verifies permanent send failure produces a DLQ entry.
func TestOutboxDrainer_PermanentSendError(t *testing.T) {
	token := domain.LeaseToken{Version: 1, Owner: "bridge-1"}
	outbox, sender, dlqStore, drainer := makeDrainer(t, token)
	sender.SendErr = domain.ErrNotAuthorized

	ctx := context.Background()
	rec := domain.OutboxRecord{
		ID: "rec-perm", RouteID: "route-1", EnvelopeID: "env-perm",
		BindingID: "bind-1", SessionID: "sess-1",
		Envelope: domain.Envelope{ID: "env-perm"}, Status: domain.OutboxPending,
	}
	_ = outbox.Persist(ctx, []domain.OutboxRecord{rec})

	drainCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_ = drainer.Run(drainCtx)

	if dlqStore.Count() != 1 {
		t.Fatalf("expected 1 DLQ entry for permanent error, got %d", dlqStore.Count())
	}
}
