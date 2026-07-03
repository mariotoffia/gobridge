package bridge

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// ---------------------------------------------------------------------------
// Finding 2 — a built-but-never-started runtime must not leak store handles.
// complete() opens the prep stores into a runtime that, on a validation
// failure, is never returned and therefore never Stop()'d, so complete must
// release them itself.
// ---------------------------------------------------------------------------

type closableLeaseStore struct {
	fakeLeaseStore
	closes atomic.Int32
}

func (s *closableLeaseStore) Close() error { s.closes.Add(1); return nil }

type closableOutboxStore struct {
	fakeOutboxStore
	closes atomic.Int32
}

func (s *closableOutboxStore) Close() error { s.closes.Add(1); return nil }

type closableStoreFactory struct {
	lease  *closableLeaseStore
	outbox *closableOutboxStore
}

func (f *closableStoreFactory) NewLeaseStore(_ context.Context, _ ports.PluginConfig) (ports.LeaseStore, error) {
	return f.lease, nil
}

func (f *closableStoreFactory) NewOutboxStore(_ context.Context, _ ports.PluginConfig, _ ports.OutboxRuntimeOptions) (ports.OutboxStore, error) {
	return f.outbox, nil
}

func (f *closableStoreFactory) NewDLQStore(_ context.Context, _ ports.PluginConfig) (ports.DLQStore, error) {
	return nil, nil
}

// TestBuilder_CompleteFailure_ClosesPrepStores validates Finding 2: when
// complete() fails (here, route validation rejects a dlq-default route without a
// DLQ store), the lease/outbox handles opened by prepare() are Close()'d instead
// of leaked, mirroring runtime.Stop's io.Closer teardown for a runtime that is
// never Started.
func TestBuilder_CompleteFailure_ClosesPrepStores(t *testing.T) {
	lease := &closableLeaseStore{}
	outbox := &closableOutboxStore{}

	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b1"},
		Stores: ports.StoresConfig{
			Lease:  &ports.StoreConfig{Type: "closable"},
			Outbox: &ports.StoreConfig{Type: "closable"},
		},
		Receivers: []ports.ReceiverDef{{ID: "rx", Transport: "fake"}},
		Senders:   []ports.SenderDef{{ID: "tx", Transport: "fake"}},
		Bindings:  []ports.BindingDef{{ID: "b1", SenderID: "tx", Address: "queue://out"}},
		Routes: []ports.RouteDef{
			{
				ID:           "r1",
				ReceiverID:   "rx",
				DeliveryMode: "direct_hold",
				Bindings:     []string{"b1"},
				// No drop policy -> default on_*=dlq -> ValidateRoutes rejects it
				// because no DLQ store is configured, so complete() fails after
				// prepare() opened the stores.
			},
		},
	}

	b := NewBuilder(cfg).
		RegisterTransportFactory("fake", &fakeTransportFactory{}).
		RegisterStoreFactory("closable", &closableStoreFactory{lease: lease, outbox: outbox})

	prep, err := b.prepare(context.Background())
	require.NoError(t, err)

	_, err = b.complete(context.Background(), prep)
	require.Error(t, err, "complete must reject a dlq-default route with no DLQ store")

	require.Equal(t, int32(1), lease.closes.Load(), "abandoned lease store handle must be closed exactly once (Finding 2)")
	require.Equal(t, int32(1), outbox.closes.Load(), "abandoned outbox store handle must be closed exactly once (Finding 2)")
}

// ---------------------------------------------------------------------------
// Finding 3 — the supervisor must forward its credential stores to the builders
// it creates so CredentialRefresher actually binds under hot-reload.
// ---------------------------------------------------------------------------

// TestSupervisor_ForwardsPushCredentialStore validates that a push credential
// store registered on the supervisor reaches the builder it constructs, so a
// supervisor-built runtime can rotate credentials (Finding 3).
func TestSupervisor_ForwardsPushCredentialStore(t *testing.T) {
	push := &fakePushStore{}
	s := NewSupervisor(WithSupervisorPushCredentialStore(push))

	b := s.newBuilder(quickCfg("r1"))
	require.Same(t, push, b.effectivePushStore(), "push credential store must be forwarded to the builder")
}

// TestSupervisor_ForwardsPolledCredentialStore validates that a polled (pull)
// credential store registered on the supervisor is forwarded and lifted into a
// push store at build time (Finding 3).
func TestSupervisor_ForwardsPolledCredentialStore(t *testing.T) {
	pull := &fakeCredentialStore{creds: map[string]*connectivity.CredentialSet{}}
	s := NewSupervisor(WithSupervisorPolledCredentialStore(pull, ports.PollBasedWrapperConfig{PollInterval: time.Second}))

	b := s.newBuilder(quickCfg("r1"))
	require.NotNil(t, b.effectivePushStore(), "polled credential store must be forwarded and liftable to a push store")
}

// ---------------------------------------------------------------------------
// Finding 15 — observability must be forwarded from the supervisor to the
// builders/runtimes it creates so config-driven deployments do not run Noop
// everything.
// ---------------------------------------------------------------------------

// TestSupervisor_ForwardsObservability validates that the metrics exporter,
// tracer, and audit logger injected into the supervisor are forwarded to the
// builder (and thus the runtime) it creates (Finding 15).
func TestSupervisor_ForwardsObservability(t *testing.T) {
	m := &ports.NoopExporter{}
	tr := &ports.NoopTracer{}
	au := &ports.NoopAuditLogger{}

	s := NewSupervisor(
		WithSupervisorMetrics(m),
		WithSupervisorTracer(tr),
		WithSupervisorAuditLogger(au),
	)

	b := s.newBuilder(quickCfg("r1"))
	require.Same(t, m, b.metrics, "metrics exporter must be forwarded")
	require.Same(t, tr, b.tracer, "tracer must be forwarded")
	require.Same(t, au, b.auditLogger, "audit logger must be forwarded")
}

// TestSupervisor_ForwardsObservability_NilIgnored validates that the
// observability options ignore nil and leave the builder unconfigured (so the
// runtime substitutes its Noop defaults) rather than storing a typed-nil.
func TestSupervisor_ForwardsObservability_NilIgnored(t *testing.T) {
	s := NewSupervisor(
		WithSupervisorMetrics(nil),
		WithSupervisorTracer(nil),
		WithSupervisorAuditLogger(nil),
	)

	b := s.newBuilder(quickCfg("r1"))
	require.Nil(t, b.metrics)
	require.Nil(t, b.tracer)
	require.Nil(t, b.auditLogger)
}

// ---------------------------------------------------------------------------
// Finding 9 — WithDefaultDrainTimeout must actually apply when the blueprint
// does not set a drain_timeout; a config value still wins; the hard 30s remains
// the final fallback.
// ---------------------------------------------------------------------------

func TestSupervisor_DrainTimeoutFrom(t *testing.T) {
	t.Run("config drain_timeout wins over supervisor default", func(t *testing.T) {
		s := NewSupervisor(WithDefaultDrainTimeout(5 * time.Second))
		cfg := &ports.BridgeConfig{Bridge: ports.BridgeSettings{DrainTimeout: "12s"}}
		require.Equal(t, 12*time.Second, s.drainTimeoutFrom(cfg))
	})

	t.Run("supervisor default applies when config omits drain_timeout", func(t *testing.T) {
		s := NewSupervisor(WithDefaultDrainTimeout(7 * time.Second))
		require.Equal(t, 7*time.Second, s.drainTimeoutFrom(&ports.BridgeConfig{}))
	})

	t.Run("hard 30s fallback when neither is set", func(t *testing.T) {
		s := NewSupervisor()
		require.Equal(t, 30*time.Second, s.drainTimeoutFrom(&ports.BridgeConfig{}))
	})
}

// ---------------------------------------------------------------------------
// Finding 7 — the supervisor must expose a terminal state that covers the
// nil-runtime wedge (swap AND recovery both failed), so the composition-root
// backstop restarts the process instead of idling alive routing nothing.
// ---------------------------------------------------------------------------

// TestSupervisor_Terminal_WedgedNilRuntime validates the Terminal() predicate
// treats a wedged supervisor (no active runtime) as terminal (Finding 7).
func TestSupervisor_Terminal_WedgedNilRuntime(t *testing.T) {
	s := NewSupervisor()
	require.False(t, s.Terminal(), "a fresh supervisor with no runtime is not terminal")

	s.mu.Lock()
	s.wedged = true
	s.rt = nil
	s.mu.Unlock()

	require.True(t, s.Terminal(), "a wedged supervisor with no active runtime must be terminal (Finding 7)")
}

// wedgeStoreFactory returns a valid lease store for the first two builds and
// then fails, so a swap that fails at complete AND its recovery rebuild both
// fail, driving the supervisor into the wedged terminal state.
type wedgeStoreFactory struct {
	leaseCalls atomic.Int32
}

func (f *wedgeStoreFactory) NewLeaseStore(_ context.Context, _ ports.PluginConfig) (ports.LeaseStore, error) {
	if f.leaseCalls.Add(1) > 2 {
		return nil, errors.New("wedge: lease store unavailable")
	}
	return &fakeLeaseStore{}, nil
}

func (f *wedgeStoreFactory) NewOutboxStore(_ context.Context, _ ports.PluginConfig, _ ports.OutboxRuntimeOptions) (ports.OutboxStore, error) {
	return &fakeOutboxStore{}, nil
}

func (f *wedgeStoreFactory) NewDLQStore(_ context.Context, _ ports.PluginConfig) (ports.DLQStore, error) {
	return nil, nil
}

// TestSupervisor_WedgesWhenSwapAndRecoveryFail validates the end-to-end wedge
// path (Finding 7): a PrepareCommit swap fails at complete (route validation)
// AFTER the old runtime is stopped, and the recovery rebuild of the old config
// also fails (its lease store is now unavailable). The supervisor must end with
// no active runtime and report Terminal() == true.
func TestSupervisor_WedgesWhenSwapAndRecoveryFail(t *testing.T) {
	onSwap, swaps := swapChan(1)
	s := NewSupervisor(
		WithSupervisorBlueprintValidator(config.Validate),
		WithOnSwap(onSwap),
	)
	s.RegisterTransport("fake", &fakeTransportFactory{})
	s.RegisterTransport("exclusive", &exclusiveTransportFactory{})
	s.RegisterStoreFactory("memory", &wedgeStoreFactory{})

	ch := make(chan *ports.BridgeConfig, 1)
	cancel, _ := quickSupervisorRun(s, supervisorTestConfigWithSession("r1", "s1"), ch)
	defer cancel()
	require.NotNil(t, s.Runtime(), "initial runtime must be running")

	// direct_hold against an exclusive session fails complete()'s route
	// validation; the config forces PrepareCommit swap mode via the exclusive
	// session, so the old runtime is stopped before complete runs.
	bad := supervisorTestConfigWithSession("r2", "s1")
	bad.Receivers[0].Transport = "exclusive"
	bad.Routes[0].DeliveryMode = "direct_hold"
	require.True(t, sendConfig(ch, bad, time.Second))

	ev := awaitSwap(t, swaps)
	require.Error(t, ev.Error)

	require.Eventually(t, s.Terminal, 2*time.Second, 10*time.Millisecond,
		"supervisor must be terminal after both the swap and its recovery fail (Finding 7)")
	require.Nil(t, s.Runtime(), "a wedged supervisor has no active runtime")
}
