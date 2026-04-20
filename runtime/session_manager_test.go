package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// Verifies a non-exclusive session manager starts the session and reconciles once without a lease.
func TestSessionManager_NonExclusive(t *testing.T) {
	session := NewFakeSession()
	cfg := goruntime.SessionConfig{
		SessionID: "sess-1",
		Exclusive: false,
		Plan:      domain.SessionPlan{},
	}

	mgr := goruntime.NewSessionManagerFromConfig(cfg, session, nil, "bridge-1", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_ = mgr.Run(ctx)

	if !session.Started {
		t.Fatal("session should be started")
	}
	if len(session.Plans) != 1 {
		t.Fatalf("expected 1 reconcile call, got %d", len(session.Plans))
	}
}

// Verifies exclusive mode acquires a lease, exposes a token with owner and version, and starts the session.
func TestSessionManager_ExclusiveLease(t *testing.T) {
	session := NewFakeSession()
	leaseStore := NewFakeLeaseStore()

	cfg := goruntime.SessionConfig{
		SessionID:     "sess-exclusive",
		Exclusive:     true,
		LeaseTTL:      500 * time.Millisecond,
		RenewInterval: 100 * time.Millisecond,
		RenewJitter:   0,
		MaxRenewFails: 3,
		StepDownGrace: 50 * time.Millisecond,
	}

	mgr := goruntime.NewSessionManagerFromConfig(cfg, session, leaseStore, "bridge-1", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	_ = mgr.Run(ctx)

	token, hasLease := mgr.Token()
	if !hasLease {
		t.Fatal("should hold lease after acquisition")
	}
	if token.Version == 0 {
		t.Fatal("token version should be > 0")
	}
	if token.Owner != "bridge-1" {
		t.Fatalf("expected owner 'bridge-1', got %q", token.Owner)
	}

	if !session.Started {
		t.Fatal("session should be started")
	}
}

// Verifies lease loss after renew failure clears hasLease and Run exits with error when the context is cancelled.
//
// Scenario: run manager; force renew errors; poll until lease cleared; cancel context and assert Run returns non-nil error.
func TestSessionManager_StepDown(t *testing.T) {
	session := NewFakeSession()
	leaseStore := NewFakeLeaseStore()

	cfg := goruntime.SessionConfig{
		SessionID:     "sess-stepdown",
		Exclusive:     true,
		LeaseTTL:      500 * time.Millisecond,
		RenewInterval: 50 * time.Millisecond,
		RenewJitter:   0,
		MaxRenewFails: 2,
		StepDownGrace: 50 * time.Millisecond,
	}

	mgr := goruntime.NewSessionManagerFromConfig(cfg, session, leaseStore, "bridge-1", nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- mgr.Run(ctx)
	}()

	select {
	case evt := <-mgr.LeaseStateChanged():
		if evt.State != goruntime.LeaseStateAcquired {
			t.Fatalf("expected LeaseStateAcquired, got %v", evt.State)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lease not acquired in time")
	}

	leaseStore.SetRenewErr(domain.ErrVersionMismatch)

	lossDeadline := time.After(5 * time.Second)
	for {
		select {
		case evt := <-mgr.LeaseStateChanged():
			if evt.State == goruntime.LeaseStateLost {
				goto leaseLost
			}
		case <-lossDeadline:
			t.Fatal("step-down should clear hasLease within timeout")
		}
	}
leaseLost:

	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected error from cancelled context")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run should exit after context cancellation")
	}
}

// Verifies Close succeeds after Run is cancelled following an exclusive session with lease renewal.
func TestSessionManager_Close(t *testing.T) {
	session := NewFakeSession()
	leaseStore := NewFakeLeaseStore()

	cfg := goruntime.SessionConfig{
		SessionID:     "sess-close",
		Exclusive:     true,
		LeaseTTL:      500 * time.Millisecond,
		RenewInterval: 100 * time.Millisecond,
		RenewJitter:   0,
		MaxRenewFails: 3,
		StepDownGrace: 50 * time.Millisecond,
	}

	mgr := goruntime.NewSessionManagerFromConfig(cfg, session, leaseStore, "bridge-1", nil)

	ctx, cancel := context.WithCancel(context.Background())

	runDone := make(chan struct{})
	go func() {
		_ = mgr.Run(ctx)
		close(runDone)
	}()

	select {
	case evt := <-mgr.LeaseStateChanged():
		if evt.State != goruntime.LeaseStateAcquired {
			t.Fatalf("expected LeaseStateAcquired, got %v", evt.State)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lease not acquired in time")
	}

	cancel()

	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run should exit after cancel")
	}

	err := mgr.Close(context.Background())
	if err != nil {
		t.Fatalf("close error: %v", err)
	}
}
