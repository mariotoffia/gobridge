package bootstrap

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	deployinfra "github.com/mariotoffia/gobridge/deployment/aws/infra"
	"github.com/mariotoffia/gobridge/ports"
)

// stubFactory is a minimal ports.TransportFactory for swap mode detection tests.
type stubFactory struct {
	capabilities []ports.Capability
}

var _ ports.TransportFactory = (*stubFactory)(nil)

func (f *stubFactory) NewSession(_ context.Context, _ ports.SessionSpec) (ports.Session, error) {
	return nil, nil
}
func (f *stubFactory) NewReceiver(_ context.Context, _ ports.ReceiverSpec, _ ports.Session) (ports.Receiver, error) {
	return nil, nil
}
func (f *stubFactory) NewSender(_ context.Context, _ ports.SenderSpec, _ ports.Session) (ports.Sender, error) {
	return nil, nil
}
func (f *stubFactory) Capabilities() []ports.Capability {
	return f.capabilities
}
func (f *stubFactory) AddressValidator() ports.AddressValidator { return nil }

func TestDetectSwapMode_OverlapWhenNoExclusiveIdentity(t *testing.T) {
	reg := &factoryRegistry{
		cfg: &ports.BridgeConfig{
			Sessions: []ports.SessionDef{
				{ID: "http-sess", Transport: "http"},
			},
		},
		transports: map[string]ports.TransportFactory{
			"http": &stubFactory{capabilities: []ports.Capability{ports.CapHTTPEndpoint}},
		},
	}

	mode := reg.detectSwapMode(nil, reg.cfg)
	assert.Equal(t, swapModeOverlap, mode)
}

func TestDetectSwapMode_PrepareCommitWhenExclusiveIdentity(t *testing.T) {
	reg := &factoryRegistry{
		cfg: &ports.BridgeConfig{
			Sessions: []ports.SessionDef{
				{ID: "mqtt-sess", Transport: "mqtt"},
			},
		},
		transports: map[string]ports.TransportFactory{
			"mqtt": &stubFactory{capabilities: []ports.Capability{ports.CapExclusiveIdentity}},
		},
	}

	mode := reg.detectSwapMode(nil, reg.cfg)
	assert.Equal(t, swapModePrepareCommit, mode)
}

func TestDetectSwapMode_UnknownTransportSkipped(t *testing.T) {
	reg := &factoryRegistry{
		cfg: &ports.BridgeConfig{
			Sessions: []ports.SessionDef{
				{ID: "unknown-sess", Transport: "unknown"},
			},
		},
		transports: map[string]ports.TransportFactory{},
	}

	mode := reg.detectSwapMode(nil, reg.cfg)
	assert.Equal(t, swapModeOverlap, mode)
}

func TestTransportHandler_ReturnsNotFoundWhenNoHTTPEndpoints(t *testing.T) {
	reg := &factoryRegistry{
		cfg: &ports.BridgeConfig{
			Receivers: []ports.ReceiverDef{
				{ID: "rx", Transport: "mqtt"},
			},
		},
		http: nil,
	}

	handler := reg.transportHandler()
	assert.NotNil(t, handler)

	rec := &fakeResponseWriter{code: 0, headers: http.Header{}}
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.code)
}

type fakeResponseWriter struct {
	code    int
	headers http.Header
	body    []byte
}

func (w *fakeResponseWriter) Header() http.Header { return w.headers }
func (w *fakeResponseWriter) Write(b []byte) (int, error) {
	w.body = append(w.body, b...)
	return len(b), nil
}
func (w *fakeResponseWriter) WriteHeader(code int) { w.code = code }

// TestNewFactoryRegistry_RegistersDynamoDBStoreFactory asserts the
// DynamoDB store factory is registered in the runtime factory set of
// this AWS deployment profile. Without it a bridge.yaml referencing a
// DynamoDB-backed lease/outbox/DLQ store fails at build time with an
// unknown store type. A nil *dynamodb.Client is sufficient here:
// registration only stores the client; it is dereferenced lazily when
// a store is actually constructed.
func TestNewFactoryRegistry_RegistersDynamoDBStoreFactory(t *testing.T) {
	app := NewApp(deployinfra.BootstrapConfig{
		BridgeID:         "bridge-a",
		ConfigFilePath:   "/tmp/bridge.yaml",
		AdminAPIKeyParam: "/admin",
	}, WithDynamoDBClient(nil))

	reg := app.newFactoryRegistry(&ports.BridgeConfig{})

	factory, ok := reg.stores[awsstore.DynamoDBKind]
	require.True(t, ok, "dynamodb store factory must be registered")
	assert.NotNil(t, factory)

	// Guard against a regression that drops the native stores while
	// adding DynamoDB.
	assert.Contains(t, reg.stores, "memory")
	assert.Contains(t, reg.stores, "sqlite")
}

// exclusiveConfigStubFactory advertises no capability but reports exclusivity
// from a receiver config, the way amqp091's factory does before any receiver
// has been built.
type exclusiveConfigStubFactory struct {
	stubFactory
	exclusive bool
}

func (f *exclusiveConfigStubFactory) ConfigRequiresExclusiveIdentity(ports.PluginConfig) bool {
	return f.exclusive
}

// TestDetectSwapMode_PrepareCommitWhenSessionDeclaresExclusive pins the probe
// a capability check cannot make: a session declared exclusive is a
// single-owner identity whatever its transport advertises. AMQP 1.0 obeys the
// single-use exclusive-session rule without advertising
// ports.CapExclusiveIdentity, so overlapping the swap would attach the new
// consumer while the old one still holds the identity.
func TestDetectSwapMode_PrepareCommitWhenSessionDeclaresExclusive(t *testing.T) {
	reg := &factoryRegistry{
		cfg: &ports.BridgeConfig{
			Sessions: []ports.SessionDef{
				{ID: "amqp-sess", Transport: "amqp10", SessionMode: "exclusive"},
			},
		},
		transports: map[string]ports.TransportFactory{
			"amqp10": &stubFactory{capabilities: []ports.Capability{ports.CapStatefulSession}},
		},
	}

	assert.Equal(t, swapModePrepareCommit, reg.detectSwapMode(nil, reg.cfg))
}

// TestDetectSwapMode_PrepareCommitWhenRouteCarriesInlineSession covers the
// other config-declared form: a route session block is always a lease-managed
// single-owner session.
func TestDetectSwapMode_PrepareCommitWhenRouteCarriesInlineSession(t *testing.T) {
	reg := &factoryRegistry{
		cfg: &ports.BridgeConfig{
			Routes: []ports.RouteDef{
				{ID: "r1", ReceiverID: "rx", Session: &ports.RouteSessionDef{}},
			},
		},
		transports: map[string]ports.TransportFactory{},
	}

	assert.Equal(t, swapModePrepareCommit, reg.detectSwapMode(nil, reg.cfg))
}

// TestDetectSwapMode_PrepareCommitWhenReceiverConfigDeclaresExclusive pins the
// third probe: a factory can report exclusivity from an incoming receiver
// config before it has built anything. Capabilities() latches only AFTER an
// exclusive receiver exists, and this root builds a fresh factory for every
// plan, so the latch is always cold here — without this probe the first (and
// every) swap onto an exclusive config would overlap.
func TestDetectSwapMode_PrepareCommitWhenReceiverConfigDeclaresExclusive(t *testing.T) {
	reg := &factoryRegistry{
		cfg: &ports.BridgeConfig{
			Sessions:  []ports.SessionDef{{ID: "sess", Transport: "amqp091"}},
			Receivers: []ports.ReceiverDef{{ID: "rx", SessionID: "sess"}},
		},
		transports: map[string]ports.TransportFactory{
			"amqp091": &exclusiveConfigStubFactory{exclusive: true},
		},
	}

	assert.Equal(t, swapModePrepareCommit, reg.detectSwapMode(nil, reg.cfg))
}

// TestApp_SwapModeWeighsTheRunningConfig pins that the applier feeds the running
// config into the swap decision. Registry-level tests pass whatever the applier
// hands them, so none of them would notice it handing over nothing; this test
// fails the moment it does.
func TestApp_SwapModeWeighsTheRunningConfig(t *testing.T) {
	// A sender references the session, as in any real config: the builder skips
	// a session nothing references, so an unreferenced one would attach nothing.
	session := func(mode string) *ports.BridgeConfig {
		return &ports.BridgeConfig{
			Sessions: []ports.SessionDef{{ID: "s1", Transport: "sqs", SessionMode: mode}},
			Senders:  []ports.SenderDef{{ID: "tx", SessionID: "s1"}},
		}
	}
	app := NewApp(testBootstrapConfig(), WithDynamoDBClient(nil))
	next := session("shared")
	reg := app.newFactoryRegistry(next)

	assert.Equal(t, swapModeOverlap, app.swapModeFor(reg, next), "first apply: nothing is running to overlap")

	app.appliedRef.Set(session("exclusive"))
	assert.Equal(t, swapModePrepareCommit, app.swapModeFor(reg, next), "leaving an exclusive session must serialize")
}
