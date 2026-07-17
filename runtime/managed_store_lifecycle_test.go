package runtime

import (
	"context"
	"sync/atomic"
	"testing"
)

type closableManagedStore struct{ closes atomic.Int32 }

func (*closableManagedStore) List(context.Context, string) ([]string, error)   { return nil, nil }
func (*closableManagedStore) Remember(context.Context, string, []string) error { return nil }
func (*closableManagedStore) Forget(context.Context, string, []string) error   { return nil }
func (s *closableManagedStore) Close() error                                   { s.closes.Add(1); return nil }

func TestRuntimeStopClosesManagedSubscriptionStore(t *testing.T) {
	store := &closableManagedStore{}
	rt := New(WithManagedSubscriptionStore(store))
	if err := rt.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := store.closes.Load(); got != 1 {
		t.Fatalf("Close count = %d, want 1", got)
	}
	if err := rt.Stop(t.Context()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if got := store.closes.Load(); got != 1 {
		t.Fatalf("idempotent Close count = %d", got)
	}
}
