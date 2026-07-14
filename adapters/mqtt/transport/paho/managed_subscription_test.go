package paho

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	pahov5 "github.com/eclipse/paho.golang/paho"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

type managedHistoryFake struct {
	mu          sync.Mutex
	values      map[string]map[string]struct{}
	listErr     error
	rememberErr error
	forgetErr   error
	operations  *[]string
}

func (s *managedHistoryFake) List(_ context.Context, identity string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	*s.operations = append(*s.operations, "list")
	if s.listErr != nil {
		return nil, s.listErr
	}
	set, ok := s.values[identity]
	if !ok {
		return nil, shared.ErrNotFound
	}
	out := make([]string, 0, len(set))
	for filter := range set {
		out = append(out, filter)
	}
	sort.Strings(out)
	return out, nil
}
func (s *managedHistoryFake) Remember(_ context.Context, identity string, filters []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	*s.operations = append(*s.operations, "remember")
	if s.rememberErr != nil {
		return s.rememberErr
	}
	set, ok := s.values[identity]
	if !ok {
		set = map[string]struct{}{}
		s.values[identity] = set
	}
	for _, filter := range filters {
		set[filter] = struct{}{}
	}
	return nil
}
func (s *managedHistoryFake) Forget(_ context.Context, identity string, filters []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	*s.operations = append(*s.operations, "forget")
	if s.forgetErr != nil {
		return s.forgetErr
	}
	set, ok := s.values[identity]
	if !ok {
		return shared.ErrNotFound
	}
	for _, filter := range filters {
		delete(set, filter)
	}
	return nil
}
func (s *managedHistoryFake) snapshot(identity string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.values[identity]))
	for filter := range s.values[identity] {
		out = append(out, filter)
	}
	sort.Strings(out)
	return out
}

type managedConnFake struct {
	operations        *[]string
	subReasons        []byte
	unsubReasons      []byte
	subErr            error
	unsubErr          error
	unsubEntered      chan struct{}
	unsubRelease      chan struct{}
	unsubOnce         sync.Once
	subscribed        []string
	unsubscribed      []string
	disconnects       int
	disconnectEntered chan struct{}
	disconnectOnce    sync.Once
}

func (*managedConnFake) AwaitConnection(context.Context) error { return nil }
func (c *managedConnFake) Disconnect(context.Context) error {
	c.disconnects++
	if c.disconnectEntered != nil {
		c.disconnectOnce.Do(func() { close(c.disconnectEntered) })
	}
	*c.operations = append(*c.operations, "disconnect")
	return nil
}
func (c *managedConnFake) Subscribe(_ context.Context, subs []subscribeSpec) ([]byte, error) {
	*c.operations = append(*c.operations, "subscribe")
	for _, sub := range subs {
		c.subscribed = append(c.subscribed, sub.Topic)
	}
	if c.subErr != nil {
		return nil, c.subErr
	}
	if c.subReasons != nil {
		return append([]byte(nil), c.subReasons...), nil
	}
	reasons := make([]byte, len(subs))
	for i := range subs {
		reasons[i] = subs[i].QoS
	}
	return reasons, nil
}
func (c *managedConnFake) Unsubscribe(ctx context.Context, topics []string) ([]byte, error) {
	*c.operations = append(*c.operations, "unsubscribe")
	c.unsubscribed = append(c.unsubscribed, topics...)
	if c.unsubEntered != nil {
		c.unsubOnce.Do(func() { close(c.unsubEntered) })
	}
	if c.unsubRelease != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.unsubRelease:
		}
	}
	if c.unsubErr != nil {
		return nil, c.unsubErr
	}
	if c.unsubReasons != nil {
		return append([]byte(nil), c.unsubReasons...), nil
	}
	reasons := make([]byte, len(topics))
	return reasons, nil
}
func (*managedConnFake) PublishEnvelope(context.Context, *messaging.Envelope, string, SenderOptions, clock.Clock) (publishResult, error) {
	return publishResult{}, nil
}
func (*managedConnFake) Underlying() *autopaho.ConnectionManager { return nil }

func newManagedTestSession(t *testing.T, store *managedHistoryFake, conn *managedConnFake) *Session {
	t.Helper()
	session := NewSession(SessionOptions{ClientID: "client-a", UnmatchedGrace: time.Millisecond}, connectivity.SessionPersistent, nil)
	session.managedStore = store
	session.managedIdentity = "safe-session-id"
	session.managedRequired = true
	session.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		return conn, func() {}, nil
	}
	return session
}

func TestRouterQuiesceForRecycleIsBoundedByContextWithStuckHandler(t *testing.T) {
	r := newRouter(nil, nil)
	handlerEntered := make(chan struct{})
	handlerRelease := make(chan struct{})
	r.RegisterFiltered("stuck", []string{"desired/#"}, func(_ *pahov5.Publish, _ func() error) {
		close(handlerEntered)
		<-handlerRelease
	})

	dispatchDone := make(chan struct{})
	go func() {
		r.dispatch(&pahov5.Publish{Topic: "desired/one", QoS: 1}, nil)
		close(dispatchDone)
	}()
	wait.RequireReceive(t, handlerEntered, 2*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.quiesceForRecycle(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("quiesce error = %v, want context cancellation", err)
	}

	// The timed-out quiesce remains fail-closed: later ingress cannot start a
	// second callback while the first callback is still stuck.
	var laterHandled atomic.Int32
	r.RegisterFiltered("later", []string{"later/#"}, func(_ *pahov5.Publish, _ func() error) {
		laterHandled.Add(1)
	})
	r.dispatch(&pahov5.Publish{Topic: "later/one", QoS: 1}, nil)
	if got := laterHandled.Load(); got != 0 {
		t.Fatalf("post-timeout dispatch callbacks = %d, want 0", got)
	}

	close(handlerRelease)
	wait.RequireReceive(t, dispatchDone, 2*time.Second)
}

func TestRouterQuiesceForRecycleWaitsForRuntimeSettlementAfterCallback(t *testing.T) {
	r := newRouter(nil, nil)
	callbackReturned := make(chan struct{})
	r.RegisterFiltered("accepted", []string{"desired/#"}, func(_ *pahov5.Publish, _ func() error) {
		close(callbackReturned)
	})
	r.dispatch(&pahov5.Publish{Topic: "desired/one", QoS: 1}, nil)
	wait.RequireReceive(t, callbackReturned, 2*time.Second)

	settlementEntered := make(chan struct{})
	settlementRelease := make(chan struct{})
	quiesced := make(chan error, 1)
	go func() {
		quiesced <- r.quiesceForRecycle(context.Background(), func(ctx context.Context) error {
			close(settlementEntered)
			select {
			case <-settlementRelease:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	wait.RequireReceive(t, settlementEntered, 2*time.Second)
	wait.Silent(t, quiesced, 25*time.Millisecond)
	close(settlementRelease)
	if err := wait.RequireReceive(t, quiesced, 2*time.Second); err != nil {
		t.Fatalf("quiesce after settlement: %v", err)
	}
}

func TestManagedCleanupStuckHandlerReturnsBoundedAndDisconnects(t *testing.T) {
	operations := []string{}
	store := &managedHistoryFake{operations: &operations, values: map[string]map[string]struct{}{
		"safe-session-id": {"stale/#": {}},
	}}
	conn := &managedConnFake{operations: &operations, unsubEntered: make(chan struct{}), unsubRelease: make(chan struct{})}
	session := newManagedTestSession(t, store, conn)
	if err := session.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = session.Close(context.Background()) })

	handlerEntered := make(chan struct{})
	handlerRelease := make(chan struct{})
	session.router.RegisterFiltered("desired", []string{"desired/#"}, func(_ *pahov5.Publish, _ func() error) {
		close(handlerEntered)
		<-handlerRelease
	})

	reconcileCtx, cancel := context.WithCancel(context.Background())
	plan := connectivity.SessionPlan{Subscriptions: []connectivity.SubscriptionPlan{{Topic: "desired/#", QoS: 1}}}
	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- session.Reconcile(reconcileCtx, plan) }()
	wait.RequireReceive(t, conn.unsubEntered, 2*time.Second)
	dispatchDone := make(chan struct{})
	go func() {
		session.router.dispatch(&pahov5.Publish{Topic: "desired/one", QoS: 1}, nil)
		close(dispatchDone)
	}()
	wait.RequireReceive(t, handlerEntered, 2*time.Second)
	close(conn.unsubRelease)
	wait.Until(t, 2*time.Second, "router enters recycle quiescence", func() bool {
		session.router.mu.Lock()
		defer session.router.mu.Unlock()
		return session.router.quiesced
	})
	cancel()

	err := wait.RequireReceive(t, reconcileDone, 2*time.Second)
	if !errors.Is(err, shared.ErrTransportClosedPermanently) {
		t.Fatalf("stuck-handler reconcile error = %v, want terminal marker", err)
	}
	if conn.disconnects != 1 || session.connection() != nil {
		t.Fatalf("stuck-handler timeout did not disconnect: disconnects=%d connection=%v", conn.disconnects, session.connection())
	}
	if health := session.Health(t.Context()); health.ServiceLevel == ports.ServiceLevelFull {
		t.Fatalf("stuck-handler timeout retained Full readiness: %+v", health)
	}

	close(handlerRelease)
	wait.RequireReceive(t, dispatchDone, 2*time.Second)
}

func TestManagedCleanupQuiescenceCancellationDisconnectsAndFailsClosed(t *testing.T) {
	operations := []string{}
	store := &managedHistoryFake{operations: &operations, values: map[string]map[string]struct{}{
		"safe-session-id": {"stale/#": {}},
	}}
	conn := &managedConnFake{operations: &operations}
	session := newManagedTestSession(t, store, conn)
	if err := session.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = session.Close(context.Background()) })

	settlementEntered := make(chan struct{})
	session.SetIngressQuiescenceWaiter(func(ctx context.Context) error {
		close(settlementEntered)
		<-ctx.Done()
		return ctx.Err()
	})

	reconcileCtx, cancel := context.WithCancel(context.Background())
	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- session.Reconcile(reconcileCtx, connectivity.SessionPlan{}) }()
	wait.RequireReceive(t, settlementEntered, 2*time.Second)
	cancel()
	err := wait.RequireReceive(t, reconcileDone, 2*time.Second)
	if !errors.Is(err, shared.ErrTransportClosedPermanently) {
		t.Fatalf("quiescence cancellation error = %v, want terminal transport marker", err)
	}
	if conn.disconnects != 1 {
		t.Fatalf("disconnects after quiescence cancellation = %d, want 1", conn.disconnects)
	}
	if session.connection() != nil {
		t.Fatal("connection remains active after quiescence cancellation")
	}
	if health := session.Health(t.Context()); health.ServiceLevel == ports.ServiceLevelFull {
		t.Fatalf("terminal quiescence failure retained Full readiness: %+v", health)
	}
}

func TestManagedSubscriptionPendingCleanupRemainsCoverageProtectedPastGrace(t *testing.T) {
	operations := []string{}
	store := &managedHistoryFake{operations: &operations, values: map[string]map[string]struct{}{
		"safe-session-id": {
			"sensors/#":                     {},
			"$share/group/shared-sensors/#": {},
		},
	}}
	conn := &managedConnFake{
		operations:   &operations,
		unsubErr:     errors.New("cleanup unavailable"),
		unsubEntered: make(chan struct{}),
		unsubRelease: make(chan struct{}),
	}
	session := newManagedTestSession(t, store, conn)
	if err := session.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = session.Close(context.Background()) })

	var acked atomic.Int32
	for _, topic := range []string{"sensors/one", "shared-sensors/two"} {
		session.router.dispatch(&pahov5.Publish{Topic: topic, QoS: 1, Payload: []byte("pending")}, func() error {
			acked.Add(1)
			return nil
		})
	}

	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- session.Reconcile(t.Context(), connectivity.SessionPlan{}) }()
	wait.RequireReceive(t, conn.unsubEntered, 2*time.Second)

	// Deterministically execute the post-grace classification while exact cleanup
	// is still blocked. Both ordinary and shared managed filters must retain their
	// concrete deliveries unacknowledged.
	session.router.sweepUnmatched()
	if got := acked.Load(); got != 0 {
		t.Fatalf("pending managed cleanup ACK-dropped %d deliveries past grace", got)
	}
	session.router.mu.Lock()
	pending := len(session.router.pending)
	session.router.mu.Unlock()
	if pending != 2 {
		t.Fatalf("pending deliveries after past-grace cleanup stall = %d, want 2", pending)
	}

	close(conn.unsubRelease)
	if err := <-reconcileDone; err == nil {
		t.Fatal("failed cleanup must keep reconciliation degraded")
	}
	if got := acked.Load(); got != 0 {
		t.Fatalf("failed cleanup ACK-dropped %d managed deliveries", got)
	}
}

func TestManagedSubscriptionCleanupGatesOverlappingDesiredHandlerAndPurgesWithoutACK(t *testing.T) {
	operations := []string{}
	store := &managedHistoryFake{operations: &operations, values: map[string]map[string]struct{}{
		"safe-session-id": {"$share/group/sensors/#": {}},
	}}
	conn := &managedConnFake{
		operations:   &operations,
		unsubEntered: make(chan struct{}),
		unsubRelease: make(chan struct{}),
	}
	session := newManagedTestSession(t, store, conn)
	if err := session.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = session.Close(context.Background()) })

	var handled atomic.Int32
	var acked atomic.Int32
	session.router.RegisterFiltered("desired", []string{"sensors/+/temperature"}, func(_ *pahov5.Publish, _ func() error) {
		handled.Add(1)
	})
	plan := connectivity.SessionPlan{Subscriptions: []connectivity.SubscriptionPlan{{Topic: "sensors/+/temperature", QoS: 1}}}
	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- session.Reconcile(t.Context(), plan) }()
	wait.RequireReceive(t, conn.unsubEntered, 2*time.Second)

	// The concrete topic matches the desired handler and the stale shared
	// wildcard. Cleanup gating must win before handler matching so this old-
	// generation delivery remains unacknowledged for broker redelivery.
	session.router.dispatch(&pahov5.Publish{Topic: "sensors/one/temperature", QoS: 1}, func() error {
		acked.Add(1)
		return nil
	})
	if got := handled.Load(); got != 0 {
		t.Fatalf("handler processed stale-shared delivery during cleanup: %d", got)
	}
	if got := acked.Load(); got != 0 {
		t.Fatalf("stale-shared delivery ACKed during cleanup: %d", got)
	}

	close(conn.unsubRelease)
	if err := <-reconcileDone; err != nil {
		t.Fatalf("successful cleanup/recycle: %v", err)
	}
	if got := handled.Load(); got != 0 {
		t.Fatalf("handler processed purged stale-shared delivery after recycle: %d", got)
	}
	if got := acked.Load(); got != 0 {
		t.Fatalf("recycle ACKed stale-shared delivery: %d", got)
	}
	session.router.mu.Lock()
	pending := len(session.router.pending)
	session.router.mu.Unlock()
	if pending != 0 {
		t.Fatalf("old-epoch pending deliveries after recycle = %d, want 0", pending)
	}
}

func TestManagedSubscriptionReloadAwaitsConnectionUpCompletionWithoutExtraRecycle(t *testing.T) {
	operations := []string{}
	store := &managedHistoryFake{operations: &operations, values: map[string]map[string]struct{}{
		"safe-session-id": {"old/#": {}},
	}}
	first := &managedConnFake{operations: &operations}
	replacement := &managedConnFake{operations: &operations}
	session := newManagedTestSession(t, store, first)
	dials := 0
	replacementDialed := make(chan struct{})
	session.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		dials++
		if dials == 1 {
			return first, func() {}, nil
		}
		close(replacementDialed)
		return replacement, func() {}, nil
	}
	if err := session.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	session.connectOverrideAwaitConnectionUp = true
	t.Cleanup(func() { _ = session.Close(context.Background()) })

	plan := connectivity.SessionPlan{Subscriptions: []connectivity.SubscriptionPlan{{Topic: "new/#", QoS: 1}}}
	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- session.Reconcile(t.Context(), plan) }()
	wait.RequireReceive(t, replacementDialed, 2*time.Second)
	wait.Silent(t, reconcileDone, 25*time.Millisecond)

	// Complete the exact replacement generation callback. Reconcile must now
	// converge that generation once, without an epoch mismatch or another reload.
	session.handleConnectionUp()
	if err := wait.RequireReceive(t, reconcileDone, 2*time.Second); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if dials != 2 {
		t.Fatalf("connection attempts = %d, want initial + one replacement", dials)
	}
	if first.disconnects != 1 || replacement.disconnects != 0 {
		t.Fatalf("disconnects first=%d replacement=%d, want 1/0", first.disconnects, replacement.disconnects)
	}
}

func TestStartConnectionUpBarrierUnblocksSafelyOnClose(t *testing.T) {
	operations := []string{}
	conn := &managedConnFake{operations: &operations}
	session := NewSession(SessionOptions{ClientID: "client-a"}, connectivity.SessionEphemeral, nil)
	dialed := make(chan struct{})
	session.connectOverrideAwaitConnectionUp = true
	session.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		close(dialed)
		return conn, func() {}, nil
	}
	startDone := make(chan error, 1)
	go func() { startDone <- session.Start(context.Background()) }()
	wait.RequireReceive(t, dialed, 2*time.Second)

	if err := session.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := wait.RequireReceive(t, startDone, 2*time.Second); err == nil {
		t.Fatal("Start must fail when Close cancels its callback barrier")
	}
	if conn.disconnects != 1 {
		t.Fatalf("fresh connection disconnects = %d, want 1", conn.disconnects)
	}
}

func TestManagedSubscriptionPinnedReplayFailsClosedAndPreservesHistory(t *testing.T) {
	operations := []string{}
	const staleFilter = "$share/group/stale/#"
	store := &managedHistoryFake{operations: &operations, values: map[string]map[string]struct{}{
		"safe-session-id": {staleFilter: {}},
	}}
	first := &managedConnFake{operations: &operations}
	replacement := &managedConnFake{operations: &operations, unsubReasons: []byte{0x11}}
	session := newManagedTestSession(t, store, first)
	dials := 0
	var staleACKs atomic.Int32
	session.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		dials++
		if dials == 1 {
			return first, func() {}, nil
		}
		session.handleConnectionUp()
		session.router.dispatch(&pahov5.Publish{Topic: "stale/one", QoS: 1}, func() error {
			staleACKs.Add(1)
			return nil
		})
		return replacement, func() {}, nil
	}
	if err := session.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = session.Close(context.Background()) })

	err := session.Reconcile(t.Context(), connectivity.SessionPlan{})
	if !errors.Is(err, shared.ErrTransportClosedPermanently) {
		t.Fatalf("pinned replay migration error = %v, want terminal marker", err)
	}
	if !strings.Contains(err.Error(), "restore the old configuration") {
		t.Fatalf("migration error does not describe restore/drain protocol: %v", err)
	}
	if got := store.snapshot("safe-session-id"); !equalManagedStrings(got, []string{staleFilter}) {
		t.Fatalf("durable history after pinned replay = %v, want preserved %q", got, staleFilter)
	}
	if got := staleACKs.Load(); got != 0 {
		t.Fatalf("pinned replay stale ACKs = %d, want 0", got)
	}
	if replacement.disconnects != 1 || session.connection() != nil {
		t.Fatalf("pinned replay did not fail closed: disconnects=%d connection=%v", replacement.disconnects, session.connection())
	}
	if health := session.Health(t.Context()); health.ServiceLevel == ports.ServiceLevelFull {
		t.Fatalf("pinned replay migration falsely reported Full: %+v", health)
	}
}

func TestManagedSubscriptionCleanupConvergesReplacementGeneration(t *testing.T) {
	operations := []string{}
	store := &managedHistoryFake{operations: &operations, values: map[string]map[string]struct{}{
		"safe-session-id": {"old/#": {}},
	}}
	first := &managedConnFake{operations: &operations}
	replacement := &managedConnFake{operations: &operations}
	session := newManagedTestSession(t, store, first)
	dials := 0
	session.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		dials++
		if dials == 1 {
			return first, func() {}, nil
		}
		session.handleConnectionUp()
		return replacement, func() {}, nil
	}
	if err := session.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = session.Close(context.Background()) })

	plan := connectivity.SessionPlan{Subscriptions: []connectivity.SubscriptionPlan{{Topic: "new/#", QoS: 1}}}
	if err := session.Reconcile(t.Context(), plan); err != nil {
		t.Fatalf("cleanup must converge the replacement generation before returning: %v", err)
	}
	if !equalManagedStrings(replacement.subscribed, []string{"new/#"}) {
		t.Fatalf("replacement subscriptions = %v, want [new/#]", replacement.subscribed)
	}
	if first.disconnects != 1 || replacement.disconnects != 0 {
		t.Fatalf("disconnects first=%d replacement=%d, want 1/0", first.disconnects, replacement.disconnects)
	}
	health := session.Health(t.Context())
	if health.SubscriptionsSatisfied == nil || !*health.SubscriptionsSatisfied {
		t.Fatalf("replacement generation did not converge subscriptions: %+v", health)
	}
}

func TestExclusiveManagedCleanupFailureDisconnectsReplacementBeforeLeaseRelease(t *testing.T) {
	operations := []string{}
	store := &managedHistoryFake{operations: &operations, values: map[string]map[string]struct{}{
		"safe-session-id": {"old/#": {}},
	}}
	first := &managedConnFake{
		operations:        &operations,
		unsubEntered:      make(chan struct{}),
		unsubRelease:      make(chan struct{}),
		disconnectEntered: make(chan struct{}),
	}
	replacement := &managedConnFake{operations: &operations, subErr: errors.New("replacement SUBSCRIBE failed")}
	session := newManagedTestSession(t, store, first)
	session.mode = connectivity.SessionExclusive
	dials := 0
	session.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		dials++
		if dials == 1 {
			return first, func() {}, nil
		}
		session.handleConnectionUp()
		return replacement, func() {}, nil
	}
	if err := session.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = session.Close(context.Background()) })

	handlerEntered := make(chan struct{})
	handlerRelease := make(chan struct{})
	session.router.RegisterFiltered("desired", []string{"new/#"}, func(_ *pahov5.Publish, _ func() error) {
		close(handlerEntered)
		<-handlerRelease
	})
	plan := connectivity.SessionPlan{Subscriptions: []connectivity.SubscriptionPlan{{Topic: "new/#", QoS: 1}}}
	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- session.Reconcile(t.Context(), plan) }()
	wait.RequireReceive(t, first.unsubEntered, 2*time.Second)
	dispatchDone := make(chan struct{})
	go func() {
		session.router.dispatch(&pahov5.Publish{Topic: "new/one", QoS: 1}, nil)
		close(dispatchDone)
	}()
	wait.RequireReceive(t, handlerEntered, 2*time.Second)
	close(first.unsubRelease)
	wait.Silent(t, first.disconnectEntered, 25*time.Millisecond)
	close(handlerRelease)
	wait.RequireReceive(t, dispatchDone, 2*time.Second)
	wait.RequireReceive(t, first.disconnectEntered, 2*time.Second)
	if err := wait.RequireReceive(t, reconcileDone, 2*time.Second); err == nil {
		t.Fatal("replacement convergence failure must propagate")
	}
	// The exclusive manager releases its lease after Reconcile returns. A nil
	// connection and a disconnected replacement prove no broker consumer can
	// survive into that lease-release boundary.
	if replacement.disconnects != 1 {
		t.Fatalf("replacement disconnects before lease release = %d, want 1", replacement.disconnects)
	}
	if session.connection() != nil {
		t.Fatal("replacement connection remains active at the lease-release boundary")
	}
}

func TestManagedSubscriptionsWriteAheadExactCleanupAndRecycle(t *testing.T) {
	operations := []string{}
	store := &managedHistoryFake{operations: &operations, values: map[string]map[string]struct{}{
		"safe-session-id": {"sensors/#": {}, "$share/group/sensors/#": {}},
	}}
	conn := &managedConnFake{operations: &operations}
	session := newManagedTestSession(t, store, conn)
	if err := session.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	plan := connectivity.SessionPlan{Subscriptions: []connectivity.SubscriptionPlan{{Topic: "sensors/new/#", QoS: 1}}}
	if err := session.Reconcile(t.Context(), plan); err != nil {
		t.Fatalf("cleanup must converge the replacement generation: %v", err)
	}
	wantOps := []string{"list", "remember", "subscribe", "unsubscribe", "disconnect", "forget", "remember", "subscribe"}
	if !equalManagedStrings(operations, wantOps) {
		t.Fatalf("operations = %v, want %v", operations, wantOps)
	}
	sort.Strings(conn.unsubscribed)
	wantUnsub := []string{"$share/group/sensors/#", "sensors/#"}
	if !equalManagedStrings(conn.unsubscribed, wantUnsub) {
		t.Fatalf("UNSUBSCRIBE = %v, want exact history %v", conn.unsubscribed, wantUnsub)
	}
	if got := store.snapshot("safe-session-id"); !equalManagedStrings(got, []string{"sensors/new/#"}) {
		t.Fatalf("history = %v", got)
	}
	if conn.disconnects != 1 {
		t.Fatalf("disconnects = %d, want recycle", conn.disconnects)
	}
}

func TestManagedSubscriptionsEmptyAppliedPlanDoesNotHideDurableHistory(t *testing.T) {
	operations := []string{}
	store := &managedHistoryFake{operations: &operations, values: map[string]map[string]struct{}{
		"safe-session-id": {"stale/#": {}},
	}}
	conn := &managedConnFake{operations: &operations}
	session := newManagedTestSession(t, store, conn)
	if err := session.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	empty := connectivity.SessionPlan{}
	session.appliedPlan = &empty
	if err := session.Reconcile(t.Context(), empty); err != nil {
		t.Fatalf("durable stale cleanup must converge after recycling: %v", err)
	}
	if !equalManagedStrings(conn.unsubscribed, []string{"stale/#"}) {
		t.Fatalf("UNSUBSCRIBE = %v, want durable stale filter", conn.unsubscribed)
	}
}

func TestManagedSubscriptionsPartialSUBACKRetainsWriteAheadCandidates(t *testing.T) {
	operations := []string{}
	store := &managedHistoryFake{operations: &operations, values: map[string]map[string]struct{}{"safe-session-id": {}}}
	conn := &managedConnFake{operations: &operations, subReasons: []byte{0x00, 0x87}}
	session := newManagedTestSession(t, store, conn)
	if err := session.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	plan := connectivity.SessionPlan{Subscriptions: []connectivity.SubscriptionPlan{{Topic: "a/#"}, {Topic: "b/#"}}}
	if err := session.Reconcile(t.Context(), plan); err == nil {
		t.Fatal("partial SUBACK must fail reconcile")
	}
	if got := store.snapshot("safe-session-id"); !equalManagedStrings(got, []string{"a/#", "b/#"}) {
		t.Fatalf("write-ahead history = %v", got)
	}
	if len(operations) < 3 || operations[1] != "remember" || operations[2] != "subscribe" {
		t.Fatalf("write-ahead ordering = %v", operations)
	}
}

func TestManagedSubscriptionsPartialUNSUBACKForgetsOnlyConfirmed(t *testing.T) {
	operations := []string{}
	store := &managedHistoryFake{operations: &operations, values: map[string]map[string]struct{}{"safe-session-id": {"old/a/#": {}, "old/b/#": {}}}}
	conn := &managedConnFake{operations: &operations, unsubReasons: []byte{0x00, 0x80}}
	session := newManagedTestSession(t, store, conn)
	if err := session.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := session.Reconcile(t.Context(), connectivity.SessionPlan{}); err == nil {
		t.Fatal("partial UNSUBACK must fail reconcile")
	}
	if got := store.snapshot("safe-session-id"); !equalManagedStrings(got, []string{"old/b/#"}) {
		t.Fatalf("history after partial UNSUBACK = %v", got)
	}
	if conn.disconnects != 1 {
		t.Fatalf("successful partial cleanup must recycle, disconnects=%d", conn.disconnects)
	}
}

func TestManagedSubscriptionRememberOutagePreventsSubscribe(t *testing.T) {
	operations := []string{}
	store := &managedHistoryFake{operations: &operations, values: map[string]map[string]struct{}{"safe-session-id": {}}, rememberErr: errors.New("history unavailable")}
	conn := &managedConnFake{operations: &operations}
	session := newManagedTestSession(t, store, conn)
	if err := session.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	plan := connectivity.SessionPlan{Subscriptions: []connectivity.SubscriptionPlan{{Topic: "sensors/#"}}}
	if err := session.Reconcile(t.Context(), plan); err == nil {
		t.Fatal("Remember outage must fail reconcile")
	}
	if len(conn.subscribed) != 0 {
		t.Fatalf("SUBSCRIBE ran before durable write-ahead: %v", conn.subscribed)
	}
	if health := session.Health(t.Context()); health.ServiceLevel == ports.ServiceLevelFull {
		t.Fatalf("history outage reported Full health: %+v", health)
	}
}

func TestManagedSubscriptionForgetOutageRetainsHistoryForRetry(t *testing.T) {
	operations := []string{}
	store := &managedHistoryFake{operations: &operations, values: map[string]map[string]struct{}{"safe-session-id": {"old/#": {}}}, forgetErr: errors.New("history unavailable")}
	conn := &managedConnFake{operations: &operations}
	session := newManagedTestSession(t, store, conn)
	if err := session.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := session.Reconcile(t.Context(), connectivity.SessionPlan{}); err == nil {
		t.Fatal("Forget outage must fail reconcile")
	}
	if got := store.snapshot("safe-session-id"); !equalManagedStrings(got, []string{"old/#"}) {
		t.Fatalf("history after failed Forget = %v", got)
	}
	if conn.disconnects != 1 {
		t.Fatalf("confirmed broker cleanup must recycle despite failed Forget, disconnects=%d", conn.disconnects)
	}
	store.mu.Lock()
	store.forgetErr = nil
	store.mu.Unlock()
	if err := session.Reconcile(t.Context(), connectivity.SessionPlan{}); err != nil {
		t.Fatalf("successful retry cleanup must converge after recycling: %v", err)
	}
	if got := store.snapshot("safe-session-id"); len(got) != 0 {
		t.Fatalf("history after successful retry = %v", got)
	}
}

func TestManagedSubscriptionForgetRetryAfterNoSubscriptionDoesNotRecycleAgain(t *testing.T) {
	operations := []string{}
	store := &managedHistoryFake{
		operations: &operations,
		values:     map[string]map[string]struct{}{"safe-session-id": {"old/#": {}}},
		forgetErr:  errors.New("history unavailable"),
	}
	conn := &managedConnFake{operations: &operations}
	session := newManagedTestSession(t, store, conn)
	if err := session.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := session.Reconcile(t.Context(), connectivity.SessionPlan{}); err == nil {
		t.Fatal("initial Forget outage must fail reconcile")
	}
	if conn.disconnects != 1 {
		t.Fatalf("initial confirmed removal disconnects = %d, want 1", conn.disconnects)
	}

	conn.unsubReasons = []byte{0x11}
	if err := session.Reconcile(t.Context(), connectivity.SessionPlan{}); err == nil {
		t.Fatal("retry Forget outage must keep reconcile degraded")
	}
	if conn.disconnects != 1 {
		t.Fatalf("broker already-absent retry recycled again: disconnects=%d, want 1", conn.disconnects)
	}
	if got := store.snapshot("safe-session-id"); !equalManagedStrings(got, []string{"old/#"}) {
		t.Fatalf("failed Forget must retain history, got %v", got)
	}
}

func TestManagedSubscriptionHistoryOutagePreventsBrokerActivation(t *testing.T) {
	operations := []string{}
	store := &managedHistoryFake{operations: &operations, values: map[string]map[string]struct{}{}, listErr: errors.New("history unavailable")}
	conn := &managedConnFake{operations: &operations}
	session := newManagedTestSession(t, store, conn)
	dials := 0
	session.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		dials++
		return conn, func() {}, nil
	}
	if err := session.Start(t.Context()); err == nil {
		t.Fatal("history outage must fail Start")
	}
	if dials != 0 {
		t.Fatalf("broker activated before history load: dials=%d", dials)
	}
	if health := session.Health(t.Context()); health.ServiceLevel == ports.ServiceLevelFull {
		t.Fatalf("history outage reported Full health: %+v", health)
	}
}

func TestManagedSubscriptionMissingBaselinePreventsBrokerActivation(t *testing.T) {
	operations := []string{}
	store := &managedHistoryFake{operations: &operations, values: map[string]map[string]struct{}{}}
	conn := &managedConnFake{operations: &operations}
	session := newManagedTestSession(t, store, conn)
	dials := 0
	session.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		dials++
		return conn, func() {}, nil
	}
	if err := session.Start(t.Context()); !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("Start error = %v, want ErrNotFound chain", err)
	}
	if dials != 0 {
		t.Fatalf("broker activated without baseline: dials=%d", dials)
	}
}

func equalManagedStrings(a, b []string) bool {
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
