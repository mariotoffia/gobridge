package runtime_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/ports"
)

func waitFor(t *testing.T, timeout time.Duration, desc string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", desc)
}

// ---------------------------------------------------------------------------
// FakeDelivery
// ---------------------------------------------------------------------------

type FakeDelivery struct {
	env        *domain.Envelope
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

func NewFakeDelivery(env *domain.Envelope) *FakeDelivery {
	return &FakeDelivery{env: env}
}

func (d *FakeDelivery) Envelope() *domain.Envelope { return d.env }

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
	Sent    []*domain.Envelope
	SendErr error
	SendFn  func(*domain.Envelope) error
}

func NewFakeSender() *FakeSender {
	return &FakeSender{}
}

func (s *FakeSender) Send(_ context.Context, env *domain.Envelope) error {
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

func (s *FakeSender) GetSent() []*domain.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*domain.Envelope, len(s.Sent))
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
	Plans        []domain.SessionPlan
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

func (s *FakeSession) Reconcile(_ context.Context, plan domain.SessionPlan) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Plans = append(s.Plans, plan)
	return s.ReconcileErr
}

func (s *FakeSession) Health(_ context.Context) ports.SessionHealth {
	return ports.SessionHealth{Connected: true}
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

func (s *FakeLeaseStore) Acquire(_ context.Context, leaseID, ownerID string, ttl time.Duration, endpoints map[string]string) (domain.LeaseToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.acquireErr != nil {
		return domain.LeaseToken{}, s.acquireErr
	}

	entry, exists := s.leases[leaseID]
	if exists && time.Now().Before(entry.expires) && entry.owner != ownerID {
		return domain.LeaseToken{}, domain.ErrAlreadyExists
	}

	version := s.maxVersions[leaseID] + 1
	s.maxVersions[leaseID] = version

	s.leases[leaseID] = &fakeLeaseEntry{
		owner:     ownerID,
		version:   version,
		expires:   time.Now().Add(ttl),
		endpoints: endpoints,
	}

	return domain.LeaseToken{Version: version, Owner: ownerID}, nil
}

func (s *FakeLeaseStore) Renew(_ context.Context, leaseID string, token domain.LeaseToken, ttl time.Duration, endpoints map[string]string) (domain.LeaseToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.renewErr != nil {
		return domain.LeaseToken{}, s.renewErr
	}

	entry, exists := s.leases[leaseID]
	if !exists || entry.version != token.Version || entry.owner != token.Owner {
		return domain.LeaseToken{}, domain.ErrVersionMismatch
	}

	entry.expires = time.Now().Add(ttl)
	if endpoints != nil {
		entry.endpoints = endpoints
	}
	return token, nil
}

func (s *FakeLeaseStore) Release(_ context.Context, leaseID string, token domain.LeaseToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.releaseErr != nil {
		return s.releaseErr
	}

	entry, exists := s.leases[leaseID]
	if !exists || entry.version != token.Version {
		return domain.ErrVersionMismatch
	}

	delete(s.leases, leaseID)
	return nil
}

func (s *FakeLeaseStore) Current(_ context.Context, leaseID string) (domain.LeaseInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, exists := s.leases[leaseID]
	if !exists {
		return domain.LeaseInfo{}, domain.ErrNotFound
	}

	return domain.LeaseInfo{
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
	records       map[string]*domain.OutboxRecord
	PersistErr    error
	PersistFn     func([]domain.OutboxRecord) error
	ClaimErr      error
	ClaimFn       func(partitionKey, ownerID string, token domain.LeaseToken, limit int) ([]domain.OutboxRecord, error)
	CompleteErr   error
	CompleteFn    func([]string, domain.LeaseToken) error
	StaleClaimAge time.Duration
}

func NewFakeOutboxStore() *FakeOutboxStore {
	return &FakeOutboxStore{records: make(map[string]*domain.OutboxRecord)}
}

func (s *FakeOutboxStore) Persist(_ context.Context, records []domain.OutboxRecord) error {
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
				return domain.ErrDuplicateRecord
			}
		}
		rec.Status = domain.OutboxPending
		clone := rec
		s.records[rec.ID] = &clone
	}
	return nil
}

func (s *FakeOutboxStore) Claim(_ context.Context, partitionKey, ownerID string, token domain.LeaseToken, limit int) ([]domain.OutboxRecord, error) {
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

	var claimed []domain.OutboxRecord
	for _, rec := range s.records {
		if len(claimed) >= limit {
			break
		}
		recPK := domain.OutboxPartitionKey(rec.SessionID, rec.BindingID)
		if recPK != partitionKey {
			continue
		}
		claimable := rec.Status == domain.OutboxPending ||
			(rec.Status == domain.OutboxClaimed && rec.ClaimedAt.Before(staleThreshold))
		if !claimable {
			continue
		}
		rec.Status = domain.OutboxClaimed
		rec.ClaimedBy = ownerID
		rec.ClaimedAt = time.Now()
		rec.ClaimVersion = token.Version
		rec.ReplayCount++
		claimed = append(claimed, *rec)
	}
	return claimed, nil
}

func (s *FakeOutboxStore) Complete(_ context.Context, recordIDs []string, token domain.LeaseToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.CompleteFn != nil {
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
			return domain.ErrStaleFencingToken
		}
		rec.Status = domain.OutboxCompleted
		rec.CompletedAt = time.Now()
	}
	return nil
}

func (s *FakeOutboxStore) Expire(_ context.Context, before time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	count := 0
	for _, rec := range s.records {
		if rec.Status == domain.OutboxPending && !rec.ExpiresAt.IsZero() && rec.ExpiresAt.Before(before) {
			rec.Status = domain.OutboxExpired
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

func (s *FakeOutboxStore) QueryPending(_ context.Context, partitionKey string, limit int) ([]domain.OutboxRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var result []domain.OutboxRecord
	for _, rec := range s.records {
		if len(result) >= limit {
			break
		}
		recPK := domain.OutboxPartitionKey(rec.SessionID, rec.BindingID)
		if recPK != partitionKey {
			continue
		}
		if rec.Status == domain.OutboxPending {
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
		if rec.Status == domain.OutboxCompleted {
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
	Entries  []domain.DLQEntry
	WriteErr error
}

func NewFakeDLQStore() *FakeDLQStore {
	return &FakeDLQStore{}
}

func (s *FakeDLQStore) Write(_ context.Context, entry domain.DLQEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.WriteErr != nil {
		return s.WriteErr
	}
	s.Entries = append(s.Entries, entry)
	return nil
}

func (s *FakeDLQStore) List(_ context.Context, filter domain.DLQFilter) ([]domain.DLQEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var result []domain.DLQEntry
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

func (s *FakeDLQStore) Replay(_ context.Context, _ []string) error { return nil }

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
	Attrs    []domain.Tag
	Ended    bool
	Err      error
	Events   []string
	SetAttrs []domain.Tag
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

func (s *FakeSpan) AddEvent(name string, _ ...domain.Tag) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Events = append(s.Events, name)
}

func (s *FakeSpan) SetAttributes(attrs ...domain.Tag) {
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
func (s *FakeSpan) Inspect() (name string, ended bool, attrs []domain.Tag, spanErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name = s.Name
	ended = s.Ended
	attrs = append([]domain.Tag(nil), s.Attrs...)
	spanErr = s.Err
	return
}

type FakeTracer struct {
	mu    sync.Mutex
	Spans []*FakeSpan
}

func (t *FakeTracer) StartSpan(ctx context.Context, name string, attrs ...domain.Tag) (context.Context, ports.Span) {
	t.mu.Lock()
	defer t.mu.Unlock()
	span := &FakeSpan{Name: name, Attrs: append([]domain.Tag{}, attrs...)}
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
	Plans      []domain.DispatchPlan
	ResolveErr error
}

func (r *FakeResolver) Resolve(_ context.Context, _ *domain.Envelope) ([]domain.DispatchPlan, error) {
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
	ProcessFn  func(context.Context, *domain.Envelope, ports.ProcessorFunc) error
	ProcessErr error
	called     int32
}

func (p *FakeProcessor) Name() string { return p.NameVal }

func (p *FakeProcessor) Process(ctx context.Context, env *domain.Envelope, next ports.ProcessorFunc) error {
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
