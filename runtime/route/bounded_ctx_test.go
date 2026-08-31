package route

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/dlq"
)

// deadlineCapturingStore records whether the context handed to Persist carried a
// deadline, proving the runtime bounds the store operation.
type deadlineCapturingStore struct {
	persistHadDeadline atomic.Bool
	persisted          atomic.Int32
}

func (s *deadlineCapturingStore) Persist(ctx context.Context, _ []*persistence.OutboxRecord) error {
	if _, ok := ctx.Deadline(); ok {
		s.persistHadDeadline.Store(true)
	}
	s.persisted.Add(1)
	return nil
}
func (s *deadlineCapturingStore) Claim(context.Context, string, persistence.LeaseToken, int) ([]*persistence.OutboxRecord, error) {
	return nil, nil
}
func (s *deadlineCapturingStore) Complete(context.Context, []string, persistence.LeaseToken) error {
	return nil
}
func (s *deadlineCapturingStore) Expire(context.Context, time.Time, string, persistence.LeaseToken) (int, error) {
	return 0, nil
}
func (s *deadlineCapturingStore) QueryPending(context.Context, string, int) ([]*persistence.OutboxRecord, error) {
	return nil, nil
}
func (s *deadlineCapturingStore) Release(context.Context, []string, persistence.LeaseToken) error {
	return nil
}

// TestPersistReceivesBoundedContext pins: the runtime wraps the
// outbox Persist call in a bounded (deadline-bearing) context so a black-holed
// store cannot pin route in-flight capacity forever.
//
// Mutation check: revert the storeOpContext wrap on Persist and this fails — the
// store receives the raw (deadline-less) caller context.
func TestPersistReceivesBoundedContext(t *testing.T) {
	store := &deadlineCapturingStore{}
	r := NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID: "store1",
		Policy: routing.RoutePolicy{
			DeliveryMode: routing.DeliverySharedOutbox,
			SendTimeout:  2 * time.Second,
		},
		OutboxStore: store,
		DLQ:         dlq.New(&recordingDLQStore{}),
		Bindings:    []routing.DestinationBinding{{ID: "b1", SessionID: "s1", Address: "addr"}},
		Resolver:    fixedResolver{plans: []routing.DispatchPlan{{BindingID: "b1", Address: "addr"}}},
		Metrics:     &ports.RecordingExporter{},
	})

	del := &stubDelivery{env: countLessEnv("store1-env")}
	if err := r.HandleDelivery(context.Background(), del); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}
	if store.persisted.Load() == 0 {
		t.Fatal("Persist was never called; test did not exercise the bounded-context path")
	}
	if !store.persistHadDeadline.Load() {
		t.Fatal("Persist received a deadline-less context; the store operation is unbounded")
	}
}

// timingOutPersistStore returns a context.DeadlineExceeded from Persist, modelling
// a slow-but-healthy store whose write exceeds the bounded store-op timeout.
type timingOutPersistStore struct{ deadlineCapturingStore }

func (s *timingOutPersistStore) Persist(context.Context, []*persistence.OutboxRecord) error {
	s.persisted.Add(1)
	return context.DeadlineExceeded
}

// TestPersistTimeoutRetriesUncountableSource pins a regression fix: a bounded-store DeadlineExceeded is an infrastructure timeout,
// not message poison, so an UNCOUNTABLE (adapter-generated-identity) source must
// be RETRIED, never terminally DLQ'd/dropped — otherwise a slow-but-healthy store
// would silently drop MQTT traffic under OnPermanentFailure=drop.
//
// Mutation check: remove the DeadlineExceeded guard before the replay-cap gate and
// this fails — the uncountable message poisons (acked/dropped) on the first slow
// write instead of retrying.
func TestPersistTimeoutRetriesUncountableSource(t *testing.T) {
	store := &timingOutPersistStore{}
	r := NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID: "store1-timeout",
		Policy: routing.RoutePolicy{
			DeliveryMode:       routing.DeliverySharedOutbox,
			SendTimeout:        time.Second,
			MaxReplayAttempts:  3,
			OnPermanentFailure: routing.FailureDrop, // the dangerous path: a poison here = silent loss
		},
		OutboxStore: store,
		DLQ:         dlq.New(&recordingDLQStore{}),
		Bindings:    []routing.DestinationBinding{{ID: "b1", SessionID: "s1", Address: "addr"}},
		Resolver:    fixedResolver{plans: []routing.DispatchPlan{{BindingID: "b1", Address: "addr"}}},
		Metrics:     &ports.RecordingExporter{},
	})

	del := &stubDelivery{env: generatedIDEnv("store1-timeout-env")}
	if err := r.HandleDelivery(context.Background(), del); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}
	if store.persisted.Load() == 0 {
		t.Fatal("Persist was never attempted")
	}
	if del.acked {
		t.Fatal("a store timeout terminally settled an uncountable message — infra timeout must not poison/drop")
	}
	if !del.retried {
		t.Fatal("a store timeout must retry (transient), not poison the message")
	}
}

// TestStoreOpContextBounded unit-checks the helper: it always returns a
// deadline, bounded by SendTimeout, falling back to the default when unset.
func TestStoreOpContextBounded(t *testing.T) {
	r := &RouteRunner{policy: routing.RoutePolicy{SendTimeout: 3 * time.Second}}
	ctx, cancel := r.storeOpContext(context.Background())
	defer cancel()
	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("storeOpContext returned a deadline-less context")
	}
	if rem := time.Until(dl); rem <= 0 || rem > 3*time.Second+time.Second {
		t.Fatalf("deadline in %v, want ~3s", rem)
	}

	rd := &RouteRunner{policy: routing.RoutePolicy{}}
	ctx2, cancel2 := rd.storeOpContext(context.Background())
	defer cancel2()
	dl2, ok2 := ctx2.Deadline()
	if !ok2 || time.Until(dl2) > routing.DefaultSendTimeout+time.Second {
		t.Fatalf("default-fallback deadline wrong: ok=%v", ok2)
	}
}
