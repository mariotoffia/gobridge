package paho

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/mariotoffia/gobridge/domain/connectivity"
)

// addedHookRecorder records every subscription-added hook call and whether the
// session mutex was free at the time, so a test can prove the session calls
// the hook outside s.mu.
type addedHookRecorder struct {
	session *Session
	mu      sync.Mutex
	calls   [][]string
	locked  int
}

func (r *addedHookRecorder) record(filters []string) {
	free := r.session.mu.TryLock()
	if free {
		r.session.mu.Unlock()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, slices.Clone(filters))
	if !free {
		r.locked++
	}
}

func (r *addedHookRecorder) snapshot() (calls [][]string, locked int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls), r.locked
}

// startHookedManagedSession starts a managed session whose durable history
// holds history and installs a recording subscription-added hook.
func startHookedManagedSession(t *testing.T, history ...string) (*Session, *managedConnFake, *addedHookRecorder) {
	t.Helper()
	operations := []string{}
	known := map[string]struct{}{}
	for _, filter := range history {
		known[filter] = struct{}{}
	}
	store := &managedHistoryFake{operations: &operations, values: map[string]map[string]struct{}{
		"safe-session-id": known,
	}}
	conn := &managedConnFake{operations: &operations}
	session := newManagedTestSession(t, store, conn)
	recorder := &addedHookRecorder{session: session}
	session.SetSubscriptionAddedHook(recorder.record)
	if err := session.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = session.Close(context.Background()) })
	return session, conn, recorder
}

func sensorsPlan() connectivity.SessionPlan {
	return connectivity.SessionPlan{Subscriptions: []connectivity.SubscriptionPlan{{Topic: "sensors/+/temp", QoS: 1}}}
}

func requireAddedCalls(t *testing.T, recorder *addedHookRecorder, want [][]string) {
	t.Helper()
	calls, locked := recorder.snapshot()
	if !slices.EqualFunc(calls, want, slices.Equal[[]string]) {
		t.Fatalf("subscription-added hook calls = %v, want %v", calls, want)
	}
	if locked != 0 {
		t.Fatalf("subscription-added hook ran %d time(s) while the session mutex was held", locked)
	}
}

func TestSubscriptionAddedHookReportsANewManagedFilterOnce(t *testing.T) {
	session, conn, recorder := startHookedManagedSession(t)

	if err := session.Reconcile(t.Context(), sensorsPlan()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	requireAddedCalls(t, recorder, [][]string{{"sensors/+/temp"}})

	// A reconnect resets the broker-observed state, so the next reconcile
	// subscribes the filter again. That re-establishes a known subscription;
	// it does not add one.
	session.handleConnectionUp()
	if err := session.Reconcile(t.Context(), sensorsPlan()); err != nil {
		t.Fatalf("Reconcile after reconnect: %v", err)
	}
	if !equalManagedStrings(conn.subscribed, []string{"sensors/+/temp", "sensors/+/temp"}) {
		t.Fatalf("SUBSCRIBE filters = %v, want the filter subscribed twice", conn.subscribed)
	}
	requireAddedCalls(t, recorder, [][]string{{"sensors/+/temp"}})
}

func TestSubscriptionAddedHookIgnoresAFilterAlreadyInHistory(t *testing.T) {
	session, conn, recorder := startHookedManagedSession(t, "sensors/+/temp")

	if err := session.Reconcile(t.Context(), sensorsPlan()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !equalManagedStrings(conn.subscribed, []string{"sensors/+/temp"}) {
		t.Fatalf("SUBSCRIBE filters = %v, want the restarted filter subscribed", conn.subscribed)
	}
	requireAddedCalls(t, recorder, nil)
}

func TestSubscriptionAddedHookSurvivesAFailedSubscribe(t *testing.T) {
	session, conn, recorder := startHookedManagedSession(t)

	conn.subErr = errors.New("SUBACK not received")
	if err := session.Reconcile(t.Context(), sensorsPlan()); err == nil {
		t.Fatal("Reconcile succeeded, want the SUBSCRIBE failure")
	}
	requireAddedCalls(t, recorder, nil)

	// The write-ahead already put the filter in the managed history, so only
	// the mark kept across the failure can still report it.
	conn.subErr = nil
	if err := session.Reconcile(t.Context(), sensorsPlan()); err != nil {
		t.Fatalf("retried Reconcile: %v", err)
	}
	requireAddedCalls(t, recorder, [][]string{{"sensors/+/temp"}})
}

func TestSubscriptionAddedHookNeverFiresForAnUnmanagedSession(t *testing.T) {
	operations := []string{}
	store := &managedHistoryFake{operations: &operations, values: map[string]map[string]struct{}{}}
	conn := &managedConnFake{operations: &operations}
	session := newManagedTestSession(t, store, conn)
	// The identity alone does not make the session keep a managed history.
	session.managedRequired = false
	recorder := &addedHookRecorder{session: session}
	session.SetSubscriptionAddedHook(recorder.record)
	if err := session.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = session.Close(context.Background()) })

	if err := session.Reconcile(t.Context(), sensorsPlan()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !equalManagedStrings(conn.subscribed, []string{"sensors/+/temp"}) {
		t.Fatalf("SUBSCRIBE filters = %v, want the filter subscribed", conn.subscribed)
	}
	requireAddedCalls(t, recorder, nil)
	if got := session.ManagedSubscriptionIdentity(); got != "" {
		t.Fatalf("ManagedSubscriptionIdentity() = %q, want empty for an unmanaged session", got)
	}
}

func TestManagedSubscriptionIdentityReportsTheStorageIdentity(t *testing.T) {
	operations := []string{}
	store := &managedHistoryFake{operations: &operations, values: map[string]map[string]struct{}{}}
	session := newManagedTestSession(t, store, &managedConnFake{operations: &operations})
	session.managedIdentity = "id-1"

	if got := session.ManagedSubscriptionIdentity(); got != "id-1" {
		t.Fatalf("ManagedSubscriptionIdentity() = %q, want %q", got, "id-1")
	}
}
