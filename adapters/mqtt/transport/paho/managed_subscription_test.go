package paho

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/mariotoffia/gobridge/domain/clock"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
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
	operations   *[]string
	subReasons   []byte
	unsubReasons []byte
	subscribed   []string
	unsubscribed []string
	disconnects  int
}

func (*managedConnFake) AwaitConnection(context.Context) error { return nil }
func (c *managedConnFake) Disconnect(context.Context) error {
	c.disconnects++
	*c.operations = append(*c.operations, "disconnect")
	return nil
}
func (c *managedConnFake) Subscribe(_ context.Context, subs []subscribeSpec) ([]byte, error) {
	*c.operations = append(*c.operations, "subscribe")
	for _, sub := range subs {
		c.subscribed = append(c.subscribed, sub.Topic)
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
func (c *managedConnFake) Unsubscribe(_ context.Context, topics []string) ([]byte, error) {
	*c.operations = append(*c.operations, "unsubscribe")
	c.unsubscribed = append(c.unsubscribed, topics...)
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
	session := NewSession(SessionOptions{ClientID: "client-a"}, connectivity.SessionPersistent, nil)
	session.managedStore = store
	session.managedIdentity = "safe-session-id"
	session.managedRequired = true
	session.connectOverride = func(context.Context) (pahoConnection, context.CancelFunc, error) {
		return conn, func() {}, nil
	}
	return session
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
	if err := session.Reconcile(t.Context(), plan); err == nil {
		t.Fatal("cleanup recycle must interrupt the old connection generation")
	}
	wantOps := []string{"list", "remember", "subscribe", "unsubscribe", "forget", "disconnect"}
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
	if err := session.Reconcile(t.Context(), empty); err == nil {
		t.Fatal("durable stale cleanup must recycle the connection generation")
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
	if err := session.Reconcile(t.Context(), connectivity.SessionPlan{}); err == nil {
		t.Fatal("successful retry cleanup must recycle the connection generation")
	}
	if got := store.snapshot("safe-session-id"); len(got) != 0 {
		t.Fatalf("history after successful retry = %v", got)
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
