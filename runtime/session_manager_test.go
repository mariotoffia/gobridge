package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

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

func TestSessionManager_ExclusiveLease(t *testing.T) {
	session := NewFakeSession()
	leaseStore := NewFakeLeaseStore()

	cfg := goruntime.SessionConfig{
		SessionID:     "sess-exclusive",
		Exclusive:     true,
		LeaseTTL:      500 * time.Millisecond,
		RenewInterval: 100 * time.Millisecond,
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

func TestSessionManager_StepDown(t *testing.T) {
	session := NewFakeSession()
	leaseStore := NewFakeLeaseStore()

	cfg := goruntime.SessionConfig{
		SessionID:     "sess-stepdown",
		Exclusive:     true,
		LeaseTTL:      500 * time.Millisecond,
		RenewInterval: 50 * time.Millisecond,
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

	time.Sleep(100 * time.Millisecond)

	leaseStore.SetRenewErr(domain.ErrVersionMismatch)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("step-down should clear hasLease within timeout")
		default:
		}
		_, hasLease := mgr.Token()
		if !hasLease {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

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

func TestSessionManager_Close(t *testing.T) {
	session := NewFakeSession()
	leaseStore := NewFakeLeaseStore()

	cfg := goruntime.SessionConfig{
		SessionID:     "sess-close",
		Exclusive:     true,
		LeaseTTL:      500 * time.Millisecond,
		RenewInterval: 100 * time.Millisecond,
		MaxRenewFails: 3,
		StepDownGrace: 50 * time.Millisecond,
	}

	mgr := goruntime.NewSessionManagerFromConfig(cfg, session, leaseStore, "bridge-1", nil)

	ctx, cancel := context.WithCancel(context.Background())

	go func() { _ = mgr.Run(ctx) }()
	time.Sleep(150 * time.Millisecond)

	cancel()
	time.Sleep(50 * time.Millisecond)

	err := mgr.Close(context.Background())
	if err != nil {
		t.Fatalf("close error: %v", err)
	}
}
