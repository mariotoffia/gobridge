package bridge

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/domain/clock/clocktest"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"
)

// ===========================================================================
// Finding #1 (builder_resolve.go:42) — credential resolution must NOT mutate
// the canonical config the Supervisor keeps for rollback/restart.
// ===========================================================================

// TestCloneConfigForBuild_KeepsCanonicalConfigPristine proves that mutating a
// cloned config's credentialed PluginConfig (exactly what ApplyCredentials does
// in place) does not reach back into the original — the property that keeps a
// rollback config re-resolvable with its watcher intact.
func TestCloneConfigForBuild_KeepsCanonicalConfigPristine(t *testing.T) {
	t.Parallel()

	orig := &ports.BridgeConfig{
		Sessions: []ports.SessionDef{
			{ID: "s1", Transport: "mqtt", Config: &testCredConfig{URI: "file://creds"}},
		},
		Receivers: []ports.ReceiverDef{
			{ID: "r1", Transport: "mqtt", Config: &testCredConfig{URI: "file://creds"}},
		},
		Senders: []ports.SenderDef{
			{ID: "tx1", Transport: "mqtt", Config: &testCredConfig{URI: "file://creds"}},
		},
	}

	clone, err := cloneConfigForBuild(orig)
	require.NoError(t, err)

	// The clone's configs are DISTINCT pointers from the originals.
	require.NotSame(t, orig.Sessions[0].Config, clone.Sessions[0].Config)
	require.NotSame(t, orig.Receivers[0].Config, clone.Receivers[0].Config)
	require.NotSame(t, orig.Senders[0].Config, clone.Senders[0].Config)

	// Simulate ApplyCredentials mutating the clone (inline creds + cleared URI).
	for _, cc := range []ports.PluginConfig{clone.Sessions[0].Config, clone.Receivers[0].Config, clone.Senders[0].Config} {
		require.NoError(t, cc.(ports.CredentialedConfig).ApplyCredentials(
			connectivity.NewCredentialSet(pwCred("resolved", "secret"), nil)))
	}

	// Originals stay pristine: URI intact, no inlined username.
	assert.Equal(t, "file://creds", orig.Sessions[0].Config.(*testCredConfig).URI)
	assert.Equal(t, "", orig.Sessions[0].Config.(*testCredConfig).Username)
	assert.Equal(t, "file://creds", orig.Receivers[0].Config.(*testCredConfig).URI)
	assert.Equal(t, "file://creds", orig.Senders[0].Config.(*testCredConfig).URI)
}

// TestBuild_LeavesCanonicalSessionConfigPristine drives the full Builder.Build
// path and asserts the CALLER's config (the object a Supervisor retains as its
// rollback snapshot) is untouched after credential resolution, while the
// factory still receives the resolved material on the private clone.
func TestBuild_LeavesCanonicalSessionConfigPristine(t *testing.T) {
	cfg := testConfig()
	canonical := &testCredConfig{URI: "file://test/creds"}
	cfg.Sessions[0].Config = canonical

	cs := &fakeCredentialStore{
		creds: map[string]*connectivity.CredentialSet{
			"file://test/creds": connectivity.NewCredentialSet(pwCred("resolved-user", "resolved-pass"), nil),
		},
	}
	mqttFactory := &capturingTransportFactory{}

	rt, err := NewBuilder(cfg, WithCredentialStore(cs)).
		RegisterTransportFactory("mqtt", mqttFactory).
		RegisterTransportFactory("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		Build(context.Background())
	require.NoError(t, err)
	require.NotNil(t, rt)

	// Canonical (rollback) config keeps its credentials_uri and no inline creds:
	// a later recovery rebuild re-resolves and re-registers a watcher.
	assert.Equal(t, "file://test/creds", canonical.URI,
		"canonical config credentials_uri must survive the build for rollback re-resolution")
	assert.Equal(t, "", canonical.Username, "canonical config must not be polluted with resolved creds")

	// The factory nonetheless received the resolved material on the clone.
	captured := mqttFactory.capturedSessionSpec.Config.(*testCredConfig)
	assert.Equal(t, "resolved-user", captured.Username)
	assert.Equal(t, "", captured.URI, "the working clone has credentials_uri cleared as usual")
}

// ===========================================================================
// Finding #2 (credential_refresh.go:221) — a dead credential watcher must be
// observable (metric + WARN), not permanently silent.
// ===========================================================================

// deadWatchPushStore hands out a channel the test closes on demand to simulate
// a watcher dying while the refresher is still live.
type deadWatchPushStore struct {
	ch      chan *connectivity.CredentialSet
	watched atomic.Int32
}

func (p *deadWatchPushStore) Watch(_ context.Context, _ string) (<-chan *connectivity.CredentialSet, error) {
	p.watched.Add(1)
	return p.ch, nil
}

// watchErrPushStore always fails Watch to exercise the Watch-error surface.
type watchErrPushStore struct{}

func (watchErrPushStore) Watch(_ context.Context, _ string) (<-chan *connectivity.CredentialSet, error) {
	return nil, errors.New("watch backend unavailable")
}

func TestCredentialRefresher_DeadWatcher_SurfacesMetricAndLog(t *testing.T) {
	t.Parallel()

	rec := &ports.RecordingExporter{}
	buf := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	push := &deadWatchPushStore{ch: make(chan *connectivity.CredentialSet)}
	r := NewCredentialRefresher(push, logger, WithRefresherMetrics(rec))
	defer r.Close()

	r.Watch(context.Background(), "file://creds", &credAwareFakeSession{fakeSession: &fakeSession{}})
	require.Eventually(t, func() bool { return push.watched.Load() == 1 }, 2*time.Second, 5*time.Millisecond)

	// Kill the watcher while the refresher parent ctx is still alive.
	close(push.ch)

	require.Eventually(t, func() bool {
		return len(rec.FindEntries(shared.MetricCredentialRefreshFailures)) == 1
	}, 2*time.Second, 5*time.Millisecond, "dead watcher must emit a refresh-failure metric")
	require.Eventually(t, func() bool {
		return strings.Contains(buf.String(), "watcher channel closed unexpectedly")
	}, 2*time.Second, 5*time.Millisecond, "dead watcher must be logged at WARN")
}

func TestCredentialRefresher_WatchError_SurfacesMetric(t *testing.T) {
	t.Parallel()

	rec := &ports.RecordingExporter{}
	r := NewCredentialRefresher(watchErrPushStore{}, nil, WithRefresherMetrics(rec))
	defer r.Close()

	r.Watch(context.Background(), "file://creds", &credAwareFakeSession{fakeSession: &fakeSession{}})

	require.Eventually(t, func() bool {
		return len(rec.FindEntries(shared.MetricCredentialRefreshFailures)) == 1
	}, 2*time.Second, 5*time.Millisecond, "a failed Watch must emit a refresh-failure metric")
}

// A normal Close must NOT be reported as a watcher death.
func TestCredentialRefresher_NormalClose_NoFailureMetric(t *testing.T) {
	t.Parallel()

	rec := &ports.RecordingExporter{}
	push := &inlinePushStore{out: make(chan *connectivity.CredentialSet, 1)}
	r := NewCredentialRefresher(push, nil, WithRefresherMetrics(rec))
	r.Watch(context.Background(), "file://creds", &credAwareFakeSession{fakeSession: &fakeSession{}})

	r.Close() // cancels parent; the watcher channel closes as part of shutdown.

	assert.Empty(t, rec.FindEntries(shared.MetricCredentialRefreshFailures),
		"a clean Close must not be mistaken for a dead watcher")
}

// ===========================================================================
// Finding #3 (supervisor.go:690) — a failed OLD-runtime stop must abort the
// swap; the NEW runtime must not start (no double-run).
// ===========================================================================

// closeFailExclusiveFactory builds exclusive-identity sessions whose Close
// fails, so the old runtime's Stop returns an error during a swap.
func closeFailExclusiveFactory() *exclusiveTransportFactory {
	ef := &exclusiveTransportFactory{}
	ef.SessionFn = func(_ context.Context, _ ports.SessionSpec) (ports.Session, error) {
		return &failingSession{closeErr: errors.New("broker close hung")}, nil
	}
	return ef
}

func TestSupervisor_OldStopFails_AbortsSwap_PrepareCommit(t *testing.T) {
	onSwap, swaps := swapChan(1)
	s := NewSupervisor(
		WithSupervisorBlueprintValidator(config.Validate),
		WithOnSwap(onSwap),
	)
	s.RegisterTransport("fake", &fakeTransportFactory{})
	s.RegisterTransport("exclusive", closeFailExclusiveFactory())
	s.RegisterStoreFactory("memory", &fakeStoreFactory{})

	ch := make(chan *ports.BridgeConfig, 1)
	cancel, _ := quickSupervisorRun(s, supervisorTestConfigWithSession("r1", "s1"), ch)
	defer cancel()
	oldRt := s.Runtime()
	require.NotNil(t, oldRt)

	require.True(t, sendConfig(ch, supervisorTestConfigWithSession("r2", "s1"), time.Second))
	ev := awaitSwap(t, swaps)
	require.Error(t, ev.Error)
	assert.Contains(t, ev.Error.Error(), "stop old runtime",
		"a failed old-runtime stop must fail the reload instead of starting the new runtime")
	assert.Same(t, oldRt, s.Runtime(), "the new runtime must NOT have been swapped in")
}

func TestSupervisor_OldStopFails_AbortsSwap_Overlap(t *testing.T) {
	onSwap, swaps := swapChan(1)
	s := NewSupervisor(
		WithSupervisorBlueprintValidator(config.Validate),
		WithOnSwap(onSwap),
		WithSwapMode(SwapOverlap),
	)
	s.RegisterTransport("fake", &fakeTransportFactory{})
	s.RegisterTransport("exclusive", closeFailExclusiveFactory())
	s.RegisterStoreFactory("memory", &fakeStoreFactory{})

	ch := make(chan *ports.BridgeConfig, 1)
	cancel, _ := quickSupervisorRun(s, supervisorTestConfigWithSession("r1", "s1"), ch)
	defer cancel()
	oldRt := s.Runtime()
	require.NotNil(t, oldRt)

	require.True(t, sendConfig(ch, supervisorTestConfigWithSession("r2", "s1"), time.Second))
	ev := awaitSwap(t, swaps)
	require.Error(t, ev.Error)
	assert.Contains(t, ev.Error.Error(), "stop old runtime")
	assert.Same(t, oldRt, s.Runtime(), "overlap swap must not start the new runtime when old stop failed")
}

// ===========================================================================
// Finding #4 (builder_prepare.go:229) — a later store-creation failure must
// close the stores already opened.
// ===========================================================================

type trackingClosableLeaseStore struct {
	fakeLeaseStore
	closed atomic.Int32
}

func (s *trackingClosableLeaseStore) Close() error {
	s.closed.Add(1)
	return nil
}

type leaseOKOutboxFailFactory struct {
	lease     *trackingClosableLeaseStore
	outboxErr error
}

func (f *leaseOKOutboxFailFactory) NewLeaseStore(_ context.Context, _ ports.PluginConfig) (ports.LeaseStore, error) {
	return f.lease, nil
}

func (f *leaseOKOutboxFailFactory) NewOutboxStore(_ context.Context, _ ports.PluginConfig, _ ports.OutboxRuntimeOptions) (ports.OutboxStore, error) {
	return nil, f.outboxErr
}

func (f *leaseOKOutboxFailFactory) NewDLQStore(_ context.Context, _ ports.PluginConfig) (ports.DLQStore, error) {
	return nil, nil
}

func TestBuildStores_LaterFailure_ClosesEarlierStores(t *testing.T) {
	t.Parallel()

	lease := &trackingClosableLeaseStore{}
	factory := &leaseOKOutboxFailFactory{lease: lease, outboxErr: errors.New("outbox backend down")}

	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b"},
		Stores: ports.StoresConfig{
			Lease:  &ports.StoreConfig{Type: "memory"},
			Outbox: &ports.StoreConfig{Type: "memory"},
		},
	}

	_, err := NewBuilder(cfg).
		RegisterStoreFactory("memory", factory).
		Build(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outbox backend down")
	assert.Equal(t, int32(1), lease.closed.Load(),
		"the earlier lease store must be closed when a later store creation fails")
}

// ===========================================================================
// Finding #5 (reconfig_strategy.go:92) — the Go 1.23+ timer drain removal must
// keep debounce/window emissions correct across reset-after-fire and cancel
// cleanly (no hang from the removed unsafe drain).
// ===========================================================================

func TestDebouncedStrategy_ResetAfterFire_NoDeadlock(t *testing.T) {
	const quiet = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	fake := clocktest.New()
	// Unbuffered: `in <- cfg` completes only once the strategy goroutine has
	// executed its receive, which is strictly AFTER it created and Stop()'d its
	// initial timer. That happens-before barrier means the subsequent
	// TimerCount()==1 can only observe the Reset-armed timer, never the transient
	// initial one — otherwise Advance could fire the pre-Stop timer before "a" is
	// consumed and the debounced emit would be lost (flaky under parallel load).
	in := make(chan *ports.BridgeConfig)
	out := NewDebouncedStrategy(quiet, fake).Filter(ctx, in)

	// Batch 1: arm -> fire -> emit.
	in <- stratCfg("a")
	require.Eventually(t, func() bool { return fake.TimerCount() == 1 }, time.Second, 2*time.Millisecond)
	fake.Advance(quiet)
	assert.Equal(t, "a", mustRecv(t, out, time.Second).Bridge.ID)

	// Batch 2: Reset AFTER a previous fire — the exact path the old
	// `if !t.Stop() { <-t.C }` drain could block on under Go 1.23+ semantics.
	in <- stratCfg("b")
	require.Eventually(t, func() bool { return fake.TimerCount() == 1 }, time.Second, 2*time.Millisecond)
	fake.Advance(quiet)
	assert.Equal(t, "b", mustRecv(t, out, time.Second).Bridge.ID)

	// Cancel while idle: must close cleanly, not hang.
	cancel()
	mustClose(t, out, time.Second)
}

func TestWindowedStrategy_ResetAfterFire_NoDeadlock(t *testing.T) {
	const quiet = 50 * time.Millisecond
	const maxDelay = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	fake := clocktest.New()
	// Unbuffered: the send is a happens-before barrier past the goroutine's
	// initial NewTimer()+Stop() startup, so TimerCount()==2 below can only be the
	// Reset-armed quiet+max pair, never the transient pre-Stop timers.
	in := make(chan *ports.BridgeConfig)
	out := NewWindowedStrategy(quiet, maxDelay, fake).Filter(ctx, in)

	// First batch arms both quiet + max timers, then the quiet window fires.
	in <- stratCfg("a")
	require.Eventually(t, func() bool { return fake.TimerCount() == 2 }, time.Second, 2*time.Millisecond)
	fake.Advance(quiet)
	assert.Equal(t, "a", mustRecv(t, out, time.Second).Bridge.ID)

	// Second batch after fire must re-arm and emit again (reset-after-fire).
	in <- stratCfg("b")
	require.Eventually(t, func() bool { return fake.TimerCount() == 2 }, time.Second, 2*time.Millisecond)
	fake.Advance(quiet)
	assert.Equal(t, "b", mustRecv(t, out, time.Second).Bridge.ID)

	cancel()
	mustClose(t, out, time.Second)
}

// ===========================================================================
// Finding #6 (supervisor.go:681) — a hot reload that changes a durable store's
// identity (type/path) must be refused so its backlog is not stranded.
// ===========================================================================

// pathIdentifiedStoreConfig is a store PluginConfig that exposes an explicit
// durable-location identity, so tuning-only changes are distinguishable from a
// path change.
type pathIdentifiedStoreConfig struct {
	path string
	tune string
}

func (c *pathIdentifiedStoreConfig) Kind() string            { return "sqlite" }
func (c *pathIdentifiedStoreConfig) Validate() error         { return nil }
func (c *pathIdentifiedStoreConfig) StorageIdentity() string { return c.path }

func TestStoreIdentityChanged_Unit(t *testing.T) {
	t.Parallel()

	mk := func(role string, sc *ports.StoreConfig) *ports.BridgeConfig {
		c := &ports.BridgeConfig{}
		switch role {
		case "outbox":
			c.Stores.Outbox = sc
		case "lease":
			c.Stores.Lease = sc
		}
		return c
	}

	t.Run("type change refused", func(t *testing.T) {
		old := mk("outbox", &ports.StoreConfig{Type: "sqlite"})
		nw := mk("outbox", &ports.StoreConfig{Type: "memory"})
		err := storeIdentityChanged(old, nw)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "outbox store type changed")
	})

	t.Run("same type same config allowed", func(t *testing.T) {
		old := mk("outbox", &ports.StoreConfig{Type: "memory"})
		nw := mk("outbox", &ports.StoreConfig{Type: "memory"})
		assert.NoError(t, storeIdentityChanged(old, nw))
	})

	t.Run("path change via StorageIdentity refused", func(t *testing.T) {
		old := mk("outbox", &ports.StoreConfig{Type: "sqlite", Config: &pathIdentifiedStoreConfig{path: "outbox.db"}})
		nw := mk("outbox", &ports.StoreConfig{Type: "sqlite", Config: &pathIdentifiedStoreConfig{path: "outbox-v2.db"}})
		require.Error(t, storeIdentityChanged(old, nw))
	})

	t.Run("tuning-only change via StorageIdentity allowed", func(t *testing.T) {
		old := mk("outbox", &ports.StoreConfig{Type: "sqlite", Config: &pathIdentifiedStoreConfig{path: "outbox.db", tune: "2m"}})
		nw := mk("outbox", &ports.StoreConfig{Type: "sqlite", Config: &pathIdentifiedStoreConfig{path: "outbox.db", tune: "5m"}})
		assert.NoError(t, storeIdentityChanged(old, nw))
	})

	t.Run("add or remove is not flagged", func(t *testing.T) {
		none := &ports.BridgeConfig{}
		added := mk("outbox", &ports.StoreConfig{Type: "sqlite"})
		assert.NoError(t, storeIdentityChanged(none, added), "adding a store strands no prior backlog")
		assert.NoError(t, storeIdentityChanged(added, none), "removal is a distinct operator action")
	})
}

func TestSupervisor_StoreTypeChange_RefusesReload(t *testing.T) {
	onSwap, swaps := swapChan(1)
	s := newTestSupervisor(WithOnSwap(onSwap))

	initial := quickCfg("r1")
	initial.Stores.Outbox = &ports.StoreConfig{Type: "memory"}

	ch := make(chan *ports.BridgeConfig, 1)
	cancel, _ := quickSupervisorRun(s, initial, ch)
	defer cancel()
	oldRt := s.Runtime()
	require.NotNil(t, oldRt)

	changed := quickCfg("r1")
	changed.Stores.Outbox = &ports.StoreConfig{Type: "sqlite"} // durable identity change
	require.True(t, sendConfig(ch, changed, time.Second))

	ev := awaitSwap(t, swaps)
	require.Error(t, ev.Error)
	assert.Contains(t, ev.Error.Error(), "store type changed")
	assert.Same(t, oldRt, s.Runtime(), "old runtime (and its durable store) must keep serving")
}
