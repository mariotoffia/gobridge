package bridge

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/ports"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// durabilityStoreFactory is a fakeStoreFactory that declares a crash-durability
// posture, so a test can compose any lease/outbox/DLQ durability pairing the
// composition guard has to judge.
type durabilityStoreFactory struct {
	fakeStoreFactory
	crashDurable bool
}

func (f *durabilityStoreFactory) IsCrashDurable() bool { return f.crashDurable }

// durabilityDLQFactory additionally returns a real (non-nil) DLQ store, which
// the bare fakeStoreFactory does not, so DLQ-posture assertions have a store to
// judge.
type durabilityDLQFactory struct {
	durabilityStoreFactory
}

func (f *durabilityDLQFactory) NewDLQStore(_ context.Context, _ ports.PluginConfig) (ports.DLQStore, error) {
	return stubDLQStore{}, nil
}

var (
	_ ports.StoreFactory             = (*durabilityStoreFactory)(nil)
	_ ports.CrashDurableStoreFactory = (*durabilityStoreFactory)(nil)
	_ ports.StoreFactory             = (*durabilityDLQFactory)(nil)
	_ ports.CrashDurableStoreFactory = (*durabilityDLQFactory)(nil)
)

// durabilityConfig returns testConfig with the lease and outbox stores on
// separately named types, so each half of the pairing can be given its own
// durability posture.
func durabilityConfig() *ports.BridgeConfig {
	cfg := testConfig()
	cfg.Stores.Lease = &ports.StoreConfig{Type: "lease-store"}
	cfg.Stores.Outbox = &ports.StoreConfig{Type: "outbox-store"}
	return cfg
}

// buildDurabilityPair prepares durabilityConfig with the given lease/outbox
// durability postures and returns the prepare error plus whatever the builder
// logged at WARN.
func buildDurabilityPair(t *testing.T, leaseDurable, outboxDurable bool) (string, error) {
	t.Helper()

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))

	_, err := NewBuilder(durabilityConfig(), WithLogger(logger)).
		RegisterTransportFactory("mqtt", &fakeTransportFactory{}).
		RegisterTransportFactory("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("lease-store", &durabilityStoreFactory{crashDurable: leaseDurable}).
		RegisterStoreFactory("outbox-store", &durabilityStoreFactory{crashDurable: outboxDurable}).
		prepare(context.Background())

	return logs.String(), err
}

// Verifies a process-volatile lease store is rejected under a crash-durable
// outbox. The outbox keeps a durable per-partition fencing high-water-mark
// while a volatile lease renumbers its fencing versions from scratch on every
// restart, so the successor claims below the mark, is fenced out, and the
// partition never drains again while ingress keeps acknowledging into it.
func TestBuilder_StoreDurability_VolatileLeaseUnderDurableOutboxRejected(t *testing.T) {
	_, err := buildDurabilityPair(t, false, true)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "crash-durable OutboxStore")
	assert.Contains(t, err.Error(), "fencing")
}

// Verifies the accepted pairings build: both halves durable (the production
// posture) and both halves volatile (the acknowledged single-process posture)
// preserve monotonic fencing, because the fencing counter and the fence it is
// compared against are lost or kept together.
func TestBuilder_StoreDurability_MatchedPairingsAccepted(t *testing.T) {
	t.Run("durable lease and durable outbox", func(t *testing.T) {
		_, err := buildDurabilityPair(t, true, true)
		require.NoError(t, err)
	})

	t.Run("volatile lease and volatile outbox", func(t *testing.T) {
		_, err := buildDurabilityPair(t, false, false)
		require.NoError(t, err)
	})
}

// Verifies a durable lease over a volatile outbox is accepted (fencing stays
// monotonic — the durable side is the counter, not the fence) but warns, naming
// the routes whose accepted work is lost on restart.
func TestBuilder_StoreDurability_VolatileOutboxWarnsNamingRoutes(t *testing.T) {
	logs, err := buildDurabilityPair(t, true, false)

	require.NoError(t, err)
	assert.Contains(t, logs, "VOLATILE")
	assert.Contains(t, logs, "r1", "the warning must name the routes riding on the volatile outbox")
}

// Verifies a volatile DLQ warns and names the routes whose terminal evidence it
// holds: those routing permanent failures or expiry to the dead-letter queue.
func TestBuilder_StoreDurability_VolatileDLQWarnsNamingRoutes(t *testing.T) {
	cfg := durabilityConfig()
	cfg.Stores.DLQ = &ports.StoreConfig{Type: "dlq-store"}
	cfg.Routes[0].Policy = ports.PolicyDef{OnPermanentFailure: "dlq", OnExpired: "drop"}

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))

	_, err := NewBuilder(cfg, WithLogger(logger)).
		RegisterTransportFactory("mqtt", &fakeTransportFactory{}).
		RegisterTransportFactory("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("lease-store", &durabilityStoreFactory{crashDurable: true}).
		RegisterStoreFactory("outbox-store", &durabilityStoreFactory{crashDurable: true}).
		RegisterStoreFactory("dlq-store", &durabilityDLQFactory{}).
		prepare(context.Background())

	require.NoError(t, err)
	assert.Contains(t, logs.String(), "VOLATILE")
	assert.Contains(t, logs.String(), "r1", "the warning must name the routes whose DLQ evidence is volatile")
}

// Verifies a fully durable composition stays silent: the warning is a real
// signal, not noise emitted on every start.
func TestBuilder_StoreDurability_DurableCompositionDoesNotWarn(t *testing.T) {
	logs, _ := buildDurabilityPair(t, true, true)

	assert.NotContains(t, logs, "VOLATILE")
}

// Verifies a factory that declares no durability capability is treated as
// volatile. The guard must fail closed on an unknown store: assuming durability
// would let exactly the wedging pairing through unchecked.
func TestBuilder_StoreDurability_UndeclaredFactoryTreatedAsVolatile(t *testing.T) {
	cfg := durabilityConfig()

	_, err := NewBuilder(cfg).
		RegisterTransportFactory("mqtt", &fakeTransportFactory{}).
		RegisterTransportFactory("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("lease-store", &fakeStoreFactory{}).
		RegisterStoreFactory("outbox-store", &durabilityStoreFactory{crashDurable: true}).
		prepare(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "crash-durable OutboxStore")
}

// Verifies a large blueprint's warning stays readable: it names a bounded
// number of routes and reports the remainder as a count, rather than emitting a
// multi-kilobyte log line an operator will never read.
func TestBuilder_StoreDurability_WarningCapsNamedRoutes(t *testing.T) {
	ids := make([]string, 0, maxNamedRoutes+5)
	for i := range maxNamedRoutes + 5 {
		ids = append(ids, fmt.Sprintf("r%02d", i))
	}

	rendered := namedRoutes(ids)

	assert.Contains(t, rendered, ids[maxNamedRoutes-1], "the capped prefix must be named in full")
	assert.NotContains(t, rendered, ids[maxNamedRoutes], "routes past the cap must not be named")
	assert.Contains(t, rendered, "(+5 more)", "the remainder must be reported as a count")
	assert.Equal(t, strings.Join(ids[:maxNamedRoutes], ","), namedRoutes(ids[:maxNamedRoutes]), "a list within the cap is named in full")
}

// Verifies the rejection is scoped to blueprints that actually drain the
// outbox. A fencing token only ever reaches the store from a shared_outbox
// route's drainer, so a durable outbox that no route drains cannot wedge and
// must not block startup.
func TestBuilder_StoreDurability_VolatileLeaseAllowedWhenNoRouteDrainsOutbox(t *testing.T) {
	cfg := durabilityConfig()
	cfg.Routes[0].DeliveryMode = "direct_hold"
	cfg.Routes[0].Session = nil
	cfg.Sessions[0].SessionMode = ""

	_, err := NewBuilder(cfg).
		RegisterTransportFactory("mqtt", &fakeTransportFactory{}).
		RegisterTransportFactory("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("lease-store", &durabilityStoreFactory{crashDurable: false}).
		RegisterStoreFactory("outbox-store", &durabilityStoreFactory{crashDurable: true}).
		prepare(context.Background())

	require.NoError(t, err)
}

// Verifies the rejection names the routes whose drain would wedge, so the
// operator can see which work is at stake without diffing the blueprint.
func TestBuilder_StoreDurability_RejectionNamesDrainingRoutes(t *testing.T) {
	_, err := buildDurabilityPair(t, false, true)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "r1", "the rejection must name the outbox-draining routes")
	assert.Contains(t, err.Error(), "ABANDONS",
		"the remediation must warn that downgrading the outbox discards its durable backlog")
}

// durabilityReloadConfig is twoSessionReloadConfig's shape reduced to one
// exclusive shared_outbox route, on separately named lease and outbox store
// types so each half of the pairing carries its own durability posture across a
// reload. address changes the binding, which is the accepted delta that forces
// the supervisor to reconstruct every store.
func durabilityReloadConfig(version int, leaseType, outboxType, address string) *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Version: version,
		Bridge:  ports.BridgeSettings{ID: "test-bridge", DrainTimeout: "1s"},
		Stores: ports.StoresConfig{
			Lease:  &ports.StoreConfig{Type: leaseType},
			Outbox: &ports.StoreConfig{Type: outboxType},
		},
		Sessions:  []ports.SessionDef{{ID: "s1", Transport: "exclusive", SessionMode: "exclusive"}},
		Receivers: []ports.ReceiverDef{{ID: "r1-rx", Transport: "fake"}},
		Senders:   []ports.SenderDef{{ID: "r1-tx", Transport: "exclusive", SessionID: "s1"}},
		Bindings:  []ports.BindingDef{{ID: "r1-b1", SenderID: "r1-tx", SessionID: "s1", Address: address}},
		Routes: []ports.RouteDef{{
			ID:           "r1",
			ReceiverID:   "r1-rx",
			DeliveryMode: "shared_outbox",
			Policy:       ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop"},
			Bindings:     []string{"r1-b1"},
			Session:      &ports.RouteSessionDef{SessionID: "s1", SenderID: "r1-tx"},
		}},
	}
}

// newDurabilitySupervisor builds a supervisor whose lease and outbox store
// types carry the given crash-durability postures, plus a third "volatile-lease"
// type a reload can switch the lease to.
func newDurabilitySupervisor(leaseDurable, outboxDurable bool, opts ...SupervisorOption) *Supervisor {
	opts = append([]SupervisorOption{WithSupervisorBlueprintValidator(config.Validate)}, opts...)
	s := NewSupervisor(opts...)
	s.RegisterTransport("fake", &fakeTransportFactory{})
	s.RegisterTransport("exclusive", &exclusiveTransportFactory{})
	s.RegisterStoreFactory("lease-store", &durabilityStoreFactory{crashDurable: leaseDurable})
	s.RegisterStoreFactory("outbox-store", &durabilityStoreFactory{crashDurable: outboxDurable})
	s.RegisterStoreFactory("volatile-lease", &durabilityStoreFactory{crashDurable: false})
	return s
}

// Verifies every accepted pairing survives a reload. A reload reconstructs each
// store from its factory, so the composition guard runs again on the new
// generation — a guard that mis-read the rebuilt stores would turn a routine
// config edit into a failed swap that strands the fleet on the old runtime.
func TestSupervisorReload_StoreDurability_AcceptedPairingsSwap(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		leaseDurable, outboxDurable bool
	}{
		{"durable lease and durable outbox", true, true},
		{"volatile lease and volatile outbox", false, false},
		{"durable lease and volatile outbox", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			onSwap, swaps := swapChan(1)
			s := newDurabilitySupervisor(tc.leaseDurable, tc.outboxDurable, WithOnSwap(onSwap))
			ch := make(chan *ports.BridgeConfig, 1)
			cancel, errCh := quickSupervisorRun(s,
				durabilityReloadConfig(1, "lease-store", "outbox-store", "topic/r1"), ch)
			defer func() { cancel(); <-errCh }()

			oldRt := s.Runtime()
			require.NotNil(t, oldRt, "the accepted pairing must start")

			require.True(t, sendConfig(ch,
				durabilityReloadConfig(2, "lease-store", "outbox-store", "topic/r1-moved"), time.Second))
			ev := awaitSwap(t, swaps)

			require.NoError(t, ev.Error, "an accepted pairing must survive store reconstruction")
			assert.Equal(t, 2, s.Config().Version)
		})
	}
}

// Verifies the wedging pairing cannot be reached from a running system, and
// that two independent guards say so.
//
// A live reload never even reaches the durability guard: repointing the lease
// store is already refused by the backing-store guard, because swapping a
// store type would strand whatever the old one holds. So the durability guard
// is the COLD-START gate — a blueprint that pairs a volatile lease with a
// durable outbox refuses to boot at all, which is why no running generation can
// ever be reloaded into the wedge. Both are asserted here so a future change
// that relaxes either one has to confront the other.
func TestSupervisorReload_StoreDurability_WedgingPairingUnreachable(t *testing.T) {
	t.Run("cold start refuses the pairing outright", func(t *testing.T) {
		s := newDurabilitySupervisor(true, true)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		err := s.Run(ctx,
			durabilityReloadConfig(1, "volatile-lease", "outbox-store", "topic/r1"),
			make(chan *ports.BridgeConfig))

		require.Error(t, err, "a volatile lease under a durable drained outbox must not boot")
		assert.Contains(t, err.Error(), "crash-durable OutboxStore")
		assert.Nil(t, s.Runtime(), "no runtime is published when the initial build is rejected")
	})

	t.Run("live reload refuses the lease repoint first", func(t *testing.T) {
		onSwap, swaps := swapChan(1)
		s := newDurabilitySupervisor(true, true, WithOnSwap(onSwap))
		ch := make(chan *ports.BridgeConfig, 1)
		cancel, errCh := quickSupervisorRun(s,
			durabilityReloadConfig(1, "lease-store", "outbox-store", "topic/r1"), ch)
		defer func() { cancel(); <-errCh }()

		oldRt := s.Runtime()
		require.NotNil(t, oldRt)

		require.True(t, sendConfig(ch,
			durabilityReloadConfig(2, "volatile-lease", "outbox-store", "topic/r1"), time.Second))
		ev := awaitSwap(t, swaps)

		require.Error(t, ev.Error, "a reload into the wedging pairing must not commit")
		assert.Contains(t, ev.Error.Error(), "lease store type changed",
			"the backing-store guard rejects the repoint before stores are rebuilt")
		assert.True(t, oldRt.IsRunning(), "the working runtime is retained when the reload is rejected")
		assert.Equal(t, 1, s.Config().Version, "the rejected generation must not become current")
	})
}
