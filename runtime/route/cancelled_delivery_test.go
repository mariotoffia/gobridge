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

// unretryableDelivery models the common source shape whose transport cannot
// defer a delivery: MQTT, HTTP and Azure Service Bus all answer Retry with
// shared.ErrNotSupported. It is the strictest witness for the cancellation
// rule, because on such a source EVERY decision to retry falls through to the
// terminal drop/DLQ fallback — so a path that merely "retries" a cancelled
// delivery still discards the message.
type unretryableDelivery struct {
	env     *messaging.Envelope
	acked   bool
	retried bool
}

func (d *unretryableDelivery) Envelope() *messaging.Envelope { return d.env }
func (d *unretryableDelivery) Ack(context.Context) error     { d.acked = true; return nil }
func (d *unretryableDelivery) Retry(context.Context, time.Duration, error) error {
	d.retried = true
	return shared.ErrNotSupported
}
func (d *unretryableDelivery) Extend(context.Context, time.Time) error { return nil }

// cancellingSender models a transport whose send is aborted because the BRIDGE
// tore the delivery context down mid-flight (SIGTERM, reconfiguration swap,
// receiver cancelling its route): it cancels the delivery and reports the
// cancellation, exactly as a cooperative sender does.
type cancellingSender struct{ cancel context.CancelFunc }

func (s *cancellingSender) Send(ctx context.Context, _ ports.OutboundMessage) error {
	s.cancel()
	return ctx.Err()
}

// cancellingProcessor aborts the processor chain the same way.
type cancellingProcessor struct{ cancel context.CancelFunc }

func (p *cancellingProcessor) Name() string { return "cancelling" }
func (p *cancellingProcessor) Process(ctx context.Context, _ *messaging.Envelope, _ ports.ProcessorFunc) error {
	p.cancel()
	return ctx.Err()
}

// cancellingResolver aborts destination resolution the same way.
type cancellingResolver struct{ cancel context.CancelFunc }

func (r *cancellingResolver) Resolve(ctx context.Context, _ *messaging.Envelope) ([]routing.DispatchPlan, error) {
	r.cancel()
	return nil, ctx.Err()
}

// cancellingOutboxStore aborts the outbox persist the same way. Every other
// operation is an inert success so only the persist phase is exercised.
type cancellingOutboxStore struct{ cancel context.CancelFunc }

func (s *cancellingOutboxStore) Persist(ctx context.Context, _ []*persistence.OutboxRecord) error {
	s.cancel()
	return ctx.Err()
}

func (s *cancellingOutboxStore) Claim(context.Context, string, persistence.LeaseToken, int) ([]*persistence.OutboxRecord, error) {
	return nil, nil
}
func (s *cancellingOutboxStore) Complete(context.Context, []string, persistence.LeaseToken) error {
	return nil
}
func (s *cancellingOutboxStore) Expire(context.Context, time.Time, string, persistence.LeaseToken) (int, error) {
	return 0, nil
}
func (s *cancellingOutboxStore) QueryPending(context.Context, string, int) ([]*persistence.OutboxRecord, error) {
	return nil, nil
}
func (s *cancellingOutboxStore) Release(context.Context, []string, persistence.LeaseToken) error {
	return nil
}

// cancellingBuildResolver cancels the delivery and then returns a plan whose
// binding carries no session, so the OUTBOX RECORD BUILD fails while the
// delivery context is already dead.
type cancellingBuildResolver struct{ cancel context.CancelFunc }

func (r *cancellingBuildResolver) Resolve(context.Context, *messaging.Envelope) ([]routing.DispatchPlan, error) {
	r.cancel()
	return []routing.DispatchPlan{{BindingID: "b1", Address: "addr"}}, nil
}

// cancellingDepthStore cancels the delivery during the outbox depth check and
// then reports the partition's state. failQuery selects between the two
// backpressure branches: a failed depth query, or a partition at capacity.
type cancellingDepthStore struct {
	cancel    context.CancelFunc
	failQuery bool
	pending   []*persistence.OutboxRecord
	persisted int
}

func (s *cancellingDepthStore) QueryPending(context.Context, string, int) ([]*persistence.OutboxRecord, error) {
	s.cancel()
	if s.failQuery {
		return nil, context.Canceled
	}
	return s.pending, nil
}

func (s *cancellingDepthStore) Persist(context.Context, []*persistence.OutboxRecord) error {
	s.persisted++
	return nil
}

func (s *cancellingDepthStore) Claim(context.Context, string, persistence.LeaseToken, int) ([]*persistence.OutboxRecord, error) {
	return nil, nil
}
func (s *cancellingDepthStore) Complete(context.Context, []string, persistence.LeaseToken) error {
	return nil
}
func (s *cancellingDepthStore) Expire(context.Context, time.Time, string, persistence.LeaseToken) (int, error) {
	return 0, nil
}
func (s *cancellingDepthStore) Release(context.Context, []string, persistence.LeaseToken) error {
	return nil
}

// TestCancelledDelivery_NeverSettlesTerminally pins the loss rule for every
// recoverable dispatch phase: when the BRIDGE cancels a delivery — a SIGTERM, a
// reconfiguration swap past the drain budget, a receiver cancelling its route —
// the failure is not evidence that the message is bad, so the delivery must be
// left UNSETTLED for the source to redeliver.
//
// The dangerous shape is pinned deliberately: an envelope whose identity is
// adapter-generated (the common MQTT publish) is UNCOUNTABLE, so the replay-cap
// gate treats any retry decision for it as already at the cap; combined with
// on_permanent_failure=drop the message would be acked and discarded on the
// first cancellation. Nothing may be acked, dropped or written to the DLQ here.
//
// Mutation check: remove the cancellation guard from any one phase and that
// sub-test fails — the delivery is acked and MessagesDropped is counted.
func TestCancelledDelivery_NeverSettlesTerminally(t *testing.T) {
	// dropPolicy is the loss-prone combination: the terminal decision DISCARDS
	// the message instead of retaining it in the DLQ.
	dropPolicy := func(mode routing.DeliveryMode) routing.RoutePolicy {
		return routing.RoutePolicy{
			DeliveryMode:       mode,
			MaxReplayAttempts:  3,
			SendTimeout:        time.Second,
			OnPermanentFailure: routing.FailureDrop,
		}
	}

	cases := []struct {
		name string
		cfg  func(cancel context.CancelFunc, router *dlq.Router, rec *ports.RecordingExporter) RouteRunnerConfig
	}{
		{
			name: "send",
			cfg: func(cancel context.CancelFunc, router *dlq.Router, rec *ports.RecordingExporter) RouteRunnerConfig {
				return RouteRunnerConfig{
					RouteID:  "cancel-send",
					Policy:   dropPolicy(routing.DeliveryDirectHold),
					Sender:   &cancellingSender{cancel: cancel},
					Bindings: []routing.DestinationBinding{{ID: "b1", SessionID: "s1", Address: "addr"}},
					Resolver: fixedResolver{plans: []routing.DispatchPlan{{BindingID: "b1", Address: "addr"}}},
					DLQ:      router,
					Metrics:  rec,
				}
			},
		},
		{
			name: "processor",
			cfg: func(cancel context.CancelFunc, router *dlq.Router, rec *ports.RecordingExporter) RouteRunnerConfig {
				return RouteRunnerConfig{
					RouteID:    "cancel-processor",
					Policy:     dropPolicy(routing.DeliveryDirectHold),
					Sender:     stubSender{},
					Processors: []ports.Processor{&cancellingProcessor{cancel: cancel}},
					Bindings:   []routing.DestinationBinding{{ID: "b1", SessionID: "s1", Address: "addr"}},
					Resolver:   fixedResolver{plans: []routing.DispatchPlan{{BindingID: "b1", Address: "addr"}}},
					DLQ:        router,
					Metrics:    rec,
				}
			},
		},
		{
			name: "resolver",
			cfg: func(cancel context.CancelFunc, router *dlq.Router, rec *ports.RecordingExporter) RouteRunnerConfig {
				return RouteRunnerConfig{
					RouteID:  "cancel-resolver",
					Policy:   dropPolicy(routing.DeliveryDirectHold),
					Sender:   stubSender{},
					Bindings: []routing.DestinationBinding{{ID: "b1", SessionID: "s1", Address: "addr"}},
					Resolver: &cancellingResolver{cancel: cancel},
					DLQ:      router,
					Metrics:  rec,
				}
			},
		},
		{
			name: "outbox_build",
			cfg: func(cancel context.CancelFunc, router *dlq.Router, rec *ports.RecordingExporter) RouteRunnerConfig {
				return RouteRunnerConfig{
					RouteID: "cancel-build",
					Policy:  dropPolicy(routing.DeliverySharedOutbox),
					// No SessionID on the binding: the record build fails because
					// the record would land in a partition no drainer polls.
					Bindings: []routing.DestinationBinding{{ID: "b1", Address: "addr"}},
					Resolver: &cancellingBuildResolver{cancel: cancel},
					// A store that would happily accept the record: the build must
					// fail before it is reached.
					OutboxStore: &deadlineCapturingStore{},
					DLQ:         router,
					Metrics:     rec,
				}
			},
		},
		{
			name: "outbox_depth_query",
			cfg: func(cancel context.CancelFunc, router *dlq.Router, rec *ports.RecordingExporter) RouteRunnerConfig {
				p := dropPolicy(routing.DeliverySharedOutbox)
				p.MaxOutboxDepth = 2
				return RouteRunnerConfig{
					RouteID:     "cancel-depth-query",
					Policy:      p,
					Bindings:    []routing.DestinationBinding{{ID: "b1", SessionID: "s1", Address: "addr"}},
					Resolver:    fixedResolver{plans: []routing.DispatchPlan{{BindingID: "b1", Address: "addr"}}},
					OutboxStore: &cancellingDepthStore{cancel: cancel, failQuery: true},
					DLQ:         router,
					Metrics:     rec,
				}
			},
		},
		{
			name: "outbox_at_capacity",
			cfg: func(cancel context.CancelFunc, router *dlq.Router, rec *ports.RecordingExporter) RouteRunnerConfig {
				p := dropPolicy(routing.DeliverySharedOutbox)
				p.MaxOutboxDepth = 2
				return RouteRunnerConfig{
					RouteID:     "cancel-at-capacity",
					Policy:      p,
					Bindings:    []routing.DestinationBinding{{ID: "b1", SessionID: "s1", Address: "addr"}},
					Resolver:    fixedResolver{plans: []routing.DispatchPlan{{BindingID: "b1", Address: "addr"}}},
					OutboxStore: &cancellingDepthStore{cancel: cancel, pending: make([]*persistence.OutboxRecord, 4)},
					DLQ:         router,
					Metrics:     rec,
				}
			},
		},
		{
			name: "outbox_persist",
			cfg: func(cancel context.CancelFunc, router *dlq.Router, rec *ports.RecordingExporter) RouteRunnerConfig {
				return RouteRunnerConfig{
					RouteID:     "cancel-persist",
					Policy:      dropPolicy(routing.DeliverySharedOutbox),
					Bindings:    []routing.DestinationBinding{{ID: "b1", SessionID: "s1", Address: "addr"}},
					Resolver:    fixedResolver{plans: []routing.DispatchPlan{{BindingID: "b1", Address: "addr"}}},
					OutboxStore: &cancellingOutboxStore{cancel: cancel},
					DLQ:         router,
					Metrics:     rec,
				}
			},
		},
	}

	// Both DLQ shapes matter. WITH a store the terminal decision writes a DLQ
	// record; WITHOUT one every terminal decision is a silent discard, and it is
	// also the shape that turns a "retry" into a drop on a source that cannot
	// retry. A guard that only holds for one of them still loses messages.
	for _, tc := range cases {
		for _, dlqShape := range []struct {
			name  string
			store *recordingDLQStore
		}{
			{name: "dlq_store", store: &recordingDLQStore{}},
			{name: "no_dlq_store"},
		} {
			t.Run(tc.name+"/"+dlqShape.name, func(t *testing.T) {
				rec := &ports.RecordingExporter{}
				router := dlq.New(nil)
				if dlqShape.store != nil {
					router = dlq.New(dlqShape.store)
				}
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				r := NewRouteRunnerFromConfig(tc.cfg(cancel, router, rec))

				del := &unretryableDelivery{env: generatedIDEnv("cancel-" + tc.name)}
				err := r.HandleDelivery(ctx, del)

				if del.acked {
					t.Fatal("a cancelled delivery was ACKed; the source can never redeliver it")
				}
				if got := countCounter(rec, shared.MetricMessagesDropped); got != 0 {
					t.Fatalf("MessagesDropped = %d, want 0: cancellation is not a message failure", got)
				}
				if dlqShape.store != nil {
					if got := dlqShape.store.writes.Load(); got != 0 {
						t.Fatalf("DLQ writes = %d, want 0: cancellation is not a message failure", got)
					}
				}
				if err == nil {
					t.Fatal("HandleDelivery returned nil for an abandoned delivery; the caller cannot tell it was left unsettled")
				}
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("error %v does not carry context.Canceled", err)
				}
			})
		}
	}
}

// TestCancelledDelivery_NoDLQStore_NeverAcks pins the same rule for the OTHER
// drop shape: a route with no DLQ store at all, where every terminal decision
// discards the message even under the default on_permanent_failure policy.
//
// Mutation check: remove the send-path cancellation guard and this fails — the
// message is dropped and acked.
func TestCancelledDelivery_NoDLQStore_NeverAcks(t *testing.T) {
	rec := &ports.RecordingExporter{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r := NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID: "cancel-no-dlq",
		Policy: routing.RoutePolicy{
			DeliveryMode:      routing.DeliveryDirectHold,
			MaxReplayAttempts: 3,
			SendTimeout:       time.Second,
		},
		Sender:   &cancellingSender{cancel: cancel},
		Bindings: []routing.DestinationBinding{{ID: "b1", SessionID: "s1", Address: "addr"}},
		Resolver: fixedResolver{plans: []routing.DispatchPlan{{BindingID: "b1", Address: "addr"}}},
		Metrics:  rec,
	})

	del := &unretryableDelivery{env: generatedIDEnv("cancel-no-dlq-env")}
	if err := r.HandleDelivery(ctx, del); err == nil {
		t.Fatal("HandleDelivery returned nil for an abandoned delivery")
	}
	if del.acked {
		t.Fatal("a cancelled delivery was ACKed with no DLQ store; the message is gone")
	}
	if got := countCounter(rec, shared.MetricMessagesDropped); got != 0 {
		t.Fatalf("MessagesDropped = %d, want 0", got)
	}
}

// TestSendTimeout_StillPoisonsUncountableSource pins the SCOPE of the guard: a
// send that fails because the TARGET is slow — the bounded send context expired
// while the delivery context stayed live — is a genuine message failure and
// keeps its terminal behaviour. Only the bridge cancelling ITSELF is exempt.
//
// Mutation check: widen the guard to any dead send context and this fails — a
// slow target stops poisoning and the route retries forever.
func TestSendTimeout_StillPoisonsUncountableSource(t *testing.T) {
	store := &recordingDLQStore{}
	r := NewRouteRunnerFromConfig(RouteRunnerConfig{
		RouteID: "send-timeout-poisons",
		Policy: routing.RoutePolicy{
			DeliveryMode:      routing.DeliveryDirectHold,
			MaxReplayAttempts: 3,
			SendTimeout:       time.Second,
		},
		Sender:   stubSender{err: context.DeadlineExceeded},
		Bindings: []routing.DestinationBinding{{ID: "b1", SessionID: "s1", Address: "addr"}},
		Resolver: fixedResolver{plans: []routing.DispatchPlan{{BindingID: "b1", Address: "addr"}}},
		DLQ:      dlq.New(store),
		Metrics:  &ports.RecordingExporter{},
	})

	del := &stubDelivery{env: generatedIDEnv("send-timeout-env")}
	if err := r.HandleDelivery(context.Background(), del); err != nil {
		t.Fatalf("HandleDelivery: %v", err)
	}
	if !del.acked {
		t.Fatal("a target-side send timeout on an uncountable message must still settle terminally")
	}
	if got := store.writes.Load(); got != 1 {
		t.Fatalf("DLQ writes = %d, want 1", got)
	}
}
