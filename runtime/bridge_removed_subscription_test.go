package runtime_test

import (
	"context"
	"maps"
	"sync"
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	runsession "github.com/mariotoffia/gobridge/runtime/session"
)

// deadLetterSession is a FakeSession that also accepts the removed-subscription
// dead-letter path, recording what the runtime installed. It reports identity
// as its managed subscription identity; "" means it keeps no managed history.
type deadLetterSession struct {
	*FakeSession
	identity  string
	mu        sync.Mutex
	installed int
	fn        func(context.Context, *messaging.Envelope, string) error
}

func newDeadLetterSession() *deadLetterSession {
	return &deadLetterSession{FakeSession: NewFakeSession()}
}

func (s *deadLetterSession) ManagedSubscriptionIdentity() string { return s.identity }

func (s *deadLetterSession) SetRemovedSubscriptionDeadLetter(fn func(context.Context, *messaging.Envelope, string) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.installed++
	s.fn = fn
}

func (s *deadLetterSession) deadLetter(t *testing.T) func(context.Context, *messaging.Envelope, string) error {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fn == nil {
		t.Fatalf("runtime installed no removed-subscription dead-letter path (installs=%d)", s.installed)
	}
	return s.fn
}

func (s *deadLetterSession) installs() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.installed
}

func startRuntime(t *testing.T, rt *goruntime.Runtime) {
	t.Helper()
	if err := rt.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = rt.Stop(context.Background()) })
}

// writeHeldDelivery drives the installed callback the way the MQTT session does
// for a delivery held for a removed filter, and returns the one DLQ entry.
func writeHeldDelivery(t *testing.T, sess *deadLetterSession, store *FakeDLQStore, filter string) routing.DLQEntry {
	t.Helper()
	env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "held-1", Subject: "stale/one"})
	if err := sess.deadLetter(t)(context.Background(), env, filter); err != nil {
		t.Fatalf("dead-letter write: %v", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.Entries) != 1 {
		t.Fatalf("DLQ entries after one dead-letter write = %d, want 1", len(store.Entries))
	}
	return store.Entries[0]
}

func TestRemovedSubscriptionDeadLetterWritesSubscriptionRemovedEntry(t *testing.T) {
	store := NewFakeDLQStore()
	rt := goruntime.New(goruntime.WithInstanceID("removed-sub-dlq"), goruntime.WithDLQStore(store))
	cfg, recv, sender := helperQuiescentRoute("source-route", nil)
	cfg.SourceSessionID = "source-session"
	sess := newDeadLetterSession()
	sessCfg := runsession.Config{SessionID: "source-session"}
	if err := rt.AddRoute(cfg, recv, sender, sess, &sessCfg); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	startRuntime(t, rt)

	const filter = "$share/group/stale/#"
	entry := writeHeldDelivery(t, sess, store, filter)
	if entry.ErrorCode() != string(shared.ErrCodeSubscriptionRemoved) {
		t.Fatalf("DLQ error code = %q, want %q", entry.ErrorCode(), shared.ErrCodeSubscriptionRemoved)
	}
	if entry.SessionID() != "source-session" || entry.RouteID() != "source-route" || entry.Address() != filter {
		t.Fatalf("DLQ entry session/route/address = %q/%q/%q, want source-session/source-route/%s",
			entry.SessionID(), entry.RouteID(), entry.Address(), filter)
	}
	if entry.SourceID() != "source-session" {
		t.Fatalf("DLQ entry source = %q, want the session ID source-session", entry.SourceID())
	}
	if entry.Category() != "permanent" {
		t.Fatalf("DLQ entry category = %q, want permanent", entry.Category())
	}
}

// A session that names its managed identity gets records an automatic redrive
// may act on: the mode is auto and the facts name the session, the removed
// filter and the identity the subscription history is stored under.
func TestRemovedSubscriptionDeadLetterMarksTheRecordForAutomaticRedrive(t *testing.T) {
	store := NewFakeDLQStore()
	rt := goruntime.New(goruntime.WithInstanceID("removed-sub-auto"), goruntime.WithDLQStore(store))
	cfg, recv, sender := helperQuiescentRoute("source-route", nil)
	cfg.SourceSessionID = "source-session"
	sess := newDeadLetterSession()
	sess.identity = "id-1"
	sessCfg := runsession.Config{SessionID: "source-session"}
	if err := rt.AddRoute(cfg, recv, sender, sess, &sessCfg); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	startRuntime(t, rt)

	entry := writeHeldDelivery(t, sess, store, "sensors/+/temp")
	if entry.RedriveMode() != routing.RedriveAuto {
		t.Fatalf("DLQ redrive mode = %q, want %q", entry.RedriveMode(), routing.RedriveAuto)
	}
	want := map[string]string{
		routing.ExtraInfoSessionID:       "source-session",
		routing.ExtraInfoSubscription:    "sensors/+/temp",
		routing.ExtraInfoManagedIdentity: "id-1",
	}
	if got := entry.ExtraInfo(); !maps.Equal(got, want) {
		t.Fatalf("DLQ extra info = %v, want %v", got, want)
	}
}

// A session that keeps no managed history reports "" and gets a manual record
// with no facts, as before automatic redrive existed.
func TestRemovedSubscriptionDeadLetterWithoutIdentityWritesAManualRecord(t *testing.T) {
	store := NewFakeDLQStore()
	rt := goruntime.New(goruntime.WithInstanceID("removed-sub-manual"), goruntime.WithDLQStore(store))
	cfg, recv, sender := helperQuiescentRoute("source-route", nil)
	cfg.SourceSessionID = "source-session"
	sess := newDeadLetterSession()
	sessCfg := runsession.Config{SessionID: "source-session"}
	if err := rt.AddRoute(cfg, recv, sender, sess, &sessCfg); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	startRuntime(t, rt)

	entry := writeHeldDelivery(t, sess, store, "sensors/+/temp")
	if entry.RedriveMode() != routing.RedriveManual {
		t.Fatalf("DLQ redrive mode = %q, want manual", entry.RedriveMode())
	}
	if got := entry.ExtraInfo(); got != nil {
		t.Fatalf("DLQ extra info = %v, want nil", got)
	}
}

func TestRemovedSubscriptionDeadLetterKeepsOneEntryPerSessionForSameEnvelopeID(t *testing.T) {
	store := NewFakeDLQStore()
	rt := goruntime.New(goruntime.WithInstanceID("removed-sub-per-session"), goruntime.WithDLQStore(store))
	sessions := map[string]*deadLetterSession{"session-a": newDeadLetterSession(), "session-b": newDeadLetterSession()}
	for sid, sess := range sessions {
		if err := rt.RegisterIngressSession(runsession.Config{SessionID: sid}, sess); err != nil {
			t.Fatalf("RegisterIngressSession(%s): %v", sid, err)
		}
	}
	startRuntime(t, rt)

	// Both producers reuse one message ID; neither session carries a route, so
	// only the session scopes the record's identity.
	for sid, sess := range sessions {
		env := messaging.MustEnvelope(messaging.EnvelopeInput{ID: "same-producer-id", Subject: "stale/one"})
		if err := sess.deadLetter(t)(context.Background(), env, "stale/#"); err != nil {
			t.Fatalf("dead-letter write through %s: %v", sid, err)
		}
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.Entries) != 2 {
		t.Fatalf("DLQ entries = %d, want 2", len(store.Entries))
	}
	a, b := store.Entries[0], store.Entries[1]
	if a.ID() == b.ID() {
		t.Fatalf("two sessions' deliveries share DLQ entry ID %q; the store would keep only the first", a.ID())
	}
	if a.SessionID() == b.SessionID() || sessions[a.SessionID()] == nil || sessions[b.SessionID()] == nil {
		t.Fatalf("DLQ entry sessions = %q/%q, want one each of session-a and session-b", a.SessionID(), b.SessionID())
	}
}

func TestRemovedSubscriptionDeadLetterResolvesSessionSenderBehindRouteWithoutSession(t *testing.T) {
	store := NewFakeDLQStore()
	rt := newTestRuntime("removed-sub-session-sender", NewFakeOutboxStore(), NewFakeLeaseStore(), store)
	fan := newDeadLetterSession()
	fanCfg := fastSessionConfig("fan-session")
	if err := rt.RegisterSessionSender(fanCfg, fan, NewFakeSender()); err != nil {
		t.Fatalf("RegisterSessionSender: %v", err)
	}
	// The route names fan-session in its session block but carries no session
	// instance: the session is resolved through its registered sender.
	cfg := goruntime.RouteConfig{
		ID:       "fanout-route",
		Policy:   routing.RoutePolicy{DeliveryMode: routing.DeliverySharedOutbox},
		Bindings: []routing.DestinationBinding{{ID: "fan-binding", Address: "devices/fan", SessionID: "fan-session"}},
	}
	routeCfg := fanCfg
	if err := rt.AddRoute(cfg, NewFakeReceiver(), NewFakeSender(), nil, &routeCfg); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	startRuntime(t, rt)

	if got := fan.installs(); got != 1 {
		t.Fatalf("dead-letter path installed %d times on the session sender, want 1", got)
	}
}

func TestRemovedSubscriptionDeadLetterNotInstalledWithoutDLQStore(t *testing.T) {
	rt := goruntime.New(goruntime.WithInstanceID("removed-sub-no-dlq"))
	cfg, recv, sender := helperQuiescentRoute("source-route", nil)
	sess := newDeadLetterSession()
	sessCfg := runsession.Config{SessionID: "source-session"}
	if err := rt.AddRoute(cfg, recv, sender, sess, &sessCfg); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	startRuntime(t, rt)

	if got := sess.installs(); got != 0 {
		t.Fatalf("dead-letter path installed %d times without a DLQ store, want 0", got)
	}
}

func TestRemovedSubscriptionDeadLetterInstalledOnSessionWithoutRoute(t *testing.T) {
	store := NewFakeDLQStore()
	rt := goruntime.New(goruntime.WithInstanceID("removed-sub-routeless"), goruntime.WithDLQStore(store))
	idle := newDeadLetterSession()
	if err := rt.RegisterIngressSession(runsession.Config{SessionID: "idle-session"}, idle); err != nil {
		t.Fatalf("RegisterIngressSession: %v", err)
	}
	cfg, recv, sender := helperQuiescentRoute("other-route", nil)
	otherCfg := runsession.Config{SessionID: "other-session"}
	if err := rt.AddRoute(cfg, recv, sender, NewFakeSession(), &otherCfg); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	startRuntime(t, rt)

	entry := writeHeldDelivery(t, idle, store, "stale/#")
	if entry.RouteID() != "" || entry.SessionID() != "idle-session" {
		t.Fatalf("DLQ entry route/session = %q/%q, want \"\"/idle-session", entry.RouteID(), entry.SessionID())
	}
}

func TestRemovedSubscriptionDeadLetterNamesNoRouteWhoseReceiverRidesElsewhere(t *testing.T) {
	store := NewFakeDLQStore()
	rt := goruntime.New(goruntime.WithInstanceID("removed-sub-sender-only"), goruntime.WithDLQStore(store))
	cfg, recv, sender := helperQuiescentRoute("sender-route", nil)
	// The route's primary session block is source-session, but its receiver
	// subscribes through another session: source-session carries no ingress
	// route, so a delivery held for its removed filter names none.
	cfg.SourceSessionID = "other-session"
	sess := newDeadLetterSession()
	sessCfg := runsession.Config{SessionID: "source-session"}
	if err := rt.AddRoute(cfg, recv, sender, sess, &sessCfg); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	startRuntime(t, rt)

	entry := writeHeldDelivery(t, sess, store, "stale/#")
	if entry.RouteID() != "" || entry.SessionID() != "source-session" {
		t.Fatalf("DLQ entry route/session = %q/%q, want empty/source-session", entry.RouteID(), entry.SessionID())
	}
}
