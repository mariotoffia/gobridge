package bridge

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Fakes for the durable-reload preflight (HIGH-2 / HIGH-3).
// ---------------------------------------------------------------------------

// pendingOutboxStore implements ports.OutboxStore AND the OPTIONAL
// ports.OutboxDepthReporter capability, reporting a configurable pending count
// for every partition. It embeds the shared fakeOutboxStore for the required
// OutboxStore methods and overrides CountPending.
type pendingOutboxStore struct {
	fakeOutboxStore
	pending int
	err     error
}

func (s *pendingOutboxStore) CountPending(context.Context, string) (int, error) {
	return s.pending, s.err
}

// bareOutboxStore implements ports.OutboxStore but deliberately NOT
// ports.OutboxDepthReporter, so the preflight cannot prove it drained and must
// fail closed. It does NOT embed fakeOutboxStore (which now carries CountPending)
// so the depth capability is genuinely absent.
type bareOutboxStore struct{}

func (bareOutboxStore) Persist(context.Context, []*persistence.OutboxRecord) error { return nil }
func (bareOutboxStore) Claim(context.Context, string, persistence.LeaseToken, int) ([]*persistence.OutboxRecord, error) {
	return nil, nil
}
func (bareOutboxStore) Complete(context.Context, []string, persistence.LeaseToken) error { return nil }
func (bareOutboxStore) Expire(context.Context, time.Time, string) (int, error)           { return 0, nil }
func (bareOutboxStore) QueryPending(context.Context, string, int) ([]*persistence.OutboxRecord, error) {
	return nil, nil
}

var (
	_ ports.OutboxStore         = (*pendingOutboxStore)(nil)
	_ ports.OutboxDepthReporter = (*pendingOutboxStore)(nil)
	_ ports.DLQStore            = (*depthDLQStore)(nil)
	_ ports.DLQDepthReporter    = (*depthDLQStore)(nil)
	_ ports.DLQStore            = (*stubDLQStore)(nil)
)

// stubDLQStore is a no-op full ports.DLQStore with no depth capability.
type stubDLQStore struct{}

func (stubDLQStore) Get(context.Context, string) (routing.DLQEntry, error) {
	return routing.DLQEntry{}, nil
}
func (stubDLQStore) List(context.Context, routing.DLQFilter) ([]routing.DLQEntry, error) {
	return nil, nil
}
func (stubDLQStore) Write(context.Context, routing.DLQEntry) error                  { return nil }
func (stubDLQStore) Delete(context.Context, []string) (int, error)                  { return 0, nil }
func (stubDLQStore) DeleteByFilter(context.Context, routing.DLQFilter) (int, error) { return 0, nil }
func (stubDLQStore) Purge(context.Context, time.Time) (int, error)                  { return 0, nil }

// depthDLQStore adds the OPTIONAL ports.DLQDepthReporter capability.
type depthDLQStore struct {
	stubDLQStore
	depth int
	err   error
}

func (s depthDLQStore) Depth(context.Context) (int, error) { return s.depth, s.err }

// ---------------------------------------------------------------------------
// sharedOutboxPartitionSessions
// ---------------------------------------------------------------------------

func TestSharedOutboxPartitionSessions(t *testing.T) {
	t.Run("shared_outbox route with inline session and bindings", func(t *testing.T) {
		cfg := supervisorTestConfigWithSession("r1", "s1")
		got := sharedOutboxPartitionSessions(cfg)
		assert.Equal(t, map[string]struct{}{"s1": {}}, got)
	})

	t.Run("direct_hold route contributes no partition", func(t *testing.T) {
		cfg := supervisorTestConfig("r1") // direct_hold, no session
		got := sharedOutboxPartitionSessions(cfg)
		assert.Empty(t, got)
	})

	t.Run("nil config is empty, not a panic", func(t *testing.T) {
		assert.Empty(t, sharedOutboxPartitionSessions(nil))
	})
}

// ---------------------------------------------------------------------------
// durableReloadPreflight — direct logic tests
// ---------------------------------------------------------------------------

func TestDurableReloadPreflight_OutboxPartitionOrphaned(t *testing.T) {
	ctx := context.Background()

	// oldCfg: a shared_outbox route owning outbox partition SESSION#s1.
	oldCfg := supervisorTestConfigWithSession("r1", "s1")

	t.Run("orphaned by store removal, backlog present -> refuse", func(t *testing.T) {
		s := NewSupervisor()
		oldRt := goruntime.New(goruntime.WithOutboxStore(&pendingOutboxStore{pending: 7}))
		newCfg := quickCfg("r2") // direct_hold, no outbox store, no session

		err := s.durableReloadPreflight(ctx, oldRt, oldCfg, newCfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "SESSION#s1")
		assert.Contains(t, err.Error(), "7 pending")
	})

	t.Run("orphaned by topology change, outbox store kept -> refuse", func(t *testing.T) {
		s := NewSupervisor()
		oldRt := goruntime.New(goruntime.WithOutboxStore(&pendingOutboxStore{pending: 3}))
		// Keep the outbox store, but flip the route to direct_hold and drop the
		// inline session so SESSION#s1 loses its drainer in the new topology.
		newCfg := supervisorTestConfigWithSession("r1", "s1")
		newCfg.Routes[0].DeliveryMode = "direct_hold"
		newCfg.Routes[0].Session = nil

		err := s.durableReloadPreflight(ctx, oldRt, oldCfg, newCfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "SESSION#s1")
	})

	t.Run("orphaned but backlog empty -> allow", func(t *testing.T) {
		s := NewSupervisor()
		oldRt := goruntime.New(goruntime.WithOutboxStore(&pendingOutboxStore{pending: 0}))
		newCfg := quickCfg("r2")

		require.NoError(t, s.durableReloadPreflight(ctx, oldRt, oldCfg, newCfg))
	})

	t.Run("partition retained in new config -> allow even with backlog", func(t *testing.T) {
		s := NewSupervisor()
		oldRt := goruntime.New(goruntime.WithOutboxStore(&pendingOutboxStore{pending: 99}))
		// New config keeps the same shared_outbox route, so SESSION#s1 still has a
		// drainer: no strand, no refusal — the preflight refuses ORPHANS only.
		newCfg := supervisorTestConfigWithSession("r1", "s1")

		require.NoError(t, s.durableReloadPreflight(ctx, oldRt, oldCfg, newCfg))
	})

	t.Run("orphaned with discard flag -> allow", func(t *testing.T) {
		s := NewSupervisor(WithAllowDestructiveReload(true))
		oldRt := goruntime.New(goruntime.WithOutboxStore(&pendingOutboxStore{pending: 7}))
		newCfg := quickCfg("r2")

		require.NoError(t, s.durableReloadPreflight(ctx, oldRt, oldCfg, newCfg))
	})

	t.Run("depth capability absent -> refuse (fail closed)", func(t *testing.T) {
		s := NewSupervisor()
		// bareOutboxStore does not implement OutboxDepthReporter, so the store
		// cannot prove the orphaned partition is drained.
		oldRt := goruntime.New(goruntime.WithOutboxStore(bareOutboxStore{}))
		newCfg := quickCfg("r2")

		err := s.durableReloadPreflight(ctx, oldRt, oldCfg, newCfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot prove")
	})

	t.Run("depth query errors -> refuse (fail closed)", func(t *testing.T) {
		s := NewSupervisor()
		oldRt := goruntime.New(goruntime.WithOutboxStore(&pendingOutboxStore{err: errors.New("boom")}))
		newCfg := quickCfg("r2")

		err := s.durableReloadPreflight(ctx, oldRt, oldCfg, newCfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "depth query failed")
	})
}

func TestDurableReloadPreflight_DLQStoreRemoval(t *testing.T) {
	ctx := context.Background()

	// oldCfg has a DLQ store; newCfg drops it entirely.
	oldCfg := supervisorTestConfig("r1")
	oldCfg.Stores.DLQ = &ports.StoreConfig{Type: "memory"}
	newCfg := supervisorTestConfig("r1") // no DLQ store

	t.Run("dlq removed with standing backlog -> refuse", func(t *testing.T) {
		s := NewSupervisor()
		oldRt := goruntime.New(goruntime.WithDLQStore(depthDLQStore{depth: 5}))

		err := s.durableReloadPreflight(ctx, oldRt, oldCfg, newCfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "dlq store")
	})

	t.Run("dlq removed but empty -> allow", func(t *testing.T) {
		s := NewSupervisor()
		oldRt := goruntime.New(goruntime.WithDLQStore(depthDLQStore{depth: 0}))

		require.NoError(t, s.durableReloadPreflight(ctx, oldRt, oldCfg, newCfg))
	})

	t.Run("dlq removed but store cannot prove depth -> refuse (fail closed)", func(t *testing.T) {
		s := NewSupervisor()
		oldRt := goruntime.New(goruntime.WithDLQStore(stubDLQStore{}))

		err := s.durableReloadPreflight(ctx, oldRt, oldCfg, newCfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot prove")
	})

	t.Run("dlq removed empty with discard flag -> allow", func(t *testing.T) {
		s := NewSupervisor(WithAllowDestructiveReload(true))
		oldRt := goruntime.New(goruntime.WithDLQStore(stubDLQStore{}))

		require.NoError(t, s.durableReloadPreflight(ctx, oldRt, oldCfg, newCfg))
	})
}

// ---------------------------------------------------------------------------
// Integration: apply() must refuse the swap and keep the OLD runtime serving
// when a reload would strand a non-empty outbox partition (HIGH-2 wiring).
// ---------------------------------------------------------------------------

// backlogStoreFactory yields a pendingOutboxStore reporting a configurable
// standing backlog for every partition (so the preflight sees a non-empty
// orphaned partition) plus the shared fakeLeaseStore.
type backlogStoreFactory struct{ pending int }

func (f *backlogStoreFactory) NewLeaseStore(context.Context, ports.PluginConfig) (ports.LeaseStore, error) {
	return &fakeLeaseStore{}, nil
}
func (f *backlogStoreFactory) NewOutboxStore(context.Context, ports.PluginConfig, ports.OutboxRuntimeOptions) (ports.OutboxStore, error) {
	return &pendingOutboxStore{pending: f.pending}, nil
}
func (f *backlogStoreFactory) NewDLQStore(context.Context, ports.PluginConfig) (ports.DLQStore, error) {
	return nil, nil
}

func newBacklogSupervisor(pending int, opts ...SupervisorOption) *Supervisor {
	opts = append([]SupervisorOption{WithSupervisorBlueprintValidator(config.Validate)}, opts...)
	s := NewSupervisor(opts...)
	s.RegisterTransport("fake", &fakeTransportFactory{})
	s.RegisterTransport("exclusive", &exclusiveTransportFactory{})
	s.RegisterStoreFactory("memory", &backlogStoreFactory{pending: pending})
	return s
}

func TestSupervisor_ReloadOrphansOutboxPartition_RefusedWhileBacklog(t *testing.T) {
	onSwap, swaps := swapChan(1)
	s := newBacklogSupervisor(7, WithOnSwap(onSwap))

	ch := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, supervisorTestConfigWithSession("r1", "s1"), ch)
	defer func() { cancel(); <-errCh }()

	oldRt := s.Runtime()
	require.NotNil(t, oldRt)

	// Reload drops the shared_outbox route, its session, AND the outbox store,
	// orphaning SESSION#s1 while it still holds 7 pending records.
	require.True(t, sendConfig(ch, quickCfg("r2"), time.Second))

	ev := awaitSwap(t, swaps)
	require.Error(t, ev.Error)
	assert.Contains(t, ev.Error.Error(), "strand durable backlog")
	assert.Same(t, oldRt, s.Runtime(), "old runtime must keep serving so its backlog is still drained")
}

func TestSupervisor_ReloadOrphansOutboxPartition_AllowedWithDiscardFlag(t *testing.T) {
	onSwap, swaps := swapChan(1)
	s := newBacklogSupervisor(7, WithOnSwap(onSwap), WithAllowDestructiveReload(true))

	ch := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, supervisorTestConfigWithSession("r1", "s1"), ch)
	defer func() { cancel(); <-errCh }()

	oldRt := s.Runtime()
	require.NotNil(t, oldRt)

	require.True(t, sendConfig(ch, quickCfg("r2"), time.Second))

	ev := awaitSwap(t, swaps)
	require.NoError(t, ev.Error, "discard flag forces the reload through")
	assert.NotSame(t, oldRt, s.Runtime(), "a forced reload must install the new runtime")
}

// ---------------------------------------------------------------------------
// destructiveReloadShape — config-only stranding shape (paused-reload path).
// ---------------------------------------------------------------------------

func TestDestructiveReloadShape(t *testing.T) {
	t.Run("nil configs are non-destructive", func(t *testing.T) {
		assert.False(t, destructiveReloadShape(nil, quickCfg("r1")))
		assert.False(t, destructiveReloadShape(quickCfg("r1"), nil))
	})

	t.Run("outbox store removed -> destructive", func(t *testing.T) {
		oldCfg := supervisorTestConfigWithSession("r1", "s1") // has outbox store
		newCfg := quickCfg("r2")                              // no outbox store
		assert.True(t, destructiveReloadShape(oldCfg, newCfg))
	})

	t.Run("dlq store removed -> destructive", func(t *testing.T) {
		oldCfg := supervisorTestConfig("r1")
		oldCfg.Stores.DLQ = &ports.StoreConfig{Type: "memory"}
		newCfg := supervisorTestConfig("r1") // no dlq
		assert.True(t, destructiveReloadShape(oldCfg, newCfg))
	})

	t.Run("store identity changed -> destructive", func(t *testing.T) {
		oldCfg := supervisorTestConfigWithSession("r1", "s1")
		newCfg := supervisorTestConfigWithSession("r1", "s1")
		newCfg.Stores.Outbox = &ports.StoreConfig{Type: "sqlite"} // type flip
		assert.True(t, destructiveReloadShape(oldCfg, newCfg))
	})

	t.Run("orphaned shared_outbox partition, store kept -> destructive", func(t *testing.T) {
		oldCfg := supervisorTestConfigWithSession("r1", "s1")
		newCfg := supervisorTestConfigWithSession("r1", "s1")
		// Keep the outbox store but drop the session/route so SESSION#s1 loses
		// its drainer.
		newCfg.Routes[0].DeliveryMode = "direct_hold"
		newCfg.Routes[0].Session = nil
		assert.True(t, destructiveReloadShape(oldCfg, newCfg))
	})

	t.Run("partition retained -> non-destructive", func(t *testing.T) {
		oldCfg := supervisorTestConfigWithSession("r1", "s1")
		newCfg := supervisorTestConfigWithSession("r1", "s1")
		assert.False(t, destructiveReloadShape(oldCfg, newCfg))
	})

	t.Run("lease-only removal -> non-destructive", func(t *testing.T) {
		oldCfg := supervisorTestConfig("r1")
		oldCfg.Stores.Lease = &ports.StoreConfig{Type: "memory"}
		newCfg := supervisorTestConfig("r1") // lease dropped, no outbox/dlq anywhere
		assert.False(t, destructiveReloadShape(oldCfg, newCfg))
	})
}

// ---------------------------------------------------------------------------
// Integration: a PAUSED destructive reload must be REFUSED (not recorded) so a
// later StartBridge cannot resume onto a config that strands durable backlog.
// While paused the old runtime is Stopped and its stores CLOSED, so the live
// depth preflight cannot run — the paused path fails closed on config shape
// alone (FIX 2, HIGH-3 paused path).
// ---------------------------------------------------------------------------

func TestSupervisor_PausedDestructiveReload_Refused(t *testing.T) {
	onSwap, swaps := swapChan(1)
	s := newBacklogSupervisor(7, WithOnSwap(onSwap))

	ch := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, supervisorTestConfigWithSession("r1", "s1"), ch)
	defer func() { cancel(); <-errCh }()

	oldCfg := s.Config()
	require.NotNil(t, oldCfg)

	// Pause the bridge: this Stops the runtime and CLOSES its outbox store.
	require.NoError(t, s.StopBridge(context.Background()))

	// A destructive reload (drops the outbox store AND orphans SESSION#s1) must
	// be refused while paused rather than recorded as the resume target.
	require.True(t, sendConfig(ch, quickCfg("r2"), time.Second))

	ev := awaitSwap(t, swaps)
	require.Error(t, ev.Error, "a paused destructive reload must resolve as a FAILURE, not a deferred success")
	assert.False(t, ev.Deferred, "a refusal is a definitive failure, not a deferred (committed-not-applied) event")
	assert.Contains(t, ev.Error.Error(), "paused destructive reload")
	assert.Same(t, oldCfg, s.Config(),
		"the destructive config must NOT be recorded; oldCfg stays the resume target")
}

func TestSupervisor_PausedDestructiveReload_AllowedWithDiscardFlag(t *testing.T) {
	onSwap, swaps := swapChan(1)
	s := newBacklogSupervisor(7, WithOnSwap(onSwap), WithAllowDestructiveReload(true))

	ch := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, supervisorTestConfigWithSession("r1", "s1"), ch)
	defer func() { cancel(); <-errCh }()

	oldCfg := s.Config()
	require.NotNil(t, oldCfg)

	require.NoError(t, s.StopBridge(context.Background()))

	newCfg := quickCfg("r2")
	require.True(t, sendConfig(ch, newCfg, time.Second))

	ev := awaitSwap(t, swaps)
	require.NoError(t, ev.Error, "the discard flag forces the paused reload to be recorded")
	assert.True(t, ev.Deferred, "a forced paused reload is deferred (committed-not-applied), not applied now")
	assert.Equal(t, newCfg, s.Config(), "the forced config becomes the resume target")
}

// ---------------------------------------------------------------------------
// FIX 4: observable post-swap strand signal.
//
// The preflight proves an orphaned shared_outbox partition empty at CHECK time
// (so the swap is NOT refused), but a record can still land in the swap window
// (late ingress) or a claimed send can fail. The supervisor re-queries the
// orphaned partition on the NEW runtime's store AFTER a successful swap and
// surfaces a non-empty result as an observable signal (MetricOutboxStranded +
// an Error log) instead of a silent strand.
// ---------------------------------------------------------------------------

// lateIngressStoreFactory hands out outbox stores whose pending count is chosen
// per creation call. The OLD runtime (created first, at boot) reports 0 pending
// so the preflight passes; the NEW runtime (created at swap) reports a non-zero
// pending for the orphaned partition, simulating a record that landed in the
// swap window.
type lateIngressStoreFactory struct {
	mu            sync.Mutex
	calls         int
	pendingByCall []int // pending for outbox store creation N; last value repeats
}

func (f *lateIngressStoreFactory) NewLeaseStore(context.Context, ports.PluginConfig) (ports.LeaseStore, error) {
	return &fakeLeaseStore{}, nil
}

func (f *lateIngressStoreFactory) NewOutboxStore(context.Context, ports.PluginConfig, ports.OutboxRuntimeOptions) (ports.OutboxStore, error) {
	f.mu.Lock()
	i := f.calls
	f.calls++
	f.mu.Unlock()
	pending := 0
	if len(f.pendingByCall) > 0 {
		if i < len(f.pendingByCall) {
			pending = f.pendingByCall[i]
		} else {
			pending = f.pendingByCall[len(f.pendingByCall)-1]
		}
	}
	return &pendingOutboxStore{pending: pending}, nil
}

func (f *lateIngressStoreFactory) NewDLQStore(context.Context, ports.PluginConfig) (ports.DLQStore, error) {
	return nil, nil
}

func TestSupervisor_ReloadOrphanStrandedAfterSwap_EmitsObservableSignal(t *testing.T) {
	rec := &ports.RecordingExporter{}
	onSwap, swaps := swapChan(1)
	s := NewSupervisor(
		WithSupervisorBlueprintValidator(config.Validate),
		WithOnSwap(onSwap),
		WithSupervisorMetrics(rec),
	)
	s.RegisterTransport("fake", &fakeTransportFactory{})
	s.RegisterTransport("exclusive", &exclusiveTransportFactory{})
	// Old outbox store empty at preflight; new store holds 1 late-ingress record.
	s.RegisterStoreFactory("memory", &lateIngressStoreFactory{pendingByCall: []int{0, 1}})

	ch := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, supervisorTestConfigWithSession("r1", "s1"), ch)
	defer func() { cancel(); <-errCh }()

	oldRt := s.Runtime()
	require.NotNil(t, oldRt)

	// Reload orphans route r1's shared_outbox partition SESSION#s1 by flipping the
	// route to direct_hold and dropping its inline session (outbox store
	// RETAINED). SESSION#s1 loses its drainer while its partition is empty at
	// check time, so the swap SUCCEEDS. NOTE: this deliberately does NOT change a
	// lease-bearing session_id (s1 -> s2) — that is refused as a cluster
	// ownership invariant (HIGH-2); dropping the session orphans the same
	// partition without tripping that refusal.
	newCfg := supervisorTestConfigWithSession("r1", "s1")
	newCfg.Routes[0].DeliveryMode = "direct_hold"
	newCfg.Routes[0].Session = nil
	require.True(t, sendConfig(ch, newCfg, time.Second))

	ev := awaitSwap(t, swaps)
	require.NoError(t, ev.Error, "swap must succeed: the orphaned partition was empty at preflight check time")
	assert.NotSame(t, oldRt, s.Runtime(), "a successful reload installs the new runtime")

	// The post-swap re-check on the NEW store observes the late-ingress record
	// and emits the strand counter, tagged with the orphaned partition.
	partition := persistence.OutboxPartitionKey("s1", "")
	strands := rec.FindEntries(shared.MetricOutboxStranded)
	require.NotEmpty(t, strands, "a non-empty orphaned partition after swap must emit MetricOutboxStranded")

	var found bool
	for _, e := range strands {
		for _, tag := range e.Tags {
			if tag.Key == shared.TagKeyPartition && tag.Value == partition {
				found = true
				assert.Equal(t, int64(1), e.IValue, "strand counter value is the pending count")
			}
		}
	}
	require.True(t, found, "strand metric must be tagged with the orphaned partition %s", partition)
}

// A reload that orphans a partition whose backlog is genuinely drained (0
// pending on the new store too) must NOT emit a strand signal — the observable
// residual is only for a real late-ingress/claim-failure strand.
func TestSupervisor_ReloadOrphanDrainedAfterSwap_NoStrandSignal(t *testing.T) {
	rec := &ports.RecordingExporter{}
	onSwap, swaps := swapChan(1)
	s := NewSupervisor(
		WithSupervisorBlueprintValidator(config.Validate),
		WithOnSwap(onSwap),
		WithSupervisorMetrics(rec),
	)
	s.RegisterTransport("fake", &fakeTransportFactory{})
	s.RegisterTransport("exclusive", &exclusiveTransportFactory{})
	// Both stores empty: no strand anywhere.
	s.RegisterStoreFactory("memory", &lateIngressStoreFactory{pendingByCall: []int{0, 0}})

	ch := make(chan *ports.BridgeConfig, 1)
	cancel, errCh := quickSupervisorRun(s, supervisorTestConfigWithSession("r1", "s1"), ch)
	defer func() { cancel(); <-errCh }()

	// Orphan SESSION#s1 without changing a lease-bearing session_id (refused as a
	// HIGH-2 cluster invariant): drop the inline session and switch to direct_hold.
	newCfg := supervisorTestConfigWithSession("r1", "s1")
	newCfg.Routes[0].DeliveryMode = "direct_hold"
	newCfg.Routes[0].Session = nil
	require.True(t, sendConfig(ch, newCfg, time.Second))

	ev := awaitSwap(t, swaps)
	require.NoError(t, ev.Error)
	assert.Empty(t, rec.FindEntries(shared.MetricOutboxStranded),
		"a genuinely-drained orphaned partition must not emit a strand signal")
}
