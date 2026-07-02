package bootstrap

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	awsstore "github.com/mariotoffia/gobridge/adapters/aws/store"
	deployinfra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
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

	mode := reg.detectSwapMode(reg.cfg)
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

	mode := reg.detectSwapMode(reg.cfg)
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

	mode := reg.detectSwapMode(reg.cfg)
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
