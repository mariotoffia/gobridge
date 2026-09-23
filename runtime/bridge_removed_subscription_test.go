package runtime_test

import (
	"context"
	"sync"
	"testing"

	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	runsession "github.com/mariotoffia/gobridge/runtime/session"
)

// deadLetterSession is a FakeSession that also accepts the removed-subscription
// dead-letter path, recording what the runtime installed.
type deadLetterSession struct {
	*FakeSession
	mu        sync.Mutex
	installed int
	fn        func(context.Context, *messaging.Envelope, string) error
}

func newDeadLetterSession() *deadLetterSession {
	return &deadLetterSession{FakeSession: NewFakeSession()}
}

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
