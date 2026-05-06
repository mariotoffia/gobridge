package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/domain"
	"github.com/mariotoffia/gobridge/domain/messaging"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- fakes ---

type fakeSession struct{}

func (f *fakeSession) Start(ctx context.Context) error                              { return nil }
func (f *fakeSession) Reconcile(ctx context.Context, plan domain.SessionPlan) error { return nil }
func (f *fakeSession) Health(ctx context.Context) ports.SessionHealth               { return ports.SessionHealth{} }
func (f *fakeSession) Events() <-chan ports.SessionEvent                            { return nil }
func (f *fakeSession) Close(ctx context.Context) error                              { return nil }

type fakeReceiver struct{}

func (f *fakeReceiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	<-ctx.Done()
	return ctx.Err()
}

type fakeSender struct{}

func (f *fakeSender) Send(ctx context.Context, env *messaging.Envelope) error { return nil }

type fakeTransportFactory struct{}

func (f *fakeTransportFactory) NewSession(_ context.Context, _ ports.SessionSpec) (ports.Session, error) {
	return &fakeSession{}, nil
}
func (f *fakeTransportFactory) NewReceiver(_ context.Context, _ ports.ReceiverSpec, _ ports.Session) (ports.Receiver, error) {
	return &fakeReceiver{}, nil
}
func (f *fakeTransportFactory) NewSender(_ context.Context, _ ports.SenderSpec, _ ports.Session) (ports.Sender, error) {
	return &fakeSender{}, nil
}
func (f *fakeTransportFactory) Capabilities() []ports.Capability {
	return []ports.Capability{ports.CapVisibilityExtension}
}

type fakeLeaseStore struct{}

func (f *fakeLeaseStore) Acquire(_ context.Context, _ string, _ string, _ time.Duration, _ map[string]string) (persistence.LeaseToken, error) {
	return persistence.LeaseToken{}, nil
}
func (f *fakeLeaseStore) Renew(_ context.Context, _ string, _ persistence.LeaseToken, _ time.Duration, _ map[string]string) (persistence.LeaseToken, error) {
	return persistence.LeaseToken{}, nil
}
func (f *fakeLeaseStore) Release(_ context.Context, _ string, _ persistence.LeaseToken) error {
	return nil
}
func (f *fakeLeaseStore) Current(_ context.Context, _ string) (persistence.LeaseInfo, error) {
	return persistence.LeaseInfo{}, nil
}

type fakeOutboxStore struct{}

func (f *fakeOutboxStore) Persist(_ context.Context, _ []persistence.OutboxRecord) error { return nil }
func (f *fakeOutboxStore) Claim(_ context.Context, _ string, _ string, _ persistence.LeaseToken, _ int) ([]persistence.OutboxRecord, error) {
	return nil, nil
}
func (f *fakeOutboxStore) Complete(_ context.Context, _ []string, _ persistence.LeaseToken) error {
	return nil
}
func (f *fakeOutboxStore) Expire(_ context.Context, _ time.Time) (int, error) { return 0, nil }
func (f *fakeOutboxStore) QueryPending(_ context.Context, _ string, _ int) ([]persistence.OutboxRecord, error) {
	return nil, nil
}

type fakeStoreFactory struct{}

func (f *fakeStoreFactory) NewLeaseStore(_ context.Context, _ ports.PluginConfig) (ports.LeaseStore, error) {
	return &fakeLeaseStore{}, nil
}
func (f *fakeStoreFactory) NewOutboxStore(_ context.Context, _ ports.PluginConfig, _ ports.OutboxRuntimeOptions) (ports.OutboxStore, error) {
	return &fakeOutboxStore{}, nil
}
func (f *fakeStoreFactory) NewDLQStore(_ context.Context, _ ports.PluginConfig) (ports.DLQStore, error) {
	return nil, nil
}

type fakeCredentialStore struct {
	creds map[string]*domain.CredentialSet
}

func (f *fakeCredentialStore) Resolve(_ context.Context, uri string) (*domain.CredentialSet, error) {
	if cs, ok := f.creds[uri]; ok {
		return cs, nil
	}
	return nil, shared.ErrNotFound.WithMessage("not found: " + uri)
}

type capturingTransportFactory struct {
	fakeTransportFactory
	capturedSessionSpec ports.SessionSpec
}

func (c *capturingTransportFactory) NewSession(_ context.Context, spec ports.SessionSpec) (ports.Session, error) {
	c.capturedSessionSpec = spec
	return &fakeSession{}, nil
}

// testCredConfig is a minimal typed PluginConfig used by builder
// credential tests. It implements CredentialedConfig so the builder's
// resolveConfigCredentials path can mutate Username/Password from the
// resolved CredentialSet.
type testCredConfig struct {
	URI      string
	Username string
	Password string
}

func (c *testCredConfig) Kind() string           { return "test.cred" }
func (c *testCredConfig) Validate() error        { return nil }
func (c *testCredConfig) CredentialsURI() string { return c.URI }
func (c *testCredConfig) ApplyCredentials(creds *domain.CredentialSet) error {
	if creds != nil && creds.Password != nil {
		if c.Username == "" {
			c.Username = creds.Password.Username
		}
		if c.Password == "" {
			c.Password = creds.Password.Password
		}
	}
	c.URI = ""
	return nil
}

// --- tests ---

func testConfig() *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "test-bridge"},
		Stores: ports.StoresConfig{
			Lease:  &ports.StoreConfig{Type: "memory"},
			Outbox: &ports.StoreConfig{Type: "memory"},
		},
		Sessions: []ports.SessionDef{
			{ID: "mqtt-s1", Transport: "mqtt", SessionMode: "exclusive"},
		},
		Receivers: []ports.ReceiverDef{
			{ID: "sqs-rx", Transport: "sqs"},
		},
		Senders: []ports.SenderDef{
			{ID: "mqtt-tx", Transport: "mqtt", SessionID: "mqtt-s1"},
		},
		Bindings: []ports.BindingDef{
			{ID: "b1", SenderID: "mqtt-tx", SessionID: "mqtt-s1", Address: "topic/test"},
		},
		Routes: []ports.RouteDef{
			{
				ID:           "r1",
				ReceiverID:   "sqs-rx",
				DeliveryMode: "shared_outbox",
				Bindings:     []string{"b1"},
				Session: &ports.RouteSessionDef{
					SessionID: "mqtt-s1",
					SenderID:  "mqtt-tx",
				},
			},
		},
	}
}

// Verifies NewBuilder wires transports and stores and produces a runtime with the expected route metadata.
func TestBuilder_Build(t *testing.T) {
	cfg := testConfig()

	rt, err := NewBuilder(cfg).
		RegisterTransport("mqtt", &fakeTransportFactory{}).
		RegisterTransport("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		Build(context.Background())

	require.NoError(t, err)
	require.NotNil(t, rt)

	routes := rt.Routes()
	require.Len(t, routes, 1)
	assert.Equal(t, "r1", routes[0].ID)
	assert.Equal(t, domain.DeliverySharedOutbox, routes[0].DeliveryMode)
}

// Verifies Build fails when a configured transport has no registered factory.
func TestBuilder_MissingTransportFactory(t *testing.T) {
	cfg := testConfig()

	_, err := NewBuilder(cfg).
		RegisterTransport("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		Build(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no transport factory")
}

// Verifies Build fails when shared_outbox requires a store type with no registered factory.
func TestBuilder_MissingStoreFactory(t *testing.T) {
	cfg := testConfig()

	_, err := NewBuilder(cfg).
		RegisterTransport("mqtt", &fakeTransportFactory{}).
		RegisterTransport("sqs", &fakeTransportFactory{}).
		Build(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no store factory")
}

// Verifies Build surfaces config validation errors for an empty bridge
// configuration when the composition root has supplied a validator
// (config.Validate via WithBlueprintValidator).
func TestBuilder_InvalidConfig(t *testing.T) {
	cfg := &ports.BridgeConfig{}

	_, err := NewBuilder(cfg, WithBlueprintValidator(config.Validate)).
		Build(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config validation")
}

// Verifies Build constructs a direct_hold route without session-scoped stores when the config is valid.
func TestBuilder_DirectHoldRoute(t *testing.T) {
	cfg := &ports.BridgeConfig{
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
			},
		},
	}

	rt, err := NewBuilder(cfg).
		RegisterTransport("sqs", &fakeTransportFactory{}).
		Build(context.Background())

	require.NoError(t, err)
	require.NotNil(t, rt)

	routes := rt.Routes()
	require.Len(t, routes, 1)
	assert.Equal(t, domain.DeliveryDirectHold, routes[0].DeliveryMode)
}

// Verifies WithCredentialStore resolves credentials_uri into the typed
// session config before creating the transport session.
func TestBuilder_WithCredentialStore(t *testing.T) {
	cfg := testConfig()
	sessCfg := &testCredConfig{URI: "file://test/creds"}
	cfg.Sessions[0].Config = sessCfg

	cs := &fakeCredentialStore{
		creds: map[string]*domain.CredentialSet{
			"file://test/creds": {
				Password: &domain.PasswordCredential{
					Username: "resolved-user",
					Password: "resolved-pass",
				},
			},
		},
	}

	mqttFactory := &capturingTransportFactory{}

	rt, err := NewBuilder(cfg, WithCredentialStore(cs)).
		RegisterTransport("mqtt", mqttFactory).
		RegisterTransport("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		Build(context.Background())

	require.NoError(t, err)
	require.NotNil(t, rt)

	captured, ok := mqttFactory.capturedSessionSpec.Config.(*testCredConfig)
	require.True(t, ok, "captured session config should be *testCredConfig")
	assert.Equal(t, "resolved-user", captured.Username)
	assert.Equal(t, "resolved-pass", captured.Password)
	assert.Equal(t, "", captured.URI, "credentials_uri should be cleared after resolution")
}

// Verifies inline session option keys override resolved credential fields while still filling missing values from the store.
func TestBuilder_CredentialInlineOverride(t *testing.T) {
	cfg := testConfig()
	sessCfg := &testCredConfig{URI: "file://test/creds", Username: "inline-user"}
	cfg.Sessions[0].Config = sessCfg

	cs := &fakeCredentialStore{
		creds: map[string]*domain.CredentialSet{
			"file://test/creds": {
				Password: &domain.PasswordCredential{
					Username: "resolved-user",
					Password: "resolved-pass",
				},
			},
		},
	}

	mqttFactory := &capturingTransportFactory{}

	rt, err := NewBuilder(cfg, WithCredentialStore(cs)).
		RegisterTransport("mqtt", mqttFactory).
		RegisterTransport("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		Build(context.Background())

	require.NoError(t, err)
	require.NotNil(t, rt)

	captured, ok := mqttFactory.capturedSessionSpec.Config.(*testCredConfig)
	require.True(t, ok, "captured session config should be *testCredConfig")
	assert.Equal(t, "inline-user", captured.Username,
		"inline value should take precedence over resolved credential")
	assert.Equal(t, "resolved-pass", captured.Password,
		"password should be resolved from credential store")
}

type fakeDistributedStoreFactory struct {
	fakeStoreFactory
}

func (f *fakeDistributedStoreFactory) IsDistributed() bool { return true }

// Verifies Build rejects clustered deployment when registered store factories are not distributed.
func TestBuilder_Clustered_NonDistributedStore_Rejected(t *testing.T) {
	cfg := testConfig()
	cfg.Bridge.DeploymentMode = "clustered"

	_, err := NewBuilder(cfg).
		RegisterTransport("mqtt", &fakeTransportFactory{}).
		RegisterTransport("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		Build(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "clustered deployment requires a distributed")
}

// Verifies Build succeeds for clustered deployment when store factories report IsDistributed.
func TestBuilder_Clustered_DistributedStore_OK(t *testing.T) {
	cfg := testConfig()
	cfg.Bridge.DeploymentMode = "clustered"

	rt, err := NewBuilder(cfg).
		RegisterTransport("mqtt", &fakeTransportFactory{}).
		RegisterTransport("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("memory", &fakeDistributedStoreFactory{}).
		Build(context.Background())

	require.NoError(t, err)
	require.NotNil(t, rt)
}

// Verifies Build accepts standalone deployment with non-distributed memory stores.
func TestBuilder_Standalone_NonDistributedStore_OK(t *testing.T) {
	cfg := testConfig()
	cfg.Bridge.DeploymentMode = "standalone"

	rt, err := NewBuilder(cfg).
		RegisterTransport("mqtt", &fakeTransportFactory{}).
		RegisterTransport("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		Build(context.Background())

	require.NoError(t, err)
	require.NotNil(t, rt)
}

// nilLeaseStoreFactory simulates the SQLite bug where NewLeaseStore returns
// (nil, nil) instead of a valid store or an error.
type nilLeaseStoreFactory struct{}

func (f *nilLeaseStoreFactory) NewLeaseStore(_ context.Context, _ ports.PluginConfig) (ports.LeaseStore, error) {
	return nil, nil
}
func (f *nilLeaseStoreFactory) NewOutboxStore(_ context.Context, _ ports.PluginConfig, _ ports.OutboxRuntimeOptions) (ports.OutboxStore, error) {
	return &fakeOutboxStore{}, nil
}
func (f *nilLeaseStoreFactory) NewDLQStore(_ context.Context, _ ports.PluginConfig) (ports.DLQStore, error) {
	return nil, nil
}
func (f *nilLeaseStoreFactory) IsDistributed() bool { return true }

// Verifies Build rejects clustered deployment when the lease store factory
// returns (nil, nil), which is the SQLite adapter bug where a nil store
// silently passes validation.
func TestBuilder_ClusteredMode_NilLeaseStore_RejectsStartup(t *testing.T) {
	cfg := testConfig()
	cfg.Bridge.DeploymentMode = "clustered"
	cfg.Stores.Lease = &ports.StoreConfig{Type: "sqlite"}

	_, err := NewBuilder(cfg).
		RegisterTransport("mqtt", &fakeTransportFactory{}).
		RegisterTransport("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("sqlite", &nilLeaseStoreFactory{}).
		RegisterStoreFactory("memory", &fakeDistributedStoreFactory{}).
		Build(context.Background())

	require.Error(t, err, "Build must reject a nil lease store in clustered mode")
	assert.Contains(t, err.Error(), "lease")
}

// Verifies Build fails when session config requests credentials_uri but no credential store was provided.
func TestBuilder_CredentialsURIWithoutStore(t *testing.T) {
	cfg := testConfig()
	cfg.Sessions[0].Config = &testCredConfig{URI: "file://test/creds"}

	_, err := NewBuilder(cfg).
		RegisterTransport("mqtt", &fakeTransportFactory{}).
		RegisterTransport("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		Build(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no credential store registered")
}

// fakeVisibilityFactory wraps fakeTransportFactory and implements
// VisibilityTimeoutProvider so that the builder can wire
// SourceVisibilityTimeout into RouteConfig.
type fakeVisibilityFactory struct {
	fakeTransportFactory
	timeout time.Duration
}

func (f *fakeVisibilityFactory) VisibilityTimeout() time.Duration {
	return f.timeout
}

// TestBuilder_PolicyFieldsReachRuntime verifies that the new policy fields
// (send_timeout, depth_cache_ttl, allow_unfenced, allow_retry_drop) survive
// the full config -> builder -> runtime path and affect route behavior.
func TestBuilder_PolicyFieldsReachRuntime(t *testing.T) {
	cfg := testConfig()
	cfg.Routes[0].DeliveryMode = "direct_hold"
	cfg.Routes[0].Session = nil
	cfg.Routes[0].Policy = ports.PolicyDef{
		SendTimeout:    "5s",
		DepthCacheTTL:  "100ms",
		AllowUnfenced:  true,
		AllowRetryDrop: true,
	}

	rt, err := NewBuilder(cfg).
		RegisterTransport("mqtt", &fakeTransportFactory{}).
		RegisterTransport("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		Build(context.Background())

	require.NoError(t, err)
	require.NotNil(t, rt)

	routes := rt.Routes()
	require.Len(t, routes, 1)
	assert.Equal(t, 5*time.Second, routes[0].Policy.SendTimeout)
	assert.Equal(t, 100*time.Millisecond, routes[0].Policy.DepthCacheTTL)
	assert.True(t, routes[0].Policy.AllowUnfenced)
	assert.True(t, routes[0].Policy.AllowRetryDrop)
}

// TestBuilder_DrainMaxFieldsReachSessionConfig verifies that
// drain_max_batch_size and drain_max_concurrency from config survive
// the builder path and are available on the resulting runtime.
func TestBuilder_DrainMaxFieldsReachSessionConfig(t *testing.T) {
	cfg := testConfig()
	cfg.Routes[0].Session.DrainMaxBatchSize = 200
	cfg.Routes[0].Session.DrainMaxConcurrency = 5

	rt, err := NewBuilder(cfg).
		RegisterTransport("mqtt", &fakeTransportFactory{}).
		RegisterTransport("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		Build(context.Background())

	require.NoError(t, err)
	require.NotNil(t, rt)
}

// TestBuilder_WiresSourceVisibilityTimeout verifies that a transport
// factory implementing VisibilityTimeoutProvider causes the builder to
// populate SourceVisibilityTimeout on the resulting route. When
// SendTimeout >= VisibilityTimeout/2, the runtime validator rejects the
// route at Start() time.
func TestBuilder_WiresSourceVisibilityTimeout(t *testing.T) {
	cfg := testConfig()
	cfg.Routes[0].DeliveryMode = "direct_hold"
	cfg.Routes[0].Session = nil
	cfg.Routes[0].Policy.SendTimeout = "20s"

	sqsFactory := &fakeVisibilityFactory{timeout: 30 * time.Second}

	rt, err := NewBuilder(cfg).
		RegisterTransport("mqtt", &fakeTransportFactory{}).
		RegisterTransport("sqs", sqsFactory).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		Build(context.Background())

	require.NoError(t, err)
	require.NotNil(t, rt)

	err = rt.Start(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SendTimeout")
	assert.Contains(t, err.Error(), "VisibilityTimeout")
}
