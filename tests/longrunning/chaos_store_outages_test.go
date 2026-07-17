//go:build longrunning

package longrunning_test

import (
	"context"
	"fmt"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/tests/testutil/prodid"
	"github.com/mariotoffia/gobridge/testutil/mqttlocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

func TestTask14_StoreOutageMatrix(t *testing.T) {
	_ = withFreshInfra(t)
	scenarios := []struct {
		name                   string
		failDLQ, failOutbox    bool
		failLease, expectInDLQ bool
	}{
		{name: "dlq", failDLQ: true, expectInDLQ: true},
		{name: "outbox", failOutbox: true},
		{name: "lease", failLease: true},
		{name: "all", failDLQ: true, failOutbox: true, failLease: true, expectInDLQ: true},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			runStoreOutageScenario(t, scenario.failDLQ, scenario.failOutbox, scenario.failLease, scenario.expectInDLQ)
		})
	}
}

func runStoreOutageScenario(
	t *testing.T,
	failDLQ, failOutbox, failLease, expectInDLQ bool,
) {
	t.Helper()
	const messageCount = 20
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	prefix := mqttlocal.UniqueClientID("task14-store")
	inputURL, inputClient := setupSQSQueue(t, prefix+"-in")
	baseLease, baseOutbox := setupDynamoStores(t)
	lease := &outageLeaseStore{LeaseStore: baseLease}
	outbox := &outageOutboxStore{OutboxStore: baseOutbox}
	baseDLQ := &lrDLQStore{}
	dlq := &outageDLQStore{inner: baseDLQ}
	topic := prefix + "/output"
	collector := newMQTTCollector(t, topic, prefix+"-collector")
	sessionID := mqttlocal.UniqueClientID(prefix + "-session")
	route := goruntime.RouteConfig{
		ID: prefix + "-route",
		Policy: routing.RoutePolicy{
			DeliveryMode:      routing.DeliverySharedOutbox,
			AckAfter:          routing.AckAfterOutboxPersist,
			MaxReplayAttempts: 20,
		},
		Resolver: goruntime.NewStaticResolver(routing.DispatchPlan{
			BindingID: prefix + "-binding",
			Address:   topic,
		}),
		Bindings: []routing.DestinationBinding{{
			ID: prefix + "-binding", SessionID: sessionID,
		}},
	}
	startRuntime := func(instanceID string) *goruntime.Runtime {
		session := newMQTTSession(t, sessionID, connectivity.SessionExclusive)
		var sender ports.Sender = setupMQTTSender(t, session)
		if expectInDLQ {
			sender = &permanentFailSender{}
		}
		sessionConfig := lrSessionConfig(sessionID)
		current := goruntime.New(
			goruntime.WithInstanceID(instanceID),
			goruntime.WithLeaseStore(lease),
			goruntime.WithOutboxStore(outbox),
			goruntime.WithDLQStore(dlq),
			goruntime.WithLogger(testLogger(t)),
		)
		require.NoError(t, current.AddRoute(
			route, newSQSReceiver(t, inputURL), sender, session, &sessionConfig,
		))
		require.NoError(t, current.Start(ctx))
		t.Cleanup(func() { _ = current.Stop(context.Background()) })
		return current
	}
	rt := startRuntime(prefix)
	gobridgesync(t, 20*time.Second, rt)

	dlq.failing.Store(failDLQ)
	outbox.failing.Store(failOutbox)
	lease.failing.Store(failLease)
	if failLease {
		wait.Until(t, 10*time.Second, "lease outage reached production store path", func() bool {
			return lease.failures.Load() > 0
		})
	}
	expected := task14ProducerBodies(messageCount)
	sendBulkToSQS(t, inputClient, inputURL, messageCount, nil)
	if failOutbox {
		wait.Until(t, 15*time.Second, "outbox outage reached production store path", func() bool {
			return outbox.failures.Load() > 0
		})
	}
	if failDLQ && !failOutbox && !failLease {
		wait.Until(t, 30*time.Second, "DLQ outage reached production store path", func() bool {
			return dlq.failures.Load() > 0
		})
	}

	// In the all-down case, DLQ writes are causally downstream of lease-backed
	// outbox persistence. Keep DLQ unavailable while recovering those
	// prerequisites, then require the pending record to hit the still-failed DLQ.
	if failLease {
		wait.Until(t, 15*time.Second, "lease outage forces bounded standby degradation", func() bool {
			return rt.Role() == "standby" && !rt.DeepHealth(context.Background()).ReadyForTraffic
		})
		// Paho sessions are intentionally single-use after step-down Close.
		// Production escalates ErrSessionUnrecoverable so an orchestrator creates
		// a fresh runtime/session. Model that documented recovery boundary here.
		require.NoError(t, rt.Stop(context.Background()))
		outbox.failing.Store(false)
		lease.failing.Store(false)
		rt = startRuntime(prefix + "-replacement")
		gobridgesync(t, 45*time.Second, rt)
	} else {
		outbox.failing.Store(false)
	}
	if failDLQ {
		wait.Until(t, 60*time.Second, "DLQ outage observed before final recovery", func() bool {
			return dlq.failures.Load() > 0
		})
		dlq.failing.Store(false)
	}

	wait.Until(t, 2*time.Minute, "store outage exact terminal accounting", func() bool {
		return len(reconcileStoreOutage(expected, collector, baseDLQ).Missing) == 0
	})
	require.NoError(t, rt.WaitQuiescent(ctx, goruntime.QuiescenceOptions{
		MinQuiet: time.Second,
		Timeout:  20 * time.Second,
	}))
	report := reconcileStoreOutage(expected, collector, baseDLQ)
	require.True(t, report.Exact(), "store outage accounting: %s", report.String())
	require.Empty(t, report.IntentionallyDropped)
	if expectInDLQ {
		wantDLQ := append([]string(nil), expected...)
		sort.Strings(wantDLQ)
		require.Equal(t, wantDLQ, report.DLQ)
		require.Zero(t, collector.count())
	} else {
		require.Empty(t, report.DLQ)
		require.Equal(t, messageCount, collector.count())
	}
	pending, supported, err := rt.OutboxPending(ctx, persistence.OutboxPartitionKey(sessionID, ""))
	require.NoError(t, err)
	require.True(t, supported)
	require.Zero(t, pending, "bridge-side outbox must complete independently of the output oracle")
}

func task14ProducerBodies(count int) []string {
	expected := make([]string, count)
	for i := range count {
		expected[i] = fmt.Sprintf(`{"seq":%d}`, i)
	}
	return expected
}

func reconcileStoreOutage(
	expected []string,
	collector *mqttCollector,
	dlq *lrDLQStore,
) prodid.Report {
	accountant, err := prodid.New(expected, false)
	if err != nil {
		panic(err)
	}
	for _, envelope := range collector.getMessages() {
		accountant.ObserveOutput(string(envelope.Payload()), envelope.ID())
	}
	for _, entry := range dlq.getEntries() {
		envelope := entry.Snapshot()
		accountant.ObserveDLQ(string(envelope.Payload()), envelope.ID())
	}
	return accountant.Reconcile()
}

type outageLeaseStore struct {
	ports.LeaseStore
	failing  atomic.Bool
	failures atomic.Int64
}

func (s *outageLeaseStore) fail() error {
	if !s.failing.Load() {
		return nil
	}
	s.failures.Add(1)
	return shared.ErrUnavailable.WithMessage("task14 lease outage")
}

func (s *outageLeaseStore) Acquire(ctx context.Context, leaseID, ownerID string, ttl time.Duration, endpoints map[string]string) (persistence.LeaseToken, error) {
	if err := s.fail(); err != nil {
		return persistence.LeaseToken{}, err
	}
	return s.LeaseStore.Acquire(ctx, leaseID, ownerID, ttl, endpoints)
}

func (s *outageLeaseStore) Renew(ctx context.Context, leaseID string, token persistence.LeaseToken, ttl time.Duration, endpoints map[string]string) (persistence.LeaseToken, error) {
	if err := s.fail(); err != nil {
		return persistence.LeaseToken{}, err
	}
	return s.LeaseStore.Renew(ctx, leaseID, token, ttl, endpoints)
}

func (s *outageLeaseStore) Release(ctx context.Context, leaseID string, token persistence.LeaseToken) error {
	if err := s.fail(); err != nil {
		return err
	}
	return s.LeaseStore.Release(ctx, leaseID, token)
}

func (s *outageLeaseStore) Current(ctx context.Context, leaseID string) (persistence.LeaseInfo, error) {
	if err := s.fail(); err != nil {
		return persistence.LeaseInfo{}, err
	}
	return s.LeaseStore.Current(ctx, leaseID)
}

type outageOutboxStore struct {
	ports.OutboxStore
	failing  atomic.Bool
	failures atomic.Int64
}

func (s *outageOutboxStore) fail() error {
	if !s.failing.Load() {
		return nil
	}
	s.failures.Add(1)
	return shared.ErrUnavailable.WithMessage("task14 outbox outage")
}

func (s *outageOutboxStore) Persist(ctx context.Context, records []*persistence.OutboxRecord) error {
	if err := s.fail(); err != nil {
		return err
	}
	return s.OutboxStore.Persist(ctx, records)
}

func (s *outageOutboxStore) Claim(ctx context.Context, key string, token persistence.LeaseToken, limit int) ([]*persistence.OutboxRecord, error) {
	if err := s.fail(); err != nil {
		return nil, err
	}
	return s.OutboxStore.Claim(ctx, key, token, limit)
}

func (s *outageOutboxStore) Complete(ctx context.Context, ids []string, token persistence.LeaseToken) error {
	if err := s.fail(); err != nil {
		return err
	}
	return s.OutboxStore.Complete(ctx, ids, token)
}

func (s *outageOutboxStore) Expire(ctx context.Context, before time.Time, partition string) (int, error) {
	if err := s.fail(); err != nil {
		return 0, err
	}
	return s.OutboxStore.Expire(ctx, before, partition)
}

func (s *outageOutboxStore) QueryPending(ctx context.Context, key string, limit int) ([]*persistence.OutboxRecord, error) {
	if err := s.fail(); err != nil {
		return nil, err
	}
	return s.OutboxStore.QueryPending(ctx, key, limit)
}

func (s *outageOutboxStore) Release(ctx context.Context, ids []string, token persistence.LeaseToken) error {
	if err := s.fail(); err != nil {
		return err
	}
	return s.OutboxStore.(ports.OutboxReleaser).Release(ctx, ids, token)
}

func (s *outageOutboxStore) CountPending(ctx context.Context, key string) (int, error) {
	if err := s.fail(); err != nil {
		return 0, err
	}
	return s.OutboxStore.(ports.OutboxDepthReporter).CountPending(ctx, key)
}

type outageDLQStore struct {
	inner    *lrDLQStore
	failing  atomic.Bool
	failures atomic.Int64
}

func (s *outageDLQStore) Write(ctx context.Context, entry routing.DLQEntry) error {
	if s.failing.Load() {
		s.failures.Add(1)
		return shared.ErrUnavailable.WithMessage("task14 DLQ outage")
	}
	return s.inner.Write(ctx, entry)
}

func (s *outageDLQStore) List(ctx context.Context, filter routing.DLQFilter) ([]routing.DLQEntry, error) {
	return s.inner.List(ctx, filter)
}
func (s *outageDLQStore) Get(ctx context.Context, id string) (routing.DLQEntry, error) {
	return s.inner.Get(ctx, id)
}
func (s *outageDLQStore) Delete(ctx context.Context, ids []string) (int, error) {
	return s.inner.Delete(ctx, ids)
}
func (s *outageDLQStore) DeleteByFilter(ctx context.Context, filter routing.DLQFilter) (int, error) {
	return s.inner.DeleteByFilter(ctx, filter)
}
func (s *outageDLQStore) Purge(ctx context.Context, before time.Time) (int, error) {
	return s.inner.Purge(ctx, before)
}

var (
	_ ports.LeaseStore          = (*outageLeaseStore)(nil)
	_ ports.OutboxStore         = (*outageOutboxStore)(nil)
	_ ports.OutboxReleaser      = (*outageOutboxStore)(nil)
	_ ports.OutboxDepthReporter = (*outageOutboxStore)(nil)
	_ ports.DLQStore            = (*outageDLQStore)(nil)
)
