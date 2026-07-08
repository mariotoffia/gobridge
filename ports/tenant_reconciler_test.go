package ports_test

import (
	"context"
	"testing"

	"github.com/mariotoffia/gobridge/ports"
)

// reconcilingTracker is a shared-style tracker that ALSO implements the optional
// TenantUsageReconciler extension. It exists to document (and compile-pin) the
// contract shape of a conforming shared tracker: paired in-flight
// increment/decrement PLUS an active reap for counts stranded by a crash.
type reconcilingTracker struct {
	reaped int64
}

func (t *reconcilingTracker) IncrementMessages(context.Context, string, int64) error { return nil }
func (t *reconcilingTracker) IncrementInFlight(context.Context, string, int64) error { return nil }

func (t *reconcilingTracker) ReconcileInFlight(_ context.Context, _ string) (int64, error) {
	return t.reaped, nil
}

// Compile-time capability assertions: a reconciling tracker satisfies BOTH the
// base tracker and the optional reconciler extension.
var (
	_ ports.TenantUsageTracker    = (*reconcilingTracker)(nil)
	_ ports.TenantUsageReconciler = (*reconcilingTracker)(nil)
)

// TestTenantUsageReconciler_ContractShape documents the crash-decay contract's
// active-reap hook: a shared tracker MAY implement TenantUsageReconciler, and a
// base (increment-only) tracker MUST NOT be forced to. The runtime never wires
// this hook (no tracker ships) — this test only pins the interface shape so an
// operator implementation type-asserts cleanly.
func TestTenantUsageReconciler_ContractShape(t *testing.T) {
	var tracker ports.TenantUsageTracker = &reconcilingTracker{reaped: 3}

	rec, ok := tracker.(ports.TenantUsageReconciler)
	if !ok {
		t.Fatal("reconcilingTracker must satisfy TenantUsageReconciler")
	}
	reaped, err := rec.ReconcileInFlight(context.Background(), "acme")
	if err != nil {
		t.Fatalf("ReconcileInFlight: unexpected error %v", err)
	}
	if reaped != 3 {
		t.Fatalf("reaped = %d, want 3", reaped)
	}
}
