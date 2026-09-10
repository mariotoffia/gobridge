package route

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime/dlq"
)

// delayRecordingDelivery records the backoff delay the runtime asked the source
// to wait before each redelivery, so a test can assert the delay actually grows
// with the attempt count.
type delayRecordingDelivery struct {
	env    *messaging.Envelope
	acked  bool
	delays []time.Duration
}

func (d *delayRecordingDelivery) Envelope() *messaging.Envelope { return d.env }
func (d *delayRecordingDelivery) Ack(context.Context) error     { d.acked = true; return nil }
func (d *delayRecordingDelivery) Retry(_ context.Context, after time.Duration, _ error) error {
	d.delays = append(d.delays, after)
	return nil
}
func (d *delayRecordingDelivery) Extend(context.Context, time.Time) error { return nil }

// stableCountLessEnv builds a COUNT-LESS envelope (no transport redelivery
// count) that still carries a STABLE per-message key, so the bridge-owned
// replay ledger can count its redeliveries. This is the AMQP 0-9-1 / MQTT shape
// the send-path backoff previously ignored: receiveCount stays 0 on every
// redelivery, so a receive-count-based backoff never leaves the first interval.
func stableCountLessEnv(id string) *messaging.Envelope {
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: id, Payload: []byte("p")})
	env.SetHeader(messaging.HeaderDeduplicationID, "stable-"+id)
	return env
}

// TestSendRetryBackoff_GrowsWithLedgerAttempt pins that a persistently failing
// downstream backs OFF for a count-less source: each redelivery must wait
// longer than the last, up to the configured ceiling. The source supplies no
// native receive count, so the growth can only come from the bridge-owned
// replay ledger.
//
// Mutation check: compute the send-path delay from receiveCount instead of the
// ledger attempt and this fails — every redelivery waits initial_interval.
func TestSendRetryBackoff_GrowsWithLedgerAttempt(t *testing.T) {
	r := NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID: "backoff-ledger",
		Policy: routing.RoutePolicy{
			DeliveryMode:       routing.DeliveryDirectHold,
			MaxReplayAttempts:  10,
			SendTimeout:        time.Second,
			TrustBridgeHeaders: true, // keep the stable dedup key across ingress
			Backoff: routing.BackoffPolicy{
				InitialInterval: time.Second,
				MaxInterval:     time.Minute,
				Multiplier:      2,
				JitterFactor:    routing.JitterDisabled,
			},
		},
		Sender:   stubSender{err: shared.ErrUnavailable},
		Bindings: []routing.DestinationBinding{{ID: "b1", SessionID: "s1", Address: "addr"}},
		Resolver: fixedResolver{plans: []routing.DispatchPlan{{BindingID: "b1", Address: "addr"}}},
		DLQ:      dlq.New(&recordingDLQStore{}),
		Metrics:  &ports.RecordingExporter{},
	})

	env := stableCountLessEnv("backoff-env")
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	var got []time.Duration
	for range want {
		del := &delayRecordingDelivery{env: env}
		if err := r.HandleDelivery(context.Background(), del); err != nil {
			t.Fatalf("HandleDelivery: %v", err)
		}
		if len(del.delays) != 1 {
			t.Fatalf("delivery did not retry exactly once: delays=%v acked=%v", del.delays, del.acked)
		}
		got = append(got, del.delays[0])
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("retry delays = %v, want %v (backoff must climb with the ledger attempt)", got, want)
		}
	}
}

// fullOutboxStore reports the route's outbox partition as at capacity on every
// QueryPending, modelling sustained backpressure from a drainer that cannot
// keep up. Persist is never expected to be reached.
type fullOutboxStore struct {
	pending  []*persistence.OutboxRecord
	persists int
}

func (s *fullOutboxStore) Persist(context.Context, []*persistence.OutboxRecord) error {
	s.persists++
	return nil
}
func (s *fullOutboxStore) Claim(context.Context, string, persistence.LeaseToken, int) ([]*persistence.OutboxRecord, error) {
	return nil, nil
}
func (s *fullOutboxStore) Complete(context.Context, []string, persistence.LeaseToken) error {
	return nil
}
func (s *fullOutboxStore) Expire(context.Context, time.Time, string, persistence.LeaseToken) (int, error) {
	return 0, nil
}
func (s *fullOutboxStore) QueryPending(context.Context, string, int) ([]*persistence.OutboxRecord, error) {
	return s.pending, nil
}
func (s *fullOutboxStore) Release(context.Context, []string, persistence.LeaseToken) error {
	return nil
}

// TestOutboxBackpressure_DoesNotChargeReplayBudget pins that waiting for a full
// outbox partition is not a message failure: the message never reached a
// destination, so it must not spend the replay budget that decides when a
// message is poisoned. Otherwise a message that merely queued behind a slow
// drainer is poisoned (DLQ'd, or DROPPED under on_permanent_failure=drop) on
// the very first genuine transient error after capacity frees.
//
// Mutation check: charge the ledger on the backpressure retry and this fails —
// the replay attempt count climbs while nothing was ever attempted.
func TestOutboxBackpressure_DoesNotChargeReplayBudget(t *testing.T) {
	store := &fullOutboxStore{pending: make([]*persistence.OutboxRecord, 4)}
	r := NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID: "backpressure-ledger",
		Policy: routing.RoutePolicy{
			DeliveryMode:       routing.DeliverySharedOutbox,
			MaxReplayAttempts:  2,
			MaxOutboxDepth:     2, // pending (4) >= depth (2): at capacity
			SendTimeout:        time.Second,
			TrustBridgeHeaders: true,
			OnPermanentFailure: routing.FailureDrop,
		},
		OutboxStore: store,
		Bindings:    []routing.DestinationBinding{{ID: "b1", SessionID: "s1", Address: "addr"}},
		Resolver:    fixedResolver{plans: []routing.DispatchPlan{{BindingID: "b1", Address: "addr"}}},
		DLQ:         dlq.New(&recordingDLQStore{}),
		Metrics:     &ports.RecordingExporter{},
	})

	env := stableCountLessEnv("backpressure-env")
	for i := range 5 {
		del := &delayRecordingDelivery{env: env}
		if err := r.HandleDelivery(context.Background(), del); err != nil {
			t.Fatalf("delivery %d: HandleDelivery: %v", i, err)
		}
		if del.acked {
			t.Fatalf("delivery %d: a backpressured message was settled terminally", i)
		}
		if len(del.delays) != 1 {
			t.Fatalf("delivery %d: want exactly one retry, got delays=%v", i, del.delays)
		}
	}
	if store.persists != 0 {
		t.Fatalf("Persist called %d times; the partition was at capacity throughout", store.persists)
	}
	if got := r.effectiveAttempt(env); got != 0 {
		t.Fatalf("replay attempts after 5 backpressure retries = %d, want 0 "+
			"(queueing behind a full partition is not a delivery attempt)", got)
	}
}

// failingDLQStore rejects every write, modelling an unhealthy DLQ backend.
type failingDLQStore struct{ recordingDLQStore }

func (s *failingDLQStore) Write(ctx context.Context, e routing.DLQEntry) error {
	_ = s.recordingDLQStore.Write(ctx, e)
	return errors.New("dlq store unavailable")
}

// TestDLQWriteFailureRetry_DoesNotChargeReplayBudget pins that a failed DLQ
// WRITE is a store fault, not another failed delivery attempt of the message.
// Charging it to the replay ledger would let an unhealthy DLQ store burn a
// message's whole budget while the message itself was never re-attempted.
//
// Mutation check: charge the ledger on the DLQ-write-failure retry and this
// fails — the attempt count climbs with the store's failures.
func TestDLQWriteFailureRetry_DoesNotChargeReplayBudget(t *testing.T) {
	store := &failingDLQStore{}
	r := NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID: "dlq-write-fail-ledger",
		Policy: routing.RoutePolicy{
			DeliveryMode:       routing.DeliveryDirectHold,
			MaxReplayAttempts:  2,
			SendTimeout:        time.Second,
			TrustBridgeHeaders: true,
		},
		// A PERMANENT send failure goes straight to the DLQ, whose write then
		// fails: exactly the retry that must not be charged.
		Sender:   stubSender{err: shared.ErrInvalidPayload},
		Bindings: []routing.DestinationBinding{{ID: "b1", SessionID: "s1", Address: "addr"}},
		Resolver: fixedResolver{plans: []routing.DispatchPlan{{BindingID: "b1", Address: "addr"}}},
		// One attempt, no write-retry backoff: the router's production
		// retry budget is irrelevant here and would only make the test slow.
		DLQ: dlq.NewFromConfig(dlq.Config{
			Store:            store,
			WriteTimeout:     time.Second,
			WriteMaxAttempts: 1,
		}),
		Metrics: &ports.RecordingExporter{},
	})

	env := stableCountLessEnv("dlq-write-fail-env")
	for i := range 4 {
		del := &delayRecordingDelivery{env: env}
		if err := r.HandleDelivery(context.Background(), del); err != nil {
			t.Fatalf("delivery %d: HandleDelivery: %v", i, err)
		}
		if del.acked {
			t.Fatalf("delivery %d: message settled although its DLQ record was never written", i)
		}
	}
	if store.writes.Load() == 0 {
		t.Fatal("the DLQ store was never asked to write; the test did not exercise the write-failure path")
	}
	if got := r.effectiveAttempt(env); got != 0 {
		t.Fatalf("replay attempts after 4 DLQ-write failures = %d, want 0 "+
			"(an unhealthy DLQ store must not spend the message's replay budget)", got)
	}
}

// TestPersistTimeout_DoesNotChargeReplayBudget pins that a bounded-store
// deadline on the outbox persist is infrastructure trouble, not a message
// failure: the store was slow, the message was never delivered anywhere, and
// the same message will be written the moment the store recovers. Charging it
// would let a run of store latency exhaust a healthy message's budget so the
// next genuine transient error poisons it.
//
// Mutation check: charge the ledger on the persist-timeout retry and this fails
// — the attempt count climbs with the store's latency.
func TestPersistTimeout_DoesNotChargeReplayBudget(t *testing.T) {
	store := &timingOutPersistStore{}
	r := NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID: "persist-timeout-ledger",
		Policy: routing.RoutePolicy{
			DeliveryMode:       routing.DeliverySharedOutbox,
			MaxReplayAttempts:  2,
			SendTimeout:        time.Second,
			TrustBridgeHeaders: true,
		},
		OutboxStore: store,
		Bindings:    []routing.DestinationBinding{{ID: "b1", SessionID: "s1", Address: "addr"}},
		Resolver:    fixedResolver{plans: []routing.DispatchPlan{{BindingID: "b1", Address: "addr"}}},
		DLQ:         dlq.New(&recordingDLQStore{}),
		Metrics:     &ports.RecordingExporter{},
	})

	env := stableCountLessEnv("persist-timeout-env")
	for i := range 4 {
		del := &delayRecordingDelivery{env: env}
		if err := r.HandleDelivery(context.Background(), del); err != nil {
			t.Fatalf("delivery %d: HandleDelivery: %v", i, err)
		}
		if del.acked {
			t.Fatalf("delivery %d: a store timeout settled the message terminally", i)
		}
	}
	if store.persisted.Load() == 0 {
		t.Fatal("Persist was never attempted; the test did not exercise the timeout path")
	}
	if got := r.effectiveAttempt(env); got != 0 {
		t.Fatalf("replay attempts after 4 store timeouts = %d, want 0 "+
			"(a slow store must not spend the message's replay budget)", got)
	}
}
