package integration_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	ddbconfig "github.com/mariotoffia/gobridge/adapters/aws/config/dynamodb"
	fileconfig "github.com/mariotoffia/gobridge/adapters/native/config/file"
	"github.com/mariotoffia/gobridge/bridge"
	"github.com/mariotoffia/gobridge/config"
	cfgparser "github.com/mariotoffia/gobridge/config/parser"
	"github.com/mariotoffia/gobridge/domain/connectivity"
	"github.com/mariotoffia/gobridge/domain/persistence"
	"github.com/mariotoffia/gobridge/ports"
	goruntime "github.com/mariotoffia/gobridge/runtime"
	"github.com/mariotoffia/gobridge/testutil/ddblocal"
	"github.com/mariotoffia/gobridge/testutil/wait"
)

// ---------------------------------------------------------------------------
// DynamoDB config helpers
// ---------------------------------------------------------------------------

// ddbConfigLoader creates a DynamoDB Loader with a unique table, 100ms
// poll interval, and automatic table creation and cleanup.
func ddbConfigLoader(t *testing.T, prefix string) *ddbconfig.Loader {
	t.Helper()
	client := ddblocal.Client(t)
	tableName := ddblocal.UniqueTable(prefix)
	loader := ddbconfig.NewLoader(client,
		ddbconfig.WithTableName(tableName),
		ddbconfig.WithBridgeID("test-bridge"),
		ddbconfig.WithPollInterval(100*time.Millisecond),
		ddbconfig.WithRegistry(newTestRegistry()),
	)
	if err := loader.EnsureTable(context.Background()); err != nil {
		t.Fatalf("ensure table: %v", err)
	}
	ddblocal.CleanupTable(t, client, tableName)
	return loader
}

// writeBaseYAML writes a minimal valid base YAML config to a temp file
// and returns the file path. The config has one fake receiver, sender,
// binding, and direct_hold route.
func writeBaseYAML(t *testing.T, bridgeID string) string {
	t.Helper()
	content := fmt.Sprintf(`bridge:
  id: %s
  shutdown_timeout: "5s"
  drain_timeout: "1s"
  log_level: "info"
stores:
  dlq:
    type: memory
receivers:
  - id: rx-base
    transport: fake
senders:
  - id: tx-base
    transport: fake
bindings:
  - id: bind-base
    sender_id: tx-base
    address: "addr/base"
routes:
  - id: r1
    receiver_id: rx-base
    delivery_mode: direct_hold
    bindings:
      - bind-base
`, bridgeID)
	f, err := os.CreateTemp(t.TempDir(), "gobridge-base-*.yaml")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		t.Fatalf("write temp file: %v", err)
	}
	_ = f.Close()
	return f.Name()
}

// writeBaseYAMLWithSession writes a base YAML with a session definition.
func writeBaseYAMLWithSession(t *testing.T, bridgeID, sessionID, transport string) string {
	t.Helper()
	content := fmt.Sprintf(`bridge:
  id: %s
  shutdown_timeout: "5s"
  drain_timeout: "1s"
  log_level: "info"
stores:
  dlq:
    type: memory
sessions:
  - id: %s
    transport: %s
    options:
      broker: "tcp://localhost:1883"
receivers:
  - id: rx-base
    transport: fake
senders:
  - id: tx-base
    transport: fake
bindings:
  - id: bind-base
    sender_id: tx-base
    address: "addr/base"
routes:
  - id: r1
    receiver_id: rx-base
    delivery_mode: direct_hold
    bindings:
      - bind-base
`, bridgeID, sessionID, transport)
	f, err := os.CreateTemp(t.TempDir(), "gobridge-base-*.yaml")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		t.Fatalf("write temp file: %v", err)
	}
	_ = f.Close()
	return f.Name()
}

// writeBridgeConfigYAML marshals a BridgeConfig to JSON (which is valid
// YAML) and writes it to a temp file. Returns the file path.
//
// Uses cfgparser.MarshalBridgeConfigJSON so that typed PluginConfig values
// (which carry json:"-" on the field) are projected back into the
// canonical "options:" wire form and survive the file → config.Parse
// round-trip.
func writeBridgeConfigYAML(t *testing.T, cfg *ports.BridgeConfig) string {
	t.Helper()
	data, err := cfgparser.MarshalBridgeConfigJSON(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	f, err := os.CreateTemp(t.TempDir(), "gobridge-cfg-*.json")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		t.Fatalf("write temp file: %v", err)
	}
	_ = f.Close()
	return f.Name()
}

// newFileBaseLayer creates a config.Layer backed by a file source.
func newFileBaseLayer(path string) config.Layer {
	return config.Layer{
		Name:   "file",
		Loader: fileconfig.NewSource(path, newTestRegistry()),
	}
}

// newDDBOverlayLayer creates a config.Layer backed by a DynamoDB loader,
// using the loader as both Loader and Watcher.
func newDDBOverlayLayer(loader *ddbconfig.Loader) config.Layer {
	return config.Layer{
		Name:    "ddb",
		Loader:  loader,
		Watcher: loader,
	}
}

// newTestConfigManager creates a config.Manager with a file base layer
// and DynamoDB overlay layer.
func newTestConfigManager(t *testing.T, basePath string, loader *ddbconfig.Loader) *config.Manager {
	t.Helper()
	mgr := config.NewManager(
		newFileBaseLayer(basePath),
		config.WithOverlay(newDDBOverlayLayer(loader)),
		config.WithManagerLogger(slog.Default()),
	)
	t.Cleanup(mgr.Stop)
	return mgr
}

// waitForConfig reads a config from the channel with a timeout.
func waitForConfig(t *testing.T, ch <-chan *ports.BridgeConfig, timeout time.Duration) *ports.BridgeConfig {
	t.Helper()
	select {
	case cfg, ok := <-ch:
		if !ok {
			t.Fatal("config channel closed unexpectedly")
		}
		if cfg == nil {
			t.Fatal("received nil config")
		}
		return cfg
	case <-time.After(timeout):
		t.Fatal("timed out waiting for config emission")
		return nil
	}
}

// waitForNoConfig asserts that no config arrives within the given duration.
func waitForNoConfig(t *testing.T, ch <-chan *ports.BridgeConfig, duration time.Duration) {
	t.Helper()
	select {
	case cfg := <-ch:
		t.Fatalf("expected no config emission, got: bridge.id=%s", cfg.Bridge.ID)
	case <-time.After(duration):
		// expected: nothing emitted
	}
}

// ---------------------------------------------------------------------------
// Config overlay builders
// ---------------------------------------------------------------------------

// minimalOverlay returns a BridgeConfig with only the bridge ID set.
// This is the smallest valid overlay (all merges are non-destructive).
func minimalOverlay(bridgeID string) *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: bridgeID},
	}
}

func testStoresWithDLQ() ports.StoresConfig {
	return ports.StoresConfig{DLQ: &ports.StoreConfig{Type: "memory"}}
}

// overlayWithRoute creates a config overlay that adds a complete route
// (receiver, sender, binding, route) using the fake transport.
func overlayWithRoute(bridgeID, routeID string) *ports.BridgeConfig {
	return &ports.BridgeConfig{
		Bridge: ports.BridgeSettings{ID: bridgeID},
		Receivers: []ports.ReceiverDef{
			{ID: routeID + "-rx", Transport: "fake"},
		},
		Senders: []ports.SenderDef{
			{ID: routeID + "-tx", Transport: "fake"},
		},
		Bindings: []ports.BindingDef{
			{ID: routeID + "-b", SenderID: routeID + "-tx", Address: "addr/" + routeID},
		},
		Routes: []ports.RouteDef{
			{
				ID:           routeID,
				ReceiverID:   routeID + "-rx",
				DeliveryMode: "direct_hold",
				Bindings:     []string{routeID + "-b"},
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Fake transport types for supervisor tests
// ---------------------------------------------------------------------------

// cfgFakeSession implements ports.Session with no-op behavior.
type cfgFakeSession struct {
	events chan ports.SessionEvent
}

func newCfgFakeSession() *cfgFakeSession {
	return &cfgFakeSession{events: make(chan ports.SessionEvent, 1)}
}

func (s *cfgFakeSession) Start(_ context.Context) error { return nil }
func (s *cfgFakeSession) Reconcile(_ context.Context, _ connectivity.SessionPlan) error {
	return nil
}
func (s *cfgFakeSession) Health(_ context.Context) ports.SessionHealth {
	return ports.SessionHealth{Connected: true, Ready: true, ServiceLevel: ports.ServiceLevelFull}
}
func (s *cfgFakeSession) Events() <-chan ports.SessionEvent { return s.events }
func (s *cfgFakeSession) Close(_ context.Context) error     { return nil }

// cfgFakeReceiver implements ports.Receiver. Blocks until context cancel.
type cfgFakeReceiver struct{}

func (r *cfgFakeReceiver) Run(ctx context.Context, _ func(context.Context, ports.Delivery) error) error {
	<-ctx.Done()
	return nil
}

// cfgFakeSender implements ports.Sender. No-op send.
type cfgFakeSender struct{}

func (s *cfgFakeSender) Send(_ context.Context, msg ports.OutboundMessage) error { return nil }

// cfgFakeTransportFactory implements ports.TransportFactory for test configs
// that use transport="fake".
type cfgFakeTransportFactory struct {
	mu            sync.Mutex
	sessionCalls  int
	receiverCalls int
	senderCalls   int
}

func (f *cfgFakeTransportFactory) NewSession(_ context.Context, _ ports.SessionSpec) (ports.Session, error) {
	f.mu.Lock()
	f.sessionCalls++
	f.mu.Unlock()
	return newCfgFakeSession(), nil
}

func (f *cfgFakeTransportFactory) NewReceiver(_ context.Context, _ ports.ReceiverSpec, _ ports.Session) (ports.Receiver, error) {
	f.mu.Lock()
	f.receiverCalls++
	f.mu.Unlock()
	return &cfgFakeReceiver{}, nil
}

func (f *cfgFakeTransportFactory) NewSender(_ context.Context, _ ports.SenderSpec, _ ports.Session) (ports.Sender, error) {
	f.mu.Lock()
	f.senderCalls++
	f.mu.Unlock()
	return &cfgFakeSender{}, nil
}

func (f *cfgFakeTransportFactory) Capabilities() []ports.Capability {
	return []ports.Capability{ports.CapSourceRedelivery, ports.CapVisibilityExtension}
}

func (f *cfgFakeTransportFactory) AddressValidator() ports.AddressValidator { return nil }

// cfgBrokenTransportFactory returns errors on NewSession for testing
// rollback when the builder fails.
type cfgBrokenTransportFactory struct {
	err error
}

func (f *cfgBrokenTransportFactory) NewSession(_ context.Context, _ ports.SessionSpec) (ports.Session, error) {
	return nil, f.err
}

func (f *cfgBrokenTransportFactory) NewReceiver(_ context.Context, _ ports.ReceiverSpec, _ ports.Session) (ports.Receiver, error) {
	return nil, f.err
}

func (f *cfgBrokenTransportFactory) NewSender(_ context.Context, _ ports.SenderSpec, _ ports.Session) (ports.Sender, error) {
	return nil, f.err
}

func (f *cfgBrokenTransportFactory) Capabilities() []ports.Capability { return nil }

func (f *cfgBrokenTransportFactory) AddressValidator() ports.AddressValidator { return nil }

// cfgExclusiveTransportFactory is like cfgFakeTransportFactory but
// declares CapExclusiveIdentity, triggering SwapPrepareCommit mode.
type cfgExclusiveTransportFactory struct {
	cfgFakeTransportFactory
}

func (f *cfgExclusiveTransportFactory) Capabilities() []ports.Capability {
	return []ports.Capability{ports.CapExclusiveIdentity, ports.CapStatefulSession}
}

// ---------------------------------------------------------------------------
// Fake store types for supervisor tests
// ---------------------------------------------------------------------------

type cfgFakeLeaseStore struct{}

func (s *cfgFakeLeaseStore) Acquire(_ context.Context, _ string, _ string, _ time.Duration, _ map[string]string) (persistence.LeaseToken, error) {
	return persistence.LeaseToken{Version: 1, Owner: "test"}, nil
}
func (s *cfgFakeLeaseStore) Renew(_ context.Context, _ string, tok persistence.LeaseToken, _ time.Duration, _ map[string]string) (persistence.LeaseToken, error) {
	tok.Version++
	return tok, nil
}
func (s *cfgFakeLeaseStore) Release(_ context.Context, _ string, _ persistence.LeaseToken) error {
	return nil
}
func (s *cfgFakeLeaseStore) Current(_ context.Context, _ string) (persistence.LeaseInfo, error) {
	return persistence.LeaseInfo{}, nil
}

type cfgFakeOutboxStore struct{}

func (s *cfgFakeOutboxStore) Persist(_ context.Context, _ []*persistence.OutboxRecord) error {
	return nil
}
func (s *cfgFakeOutboxStore) Claim(_ context.Context, _ string, _ persistence.LeaseToken, _ int) ([]*persistence.OutboxRecord, error) {
	return nil, nil
}
func (s *cfgFakeOutboxStore) Complete(_ context.Context, _ []string, _ persistence.LeaseToken) error {
	return nil
}
func (s *cfgFakeOutboxStore) Expire(_ context.Context, _ time.Time, _ string, _ persistence.LeaseToken) (int, error) {
	return 0, nil
}
func (s *cfgFakeOutboxStore) QueryPending(_ context.Context, _ string, _ int) ([]*persistence.OutboxRecord, error) {
	return nil, nil
}

type cfgFakeStoreFactory struct{}

func (f *cfgFakeStoreFactory) NewLeaseStore(_ context.Context, _ ports.PluginConfig) (ports.LeaseStore, error) {
	return &cfgFakeLeaseStore{}, nil
}
func (f *cfgFakeStoreFactory) NewOutboxStore(_ context.Context, _ ports.PluginConfig, _ ports.OutboxRuntimeOptions) (ports.OutboxStore, error) {
	return &cfgFakeOutboxStore{}, nil
}
func (f *cfgFakeStoreFactory) NewDLQStore(_ context.Context, _ ports.PluginConfig) (ports.DLQStore, error) {
	return &fakeDLQStore{}, nil
}

// ---------------------------------------------------------------------------
// Supervisor test helpers
// ---------------------------------------------------------------------------

// newCfgTestSupervisor creates a Supervisor with fake transports and
// stores registered. Optionally accepts additional supervisor options.
func newCfgTestSupervisor(opts ...bridge.SupervisorOption) *bridge.Supervisor {
	s := bridge.NewSupervisor(opts...)
	s.RegisterTransport("fake", &cfgFakeTransportFactory{})
	s.RegisterStoreFactory("memory", &cfgFakeStoreFactory{})
	return s
}

// runSupervisorInBackground starts the supervisor in a goroutine and
// returns a cancel function and error channel.
func runSupervisorInBackground(
	ctx context.Context,
	s *bridge.Supervisor,
	cfg *ports.BridgeConfig,
	changes <-chan *ports.BridgeConfig,
) (context.CancelFunc, chan error) {
	ctx, cancel := context.WithCancel(ctx)
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Run(ctx, cfg, changes)
	}()
	return cancel, errCh
}

// waitForSupervisorRuntime waits until Runtime() is non-nil and reports an
// initial build/start failure immediately instead of masking it as a timeout.
func waitForSupervisorRuntime(
	t *testing.T,
	s *bridge.Supervisor,
	errCh chan error,
	timeout time.Duration,
) *goruntime.Runtime {
	t.Helper()

	var (
		runDone bool
		runErr  error
	)
	wait.Until(t, timeout, "supervisor runtime", func() bool {
		if s.Runtime() != nil {
			return true
		}
		select {
		case runErr = <-errCh:
			runDone = true
			return true
		default:
			return false
		}
	})
	if runDone {
		// Preserve the terminal result for the caller's cleanup, which also
		// waits on errCh after cancelling the supervisor.
		errCh <- runErr
		t.Fatalf("supervisor exited before publishing initial runtime: %v", runErr)
	}
	if rt := s.Runtime(); rt != nil {
		return rt
	}
	t.Fatal("timed out waiting for supervisor runtime")
	return nil
}

// waitForSupervisorRouteID polls until the supervisor's runtime has a
// route matching the given ID.
func waitForSupervisorRouteID(t *testing.T, s *bridge.Supervisor, routeID string, timeout time.Duration) {
	t.Helper()
	wait.Until(t, timeout, "supervisor route "+routeID, func() bool {
		if rt := s.Runtime(); rt != nil {
			for _, r := range rt.Routes() {
				if r.ID == routeID {
					return true
				}
			}
		}
		return false
	})
}

// Interface compliance checks.
var (
	_ ports.TransportFactory = (*cfgFakeTransportFactory)(nil)
	_ ports.TransportFactory = (*cfgBrokenTransportFactory)(nil)
	_ ports.TransportFactory = (*cfgExclusiveTransportFactory)(nil)
	_ ports.StoreFactory     = (*cfgFakeStoreFactory)(nil)
	_ ports.Session          = (*cfgFakeSession)(nil)
	_ ports.Receiver         = (*cfgFakeReceiver)(nil)
	_ ports.Sender           = (*cfgFakeSender)(nil)
	_ ports.LeaseStore       = (*cfgFakeLeaseStore)(nil)
	_ ports.OutboxStore      = (*cfgFakeOutboxStore)(nil)
)
