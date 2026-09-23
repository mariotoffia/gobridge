package paho

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// deadLetterFake records every dead-letter write and every protocol ACK in one
// ordered log, so a test can prove each ACK follows its own write.
type deadLetterFake struct {
	mu      sync.Mutex
	log     []string
	filters []string
	// failures makes the next N writes fail.
	failures int
	// block makes every write wait for its context and return its error.
	block bool
	// afterWrite runs after a successful write is recorded.
	afterWrite func()
}

func (f *deadLetterFake) write(ctx context.Context, env *messaging.Envelope, filter string) error {
	if f.block {
		<-ctx.Done()
		return ctx.Err()
	}
	topic, _ := env.Header(HeaderMQTTTopic)
	f.mu.Lock()
	if f.failures > 0 {
		f.failures--
		f.mu.Unlock()
		return errors.New("dead-letter store unavailable")
	}
	f.log = append(f.log, "write "+topic.(string))
	f.filters = append(f.filters, filter)
	after := f.afterWrite
	f.mu.Unlock()
	if after != nil {
		after()
	}
	return nil
}

// ack returns a protocol-ACK callback that records itself in the shared log.
func (f *deadLetterFake) ack(topic string, err error) func() error {
	return func() error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if err != nil {
			return err
		}
		f.log = append(f.log, "ack "+topic)
		return nil
	}
}

func (f *deadLetterFake) snapshot() (log, filters []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.log), slices.Clone(f.filters)
}

// startHeldRemovedSubscription starts a managed session whose durable history
// holds staleFilter, the broker answers every UNSUBSCRIBE with 0x11 (already
// absent), and one delivery for the removed filter is already buffered.
func startHeldRemovedSubscription(t *testing.T, fake *deadLetterFake, ackErr error) (*Session, *managedHistoryFake, *[]string) {
	t.Helper()
	operations := []string{}
	store := &managedHistoryFake{operations: &operations, values: map[string]map[string]struct{}{
		"safe-session-id": {"stale/#": {}},
	}}
	conn := &managedConnFake{operations: &operations, unsubReasons: []byte{0x11}}
	session := newManagedTestSession(t, store, conn)
	session.SetRemovedSubscriptionDeadLetter(fake.write)
	if err := session.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = session.Close(context.Background()) })
	session.router.dispatch(&pahov5.Publish{Topic: "stale/held", QoS: 1}, fake.ack("stale/held", ackErr))
	if got := session.Router().PendingCount(); got != 1 {
		t.Fatalf("held deliveries before reconcile = %d, want 1", got)
	}
	return session, store, &operations
}

func requireTransientNotTerminal(t *testing.T, session *Session, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("reconcile succeeded, want a transient error")
	}
	if errors.Is(err, shared.ErrTransportClosedPermanently) {
		t.Fatalf("reconcile error = %v, must not carry the terminal marker", err)
	}
	if !errors.Is(err, shared.ErrUnavailable) {
		t.Fatalf("reconcile error = %v, want ErrUnavailable", err)
	}
	if session.connection() == nil {
		t.Fatal("transient dead-letter failure closed the broker connection")
	}
}

func TestManagedSubscriptionPinnedReplayIsDeadLetteredAndConverges(t *testing.T) {
	operations := []string{}
	const staleFilter = "$share/group/stale/#"
	store := &managedHistoryFake{operations: &operations, values: map[string]map[string]struct{}{
		"safe-session-id": {staleFilter: {}},
	}}
	first := &managedConnFake{operations: &operations}
	replacement := &managedConnFake{operations: &operations, unsubReasons: []byte{0x11}}
	session := newManagedTestSession(t, store, first)
	fake := &deadLetterFake{}
	session.SetRemovedSubscriptionDeadLetter(fake.write)
	dials := 0
	session.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		dials++
		if dials == 1 {
			return first, func() {}, nil
		}
		session.handleConnectionUp()
		for _, topic := range []string{"stale/one", "stale/two"} {
			session.router.dispatch(&pahov5.Publish{Topic: topic, QoS: 1}, fake.ack(topic, nil))
		}
		return replacement, func() {}, nil
	}
	if err := session.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = session.Close(context.Background()) })

	if err := session.Reconcile(t.Context(), connectivity.SessionPlan{}); err != nil {
		t.Fatalf("pinned replay with a dead-letter path must converge: %v", err)
	}
	log, filters := fake.snapshot()
	wantLog := []string{"write stale/one", "ack stale/one", "write stale/two", "ack stale/two"}
	if !equalManagedStrings(log, wantLog) {
		t.Fatalf("dead-letter/ACK order = %v, want %v", log, wantLog)
	}
	if !equalManagedStrings(filters, []string{staleFilter, staleFilter}) {
		t.Fatalf("dead-letter filters = %v, want the removed filter twice", filters)
	}
	if got := store.snapshot("safe-session-id"); len(got) != 0 {
		t.Fatalf("durable history after dead-lettered replay = %v, want empty", got)
	}
	if !slices.Contains(operations, "forget") {
		t.Fatalf("dead-lettered replay did not forget history: operations=%v", operations)
	}
	if got := session.Router().PendingCount(); got != 0 {
		t.Fatalf("pending after dead-lettered replay = %d, want 0", got)
	}
	if session.connection() == nil {
		t.Fatal("dead-lettered replay closed the replacement connection")
	}
	health := session.Health(t.Context())
	if health.SubscriptionsSatisfied == nil || !*health.SubscriptionsSatisfied {
		t.Fatalf("dead-lettered replay left subscriptions unsatisfied: %+v", health)
	}
}

func TestManagedSubscriptionCrashRestartDelayedReplayIsDeadLettered(t *testing.T) {
	operations := []string{}
	const staleFilter = "$share/group/crash/#"
	store := &managedHistoryFake{operations: &operations, values: map[string]map[string]struct{}{
		"safe-session-id": {staleFilter: {}},
	}}
	conn := &managedConnFake{operations: &operations, unsubReasons: []byte{0x11}}
	session := newManagedTestSession(t, store, conn)
	fakeClock := clocktest.NewAt(time.Unix(1_700_000_000, 0))
	session.router.clk = fakeClock
	session.router.graceWindow = time.Minute
	// The write moves the fake clock past the replay-grace window before the
	// session looks for more replays, so the wait after it ends at once.
	fake := &deadLetterFake{afterWrite: func() { fakeClock.Advance(time.Minute) }}
	session.SetRemovedSubscriptionDeadLetter(fake.write)
	verificationStarted := make(chan struct{})
	var verificationOnce sync.Once
	session.router.awaitManagedReplayHook = func() {
		verificationOnce.Do(func() { close(verificationStarted) })
	}
	if err := session.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	session.handleConnectionUp()
	t.Cleanup(func() { _ = session.Close(context.Background()) })

	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- session.Reconcile(t.Context(), connectivity.SessionPlan{}) }()
	wait.RequireReceive(t, verificationStarted, 2*time.Second)
	session.router.dispatch(&pahov5.Publish{Topic: "crash/delayed", QoS: 1}, fake.ack("crash/delayed", nil))

	if err := wait.RequireReceive(t, reconcileDone, 2*time.Second); err != nil {
		t.Fatalf("delayed crash replay with a dead-letter path must converge: %v", err)
	}
	log, filters := fake.snapshot()
	if !equalManagedStrings(log, []string{"write crash/delayed", "ack crash/delayed"}) {
		t.Fatalf("dead-letter/ACK log = %v, want one write then one ACK", log)
	}
	if !equalManagedStrings(filters, []string{staleFilter}) {
		t.Fatalf("dead-letter filters = %v, want [%s]", filters, staleFilter)
	}
	if !slices.Contains(operations, "forget") {
		t.Fatalf("delayed crash replay did not forget history: operations=%v", operations)
	}
	if got := session.Router().PendingCount(); got != 0 {
		t.Fatalf("pending after dead-lettered crash replay = %d, want 0", got)
	}
}

func TestManagedSubscriptionDeadLetterFailureIsTransientAndRetried(t *testing.T) {
	fake := &deadLetterFake{failures: 1}
	session, store, operations := startHeldRemovedSubscription(t, fake, nil)

	err := session.Reconcile(t.Context(), connectivity.SessionPlan{})
	requireTransientNotTerminal(t, session, err)
	if log, _ := fake.snapshot(); len(log) != 0 {
		t.Fatalf("failed dead-letter write recorded %v, want no write and no ACK", log)
	}
	if got := session.Router().PendingCount(); got != 1 {
		t.Fatalf("pending after failed dead-letter write = %d, want 1", got)
	}
	if got := store.snapshot("safe-session-id"); !equalManagedStrings(got, []string{"stale/#"}) {
		t.Fatalf("history after failed dead-letter write = %v, want preserved", got)
	}
	if slices.Contains(*operations, "forget") {
		t.Fatalf("failed dead-letter write forgot history: operations=%v", *operations)
	}

	if err := session.Reconcile(t.Context(), connectivity.SessionPlan{}); err != nil {
		t.Fatalf("retried reconcile must dead-letter the held delivery: %v", err)
	}
	if log, _ := fake.snapshot(); !equalManagedStrings(log, []string{"write stale/held", "ack stale/held"}) {
		t.Fatalf("retry dead-letter/ACK log = %v, want one write then one ACK", log)
	}
	if !slices.Contains(*operations, "forget") {
		t.Fatalf("retried reconcile did not forget history: operations=%v", *operations)
	}
	if got := session.Router().PendingCount(); got != 0 {
		t.Fatalf("pending after retried reconcile = %d, want 0", got)
	}
}

func TestManagedSubscriptionDeadLetterAckFailureKeepsDeliveryPending(t *testing.T) {
	fake := &deadLetterFake{}
	session, store, operations := startHeldRemovedSubscription(t, fake, errors.New("PUBACK failed"))

	err := session.Reconcile(t.Context(), connectivity.SessionPlan{})
	requireTransientNotTerminal(t, session, err)
	if got := session.Router().PendingCount(); got != 1 {
		t.Fatalf("pending after failed ACK = %d, want 1", got)
	}
	if got := store.snapshot("safe-session-id"); !equalManagedStrings(got, []string{"stale/#"}) {
		t.Fatalf("history after failed ACK = %v, want preserved", got)
	}
	if slices.Contains(*operations, "forget") {
		t.Fatalf("failed ACK forgot history: operations=%v", *operations)
	}
}

func TestManagedSubscriptionDeadLetterWritesShareOneReconcileBudget(t *testing.T) {
	fake := &deadLetterFake{block: true}
	operations := []string{}
	store := &managedHistoryFake{operations: &operations, values: map[string]map[string]struct{}{
		"safe-session-id": {"stale/#": {}},
	}}
	conn := &managedConnFake{operations: &operations, unsubReasons: []byte{0x11}}
	session := newManagedTestSession(t, store, conn)
	session.opts.ReconcileTimeout = 50 * time.Millisecond
	session.SetRemovedSubscriptionDeadLetter(fake.write)
	if err := session.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = session.Close(context.Background()) })
	session.router.dispatch(&pahov5.Publish{Topic: "stale/held", QoS: 1}, fake.ack("stale/held", nil))

	// The reconcile context has no deadline: only the dead-letter budget can
	// end the blocked write.
	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- session.Reconcile(t.Context(), connectivity.SessionPlan{}) }()
	err := wait.RequireReceive(t, reconcileDone, 2*time.Second)
	requireTransientNotTerminal(t, session, err)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("budgeted reconcile error = %v, want the budget deadline as cause", err)
	}
	if got := session.Router().PendingCount(); got != 1 {
		t.Fatalf("pending after exhausted budget = %d, want 1", got)
	}
}

// reconcileOverlappingReplacement replaces the removed shared filter
// $share/old/a/# with $share/new/a/#, which covers the same topics. A live
// delivery on a/x reaches the replacement generation while the removed filter
// still gates it. It returns the topics the new filter's handler received,
// the durable history afterwards and the reconcile error.
func reconcileOverlappingReplacement(t *testing.T, fake *deadLetterFake) (<-chan string, []string, error) {
	t.Helper()
	const oldFilter, newFilter = "$share/old/a/#", "$share/new/a/#"
	operations := []string{}
	store := &managedHistoryFake{operations: &operations, values: map[string]map[string]struct{}{
		"safe-session-id": {oldFilter: {}},
	}}
	first := &managedConnFake{operations: &operations}
	replacement := &managedConnFake{operations: &operations}
	session := newManagedTestSession(t, store, first)
	if fake != nil {
		session.SetRemovedSubscriptionDeadLetter(fake.write)
	}
	dials := 0
	session.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		dials++
		if dials == 1 {
			return first, func() {}, nil
		}
		session.handleConnectionUp()
		session.router.dispatch(&pahov5.Publish{Topic: "a/x", QoS: 1}, func() error { return nil })
		return replacement, func() {}, nil
	}
	if err := session.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = session.Close(context.Background()) })
	delivered := make(chan string, 4)
	session.router.RegisterFiltered("replacement", []string{newFilter}, func(pub *pahov5.Publish, _ func() error) {
		delivered <- pub.Topic
	})

	plan := connectivity.SessionPlan{Subscriptions: []connectivity.SubscriptionPlan{{Topic: newFilter, QoS: 1}}}
	err := session.Reconcile(t.Context(), plan)
	return delivered, store.snapshot("safe-session-id"), err
}

func TestManagedSubscriptionOverlappingReplacementDeliversLiveTrafficInsteadOfDeadLettering(t *testing.T) {
	fake := &deadLetterFake{}
	delivered, history, err := reconcileOverlappingReplacement(t, fake)
	if err != nil {
		t.Fatalf("overlapping replacement must converge: %v", err)
	}
	if log, _ := fake.snapshot(); len(log) != 0 {
		t.Fatalf("live delivery for the replacement filter was dead-lettered: %v", log)
	}
	if got := wait.RequireReceive(t, delivered, 2*time.Second); got != "a/x" {
		t.Fatalf("replacement handler received %q, want a/x", got)
	}
	if !equalManagedStrings(history, []string{"$share/new/a/#"}) {
		t.Fatalf("history after overlapping replacement = %v, want only the new filter", history)
	}
}

func TestManagedSubscriptionOverlappingReplacementWithoutDeadLetterPathDoesNotFailClosed(t *testing.T) {
	delivered, history, err := reconcileOverlappingReplacement(t, nil)
	if err != nil {
		t.Fatalf("overlapping replacement without a dead-letter path must converge: %v", err)
	}
	if got := wait.RequireReceive(t, delivered, 2*time.Second); got != "a/x" {
		t.Fatalf("replacement handler received %q, want a/x", got)
	}
	if !equalManagedStrings(history, []string{"$share/new/a/#"}) {
		t.Fatalf("history after overlapping replacement = %v, want only the new filter", history)
	}
}

func TestRouterDeadLetterPendingStopsAtFirstFailure(t *testing.T) {
	r := newRouter(nil, nil)
	r.mu.Lock()
	r.quiesced = true
	r.mu.Unlock()
	fake := &deadLetterFake{}
	for _, topic := range []string{"stale/one", "other/kept", "stale/two", "stale/three"} {
		pub := &pahov5.Publish{Topic: topic, QoS: 1}
		if !r.reserveQueueSlot(pub, 1) {
			t.Fatalf("reserve dispatch slot for %s failed", topic)
		}
		r.dispatch(pub, fake.ack(topic, nil))
	}
	if got := r.PendingCount(); got != 4 {
		t.Fatalf("pending before dead-letter = %d, want 4", got)
	}

	calls := 0
	write := func(ctx context.Context, env *messaging.Envelope, filter string) error {
		calls++
		if calls == 2 {
			return errors.New("dead-letter store unavailable")
		}
		return fake.write(ctx, env, filter)
	}
	handled, err := r.deadLetterPending(t.Context(), []string{"stale/#"}, write)
	if err == nil || handled != 1 {
		t.Fatalf("deadLetterPending = (%d, %v), want (1, error)", handled, err)
	}
	if log, _ := fake.snapshot(); !equalManagedStrings(log, []string{"write stale/one", "ack stale/one"}) {
		t.Fatalf("dead-letter/ACK log = %v, want only stale/one handled", log)
	}
	r.mu.Lock()
	remaining := make([]string, 0, len(r.pending))
	for _, pending := range r.pending {
		remaining = append(remaining, pending.pub.Topic)
	}
	reserved := r.queueReserved
	r.mu.Unlock()
	if !equalManagedStrings(remaining, []string{"other/kept", "stale/two", "stale/three"}) {
		t.Fatalf("pending after failed dead-letter = %v", remaining)
	}
	if reserved != 3 {
		t.Fatalf("dispatch reservations after one handled entry = %d, want 3", reserved)
	}
}
