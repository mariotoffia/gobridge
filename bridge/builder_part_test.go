package bridge

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/mariotoffia/gobridge/ports"
	"github.com/mariotoffia/gobridge/runtime"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openCountingStoreFactory is a closableStoreFactory that also counts the
// stores it opens.
type openCountingStoreFactory struct {
	closableStoreFactory
	opens atomic.Int32
}

func (f *openCountingStoreFactory) NewLeaseStore(ctx context.Context, cfg ports.PluginConfig) (ports.LeaseStore, error) {
	f.opens.Add(1)
	return f.closableStoreFactory.NewLeaseStore(ctx, cfg)
}

func (f *openCountingStoreFactory) NewOutboxStore(ctx context.Context, cfg ports.PluginConfig, opts ports.OutboxRuntimeOptions) (ports.OutboxStore, error) {
	f.opens.Add(1)
	return f.closableStoreFactory.NewOutboxStore(ctx, cfg, opts)
}

var _ ports.StoreFactory = (*openCountingStoreFactory)(nil)

// partTestBuilder returns a builder over tf as the tracked transport and each
// of stores under its name.
func partTestBuilder(cfg *ports.BridgeConfig, tf ports.TransportFactory, stores map[string]ports.StoreFactory) *Builder {
	b := NewBuilder(cfg).RegisterTransportFactory("tracked", tf)
	for name, sf := range stores {
		b.RegisterStoreFactory(name, sf)
	}
	return b
}

// startPartTestHost builds and starts the runtime a part is built for. It is
// stopped when the test ends.
func startPartTestHost(t *testing.T, b *Builder) *runtime.Runtime {
	t.Helper()
	host, err := b.Build(context.Background())
	require.NoError(t, err)
	require.NoError(t, host.Start(context.Background()))
	t.Cleanup(func() { _ = host.Stop(context.Background()) })
	return host
}

func TestBuildPart_BorrowsHostStoresAndNeverClosesThem(t *testing.T) {
	sf := &openCountingStoreFactory{closableStoreFactory: closableStoreFactory{
		lease: &closableLeaseStore{}, outbox: &closableOutboxStore{},
	}}
	stores := map[string]ports.StoreFactory{"closable": sf}
	withStores := func(cfg *ports.BridgeConfig) *ports.BridgeConfig {
		cfg.Stores = ports.StoresConfig{
			Lease:  &ports.StoreConfig{Type: "closable"},
			Outbox: &ports.StoreConfig{Type: "closable"},
		}
		return cfg
	}
	tf := newPerSessionTransportFactory(false)
	host := startPartTestHost(t, partTestBuilder(withStores(applyTestConfig("a")), tf, stores))
	require.Equal(t, int32(2), sf.opens.Load())

	tf.refuseSessions("b-s", 1)
	_, err := partTestBuilder(withStores(applyTestConfig("b")), tf, stores).buildPart(context.Background(), host)
	require.ErrorIs(t, err, errSessionRefused)
	assert.Zero(t, sf.lease.closes.Load(), "a failed part build closes no store it borrowed")
	assert.Zero(t, sf.outbox.closes.Load())

	part, err := partTestBuilder(withStores(applyTestConfig("b")), tf, stores).buildPart(context.Background(), host)
	require.NoError(t, err)

	assert.Equal(t, int32(2), sf.opens.Load(), "a part opens no store")
	assert.Equal(t, host.Stores(), part.Stores(), "a part runs over the host's stores")
	require.NoError(t, part.Stop(context.Background()))
	assert.Zero(t, sf.lease.closes.Load(), "a part never closes the stores it borrowed")
	assert.Zero(t, sf.outbox.closes.Load())
	assert.Equal(t, []int{1}, tf.closeCounts("b-s"), "the part's Stop still releases its own session")
	require.NoError(t, host.Stop(context.Background()))
	assert.Equal(t, int32(1), sf.lease.closes.Load(), "the host closes its stores")
	assert.Equal(t, int32(1), sf.outbox.closes.Load())
}

// The durability rule is judged over the part's own routes: the running
// configuration drains nothing through the outbox, so it built, and the part
// is the first to add a route that does.
func TestBuildPart_RejectsSharedOutboxRouteOnVolatileLeaseWithDurableOutbox(t *testing.T) {
	stores := map[string]ports.StoreFactory{
		"lease-store":  &durabilityStoreFactory{crashDurable: false},
		"outbox-store": &durabilityStoreFactory{crashDurable: true},
	}
	withStores := func(cfg *ports.BridgeConfig) *ports.BridgeConfig {
		cfg.Stores = ports.StoresConfig{
			Lease:  &ports.StoreConfig{Type: "lease-store"},
			Outbox: &ports.StoreConfig{Type: "outbox-store"},
		}
		return cfg
	}
	tf := newPerSessionTransportFactory(false)
	host := startPartTestHost(t, partTestBuilder(withStores(applyTestConfig("a")), tf, stores))
	sub := withStores(applyTestConfig("b"))
	sub.Routes[0].DeliveryMode = "shared_outbox"

	part, err := partTestBuilder(sub, tf, stores).buildPart(context.Background(), host)

	require.ErrorContains(t, err, "crash-durable OutboxStore")
	assert.Nil(t, part)
	assert.Empty(t, tf.closeCounts("b-s"), "a refused part builds no session")
}

func TestBuildPart_RequiresManagedSubscriptionStoreWhenSubConfigNeedsIt(t *testing.T) {
	stores := map[string]ports.StoreFactory{"memory": &fakeStoreFactory{}}
	tf := newPerSessionTransportFactory(false)
	host := startPartTestHost(t, partTestBuilder(applyTestConfig("a"), tf, stores))
	// A persistent MQTT session whose receiver wants a subscription needs the
	// store that remembers its subscriptions, which the host was built without.
	sub := applyTestConfig("m")
	sub.Sessions[0].Transport = "mqtt"
	sub.Sessions[0].SessionMode = "persistent"
	sub.Receivers[0].Topics = []ports.SubscriptionDef{{Topic: "sensors/m", QoS: 1}}
	b := partTestBuilder(sub, tf, stores).RegisterTransportFactory("mqtt", newPerSessionTransportFactory(false))

	// Refused when the part is planned, before an in-place reload retires
	// anything for it.
	plan, err := b.planPart(context.Background(), host)

	require.ErrorContains(t, err, "stores.managed_subscriptions")
	assert.Nil(t, plan)
}

// A part joins a runtime that was built for the same bridge-wide sections, so
// a store the configuration names must be one the host holds.
func TestBuildPart_RefusesStoresTheHostDoesNotHold(t *testing.T) {
	stores := map[string]ports.StoreFactory{"memory": &fakeStoreFactory{}}
	tf := newPerSessionTransportFactory(false)
	host := startPartTestHost(t, partTestBuilder(applyTestConfig("a"), tf, stores))
	sub := applyTestConfig("b")
	sub.Stores.Outbox = &ports.StoreConfig{Type: "memory"}

	part, err := partTestBuilder(sub, tf, stores).buildPart(context.Background(), host)

	require.ErrorContains(t, err, "outbox store")
	assert.Nil(t, part)
	assert.Empty(t, tf.closeCounts("b-s"))
}
