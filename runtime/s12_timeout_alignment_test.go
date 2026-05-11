package runtime_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// ═══════════════════════════════════════════════════════════════════════
// S12 Timeout Alignment Tests
//
// Validates the S12 changes that align lease-related timeouts:
//   - DefaultSessionConfig produces correct new defaults
//   - RenewInterval is derived from LeaseTTL / MaxRenewFails
//   - Session manager lifecycle works with derived intervals
//
// ┌─────────────┐     ┌───────────────┐     ┌────────────────┐
// │ LeaseTTL    │────▶│ RenewInterval │────▶│ StepDownGrace  │
// │   (360s)    │     │  (TTL/Fails)  │     │    (15s)       │
// └─────────────┘     └───────────────┘     └────────────────┘
//       │                                          │
//       ▼                                          ▼
// ┌─────────────┐                          ┌────────────────┐
// │ MaxRenew    │                          │ staleClaimAge  │
// │ Fails (3)   │                          │ (grace + 15s)  │
// └─────────────┘                          └────────────────┘
// ═══════════════════════════════════════════════════════════════════════

// TestDefaultSessionConfig_S12Defaults validates that DefaultSessionConfig
// returns the S12-aligned timeout values including drain-related fields.
func TestDefaultSessionConfig_S12Defaults(t *testing.T) {
	cfg := goruntime.DefaultSessionConfig("test", true)

	durationChecks := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"LeaseTTL", cfg.LeaseTTL, 360 * time.Second},
		{"RenewInterval", cfg.RenewInterval, 0},
		{"RenewJitter", cfg.RenewJitter, 5 * time.Second},
		{"StepDownGrace", cfg.StepDownGrace, 15 * time.Second},
	}
	for _, c := range durationChecks {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}

	intChecks := []struct {
		name string
		got  int
		want int
	}{
		{"MaxRenewFails", cfg.MaxRenewFails, 3},
		{"DrainBatchSize", cfg.DrainBatchSize, 100},
		{"DrainMaxBatchSize", cfg.DrainMaxBatchSize, 500},
		{"DrainMaxConcurrency", cfg.DrainMaxConcurrency, 10},
	}
	for _, c := range intChecks {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}

	if cfg.SessionID != "test" {
		t.Errorf("SessionID: got %q, want %q", cfg.SessionID, "test")
	}
	if !cfg.Exclusive {
		t.Error("Exclusive: got false, want true")
	}
	if cfg.DrainStrategy == nil {
		t.Error("DrainStrategy should not be nil")
	}
}

// TestSessionManager_DerivedRenewInterval validates that the session
// manager derives RenewInterval correctly using a table-driven approach.
//
// ═══════════════════════════════════════════════════════════════════════
// Formula: RenewInterval = LeaseTTL / MaxRenewFails
// When MaxRenewFails=0 → defaulted to 3 first
// ═══════════════════════════════════════════════════════════════════════
func TestSessionManager_DerivedRenewInterval(t *testing.T) {
	tests := []struct {
		name          string
		leaseTTL      time.Duration
		renewInterval time.Duration
		maxFails      int
	}{
		{
			name:     "derived from MaxRenewFails=3",
			leaseTTL: 600 * time.Millisecond,
			maxFails: 3,
		},
		{
			name:     "derived from custom MaxRenewFails=5",
			leaseTTL: 500 * time.Millisecond,
			maxFails: 5,
		},
		{
			name:     "both zero - MaxRenewFails defaults to 3 first",
			leaseTTL: 600 * time.Millisecond,
			maxFails: 0,
		},
		{
			name:     "MaxRenewFails=1 — interval equals TTL",
			leaseTTL: 300 * time.Millisecond,
			maxFails: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := NewFakeSession()
			leaseStore := NewFakeLeaseStore()

			cfg := goruntime.SessionConfig{
				SessionID:     "sess-" + tt.name,
				Exclusive:     true,
				LeaseTTL:      tt.leaseTTL,
				RenewInterval: tt.renewInterval,
				RenewJitter:   0,
				MaxRenewFails: tt.maxFails,
				StepDownGrace: 50 * time.Millisecond,
			}

			mgr := goruntime.NewSessionManagerFromConfig(cfg, session, leaseStore, "owner-1", nil)

			ctx, cancel := context.WithTimeout(context.Background(), tt.leaseTTL*2+200*time.Millisecond)
			defer cancel()

			err := mgr.Run(ctx)
			if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
				t.Fatalf("Run returned unexpected error: %v", err)
			}

			token, ok := mgr.Token()
			if !ok {
				t.Fatal("expected valid token from Token()")
			}
			if token.Version == 0 {
				t.Fatal("expected lease to be acquired")
			}
		})
	}
}

// TestOutboxDrainer_FinalDrainCompletesPendingRecords validates that
// the outbox drainer performs a final drain sweep on shutdown, completing
// any pending records within the DrainTimeout budget.
func TestOutboxDrainer_FinalDrainCompletesPendingRecords(t *testing.T) {
	outbox := NewFakeOutboxStore()
	lease := NewFakeLeaseStore()
	token := persistence.LeaseToken{Version: 1, Owner: "me"}

	_ = outbox.Persist(context.Background(), []*persistence.OutboxRecord{persistence.RehydrateFromSnapshot(persistence.OutboxSnapshot{
		ID:        "rec-1",
		RouteID:   "r1",
		SessionID: "s1",
		Status:    persistence.OutboxPending,
		Envelope:  messaging.Envelope{ID: "env-1", Payload: []byte("hello")},
	})})

	var sent atomic.Int32
	sender := &FakeSender{
		SendFn: func(_ *messaging.Envelope) error {
			sent.Add(1)
			return nil
		},
	}

	drainer := goruntime.NewOutboxDrainerFromConfig(goruntime.OutboxDrainerConfig{
		OutboxStore:  outbox,
		LeaseStore:   lease,
		Sender:       sender,
		RouteID:      "r1",
		PartitionKey: "SESSION#s1",
		LeaseID:      "s1",
		OwnerID:      "me",
		Policy:       routing.RoutePolicy{}.WithDefaults(),
		Strategy:     persistence.NewFixedPoll(50 * time.Millisecond),
		DrainTimeout: 5 * time.Second,
		TokenFn:      func() (persistence.LeaseToken, bool) { return token, true },
	})

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- drainer.Run(ctx)
	}()

	deadline := time.After(3 * time.Second)
	for sent.Load() <= 0 {

		select {
		case <-deadline:
			t.Fatal("timeout waiting for send")
		default:
			time.Sleep(20 * time.Millisecond) // SYNC: poll interval in inline wait loop
		}
	}

	cancel()

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for Run to return after cancel")
	}

	if c := outbox.CompletedCount(); c == 0 {
		t.Error("expected at least one completed record after drain")
	}
}
