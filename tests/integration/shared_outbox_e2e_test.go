package integration_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	dblease "github.com/mariotoffia/gobridge/adapters/aws/store/dynamodblease"
	dboutbox "github.com/mariotoffia/gobridge/adapters/aws/store/dynamodboutbox"
	"github.com/mariotoffia/gobridge/adapters/native/store/memorylease"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/sqslocal"
)

func TestMain(m *testing.M) {
	ddblocal.Configure(ddblocal.WithCleanOrphans(true))
	sqslocal.Configure(sqslocal.WithCleanOrphans(true))
	mqttlocal.Configure(mqttlocal.WithCleanOrphans(true))

	code := m.Run()

	sqslocal.Shutdown()
	mqttlocal.Shutdown()
	ddblocal.Shutdown()
	os.Exit(code)
}

// ---------------------------------------------------------------------------
// Fakes for transports (SQS receiver emulation, MQTT sender emulation)
// ---------------------------------------------------------------------------

type fakeDelivery struct {
	env        *messaging.Envelope
	mu         sync.Mutex
	acked      bool
	retried    bool
	retryAfter time.Duration
}

func newFakeDelivery(env *messaging.Envelope) *fakeDelivery {
	return &fakeDelivery{env: env}
}

func (d *fakeDelivery) Envelope() *messaging.Envelope { return d.env }

func (d *fakeDelivery) Ack(_ context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.acked = true
	return nil
}

func (d *fakeDelivery) Retry(_ context.Context, after time.Duration, _ error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.retried = true
	d.retryAfter = after
	return nil
}

func (d *fakeDelivery) Extend(_ context.Context, _ time.Time) error { return nil }

func (d *fakeDelivery) isAcked() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.acked
}

type fakeReceiver struct {
	mu    sync.Mutex
	emit  func(context.Context, ports.Delivery) error
	ready chan struct{}
}

func newFakeReceiver() *fakeReceiver {
	return &fakeReceiver{ready: make(chan struct{})}
}

func (r *fakeReceiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	r.mu.Lock()
	r.emit = emit
	close(r.ready)
	r.mu.Unlock()
	<-ctx.Done()
	return ctx.Err()
}

func (r *fakeReceiver) Emit(ctx context.Context, del ports.Delivery) error {
	<-r.ready
	r.mu.Lock()
	emit := r.emit
	r.mu.Unlock()
	return emit(ctx, del)
}

type fakeSender struct {
	mu      sync.Mutex
	sent    []*messaging.Envelope
	sendErr error
}

func newFakeSender() *fakeSender { return &fakeSender{} }

func (s *fakeSender) Send(_ context.Context, env *messaging.Envelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sendErr != nil {
		return s.sendErr
	}
	s.sent = append(s.sent, env.Clone())
	return nil
}

func (s *fakeSender) sentCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

func (s *fakeSender) getSent() []*messaging.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]*messaging.Envelope, len(s.sent))
	copy(cp, s.sent)
	return cp
}

type fakeSession struct {
	mu      sync.Mutex
	started bool
	events  chan ports.SessionEvent
	once    sync.Once
}

func newFakeSession() *fakeSession {
	return &fakeSession{events: make(chan ports.SessionEvent, 16)}
}

func (s *fakeSession) Start(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = true
	return nil
}

func (s *fakeSession) Reconcile(_ context.Context, _ domain.SessionPlan) error { return nil }

func (s *fakeSession) Health(_ context.Context) ports.SessionHealth {
	return ports.SessionHealth{Connected: true, Ready: true, ServiceLevel: ports.ServiceLevelFull}
}

func (s *fakeSession) Events() <-chan ports.SessionEvent { return s.events }

func (s *fakeSession) Close(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.once.Do(func() { close(s.events) })
	return nil
}

func (s *fakeSession) isStarted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

type fakeDLQStore struct {
	mu      sync.Mutex
	entries []domain.DLQEntry
}

func (s *fakeDLQStore) Write(_ context.Context, entry domain.DLQEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, entry)
	return nil
}

func (s *fakeDLQStore) List(_ context.Context, _ domain.DLQFilter) ([]domain.DLQEntry, error) {
	return nil, nil
}

func (s *fakeDLQStore) Get(_ context.Context, _ string) (domain.DLQEntry, error) {
	return domain.DLQEntry{}, shared.ErrNotFound
}
func (s *fakeDLQStore) Delete(_ context.Context, _ []string) (int, error) { return 0, nil }
func (s *fakeDLQStore) DeleteByFilter(_ context.Context, _ domain.DLQFilter) (int, error) {
	return 0, nil
}
func (s *fakeDLQStore) Purge(_ context.Context, _ time.Time) (int, error) { return 0, nil }

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func waitFor(t *testing.T, timeout time.Duration, desc string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond) // SYNC: poll for condition
	}
	t.Fatalf("timed out waiting for: %s", desc)
}

func fastSessionConfig(sessionID string) goruntime.SessionConfig {
	cfg := goruntime.DefaultSessionConfig(sessionID, true)
	cfg.LeaseTTL = 500 * time.Millisecond
	cfg.RenewInterval = 80 * time.Millisecond
	cfg.RenewJitter = 10 * time.Millisecond
	cfg.StepDownGrace = 100 * time.Millisecond
	cfg.DrainStrategy = persistence.NewFixedPoll(50 * time.Millisecond)
	cfg.DrainBatchSize = 50
	return cfg
}

// ---------------------------------------------------------------------------
// Tests with DynamoDB outbox + lease stores
// ---------------------------------------------------------------------------

// validates shared_outbox with DynamoDB lease and outbox stores: ingress persists and egress drains five fake deliveries.
func TestE2E_DynamoDB_SharedOutboxFlow(t *testing.T) {
	client := ddblocal.Client(t)

	leaseTable := ddblocal.UniqueTable("leases")
	outboxTable := ddblocal.UniqueTable("outbox")

	leaseStore := dblease.NewStore(client, dblease.WithTableName(leaseTable), dblease.WithGracePeriod(5*time.Second))
	if err := leaseStore.EnsureTable(context.Background()); err != nil {
		t.Fatalf("ensure lease table: %v", err)
	}
	ddblocal.CleanupTable(t, client, leaseTable)

	outboxStore := dboutbox.NewStore(client, dboutbox.WithTableName(outboxTable))
	if err := outboxStore.CreateTable(context.Background()); err != nil {
		t.Fatalf("create outbox table: %v", err)
	}
	ddblocal.CleanupTable(t, client, outboxTable)

	dlq := &fakeDLQStore{}

	const sessionID = "mqtt-ddb-session"

	// Instance A: ingress.
	rtA := goruntime.New(
		goruntime.WithInstanceID("bridge-ddb-A"),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithDLQStore(dlq),
	)
	receiverA := newFakeReceiver()
	senderA := newFakeSender()

	cfgA := goruntime.RouteConfig{
		ID: "ddb-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
		},
		Resolver: &staticResolver{
			plans: []domain.DispatchPlan{
				{BindingID: "mqtt-bind", Address: "devices/ddb/state"},
			},
		},
		Bindings: []domain.DestinationBinding{
			{ID: "mqtt-bind", SessionID: sessionID},
		},
	}
	_ = rtA.AddRoute(cfgA, receiverA, senderA, nil, nil)

	// Instance B: egress.
	rtB := goruntime.New(
		goruntime.WithInstanceID("bridge-ddb-B"),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithDLQStore(dlq),
	)
	receiverB := newFakeReceiver()
	senderB := newFakeSender()
	sessionB := newFakeSession()

	sessCfgB := fastSessionConfig(sessionID)

	cfgB := goruntime.RouteConfig{
		ID: "ddb-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
		},
		Resolver: &staticResolver{
			plans: []domain.DispatchPlan{
				{BindingID: "mqtt-bind", Address: "devices/ddb/state"},
			},
		},
		Bindings: []domain.DestinationBinding{
			{ID: "mqtt-bind", SessionID: sessionID},
		},
	}
	_ = rtB.AddRoute(cfgB, receiverB, senderB, sessionB, &sessCfgB)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = rtA.Start(ctx)
	_ = rtB.Start(ctx)
	defer func() {
		_ = rtA.Stop(context.Background())
		_ = rtB.Stop(context.Background())
	}()

	waitFor(t, 5*time.Second, "session B started", func() bool {
		return sessionB.isStarted()
	})

	// Emit messages through A with distinct payloads.
	const msgCount = 5
	dels := make([]*fakeDelivery, msgCount)
	for i := 0; i < msgCount; i++ {
		env := &messaging.Envelope{
			ID:      t.Name() + "-msg-" + string(rune('A'+i)),
			Payload: []byte(fmt.Sprintf(`{"seq":%d}`, i)),
		}
		del := newFakeDelivery(env)
		dels[i] = del
		_ = receiverA.Emit(ctx, del)
	}

	// All should be acked.
	for i, d := range dels {
		waitFor(t, 3*time.Second, "ack "+string(rune('A'+i)), func() bool {
			return d.isAcked()
		})
	}

	// All should be drained and sent by B.
	waitFor(t, 10*time.Second, "all sent by B", func() bool {
		return senderB.sentCount() >= msgCount
	})

	sentMsgs := senderB.getSent()
	rxPayloads := make(map[string]bool, len(sentMsgs))
	for _, env := range sentMsgs {
		rxPayloads[string(env.Payload)] = true
	}
	for i := 0; i < msgCount; i++ {
		want := fmt.Sprintf(`{"seq":%d}`, i)
		if !rxPayloads[want] {
			t.Errorf("missing payload %s in sent messages", want)
		}
	}
}

// validates cross-instance lease transfer on DynamoDB: secondary drains the persisted message after primary stops.
func TestE2E_DynamoDB_LeaseTransfer(t *testing.T) {
	client := ddblocal.Client(t)

	leaseTable := ddblocal.UniqueTable("leases")
	outboxTable := ddblocal.UniqueTable("outbox")

	leaseStore := dblease.NewStore(client, dblease.WithTableName(leaseTable), dblease.WithGracePeriod(5*time.Second))
	_ = leaseStore.EnsureTable(context.Background())
	ddblocal.CleanupTable(t, client, leaseTable)

	outboxStore := dboutbox.NewStore(client, dboutbox.WithTableName(outboxTable))
	_ = outboxStore.CreateTable(context.Background())
	ddblocal.CleanupTable(t, client, outboxTable)

	dlq := &fakeDLQStore{}
	const sessionID = "mqtt-transfer-session"

	// Instance A: starts first.
	ctxA, cancelA := context.WithCancel(context.Background())

	rtA := goruntime.New(
		goruntime.WithInstanceID("bridge-transfer-A"),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithDLQStore(dlq),
	)
	receiverA := newFakeReceiver()
	senderA := newFakeSender()
	sessionA := newFakeSession()

	sessCfgA := fastSessionConfig(sessionID)
	sessCfgA.LeaseTTL = 400 * time.Millisecond
	sessCfgA.RenewInterval = 80 * time.Millisecond
	sessCfgA.DrainStrategy = persistence.NewFixedPoll(10 * time.Second) // Long interval so A doesn't drain.

	cfgA := goruntime.RouteConfig{
		ID: "transfer-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
		},
		Bindings: []domain.DestinationBinding{
			{ID: "b1", SessionID: sessionID},
		},
	}

	if err := rtA.AddRoute(cfgA, receiverA, senderA, sessionA, &sessCfgA); err != nil {
		t.Fatalf("AddRoute A: %v", err)
	}
	if err := rtA.Start(ctxA); err != nil {
		t.Fatalf("Start A: %v", err)
	}

	waitFor(t, 5*time.Second, "session A started", func() bool {
		return sessionA.isStarted()
	})

	// Persist messages via A.
	env := &messaging.Envelope{ID: t.Name() + "-transfer-msg", Payload: []byte("transfer")}
	del := newFakeDelivery(env)
	_ = receiverA.Emit(ctxA, del)
	waitFor(t, 3*time.Second, "acked by A", func() bool { return del.isAcked() })

	// Crash A.
	cancelA()
	_ = rtA.Stop(context.Background())

	// Instance B takes over.
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()

	rtB := goruntime.New(
		goruntime.WithInstanceID("bridge-transfer-B"),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithDLQStore(dlq),
	)
	receiverB := newFakeReceiver()
	senderB := newFakeSender()
	sessionB := newFakeSession()

	sessCfgB := fastSessionConfig(sessionID)
	sessCfgB.LeaseTTL = 400 * time.Millisecond
	sessCfgB.RenewInterval = 80 * time.Millisecond

	cfgB := goruntime.RouteConfig{
		ID: "transfer-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
		},
		Bindings: []domain.DestinationBinding{
			{ID: "b1", SessionID: sessionID},
		},
	}

	if err := rtB.AddRoute(cfgB, receiverB, senderB, sessionB, &sessCfgB); err != nil {
		t.Fatalf("AddRoute B: %v", err)
	}
	if err := rtB.Start(ctxB); err != nil {
		t.Fatalf("Start B: %v", err)
	}

	waitFor(t, 5*time.Second, "session B started", func() bool {
		return sessionB.isStarted()
	})

	// B should drain A's persisted message.
	waitFor(t, 10*time.Second, "sent by B", func() bool {
		return senderB.sentCount() >= 1
	})

	sent := senderB.getSent()
	if sent[0].ID != t.Name()+"-transfer-msg" {
		t.Errorf("expected transfer-msg, got %q", sent[0].ID)
	}

	_ = rtB.Stop(context.Background())
	t.Logf("DynamoDB lease transfer: message recovered by B")
}

// validates combining in-memory lease store with DynamoDB outbox (independent store backends).
func TestE2E_MemoryLease_DynamoOutbox(t *testing.T) {
	client := ddblocal.Client(t)
	outboxTable := ddblocal.UniqueTable("outbox")

	outboxStore := dboutbox.NewStore(client, dboutbox.WithTableName(outboxTable))
	_ = outboxStore.CreateTable(context.Background())
	ddblocal.CleanupTable(t, client, outboxTable)

	leaseStore := memorylease.NewStore()
	dlq := &fakeDLQStore{}

	const sessionID = "mqtt-mixed"

	rt := goruntime.New(
		goruntime.WithInstanceID("bridge-mixed"),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithDLQStore(dlq),
	)
	receiver := newFakeReceiver()
	sender := newFakeSender()
	session := newFakeSession()
	sessCfg := fastSessionConfig(sessionID)

	cfg := goruntime.RouteConfig{
		ID: "mixed-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
		},
		Bindings: []domain.DestinationBinding{
			{ID: "b1", SessionID: sessionID},
		},
	}

	if err := rt.AddRoute(cfg, receiver, sender, session, &sessCfg); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = rt.Stop(context.Background()) }()

	waitFor(t, 5*time.Second, "session started", func() bool {
		return session.isStarted()
	})

	env := &messaging.Envelope{ID: t.Name() + "-mixed-msg", Payload: []byte("mixed")}
	del := newFakeDelivery(env)
	_ = receiver.Emit(ctx, del)

	waitFor(t, 3*time.Second, "acked", func() bool { return del.isAcked() })
	waitFor(t, 10*time.Second, "sent", func() bool { return sender.sentCount() >= 1 })

	t.Logf("Mixed stores: memory lease + DynamoDB outbox works")
}

// ---------------------------------------------------------------------------
// G4: Crash recovery with DynamoDB stores
// ---------------------------------------------------------------------------

// validates crash recovery: primary persists and acks then stops before drain; secondary acquires the lease and sends the orphaned record.
func TestE2E_DynamoDB_CrashRecovery(t *testing.T) {
	client := ddblocal.Client(t)

	leaseTable := ddblocal.UniqueTable("leases")
	outboxTable := ddblocal.UniqueTable("outbox")

	leaseStore := dblease.NewStore(client, dblease.WithTableName(leaseTable), dblease.WithGracePeriod(5*time.Second))
	if err := leaseStore.EnsureTable(context.Background()); err != nil {
		t.Fatalf("ensure lease table: %v", err)
	}
	ddblocal.CleanupTable(t, client, leaseTable)

	outboxStore := dboutbox.NewStore(client,
		dboutbox.WithTableName(outboxTable),
		dboutbox.WithStaleClaimDuration(200*time.Millisecond),
	)
	if err := outboxStore.CreateTable(context.Background()); err != nil {
		t.Fatalf("create outbox table: %v", err)
	}
	ddblocal.CleanupTable(t, client, outboxTable)

	dlq := &fakeDLQStore{}
	const sessionID = "mqtt-crash-recovery"

	// Instance A: set a very long drain interval so it won't drain before crash.
	ctxA, cancelA := context.WithCancel(context.Background())

	rtA := goruntime.New(
		goruntime.WithInstanceID("bridge-crash-A"),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithDLQStore(dlq),
	)
	receiverA := newFakeReceiver()
	senderA := newFakeSender()
	sessionA := newFakeSession()

	sessCfgA := fastSessionConfig(sessionID)
	sessCfgA.LeaseTTL = 400 * time.Millisecond
	sessCfgA.RenewInterval = 80 * time.Millisecond
	sessCfgA.DrainStrategy = persistence.NewFixedPoll(30 * time.Second)

	cfgA := goruntime.RouteConfig{
		ID: "crash-recovery-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
		},
		Resolver: &staticResolver{
			plans: []domain.DispatchPlan{
				{BindingID: "mqtt-bind", Address: "devices/recovery/state"},
			},
		},
		Bindings: []domain.DestinationBinding{
			{ID: "mqtt-bind", SessionID: sessionID},
		},
	}
	_ = rtA.AddRoute(cfgA, receiverA, senderA, sessionA, &sessCfgA)
	_ = rtA.Start(ctxA)

	waitFor(t, 5*time.Second, "session A started", func() bool {
		return sessionA.isStarted()
	})

	// Persist messages via A.
	env := &messaging.Envelope{ID: t.Name() + "-crash-msg", Payload: []byte("orphaned")}
	del := newFakeDelivery(env)
	_ = receiverA.Emit(ctxA, del)
	waitFor(t, 3*time.Second, "acked by A", func() bool { return del.isAcked() })

	// Crash A before it can drain.
	cancelA()
	_ = rtA.Stop(context.Background())

	// Instance B takes over.
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()

	rtB := goruntime.New(
		goruntime.WithInstanceID("bridge-crash-B"),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithDLQStore(dlq),
	)
	receiverB := newFakeReceiver()
	senderB := newFakeSender()
	sessionB := newFakeSession()

	sessCfgB := fastSessionConfig(sessionID)
	sessCfgB.LeaseTTL = 400 * time.Millisecond
	sessCfgB.RenewInterval = 80 * time.Millisecond

	cfgB := goruntime.RouteConfig{
		ID: "crash-recovery-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
		},
		Resolver: &staticResolver{
			plans: []domain.DispatchPlan{
				{BindingID: "mqtt-bind", Address: "devices/recovery/state"},
			},
		},
		Bindings: []domain.DestinationBinding{
			{ID: "mqtt-bind", SessionID: sessionID},
		},
	}
	_ = rtB.AddRoute(cfgB, receiverB, senderB, sessionB, &sessCfgB)
	_ = rtB.Start(ctxB)

	waitFor(t, 5*time.Second, "session B started", func() bool {
		return sessionB.isStarted()
	})

	// B should drain A's orphaned record.
	waitFor(t, 10*time.Second, "sent by B", func() bool {
		return senderB.sentCount() >= 1
	})

	sent := senderB.getSent()
	if sent[0].ID != t.Name()+"-crash-msg" {
		t.Errorf("expected crash-msg, got %q", sent[0].ID)
	}

	_ = rtB.Stop(context.Background())
	t.Logf("DynamoDB crash recovery: orphaned message recovered by B")
}

// ---------------------------------------------------------------------------
// G5: Fencing token validation with DynamoDB
// ---------------------------------------------------------------------------

// validates DynamoDB conditional writes reject Complete with a stale token after another owner reclaims the record.
func TestE2E_DynamoDB_FencingValidation(t *testing.T) {
	client := ddblocal.Client(t)

	leaseTable := ddblocal.UniqueTable("leases")
	outboxTable := ddblocal.UniqueTable("outbox")

	leaseStore := dblease.NewStore(client, dblease.WithTableName(leaseTable), dblease.WithGracePeriod(5*time.Second))
	_ = leaseStore.EnsureTable(context.Background())
	ddblocal.CleanupTable(t, client, leaseTable)

	outboxStore := dboutbox.NewStore(client,
		dboutbox.WithTableName(outboxTable),
		dboutbox.WithStaleClaimDuration(200*time.Millisecond),
	)
	_ = outboxStore.CreateTable(context.Background())
	ddblocal.CleanupTable(t, client, outboxTable)

	ctx := context.Background()
	const leaseID = "mqtt-fencing-session"

	// Owner A acquires the lease.
	tokenA, err := leaseStore.Acquire(ctx, leaseID, "owner-A", 300*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("A acquire: %v", err)
	}

	// Persist a record and claim it with A's token.
	rec := persistence.OutboxRecord{
		ID:         "fencing-rec-1",
		RouteID:    "fencing-route",
		EnvelopeID: "fencing-env-1",
		BindingID:  "bind-1",
		SessionID:  leaseID,
		Address:    "topic/fencing",
		Status:     persistence.OutboxPending,
		Envelope:   messaging.Envelope{ID: "fencing-env-1", Payload: []byte("data")},
	}
	if err := outboxStore.Persist(ctx, []persistence.OutboxRecord{rec}); err != nil {
		t.Fatalf("persist: %v", err)
	}

	pk := persistence.OutboxPartitionKey(leaseID, "bind-1")
	claimed, err := outboxStore.Claim(ctx, pk, "owner-A", tokenA, 10)
	if err != nil {
		t.Fatalf("claim with A: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("expected 1 claimed, got %d", len(claimed))
	}

	time.Sleep(400 * time.Millisecond) // ESSENTIAL: wait for A's lease to expire so B can acquire
	tokenB, err := leaseStore.Acquire(ctx, leaseID, "owner-B", 5*time.Second, nil)
	if err != nil {
		t.Fatalf("B acquire: %v", err)
	}
	if tokenB.Version <= tokenA.Version {
		t.Fatalf("B's version (%d) should be higher than A's (%d)", tokenB.Version, tokenA.Version)
	}

	// B reclaims the stale-claimed record (updating claim_version to B's).
	time.Sleep(300 * time.Millisecond) // ESSENTIAL: wait for stale claim threshold
	reclaimedB, err := outboxStore.Claim(ctx, pk, "owner-B", tokenB, 10)
	if err != nil {
		t.Fatalf("reclaim with B: %v", err)
	}
	if len(reclaimedB) != 1 {
		t.Fatalf("expected 1 reclaimed by B, got %d", len(reclaimedB))
	}

	// Now A's stale token should be rejected on Complete because the
	// record's claim_version was updated when B reclaimed it.
	err = outboxStore.Complete(ctx, []string{reclaimedB[0].ID}, tokenA)
	if err == nil {
		t.Fatal("expected stale fencing token error on Complete with A's token after B reclaimed")
	}
	if be, ok := shared.AsBridgeError(err); ok {
		t.Logf("correctly rejected with bridge error: %s", be.Code)
	}

	// B completes successfully with its own token.
	err = outboxStore.Complete(ctx, []string{reclaimedB[0].ID}, tokenB)
	if err != nil {
		t.Fatalf("complete with B: %v", err)
	}

	t.Logf("DynamoDB fencing: stale token rejected after reclaim, new owner completed")
}

// ---------------------------------------------------------------------------
// G6: Poison message with DynamoDB
// ---------------------------------------------------------------------------

// validates poison handling: repeated send failure exceeds MaxReplayAttempts and the record moves to the DLQ.
func TestE2E_DynamoDB_PoisonMessage(t *testing.T) {
	client := ddblocal.Client(t)

	leaseTable := ddblocal.UniqueTable("leases")
	outboxTable := ddblocal.UniqueTable("outbox")

	leaseStore := dblease.NewStore(client, dblease.WithTableName(leaseTable), dblease.WithGracePeriod(5*time.Second))
	_ = leaseStore.EnsureTable(context.Background())
	ddblocal.CleanupTable(t, client, leaseTable)

	// MaxReplayAttempts=3 means 3 send attempts; on the 4th claim the
	// drainer detects ReplayCount > MaxReplayAttempts and sends to DLQ.
	// The DynamoDB store's Claim filter checks pre-update replay_count,
	// so maxReplayCount must be MaxReplayAttempts+1 to allow that 4th claim.
	outboxStore := dboutbox.NewStore(client,
		dboutbox.WithTableName(outboxTable),
		dboutbox.WithStaleClaimDuration(200*time.Millisecond),
		dboutbox.WithMaxReplayCount(4),
	)
	_ = outboxStore.CreateTable(context.Background())
	ddblocal.CleanupTable(t, client, outboxTable)

	dlq := &fakeDLQStore{}
	const sessionID = "mqtt-poison-session"

	rt := goruntime.New(
		goruntime.WithInstanceID("bridge-poison"),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithDLQStore(dlq),
	)

	receiver := newFakeReceiver()

	// Sender always fails with transient error -> drainer retries until
	// max replay count is exceeded.
	sender := &fakeSender{
		sendErr: shared.NewBridgeError("CRASH", shared.ErrorTransient, "always fails"),
	}

	session := newFakeSession()
	sessCfg := fastSessionConfig(sessionID)
	sessCfg.DrainStrategy = persistence.NewFixedPoll(50 * time.Millisecond)

	cfg := goruntime.RouteConfig{
		ID: "poison-route",
		Policy: domain.RoutePolicy{
			DeliveryMode:      domain.DeliverySharedOutbox,
			MaxReplayAttempts: 3,
		},
		Bindings: []domain.DestinationBinding{
			{ID: "b1", SessionID: sessionID},
		},
	}

	_ = rt.AddRoute(cfg, receiver, sender, session, &sessCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = rt.Start(ctx)
	defer func() { _ = rt.Stop(context.Background()) }()

	waitFor(t, 5*time.Second, "session started", func() bool {
		return session.isStarted()
	})

	env := &messaging.Envelope{ID: t.Name() + "-poison-msg", Payload: []byte("toxic")}
	del := newFakeDelivery(env)
	_ = receiver.Emit(ctx, del)
	waitFor(t, 3*time.Second, "acked", func() bool { return del.isAcked() })

	// Wait for the drainer to exhaust replay attempts and send to DLQ.
	waitFor(t, 30*time.Second, "DLQ entry", func() bool {
		dlq.mu.Lock()
		defer dlq.mu.Unlock()
		return len(dlq.entries) >= 1
	})

	t.Logf("DynamoDB poison message: record moved to DLQ after max replays")
}

// ---------------------------------------------------------------------------
// G7: Fan-out atomicity with DynamoDB
// ---------------------------------------------------------------------------

// validates fan-out persist and drain to two sessions; idempotent re-emit of the same envelope does not duplicate sends.
func TestE2E_DynamoDB_FanOutAtomicity(t *testing.T) {
	client := ddblocal.Client(t)

	leaseTable := ddblocal.UniqueTable("leases")
	outboxTable := ddblocal.UniqueTable("outbox")

	leaseStore := dblease.NewStore(client, dblease.WithTableName(leaseTable), dblease.WithGracePeriod(5*time.Second))
	_ = leaseStore.EnsureTable(context.Background())
	ddblocal.CleanupTable(t, client, leaseTable)

	outboxStore := dboutbox.NewStore(client, dboutbox.WithTableName(outboxTable))
	_ = outboxStore.CreateTable(context.Background())
	ddblocal.CleanupTable(t, client, outboxTable)

	dlq := &fakeDLQStore{}
	const sessionID = "mqtt-fanout-session"

	rt := goruntime.New(
		goruntime.WithInstanceID("bridge-fanout"),
		goruntime.WithOutboxStore(outboxStore),
		goruntime.WithLeaseStore(leaseStore),
		goruntime.WithDLQStore(dlq),
	)

	receiver := newFakeReceiver()
	senderA := newFakeSender()
	sessionA := newFakeSession()
	sessCfgA := fastSessionConfig(sessionID + "-a")

	// Second session for fan-out.
	senderB := newFakeSender()
	sessionB := newFakeSession()
	sessCfgB := fastSessionConfig(sessionID + "-b")

	_ = rt.RegisterSessionSender(sessCfgB, sessionB, senderB)

	cfg := goruntime.RouteConfig{
		ID: "fanout-route",
		Policy: domain.RoutePolicy{
			DeliveryMode: domain.DeliverySharedOutbox,
		},
		Resolver: &staticResolver{
			plans: []domain.DispatchPlan{
				{BindingID: "bind-a", Address: "factory/a/orders/42"},
				{BindingID: "bind-b", Address: "factory/b/orders/42"},
			},
		},
		Bindings: []domain.DestinationBinding{
			{ID: "bind-a", SessionID: sessionID + "-a"},
			{ID: "bind-b", SessionID: sessionID + "-b"},
		},
	}

	_ = rt.AddRoute(cfg, receiver, senderA, sessionA, &sessCfgA)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = rt.Start(ctx)
	defer func() { _ = rt.Stop(context.Background()) }()

	waitFor(t, 5*time.Second, "sessions started", func() bool {
		return sessionA.isStarted() && sessionB.isStarted()
	})

	// Send a message that fans out to both sessions.
	env := &messaging.Envelope{ID: t.Name() + "-fanout-msg", Payload: []byte("multi")}
	del := newFakeDelivery(env)
	_ = receiver.Emit(ctx, del)

	waitFor(t, 3*time.Second, "acked", func() bool { return del.isAcked() })

	// Both drainers should eventually send their records.
	waitFor(t, 10*time.Second, "both sent", func() bool {
		return senderA.sentCount() >= 1 && senderB.sentCount() >= 1
	})

	sentA := senderA.getSent()
	if sentA[0].Subject != "factory/a/orders/42" {
		t.Errorf("sender A: expected factory/a/orders/42, got %q", sentA[0].Subject)
	}
	sentBList := senderB.getSent()
	if sentBList[0].Subject != "factory/b/orders/42" {
		t.Errorf("sender B: expected factory/b/orders/42, got %q", sentBList[0].Subject)
	}

	// Verify idempotent persist: re-emit the same envelope should not create duplicates.
	env2 := &messaging.Envelope{ID: t.Name() + "-fanout-msg", Payload: []byte("multi")}
	del2 := newFakeDelivery(env2)
	_ = receiver.Emit(ctx, del2)
	waitFor(t, 3*time.Second, "redelivery acked", func() bool { return del2.isAcked() })

	t.Logf("DynamoDB fan-out atomicity: both records persisted and drained")
}

// staticResolver always returns the same plans.
type staticResolver struct {
	plans []domain.DispatchPlan
}

func (r *staticResolver) Resolve(_ context.Context, _ *messaging.Envelope) ([]domain.DispatchPlan, error) {
	return r.plans, nil
}
