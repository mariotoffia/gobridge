package paho

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// ═══════════════════════════════════════════════════════════════════════════
// Session lifecycle, races, resilience — pure-unit and unreachable-broker
// tests. Integration tests against a real broker live in the _test
// (paho_test) package files.
// ═══════════════════════════════════════════════════════════════════════════

// TestAnaSession_HealthAfterClose_ReportsDisconnected asserts the
// post-Close Health state: Connected=false, ServiceLevel=None.
func TestAnaSession_HealthAfterClose_ReportsDisconnected(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-close-health",
	}, domain.SessionEphemeral, nil)

	_ = s.Close(context.Background())

	h := s.Health(context.Background())
	if h.Connected {
		t.Error("Connected should be false after Close")
	}
	if h.Ready {
		t.Error("Ready should be false after Close")
	}
	if h.ServiceLevel != ports.ServiceLevelNone {
		t.Errorf("ServiceLevel = %v, want None", h.ServiceLevel)
	}
}

// TestAnaSession_HealthBeforeStart_ReportsDisconnected asserts the
// freshly-constructed session reports Connected=false.
func TestAnaSession_HealthBeforeStart_ReportsDisconnected(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-fresh-health",
	}, domain.SessionEphemeral, nil)

	h := s.Health(context.Background())
	if h.Connected {
		t.Error("Connected should be false before Start")
	}
	if h.ReceiveMaximum != 65535 {
		t.Errorf("ReceiveMaximum default = %d, want 65535", h.ReceiveMaximum)
	}
}

// TestAnaSession_HealthRespectsConfiguredReceiveMaximum asserts that a
// non-default ReceiveMaximum is reported through Health for operator
// observability.
func TestAnaSession_HealthRespectsConfiguredReceiveMaximum(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs:     []string{"tcp://192.0.2.1:1883"},
		ClientID:       "ana-rm",
		ReceiveMaximum: 1234,
	}, domain.SessionEphemeral, nil)

	h := s.Health(context.Background())
	if h.ReceiveMaximum != 1234 {
		t.Errorf("ReceiveMaximum = %d, want 1234", h.ReceiveMaximum)
	}
}

// TestAnaSession_StartCtxCancelled_ReturnsClassifiedError verifies that
// cancelling the context passed to Start makes Start return promptly
// with a typed error (not panic, not hang).
func TestAnaSession_StartCtxCancelled_ReturnsClassifiedError(t *testing.T) {
	if testing.Short() {
		t.Skip("uses connect timeout on unreachable broker")
	}

	s := NewSession(SessionOptions{
		BrokerURLs:     []string{"tcp://192.0.2.1:1883"},
		ClientID:       "ana-ctx-cancel",
		ConnectTimeout: 5 * time.Second,
	}, domain.SessionEphemeral, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond) // OTHER: race window — cancel during Start connect attempt
		cancel()
	}()

	start := time.Now()
	err := s.Start(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from Start with cancelled ctx")
	}
	if _, ok := err.(*shared.BridgeError); !ok {
		t.Fatalf("err type = %T, want *shared.BridgeError", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Start took %v after cancel; expected fast return", elapsed)
	}

	_ = s.Close(context.Background())
}

// TestAnaSession_DoubleClose_NoPanic_NoEventChannelDoublyClosed confirms
// that a second Close is a strict no-op (does not double-close the
// events channel which would panic).
func TestAnaSession_DoubleClose_NoPanic_NoEventChannelDoublyClosed(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-2x-close",
	}, domain.SessionEphemeral, nil)

	defer func() {
		if rv := recover(); rv != nil {
			t.Fatalf("double Close panicked: %v", rv)
		}
	}()

	if err := s.Close(context.Background()); err != nil {
		t.Logf("first close: %v", err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Logf("second close: %v", err)
	}

	// Events channel must be closed (read returns zero, ok=false).
	select {
	case _, ok := <-s.Events():
		if ok {
			t.Fatal("events channel should be closed and read should return ok=false")
		}
	default:
		t.Fatal("events channel must be closed (zero-value readable)")
	}
}

// TestAnaSession_ReconcileBeforeStart_ReturnsErrUnavailable validates
// the explicit error path documented in Reconcile: when no CM is
// available, Reconcile must return ErrUnavailable so callers can stash
// the plan and retry after Start.
func TestAnaSession_ReconcileBeforeStart_ReturnsErrUnavailable(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-recon-pre",
	}, domain.SessionEphemeral, nil)

	err := s.Reconcile(context.Background(), domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{{Topic: "t/x", QoS: 1}},
	})
	if err == nil {
		t.Fatal("expected error from Reconcile-before-Start")
	}
	be, ok := err.(*shared.BridgeError)
	if !ok {
		t.Fatalf("err type = %T, want *shared.BridgeError", err)
	}
	if be.Code != shared.ErrUnavailable.Code {
		t.Fatalf("err code = %s, want %s", be.Code, shared.ErrUnavailable.Code)
	}
}

// TestAnaSession_ReconcileBeforeStart_StashesPlanForOnConnectionUp pins
// the BUG-A-fix invariant: even though Reconcile returns an error, the
// plan IS stashed so that OnConnectionUp can apply it once a connection
// comes up.
func TestAnaSession_ReconcileBeforeStart_StashesPlanForOnConnectionUp(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-recon-stash",
	}, domain.SessionEphemeral, nil)

	plan := domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{{Topic: "t/stash", QoS: 1}},
	}
	_ = s.Reconcile(context.Background(), plan)

	s.mu.Lock()
	stored := s.plan
	s.mu.Unlock()
	if stored == nil {
		t.Fatal("plan should be stashed after Reconcile-before-Start")
	}
	if len(stored.Subscriptions) != 1 || stored.Subscriptions[0].Topic != "t/stash" {
		t.Fatalf("stashed plan = %+v, want one subscription on t/stash", stored)
	}
}

// TestAnaSession_ReconcileEmptyPlanWithPriorPlan_IsNoOp documents the
// intentional no-op behaviour: an empty plan handed to Reconcile when
// a prior plan exists does NOT clear active subscriptions. This is by
// design (per session.go comment) so that a SessionManager with no
// subscriptions cannot accidentally tear down externally-managed topics.
func TestAnaSession_ReconcileEmptyPlanWithPriorPlan_IsNoOp(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-recon-empty",
	}, domain.SessionEphemeral, nil)
	s.mu.Lock()
	s.cm = &pahoConn{cm: fakeCM}
	s.plan = &domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{{Topic: "kept", QoS: 1}},
	}
	s.activeSubs = map[string]byte{"kept": 1}
	s.mu.Unlock()

	if err := s.Reconcile(context.Background(), domain.SessionPlan{}); err != nil {
		t.Fatalf("empty Reconcile with prior plan must be a silent no-op, got %v", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.activeSubs) != 1 {
		t.Fatalf("activeSubs len = %d, want 1 (empty plan must NOT unsubscribe by design)", len(s.activeSubs))
	}
	if s.plan == nil || len(s.plan.Subscriptions) != 1 {
		t.Fatalf("prior plan must be preserved")
	}
}

// TestAnaSession_HealthReadAfterReconcileStashesPlan_NoCrash verifies
// the previously-fragile path: a plan stashed before Start must not
// cause Health to panic when reading plan.Subscriptions outside the
// lock.
func TestAnaSession_HealthReadAfterReconcileStashesPlan_NoCrash(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-health-stash",
	}, domain.SessionEphemeral, nil)

	_ = s.Reconcile(context.Background(), domain.SessionPlan{
		Subscriptions: []domain.SubscriptionPlan{
			{Topic: "x/1", QoS: 0},
			{Topic: "x/2", QoS: 1},
		},
	})

	defer func() {
		if rv := recover(); rv != nil {
			t.Fatalf("Health panicked on stashed plan: %v", rv)
		}
	}()
	h := s.Health(context.Background())
	if h.SubscriptionsWanted != 2 {
		t.Errorf("SubscriptionsWanted = %d, want 2", h.SubscriptionsWanted)
	}
	if h.Connected {
		t.Errorf("Connected should be false (Start was never called)")
	}
}

// TestAnaSession_PushEvent_ManyConcurrent_NoPanic stresses pushEvent
// from many goroutines while Close is invoked at random points. Must
// never panic on a closed channel.
func TestAnaSession_PushEvent_ManyConcurrent_NoPanic(t *testing.T) {
	for trial := 0; trial < 5; trial++ {
		s := NewSession(SessionOptions{
			BrokerURLs: []string{"tcp://192.0.2.1:1883"},
			ClientID:   "ana-pushevent-" + strconv.Itoa(trial),
		}, domain.SessionEphemeral, nil)

		var wg sync.WaitGroup
		const pushers = 16
		const perPusher = 200

		wg.Add(pushers)
		for p := 0; p < pushers; p++ {
			go func() {
				defer wg.Done()
				for i := 0; i < perPusher; i++ {
					s.pushEvent(ports.SessionConnected, nil)
				}
			}()
		}
		go func() {
			time.Sleep(time.Duration(trial+1) * 100 * time.Microsecond) // OTHER: race window — Close during concurrent pushEvent
			_ = s.Close(context.Background())
		}()

		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("trial %d: pushers did not complete (deadlock?)", trial)
		}
	}
}

// TestAnaSession_HealthDuringConcurrentPushEvent_NoRace verifies
// pushEvent and Health do not race (verify with -race).
func TestAnaSession_HealthDuringConcurrentPushEvent_NoRace(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-health-race",
	}, domain.SessionEphemeral, nil)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				s.pushEvent(ports.SessionConnected, nil)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = s.Health(context.Background())
			}
		}
	}()

	time.Sleep(80 * time.Millisecond) // OTHER: race window — let concurrent goroutines exercise before stopping
	close(stop)
	wg.Wait()
	_ = s.Close(context.Background())
}

// TestAnaSession_ConcurrentReconcileAndHealth_NoRace verifies Reconcile
// stashing s.plan and Health reading s.plan do not race.
func TestAnaSession_ConcurrentReconcileAndHealth_NoRace(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-recon-health",
	}, domain.SessionEphemeral, nil)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(2)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
				_ = s.Reconcile(context.Background(), domain.SessionPlan{
					Subscriptions: []domain.SubscriptionPlan{
						{Topic: "t/" + strconv.Itoa(i%4), QoS: 1},
					},
				})
				i++
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = s.Health(context.Background())
			}
		}
	}()

	time.Sleep(80 * time.Millisecond) // OTHER: race window — let concurrent goroutines exercise before stopping
	close(stop)
	wg.Wait()
	_ = s.Close(context.Background())
}

// TestAnaSession_EventsChannelDeliversInOrder validates FIFO ordering
// of the events buffer (under-subscription / non-full case).
func TestAnaSession_EventsChannelDeliversInOrder(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-events-fifo",
	}, domain.SessionEphemeral, nil)

	// pushEvent uses a buffered channel; pushing a few events should
	// preserve order.
	types := []ports.SessionEventType{
		ports.SessionConnected,
		ports.SessionReconnecting,
		ports.SessionDisconnected,
	}
	for _, t := range types {
		s.pushEvent(t, nil)
	}

	for i, want := range types {
		select {
		case ev := <-s.Events():
			if ev.Type != want {
				t.Fatalf("event %d type = %v, want %v", i, ev.Type, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for event %d", i)
		}
	}
}

// TestAnaSession_PushEvent_TimestampSet validates timestamps are
// populated by pushEvent (used for operator dashboards).
func TestAnaSession_PushEvent_TimestampSet(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-events-ts",
	}, domain.SessionEphemeral, nil)

	before := time.Now()
	s.pushEvent(ports.SessionConnected, nil)
	after := time.Now()

	ev := <-s.Events()
	if ev.Timestamp.Before(before) || ev.Timestamp.After(after) {
		t.Fatalf("timestamp %v outside [%v, %v]", ev.Timestamp, before, after)
	}
}

// TestAnaSession_ConnectionManagerAccessor_LockSafe verifies the
// ConnectionManager() accessor is safe under concurrent Close.
func TestAnaSession_ConnectionManagerAccessor_LockSafe(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-cm-accessor",
	}, domain.SessionEphemeral, nil)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var n atomic.Int64
		for {
			select {
			case <-stop:
				_ = n.Load()
				return
			default:
				_ = s.ConnectionManager()
				n.Add(1)
			}
		}
	}()

	time.Sleep(20 * time.Millisecond) // OTHER: race window — let ConnectionManager accessor goroutine exercise before Close
	_ = s.Close(context.Background())
	close(stop)
	wg.Wait()
}

// TestAnaSession_RouterAccessor_NotNil validates the Router() accessor
// returns a non-nil router immediately after construction.
func TestAnaSession_RouterAccessor_NotNil(t *testing.T) {
	s := NewSession(SessionOptions{
		BrokerURLs: []string{"tcp://192.0.2.1:1883"},
		ClientID:   "ana-router-accessor",
	}, domain.SessionEphemeral, nil)
	if s.Router() == nil {
		t.Fatal("Router() should be non-nil after construction")
	}
}
