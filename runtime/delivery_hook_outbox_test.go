package runtime_test

// ═══════════════════════════════════════════════════════════════════════
// Delivery Hook Tests — SharedOutbox / OutboxDrainer
//
// Validates the DeliveryHook lifecycle for SharedOutbox delivery mode.
// Covers egress OnAttempt calls during drain, terminal OnSettled events
// for success, poison, expired, permanent-error, and transient cases.
//
// Drainer hook flow:
//
//   Claim ──▶ processRecord ──▶ sender.Send
//                  │                  │
//                  │   OnAttempt(egress, err) ◀──┘
//                  │                  │
//                  │   OnSettled(egress)       ◀──┘ (complete / DLQ)
//                  │
//                  ├── handleExpired ──▶ OnSettled(expired)
//                  └── handlePoison  ──▶ OnSettled(poison)
//
// ═══════════════════════════════════════════════════════════════════════

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"
	outboxpkg "github.com/mariotoffia/gobridge/runtime/outbox"
)

// ---------------------------------------------------------------------------
// SharedOutbox / OutboxDrainer hook tests
// ---------------------------------------------------------------------------

// TestDeliveryHook_SharedOutbox_Success validates OnAttempt fires with
// egress direction and nil error, then OnSettled fires with nil error
// and Terminal=true when a record is successfully drained and completed.
func TestDeliveryHook_SharedOutbox_Success(t *testing.T) {
	hook := &recordingHook{}
	token := persistence.LeaseToken{Version: 1, Owner: "owner-1"}
	outbox, _, _, drainer := makeDrainer(t, token, func(cfg *outboxpkg.Config) {
		cfg.Hook = hook
	})

	env := *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "outbox-msg-1", Payload: []byte("payload")})
	_ = outbox.Persist(context.Background(), []*persistence.OutboxRecord{persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
		ID: "rec-1", RouteID: "route-1", EnvelopeID: env.ID(),
		BindingID: "bind-1", SessionID: "sess-1",
		Envelope: env, Status: persistence.OutboxPending, CreatedAt: time.Now(),
	})})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = drainer.Run(ctx) }()

	waitFor(t, 2*time.Second, "hook settled", func() bool { return hook.SettledCount() >= 1 })
	cancel()

	attempts := hook.Attempts()
	settled := hook.Settled()

	if len(attempts) < 1 {
		t.Fatalf("expected >= 1 egress attempt, got %d", len(attempts))
	}
	if attempts[0].Direction != ports.DirectionEgress {
		t.Errorf("direction = %s, want egress", attempts[0].Direction)
	}
	if attempts[0].Err != nil {
		t.Errorf("attempt Err = %v, want nil", attempts[0].Err)
	}
	if attempts[0].BindingID != "bind-1" {
		t.Errorf("attempt BindingID = %q, want bind-1", attempts[0].BindingID)
	}
	if len(settled) < 1 {
		t.Fatalf("expected >= 1 settled, got %d", len(settled))
	}
	if settled[0].Err != nil {
		t.Errorf("settled Err = %v, want nil", settled[0].Err)
	}
	if !settled[0].Terminal {
		t.Error("settled Terminal should be true")
	}
}

// TestDeliveryHook_SharedOutbox_Poison validates OnSettled fires with a
// poison error when the record's ReplayCount exceeds MaxReplayAttempts.
func TestDeliveryHook_SharedOutbox_Poison(t *testing.T) {
	hook := &recordingHook{}
	token := persistence.LeaseToken{Version: 1, Owner: "owner-1"}
	outbox, _, _, drainer := makeDrainer(t, token, func(cfg *outboxpkg.Config) {
		cfg.Hook = hook
		cfg.Policy.MaxReplayAttempts = 1
	})

	env := *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "poison-msg", Payload: []byte("payload")})
	_ = outbox.Persist(context.Background(), []*persistence.OutboxRecord{persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
		ID: "rec-poison", RouteID: "route-1", EnvelopeID: env.ID(),
		BindingID: "bind-1", SessionID: "sess-1",
		Envelope: env, ReplayCount: 5,
		Status: persistence.OutboxPending, CreatedAt: time.Now(),
	})})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = drainer.Run(ctx) }()

	waitFor(t, 2*time.Second, "hook settled", func() bool { return hook.SettledCount() >= 1 })
	cancel()

	settled := hook.Settled()
	if len(settled) < 1 {
		t.Fatalf("expected >= 1 settled for poison, got %d", len(settled))
	}
	if settled[0].Err == nil {
		t.Error("settled should carry poison error")
	}
	if !settled[0].Terminal {
		t.Error("settled should be terminal")
	}
	// FakeOutboxStore.Claim increments ReplayCount, so initial 5 becomes
	// 6 after claim, and Attempt = ReplayCount+1 = 7.
	if settled[0].Attempt != 7 {
		t.Errorf("settled Attempt = %d, want (claimed ReplayCount)+1=7", settled[0].Attempt)
	}
}

// TestDeliveryHook_SharedOutbox_Expired validates OnSettled fires with
// ErrMessageExpired when a record's envelope has expired before send.
func TestDeliveryHook_SharedOutbox_Expired(t *testing.T) {
	hook := &recordingHook{}
	token := persistence.LeaseToken{Version: 1, Owner: "owner-1"}
	outbox, _, _, drainer := makeDrainer(t, token, func(cfg *outboxpkg.Config) {
		cfg.Hook = hook
	})

	expEnv := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "expired-msg", Payload: []byte("old"), CreatedAt: time.Now().Add(-2 * time.Hour)})
	_ = expEnv.SetExpiry(time.Now().Add(-1 * time.Hour))
	env := *expEnv
	_ = outbox.Persist(context.Background(), []*persistence.OutboxRecord{persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
		ID: "rec-expired", RouteID: "route-1", EnvelopeID: env.ID(),
		BindingID: "bind-1", SessionID: "sess-1",
		Envelope: env, Status: persistence.OutboxPending, CreatedAt: time.Now(),
	})})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = drainer.Run(ctx) }()

	waitFor(t, 2*time.Second, "hook settled", func() bool { return hook.SettledCount() >= 1 })
	cancel()

	for _, a := range hook.Attempts() {
		if a.Direction == ports.DirectionEgress {
			t.Error("expired record should not fire egress OnAttempt")
		}
	}

	settled := hook.Settled()
	if len(settled) < 1 {
		t.Fatalf("expected >= 1 settled for expired, got %d", len(settled))
	}
	if settled[0].Err == nil {
		t.Error("settled should carry ErrMessageExpired")
	}
	if !settled[0].Terminal {
		t.Error("settled should be terminal")
	}
}

// TestDeliveryHook_SharedOutbox_PermanentSendError validates OnSettled
// fires with the permanent send error after DLQ routing.
func TestDeliveryHook_SharedOutbox_PermanentSendError(t *testing.T) {
	hook := &recordingHook{}
	permErr := shared.NewBridgeError("PERM", shared.ErrorPermanent, "perm fail")
	token := persistence.LeaseToken{Version: 1, Owner: "owner-1"}
	outbox, sender, _, drainer := makeDrainer(t, token, func(cfg *outboxpkg.Config) {
		cfg.Hook = hook
	})
	sender.SendErr = permErr

	env := *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "perm-msg", Payload: []byte("fail")})
	_ = outbox.Persist(context.Background(), []*persistence.OutboxRecord{persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
		ID: "rec-perm", RouteID: "route-1", EnvelopeID: env.ID(),
		BindingID: "bind-1", SessionID: "sess-1",
		Envelope: env, Status: persistence.OutboxPending, CreatedAt: time.Now(),
	})})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = drainer.Run(ctx) }()

	waitFor(t, 2*time.Second, "hook settled", func() bool { return hook.SettledCount() >= 1 })
	cancel()

	settled := hook.Settled()
	if len(settled) < 1 {
		t.Fatalf("expected >= 1 settled for permanent error, got %d", len(settled))
	}
	if settled[0].Err == nil {
		t.Error("settled should carry permanent error")
	}
	if !settled[0].Terminal {
		t.Error("settled should be terminal")
	}
}

// TestDeliveryHook_SharedOutbox_TransientNoSettled validates that a
// transient send failure fires OnAttempt but does NOT fire OnSettled
// because the record will be retried on the next drain cycle.
func TestDeliveryHook_SharedOutbox_TransientNoSettled(t *testing.T) {
	hook := &recordingHook{}
	transientErr := shared.NewBridgeError("TRANSIENT", shared.ErrorTransient, "try again")
	token := persistence.LeaseToken{Version: 1, Owner: "owner-1"}
	outbox, sender, _, drainer := makeDrainer(t, token, func(cfg *outboxpkg.Config) {
		cfg.Hook = hook
	})
	sender.SendErr = transientErr

	env := *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "transient-msg", Payload: []byte("retry")})
	_ = outbox.Persist(context.Background(), []*persistence.OutboxRecord{persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
		ID: "rec-transient", RouteID: "route-1", EnvelopeID: env.ID(),
		BindingID: "bind-1", SessionID: "sess-1",
		Envelope: env, Status: persistence.OutboxPending, CreatedAt: time.Now(),
	})})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	go func() { _ = drainer.Run(ctx) }()

	waitFor(t, 400*time.Millisecond, "at least 1 attempt", func() bool {
		return hook.AttemptCount() >= 1
	})
	cancel()

	if hook.SettledCount() != 0 {
		t.Errorf("expected 0 settled for transient failure, got %d", hook.SettledCount())
	}
	egressFound := false
	for _, a := range hook.Attempts() {
		if a.Direction == ports.DirectionEgress && a.Err != nil {
			egressFound = true
		}
	}
	if !egressFound {
		t.Error("expected at least one egress OnAttempt with error")
	}
}

// TestDeliveryHook_SharedOutbox_AttemptIsReplayCountPlusOne validates
// that the Attempt field on drainer hook events equals ReplayCount+1.
func TestDeliveryHook_SharedOutbox_AttemptIsReplayCountPlusOne(t *testing.T) {
	hook := &recordingHook{}
	token := persistence.LeaseToken{Version: 1, Owner: "owner-1"}
	outbox, _, _, drainer := makeDrainer(t, token, func(cfg *outboxpkg.Config) {
		cfg.Hook = hook
	})

	env := *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "replay-msg", Payload: []byte("replay")})
	_ = outbox.Persist(context.Background(), []*persistence.OutboxRecord{persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
		ID: "rec-replay", RouteID: "route-1", EnvelopeID: env.ID(),
		BindingID: "bind-1", SessionID: "sess-1",
		Envelope: env, ReplayCount: 3,
		Status: persistence.OutboxPending, CreatedAt: time.Now(),
	})})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = drainer.Run(ctx) }()

	waitFor(t, 2*time.Second, "hook settled", func() bool { return hook.SettledCount() >= 1 })
	cancel()

	// FakeOutboxStore.Claim increments ReplayCount, so initial 3 becomes
	// 4 after claim, and Attempt = ReplayCount+1 = 5.
	for _, a := range hook.Attempts() {
		if a.Direction == ports.DirectionEgress {
			if a.Attempt != 5 {
				t.Errorf("Attempt = %d, want (claimed ReplayCount)+1=5", a.Attempt)
			}
			return
		}
	}
	t.Error("no egress OnAttempt found")
}

// TestDeliveryHook_SharedOutbox_MultipleBatchRecords validates that
// each record in a drain batch fires its own independent hook calls.
func TestDeliveryHook_SharedOutbox_MultipleBatchRecords(t *testing.T) {
	hook := &recordingHook{}
	token := persistence.LeaseToken{Version: 1, Owner: "owner-1"}
	outbox, _, _, drainer := makeDrainer(t, token, func(cfg *outboxpkg.Config) {
		cfg.Hook = hook
	})

	for i := range 3 {
		env := *messaging.MustEnvelope(messaging.EnvelopeInput{
			ID:      "batch-" + string(rune('A'+i)),
			Payload: []byte("data"),
		})
		_ = outbox.Persist(context.Background(), []*persistence.OutboxRecord{persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
			ID: "rec-batch-" + string(rune('A'+i)), RouteID: "route-1",
			EnvelopeID: env.ID(), BindingID: "bind-1", SessionID: "sess-1",
			Envelope: env, Status: persistence.OutboxPending, CreatedAt: time.Now(),
		})})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = drainer.Run(ctx) }()

	waitFor(t, 2*time.Second, "all 3 settled", func() bool { return hook.SettledCount() >= 3 })
	cancel()

	if hook.AttemptCount() < 3 {
		t.Errorf("expected >= 3 egress attempts, got %d", hook.AttemptCount())
	}
	if hook.SettledCount() < 3 {
		t.Errorf("expected >= 3 settled, got %d", hook.SettledCount())
	}
}

// ---------------------------------------------------------------------------
// Builder / runtime.Option registration test
// ---------------------------------------------------------------------------

// TestDeliveryHook_Builder_RegisterPropagates validates that
// WithDeliveryHook on the runtime propagates the hook into RouteRunners
// created during Start().
func TestDeliveryHook_Builder_RegisterPropagates(t *testing.T) {
	var called atomic.Bool
	hook := &funcHook{
		onAttempt: func(_ context.Context, _ ports.DeliveryAttempt) { called.Store(true) },
		onSettled: func(_ context.Context, _ ports.DeliveryOutcome) {},
	}

	rt := runtime.New(
		runtime.WithInstanceID("test-hook-prop"),
		runtime.WithDeliveryHook(hook),
		runtime.WithDLQStore(NewFakeDLQStore()),
	)

	cfg := runtime.RouteConfig{
		ID: "hook-route",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliveryDirectHold,
		},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension},
	}
	receiver := NewFakeReceiver()

	if err := rt.AddRoute(cfg, receiver, NewFakeSender(), nil, nil); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	del := NewFakeDelivery(messaging.MustEnvelope(messaging.EnvelopeInput{ID: "prop-msg", Payload: []byte("test")}))
	if err := receiver.Emit(ctx, del); err != nil {
		t.Fatalf("emit: %v", err)
	}
	waitFor(t, time.Second, "delivery acked", func() bool { return del.IsAcked() })

	if !called.Load() {
		t.Error("hook OnAttempt was not called — hook did not propagate")
	}
	_ = rt.Stop(context.Background())
}
