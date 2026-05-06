package runtime_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

func waitFor(t *testing.T, timeout time.Duration, desc string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond) // SYNC: poll interval in waitFor helper
	}
	t.Fatalf("timeout waiting for %s", desc)
}

// ---------------------------------------------------------------------------
// FakeDelivery
// ---------------------------------------------------------------------------

type FakeDelivery struct {
	env        *messaging.Envelope
	mu         sync.Mutex
	Acked      bool
	Retried    bool
	RetryAfter time.Duration
	RetryErr   error
	Extended   bool
	ExtendTo   time.Time
	AckErr     error
	RetryFnErr error
}

func NewFakeDelivery(env *messaging.Envelope) *FakeDelivery {
	return &FakeDelivery{env: env}
}

func (d *FakeDelivery) Envelope() *messaging.Envelope { return d.env }

func (d *FakeDelivery) Ack(_ context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Acked = true
	return d.AckErr
}

func (d *FakeDelivery) Retry(_ context.Context, after time.Duration, reason error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Retried = true
	d.RetryAfter = after
	d.RetryErr = reason
	return d.RetryFnErr
}

func (d *FakeDelivery) Extend(_ context.Context, until time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Extended = true
	d.ExtendTo = until
	return nil
}

func (d *FakeDelivery) IsAcked() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.Acked
}

func (d *FakeDelivery) IsRetried() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.Retried
}

// ---------------------------------------------------------------------------
// FakeReceiver
// ---------------------------------------------------------------------------

type FakeReceiver struct {
	mu     sync.Mutex
	emit   func(context.Context, ports.Delivery) error
	ready  chan struct{}
	RunErr error
}

func NewFakeReceiver() *FakeReceiver {
	return &FakeReceiver{ready: make(chan struct{})}
}

func (r *FakeReceiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	r.mu.Lock()
	r.emit = emit
	close(r.ready)
	r.mu.Unlock()

	if r.RunErr != nil {
		return r.RunErr
	}

	<-ctx.Done()
	return ctx.Err()
}

func (r *FakeReceiver) Emit(ctx context.Context, del ports.Delivery) error {
	<-r.ready
	r.mu.Lock()
	emit := r.emit
	r.mu.Unlock()
	return emit(ctx, del)
}

func (r *FakeReceiver) Ready() <-chan struct{} {
	return r.ready
}

// ---------------------------------------------------------------------------
// FakeSender
// ---------------------------------------------------------------------------

type FakeSender struct {
	mu      sync.Mutex
	Sent    []*messaging.Envelope
	SendErr error
	SendFn  func(*messaging.Envelope) error
}

func NewFakeSender() *FakeSender {
	return &FakeSender{}
}

func (s *FakeSender) Send(_ context.Context, env *messaging.Envelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.SendFn != nil {
		if err := s.SendFn(env); err != nil {
			return err
		}
		s.Sent = append(s.Sent, env.Clone())
		return nil
	}
	if s.SendErr != nil {
		return s.SendErr
	}
	s.Sent = append(s.Sent, env.Clone())
	return nil
}

func (s *FakeSender) SentCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Sent)
}

func (s *FakeSender) GetSent() []*messaging.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*messaging.Envelope, len(s.Sent))
	copy(out, s.Sent)
	return out
}

func (s *FakeSender) SetSendErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.SendErr = err
}

// ---------------------------------------------------------------------------
// FakeSession
// ---------------------------------------------------------------------------

type FakeSession struct {
	mu           sync.Mutex
	Started      bool
	Closed       bool
	Plans        []connectivity.SessionPlan
	events       chan ports.SessionEvent
	closeOnce    sync.Once
	StartErr     error
	CloseErr     error
	ReconcileErr error
}

func NewFakeSession() *FakeSession {
	return &FakeSession{events: make(chan ports.SessionEvent, 16)}
}

func (s *FakeSession) Start(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Started = true
	return s.StartErr
}

func (s *FakeSession) Reconcile(_ context.Context, plan connectivity.SessionPlan) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Plans = append(s.Plans, plan)
	return s.ReconcileErr
}

func (s *FakeSession) Health(_ context.Context) ports.SessionHealth {
	return ports.SessionHealth{Connected: true, Ready: true, ServiceLevel: ports.ServiceLevelFull}
}

func (s *FakeSession) Events() <-chan ports.SessionEvent {
	return s.events
}

func (s *FakeSession) Close(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Closed = true
	s.closeOnce.Do(func() { close(s.events) })
	return s.CloseErr
}

func (s *FakeSession) PushEvent(ev ports.SessionEvent) {
	s.events <- ev
}

func (s *FakeSession) SetReconcileErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ReconcileErr = err
}

func (s *FakeSession) PlanCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Plans)
}

// ---------------------------------------------------------------------------
// FakeLeaseStore
// ---------------------------------------------------------------------------

type fakeLeaseEntry struct {
	owner     string
	version   uint64
	expires   time.Time
	endpoints map[string]string
}

type FakeLeaseStore struct {
	mu          sync.Mutex
	leases      map[string]*fakeLeaseEntry
	maxVersions map[string]uint64
	acquireErr  error
	renewErr    error
	releaseErr  error
}

func NewFakeLeaseStore() *FakeLeaseStore {
	return &FakeLeaseStore{
		leases:      make(map[string]*fakeLeaseEntry),
		maxVersions: make(map[string]uint64),
	}
}

func (s *FakeLeaseStore) SetAcquireErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acquireErr = err
}

func (s *FakeLeaseStore) SetRenewErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.renewErr = err
}

func (s *FakeLeaseStore) SetReleaseErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseErr = err
}

func (s *FakeLeaseStore) Acquire(_ context.Context, leaseID, ownerID string, ttl time.Duration, endpoints map[string]string) (persistence.LeaseToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.acquireErr != nil {
		return persistence.LeaseToken{}, s.acquireErr
	}

	entry, exists := s.leases[leaseID]
	if exists && time.Now().Before(entry.expires) && entry.owner != ownerID {
		return persistence.LeaseToken{}, shared.ErrAlreadyExists
	}

	version := s.maxVersions[leaseID] + 1
	s.maxVersions[leaseID] = version

	s.leases[leaseID] = &fakeLeaseEntry{
		owner:     ownerID,
		version:   version,
		expires:   time.Now().Add(ttl),
		endpoints: endpoints,
	}

	return persistence.LeaseToken{Version: version, Owner: ownerID}, nil
}

func (s *FakeLeaseStore) Renew(_ context.Context, leaseID string, token persistence.LeaseToken, ttl time.Duration, endpoints map[string]string) (persistence.LeaseToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.renewErr != nil {
		return persistence.LeaseToken{}, s.renewErr
	}

	entry, exists := s.leases[leaseID]
	if !exists || entry.version != token.Version || entry.owner != token.Owner {
		return persistence.LeaseToken{}, shared.ErrVersionMismatch
	}

	entry.expires = time.Now().Add(ttl)
	if endpoints != nil {
		entry.endpoints = endpoints
	}
	return token, nil
}

func (s *FakeLeaseStore) Release(_ context.Context, leaseID string, token persistence.LeaseToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.releaseErr != nil {
		return s.releaseErr
	}

	entry, exists := s.leases[leaseID]
	if !exists || entry.version != token.Version {
		return shared.ErrVersionMismatch
	}

	delete(s.leases, leaseID)
	return nil
}

func (s *FakeLeaseStore) Current(_ context.Context, leaseID string) (persistence.LeaseInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, exists := s.leases[leaseID]
	if !exists {
		return persistence.LeaseInfo{}, shared.ErrNotFound
	}

	return persistence.LeaseInfo{
		LeaseID:   leaseID,
		Owner:     entry.owner,
		Version:   entry.version,
		ExpiresAt: entry.expires,
		Endpoints: entry.endpoints,
	}, nil
}

// ---------------------------------------------------------------------------
// FakeOutboxStore
// ---------------------------------------------------------------------------

type FakeOutboxStore struct {
	mu            sync.Mutex
	records       map[string]*persistence.OutboxRecord
	PersistErr    error
	PersistFn     func([]persistence.OutboxRecord) error
	ClaimErr      error
	ClaimFn       func(partitionKey, ownerID string, token persistence.LeaseToken, limit int) ([]persistence.OutboxRecord, error)
	CompleteErr   error
	CompleteFn    func([]string, persistence.LeaseToken) error
	CompleteCtxFn func(context.Context, []string, persistence.LeaseToken) error
	StaleClaimAge time.Duration
}

func NewFakeOutboxStore() *FakeOutboxStore {
	return &FakeOutboxStore{records: make(map[string]*persistence.OutboxRecord)}
}

func (s *FakeOutboxStore) Persist(_ context.Context, records []persistence.OutboxRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.PersistFn != nil {
		if err := s.PersistFn(records); err != nil {
			return err
		}
	} else if s.PersistErr != nil {
		return s.PersistErr
	}

	for i := range records {
		rec := records[i]
		dedupKey := rec.EnvelopeID + ":" + rec.BindingID
		for _, existing := range s.records {
			existingKey := existing.EnvelopeID + ":" + existing.BindingID
			if existingKey == dedupKey {
				return shared.ErrDuplicateRecord
			}
		}
		rec.Status = persistence.OutboxPending
		clone := rec
		s.records[rec.ID] = &clone
	}
	return nil
}

func (s *FakeOutboxStore) Claim(_ context.Context, partitionKey, ownerID string, token persistence.LeaseToken, limit int) ([]persistence.OutboxRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.ClaimFn != nil {
		return s.ClaimFn(partitionKey, ownerID, token, limit)
	}
	if s.ClaimErr != nil {
		return nil, s.ClaimErr
	}

	staleAge := s.StaleClaimAge
	if staleAge == 0 {
		staleAge = 200 * time.Millisecond
	}
	staleThreshold := time.Now().Add(-staleAge)

	var claimed []persistence.OutboxRecord
	for _, rec := range s.records {
		if len(claimed) >= limit {
			break
		}
		recPK := persistence.OutboxPartitionKey(rec.SessionID, rec.BindingID)
		if recPK != partitionKey {
			continue
		}
		claimable := rec.Status == persistence.OutboxPending ||
			(rec.Status == persistence.OutboxClaimed && rec.ClaimedAt.Before(staleThreshold))
		if !claimable {
			continue
		}
		rec.Status = persistence.OutboxClaimed
		rec.ClaimedBy = ownerID
		rec.ClaimedAt = time.Now()
		rec.ClaimVersion = token.Version
		rec.ReplayCount++
		claimed = append(claimed, *rec)
	}
	return claimed, nil
}

func (s *FakeOutboxStore) Complete(ctx context.Context, recordIDs []string, token persistence.LeaseToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.CompleteCtxFn != nil {
		if err := s.CompleteCtxFn(ctx, recordIDs, token); err != nil {
			return err
		}
	} else if s.CompleteFn != nil {
		if err := s.CompleteFn(recordIDs, token); err != nil {
			return err
		}
	} else if s.CompleteErr != nil {
		return s.CompleteErr
	}

	for _, id := range recordIDs {
		rec, exists := s.records[id]
		if !exists {
			continue
		}
		if rec.ClaimVersion != token.Version {
			return shared.ErrStaleFencingToken
		}
		rec.Status = persistence.OutboxCompleted
		rec.CompletedAt = time.Now()
	}
	return nil
}

func (s *FakeOutboxStore) Expire(_ context.Context, before time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	count := 0
	for _, rec := range s.records {
		if rec.Status == persistence.OutboxPending && !rec.ExpiresAt.IsZero() && rec.ExpiresAt.Before(before) {
			rec.Status = persistence.OutboxExpired
			count++
		}
	}
	return count, nil
}

func (s *FakeOutboxStore) SetClaimErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ClaimErr = err
}

func (s *FakeOutboxStore) QueryPending(_ context.Context, partitionKey string, limit int) ([]persistence.OutboxRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var result []persistence.OutboxRecord
	for _, rec := range s.records {
		if len(result) >= limit {
			break
		}
		recPK := persistence.OutboxPartitionKey(rec.SessionID, rec.BindingID)
		if recPK != partitionKey {
			continue
		}
		if rec.Status == persistence.OutboxPending {
			result = append(result, *rec)
		}
	}
	return result, nil
}

func (s *FakeOutboxStore) RecordCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

func (s *FakeOutboxStore) CompletedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, rec := range s.records {
		if rec.Status == persistence.OutboxCompleted {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// FakeDLQStore
// ---------------------------------------------------------------------------

type FakeDLQStore struct {
	mu       sync.Mutex
	Entries  []routing.DLQEntry
	WriteErr error
}

func NewFakeDLQStore() *FakeDLQStore {
	return &FakeDLQStore{}
}

func (s *FakeDLQStore) Write(_ context.Context, entry routing.DLQEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.WriteErr != nil {
		return s.WriteErr
	}
	s.Entries = append(s.Entries, entry)
	return nil
}

func (s *FakeDLQStore) List(_ context.Context, filter routing.DLQFilter) ([]routing.DLQEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var result []routing.DLQEntry
	for _, e := range s.Entries {
		if filter.RouteID != "" && e.RouteID != filter.RouteID {
			continue
		}
		if filter.Category != "" && e.Category != filter.Category {
			continue
		}
		result = append(result, e)
		if filter.Limit > 0 && len(result) >= filter.Limit {
			break
		}
	}
	return result, nil
}

func (s *FakeDLQStore) Get(_ context.Context, id string) (routing.DLQEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.Entries {
		if e.ID == id {
			return e, nil
		}
	}
	return routing.DLQEntry{}, shared.ErrNotFound
}

func (s *FakeDLQStore) Delete(_ context.Context, ids []string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idSet := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		idSet[id] = struct{}{}
	}
	var remaining []routing.DLQEntry
	var count int
	for _, e := range s.Entries {
		if _, ok := idSet[e.ID]; ok {
			count++
		} else {
			remaining = append(remaining, e)
		}
	}
	s.Entries = remaining
	return count, nil
}

func (s *FakeDLQStore) DeleteByFilter(_ context.Context, _ routing.DLQFilter) (int, error) {
	return 0, nil
}

func (s *FakeDLQStore) Purge(_ context.Context, _ time.Time) (int, error) { return 0, nil }

func (s *FakeDLQStore) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Entries)
}

// ---------------------------------------------------------------------------
// FakeTracer / FakeSpan
// ---------------------------------------------------------------------------

type FakeSpan struct {
	mu       sync.Mutex
	Name     string
	Attrs    []shared.Tag
	Ended    bool
	Err      error
	Events   []string
	SetAttrs []shared.Tag
}

func (s *FakeSpan) End() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Ended = true
}

func (s *FakeSpan) SetError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Err = err
}

func (s *FakeSpan) AddEvent(name string, _ ...shared.Tag) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Events = append(s.Events, name)
}

func (s *FakeSpan) SetAttributes(attrs ...shared.Tag) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.SetAttrs = append(s.SetAttrs, attrs...)
}

func (s *FakeSpan) IsEnded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Ended
}

// Inspect returns a consistent snapshot of span fields for test assertions.
func (s *FakeSpan) Inspect() (name string, ended bool, attrs []shared.Tag, spanErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name = s.Name
	ended = s.Ended
	attrs = append([]shared.Tag(nil), s.Attrs...)
	spanErr = s.Err
	return
}

type FakeTracer struct {
	mu    sync.Mutex
	Spans []*FakeSpan
}

func (t *FakeTracer) StartSpan(ctx context.Context, name string, attrs ...shared.Tag) (context.Context, ports.Span) {
	t.mu.Lock()
	defer t.mu.Unlock()
	span := &FakeSpan{Name: name, Attrs: append([]shared.Tag{}, attrs...)}
	t.Spans = append(t.Spans, span)
	return ctx, span
}

func (t *FakeTracer) SpanCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.Spans)
}

func (t *FakeTracer) LastSpan() *FakeSpan {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.Spans) == 0 {
		return nil
	}
	return t.Spans[len(t.Spans)-1]
}

// ---------------------------------------------------------------------------
// FakeResolver
// ---------------------------------------------------------------------------

type FakeResolver struct {
	Plans      []routing.DispatchPlan
	ResolveErr error
}

func (r *FakeResolver) Resolve(_ context.Context, _ *messaging.Envelope) ([]routing.DispatchPlan, error) {
	if r.ResolveErr != nil {
		return nil, r.ResolveErr
	}
	return r.Plans, nil
}

// ---------------------------------------------------------------------------
// FakeProcessor
// ---------------------------------------------------------------------------

type FakeProcessor struct {
	NameVal    string
	ProcessFn  func(context.Context, *messaging.Envelope, ports.ProcessorFunc) error
	ProcessErr error
	called     int32
}

func (p *FakeProcessor) Name() string { return p.NameVal }

func (p *FakeProcessor) Process(ctx context.Context, env *messaging.Envelope, next ports.ProcessorFunc) error {
	atomic.AddInt32(&p.called, 1)
	if p.ProcessFn != nil {
		return p.ProcessFn(ctx, env, next)
	}
	if p.ProcessErr != nil {
		return p.ProcessErr
	}
	return next(ctx, env)
}

func (p *FakeProcessor) CalledCount() int32 {
	return atomic.LoadInt32(&p.called)
}
