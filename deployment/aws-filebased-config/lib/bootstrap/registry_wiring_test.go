package bootstrap

import (
	"context"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"

	httptransport "github.com/mariotoffia/gobridge/adapters/http/transport"
	"github.com/mariotoffia/gobridge/bridge"
	deployinfra "github.com/mariotoffia/gobridge/deployment/aws-filebased-config/infra"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/ports"
)

// The tests below are white-box wiring assertions on the bridge.Builder and
// httptransport.Factory built by newFactoryRegistry. Builder options are
// opaque closures and neither type exposes accessors for the wired
// collaborators, so the assertions read the private fields via reflection
// (IsNil is legal on unexported interface fields). The field names are pinned
// to bridge.Builder / httptransport.Factory internals; a rename there fails
// these tests loudly via the IsValid guard rather than silently passing.

// builderField returns the named private field of a *bridge.Builder.
func builderField(t *testing.T, b *bridge.Builder, name string) reflect.Value {
	t.Helper()
	v := reflect.ValueOf(b).Elem().FieldByName(name)
	require.True(t, v.IsValid(), "bridge.Builder has no field %q — update this wiring test", name)
	return v
}

// factoryField returns the named private field of a *httptransport.Factory.
func factoryField(t *testing.T, f *httptransport.Factory, name string) reflect.Value {
	t.Helper()
	v := reflect.ValueOf(f).Elem().FieldByName(name)
	require.True(t, v.IsValid(), "httptransport.Factory has no field %q — update this wiring test", name)
	return v
}

// fakePullStore is a minimal ports.PullCredentialStore (pull-only, like the
// production runtime.CredentialResolver over SSM).
type fakePullStore struct{}

var _ ports.PullCredentialStore = (*fakePullStore)(nil)

func (fakePullStore) Resolve(_ context.Context, _ string) (*connectivity.CredentialSet, error) {
	return &connectivity.CredentialSet{}, nil
}

func testBootstrapConfig() deployinfra.BootstrapConfig {
	return deployinfra.BootstrapConfig{
		BridgeID:         "bridge-a",
		ConfigFilePath:   "/tmp/bridge.yaml",
		AdminAPIKeyParam: "/admin",
	}
}

// TestNewFactoryRegistry_WiresPolledCredentialStore asserts credential
// rotation is wired: the pull store must be registered BOTH as the
// synchronous pull store (initial resolve at session construction) and as the
// polled store (bridge wraps it in a PollBasedWrapper push store at Build
// time so rotations reach long-lived transport sessions). With only
// WithCredentialStore the push side stays nil and rotation never reaches
// transports.
func TestNewFactoryRegistry_WiresPolledCredentialStore(t *testing.T) {
	app := NewApp(testBootstrapConfig(),
		WithDynamoDBClient(nil),
		WithCredentialStore(&fakePullStore{}),
	)

	reg := app.newFactoryRegistry(&ports.BridgeConfig{})

	require.False(t, builderField(t, reg.builder, "credStore").IsNil(),
		"pull credential store must be wired for the synchronous initial resolve")
	require.False(t, builderField(t, reg.builder, "pollCredStore").IsNil(),
		"polled credential store must be wired so rotation reaches transports (bridge.WithPolledCredentialStore)")
}

// TestNewFactoryRegistry_NoCredentialStore_LeavesCredentialWiringEmpty pins
// the guard: without a credential store nothing is registered (the builder
// then skips credential refresh entirely).
func TestNewFactoryRegistry_NoCredentialStore_LeavesCredentialWiringEmpty(t *testing.T) {
	app := NewApp(testBootstrapConfig(), WithDynamoDBClient(nil))

	reg := app.newFactoryRegistry(&ports.BridgeConfig{})

	require.True(t, builderField(t, reg.builder, "credStore").IsNil())
	require.True(t, builderField(t, reg.builder, "pollCredStore").IsNil())
}

// TestNewFactoryRegistry_WiresRuntimeAuditLogger asserts a runtime
// ports.AuditLogger is passed to every builder so lease/DLQ audit events are
// emitted through the App's slog logger instead of the Noop default.
func TestNewFactoryRegistry_WiresRuntimeAuditLogger(t *testing.T) {
	app := NewApp(testBootstrapConfig(), WithDynamoDBClient(nil))

	reg := app.newFactoryRegistry(&ports.BridgeConfig{})

	require.False(t, builderField(t, reg.builder, "auditLogger").IsNil(),
		"runtime audit logger must be wired (bridge.WithAuditLogger); Noop loses lease/DLQ audit")
}

// TestNewFactoryRegistry_WiresHTTPFactoryMetrics asserts the HTTP transport
// factory receives the metrics exporter the bootstrap builds, so receiver /
// SSE-sender metrics (ingress latency, duplicates, forward counters) are
// exported instead of silently dropped by the factory-level noop fallback.
func TestNewFactoryRegistry_WiresHTTPFactoryMetrics(t *testing.T) {
	app := NewApp(testBootstrapConfig(),
		WithDynamoDBClient(nil),
		WithMetricsExporter(&ports.RecordingExporter{}),
	)

	reg := app.newFactoryRegistry(&ports.BridgeConfig{})

	require.NotNil(t, reg.http)
	require.False(t, factoryField(t, reg.http, "metrics").IsNil(),
		"HTTP factory must receive the bootstrap metrics exporter (httptransport.WithFactoryMetrics)")
}

func TestNewFactoryRegistry_ClusteredRegistersECSEndpointResolver(t *testing.T) {
	app := NewApp(testBootstrapConfig(), WithDynamoDBClient(nil))
	cfg := &ports.BridgeConfig{Bridge: ports.BridgeSettings{DeploymentMode: "clustered"}}
	reg := app.newFactoryRegistry(cfg)
	require.False(t, builderField(t, reg.builder, "endpointResolver").IsNil(),
		"clustered profile must register the existing ECS endpoint resolver")
}

func TestNewFactoryRegistry_StandaloneLeavesEndpointResolverEmpty(t *testing.T) {
	app := NewApp(testBootstrapConfig(), WithDynamoDBClient(nil))
	reg := app.newFactoryRegistry(&ports.BridgeConfig{Bridge: ports.BridgeSettings{DeploymentMode: "standalone"}})
	require.True(t, builderField(t, reg.builder, "endpointResolver").IsNil())
}
