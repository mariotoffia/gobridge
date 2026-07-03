package bridge

import (
	"context"
	"fmt"
	"testing"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/ports"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// directHoldConfig returns a minimal config with a direct_hold route.
func directHoldConfig() *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b1"},
		Receivers: []ports.ReceiverDef{
			{ID: "rx1", Transport: "sqs"},
		},
		Senders: []ports.SenderDef{
			{ID: "tx1", Transport: "sqs"},
		},
		Bindings: []ports.BindingDef{
			{ID: "b1", SenderID: "tx1", Address: "queue://out"},
		},
		Routes: []ports.RouteDef{
			{
				ID:           "r1",
				ReceiverID:   "rx1",
				DeliveryMode: "direct_hold",
				Bindings:     []string{"b1"},
				// drop policies keep the route valid under build-time
				// ValidateRoutes (Finding 5 / C2); the default is "dlq" which
				// needs a DLQ store this minimal config omits.
				Policy: ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop"},
			},
		},
	}
}

// buildWith creates a builder from cfg, registers fakeTransportFactory for
// each named transport, and registers fakeStoreFactory for "memory".
func buildWith(cfg *ports.BridgeConfig, transports ...string) *Builder {
	b := NewBuilder(cfg)
	for _, t := range transports {
		b.RegisterTransportFactory(t, &fakeTransportFactory{})
	}
	b.RegisterStoreFactory("memory", &fakeStoreFactory{})
	return b
}

// ---------------------------------------------------------------------------
// Equivalence: Prepare+Complete == Build
// ---------------------------------------------------------------------------

// TestBuilder_PrepareComplete_EquivalentToBuild validates that the two-phase
// Prepare+Complete path produces runtime routes identical to the single Build.
func TestBuilder_PrepareComplete_EquivalentToBuild(t *testing.T) {
	cfg := supervisorTestConfig("r1")
	ctx := context.Background()

	rtBuild, err := NewBuilder(cfg).
		RegisterTransportFactory("fake", &fakeTransportFactory{}).
		Build(ctx)
	require.NoError(t, err)

	cfg2 := supervisorTestConfig("r1")
	builder := NewBuilder(cfg2).
		RegisterTransportFactory("fake", &fakeTransportFactory{})

	prep, err := builder.prepare(ctx)
	require.NoError(t, err)

	rtPC, err := builder.complete(ctx, prep)
	require.NoError(t, err)

	buildRoutes := rtBuild.Routes()
	pcRoutes := rtPC.Routes()
	require.Len(t, pcRoutes, len(buildRoutes))

	for i := range buildRoutes {
		assert.Equal(t, buildRoutes[i].ID, pcRoutes[i].ID)
		assert.Equal(t, buildRoutes[i].DeliveryMode, pcRoutes[i].DeliveryMode)
		assert.Equal(t, buildRoutes[i].DispatchMode, pcRoutes[i].DispatchMode)
	}
}

// TestBuilder_PrepareComplete_DirectHold validates that a direct_hold route
// yields identical runtime routes whether built via Build or Prepare+Complete.
func TestBuilder_PrepareComplete_DirectHold(t *testing.T) {
	ctx := context.Background()

	rtBuild, err := buildWith(directHoldConfig(), "sqs").Build(ctx)
	require.NoError(t, err)

	cfg2 := directHoldConfig()
	builder := buildWith(cfg2, "sqs")

	prep, err := builder.prepare(ctx)
	require.NoError(t, err)

	rtPC, err := builder.complete(ctx, prep)
	require.NoError(t, err)

	buildRoutes := rtBuild.Routes()
	pcRoutes := rtPC.Routes()
	require.Len(t, pcRoutes, len(buildRoutes))
	assert.Equal(t, routing.DeliveryDirectHold, pcRoutes[0].DeliveryMode)
	assert.Equal(t, buildRoutes[0].ID, pcRoutes[0].ID)
}

// TestBuilder_PrepareComplete_SharedOutbox validates that a shared_outbox
// route with a session produces identical runtime routes via both paths.
func TestBuilder_PrepareComplete_SharedOutbox(t *testing.T) {
	ctx := context.Background()

	rtBuild, err := NewBuilder(testConfig()).
		RegisterTransportFactory("mqtt", &fakeTransportFactory{}).
		RegisterTransportFactory("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		Build(ctx)
	require.NoError(t, err)

	cfg2 := testConfig()
	builder := NewBuilder(cfg2).
		RegisterTransportFactory("mqtt", &fakeTransportFactory{}).
		RegisterTransportFactory("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("memory", &fakeStoreFactory{})

	prep, err := builder.prepare(ctx)
	require.NoError(t, err)

	rtPC, err := builder.complete(ctx, prep)
	require.NoError(t, err)

	buildRoutes := rtBuild.Routes()
	pcRoutes := rtPC.Routes()
	require.Len(t, pcRoutes, len(buildRoutes))
	assert.Equal(t, routing.DeliverySharedOutbox, pcRoutes[0].DeliveryMode)
	assert.Equal(t, buildRoutes[0].ID, pcRoutes[0].ID)
}

// ---------------------------------------------------------------------------
// Prepare phase
// ---------------------------------------------------------------------------

// TestBuilder_PrepareFailsOnInvalidConfig validates that Prepare rejects
// an empty BridgeConfig when the composition root has supplied a
// blueprint validator. The validator is injected via
// WithBlueprintValidator so the bridge package itself does not depend
// on the config parser.
func TestBuilder_PrepareFailsOnInvalidConfig(t *testing.T) {
	_, err := NewBuilder(&ports.BridgeConfig{},
		WithBlueprintValidator(config.Validate),
	).prepare(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config validation")
}

// TestBuilder_PrepareFailsOnMissingStoreFactory validates that Prepare
// fails when the config references a store type with no registered factory.
func TestBuilder_PrepareFailsOnMissingStoreFactory(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "b1"},
		Stores: ports.StoresConfig{
			Lease: &ports.StoreConfig{Type: "dynamodb"},
		},
	}

	_, err := NewBuilder(cfg).prepare(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no store factory")
	assert.Contains(t, err.Error(), "dynamodb")
}

// TestBuilder_PrepareBuildsStores validates that Prepare succeeds and
// returns a non-nil preparedBuild when all stores are valid.
func TestBuilder_PrepareBuildsStores(t *testing.T) {
	cfg := testConfig()

	prep, err := NewBuilder(cfg).
		RegisterTransportFactory("mqtt", &fakeTransportFactory{}).
		RegisterTransportFactory("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		prepare(context.Background())

	require.NoError(t, err)
	require.NotNil(t, prep)
}

// TestBuilder_PrepareDoesNotCallTransportFactory validates that Prepare
// never invokes NewSession, NewReceiver, or NewSender on the transport.
func TestBuilder_PrepareDoesNotCallTransportFactory(t *testing.T) {
	cfg := supervisorTestConfig("r1")
	ct := &countingTransportFactory{}

	prep, err := NewBuilder(cfg).
		RegisterTransportFactory("fake", ct).
		prepare(context.Background())
	require.NoError(t, err)
	require.NotNil(t, prep)

	sessions, receivers, senders := ct.Counts()

	t.Run("NewSession not called", func(t *testing.T) {
		assert.Equal(t, 0, sessions, "Prepare must not call NewSession")
	})
	t.Run("NewReceiver not called", func(t *testing.T) {
		assert.Equal(t, 0, receivers, "Prepare must not call NewReceiver")
	})
	t.Run("NewSender not called", func(t *testing.T) {
		assert.Equal(t, 0, senders, "Prepare must not call NewSender")
	})
}

// TestBuilder_Prepare_ClusteredNonDistributedStore_Rejected validates that
// clustered deployment mode is rejected when the store factory is local-only.
func TestBuilder_Prepare_ClusteredNonDistributedStore_Rejected(t *testing.T) {
	cfg := testConfig()
	cfg.Bridge.DeploymentMode = "clustered"

	_, err := NewBuilder(cfg).
		RegisterTransportFactory("mqtt", &fakeTransportFactory{}).
		RegisterTransportFactory("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		prepare(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires a distributed")
}

// TestBuilder_Prepare_ClusterEndpointsImplyClustered validates that configured
// cluster endpoints imply clustered posture even when deployment_mode is
// unset: process-local stores must be rejected, or cross-instance forwarding
// would silently break lease exclusivity (cluster finding 11).
func TestBuilder_Prepare_ClusterEndpointsImplyClustered(t *testing.T) {
	cfg := testConfig()
	cfg.Bridge.DeploymentMode = ""
	cfg.Bridge.Cluster = &ports.ClusterConfig{
		Endpoints: map[string]string{"node-2": "http://node-2:8080"},
	}

	_, err := NewBuilder(cfg).
		RegisterTransportFactory("mqtt", &fakeTransportFactory{}).
		RegisterTransportFactory("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		prepare(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires a distributed")
	assert.Contains(t, err.Error(), "cluster.endpoints")
}

// ---------------------------------------------------------------------------
// Complete phase
// ---------------------------------------------------------------------------

// TestBuilder_CompleteCreatesSessionsAndRoutes validates that Complete invokes
// NewSession, NewReceiver, and NewSender, and the resulting runtime has routes.
func TestBuilder_CompleteCreatesSessionsAndRoutes(t *testing.T) {
	cfg := supervisorTestConfig("r1")
	ctx := context.Background()
	ct := &countingTransportFactory{}

	builder := NewBuilder(cfg).RegisterTransportFactory("fake", ct)

	prep, err := builder.prepare(ctx)
	require.NoError(t, err)

	sessions, receivers, senders := ct.Counts()
	require.Equal(t, 0, sessions+receivers+senders, "Prepare must not call transport")

	rt, err := builder.complete(ctx, prep)
	require.NoError(t, err)
	require.NotNil(t, rt)

	_, receivers, senders = ct.Counts()
	assert.Greater(t, receivers, 0, "Complete must call NewReceiver")
	assert.Greater(t, senders, 0, "Complete must call NewSender")

	routes := rt.Routes()
	require.Len(t, routes, 1)
	assert.Equal(t, "r1", routes[0].ID)
}

// TestBuilder_CompleteFailsOnSessionCreationError validates that Complete
// surfaces transport session creation errors.
func TestBuilder_CompleteFailsOnSessionCreationError(t *testing.T) {
	cfg := testConfig()
	ctx := context.Background()

	ft := &failingTransportFactory{sessionErr: fmt.Errorf("connection refused")}

	builder := NewBuilder(cfg).
		RegisterTransportFactory("mqtt", ft).
		RegisterTransportFactory("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("memory", &fakeStoreFactory{})

	prep, err := builder.prepare(ctx)
	require.NoError(t, err, "Prepare should succeed — no sessions created yet")

	_, err = builder.complete(ctx, prep)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection refused")
}

// TestBuilder_CompleteNilPrepared validates that Complete returns a clear
// error when called with a nil preparedBuild.
func TestBuilder_CompleteNilPrepared(t *testing.T) {
	builder := NewBuilder(supervisorTestConfig("r1")).
		RegisterTransportFactory("fake", &fakeTransportFactory{})

	_, err := builder.complete(context.Background(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil preparedBuild")
}
