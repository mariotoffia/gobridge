package runtime_test

import (
	"context"
	"sync"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

// ContextTrackingLeaseStore delegates to FakeLeaseStore but records whether
// Release was called with a non-expired context.
type ContextTrackingLeaseStore struct {
	inner           *FakeLeaseStore
	trackMu         sync.Mutex
	ReleaseCalled   bool
	ReleaseCtxValid bool
}

func NewContextTrackingLeaseStore() *ContextTrackingLeaseStore {
	return &ContextTrackingLeaseStore{inner: NewFakeLeaseStore()}
}

func (s *ContextTrackingLeaseStore) Acquire(ctx context.Context, leaseID, ownerID string, ttl time.Duration, endpoints map[string]string) (domain.LeaseToken, error) {
	return s.inner.Acquire(ctx, leaseID, ownerID, ttl, endpoints)
}

func (s *ContextTrackingLeaseStore) Renew(ctx context.Context, leaseID string, token domain.LeaseToken, ttl time.Duration, endpoints map[string]string) (domain.LeaseToken, error) {
	return s.inner.Renew(ctx, leaseID, token, ttl, endpoints)
}

func (s *ContextTrackingLeaseStore) Release(ctx context.Context, leaseID string, token domain.LeaseToken) error {
	s.trackMu.Lock()
	s.ReleaseCalled = true
	s.ReleaseCtxValid = ctx.Err() == nil
	s.trackMu.Unlock()
	return s.inner.Release(ctx, leaseID, token)
}

func (s *ContextTrackingLeaseStore) Current(ctx context.Context, leaseID string) (domain.LeaseInfo, error) {
	return s.inner.Current(ctx, leaseID)
}

func (s *ContextTrackingLeaseStore) IsReleaseCtxValid() bool {
	s.trackMu.Lock()
	defer s.trackMu.Unlock()
	return s.ReleaseCtxValid
}

func (s *ContextTrackingLeaseStore) WasReleaseCalled() bool {
	s.trackMu.Lock()
	defer s.trackMu.Unlock()
	return s.ReleaseCalled
}

// CountingLeaseStore delegates to FakeLeaseStore but counts Current() calls.
type CountingLeaseStore struct {
	inner    *FakeLeaseStore
	countMu  sync.Mutex
	CurrentN int
}

func NewCountingLeaseStore() *CountingLeaseStore {
	return &CountingLeaseStore{inner: NewFakeLeaseStore()}
}

func (s *CountingLeaseStore) Acquire(ctx context.Context, leaseID, ownerID string, ttl time.Duration, endpoints map[string]string) (domain.LeaseToken, error) {
	return s.inner.Acquire(ctx, leaseID, ownerID, ttl, endpoints)
}

func (s *CountingLeaseStore) Renew(ctx context.Context, leaseID string, token domain.LeaseToken, ttl time.Duration, endpoints map[string]string) (domain.LeaseToken, error) {
	return s.inner.Renew(ctx, leaseID, token, ttl, endpoints)
}

func (s *CountingLeaseStore) Release(ctx context.Context, leaseID string, token domain.LeaseToken) error {
	return s.inner.Release(ctx, leaseID, token)
}

func (s *CountingLeaseStore) Current(ctx context.Context, leaseID string) (domain.LeaseInfo, error) {
	s.countMu.Lock()
	s.CurrentN++
	s.countMu.Unlock()
	return s.inner.Current(ctx, leaseID)
}

func (s *CountingLeaseStore) GetCurrentCalls() int {
	s.countMu.Lock()
	defer s.countMu.Unlock()
	return s.CurrentN
}

// SlowExitReceiver exits slowly after context cancellation, ensuring
// wg.Wait() does not complete before the stop-context select fires.
type SlowExitReceiver struct {
	mu        sync.Mutex
	emit      func(context.Context, ports.Delivery) error
	ready     chan struct{}
	ExitDelay time.Duration
}

func NewSlowExitReceiver(delay time.Duration) *SlowExitReceiver {
	return &SlowExitReceiver{ready: make(chan struct{}), ExitDelay: delay}
}

func (r *SlowExitReceiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	r.mu.Lock()
	r.emit = emit
	close(r.ready)
	r.mu.Unlock()
	<-ctx.Done()
	time.Sleep(r.ExitDelay) // OTHER: simulated processing duration
	return ctx.Err()
}

func (r *SlowExitReceiver) Emit(ctx context.Context, del ports.Delivery) error {
	<-r.ready
	r.mu.Lock()
	emit := r.emit
	r.mu.Unlock()
	return emit(ctx, del)
}

// ControllableSession implements ports.Session with controllable Health
// and Start/Close call counting for re-acquisition tests.
type ControllableSession struct {
	mu           sync.Mutex
	startCount   int
	closeCount   int
	plans        []domain.SessionPlan
	events       chan ports.SessionEvent
	connected    bool
	startErr     error
	reconcileErr error
}

func NewControllableSession() *ControllableSession {
	return &ControllableSession{
		events:    make(chan ports.SessionEvent, 16),
		connected: true,
	}
}

func (s *ControllableSession) Start(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.startErr != nil {
		return s.startErr
	}
	s.startCount++
	return nil
}

func (s *ControllableSession) Reconcile(_ context.Context, plan domain.SessionPlan) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.plans = append(s.plans, plan)
	return s.reconcileErr
}

func (s *ControllableSession) Health(_ context.Context) ports.SessionHealth {
	s.mu.Lock()
	defer s.mu.Unlock()
	sl := ports.ServiceLevelNone
	if s.connected {
		sl = ports.ServiceLevelFull
	}
	return ports.SessionHealth{Connected: s.connected, Ready: s.connected, ServiceLevel: sl}
}

func (s *ControllableSession) Events() <-chan ports.SessionEvent {
	return s.events
}

func (s *ControllableSession) Close(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeCount++
	return nil
}

func (s *ControllableSession) SetConnected(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connected = v
}

func (s *ControllableSession) GetStartCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startCount
}
