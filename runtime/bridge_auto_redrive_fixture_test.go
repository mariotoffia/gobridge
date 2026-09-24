package runtime_test

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	runsession "github.com/mariotoffia/gobridge/runtime/session"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

const (
	// autoRedriveFilter is the managed subscription the fixture's records were
	// dead-lettered for and the tests add back.
	autoRedriveFilter = "sensors/+/temp"
	// autoRedriveWait bounds every wait for an automatic redrive outcome.
	autoRedriveWait = 10 * time.Second
	// autoRedrivePassFinished is the debug line the runtime logs when a pass ends.
	autoRedrivePassFinished = "automatic redrive pass finished"
	// autoRedriveAuditAction is the audit action of one automatically redriven record.
	autoRedriveAuditAction = "dlq.redrive.auto"
)

// orderedDLQStore is a DLQ store double that honours the ports.DLQReader
// contract an automatic redrive pages by: List returns entries oldest first by
// FailedAt with the entry ID as tiebreak, Since inclusive, Before exclusive, and
// at most Limit entries. Write refuses an ID it already holds. Like a real
// store, Get, List and Delete fail on a context that has ended.
type orderedDLQStore struct {
	mu      sync.Mutex
	entries map[string]routing.DLQEntry
}

var _ ports.DLQStore = (*orderedDLQStore)(nil)

func newOrderedDLQStore() *orderedDLQStore {
	return &orderedDLQStore{entries: make(map[string]routing.DLQEntry)}
}

func (s *orderedDLQStore) Write(_ context.Context, e routing.DLQEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[e.ID()]; ok {
		return shared.ErrDuplicateRecord
	}
	s.entries[e.ID()] = e
	return nil
}

func (s *orderedDLQStore) Get(ctx context.Context, id string) (routing.DLQEntry, error) {
	if err := ctx.Err(); err != nil {
		return routing.DLQEntry{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	if !ok {
		return routing.DLQEntry{}, shared.ErrNotFound
	}
	return e, nil
}

func (s *orderedDLQStore) List(ctx context.Context, f routing.DLQFilter) ([]routing.DLQEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []routing.DLQEntry
	for _, e := range s.entries {
		switch {
		case f.RouteID != "" && e.RouteID() != f.RouteID,
			f.Category != "" && e.Category() != f.Category,
			!f.Since.IsZero() && e.FailedAt().Before(f.Since),
			!f.Before.IsZero() && !e.FailedAt().Before(f.Before):
			continue
		}
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b routing.DLQEntry) int {
		if c := a.FailedAt().Compare(b.FailedAt()); c != 0 {
			return c
		}
		return strings.Compare(a.ID(), b.ID())
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

func (s *orderedDLQStore) Delete(ctx context.Context, ids []string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, id := range ids {
		if _, ok := s.entries[id]; ok {
			delete(s.entries, id)
			n++
		}
	}
	return n, nil
}

func (s *orderedDLQStore) DeleteByFilter(context.Context, routing.DLQFilter) (int, error) {
	return 0, shared.ErrNotSupported
}

func (s *orderedDLQStore) Purge(context.Context, time.Time) (int, error) {
	return 0, shared.ErrNotSupported
}

// ids returns the IDs of every entry the store holds, sorted.
func (s *orderedDLQStore) ids() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.entries))
	for id := range s.entries {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

func (s *orderedDLQStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// autoRedriveSession is a managed MQTT-like session: it takes the
// removed-subscription dead-letter path, reports its managed identity, keeps
// the subscription-added hook the runtime installs, and reports the health the
// test sets (down = not connected, nothing subscribed).
type autoRedriveSession struct {
	*deadLetterSession
	down        atomic.Bool
	healthCalls atomic.Int64
	hookMu      sync.Mutex
	hookSets    int
	hook        func([]string)
}

func newAutoRedriveSession(identity string) *autoRedriveSession {
	s := &autoRedriveSession{deadLetterSession: newDeadLetterSession()}
	s.identity = identity
	return s
}

func (s *autoRedriveSession) SetSubscriptionAddedHook(fn func([]string)) {
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	s.hookSets++
	s.hook = fn
}

func (s *autoRedriveSession) hookInstalls() int {
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	return s.hookSets
}

// subscriptionAdded calls the installed hook the way the session does after the
// broker grants a subscription that was not in its history.
func (s *autoRedriveSession) subscriptionAdded(tb testing.TB, filters ...string) {
	tb.Helper()
	s.hookMu.Lock()
	fn := s.hook
	s.hookMu.Unlock()
	if fn == nil {
		tb.Fatalf("runtime installed no subscription-added hook")
	}
	fn(filters)
}

func (s *autoRedriveSession) Health(context.Context) ports.SessionHealth {
	s.healthCalls.Add(1)
	up := !s.down.Load()
	return ports.SessionHealth{Connected: up, Ready: up, SubscriptionsSatisfied: &up, ServiceLevel: ports.ServiceLevelFull}
}

// redriveSender records, by payload, every message it is handed and every one
// it delivers. fail, when set, decides whether a send fails; it gets the send
// context, so it can also block until the runtime cancels the send.
type redriveSender struct {
	mu        sync.Mutex
	fail      func(ctx context.Context, payload string) error
	attempted []string
	sent      []string
}

var _ ports.Sender = (*redriveSender)(nil)

func (s *redriveSender) Send(ctx context.Context, msg ports.OutboundMessage) error {
	payload := string(msg.Envelope.Payload())
	s.mu.Lock()
	fail := s.fail
	s.attempted = append(s.attempted, payload)
	s.mu.Unlock()
	if fail != nil {
		if err := fail(ctx, payload); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, payload)
	return nil
}

func (s *redriveSender) setFail(fn func(ctx context.Context, payload string) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = fn
}

func (s *redriveSender) tried() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.attempted)
}

func (s *redriveSender) delivered() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.sent)
}

// recordingAudit keeps every audit event the runtime logs.
type recordingAudit struct {
	mu     sync.Mutex
	events []ports.AuditEvent
}

func (a *recordingAudit) Log(_ context.Context, ev ports.AuditEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, ev)
}

// autoRedrives returns the automatic-redrive events, in the order logged.
func (a *recordingAudit) autoRedrives() []ports.AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []ports.AuditEvent
	for _, ev := range a.events {
		if ev.Action == autoRedriveAuditAction {
			out = append(out, ev)
		}
	}
	return out
}

// autoRedriveFixture is one runtime with route r1, a direct_hold route whose
// receiver subscribes through the managed session plant-a, over a DLQ store.
type autoRedriveFixture struct {
	clk     *clocktest.Fake
	store   *orderedDLQStore
	sess    *autoRedriveSession
	sender  *redriveSender
	audit   *recordingAudit
	metrics *ports.RecordingExporter
	logs    *logCaptureHandler
	rt      *goruntime.Runtime
}

// newAutoRedriveFixture builds, without starting it, a runtime over its own
// store whose session reports identity id-1. opts apply after the fixture's.
func newAutoRedriveFixture(tb testing.TB, opts ...goruntime.Option) *autoRedriveFixture {
	tb.Helper()
	return newAutoRedriveMember(tb, newOrderedDLQStore(), "auto-redrive", "id-1", opts...)
}

// newAutoRedriveMember builds, without starting it, one cluster member over a
// shared store: its own instance ID, clock, session identity and sender.
func newAutoRedriveMember(tb testing.TB, store *orderedDLQStore, instanceID, identity string, opts ...goruntime.Option) *autoRedriveFixture {
	tb.Helper()
	return newAutoRedriveRuntime(tb, store, instanceID, identity, autoRedriveRoute("r1"), opts...)
}

// newAutoRedriveRuntime builds, without starting it, a runtime over store that
// adds route with the managed session plant-a as its session argument.
func newAutoRedriveRuntime(tb testing.TB, store *orderedDLQStore, instanceID, identity string, route goruntime.RouteConfig, opts ...goruntime.Option) *autoRedriveFixture {
	tb.Helper()
	f := &autoRedriveFixture{
		clk:     clocktest.New(),
		store:   store,
		sess:    newAutoRedriveSession(identity),
		sender:  &redriveSender{},
		audit:   &recordingAudit{},
		metrics: &ports.RecordingExporter{},
		logs:    &logCaptureHandler{},
	}
	base := []goruntime.Option{
		goruntime.WithInstanceID(instanceID),
		goruntime.WithClock(f.clk),
		goruntime.WithDLQStore(store),
		goruntime.WithAuditLogger(f.audit),
		goruntime.WithMetrics(f.metrics),
		goruntime.WithLogger(slog.New(f.logs)),
		goruntime.WithAutoRedriveWindow(24 * time.Hour),
	}
	f.rt = goruntime.New(append(base, opts...)...)
	sessCfg := runsession.Config{SessionID: "plant-a"}
	if err := f.rt.AddRoute(route, NewFakeReceiver(), f.sender, f.sess, &sessCfg); err != nil {
		tb.Fatalf("AddRoute: %v", err)
	}
	return f
}

// autoRedriveRoute is a direct_hold route on session plant-a that sends once
// per delivery and drops a message whose send fails permanently or that expired.
func autoRedriveRoute(id string) goruntime.RouteConfig {
	return goruntime.RouteConfig{
		ID:              id,
		SourceSessionID: "plant-a",
		Policy: routing.RoutePolicy{
			DeliveryMode:       routing.DeliveryDirectHold,
			MaxReplayAttempts:  3,
			SendTimeout:        30 * time.Second,
			OnPermanentFailure: routing.FailureDrop,
			OnExpired:          routing.ExpiredDrop,
			SendRetryBudget:    routing.SendRetryBudgetDisabled,
		},
		Bindings:           []routing.DestinationBinding{{ID: "b1", Address: "out/temp"}},
		SourceCapabilities: []ports.Capability{ports.CapVisibilityExtension, ports.CapSourceRedelivery},
	}
}

func (f *autoRedriveFixture) start(tb testing.TB) {
	tb.Helper()
	if err := f.rt.Start(context.Background()); err != nil {
		tb.Fatalf("Start: %v", err)
	}
	tb.Cleanup(func() { _ = f.rt.Stop(context.Background()) })
}

// seed stores record(id, age, mutate...).
func (f *autoRedriveFixture) seed(tb testing.TB, id string, age time.Duration, mutate ...func(*routing.DLQEntrySpec)) {
	tb.Helper()
	if err := f.store.Write(context.Background(), f.record(id, age, mutate...)); err != nil {
		tb.Fatalf("seed %s: %v", id, err)
	}
}

// record is the record the removed-subscription path writes for a message
// whose payload is id, failed age before the fake clock's now. mutate adjusts
// it before it is built.
func (f *autoRedriveFixture) record(id string, age time.Duration, mutate ...func(*routing.DLQEntrySpec)) routing.DLQEntry {
	spec := routing.DLQEntrySpec{
		ID:          id,
		Envelope:    *messaging.MustEnvelope(messaging.EnvelopeInput{ID: "msg-" + id, Subject: "sensors/a/temp", Payload: []byte(id)}),
		RouteID:     "r1",
		Address:     autoRedriveFilter,
		SessionID:   "plant-a",
		SourceID:    "plant-a",
		Category:    "permanent",
		ErrorCode:   string(shared.ErrCodeSubscriptionRemoved),
		FailedAt:    f.clk.Now().Add(-age),
		RedriveMode: routing.RedriveAuto,
		ExtraInfo: map[string]string{
			routing.ExtraInfoSessionID:       "plant-a",
			routing.ExtraInfoSubscription:    autoRedriveFilter,
			routing.ExtraInfoManagedIdentity: f.sess.identity,
		},
	}
	for _, m := range mutate {
		m(&spec)
	}
	return routing.NewDLQEntry(spec)
}

// eventually waits for cond, moving the fake clock a second per poll so a pass
// waiting for the runtime to be ready re-checks.
func (f *autoRedriveFixture) eventually(tb testing.TB, desc string, cond func() bool) {
	tb.Helper()
	if !wait.Poll(autoRedriveWait, func() bool { f.clk.Advance(time.Second); return cond() }) {
		tb.Fatalf("%s: not reached within %s", desc, autoRedriveWait)
	}
}

// logged counts the log lines with exactly msg.
func (f *autoRedriveFixture) logged(msg string) int {
	n := 0
	for _, m := range f.logs.Messages() {
		if m == msg {
			n++
		}
	}
	return n
}

// waitPasses waits until n automatic redrive passes have finished.
func (f *autoRedriveFixture) waitPasses(tb testing.TB, n int) {
	tb.Helper()
	f.eventually(tb, "automatic redrive passes finished", func() bool { return f.logged(autoRedrivePassFinished) >= n })
}

// counted sums the named counter over the entries tagged with route r1.
func (f *autoRedriveFixture) counted(name string) int64 {
	var n int64
	for _, e := range f.metrics.FindEntries(name) {
		if metricHasTag(e, shared.TagKeyRouteID, "r1") {
			n += e.IValue
		}
	}
	return n
}
