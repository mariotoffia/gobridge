package bridge

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
)

// ---------------------------------------------------------------------------
// hangingSession — Close() blocks until context expires
// ---------------------------------------------------------------------------

type hangingSession struct{ fakeSession }

func (s *hangingSession) Close(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// ---------------------------------------------------------------------------
// slowSession — Close() sleeps then succeeds
// ---------------------------------------------------------------------------

type slowSession struct {
	fakeSession
	delay time.Duration
}

func (s *slowSession) Close(ctx context.Context) error {
	select {
	case <-time.After(s.delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ---------------------------------------------------------------------------
// failingSession — Close() returns an error
// ---------------------------------------------------------------------------

type failingSession struct {
	fakeSession
	closeErr error
}

func (s *failingSession) Close(_ context.Context) error {
	return s.closeErr
}

// ---------------------------------------------------------------------------
// countingTransportFactory — counts NewSession/NewReceiver/NewSender calls
// ---------------------------------------------------------------------------

type countingTransportFactory struct {
	fakeTransportFactory
	mu            sync.Mutex
	SessionCalls  int
	ReceiverCalls int
	SenderCalls   int
	SessionFn     func(context.Context, ports.SessionSpec) (ports.Session, error)
}

func (f *countingTransportFactory) NewSession(ctx context.Context, spec ports.SessionSpec) (ports.Session, error) {
	f.mu.Lock()
	f.SessionCalls++
	fn := f.SessionFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, spec)
	}
	return &fakeSession{}, nil
}

func (f *countingTransportFactory) NewReceiver(ctx context.Context, _ ports.ReceiverSpec, sess ports.Session) (ports.Receiver, error) {
	f.mu.Lock()
	f.ReceiverCalls++
	f.mu.Unlock()
	return &fakeReceiver{}, nil
}

func (f *countingTransportFactory) NewSender(ctx context.Context, _ ports.SenderSpec, sess ports.Session) (ports.Sender, error) {
	f.mu.Lock()
	f.SenderCalls++
	f.mu.Unlock()
	return &fakeSender{}, nil
}

func (f *countingTransportFactory) Counts() (sessions, receivers, senders int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.SessionCalls, f.ReceiverCalls, f.SenderCalls
}

// ---------------------------------------------------------------------------
// exclusiveTransportFactory — declares CapExclusiveIdentity
// ---------------------------------------------------------------------------

type exclusiveTransportFactory struct {
	countingTransportFactory
}

func (f *exclusiveTransportFactory) Capabilities() []ports.Capability {
	return []ports.Capability{ports.CapExclusiveIdentity, ports.CapStatefulSession}
}

// ---------------------------------------------------------------------------
// failingTransportFactory — NewSession returns error
// ---------------------------------------------------------------------------

type failingTransportFactory struct {
	fakeTransportFactory
	sessionErr error
}

func (f *failingTransportFactory) NewSession(_ context.Context, _ ports.SessionSpec) (ports.Session, error) {
	return nil, f.sessionErr
}

// ---------------------------------------------------------------------------
// failingStoreFactory — store creation returns error
// ---------------------------------------------------------------------------

type failingStoreFactory struct {
	leaseErr  error
	outboxErr error
	dlqErr    error
}

func (f *failingStoreFactory) NewLeaseStore(_ context.Context, _ ports.StoreSpec) (ports.LeaseStore, error) {
	if f.leaseErr != nil {
		return nil, f.leaseErr
	}
	return &fakeLeaseStore{}, nil
}

func (f *failingStoreFactory) NewOutboxStore(_ context.Context, _ ports.StoreSpec) (ports.OutboxStore, error) {
	if f.outboxErr != nil {
		return nil, f.outboxErr
	}
	return &fakeOutboxStore{}, nil
}

func (f *failingStoreFactory) NewDLQStore(_ context.Context, _ ports.StoreSpec) (ports.DLQStore, error) {
	if f.dlqErr != nil {
		return nil, f.dlqErr
	}
	return nil, nil
}

// ---------------------------------------------------------------------------
// trackingSession — records Close() calls with atomic counter
// ---------------------------------------------------------------------------

type trackingSession struct {
	fakeSession
	closed atomic.Int32
}

func (s *trackingSession) Close(_ context.Context) error {
	s.closed.Add(1)
	return nil
}

func (s *trackingSession) CloseCount() int {
	return int(s.closed.Load())
}

// ---------------------------------------------------------------------------
// trackingTransportFactory — creates trackingSessions, records order
// ---------------------------------------------------------------------------

type trackingTransportFactory struct {
	fakeTransportFactory
	mu       sync.Mutex
	sessions []*trackingSession
	failAt   int // -1 = never fail; 0-based index of session that fails
}

func (f *trackingTransportFactory) NewSession(_ context.Context, _ ports.SessionSpec) (ports.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	idx := len(f.sessions)
	if f.failAt >= 0 && idx == f.failAt {
		return nil, fmt.Errorf("session creation failed at index %d", idx)
	}
	s := &trackingSession{}
	f.sessions = append(f.sessions, s)
	return s, nil
}

func (f *trackingTransportFactory) Sessions() []*trackingSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]*trackingSession, len(f.sessions))
	copy(cp, f.sessions)
	return cp
}

// ---------------------------------------------------------------------------
// Config helpers
// ---------------------------------------------------------------------------

func supervisorTestConfig(routeID string) *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:           "test-bridge",
			DrainTimeout: "1s",
		},
		Receivers: []ports.ReceiverDef{
			{ID: routeID + "-rx", Transport: "fake"},
		},
		Senders: []ports.SenderDef{
			{ID: routeID + "-tx", Transport: "fake"},
		},
		Bindings: []ports.BindingDef{
			{ID: routeID + "-b1", SenderID: routeID + "-tx", Address: "addr/" + routeID},
		},
		Routes: []ports.RouteDef{
			{
				ID:           routeID,
				ReceiverID:   routeID + "-rx",
				DeliveryMode: "direct_hold",
				Bindings:     []string{routeID + "-b1"},
			},
		},
	}
}

func supervisorTestConfigWithSession(routeID, sessionID string) *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{
			ID:           "test-bridge",
			DrainTimeout: "1s",
		},
		Stores: ports.StoresConfig{
			Lease:  &ports.StoreConfig{Type: "memory"},
			Outbox: &ports.StoreConfig{Type: "memory"},
		},
		Sessions: []ports.SessionDef{
			{ID: sessionID, Transport: "exclusive", SessionMode: "exclusive"},
		},
		Receivers: []ports.ReceiverDef{
			{ID: routeID + "-rx", Transport: "fake"},
		},
		Senders: []ports.SenderDef{
			{ID: routeID + "-tx", Transport: "exclusive", SessionID: sessionID},
		},
		Bindings: []ports.BindingDef{
			{ID: routeID + "-b1", SenderID: routeID + "-tx", SessionID: sessionID, Address: "topic/" + routeID},
		},
		Routes: []ports.RouteDef{
			{
				ID:           routeID,
				ReceiverID:   routeID + "-rx",
				DeliveryMode: "shared_outbox",
				Bindings:     []string{routeID + "-b1"},
				Session: &ports.RouteSessionDef{
					SessionID: sessionID,
					SenderID:  routeID + "-tx",
				},
			},
		},
	}
}

// newTestSupervisor creates a Supervisor with fake transports and
// stores. config.Validate is injected as the blueprint validator so
// invalid-config tests behave the same as the production composition
// root would.
func newTestSupervisor(opts ...SupervisorOption) *Supervisor {
	opts = append([]SupervisorOption{WithSupervisorBlueprintValidator(config.Validate)}, opts...)
	s := NewSupervisor(opts...)
	s.RegisterTransport("fake", &fakeTransportFactory{})
	s.RegisterStoreFactory("memory", &fakeStoreFactory{})
	return s
}

// newTestSupervisorWithExclusive creates a Supervisor with both a
// fake transport and an exclusive transport.
func newTestSupervisorWithExclusive(opts ...SupervisorOption) (*Supervisor, *exclusiveTransportFactory) {
	opts = append([]SupervisorOption{WithSupervisorBlueprintValidator(config.Validate)}, opts...)
	s := NewSupervisor(opts...)
	s.RegisterTransport("fake", &fakeTransportFactory{})
	ef := &exclusiveTransportFactory{}
	s.RegisterTransport("exclusive", ef)
	s.RegisterStoreFactory("memory", &fakeStoreFactory{})
	return s, ef
}

// runSupervisorAsync starts the supervisor in a goroutine and returns
// a channel that receives the Run result.
func runSupervisorAsync(ctx context.Context, s *Supervisor, cfg *ports.BridgeConfig, ch <-chan *ports.BridgeConfig) <-chan error {
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Run(ctx, cfg, ch)
	}()
	return errCh
}

// waitForRuntime polls s.Runtime() until it returns non-nil or
// timeout is reached.
func waitForRuntime(s *Supervisor, timeout time.Duration) *goruntime.Runtime {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if rt := s.Runtime(); rt != nil {
			return rt
		}
		time.Sleep(5 * time.Millisecond) // STARTUP: supervisor runtime init poll
	}
	return nil
}

// waitForRouteID polls until the first route has the given ID or
// timeout is reached.
func waitForRouteID(s *Supervisor, routeID string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		rt := s.Runtime()
		if rt != nil {
			routes := rt.Routes()
			if len(routes) > 0 && routes[0].ID == routeID {
				return true
			}
		}
		time.Sleep(5 * time.Millisecond) // STARTUP: supervisor route convergence poll
	}
	return false
}

// quickCfg returns a minimal valid BridgeConfig with a unique route ID.
func quickCfg(id string) *ports.BridgeConfig {
	return supervisorTestConfig(id)
}

// invalidCfg returns a BridgeConfig that fails validation.
func invalidCfg() *ports.BridgeConfig {
	return &ports.BridgeConfig{}
}

// sendConfig sends a config on the channel without blocking indefinitely.
func sendConfig(ch chan<- *ports.BridgeConfig, cfg *ports.BridgeConfig, timeout time.Duration) bool {
	select {
	case ch <- cfg:
		return true
	case <-time.After(timeout):
		return false
	}
}

// unused variable guard
var _ = []any{
	(*hangingSession)(nil),
	(*slowSession)(nil),
	(*failingSession)(nil),
	(*countingTransportFactory)(nil),
	(*exclusiveTransportFactory)(nil),
	(*failingTransportFactory)(nil),
	(*failingStoreFactory)(nil),
	(*trackingSession)(nil),
	(*trackingTransportFactory)(nil),
}

// fakeReceiver/fakeSender/fakeSession/fakeTransportFactory/fakeStoreFactory
// are defined in builder_test.go and shared across test files in this package.
// The types below reference them via embedding.

// ensure interface compliance for custom fakes
var (
	_ ports.TransportFactory = (*countingTransportFactory)(nil)
	_ ports.TransportFactory = (*exclusiveTransportFactory)(nil)
	_ ports.TransportFactory = (*failingTransportFactory)(nil)
	_ ports.StoreFactory     = (*failingStoreFactory)(nil)
	_ ports.Session          = (*hangingSession)(nil)
	_ ports.Session          = (*slowSession)(nil)
	_ ports.Session          = (*failingSession)(nil)
	_ ports.Session          = (*trackingSession)(nil)
)

// quickSupervisorRun is a helper that starts a supervisor, waits for the
// runtime to appear, then returns the cancel function and error channel.
func quickSupervisorRun(s *Supervisor, cfg *ports.BridgeConfig, changes chan *ports.BridgeConfig) (context.CancelFunc, <-chan error) {
	ctx, cancel := context.WithCancel(context.Background())
	errCh := runSupervisorAsync(ctx, s, cfg, changes)
	waitForRuntime(s, 2*time.Second)
	return cancel, errCh
}
