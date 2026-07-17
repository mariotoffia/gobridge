package storetest

import (
	"context"
	"errors"
	"testing"

	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// ManagedSubscriptionStoreHarness supplies a live store and a fresh handle to
// the same durable backing for restart-persistence checks.
type ManagedSubscriptionStoreHarness struct {
	Store   ports.ManagedSubscriptionStore
	Restart func(t *testing.T) ports.ManagedSubscriptionStore
}

// RunManagedSubscriptionStoreTests verifies the ManagedSubscriptionStore
// contract for every durable backend.
func RunManagedSubscriptionStoreTests(t *testing.T, h ManagedSubscriptionStoreHarness) {
	t.Helper()
	t.Run("MissingBaseline", func(t *testing.T) {
		_, err := h.Store.List(context.Background(), "missing-baseline")
		if !errors.Is(err, shared.ErrNotFound) {
			t.Fatalf("List missing baseline error = %v, want ErrNotFound", err)
		}
	})
	t.Run("ForgetMissingBaseline", func(t *testing.T) {
		err := h.Store.Forget(context.Background(), "missing-forget-baseline", []string{"old/#"})
		if !errors.Is(err, shared.ErrNotFound) {
			t.Fatalf("Forget missing baseline error = %v, want ErrNotFound", err)
		}
	})
	t.Run("ContextCancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := h.Store.Remember(ctx, "cancelled-baseline", []string{"a/#"}); !errors.Is(err, context.Canceled) {
			t.Fatalf("Remember canceled error = %v, want context.Canceled", err)
		}
	})
	t.Run("RememberEmptyEstablishesBaseline", func(t *testing.T) {
		const identity = "empty-baseline"
		if err := h.Store.Remember(context.Background(), identity, nil); err != nil {
			t.Fatalf("Remember empty: %v", err)
		}
		got, err := h.Store.List(context.Background(), identity)
		if err != nil {
			t.Fatalf("List empty baseline: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("List empty baseline = %v, want empty", got)
		}
	})
	t.Run("IdempotentDeterministicHistory", func(t *testing.T) {
		const identity = "ordered-history"
		filters := []string{"sensors/#", "$share/group/sensors/#", "sensors/#"}
		if err := h.Store.Remember(context.Background(), identity, filters); err != nil {
			t.Fatalf("Remember: %v", err)
		}
		if err := h.Store.Remember(context.Background(), identity, filters); err != nil {
			t.Fatalf("Remember repeated: %v", err)
		}
		got, err := h.Store.List(context.Background(), identity)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		want := []string{"$share/group/sensors/#", "sensors/#"}
		if !equalStrings(got, want) {
			t.Fatalf("List = %v, want %v", got, want)
		}
	})
	t.Run("IdentityIsolation", func(t *testing.T) {
		ctx := context.Background()
		if err := h.Store.Remember(ctx, "identity-a", []string{"a/#"}); err != nil {
			t.Fatalf("Remember identity-a: %v", err)
		}
		if err := h.Store.Remember(ctx, "identity-b", []string{"b/#"}); err != nil {
			t.Fatalf("Remember identity-b: %v", err)
		}
		if err := h.Store.Forget(ctx, "identity-a", []string{"a/#"}); err != nil {
			t.Fatalf("Forget identity-a: %v", err)
		}
		a, err := h.Store.List(ctx, "identity-a")
		if err != nil {
			t.Fatalf("List identity-a: %v", err)
		}
		b, err := h.Store.List(ctx, "identity-b")
		if err != nil {
			t.Fatalf("List identity-b: %v", err)
		}
		if len(a) != 0 || !equalStrings(b, []string{"b/#"}) {
			t.Fatalf("isolated histories: a=%v b=%v", a, b)
		}
	})
	t.Run("ForgetOnlyConfirmedFilters", func(t *testing.T) {
		const identity = "partial-forget"
		ctx := context.Background()
		if err := h.Store.Remember(ctx, identity, []string{"old/a/#", "old/b/#"}); err != nil {
			t.Fatalf("Remember: %v", err)
		}
		if err := h.Store.Forget(ctx, identity, []string{"old/a/#", "old/a/#"}); err != nil {
			t.Fatalf("Forget confirmed filter: %v", err)
		}
		if err := h.Store.Forget(ctx, identity, []string{"not-managed/#"}); err != nil {
			t.Fatalf("Forget unknown filter: %v", err)
		}
		got, err := h.Store.List(ctx, identity)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if !equalStrings(got, []string{"old/b/#"}) {
			t.Fatalf("List after partial Forget = %v, want failed filter retained", got)
		}
	})
	t.Run("RestartPersistence", func(t *testing.T) {
		const identity = "restart-history"
		if err := h.Store.Remember(context.Background(), identity, []string{"sensors/#"}); err != nil {
			t.Fatalf("Remember before restart: %v", err)
		}
		restarted := h.Restart(t)
		got, err := restarted.List(context.Background(), identity)
		if err != nil {
			t.Fatalf("List after restart: %v", err)
		}
		if !equalStrings(got, []string{"sensors/#"}) {
			t.Fatalf("List after restart = %v", got)
		}
	})
	t.Run("ValidationIsAtomic", func(t *testing.T) {
		ctx := context.Background()
		for _, call := range []struct {
			name string
			fn   func() error
		}{
			{"list empty identity", func() error { _, err := h.Store.List(ctx, ""); return err }},
			{"remember empty identity", func() error { return h.Store.Remember(ctx, "", nil) }},
			{"forget empty identity", func() error { return h.Store.Forget(ctx, "", nil) }},
			{"remember empty filter", func() error { return h.Store.Remember(ctx, "invalid-filter", []string{"valid/#", ""}) }},
			{"forget empty filter", func() error { return h.Store.Forget(ctx, "invalid-filter", []string{""}) }},
		} {
			if err := call.fn(); !errors.Is(err, shared.ErrInvalidConfig) {
				t.Errorf("%s error = %v, want ErrInvalidConfig", call.name, err)
			}
		}
		_, err := h.Store.List(ctx, "invalid-filter")
		if !errors.Is(err, shared.ErrNotFound) {
			t.Fatalf("invalid Remember mutated baseline: %v", err)
		}
	})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
