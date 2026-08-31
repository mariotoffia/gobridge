package bridge

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/mariotoffia/gobridge/config"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/domain/routing"
	"github.com/mariotoffia/gobridge/domain/shared"
	"github.com/mariotoffia/gobridge/ports"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- fakes ---

type fakeSession struct{}

func (f *fakeSession) Start(ctx context.Context) error                                    { return nil }
func (f *fakeSession) Reconcile(ctx context.Context, plan connectivity.SessionPlan) error { return nil }
func (f *fakeSession) Health(ctx context.Context) ports.SessionHealth                     { return ports.SessionHealth{} }
func (f *fakeSession) Events() <-chan ports.SessionEvent                                  { return nil }
func (f *fakeSession) Close(ctx context.Context) error                                    { return nil }

type fakeReceiver struct{}

func (f *fakeReceiver) Run(ctx context.Context, emit func(context.Context, ports.Delivery) error) error {
	<-ctx.Done()
	return ctx.Err()
}

// fakeSender records every OutboundMessage handed to Send so tests can
// assert on both the envelope and the destination Address, proving that
// the bridge builder wires the configured BindingDef.Address through to
// ports.OutboundMessage.Address without touching Envelope.Subject.
type fakeSender struct {
	mu       sync.Mutex
	captured []ports.OutboundMessage
	done     chan struct{}
}

func (f *fakeSender) Send(_ context.Context, msg ports.OutboundMessage) error {
	f.mu.Lock()
	f.captured = append(f.captured, msg)
	f.mu.Unlock()
	if f.done != nil {
		select {
		case f.done <- struct{}{}:
		default:
		}
	}
	return nil
}

func (f *fakeSender) snapshot() []ports.OutboundMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ports.OutboundMessage, len(f.captured))
	copy(out, f.captured)
	return out
}

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
func (f *fakeTransportFactory) AddressValidator() ports.AddressValidator { return nil }

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

func (f *fakeOutboxStore) Persist(_ context.Context, _ []*persistence.OutboxRecord) error { return nil }
func (f *fakeOutboxStore) Claim(_ context.Context, _ string, _ persistence.LeaseToken, _ int) ([]*persistence.OutboxRecord, error) {
	return nil, nil
}
func (f *fakeOutboxStore) Complete(_ context.Context, _ []string, _ persistence.LeaseToken) error {
	return nil
}
func (f *fakeOutboxStore) Expire(_ context.Context, _ time.Time, _ string, _ persistence.LeaseToken) (int, error) {
	return 0, nil
}
func (f *fakeOutboxStore) QueryPending(_ context.Context, _ string, _ int) ([]*persistence.OutboxRecord, error) {
	return nil, nil
}

// CountPending advertises the OPTIONAL ports.OutboxDepthReporter capability and
// reports every partition empty. The supervisor's durable-reload preflight
// refuses a reload that orphans a NON-empty outbox partition; a
// store that cannot prove its depth is treated as non-empty (fail closed). This
// fake proves empty so the many existing reload/orphaning tests that use it keep
// exercising the allow path. Tests that want to exercise the REFUSE path use a
// dedicated store reporting a positive count.
func (f *fakeOutboxStore) CountPending(_ context.Context, _ string) (int, error) {
	return 0, nil
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
	creds map[string]*connectivity.CredentialSet
}

func (f *fakeCredentialStore) Resolve(_ context.Context, uri string) (*connectivity.CredentialSet, error) {
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
	Password shared.Secret
}

func (c *testCredConfig) Kind() string           { return "test.cred" }
func (c *testCredConfig) Validate() error        { return nil }
func (c *testCredConfig) CredentialsURI() string { return c.URI }
func (c *testCredConfig) FreezePluginConfig() ports.PluginConfig {
	frozen := *c
	return &frozen
}
func (c *testCredConfig) ApplyCredentials(creds *connectivity.CredentialSet) error {
	if creds != nil && creds.Password() != nil {
		if c.Username == "" {
			c.Username = creds.Password().Username()
		}
		if c.Password.IsZero() {
			c.Password = creds.Password().Password()
		}
	}
	c.URI = ""
	return nil
}

type typedNilBuildConfig struct{}

func (*typedNilBuildConfig) Kind() string    { panic("typed nil Kind invoked") }
func (*typedNilBuildConfig) Validate() error { panic("typed nil Validate invoked") }

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
				// drop policies keep the shared testConfig route valid under the
				// build-time ValidateRoutes call (Finding 5 /): the default
				// on_permanent_failure/on_expired is "dlq", which requires a DLQ
				// store. Tests that need DLQ behaviour set it explicitly.
				Policy: ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop"},
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
		RegisterTransportFactory("mqtt", &fakeTransportFactory{}).
		RegisterTransportFactory("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		Build(context.Background())

	require.NoError(t, err)
	require.NotNil(t, rt)

	routes := rt.Routes()
	require.Len(t, routes, 1)
	assert.Equal(t, "r1", routes[0].ID)
	assert.Equal(t, routing.DeliverySharedOutbox, routes[0].DeliveryMode)
}

// Verifies Build fails when a configured transport has no registered factory.
func TestBuilder_MissingTransportFactory(t *testing.T) {
	cfg := testConfig()

	_, err := NewBuilder(cfg).
		RegisterTransportFactory("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		Build(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no transport factory")
}

// Verifies Build fails when shared_outbox requires a store type with no registered factory.
func TestBuilder_MissingStoreFactory(t *testing.T) {
	cfg := testConfig()

	_, err := NewBuilder(cfg).
		RegisterTransportFactory("mqtt", &fakeTransportFactory{}).
		RegisterTransportFactory("sqs", &fakeTransportFactory{}).
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
				Policy:       ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop"},
			},
		},
	}

	rt, err := NewBuilder(cfg).
		RegisterTransportFactory("sqs", &fakeTransportFactory{}).
		Build(context.Background())

	require.NoError(t, err)
	require.NotNil(t, rt)

	routes := rt.Routes()
	require.Len(t, routes, 1)
	assert.Equal(t, routing.DeliveryDirectHold, routes[0].DeliveryMode)
}

// TestBuilder_SharedOutboxBinding_DirectHoldPrimary_Rejected verifies the
// builder fails closed when a shared_outbox binding targets a session that is a
// direct_hold route's primary session. A direct_hold session has no outbox
// drainer, so the binding's records would persist under SESSION#<id> and never
// drain — a silent message loss. The direct_hold route is declared FIRST, which
// is the order that previously suppressed the binding's session-sender
// registration (the order-dependent landmine this validation removes).
func TestBuilder_SharedOutboxBinding_DirectHoldPrimary_Rejected(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "test-bridge"},
		Stores: ports.StoresConfig{
			Lease:  &ports.StoreConfig{Type: "memory"},
			Outbox: &ports.StoreConfig{Type: "memory"},
		},
		Sessions: []ports.SessionDef{
			{ID: "mqtt-s1", Transport: "mqtt", SessionMode: "exclusive"},
			{ID: "mqtt-s2", Transport: "mqtt", SessionMode: "exclusive"},
		},
		Receivers: []ports.ReceiverDef{
			{ID: "sqs-rx", Transport: "sqs"},
			{ID: "sqs-rx2", Transport: "sqs"},
		},
		Senders: []ports.SenderDef{
			{ID: "mqtt-tx", Transport: "mqtt", SessionID: "mqtt-s1"},
			{ID: "mqtt-tx2", Transport: "mqtt", SessionID: "mqtt-s2"},
		},
		Bindings: []ports.BindingDef{
			// Targets mqtt-s1, which is the direct_hold route's primary session.
			{ID: "b-so", SenderID: "mqtt-tx", SessionID: "mqtt-s1", Address: "topic/test"},
		},
		Routes: []ports.RouteDef{
			{
				ID:           "r-direct",
				ReceiverID:   "sqs-rx",
				DeliveryMode: "direct_hold",
				Session:      &ports.RouteSessionDef{SessionID: "mqtt-s1", SenderID: "mqtt-tx"},
			},
			{
				ID:           "r-shared",
				ReceiverID:   "sqs-rx2",
				DeliveryMode: "shared_outbox",
				Bindings:     []string{"b-so"},
				Session:      &ports.RouteSessionDef{SessionID: "mqtt-s2", SenderID: "mqtt-tx2"},
			},
		},
	}

	_, err := NewBuilder(cfg).
		RegisterTransportFactory("mqtt", &fakeTransportFactory{}).
		RegisterTransportFactory("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		Build(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "silent message loss")
	assert.Contains(t, err.Error(), "direct_hold route \"r-direct\"")
}

// TestBuilder_SharedOutboxBinding_OtherSharedOutboxPrimary_Rejected verifies the
// builder fails closed when a shared_outbox binding targets a session that is a
// DIFFERENT shared_outbox route's primary. A drainer exists, but it belongs to
// the other route and uses the other route's sender, so the binding's records
// would be delivered to the wrong destination (mis-delivery, not loss).
func TestBuilder_SharedOutboxBinding_OtherSharedOutboxPrimary_Rejected(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "test-bridge"},
		Stores: ports.StoresConfig{
			Lease:  &ports.StoreConfig{Type: "memory"},
			Outbox: &ports.StoreConfig{Type: "memory"},
		},
		Sessions: []ports.SessionDef{
			{ID: "mqtt-s1", Transport: "mqtt", SessionMode: "exclusive"},
			{ID: "mqtt-s2", Transport: "mqtt", SessionMode: "exclusive"},
		},
		Receivers: []ports.ReceiverDef{
			{ID: "sqs-rx", Transport: "sqs"},
			{ID: "sqs-rx2", Transport: "sqs"},
		},
		Senders: []ports.SenderDef{
			{ID: "mqtt-tx", Transport: "mqtt", SessionID: "mqtt-s1"},
			{ID: "mqtt-tx2", Transport: "mqtt", SessionID: "mqtt-s2"},
		},
		Bindings: []ports.BindingDef{
			// Targets mqtt-s1, which is r-shared-a's primary session.
			{ID: "b-cross", SenderID: "mqtt-tx", SessionID: "mqtt-s1", Address: "topic/test"},
		},
		Routes: []ports.RouteDef{
			{
				ID:           "r-shared-a",
				ReceiverID:   "sqs-rx",
				DeliveryMode: "shared_outbox",
				Session:      &ports.RouteSessionDef{SessionID: "mqtt-s1", SenderID: "mqtt-tx"},
			},
			{
				ID:           "r-shared-b",
				ReceiverID:   "sqs-rx2",
				DeliveryMode: "shared_outbox",
				Bindings:     []string{"b-cross"},
				Session:      &ports.RouteSessionDef{SessionID: "mqtt-s2", SenderID: "mqtt-tx2"},
			},
		},
	}

	_, err := NewBuilder(cfg).
		RegisterTransportFactory("mqtt", &fakeTransportFactory{}).
		RegisterTransportFactory("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		Build(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "wrong destination")
	assert.Contains(t, err.Error(), "shared_outbox route \"r-shared-a\"")
}

// TestBuilder_SharedOutboxBinding_DedicatedFanoutSession_OK locks in the
// no-false-positive contract: a shared_outbox route fanning out to a dedicated
// session (owned by no other route, registered as a session-sender) builds
// cleanly. Only cross-route PRIMARY references are rejected.
func TestBuilder_SharedOutboxBinding_DedicatedFanoutSession_OK(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "test-bridge"},
		Stores: ports.StoresConfig{
			Lease:  &ports.StoreConfig{Type: "memory"},
			Outbox: &ports.StoreConfig{Type: "memory"},
		},
		Sessions: []ports.SessionDef{
			{ID: "mqtt-s1", Transport: "mqtt", SessionMode: "exclusive"},
			{ID: "mqtt-s2", Transport: "mqtt", SessionMode: "exclusive"},
		},
		Receivers: []ports.ReceiverDef{
			{ID: "sqs-rx", Transport: "sqs"},
		},
		Senders: []ports.SenderDef{
			{ID: "mqtt-tx", Transport: "mqtt", SessionID: "mqtt-s1"},
			{ID: "mqtt-tx2", Transport: "mqtt", SessionID: "mqtt-s2"},
		},
		Bindings: []ports.BindingDef{
			{ID: "b1", SenderID: "mqtt-tx", SessionID: "mqtt-s1", Address: "topic/a"},
			// mqtt-s2 is a dedicated fan-out session, not any route's primary.
			{ID: "b2", SenderID: "mqtt-tx2", SessionID: "mqtt-s2", Address: "topic/b"},
		},
		Routes: []ports.RouteDef{
			{
				ID:           "r1",
				ReceiverID:   "sqs-rx",
				DeliveryMode: "shared_outbox",
				DispatchMode: "fanout",
				Bindings:     []string{"b1", "b2"},
				Policy:       ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop"},
				Session:      &ports.RouteSessionDef{SessionID: "mqtt-s1", SenderID: "mqtt-tx"},
			},
		},
	}

	rt, err := NewBuilder(cfg).
		RegisterTransportFactory("mqtt", &fakeTransportFactory{}).
		RegisterTransportFactory("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		Build(context.Background())

	require.NoError(t, err)
	require.NotNil(t, rt)
}

// TestBuilder_SharedOutboxBinding_DedicatedSessionConflictingSenders_Rejected
// verifies the builder fails closed when two shared_outbox bindings drain the
// SAME dedicated session with DIFFERENT senders. A session has exactly one
// Path-2 drainer, wired with the first registered binding's sender; the builder's
// registeredSessions dedup silently drops the second sender, so that binding's
// records mis-deliver via the wrong sender.
func TestBuilder_SharedOutboxBinding_DedicatedSessionConflictingSenders_Rejected(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "test-bridge"},
		Stores: ports.StoresConfig{
			Lease:  &ports.StoreConfig{Type: "memory"},
			Outbox: &ports.StoreConfig{Type: "memory"},
		},
		Sessions: []ports.SessionDef{
			{ID: "mqtt-s1", Transport: "mqtt", SessionMode: "exclusive"},
			{ID: "mqtt-ded", Transport: "mqtt", SessionMode: "exclusive"},
		},
		Receivers: []ports.ReceiverDef{
			{ID: "sqs-rx", Transport: "sqs"},
		},
		Senders: []ports.SenderDef{
			{ID: "mqtt-tx", Transport: "mqtt", SessionID: "mqtt-s1"},
			{ID: "tx-a", Transport: "mqtt", SessionID: "mqtt-ded"},
			{ID: "tx-b", Transport: "mqtt", SessionID: "mqtt-ded"},
		},
		Bindings: []ports.BindingDef{
			{ID: "b1", SenderID: "mqtt-tx", SessionID: "mqtt-s1", Address: "topic/own"},
			// Both drain mqtt-ded, but with different senders.
			{ID: "b-a", SenderID: "tx-a", SessionID: "mqtt-ded", Address: "topic/a"},
			{ID: "b-b", SenderID: "tx-b", SessionID: "mqtt-ded", Address: "topic/b"},
		},
		Routes: []ports.RouteDef{
			{
				ID:           "r1",
				ReceiverID:   "sqs-rx",
				DeliveryMode: "shared_outbox",
				DispatchMode: "fanout",
				Bindings:     []string{"b1", "b-a", "b-b"},
				Session:      &ports.RouteSessionDef{SessionID: "mqtt-s1", SenderID: "mqtt-tx"},
			},
		},
	}

	_, err := NewBuilder(cfg).
		RegisterTransportFactory("mqtt", &fakeTransportFactory{}).
		RegisterTransportFactory("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		Build(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "wrong destination")
	assert.Contains(t, err.Error(), "mqtt-ded")
}

// TestBuilder_SharedOutboxBinding_DedicatedSessionSameSender_OK locks in the
// no-false-positive contract for the sender-conflict check: two bindings draining
// the same dedicated session with the SAME sender is the legitimate fan-out case
// (one drainer, one sender, distinct addresses) and must build cleanly.
func TestBuilder_SharedOutboxBinding_DedicatedSessionSameSender_OK(t *testing.T) {
	cfg := &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: "test-bridge"},
		Stores: ports.StoresConfig{
			Lease:  &ports.StoreConfig{Type: "memory"},
			Outbox: &ports.StoreConfig{Type: "memory"},
		},
		Sessions: []ports.SessionDef{
			{ID: "mqtt-s1", Transport: "mqtt", SessionMode: "exclusive"},
			{ID: "mqtt-ded", Transport: "mqtt", SessionMode: "exclusive"},
		},
		Receivers: []ports.ReceiverDef{
			{ID: "sqs-rx", Transport: "sqs"},
		},
		Senders: []ports.SenderDef{
			{ID: "mqtt-tx", Transport: "mqtt", SessionID: "mqtt-s1"},
			{ID: "tx-ded", Transport: "mqtt", SessionID: "mqtt-ded"},
		},
		Bindings: []ports.BindingDef{
			{ID: "b1", SenderID: "mqtt-tx", SessionID: "mqtt-s1", Address: "topic/own"},
			// Both drain mqtt-ded via the same sender, different addresses.
			{ID: "b-a", SenderID: "tx-ded", SessionID: "mqtt-ded", Address: "topic/a"},
			{ID: "b-b", SenderID: "tx-ded", SessionID: "mqtt-ded", Address: "topic/b"},
		},
		Routes: []ports.RouteDef{
			{
				ID:           "r1",
				ReceiverID:   "sqs-rx",
				DeliveryMode: "shared_outbox",
				DispatchMode: "fanout",
				Bindings:     []string{"b1", "b-a", "b-b"},
				Policy:       ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop"},
				Session:      &ports.RouteSessionDef{SessionID: "mqtt-s1", SenderID: "mqtt-tx"},
			},
		},
	}

	rt, err := NewBuilder(cfg).
		RegisterTransportFactory("mqtt", &fakeTransportFactory{}).
		RegisterTransportFactory("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		Build(context.Background())

	require.NoError(t, err)
	require.NotNil(t, rt)
}

// TestBuilder_SharedOutboxBinding_PrimarySessionConflictingSender_Rejected
// verifies the builder fails closed when a binding whose session is the route's
// OWN primary names a different sender than the route. The primary session has a
// single Path-1 drainer wired with the route sender, so the binding's sender is
// silently ignored and its records mis-deliver through the route sender.
func TestBuilder_SharedOutboxBinding_PrimarySessionConflictingSender_Rejected(t *testing.T) {
	cfg := &ports.BridgeConfig{
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
			{ID: "tx-other", Transport: "mqtt", SessionID: "mqtt-s1"},
		},
		Bindings: []ports.BindingDef{
			// Names the route's own primary session, but a different sender.
			{ID: "b1", SenderID: "tx-other", SessionID: "mqtt-s1", Address: "topic/x"},
		},
		Routes: []ports.RouteDef{
			{
				ID:           "r1",
				ReceiverID:   "sqs-rx",
				DeliveryMode: "shared_outbox",
				DispatchMode: "fanout",
				Bindings:     []string{"b1"},
				Session:      &ports.RouteSessionDef{SessionID: "mqtt-s1", SenderID: "mqtt-tx"},
			},
		},
	}

	_, err := NewBuilder(cfg).
		RegisterTransportFactory("mqtt", &fakeTransportFactory{}).
		RegisterTransportFactory("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		Build(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "wrong sender")
	assert.Contains(t, err.Error(), "mqtt-s1")
}

// TestBuilder_SharedOutboxBinding_PrimarySessionInheritedConflictingSender_Rejected
// covers the realistic variant: a binding that inherits the route's primary
// session (empty SessionID) but names its own sender. The operator likely
// believes binding.sender_id controls delivery; it is silently ignored. The
// builder must reject so the footgun is explicit.
func TestBuilder_SharedOutboxBinding_PrimarySessionInheritedConflictingSender_Rejected(t *testing.T) {
	cfg := &ports.BridgeConfig{
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
			{ID: "tx-other", Transport: "mqtt", SessionID: "mqtt-s1"},
		},
		Bindings: []ports.BindingDef{
			// No SessionID -> inherits mqtt-s1; names a different sender.
			{ID: "b1", SenderID: "tx-other", Address: "topic/x"},
		},
		Routes: []ports.RouteDef{
			{
				ID:           "r1",
				ReceiverID:   "sqs-rx",
				DeliveryMode: "shared_outbox",
				DispatchMode: "fanout",
				Bindings:     []string{"b1"},
				Session:      &ports.RouteSessionDef{SessionID: "mqtt-s1", SenderID: "mqtt-tx"},
			},
		},
	}

	_, err := NewBuilder(cfg).
		RegisterTransportFactory("mqtt", &fakeTransportFactory{}).
		RegisterTransportFactory("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		Build(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "wrong sender")
	assert.Contains(t, err.Error(), "b1")
}

// TestBuilder_SharedOutboxBinding_PrimarySessionInheritedSameSender_OK locks in
// the no-false-positive contract for the own-primary sender check: a binding that
// inherits the route's primary session and names the SAME sender as the route is
// the normal case and must build cleanly.
func TestBuilder_SharedOutboxBinding_PrimarySessionInheritedSameSender_OK(t *testing.T) {
	cfg := &ports.BridgeConfig{
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
			// Inherits mqtt-s1 and names the route's own sender -> harmless.
			{ID: "b1", SenderID: "mqtt-tx", Address: "topic/x"},
		},
		Routes: []ports.RouteDef{
			{
				ID:           "r1",
				ReceiverID:   "sqs-rx",
				DeliveryMode: "shared_outbox",
				DispatchMode: "fanout",
				Bindings:     []string{"b1"},
				Policy:       ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop"},
				Session:      &ports.RouteSessionDef{SessionID: "mqtt-s1", SenderID: "mqtt-tx"},
			},
		},
	}

	rt, err := NewBuilder(cfg).
		RegisterTransportFactory("mqtt", &fakeTransportFactory{}).
		RegisterTransportFactory("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		Build(context.Background())

	require.NoError(t, err)
	require.NotNil(t, rt)
}

// Verifies WithCredentialStore resolves credentials_uri into the typed
// session config before creating the transport session.
func TestBuilder_WithCredentialStore(t *testing.T) {
	cfg := testConfig()
	sessCfg := &testCredConfig{URI: "file://test/creds"}
	cfg.Sessions[0].Config = sessCfg

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

	captured, ok := mqttFactory.capturedSessionSpec.Config.(*testCredConfig)
	require.True(t, ok, "captured session config should be *testCredConfig")
	assert.Equal(t, "resolved-user", captured.Username)
	assert.Equal(t, "resolved-pass", captured.Password.Reveal())
	assert.Equal(t, "", captured.URI, "credentials_uri should be cleared after resolution")
}

// Verifies inline session option keys override resolved credential fields while still filling missing values from the store.
func TestBuilder_CredentialInlineOverride(t *testing.T) {
	cfg := testConfig()
	sessCfg := &testCredConfig{URI: "file://test/creds", Username: "inline-user"}
	cfg.Sessions[0].Config = sessCfg

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

	captured, ok := mqttFactory.capturedSessionSpec.Config.(*testCredConfig)
	require.True(t, ok, "captured session config should be *testCredConfig")
	assert.Equal(t, "inline-user", captured.Username,
		"inline value should take precedence over resolved credential")
	assert.Equal(t, "resolved-pass", captured.Password.Reveal(),
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
		RegisterTransportFactory("mqtt", &fakeTransportFactory{}).
		RegisterTransportFactory("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		Build(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires a distributed")
}

// Verifies Build succeeds for clustered deployment when store factories report IsDistributed.
func TestBuilder_Clustered_DistributedStore_OK(t *testing.T) {
	cfg := testConfig()
	cfg.Bridge.DeploymentMode = "clustered"

	rt, err := NewBuilder(cfg).
		RegisterTransportFactory("mqtt", &fakeTransportFactory{}).
		RegisterTransportFactory("sqs", &fakeTransportFactory{}).
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
		RegisterTransportFactory("mqtt", &fakeTransportFactory{}).
		RegisterTransportFactory("sqs", &fakeTransportFactory{}).
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
		RegisterTransportFactory("mqtt", &fakeTransportFactory{}).
		RegisterTransportFactory("sqs", &fakeTransportFactory{}).
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
		RegisterTransportFactory("mqtt", &fakeTransportFactory{}).
		RegisterTransportFactory("sqs", &fakeTransportFactory{}).
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

// fakeVisibilityConfig is a receiver PluginConfig that satisfies
// ports.VisibilityTimeoutConfig, letting a test assert the builder
// prefers the per-route configured window over the transport Factory's
// VisibilityTimeoutProvider constant and threads the auto-extend flag.
type fakeVisibilityConfig struct {
	timeout    time.Duration
	autoExtend bool
}

func (fakeVisibilityConfig) Kind() string    { return "sqs" }
func (fakeVisibilityConfig) Validate() error { return nil }

func (c fakeVisibilityConfig) EffectiveVisibilityTimeout() time.Duration { return c.timeout }
func (c fakeVisibilityConfig) AutoExtendEnabled() bool                   { return c.autoExtend }

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
		// drop policies keep the route valid under build-time ValidateRoutes
		// (Finding 5); this test exercises the OTHER policy fields.
		OnPermanentFailure: "drop",
		OnExpired:          "drop",
	}

	rt, err := NewBuilder(cfg).
		RegisterTransportFactory("mqtt", &fakeTransportFactory{}).
		RegisterTransportFactory("sqs", &fakeTransportFactory{}).
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
		RegisterTransportFactory("mqtt", &fakeTransportFactory{}).
		RegisterTransportFactory("sqs", &fakeTransportFactory{}).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		Build(context.Background())

	require.NoError(t, err)
	require.NotNil(t, rt)
}

// TestBuilder_WiresSourceVisibilityTimeout verifies that a transport
// factory implementing VisibilityTimeoutProvider causes the builder to
// populate SourceVisibilityTimeout on the resulting route. When
// SendTimeout >= VisibilityTimeout/2, the route is rejected — and, since
// Finding 5 / moved static route validation into complete(), that
// rejection now happens at BUILD time (before the old runtime is stopped),
// not only at Start().
func TestBuilder_WiresSourceVisibilityTimeout(t *testing.T) {
	cfg := testConfig()
	cfg.Routes[0].DeliveryMode = "direct_hold"
	cfg.Routes[0].Session = nil
	cfg.Routes[0].Policy.SendTimeout = "20s"

	sqsFactory := &fakeVisibilityFactory{timeout: 30 * time.Second}

	_, err := NewBuilder(cfg).
		RegisterTransportFactory("mqtt", &fakeTransportFactory{}).
		RegisterTransportFactory("sqs", sqsFactory).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		Build(context.Background())

	require.Error(t, err, "the SendTimeout/VisibilityTimeout mismatch must fail at build (Finding 5)")
	assert.Contains(t, err.Error(), "SendTimeout")
	assert.Contains(t, err.Error(), "VisibilityTimeout")
}

// TestBuilder_ReceiverConfigVisibilityTimeoutOverridesFactory proves the
// per-route receiver config (ports.VisibilityTimeoutConfig) wins over the
// transport Factory's VisibilityTimeoutProvider constant. The
// factory reports 30s, under which SendTimeout=8s is safe (8 < 15). The
// receiver config reports a shorter 10s window with auto-extend OFF (a
// fixed window), under which the same SendTimeout is unsafe (8 > 5), so
// the route must be rejected. Since Finding 5 / moved static route
// validation into complete(), that rejection now happens at BUILD time. If
// the builder ignored the config and used the factory constant, no error
// would fire — making this a true regression guard.
func TestBuilder_ReceiverConfigVisibilityTimeoutOverridesFactory(t *testing.T) {
	cfg := testConfig()
	cfg.Routes[0].DeliveryMode = "direct_hold"
	cfg.Routes[0].Session = nil
	cfg.Routes[0].Policy.SendTimeout = "8s"
	cfg.Routes[0].Policy.OnPermanentFailure = "drop"
	cfg.Routes[0].Policy.OnExpired = "drop"
	cfg.Receivers[0].Config = fakeVisibilityConfig{timeout: 10 * time.Second, autoExtend: false}

	sqsFactory := &fakeVisibilityFactory{timeout: 30 * time.Second}

	_, err := NewBuilder(cfg).
		RegisterTransportFactory("mqtt", &fakeTransportFactory{}).
		RegisterTransportFactory("sqs", sqsFactory).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		Build(context.Background())

	require.Error(t, err, "the shorter receiver-config window must fail the route at build (Finding 5)")
	assert.Contains(t, err.Error(), "SendTimeout")
	assert.Contains(t, err.Error(), "VisibilityTimeout")
}

// TestBuilder_AutoExtendSkipsVisibilityTimeoutCheck proves that when the
// receiver auto-extends its window (SQS/ASB auto_extend, on by default),
// the validator skips the SendTimeout-vs-window check even for a short
// window. Same 10s window and 8s SendTimeout as the test
// above — which would be rejected with a fixed window — but auto-extend
// ON makes it a valid config, so Start() must succeed. Guards against the
// threading fix over-rejecting the default, auto-extend-backed config.
func TestBuilder_AutoExtendSkipsVisibilityTimeoutCheck(t *testing.T) {
	cfg := testConfig()
	cfg.Routes[0].DeliveryMode = "direct_hold"
	cfg.Routes[0].Session = nil
	cfg.Routes[0].Policy.SendTimeout = "8s"
	cfg.Routes[0].Policy.OnPermanentFailure = "drop"
	cfg.Routes[0].Policy.OnExpired = "drop"
	cfg.Receivers[0].Config = fakeVisibilityConfig{timeout: 10 * time.Second, autoExtend: true}

	sqsFactory := &fakeVisibilityFactory{timeout: 30 * time.Second}

	rt, err := NewBuilder(cfg).
		RegisterTransportFactory("mqtt", &fakeTransportFactory{}).
		RegisterTransportFactory("sqs", sqsFactory).
		RegisterStoreFactory("memory", &fakeStoreFactory{}).
		Build(context.Background())

	require.NoError(t, err)
	require.NotNil(t, rt)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, rt.Start(ctx))
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopCancel()
		_ = rt.Stop(stopCtx)
	})
}

// fakeCapabilityConfig is a receiver PluginConfig that satisfies
// ports.CapabilityConfig, letting a test assert the builder threads a
// per-route source-capability set (e.g. ASB ReceiveAndDelete, which
// redelivers nothing) over the transport Factory's transport-wide
// Capabilities() constant.
type fakeCapabilityConfig struct {
	caps []ports.Capability
}

func (fakeCapabilityConfig) Kind() string                       { return "sqs" }
func (fakeCapabilityConfig) Validate() error                    { return nil }
func (c fakeCapabilityConfig) Capabilities() []ports.Capability { return c.caps }

// TestBuilder_ReceiverConfigCapabilitiesOverrideFactory proves a per-route
// receiver config (ports.CapabilityConfig) wins over the transport
// Factory's transport-wide Capabilities() constant. The fake sqs
// factory declares CapVisibilityExtension, under which failed messages are
// treated as redeliverable and the retry-fallback silent-drop check is
// skipped. testConfig has no DLQ store and does not set AllowRetryDrop, so
// the ONLY thing keeping its route valid is that redelivery capability.
//
//   - caps=nil models ASB ReceiveAndDelete (Extend is a no-op, the message
//     is already removed on receive): the route must now be REJECTED at
//     build as a silent-drop config.
//   - caps=[CapSourceRedelivery] models a mode that can redeliver: the same
//     route must still build clean, proving the override does not
//     over-reject and threads a non-empty set correctly.
//
// If the builder ignored the config and used the factory constant, the
// nil-caps case would not error — so this is a true regression guard.
func TestBuilder_ReceiverConfigCapabilitiesOverrideFactory(t *testing.T) {
	tests := []struct {
		name      string
		caps      []ports.Capability
		wantError bool
	}{
		{name: "receive_and_delete_no_redelivery_rejected", caps: nil, wantError: true},
		{name: "redelivery_capable_allowed", caps: []ports.Capability{ports.CapSourceRedelivery}, wantError: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Receivers[0].Config = fakeCapabilityConfig{caps: tc.caps}

			_, err := NewBuilder(cfg).
				RegisterTransportFactory("mqtt", &fakeTransportFactory{}).
				RegisterTransportFactory("sqs", &fakeTransportFactory{}).
				RegisterStoreFactory("memory", &fakeStoreFactory{}).
				Build(context.Background())

			if tc.wantError {
				require.Error(t, err, "no redelivery capability + no DLQ must fail the route at build")
				assert.Contains(t, err.Error(), "does not support retry/redelivery")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// fakeValidatingSender is a fakeSender that also implements
// ports.AddressValidatingSender, rejecting any address other than
// "good-queue". It lets the builder test exercise the build-time address
// validation path without depending on a real transport adapter.
type fakeValidatingSender struct {
	fakeSender
}

func (f *fakeValidatingSender) ValidateAddress(address string) error {
	if address == "good-queue" {
		return nil
	}
	return fmt.Errorf("address %q rejected", address)
}

var _ ports.AddressValidatingSender = (*fakeValidatingSender)(nil)

type fakeValidatingTransportFactory struct {
	fakeTransportFactory
}

func (f *fakeValidatingTransportFactory) NewSender(_ context.Context, _ ports.SenderSpec, _ ports.Session) (ports.Sender, error) {
	return &fakeValidatingSender{}, nil
}

// TestBuilder_ValidatesStaticAddressAtBuildTime proves the builder invokes a
// sender's ports.AddressValidatingSender hook against each binding's static
// address, failing the build on a mismatch (fail-fast) while skipping
// templated addresses (rendered per message, not statically checkable).
//
// The mismatch case is the true-regression guard: removing the build-time
// validation loop makes it pass Build() with no error.
func TestBuilder_ValidatesStaticAddressAtBuildTime(t *testing.T) {
	newCfg := func(address string) *ports.BridgeConfig {
		return &ports.BridgeConfig{
			Bridge:    ports.BridgeSettings{ID: "b1"},
			Receivers: []ports.ReceiverDef{{ID: "rx1", Transport: "sqs"}},
			Senders:   []ports.SenderDef{{ID: "tx1", Transport: "sqs"}},
			Bindings:  []ports.BindingDef{{ID: "b1", SenderID: "tx1", Address: address}},
			Routes: []ports.RouteDef{
				// drop policies keep the route valid under the build-time
				// ValidateRoutes call (Finding 5 /) so this test isolates
				// static ADDRESS validation, not DLQ-policy validation.
				{ID: "r1", ReceiverID: "rx1", DeliveryMode: "direct_hold", Bindings: []string{"b1"},
					Policy: ports.PolicyDef{OnPermanentFailure: "drop", OnExpired: "drop"}},
			},
		}
	}

	cases := []struct {
		name    string
		address string
		wantErr bool
	}{
		{"matching-literal", "good-queue", false},
		{"mismatched-literal", "bad-queue", true},
		{"templated-address-skipped", "svc-{tenant}", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt, err := NewBuilder(newCfg(tc.address)).
				RegisterTransportFactory("sqs", &fakeValidatingTransportFactory{}).
				Build(context.Background())

			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "b1", "error must identify the offending binding")
			} else {
				require.NoError(t, err)
				require.NotNil(t, rt)
			}
		})
	}
}

func TestBuilder_TypedNilReceiverConfigReturnsValidationError(t *testing.T) {
	cfg := testConfig()
	var typedNil *typedNilBuildConfig
	cfg.Receivers[0].Config = typedNil

	var err error
	require.NotPanics(t, func() {
		_, err = NewBuilder(cfg).
			RegisterTransportFactory("mqtt", &fakeTransportFactory{}).
			RegisterTransportFactory("sqs", &fakeTransportFactory{}).
			RegisterStoreFactory("memory", &fakeStoreFactory{}).
			Build(t.Context())
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, shared.ErrInvalidConfig)
}
