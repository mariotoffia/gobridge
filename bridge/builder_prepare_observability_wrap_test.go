package bridge

import (
	"context"
	"io"
	"testing"

	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"

	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Finding 15 / — buildStores must wrap lease and outbox stores with the
// runtime's metric decorators when an exporter is configured, WITHOUT masking
// the inner stores' optional capabilities (io.Closer on both, OutboxReleaser
// on the outbox). A bare decorator here silently degrades the drainer's
// fast-release path to stale reclaim and leaks durable store handles on every
// reconfiguration — see builder_prepare.go buildStores and instrumented_store.go.
// ---------------------------------------------------------------------------

// closableFakeLeaseStore is a lease store with the optional io.Closer
// capability, mirroring durable (file-backed) lease stores.
type closableFakeLeaseStore struct {
	fakeLeaseStore
	closed bool
}

func (s *closableFakeLeaseStore) Close() error {
	s.closed = true
	return nil
}

// capabilityFakeOutboxStore is an outbox store with BOTH optional
// capabilities: io.Closer and ports.OutboxReleaser.
type capabilityFakeOutboxStore struct {
	fakeOutboxStore
	closed   bool
	released [][]string
}

func (s *capabilityFakeOutboxStore) Close() error {
	s.closed = true
	return nil
}

func (s *capabilityFakeOutboxStore) Release(_ context.Context, recordIDs []string, _ persistence.LeaseToken) error {
	s.released = append(s.released, recordIDs)
	return nil
}

var (
	_ io.Closer            = (*capabilityFakeOutboxStore)(nil)
	_ ports.OutboxReleaser = (*capabilityFakeOutboxStore)(nil)
	_ io.Closer            = (*closableFakeLeaseStore)(nil)
)

// capabilityStoreFactory hands out the capability-bearing fakes above.
type capabilityStoreFactory struct {
	lease  *closableFakeLeaseStore
	outbox *capabilityFakeOutboxStore
}

func (f *capabilityStoreFactory) NewLeaseStore(_ context.Context, _ ports.PluginConfig) (ports.LeaseStore, error) {
	return f.lease, nil
}

func (f *capabilityStoreFactory) NewOutboxStore(_ context.Context, _ ports.PluginConfig, _ ports.OutboxRuntimeOptions) (ports.OutboxStore, error) {
	return f.outbox, nil
}

func (f *capabilityStoreFactory) NewDLQStore(_ context.Context, _ ports.PluginConfig) (ports.DLQStore, error) {
	return nil, nil
}

func capabilityStoresConfig() (*ports.BridgeConfig, *capabilityStoreFactory) {
	cfg := directHoldConfig()
	cfg.Stores = ports.StoresConfig{
		Lease:  &ports.StoreConfig{Type: "capfake"},
		Outbox: &ports.StoreConfig{Type: "capfake"},
	}
	return cfg, &capabilityStoreFactory{
		lease:  &closableFakeLeaseStore{},
		outbox: &capabilityFakeOutboxStore{},
	}
}

// TestBuildStores_WithMetrics_WrapsStoresPreservingCapabilities validates the
// Finding 15 composition wiring: with WithMetrics configured, buildStores
// returns instrumented stores whose operations emit through the exporter,
// while the inner stores' optional io.Closer / ports.OutboxReleaser
// capabilities remain reachable through the decorators.
func TestBuildStores_WithMetrics_WrapsStoresPreservingCapabilities(t *testing.T) {
	ctx := context.Background()
	rec := &ports.RecordingExporter{}
	cfg, factory := capabilityStoresConfig()

	b := NewBuilder(cfg, WithMetrics(rec))
	b.RegisterStoreFactory("capfake", factory)

	res, err := b.buildStores(ctx)
	require.NoError(t, err)

	// Lease store is wrapped with the closable decorator, not returned bare.
	_, wrapped := res.lease.(instrumentedClosableLeaseStore)
	require.True(t, wrapped, "lease store must be wrapped in instrumentedClosableLeaseStore, got %T", res.lease)

	// Lease metrics flow through the decorator.
	_, err = res.lease.Acquire(ctx, "lease-1", "owner-1", 0, nil)
	require.NoError(t, err)
	require.NotEmpty(t, rec.FindEntries(shared.MetricLeaseAcquireLatency),
		"instrumented lease store must emit LeaseAcquireLatency")

	// Close still reaches the inner durable store (no handle leak on swap).
	leaseCloser, ok := res.lease.(io.Closer)
	require.True(t, ok, "wrapped lease store must re-export io.Closer")
	require.NoError(t, leaseCloser.Close())
	require.True(t, factory.lease.closed, "Close must forward to the inner lease store")

	// Outbox metrics flow through the decorator.
	_, err = res.outbox.QueryPending(ctx, "p1", 10)
	require.NoError(t, err)
	require.NotEmpty(t, rec.FindEntries(shared.MetricOutboxDepth),
		"instrumented outbox store must emit OutboxDepth on QueryPending")

	// Optional OutboxReleaser survives the wrap: the drainer's fast-release
	// probe must still succeed on the decorated store.
	releaser, ok := res.outbox.(ports.OutboxReleaser)
	require.True(t, ok, "wrapped outbox store must re-export ports.OutboxReleaser")
	require.NoError(t, releaser.Release(ctx, []string{"r1"}, persistence.LeaseToken{}))
	require.Len(t, factory.outbox.released, 1, "Release must forward to the inner outbox store")

	// Optional io.Closer survives the wrap.
	outboxCloser, ok := res.outbox.(io.Closer)
	require.True(t, ok, "wrapped outbox store must re-export io.Closer")
	require.NoError(t, outboxCloser.Close())
	require.True(t, factory.outbox.closed, "Close must forward to the inner outbox store")
}

// TestBuildStores_NoMetrics_LeavesStoresUnwrapped validates the absence path:
// without a configured exporter the factory-built stores are used as-is (no
// decorator, no behavioural change for deployments without metrics).
func TestBuildStores_NoMetrics_LeavesStoresUnwrapped(t *testing.T) {
	ctx := context.Background()
	cfg, factory := capabilityStoresConfig()

	b := NewBuilder(cfg)
	b.RegisterStoreFactory("capfake", factory)

	res, err := b.buildStores(ctx)
	require.NoError(t, err)
	require.Same(t, ports.LeaseStore(factory.lease), res.lease, "no exporter => lease store unwrapped")
	require.Same(t, ports.OutboxStore(factory.outbox), res.outbox, "no exporter => outbox store unwrapped")
}
